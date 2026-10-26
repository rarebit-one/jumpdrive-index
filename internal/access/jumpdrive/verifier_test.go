package jumpdrive_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/access/jumpdrive"
)

var bg = context.Background()

func newVerifier(t *testing.T, js *jwksServer, clk *clock) *jumpdrive.Verifier {
	t.Helper()
	log, _ := newLogger()
	v, err := jumpdrive.NewVerifier(jumpdrive.VerifierConfig{
		JWKSURL: js.URL, Issuer: testIssuer, Now: clk.Now, Logger: log,
		MinRefetchInterval: 10 * time.Second, RefreshAfter: 5 * time.Minute,
	})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

func TestVerifyAcceptsAValidCredential(t *testing.T) {
	clk := newClock()
	k := newTestKey(t, "kid-1")
	v := newVerifier(t, newJWKSServer(t, k.jwk(t)), clk)

	c, err := v.Verify(bg, k.mint(t, clk.Now(), nil))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "jd:acct:alice" || c.Org != "org-a" || c.Ws != "ws-a" || c.JTI != "jti-1" || c.Client != "claude-code-mcp" {
		t.Errorf("claims = %+v", c)
	}
	if len(*c.Spaces.Read) != 2 || len(*c.Spaces.Write) != 1 || len(*c.Spaces.Approve) != 0 {
		t.Errorf("spaces = %+v", c.Spaces)
	}
	// jd_client is optional.
	if _, err := v.Verify(bg, k.mint(t, clk.Now(), map[string]any{"jd_client": nil})); err != nil {
		t.Errorf("credential without jd_client: %v", err)
	}
	// aud in its array form is accepted when it contains jumpdrive-index.
	if _, err := v.Verify(bg, k.mint(t, clk.Now(), map[string]any{"aud": []string{"other", "jumpdrive-index"}})); err != nil {
		t.Errorf("array aud: %v", err)
	}
}

// Only ES256 is accepted, and the algorithm is checked before any key is
// looked up: none of these even reach the JWKS.
func TestVerifyPinsES256(t *testing.T) {
	clk := newClock()
	k := newTestKey(t, "kid-1")
	js := newJWKSServer(t, k.jwk(t))
	v := newVerifier(t, js, clk)
	claims := segment(t, claimsAt(clk.Now(), nil))

	hs := func(alg string) string {
		in := segment(t, map[string]any{"alg": alg, "kid": "kid-1", "typ": "jd-index+jwt"}) + "." + claims
		mac := hmac.New(sha256.New, []byte(k.jwk(t)["x"].(string)))
		mac.Write([]byte(in))
		return in + "." + b64.EncodeToString(mac.Sum(nil))
	}
	cases := map[string]string{
		"none":       segment(t, map[string]any{"alg": "none", "kid": "kid-1", "typ": "jd-index+jwt"}) + "." + claims + ".",
		"HS256":      hs("HS256"),
		"RS256":      hs("RS256"),
		"ES384":      hs("ES384"),
		"lowercase":  hs("es256"),
		"no alg":     hs(""),
		"not a JWS":  "opaque-bearer-token",
		"two parts":  "a.b",
		"bad base64": "!!!.!!!.!!!",
	}
	for name, tok := range cases {
		if _, err := v.Verify(bg, tok); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
			t.Errorf("%s: err=%v, want ErrInvalidCredential", name, err)
		}
	}
	if n := js.fetches(); n != 0 {
		t.Errorf("JWKS fetched %d times for credentials refused on alg/shape, want 0", n)
	}
}

func TestVerifyChecksHeaderAndClaims(t *testing.T) {
	clk := newClock()
	k := newTestKey(t, "kid-1")
	v := newVerifier(t, newJWKSServer(t, k.jwk(t)), clk)
	now := clk.Now().Unix()

	type tc struct {
		header map[string]any
		edits  map[string]any
		ok     bool
	}
	h := k.header()
	withHeader := func(key string, val any) map[string]any {
		out := map[string]any{}
		for k, v := range h {
			out[k] = v
		}
		if val == nil {
			delete(out, key)
		} else {
			out[key] = val
		}
		return out
	}
	cases := map[string]tc{
		"wrong typ":            {header: withHeader("typ", "JWT")},
		"missing typ":          {header: withHeader("typ", nil)},
		"missing kid":          {header: withHeader("kid", nil)},
		"crit header":          {header: withHeader("crit", []string{"exp"})},
		"wrong aud":            {edits: map[string]any{"aud": "jumpdrive-mcp"}},
		"aud array without us": {edits: map[string]any{"aud": []string{"a", "b"}}},
		"missing aud":          {edits: map[string]any{"aud": nil}},
		"wrong iss":            {edits: map[string]any{"iss": "https://evil.example"}},
		"iss trailing slash":   {edits: map[string]any{"iss": testIssuer + "/"}},
		"jd_ver 2":             {edits: map[string]any{"jd_ver": 2}},
		"missing jd_ver":       {edits: map[string]any{"jd_ver": nil}},
		"missing sub":          {edits: map[string]any{"sub": nil}},
		"missing jd_org":       {edits: map[string]any{"jd_org": nil}},
		"missing jd_ws":        {edits: map[string]any{"jd_ws": nil}},
		"missing jti":          {edits: map[string]any{"jti": nil}},
		"bad jd_access":        {edits: map[string]any{"jd_access": "admin"}},
		"missing jd_spaces":    {edits: map[string]any{"jd_spaces": nil}},
		"jd_spaces no write":   {edits: map[string]any{"jd_spaces": map[string]any{"read": []string{}, "approve": []string{}}}},
		"missing exp":          {edits: map[string]any{"exp": nil}},
		"missing nbf":          {edits: map[string]any{"nbf": nil}},
		"missing iat":          {edits: map[string]any{"iat": nil}},
		"expired beyond skew":  {edits: map[string]any{"iat": now - 120, "nbf": now - 120, "exp": now - 31}},
		"expired within skew":  {edits: map[string]any{"iat": now - 80, "nbf": now - 80, "exp": now - 20}, ok: true},
		"nbf beyond skew":      {edits: map[string]any{"nbf": now + 31, "exp": now + 91}},
		"nbf within skew":      {edits: map[string]any{"nbf": now + 20, "exp": now + 80}, ok: true},
		"read-only access":     {edits: map[string]any{"jd_access": "read"}, ok: true},
	}
	for name, c := range cases {
		hdr := c.header
		if hdr == nil {
			hdr = h
		}
		_, err := v.Verify(bg, k.sign(t, hdr, claimsAt(clk.Now(), c.edits)))
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: unexpected rejection: %v", name, err)
		case !c.ok && !errors.Is(err, jumpdrive.ErrInvalidCredential):
			t.Errorf("%s: err=%v, want ErrInvalidCredential", name, err)
		}
	}
}

func TestVerifyRejectsTamperingAndForeignKeys(t *testing.T) {
	clk := newClock()
	k := newTestKey(t, "kid-1")
	v := newVerifier(t, newJWKSServer(t, k.jwk(t)), clk)

	tok := k.mint(t, clk.Now(), nil)
	parts := strings.Split(tok, ".")
	escalated := segment(t, claimsAt(clk.Now(), map[string]any{
		"jd_spaces": map[string]any{"read": []string{"jd:ws:ws-b"}, "write": []string{"jd:ws:ws-b"}, "approve": []string{"jd:ws:ws-b"}},
	}))
	if _, err := v.Verify(bg, parts[0]+"."+escalated+"."+parts[2]); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
		t.Errorf("tampered claims: err=%v, want ErrInvalidCredential", err)
	}
	// A different key claiming the trusted kid.
	forger := newTestKey(t, "kid-1")
	if _, err := v.Verify(bg, forger.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
		t.Errorf("foreign key under a trusted kid: err=%v, want ErrInvalidCredential", err)
	}
	// A truncated signature.
	if _, err := v.Verify(bg, parts[0]+"."+parts[1]+"."+parts[2][:20]); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
		t.Errorf("short signature: err=%v, want ErrInvalidCredential", err)
	}
}

func TestJWKSRotationRefetchesOnKidMiss(t *testing.T) {
	clk := newClock()
	k1, k2 := newTestKey(t, "kid-1"), newTestKey(t, "kid-2")
	js := newJWKSServer(t, k1.jwk(t))
	v := newVerifier(t, js, clk)

	if _, err := v.Verify(bg, k1.mint(t, clk.Now(), nil)); err != nil {
		t.Fatalf("kid-1: %v", err)
	}
	if n := js.fetches(); n != 1 {
		t.Fatalf("fetches after first verify = %d, want 1", n)
	}
	// Cached: no refetch for a known kid.
	if _, err := v.Verify(bg, k1.mint(t, clk.Now(), nil)); err != nil {
		t.Fatalf("kid-1 again: %v", err)
	}
	if n := js.fetches(); n != 1 {
		t.Errorf("fetches for a cached kid = %d, want 1", n)
	}

	// The control plane rotates to kid-2. Past the rate limit, the first kid-2
	// credential refetches and verifies.
	js.set(k2.jwk(t))
	clk.Advance(11 * time.Second)
	if _, err := v.Verify(bg, k2.mint(t, clk.Now(), nil)); err != nil {
		t.Fatalf("kid-2 after rotation: %v", err)
	}
	if n := js.fetches(); n != 2 {
		t.Errorf("fetches after rotation = %d, want 2", n)
	}
	// The withdrawn kid-1 is gone from the refreshed cache.
	if _, err := v.Verify(bg, k1.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
		t.Errorf("withdrawn kid-1: err=%v, want ErrInvalidCredential", err)
	}
}

func TestJWKSKidMissRefetchIsRateLimited(t *testing.T) {
	clk := newClock()
	k := newTestKey(t, "kid-1")
	js := newJWKSServer(t, k.jwk(t))
	v := newVerifier(t, js, clk)
	if _, err := v.Verify(bg, k.mint(t, clk.Now(), nil)); err != nil {
		t.Fatalf("warm: %v", err)
	}

	for i := 0; i < 25; i++ {
		bogus := newTestKey(t, "made-up-"+string(rune('a'+i)))
		if _, err := v.Verify(bg, bogus.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
			t.Fatalf("unknown kid: err=%v, want ErrInvalidCredential", err)
		}
	}
	if n := js.fetches(); n != 1 {
		t.Errorf("fetches during a burst of unknown kids inside the window = %d, want 1 (no refetch)", n)
	}
	clk.Advance(11 * time.Second)
	for i := 0; i < 25; i++ {
		bogus := newTestKey(t, "other-"+string(rune('a'+i)))
		_, _ = v.Verify(bg, bogus.mint(t, clk.Now(), nil))
	}
	if n := js.fetches(); n != 2 {
		t.Errorf("fetches after the window = %d, want exactly one more (2)", n)
	}
}

// A caller that cancels while triggering the cold-cache fetch must not leave
// every other caller locked out for the cooldown.
func TestJWKSFetchSurvivesCallerCancellation(t *testing.T) {
	clk := newClock()
	k := newTestKey(t, "kid-1")
	js := newJWKSServer(t, k.jwk(t))
	v := newVerifier(t, js, clk)

	gone, cancel := context.WithCancel(bg)
	cancel()
	_, _ = v.Verify(gone, k.mint(t, clk.Now(), nil))
	if _, err := v.Verify(bg, k.mint(t, clk.Now(), nil)); err != nil {
		t.Errorf("Verify after a cancelled caller's fetch, inside the cooldown: %v", err)
	}
	if n := js.fetches(); n != 1 {
		t.Errorf("fetches = %d, want 1", n)
	}
}

func TestJWKSOutageServesCacheAndFailsClosed(t *testing.T) {
	clk := newClock()
	k1, k2 := newTestKey(t, "kid-1"), newTestKey(t, "kid-2")
	js := newJWKSServer(t, k1.jwk(t), k2.jwk(t))
	v := newVerifier(t, js, clk)
	if _, err := v.Verify(bg, k1.mint(t, clk.Now(), nil)); err != nil {
		t.Fatalf("warm: %v", err)
	}

	js.setDown(true)
	clk.Advance(10 * time.Minute) // the cache is stale, and the refresh fails
	if _, err := v.Verify(bg, k1.mint(t, clk.Now(), nil)); err != nil {
		t.Errorf("known kid during a JWKS outage: %v, want served from cache", err)
	}
	if _, err := v.Verify(bg, k2.mint(t, clk.Now(), nil)); err != nil {
		t.Errorf("second cached kid during the outage: %v", err)
	}
	unknown := newTestKey(t, "kid-3")
	clk.Advance(time.Minute)
	if _, err := v.Verify(bg, unknown.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
		t.Errorf("unknown kid during the outage: err=%v, want ErrInvalidCredential (fail closed)", err)
	}
	if js.fetches() < 2 {
		t.Error("the stale cache should have attempted a refresh")
	}
}

// A JWKS that never loaded (outage from boot) verifies nothing.
func TestJWKSUnavailableFromBootFailsClosed(t *testing.T) {
	clk := newClock()
	k := newTestKey(t, "kid-1")
	js := newJWKSServer(t, k.jwk(t))
	js.setDown(true)
	v := newVerifier(t, js, clk)
	if _, err := v.Verify(bg, k.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
		t.Errorf("err=%v, want ErrInvalidCredential", err)
	}
	// An empty set (the control plane with no signing key) is the same.
	js.setDown(false)
	js.set()
	clk.Advance(11 * time.Second)
	if _, err := v.Verify(bg, k.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
		t.Errorf("empty JWKS: err=%v, want ErrInvalidCredential", err)
	}
}

func TestJWKSSkipsUnusableKeys(t *testing.T) {
	clk := newClock()
	good := newTestKey(t, "good")
	bad := map[string]map[string]any{}
	for name, mutate := range map[string]func(m map[string]any){
		"rsa":       func(m map[string]any) { m["kty"] = "RSA" },
		"p384":      func(m map[string]any) { m["crv"] = "P-384" },
		"alg rs256": func(m map[string]any) { m["alg"] = "RS256" },
		"use enc":   func(m map[string]any) { m["use"] = "enc" },
		"off curve": func(m map[string]any) { m["y"] = m["x"] },
		"short x":   func(m map[string]any) { m["x"] = "AAAA" },
	} {
		k := newTestKey(t, name)
		m := k.jwk(t)
		mutate(m)
		bad[name] = m
	}
	keys := []map[string]any{good.jwk(t)}
	for _, m := range bad {
		keys = append(keys, m)
	}
	v := newVerifier(t, newJWKSServer(t, keys...), clk)
	if _, err := v.Verify(bg, good.mint(t, clk.Now(), nil)); err != nil {
		t.Fatalf("good key: %v", err)
	}
	for name := range bad {
		k := newTestKey(t, name) // signer is irrelevant: the kid must not resolve
		if _, err := v.Verify(bg, k.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrInvalidCredential) {
			t.Errorf("%s key trusted: err=%v", name, err)
		}
	}
}

func TestNewVerifierRequiresURLAndIssuer(t *testing.T) {
	if _, err := jumpdrive.NewVerifier(jumpdrive.VerifierConfig{Issuer: testIssuer}); err == nil {
		t.Error("empty JWKS URL accepted")
	}
	if _, err := jumpdrive.NewVerifier(jumpdrive.VerifierConfig{JWKSURL: "https://x/jwks.json"}); err == nil {
		t.Error("empty issuer accepted")
	}
}
