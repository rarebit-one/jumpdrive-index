// Package jumpdrive is the hosted access model: the caller's identity comes from
// Jumpdrive's control plane, and the hard-ACL questions are answered locally with
// the SAME rules as the starchart model (public / space / private + a restricted
// @type gate). Only WHERE the principal comes from differs.
//
// The bearer is the per-call credential the control plane mints for every
// entity_* call (jumpdrive-web docs/index-principal-adr.md, Decisions 2 and 3):
// a 60-second ES256 JWT bound to one account, org and workspace, carrying the
// read/write/approve space sets. JDX_JUMPDRIVE_VERIFY picks how it is trusted:
//
//   - jwks: verify the signature locally against <JDX_JUMPDRIVE_URL>/jwks.json
//     and read the sets from the claims. No callback on the hot path; the
//     production end state.
//   - authorize: POST the bearer to <JDX_JUMPDRIVE_URL>/authorize, the control
//     plane's resolver of record, and use its decision. For non-proxy
//     credentials and as an incident fallback.
//   - dual: run both, enforce the INTERSECTION (a space counts only if both
//     grant it), and log every mismatch. For the staged rollout.
//
// The authorize client is shaped like jumpdrive-web's RunnerClient: base URL from
// config, a REQUIRED service-secret bearer, explicit timeouts, a propagated
// X-Request-Id, a typed error, and a narrow httpDoer seam for testing.
//
// PROVISIONAL: the Jumpdrive `/authorize` request/response contract below is not
// yet ratified against jumpdrive-web; treat it as a proposal.
package jumpdrive

import (
	"bytes"
	"container/list"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// ErrUnauthenticated is returned when the credential is rejected: by the
// authorizer (401/403), or by local verification.
var ErrUnauthenticated = errors.New("jumpdrive: credential rejected")

// ErrAuthorizer is returned when the authorizer call itself fails (network,
// timeout, unexpected status or contract).
var ErrAuthorizer = errors.New("jumpdrive: authorizer request failed")

// AuthorizeContract is the /authorize response contract this client speaks.
const AuthorizeContract = "jumpdrive-authorize/v1"

// httpDoer is the minimal HTTP seam (satisfied by *http.Client), so tests can
// drive the model against an httptest.Server or a fake.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// VerifyMode selects how a credential is trusted (JDX_JUMPDRIVE_VERIFY).
type VerifyMode string

// VerifyMode values.
const (
	VerifyJWKS      VerifyMode = "jwks"
	VerifyAuthorize VerifyMode = "authorize"
	VerifyDual      VerifyMode = "dual"
)

// Config configures the model.
type Config struct {
	// BaseURL is the control plane's index route, <console>/internal/index. The
	// model appends /authorize and /jwks.json.
	BaseURL string
	// SharedSecret is the index→control-plane service bearer for /authorize
	// (JDX_JUMPDRIVE_SECRET = the control plane's
	// JUMPDRIVE_INDEX_AUTHORIZE_SECRET). Required in authorize and dual modes.
	SharedSecret string
	// Mode is the verification mode. Required.
	Mode VerifyMode
	// Issuer is the `iss` a credential must carry: the control plane's base URL.
	// Required in jwks and dual modes.
	Issuer string

	RestrictedDenyTypes []domain.Type
	Timeout             time.Duration // per authorize/JWKS request; default 5s
	HTTPClient          httpDoer      // default *http.Client with Timeout
	// CacheSize bounds the authorize decision cache. Default 1024.
	CacheSize int
	// Verifier overrides the JWKS verifier built from BaseURL/Issuer (tests).
	Verifier *Verifier
	Logger   *slog.Logger
	Now      func() time.Time
}

// Model is the jumpdrive access.Model.
type Model struct {
	baseURL   string
	secret    string
	mode      VerifyMode
	denyTypes []domain.Type
	http      httpDoer
	timeout   time.Duration
	verifier  *Verifier
	cache     *decisionCache
	log       *slog.Logger
	now       func() time.Time
}

var _ access.Model = (*Model)(nil)

// New builds the model, refusing a configuration that cannot verify anything.
func New(cfg Config) (*Model, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("jumpdrive: empty BaseURL")
	}
	switch cfg.Mode {
	case VerifyJWKS, VerifyAuthorize, VerifyDual:
	default:
		return nil, fmt.Errorf("jumpdrive: unrecognised verify mode %q (want jwks|authorize|dual)", cfg.Mode)
	}
	if cfg.Mode != VerifyJWKS && cfg.SharedSecret == "" {
		return nil, fmt.Errorf("jumpdrive: verify mode %q needs the authorize service secret", cfg.Mode)
	}
	to := cfg.Timeout
	if to <= 0 {
		to = 5 * time.Second
	}
	h := cfg.HTTPClient
	if h == nil {
		h = &http.Client{Timeout: to}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	m := &Model{
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		secret:    cfg.SharedSecret,
		mode:      cfg.Mode,
		denyTypes: append([]domain.Type(nil), cfg.RestrictedDenyTypes...),
		http:      h,
		timeout:   to,
		cache:     newDecisionCache(cfg.CacheSize),
		log:       log,
		now:       now,
	}
	if cfg.Mode != VerifyAuthorize {
		m.verifier = cfg.Verifier
		if m.verifier == nil {
			v, err := NewVerifier(VerifierConfig{
				JWKSURL: m.baseURL + "/jwks.json", Issuer: cfg.Issuer, HTTPClient: h, Now: now, Logger: log,
			})
			if err != nil {
				return nil, err
			}
			m.verifier = v
		}
	}
	return m, nil
}

// Mode reports the configured verification mode.
func (m *Model) Mode() VerifyMode { return m.mode }

// Authenticate resolves the bearer to a decision under the configured mode. The
// read, write and approve sets all travel on the returned decision; nothing is
// cached per principal, so two concurrent requests from one user in different
// workspaces never see each other's rights. ctx is the caller's request context.
func (m *Model) Authenticate(ctx context.Context, bearer string) (access.Decision, error) {
	if bearer == "" {
		return access.Decision{}, ErrUnauthenticated
	}
	switch m.mode {
	case VerifyJWKS:
		return m.authenticateJWKS(ctx, bearer)
	case VerifyAuthorize:
		return m.authorize(ctx, bearer)
	default:
		return m.authenticateDual(ctx, bearer)
	}
}

func (m *Model) authenticateJWKS(ctx context.Context, bearer string) (access.Decision, error) {
	c, err := m.verifier.Verify(ctx, bearer)
	if err != nil {
		return access.Decision{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	return decisionFromClaims(c), nil
}

// decisionFromClaims maps verified claims to a decision. Jumpdrive has no
// child-safety principal, so a credential is never Restricted.
func decisionFromClaims(c Claims) access.Decision {
	return access.Decision{
		Principal: access.Principal{
			ID:             domain.PrincipalID(c.Subject),
			Spaces:         toSpaces(*c.Spaces.Read),
			WriteSpaces:    toSpaces(*c.Spaces.Write),
			ApproverSpaces: toSpaces(*c.Spaces.Approve),
		},
		Source: ProvenanceSource(c.Org, c.Ws, c.JTI, c.Client),
	}
}

// authenticateDual runs both paths and enforces their intersection. Either path
// failing denies; a principal mismatch denies; any difference in the sets is
// logged (without the bearer) and resolved to the narrower answer.
func (m *Model) authenticateDual(ctx context.Context, bearer string) (access.Decision, error) {
	jd, jerr := m.authenticateJWKS(ctx, bearer)
	ad, aerr := m.authorize(ctx, bearer)
	switch {
	case jerr != nil && aerr != nil:
		return access.Decision{}, jerr
	case jerr != nil:
		m.log.Warn("jumpdrive dual: mismatch", "kind", "jwks_rejected_authorize_accepted", "principal", ad.Principal.ID, "err", jerr)
		return access.Decision{}, jerr
	case aerr != nil:
		if errors.Is(aerr, ErrUnauthenticated) {
			m.log.Warn("jumpdrive dual: mismatch", "kind", "authorize_rejected_jwks_accepted", "principal", jd.Principal.ID, "source", jd.Source, "err", aerr)
		}
		return access.Decision{}, aerr
	}
	if jd.Principal.ID != ad.Principal.ID {
		m.log.Warn("jumpdrive dual: mismatch", "kind", "principal", "jwks", jd.Principal.ID, "authorize", ad.Principal.ID, "source", jd.Source)
		return access.Decision{}, fmt.Errorf("%w: dual verification disagrees on the principal", ErrUnauthenticated)
	}
	out := jd
	out.Principal.Spaces = intersect(jd.Principal.Spaces, ad.Principal.Spaces)
	out.Principal.WriteSpaces = intersect(jd.Principal.WriteSpaces, ad.Principal.WriteSpaces)
	out.Principal.ApproverSpaces = intersect(jd.Principal.ApproverSpaces, ad.Principal.ApproverSpaces)
	out.Principal.Restricted = jd.Principal.Restricted || ad.Principal.Restricted
	for _, set := range []struct {
		name     string
		jwks, au []domain.SpaceID
	}{
		{"read", jd.Principal.Spaces, ad.Principal.Spaces},
		{"write", jd.Principal.WriteSpaces, ad.Principal.WriteSpaces},
		{"approve", jd.Principal.ApproverSpaces, ad.Principal.ApproverSpaces},
	} {
		if !sameSet(set.jwks, set.au) {
			m.log.Warn("jumpdrive dual: mismatch", "kind", "spaces", "set", set.name, "principal", jd.Principal.ID,
				"source", jd.Source, "jwks", set.jwks, "authorize", set.au)
		}
	}
	if out.Principal.Restricted != jd.Principal.Restricted {
		m.log.Warn("jumpdrive dual: mismatch", "kind", "restricted", "principal", jd.Principal.ID, "source", jd.Source)
	}
	return out, nil
}

// authorizeRequest / authorizeResponse are the /authorize wire contract.
type authorizeRequest struct {
	Bearer string `json:"bearer"`
}

// Spaces is the READ set. WriteSpaces and ApproverSpaces are separate sets; a
// response that omits either grants nothing there (fail closed).
type authorizeResponse struct {
	Contract       string   `json:"contract"`
	PrincipalID    string   `json:"principal_id"`
	Spaces         []string `json:"spaces"`
	WriteSpaces    []string `json:"write_spaces"`
	ApproverSpaces []string `json:"approver_spaces"`
	Restricted     bool     `json:"restricted"`
	OrgID          string   `json:"org_id"`
	WorkspaceID    string   `json:"workspace_id"`
	Access         string   `json:"access"`
	ExpiresAt      string   `json:"expires_at"`
}

type authorizeError struct {
	Error string `json:"error"`
}

// authorize asks the control plane's resolver of record, through a bounded cache
// keyed by sha256(bearer). A decision is cached for min(max-age, expires_at);
// failures are never cached.
func (m *Model) authorize(ctx context.Context, bearer string) (access.Decision, error) {
	key := sha256.Sum256([]byte(bearer))
	if d, ok := m.cache.get(key, m.now()); ok {
		return d, nil
	}

	body, err := json.Marshal(authorizeRequest{Bearer: bearer})
	if err != nil {
		return access.Decision{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/authorize", bytes.NewReader(body))
	if err != nil {
		return access.Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", requestID(ctx))
	req.Header.Set("Authorization", "Bearer "+m.secret)

	resp, err := m.http.Do(req)
	if err != nil {
		return access.Decision{}, fmt.Errorf("%w: %w", ErrAuthorizer, err)
	}
	defer func() { _ = resp.Body.Close() }()
	lr := io.LimitReader(resp.Body, 1<<20)

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		var ae authorizeError
		_ = json.NewDecoder(lr).Decode(&ae)
		if ae.Error == "invalid_service_secret" {
			// Our own credential is wrong: an operator problem, not a user one.
			m.log.Error("jumpdrive: the control plane rejected JDX_JUMPDRIVE_SECRET; every authorize call will fail",
				"url", m.baseURL+"/authorize")
		}
		return access.Decision{}, fmt.Errorf("%w: authorize %d %s", ErrUnauthenticated, resp.StatusCode, ae.Error)
	case resp.StatusCode != http.StatusOK:
		return access.Decision{}, fmt.Errorf("%w: status %d", ErrAuthorizer, resp.StatusCode)
	}

	var ar authorizeResponse
	if err := json.NewDecoder(lr).Decode(&ar); err != nil {
		return access.Decision{}, fmt.Errorf("%w: decode: %w", ErrAuthorizer, err)
	}
	if ar.Contract != AuthorizeContract {
		return access.Decision{}, fmt.Errorf("%w: unexpected contract %q (want %s)", ErrAuthorizer, ar.Contract, AuthorizeContract)
	}
	if ar.PrincipalID == "" {
		return access.Decision{}, ErrUnauthenticated
	}

	d := access.Decision{
		Principal: access.Principal{
			ID:             domain.PrincipalID(ar.PrincipalID),
			Spaces:         toSpaces(ar.Spaces),
			WriteSpaces:    toSpaces(ar.WriteSpaces),
			ApproverSpaces: toSpaces(ar.ApproverSpaces),
			Restricted:     ar.Restricted,
		},
		Source: ProvenanceSource(ar.OrgID, ar.WorkspaceID, "", ""),
	}
	if until, ok := cacheUntil(m.now(), resp.Header.Get("Cache-Control"), ar.ExpiresAt); ok {
		m.cache.put(key, d, until)
	}
	return d, nil
}

// cacheUntil is when a decision stops being reusable: the earlier of now +
// max-age and the credential's expires_at. No max-age, no-store, or a missing
// or past expires_at means do not cache.
func cacheUntil(now time.Time, cacheControl, expiresAt string) (time.Time, bool) {
	maxAge := -1
	for _, dir := range strings.Split(cacheControl, ",") {
		dir = strings.ToLower(strings.TrimSpace(dir))
		switch {
		case dir == "no-store" || dir == "no-cache":
			return time.Time{}, false
		case strings.HasPrefix(dir, "max-age="):
			if n, err := strconv.Atoi(strings.TrimPrefix(dir, "max-age=")); err == nil && n >= 0 {
				maxAge = n
			}
		}
	}
	if maxAge <= 0 {
		return time.Time{}, false
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil {
		return time.Time{}, false
	}
	until := now.Add(time.Duration(maxAge) * time.Second)
	if exp.Before(until) {
		until = exp
	}
	if !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}

// decisionCache is a bounded LRU of authorize decisions with per-entry expiry.
type decisionCache struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[[32]byte]*list.Element
}

type cacheEntry struct {
	key   [32]byte
	d     access.Decision
	until time.Time
}

func newDecisionCache(max int) *decisionCache {
	if max <= 0 {
		max = 1024
	}
	return &decisionCache{max: max, ll: list.New(), items: map[[32]byte]*list.Element{}}
}

func (c *decisionCache) get(k [32]byte, now time.Time) (access.Decision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return access.Decision{}, false
	}
	e := el.Value.(*cacheEntry)
	if !now.Before(e.until) {
		c.ll.Remove(el)
		delete(c.items, k)
		return access.Decision{}, false
	}
	c.ll.MoveToFront(el)
	return e.d, true
}

func (c *decisionCache) put(k [32]byte, d access.Decision, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		el.Value = &cacheEntry{key: k, d: d, until: until}
		c.ll.MoveToFront(el)
		return
	}
	c.items[k] = c.ll.PushFront(&cacheEntry{key: k, d: d, until: until})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*cacheEntry).key)
	}
}

func intersect(a, b []domain.SpaceID) []domain.SpaceID {
	out := make([]domain.SpaceID, 0, len(a))
	for _, s := range a {
		if access.ContainsSpace(b, s) && !access.ContainsSpace(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func sameSet(a, b []domain.SpaceID) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

// ProvenanceSource renders the request trace a jumpdrive decision stamps on
// every write as Provenance.Source: the control-plane org and workspace the
// credential was bound to, its jti and the OAuth client, under their claim
// names. Empty parts are left out, so the format is
// "jumpdrive;jd_org=<uuid>;jd_ws=<uuid>;jti=<uuid>;jd_client=<id>".
func ProvenanceSource(org, ws, jti, client string) string {
	var b strings.Builder
	b.WriteString("jumpdrive")
	for _, kv := range [][2]string{{"jd_org", org}, {"jd_ws", ws}, {"jti", jti}, {"jd_client", client}} {
		if kv[1] != "" {
			b.WriteString(";" + kv[0] + "=" + kv[1])
		}
	}
	return b.String()
}

func toSpaces(in []string) []domain.SpaceID {
	out := make([]domain.SpaceID, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, domain.SpaceID(s))
		}
	}
	return out
}

// FilterFor builds the SQL-pushdown Filter (same shape as starchart).
func (m *Model) FilterFor(d access.Decision) access.Filter {
	f := access.Filter{
		Principal:   d.Principal.ID,
		Spaces:      d.Principal.Spaces,
		Restricted:  d.Principal.Restricted,
		AllowPublic: true,
	}
	if d.Principal.Restricted {
		f.DenyTypes = m.denyTypes
	}
	return f
}

// CanRead is the hard ACL, identical to starchart's and to the SQLite
// accessWhere clause.
func (m *Model) CanRead(d access.Decision, g access.Guarded) bool {
	if d.Principal.Restricted && containsType(m.denyTypes, g.Type) {
		return false
	}
	switch g.Visibility {
	case domain.VisPublic:
		return true
	case domain.VisSpace:
		return access.ContainsSpace(d.Principal.Spaces, g.Space)
	case domain.VisPrivate:
		return g.Owner == d.Principal.ID
	default:
		return false
	}
}

// CanWrite reports whether the decision grants writes into space. The write set
// is separate from the read set: a read-only (mcp:read) credential carries an
// empty one.
func (m *Model) CanWrite(d access.Decision, space domain.SpaceID) bool {
	return access.ContainsSpace(d.Principal.WriteSpaces, space)
}

// CanApprove reports whether the decision grants approving in space, from the
// approver set carried on the decision itself.
func (m *Model) CanApprove(d access.Decision, space domain.SpaceID) bool {
	return access.ContainsSpace(d.Principal.ApproverSpaces, space)
}

// requestID propagates the caller's X-Request-Id (access.WithRequestID) or
// mints one.
func requestID(ctx context.Context) string {
	if id := access.RequestID(ctx); id != "" {
		return id
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "jdx-req"
	}
	return hex.EncodeToString(b[:])
}

func containsType(types []domain.Type, t domain.Type) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}
