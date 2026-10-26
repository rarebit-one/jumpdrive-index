package jumpdrive

// The JWKS verifier for the per-caller credential the control plane mints for
// every entity_* call (jumpdrive-web docs/index-principal-adr.md Decision 2,
// contract credential.v1.json + jwks.v1.json, vendored under contract/).
//
// It is deliberately stdlib-only (crypto/ecdsa, no JOSE dependency) and pinned:
// the only accepted algorithm is ES256, so `none`, HS* and RS* are refused from
// the header before any key is consulted, and a key from the JWKS is only ever
// used as a P-256 ECDSA key. Header fields that would let a token name its own
// key (jwk, jku, x5u, x5c) are never read.
//
// Keys are cached by kid. A kid miss triggers a JWKS refetch, rate-limited so a
// stream of made-up kids costs at most one fetch per MinRefetchInterval. A cache
// older than RefreshAfter is refreshed on the next verify; if the JWKS endpoint
// is down, known kids keep verifying from the cache and an unknown kid fails
// closed.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Credential contract constants (credential.v1.json).
const (
	CredentialAlg      = "ES256"
	CredentialType     = "jd-index+jwt"
	CredentialAudience = "jumpdrive-index"
	CredentialVersion  = 1
	// MaxClockSkew is the tolerance on exp and nbf.
	MaxClockSkew = 30 * time.Second
)

// ErrInvalidCredential is returned for any credential that fails verification:
// malformed, wrong alg/typ/aud/iss/version, bad signature, expired, not yet
// valid, or signed by a kid the JWKS does not (or cannot currently) vouch for.
var ErrInvalidCredential = errors.New("jumpdrive: invalid credential")

// Claims is the verified claim set of a credential (credential.v1.json).
type Claims struct {
	Issuer   string    `json:"iss"`
	Audience audience  `json:"aud"`
	Subject  string    `json:"sub"`
	Org      string    `json:"jd_org"`
	Ws       string    `json:"jd_ws"`
	Access   string    `json:"jd_access"`
	Spaces   jdSpaces  `json:"jd_spaces"`
	Client   string    `json:"jd_client,omitempty"`
	Version  int       `json:"jd_ver"`
	JTI      string    `json:"jti"`
	IssuedAt *jsonTime `json:"iat"`
	NotEarly *jsonTime `json:"nbf"`
	Expires  *jsonTime `json:"exp"`
}

type jdSpaces struct {
	Read    *[]string `json:"read"`
	Write   *[]string `json:"write"`
	Approve *[]string `json:"approve"`
}

// audience accepts the RFC 7519 string-or-array form.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*a = audience{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("aud must be a string or an array of strings")
	}
	*a = many
	return nil
}

// jsonTime is a NumericDate. Fractional seconds are truncated.
type jsonTime struct{ time.Time }

func (t *jsonTime) UnmarshalJSON(b []byte) error {
	var f float64
	if err := json.Unmarshal(b, &f); err != nil {
		return errors.New("NumericDate must be a number")
	}
	t.Time = time.Unix(int64(f), 0)
	return nil
}

type jwsHeader struct {
	Alg  string   `json:"alg"`
	Kid  string   `json:"kid"`
	Typ  string   `json:"typ"`
	Crit []string `json:"crit"`
}

// VerifierConfig configures a Verifier.
type VerifierConfig struct {
	JWKSURL string // <console>/internal/index/jwks.json
	Issuer  string // the control plane base URL; must equal `iss` exactly
	// MinRefetchInterval bounds JWKS fetches triggered by a kid miss or a stale
	// cache: at most one attempt per interval. Default 10s.
	MinRefetchInterval time.Duration
	// RefreshAfter is how long a fetched JWKS is used before it is refreshed
	// (so a key the control plane withdraws stops verifying). Default 5m.
	RefreshAfter time.Duration
	HTTPClient   httpDoer // default *http.Client{Timeout: 5s}
	Now          func() time.Time
	Logger       *slog.Logger
}

// Verifier verifies credentials against the control plane's JWKS.
type Verifier struct {
	jwksURL      string
	issuer       string
	minRefetch   time.Duration
	refreshAfter time.Duration
	http         httpDoer
	now          func() time.Time
	log          *slog.Logger

	fetchMu   sync.Mutex // serialises fetches (one in flight)
	mu        sync.RWMutex
	keys      map[string]*ecdsa.PublicKey
	fetchedAt time.Time // last successful fetch
	triedAt   time.Time // last fetch attempt, success or not
}

// NewVerifier builds a Verifier. JWKSURL and Issuer are required.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if cfg.JWKSURL == "" {
		return nil, errors.New("jumpdrive: empty JWKS URL")
	}
	if cfg.Issuer == "" {
		return nil, errors.New("jumpdrive: empty credential issuer")
	}
	v := &Verifier{
		jwksURL:      cfg.JWKSURL,
		issuer:       cfg.Issuer,
		minRefetch:   cfg.MinRefetchInterval,
		refreshAfter: cfg.RefreshAfter,
		http:         cfg.HTTPClient,
		now:          cfg.Now,
		log:          cfg.Logger,
		keys:         map[string]*ecdsa.PublicKey{},
	}
	if v.minRefetch <= 0 {
		v.minRefetch = 10 * time.Second
	}
	if v.refreshAfter <= 0 {
		v.refreshAfter = 5 * time.Minute
	}
	if v.http == nil {
		v.http = &http.Client{Timeout: 5 * time.Second}
	}
	if v.now == nil {
		v.now = time.Now
	}
	if v.log == nil {
		v.log = slog.Default()
	}
	return v, nil
}

// Verify checks a compact JWS credential and returns its claims. Every failure
// wraps ErrInvalidCredential.
func (v *Verifier) Verify(ctx context.Context, token string) (Claims, error) {
	c, err := v.verify(ctx, token)
	if err != nil {
		return Claims{}, fmt.Errorf("%w: %w", ErrInvalidCredential, err)
	}
	return c, nil
}

func (v *Verifier) verify(ctx context.Context, token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("not a compact JWS")
	}
	var h jwsHeader
	if err := decodeSegment(parts[0], &h); err != nil {
		return Claims{}, fmt.Errorf("header: %w", err)
	}
	// Pin the algorithm before anything else: none/HS*/RS*/PS* never get as far
	// as a key lookup.
	if h.Alg != CredentialAlg {
		return Claims{}, fmt.Errorf("alg %q not allowed", h.Alg)
	}
	if h.Typ != CredentialType {
		return Claims{}, fmt.Errorf("typ %q not allowed", h.Typ)
	}
	if len(h.Crit) > 0 {
		return Claims{}, errors.New("crit header not supported")
	}
	if h.Kid == "" {
		return Claims{}, errors.New("missing kid")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return Claims{}, errors.New("malformed ES256 signature")
	}
	key, err := v.key(ctx, h.Kid)
	if err != nil {
		return Claims{}, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(key, digest[:], r, s) {
		return Claims{}, errors.New("bad signature")
	}

	var c Claims
	if err := decodeSegment(parts[1], &c); err != nil {
		return Claims{}, fmt.Errorf("claims: %w", err)
	}
	return c, v.checkClaims(c)
}

func (v *Verifier) checkClaims(c Claims) error {
	switch {
	case c.Issuer != v.issuer:
		return fmt.Errorf("iss %q not trusted", c.Issuer)
	case !contains(c.Audience, CredentialAudience):
		return errors.New("aud is not jumpdrive-index")
	case c.Version != CredentialVersion:
		return fmt.Errorf("unsupported jd_ver %d", c.Version)
	case c.Subject == "" || c.Org == "" || c.Ws == "" || c.JTI == "":
		return errors.New("missing sub, jd_org, jd_ws or jti")
	case c.Access != "read" && c.Access != "full":
		return fmt.Errorf("jd_access %q not recognised", c.Access)
	case c.Spaces.Read == nil || c.Spaces.Write == nil || c.Spaces.Approve == nil:
		return errors.New("jd_spaces must carry read, write and approve")
	case c.IssuedAt == nil || c.NotEarly == nil || c.Expires == nil:
		return errors.New("missing iat, nbf or exp")
	}
	now := v.now()
	if now.After(c.Expires.Add(MaxClockSkew)) {
		return errors.New("expired")
	}
	if now.Before(c.NotEarly.Add(-MaxClockSkew)) {
		return errors.New("not yet valid")
	}
	return nil
}

// key returns the verification key for kid, refetching the JWKS when the cache
// is stale or the kid is unknown (rate-limited). On a fetch failure a cached key
// is still served; an unknown kid fails closed.
func (v *Verifier) key(ctx context.Context, kid string) (*ecdsa.PublicKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	stale := v.fetchedAt.IsZero() || v.now().Sub(v.fetchedAt) >= v.refreshAfter
	v.mu.RUnlock()
	if ok && !stale {
		return k, nil
	}
	v.refetch(ctx, !ok)
	v.mu.RLock()
	k, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	return k, nil
}

// refetch fetches the JWKS unless an attempt was made within the rate limit. A
// failed fetch keeps the cached keys (outage tolerance) and is logged.
func (v *Verifier) refetch(ctx context.Context, kidMiss bool) {
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	v.mu.RLock()
	recent := !v.triedAt.IsZero() && v.now().Sub(v.triedAt) < v.minRefetch
	v.mu.RUnlock()
	if recent {
		return
	}
	v.mu.Lock()
	v.triedAt = v.now()
	v.mu.Unlock()

	keys, err := v.fetch(ctx)
	if err != nil {
		v.log.Warn("jumpdrive: JWKS fetch failed; serving cached keys", "url", v.jwksURL, "kid_miss", kidMiss, "err", err)
		return
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = v.now()
	v.mu.Unlock()
}

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
}

func (v *Verifier) fetch(ctx context.Context) (map[string]*ecdsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := v.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return parseJWKS(set.Keys, v.log), nil
}

// parseJWKS keeps only usable ES256 signing keys; anything else in the set is
// skipped (and logged), never trusted under a different algorithm.
func parseJWKS(keys []jwk, log *slog.Logger) map[string]*ecdsa.PublicKey {
	out := make(map[string]*ecdsa.PublicKey, len(keys))
	for _, k := range keys {
		pub, err := k.publicKey()
		if err != nil {
			log.Warn("jumpdrive: skipping unusable JWKS key", "kid", k.Kid, "err", err)
			continue
		}
		out[k.Kid] = pub
	}
	return out
}

func (k jwk) publicKey() (*ecdsa.PublicKey, error) {
	switch {
	case k.Kid == "":
		return nil, errors.New("no kid")
	case k.Kty != "EC" || k.Crv != "P-256":
		return nil, fmt.Errorf("kty/crv %s/%s is not EC P-256", k.Kty, k.Crv)
	case k.Alg != "" && k.Alg != CredentialAlg:
		return nil, fmt.Errorf("alg %q is not ES256", k.Alg)
	case k.Use != "" && k.Use != "sig":
		return nil, fmt.Errorf("use %q is not sig", k.Use)
	}
	x, errX := base64.RawURLEncoding.DecodeString(k.X)
	y, errY := base64.RawURLEncoding.DecodeString(k.Y)
	if errX != nil || errY != nil || len(x) != 32 || len(y) != 32 {
		return nil, errors.New("malformed coordinates")
	}
	// ParseUncompressedPublicKey rejects a point that is not on the curve.
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
}

// decodeSegment base64url-decodes one JWS segment into v, refusing trailing data.
func decodeSegment(seg string, v any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return errors.New("bad base64url")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
