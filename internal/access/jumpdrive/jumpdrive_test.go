package jumpdrive_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/access/jumpdrive"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// authorizeServer is a fake control-plane /authorize. decide maps a bearer to a
// response body (nil → 401 invalid_credential).
type authorizeServer struct {
	*httptest.Server
	hits     atomic.Int64
	mu       sync.Mutex
	lastReq  *http.Request
	lastBody map[string]any
}

func newAuthorizeServer(t *testing.T, decide func(bearer string) (map[string]any, http.Header)) *authorizeServer {
	t.Helper()
	as := &authorizeServer{}
	as.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as.hits.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		as.mu.Lock()
		as.lastReq, as.lastBody = r, body
		as.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/internal/index/authorize" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer svc-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_service_secret"}`))
			return
		}
		bearer, _ := body["bearer"].(string)
		resp, hdr := decide(bearer)
		if resp == nil {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_credential"}`))
			return
		}
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(as.Close)
	return as
}

func (as *authorizeServer) base() string { return as.URL + "/internal/index" }

func authorizeBody(pid string, read, write, approve []string, edits map[string]any) map[string]any {
	b := map[string]any{
		"contract": "jumpdrive-authorize/v1", "principal_id": pid,
		"spaces": read, "restricted": false,
		"org_id": "org-a", "workspace_id": "ws-a", "access": "full",
		"expires_at": "2026-10-26T03:01:00Z",
	}
	if write != nil {
		b["write_spaces"] = write
	}
	if approve != nil {
		b["approver_spaces"] = approve
	}
	for k, v := range edits {
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
	}
	return b
}

func newAuthorizeModel(t *testing.T, base string, edit func(*jumpdrive.Config)) (*jumpdrive.Model, *logBuffer) {
	t.Helper()
	log, lb := newLogger()
	cfg := jumpdrive.Config{BaseURL: base, SharedSecret: "svc-secret", Mode: jumpdrive.VerifyAuthorize, Logger: log}
	if edit != nil {
		edit(&cfg)
	}
	m, err := jumpdrive.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, lb
}

func TestAuthorizeHappyPath(t *testing.T) {
	as := newAuthorizeServer(t, func(bearer string) (map[string]any, http.Header) {
		if bearer != "good" {
			return nil, nil
		}
		return authorizeBody("alice", []string{"fam", "proj"}, nil, []string{"fam"}, map[string]any{"restricted": true}), nil
	})
	m, _ := newAuthorizeModel(t, as.base(), func(c *jumpdrive.Config) { c.RestrictedDenyTypes = []domain.Type{"Movie"} })

	ctx := access.WithRequestID(bg, "req-123")
	d, err := m.Authenticate(ctx, "good")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if d.Principal.ID != "alice" || len(d.Principal.Spaces) != 2 || !d.Principal.Restricted {
		t.Errorf("mapped principal wrong: %+v", d.Principal)
	}
	as.mu.Lock()
	req, body := as.lastReq, as.lastBody
	as.mu.Unlock()
	if got := req.Header.Get("Authorization"); got != "Bearer svc-secret" {
		t.Errorf("service secret not sent: Authorization=%q", got)
	}
	if got := req.Header.Get("X-Request-Id"); got != "req-123" {
		t.Errorf("X-Request-Id = %q, want the propagated req-123", got)
	}
	if got := req.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if len(body) != 1 || body["bearer"] != "good" {
		t.Errorf("request body = %v, want exactly {bearer}", body)
	}

	// The delegate applies the SAME hard ACL as starchart.
	if m.CanRead(d, access.Guarded{Visibility: domain.VisPublic, Type: "Movie"}) {
		t.Error("restricted principal must not read a deny-type Movie")
	}
	if !m.CanRead(d, access.Guarded{Visibility: domain.VisPublic, Type: "Note"}) {
		t.Error("restricted principal should still read a non-deny public type")
	}
	if !m.CanApprove(d, "fam") || m.CanApprove(d, "proj") {
		t.Error("approver set not taken from the decision")
	}
	f := m.FilterFor(d)
	if !f.Restricted || len(f.DenyTypes) != 1 {
		t.Errorf("restricted filter wrong: %+v", f)
	}
	// The response carried no write_spaces: absent means [] (fail closed), even
	// for spaces the principal reads.
	if m.CanWrite(d, "fam") || m.CanWrite(d, "proj") {
		t.Error("absent write_spaces must grant no writes")
	}
	// Without an inbound id, one is minted.
	if _, err := m.Authenticate(bg, "good"); err != nil {
		t.Fatal(err)
	}
	as.mu.Lock()
	minted := as.lastReq.Header.Get("X-Request-Id")
	as.mu.Unlock()
	if minted == "" || minted == "req-123" {
		t.Errorf("minted X-Request-Id = %q", minted)
	}
}

func TestAuthorizeRejections(t *testing.T) {
	as := newAuthorizeServer(t, func(bearer string) (map[string]any, http.Header) {
		switch bearer {
		case "wrong-contract":
			return authorizeBody("alice", nil, nil, nil, map[string]any{"contract": "jumpdrive-authorize/v2"}), nil
		case "no-contract":
			return authorizeBody("alice", nil, nil, nil, map[string]any{"contract": nil}), nil
		case "no-principal":
			return authorizeBody("", nil, nil, nil, nil), nil
		}
		return nil, nil
	})
	m, lb := newAuthorizeModel(t, as.base(), nil)

	if _, err := m.Authenticate(bg, "bad"); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
		t.Errorf("401: err=%v, want ErrUnauthenticated", err)
	}
	if _, err := m.Authenticate(bg, ""); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
		t.Errorf("empty bearer: err=%v, want ErrUnauthenticated", err)
	}
	for _, tok := range []string{"wrong-contract", "no-contract"} {
		if _, err := m.Authenticate(bg, tok); !errors.Is(err, jumpdrive.ErrAuthorizer) {
			t.Errorf("%s: err=%v, want ErrAuthorizer", tok, err)
		}
	}
	if _, err := m.Authenticate(bg, "no-principal"); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
		t.Errorf("no principal: err=%v, want ErrUnauthenticated", err)
	}
	if strings.Contains(lb.String(), "level=ERROR") {
		t.Errorf("a user-credential rejection must not log at error level:\n%s", lb)
	}

	// A wrong service secret is an operator problem: still a rejection, but
	// logged at error level, without the secret.
	bad, lb2 := newAuthorizeModel(t, as.base(), func(c *jumpdrive.Config) { c.SharedSecret = "stale-secret" })
	if _, err := bad.Authenticate(bg, "good"); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
		t.Errorf("bad service secret: err=%v, want ErrUnauthenticated", err)
	}
	if out := lb2.String(); !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "JDX_JUMPDRIVE_SECRET") || strings.Contains(out, "stale-secret") {
		t.Errorf("service-secret rejection log = %q", out)
	}
}

func TestAuthorizeServerErrorIsAuthorizerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	m, _ := newAuthorizeModel(t, srv.URL, nil)
	if _, err := m.Authenticate(bg, "x"); !errors.Is(err, jumpdrive.ErrAuthorizer) {
		t.Errorf("err=%v, want ErrAuthorizer", err)
	}
}

// The request's context reaches the authorizer call: cancelling it aborts the
// call instead of waiting for a background timeout.
func TestAuthorizePropagatesContextCancellation(t *testing.T) {
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Consume the body so the server watches the connection for a close.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		close(released)
	}))
	defer srv.Close()
	m, _ := newAuthorizeModel(t, srv.URL, func(c *jumpdrive.Config) { c.Timeout = 20 * time.Second })

	ctx, cancel := context.WithCancel(bg)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := m.Authenticate(ctx, "x")
	if !errors.Is(err, jumpdrive.ErrAuthorizer) || !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want ErrAuthorizer wrapping context.Canceled", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("Authenticate returned after %v; cancellation did not propagate", el)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Error("the server never saw the request cancelled")
	}
}

func TestAuthorizeCacheRespectsMaxAgeExpiresAtAndBound(t *testing.T) {
	clk := newClock() // 2026-10-26T03:00:00Z
	var expMu sync.Mutex
	exp := clk.Now().Add(45 * time.Second).UTC().Format(time.RFC3339)
	as := newAuthorizeServer(t, func(bearer string) (map[string]any, http.Header) {
		expMu.Lock()
		exp := exp
		expMu.Unlock()
		switch {
		case strings.HasPrefix(bearer, "nostore"):
			return authorizeBody("alice", []string{"s"}, nil, nil, map[string]any{"expires_at": exp}),
				http.Header{"Cache-Control": {"no-store"}}
		case strings.HasPrefix(bearer, "noexp"):
			return authorizeBody("alice", []string{"s"}, nil, nil, map[string]any{"expires_at": nil}),
				http.Header{"Cache-Control": {"max-age=30, private"}}
		case strings.HasPrefix(bearer, "ok"):
			return authorizeBody("alice", []string{"s"}, nil, nil, map[string]any{"expires_at": exp}),
				http.Header{"Cache-Control": {"max-age=30, private"}}
		}
		return nil, nil
	})
	m, _ := newAuthorizeModel(t, as.base(), func(c *jumpdrive.Config) { c.Now = clk.Now; c.CacheSize = 2 })
	call := func(tok string) {
		t.Helper()
		if _, err := m.Authenticate(bg, tok); err != nil && !strings.HasPrefix(tok, "bad") {
			t.Fatalf("Authenticate(%s): %v", tok, err)
		}
	}

	call("ok-1")
	call("ok-1")
	if n := as.hits.Load(); n != 1 {
		t.Fatalf("hits after a cached repeat = %d, want 1", n)
	}
	// max-age=30 bounds it (expires_at is 45s out).
	clk.Advance(31 * time.Second)
	call("ok-1")
	if n := as.hits.Load(); n != 2 {
		t.Errorf("hits after max-age = %d, want 2", n)
	}
	// Re-cached at t=31 for min(31+30, 45) = 45: expires_at wins.
	clk.Advance(10 * time.Second) // t=41: still cached
	call("ok-1")
	if n := as.hits.Load(); n != 2 {
		t.Errorf("hits inside expires_at = %d, want 2", n)
	}
	clk.Advance(5 * time.Second) // t=46: past expires_at
	call("ok-1")
	if n := as.hits.Load(); n != 3 {
		t.Errorf("hits past expires_at = %d, want 3 (never cached past the credential's exp)", n)
	}

	// no-store and a missing expires_at are never cached; nor are failures.
	for _, tok := range []string{"nostore-1", "nostore-1", "noexp-1", "noexp-1", "bad-1", "bad-1"} {
		call(tok)
	}
	if n := as.hits.Load(); n != 9 {
		t.Errorf("hits for uncacheable answers = %d, want 9", n)
	}

	// The cache is bounded (2): a third bearer evicts the least recent.
	clk.Advance(time.Hour)
	expMu.Lock()
	exp = clk.Now().Add(60 * time.Second).UTC().Format(time.RFC3339)
	expMu.Unlock()
	base := as.hits.Load()
	call("ok-a")
	call("ok-b")
	call("ok-c") // evicts ok-a
	call("ok-b")
	call("ok-c")
	if n := as.hits.Load() - base; n != 3 {
		t.Errorf("hits for a,b,c,b,c = %d, want 3", n)
	}
	call("ok-a")
	if n := as.hits.Load() - base; n != 4 {
		t.Errorf("ok-a should have been evicted (hits=%d, want 4)", n)
	}
}

// One user, two workspace-bound credentials: each decision carries only its own
// write and approve sets. The old per-principal approver cache let the latest
// Authenticate overwrite the earlier request's rights.
func TestDecisionsForOnePrincipalDoNotShareRights(t *testing.T) {
	as := newAuthorizeServer(t, func(bearer string) (map[string]any, http.Header) {
		ws := map[string]string{"tok-ws1": "jd:ws:1", "tok-ws2": "jd:ws:2"}[bearer]
		if ws == "" {
			return nil, nil
		}
		return authorizeBody("jd:acct:alice", []string{ws, "jd:org:a"}, []string{ws}, []string{ws},
			map[string]any{"org_id": "a", "workspace_id": strings.TrimPrefix(ws, "jd:ws:")}), nil
	})
	m, _ := newAuthorizeModel(t, as.base(), nil)

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, own, other := "tok-ws1", domain.SpaceID("jd:ws:1"), domain.SpaceID("jd:ws:2")
			if i%2 == 1 {
				tok, own, other = "tok-ws2", other, own
			}
			d, err := m.Authenticate(bg, tok)
			if err != nil {
				t.Errorf("Authenticate(%s): %v", tok, err)
				return
			}
			if !m.CanApprove(d, own) || !m.CanWrite(d, own) {
				t.Errorf("%s lost its own rights in %s", tok, own)
			}
			if m.CanApprove(d, other) || m.CanWrite(d, other) {
				t.Errorf("%s gained rights in %s from a concurrent decision", tok, other)
			}
			if want := "jumpdrive;jd_org=a;jd_ws=" + strings.TrimPrefix(string(own), "jd:ws:"); d.Source != want {
				t.Errorf("source = %q, want %q", d.Source, want)
			}
		}()
	}
	wg.Wait()
}

func TestProvenanceSourceOmitsEmptyParts(t *testing.T) {
	if got := jumpdrive.ProvenanceSource("o", "w", "j", "c"); got != "jumpdrive;jd_org=o;jd_ws=w;jti=j;jd_client=c" {
		t.Errorf("full source = %q", got)
	}
	if got := jumpdrive.ProvenanceSource("o", "", "j", ""); got != "jumpdrive;jd_org=o;jti=j" {
		t.Errorf("partial source = %q", got)
	}
}

func TestNewRefusesUnusableConfig(t *testing.T) {
	cases := map[string]jumpdrive.Config{
		"no base URL":                {Mode: jumpdrive.VerifyJWKS, Issuer: testIssuer},
		"no mode":                    {BaseURL: "https://c/internal/index", SharedSecret: "s", Issuer: testIssuer},
		"unknown mode":               {BaseURL: "https://c/internal/index", SharedSecret: "s", Issuer: testIssuer, Mode: "both"},
		"authorize without a secret": {BaseURL: "https://c/internal/index", Mode: jumpdrive.VerifyAuthorize},
		"dual without a secret":      {BaseURL: "https://c/internal/index", Mode: jumpdrive.VerifyDual, Issuer: testIssuer},
		"jwks without an issuer":     {BaseURL: "https://c/internal/index", Mode: jumpdrive.VerifyJWKS, SharedSecret: "s"},
		"dual without an issuer":     {BaseURL: "https://c/internal/index", Mode: jumpdrive.VerifyDual, SharedSecret: "s"},
	}
	for name, cfg := range cases {
		if _, err := jumpdrive.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// ---- jwks and dual modes ----

// controlPlane serves both /jwks.json and /authorize under /internal/index, the
// way the real control plane does.
type controlPlane struct {
	key      testKey
	jwks     *jwksServer
	auth     *authorizeServer
	mux      *httptest.Server
	authDown atomic.Bool
}

func newControlPlane(t *testing.T, decide func(bearer string) (map[string]any, http.Header)) *controlPlane {
	t.Helper()
	cp := &controlPlane{key: newTestKey(t, "kid-1")}
	cp.jwks = newJWKSServer(t, cp.key.jwk(t))
	cp.auth = newAuthorizeServer(t, decide)
	cp.mux = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/internal/index/jwks.json":
			cp.jwks.Config.Handler.ServeHTTP(w, r)
		case cp.authDown.Load():
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			cp.auth.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(cp.mux.Close)
	return cp
}

func (cp *controlPlane) model(t *testing.T, mode jumpdrive.VerifyMode, clk *clock) (*jumpdrive.Model, *logBuffer) {
	t.Helper()
	log, lb := newLogger()
	m, err := jumpdrive.New(jumpdrive.Config{
		BaseURL: cp.mux.URL + "/internal/index", SharedSecret: "svc-secret", Mode: mode,
		Issuer: testIssuer, Logger: log, Now: clk.Now,
	})
	if err != nil {
		t.Fatalf("New(%s): %v", mode, err)
	}
	return m, lb
}

func TestJWKSModeReadsTheDecisionFromClaims(t *testing.T) {
	clk := newClock()
	cp := newControlPlane(t, func(string) (map[string]any, http.Header) { return nil, nil })
	m, _ := cp.model(t, jumpdrive.VerifyJWKS, clk)

	d, err := m.Authenticate(bg, cp.key.mint(t, clk.Now(), nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	p := d.Principal
	if p.ID != "jd:acct:alice" || p.Restricted ||
		!m.CanWrite(d, "jd:ws:ws-a") || m.CanWrite(d, "jd:org:org-a") || m.CanApprove(d, "jd:ws:ws-a") {
		t.Errorf("decision = %+v", p)
	}
	if f := m.FilterFor(d); len(f.Spaces) != 2 || !f.AllowPublic {
		t.Errorf("filter = %+v", f)
	}
	if want := "jumpdrive;jd_org=org-a;jd_ws=ws-a;jti=jti-1;jd_client=claude-code-mcp"; d.Source != want {
		t.Errorf("source = %q, want %q", d.Source, want)
	}
	if n := cp.auth.hits.Load(); n != 0 {
		t.Errorf("jwks mode called /authorize %d times, want 0 (no callback on the hot path)", n)
	}
	// An opaque (non-JWT) bearer is simply unauthenticated.
	if _, err := m.Authenticate(bg, "legacy-shared-token"); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
		t.Errorf("opaque bearer: err=%v, want ErrUnauthenticated", err)
	}
}

func TestDualModeEnforcesTheIntersection(t *testing.T) {
	clk := newClock()
	// The control plane's live re-resolution has narrowed alice since minting:
	// she lost write, gained (but the token lacks) an approve right, and
	// authorize knows an extra read space.
	cp := newControlPlane(t, func(bearer string) (map[string]any, http.Header) {
		if bearer == "" {
			return nil, nil
		}
		return authorizeBody("jd:acct:alice",
			[]string{"jd:ws:ws-a", "jd:org:org-a", "jd:ws:extra"}, []string{}, []string{"jd:ws:ws-a"}, nil), nil
	})
	m, lb := cp.model(t, jumpdrive.VerifyDual, clk)

	d, err := m.Authenticate(bg, cp.key.mint(t, clk.Now(), nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got := d.Principal.Spaces; len(got) != 2 || access.ContainsSpace(got, "jd:ws:extra") {
		t.Errorf("read = %v, want the intersection [ws-a org-a]", got)
	}
	if m.CanWrite(d, "jd:ws:ws-a") {
		t.Error("write survived though authorize revoked it")
	}
	if m.CanApprove(d, "jd:ws:ws-a") {
		t.Error("approve granted though the credential did not carry it")
	}
	// The JWT's jti still reaches provenance.
	if !strings.Contains(d.Source, "jti=jti-1") {
		t.Errorf("source = %q, want the credential's jti", d.Source)
	}
	out := lb.String()
	for _, set := range []string{"set=read", "set=write", "set=approve"} {
		if !strings.Contains(out, "jumpdrive dual: mismatch") || !strings.Contains(out, set) {
			t.Errorf("no mismatch log for %s:\n%s", set, out)
		}
	}
	if strings.Contains(out, "eyJ") {
		t.Error("the mismatch log echoed the credential")
	}
}

func TestDualModeAgreementLogsNothing(t *testing.T) {
	clk := newClock()
	cp := newControlPlane(t, func(string) (map[string]any, http.Header) {
		return authorizeBody("jd:acct:alice", []string{"jd:org:org-a", "jd:ws:ws-a"}, []string{"jd:ws:ws-a"}, []string{}, nil), nil
	})
	m, lb := cp.model(t, jumpdrive.VerifyDual, clk)
	d, err := m.Authenticate(bg, cp.key.mint(t, clk.Now(), nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !m.CanWrite(d, "jd:ws:ws-a") || len(d.Principal.Spaces) != 2 {
		t.Errorf("decision = %+v", d.Principal)
	}
	if strings.Contains(lb.String(), "mismatch") {
		t.Errorf("agreeing paths logged a mismatch:\n%s", lb)
	}
}

func TestDualModeDeniesWhenEitherPathDenies(t *testing.T) {
	clk := newClock()
	agree := func(string) (map[string]any, http.Header) {
		return authorizeBody("jd:acct:alice", []string{"jd:ws:ws-a"}, []string{"jd:ws:ws-a"}, []string{}, nil), nil
	}

	t.Run("authorize rejects", func(t *testing.T) {
		cp := newControlPlane(t, func(string) (map[string]any, http.Header) { return nil, nil })
		m, lb := cp.model(t, jumpdrive.VerifyDual, clk)
		if _, err := m.Authenticate(bg, cp.key.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
			t.Errorf("err=%v, want ErrUnauthenticated", err)
		}
		if !strings.Contains(lb.String(), "authorize_rejected_jwks_accepted") {
			t.Errorf("mismatch not logged:\n%s", lb)
		}
	})
	t.Run("jwks rejects", func(t *testing.T) {
		cp := newControlPlane(t, agree)
		m, lb := cp.model(t, jumpdrive.VerifyDual, clk)
		forged := newTestKey(t, "kid-1").mint(t, clk.Now(), nil)
		if _, err := m.Authenticate(bg, forged); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
			t.Errorf("err=%v, want ErrUnauthenticated", err)
		}
		if !strings.Contains(lb.String(), "jwks_rejected_authorize_accepted") {
			t.Errorf("mismatch not logged:\n%s", lb)
		}
	})
	t.Run("principals differ", func(t *testing.T) {
		cp := newControlPlane(t, func(string) (map[string]any, http.Header) {
			return authorizeBody("jd:acct:mallory", []string{"jd:ws:ws-a"}, []string{"jd:ws:ws-a"}, []string{}, nil), nil
		})
		m, lb := cp.model(t, jumpdrive.VerifyDual, clk)
		if _, err := m.Authenticate(bg, cp.key.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
			t.Errorf("err=%v, want ErrUnauthenticated", err)
		}
		if !strings.Contains(lb.String(), "kind=principal") {
			t.Errorf("mismatch not logged:\n%s", lb)
		}
	})
	t.Run("authorize unavailable", func(t *testing.T) {
		cp := newControlPlane(t, agree)
		m, _ := cp.model(t, jumpdrive.VerifyDual, clk)
		cp.authDown.Store(true)
		if _, err := m.Authenticate(bg, cp.key.mint(t, clk.Now(), nil)); !errors.Is(err, jumpdrive.ErrAuthorizer) {
			t.Errorf("err=%v, want ErrAuthorizer (fail closed)", err)
		}
	})
}
