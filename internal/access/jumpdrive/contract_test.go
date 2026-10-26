package jumpdrive_test

// The control-plane ↔ index drift gate for the caller-identity contract (v1).
//
// contract/ holds the control plane's fixtures, vendored byte-for-byte (see
// contract/SOURCE.md). These tests fail if the verifier or the /authorize client
// and those fixtures disagree, in either direction: a key the control plane
// sends that this package does not decode, a key this package needs that the
// control plane does not send, a pinned constant (alg, typ, aud, jd_ver,
// contract) that moved, or a fixture this package cannot verify, decode or act
// on. A red here means the two sides diverged: re-vendor (SOURCE.md) and fix the
// code or the fixtures deliberately, never by editing a fixture to follow the
// code.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/access/jumpdrive"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

func readContract(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("contract", name))
	if err != nil {
		t.Fatalf("read vendored fixture: %v", err)
	}
	return b
}

func decodeContract(t *testing.T, name string, v any) {
	t.Helper()
	if err := json.Unmarshal(readContract(t, name), v); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
}

// jsonKeys is the set of JSON object keys a struct type decodes.
func jsonKeys(typ reflect.Type) []string {
	var out []string
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// sameKeys fails unless the fixture object and the Go type agree on the key set.
func sameKeys(t *testing.T, what string, fixture map[string]json.RawMessage, typ reflect.Type) {
	t.Helper()
	if got, want := keysOf(fixture), jsonKeys(typ); !slices.Equal(got, want) {
		t.Errorf("%s: fixture keys %v, but %s decodes %v", what, got, typ, want)
	}
}

func compact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("compact: %v", err)
	}
	return b.Bytes()
}

// signRaw signs the given header and claims bytes as a compact ES256 JWS, so the
// control plane's own JSON is what the verifier parses.
func signRaw(t *testing.T, k testKey, header, claims []byte) string {
	t.Helper()
	in := b64.EncodeToString(header) + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(in))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return in + "." + b64.EncodeToString(sig)
}

// credentialFixture is credential.v1.json.
type credentialFixture struct {
	Header map[string]json.RawMessage `json:"header"`
	Claims map[string]json.RawMessage `json:"claims"`
	raw    struct {
		Header json.RawMessage `json:"header"`
		Claims json.RawMessage `json:"claims"`
	}
	claims struct {
		Iss    string `json:"iss"`
		Aud    string `json:"aud"`
		Sub    string `json:"sub"`
		Org    string `json:"jd_org"`
		Ws     string `json:"jd_ws"`
		Access string `json:"jd_access"`
		Spaces struct {
			Read    []string `json:"read"`
			Write   []string `json:"write"`
			Approve []string `json:"approve"`
		} `json:"jd_spaces"`
		Client string `json:"jd_client"`
		Ver    int    `json:"jd_ver"`
		JTI    string `json:"jti"`
		Iat    int64  `json:"iat"`
		Nbf    int64  `json:"nbf"`
		Exp    int64  `json:"exp"`
	}
	header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
		Typ string `json:"typ"`
	}
}

func loadCredential(t *testing.T) *credentialFixture {
	t.Helper()
	f := &credentialFixture{}
	decodeContract(t, "credential.v1.json", f)
	decodeContract(t, "credential.v1.json", &f.raw)
	if err := json.Unmarshal(f.raw.Claims, &f.claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if err := json.Unmarshal(f.raw.Header, &f.header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	return f
}

// authorizeResponseFixture is authorize-response.v1.json.
type authorizeResponseFixture struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
	Body    json.RawMessage   `json:"body"`
	body    struct {
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
}

func loadAuthorizeResponse(t *testing.T) *authorizeResponseFixture {
	t.Helper()
	f := &authorizeResponseFixture{}
	decodeContract(t, "authorize-response.v1.json", f)
	if err := json.Unmarshal(f.Body, &f.body); err != nil {
		t.Fatalf("decode authorize body: %v", err)
	}
	return f
}

// fixtureServer serves a fixed status, headers and body at /internal/index/authorize
// and records each request.
type fixtureServer struct {
	*httptest.Server
	mu    sync.Mutex
	reqs  []*http.Request
	body  []byte
	jwks  []byte
	hits  int
	reply func() (int, http.Header, []byte)
}

func newFixtureServer(t *testing.T, reply func() (int, http.Header, []byte)) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{reply: reply}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/index/jwks.json" {
			fs.mu.Lock()
			jwks := fs.jwks
			fs.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(jwks)
			return
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		fs.mu.Lock()
		fs.reqs = append(fs.reqs, r)
		fs.body = buf.Bytes()
		fs.hits++
		fs.mu.Unlock()
		status, hdr, body := fs.reply()
		for k, v := range hdr {
			w.Header()[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fixtureServer) base() string { return fs.URL + "/internal/index" }

func (fs *fixtureServer) authorizeHits() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.hits
}

func (fs *fixtureServer) setJWKS(b []byte) {
	fs.mu.Lock()
	fs.jwks = b
	fs.mu.Unlock()
}

func spaceStrings(in []domain.SpaceID) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = string(s)
	}
	return out
}

// TestContractCredential pins the credential: the header and claim keys the
// control plane mints are exactly the ones the verifier reads, the pinned
// constants match, and the fixture's own JSON, once signed, verifies to the
// decision it describes.
func TestContractCredential(t *testing.T) {
	f := loadCredential(t)

	// The header carries alg, kid and typ; the verifier reads those (and crit,
	// which it refuses), never a key-bearing field.
	wantHdr := jsonKeys(reflect.TypeFor[jumpdrive.JWSHeaderWire]())
	for _, k := range keysOf(f.Header) {
		if !slices.Contains(wantHdr, k) {
			t.Errorf("credential header key %q is not one the verifier reads (%v)", k, wantHdr)
		}
	}
	if f.header.Alg != jumpdrive.CredentialAlg || f.header.Typ != jumpdrive.CredentialType || f.header.Kid == "" {
		t.Errorf("credential header = %+v, want alg=%s typ=%s and a kid", f.header, jumpdrive.CredentialAlg, jumpdrive.CredentialType)
	}

	sameKeys(t, "credential claims", f.Claims, reflect.TypeFor[jumpdrive.Claims]())
	spacesField, _ := reflect.TypeFor[jumpdrive.Claims]().FieldByName("Spaces")
	var spaces map[string]json.RawMessage
	if err := json.Unmarshal(f.Claims["jd_spaces"], &spaces); err != nil {
		t.Fatalf("jd_spaces: %v", err)
	}
	sameKeys(t, "credential jd_spaces", spaces, spacesField.Type)

	c := f.claims
	if c.Aud != jumpdrive.CredentialAudience {
		t.Errorf("aud = %q, want %q", c.Aud, jumpdrive.CredentialAudience)
	}
	if c.Ver != jumpdrive.CredentialVersion {
		t.Errorf("jd_ver = %d, want %d", c.Ver, jumpdrive.CredentialVersion)
	}
	if c.Exp-c.Iat != 60 || c.Nbf != c.Iat {
		t.Errorf("iat/nbf/exp = %d/%d/%d, want exp = iat + 60 and nbf = iat", c.Iat, c.Nbf, c.Exp)
	}

	// Sign the control plane's own header and claims bytes and verify them.
	k := newTestKey(t, f.header.Kid)
	js := newJWKSServer(t, k.jwk(t))
	clk := &clock{t: time.Unix(c.Iat, 0)}
	log, _ := newLogger()
	v, err := jumpdrive.NewVerifier(jumpdrive.VerifierConfig{JWKSURL: js.URL, Issuer: c.Iss, Now: clk.Now, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	m, err := jumpdrive.New(jumpdrive.Config{
		BaseURL: js.URL, Mode: jumpdrive.VerifyJWKS, Issuer: c.Iss, Verifier: v, Logger: log, Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := signRaw(t, k, compact(t, f.raw.Header), compact(t, f.raw.Claims))
	d, err := m.Authenticate(bg, token)
	if err != nil {
		t.Fatalf("the vendored credential does not verify: %v", err)
	}
	p := d.Principal
	if string(p.ID) != c.Sub || p.Restricted ||
		!slices.Equal(spaceStrings(p.Spaces), c.Spaces.Read) ||
		!slices.Equal(spaceStrings(p.WriteSpaces), c.Spaces.Write) ||
		!slices.Equal(spaceStrings(p.ApproverSpaces), c.Spaces.Approve) {
		t.Errorf("decision %+v does not match the credential's sub/jd_spaces", p)
	}
	// Provenance: jd_org, jd_ws and jti (plus jd_client) reach every write.
	if want := jumpdrive.ProvenanceSource(c.Org, c.Ws, c.JTI, c.Client); d.Source != want {
		t.Errorf("source = %q, want %q", d.Source, want)
	}
	for _, part := range []string{"jd_org=" + c.Org, "jd_ws=" + c.Ws, "jti=" + c.JTI} {
		if !strings.Contains(d.Source, part) {
			t.Errorf("source %q is missing %s", d.Source, part)
		}
	}

	// The 60s lifetime is enforced with the pinned skew, no more.
	clk.Advance(60*time.Second + jumpdrive.MaxClockSkew + time.Second)
	if _, err := m.Authenticate(bg, token); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
		t.Errorf("credential past exp+skew: err=%v, want ErrUnauthenticated", err)
	}
}

// TestContractJWKS pins the JWKS: its key fields are the ones the verifier
// parses, the kid is the key's RFC 7638 thumbprint and matches the credential's,
// and the verifier accepts the vendored document as served.
func TestContractJWKS(t *testing.T) {
	raw := readContract(t, "jwks.v1.json")
	var set struct {
		Keys []map[string]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) == 0 {
		t.Fatal("jwks.v1.json has no keys")
	}
	var typed struct {
		Keys []struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   string `json:"x"`
			Y   string `json:"y"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	for i, k := range set.Keys {
		sameKeys(t, "jwks key", k, reflect.TypeFor[jumpdrive.JWKWire]())
		tk := typed.Keys[i]
		if tk.Kty != "EC" || tk.Crv != "P-256" || tk.Use != "sig" || tk.Alg != jumpdrive.CredentialAlg {
			t.Errorf("jwks key %+v is not an EC P-256 ES256 signing key", tk)
		}
		thumb := sha256.Sum256([]byte(`{"crv":"` + tk.Crv + `","kty":"` + tk.Kty + `","x":"` + tk.X + `","y":"` + tk.Y + `"}`))
		if got := b64.EncodeToString(thumb[:]); got != tk.Kid {
			t.Errorf("kid %q is not the RFC 7638 thumbprint %q", tk.Kid, got)
		}
	}
	cred := loadCredential(t)
	var kids []string
	for _, k := range typed.Keys {
		kids = append(kids, k.Kid)
	}
	if !slices.Contains(kids, cred.header.Kid) {
		t.Errorf("credential kid %q is not in jwks.v1.json (%v)", cred.header.Kid, kids)
	}

	// Serve the vendored bytes. A token under the fixture kid gets as far as the
	// signature check: the key parsed, is on the curve, and was not skipped.
	fs := newFixtureServer(t, func() (int, http.Header, []byte) { return http.StatusNotFound, nil, nil })
	fs.setJWKS(raw)
	log, lb := newLogger()
	v, err := jumpdrive.NewVerifier(jumpdrive.VerifierConfig{
		JWKSURL: fs.base() + "/jwks.json", Issuer: testIssuer, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	other := newTestKey(t, cred.header.Kid)
	_, err = v.Verify(bg, other.mint(t, time.Now(), nil))
	if err == nil || !strings.Contains(err.Error(), "bad signature") {
		t.Errorf("token under the fixture kid: err=%v, want a bad-signature failure (the key must load)", err)
	}
	if strings.Contains(lb.String(), "skipping unusable JWKS key") {
		t.Errorf("the verifier skipped a vendored key:\n%s", lb)
	}
}

// TestContractAuthorizeRequest pins what the index sends to /authorize.
func TestContractAuthorizeRequest(t *testing.T) {
	var f struct {
		Headers map[string]string          `json:"headers"`
		Method  string                     `json:"method"`
		Path    string                     `json:"path"`
		Body    map[string]json.RawMessage `json:"body"`
	}
	decodeContract(t, "authorize-request.v1.json", &f)
	sameKeys(t, "authorize request body", f.Body, reflect.TypeFor[jumpdrive.AuthorizeRequestWire]())

	resp := loadAuthorizeResponse(t)
	fs := newFixtureServer(t, func() (int, http.Header, []byte) { return resp.Status, nil, resp.Body })
	m, _ := newAuthorizeModel(t, fs.base(), nil)
	ctx := access.WithRequestID(bg, "req-contract")
	if _, err := m.Authenticate(ctx, "the-credential"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	fs.mu.Lock()
	req, body := fs.reqs[0], fs.body
	fs.mu.Unlock()
	if req.Method != f.Method || req.URL.Path != f.Path {
		t.Errorf("request = %s %s, want %s %s", req.Method, req.URL.Path, f.Method, f.Path)
	}
	for name := range f.Headers {
		if req.Header.Get(name) == "" {
			t.Errorf("request is missing the %s header", name)
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bearer svc-secret" {
		t.Errorf("Authorization = %q, want the service secret as a bearer", got)
	}
	if got, want := req.Header.Get("Content-Type"), f.Headers["Content-Type"]; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got := req.Header.Get("X-Request-Id"); got != "req-contract" {
		t.Errorf("X-Request-Id = %q, want the propagated id", got)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(body, &sent); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if got, want := keysOf(sent), keysOf(f.Body); !slices.Equal(got, want) {
		t.Errorf("request body keys = %v, want %v", got, want)
	}
	var bearer string
	_ = json.Unmarshal(sent["bearer"], &bearer)
	if bearer != "the-credential" {
		t.Errorf("bearer = %q, want the credential", bearer)
	}
}

// TestContractAuthorizeResponse pins the 200 decision: its keys are exactly the
// ones the client decodes, and the vendored body (served verbatim, with its
// Cache-Control) maps to the decision it describes and is cached no longer than
// expires_at.
func TestContractAuthorizeResponse(t *testing.T) {
	f := loadAuthorizeResponse(t)
	var body map[string]json.RawMessage
	if err := json.Unmarshal(f.Body, &body); err != nil {
		t.Fatal(err)
	}
	sameKeys(t, "authorize response body", body, reflect.TypeFor[jumpdrive.AuthorizeResponseWire]())
	b := f.body
	if f.Status != http.StatusOK || b.Contract != jumpdrive.AuthorizeContract {
		t.Errorf("status/contract = %d/%q, want 200/%q", f.Status, b.Contract, jumpdrive.AuthorizeContract)
	}
	exp, err := time.Parse(time.RFC3339, b.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at: %v", err)
	}

	hdr := http.Header{}
	for k, v := range f.Headers {
		hdr.Set(k, v)
	}
	fs := newFixtureServer(t, func() (int, http.Header, []byte) { return f.Status, hdr, f.Body })
	clk := &clock{t: exp.Add(-45 * time.Second)}
	m, _ := newAuthorizeModel(t, fs.base(), func(c *jumpdrive.Config) { c.Now = clk.Now })

	d, err := m.Authenticate(bg, "the-credential")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	p := d.Principal
	if string(p.ID) != b.PrincipalID || p.Restricted != b.Restricted ||
		!slices.Equal(spaceStrings(p.Spaces), b.Spaces) ||
		!slices.Equal(spaceStrings(p.WriteSpaces), b.WriteSpaces) ||
		!slices.Equal(spaceStrings(p.ApproverSpaces), b.ApproverSpaces) {
		t.Errorf("decision %+v does not match the vendored response", p)
	}
	if want := jumpdrive.ProvenanceSource(b.OrgID, b.WorkspaceID, "", ""); d.Source != want {
		t.Errorf("source = %q, want %q", d.Source, want)
	}

	// Cache-Control max-age=30 is honoured...
	clk.Advance(20 * time.Second)
	if _, err := m.Authenticate(bg, "the-credential"); err != nil {
		t.Fatal(err)
	}
	if n := fs.authorizeHits(); n != 1 {
		t.Errorf("authorize hits within max-age = %d, want 1 (cached)", n)
	}
	// ...but never past expires_at, even inside a fresh max-age window.
	clk.Advance(10 * time.Second) // exp - 15s: a re-fetch, cached only until exp
	if _, err := m.Authenticate(bg, "the-credential"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(16 * time.Second) // exp + 1s
	if _, err := m.Authenticate(bg, "the-credential"); err != nil {
		t.Fatal(err)
	}
	if n := fs.authorizeHits(); n != 3 {
		t.Errorf("authorize hits = %d, want 3 (a cached decision must not outlive expires_at)", n)
	}
}

// TestContractAuthorizeErrors pins every error body: each is a rejection
// (ErrUnauthenticated), and only the service-secret code is an operator error.
func TestContractAuthorizeErrors(t *testing.T) {
	var errs map[string]struct {
		Status int                        `json:"status"`
		Body   map[string]json.RawMessage `json:"body"`
	}
	decodeContract(t, "authorize-errors.v1.json", &errs)
	if _, ok := errs[jumpdrive.AuthorizeErrServiceSecret]; !ok {
		t.Errorf("authorize-errors.v1.json has no %q; the client keys its operator alert on it", jumpdrive.AuthorizeErrServiceSecret)
	}
	for code, e := range errs {
		t.Run(code, func(t *testing.T) {
			sameKeys(t, "error body", e.Body, reflect.TypeFor[jumpdrive.AuthorizeErrorWire]())
			var got string
			_ = json.Unmarshal(e.Body["error"], &got)
			if got != code {
				t.Errorf("body error = %q, want %q", got, code)
			}
			if e.Status != http.StatusUnauthorized && e.Status != http.StatusForbidden {
				t.Fatalf("status %d: the client maps only 401/403 to a rejection", e.Status)
			}
			body, _ := json.Marshal(e.Body)
			fs := newFixtureServer(t, func() (int, http.Header, []byte) { return e.Status, nil, body })
			m, lb := newAuthorizeModel(t, fs.base(), nil)
			if _, err := m.Authenticate(bg, "the-credential"); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
				t.Errorf("err = %v, want ErrUnauthenticated", err)
			}
			if operator := strings.Contains(lb.String(), "level=ERROR"); operator != (code == jumpdrive.AuthorizeErrServiceSecret) {
				t.Errorf("error-level log = %v for %s:\n%s", operator, code, lb)
			}
		})
	}
}

// TestContractDualAgrees runs dual mode over the two vendored decisions: the
// credential and the authorize response describe the same principal and sets,
// so dual verification accepts them with no mismatch logged.
func TestContractDualAgrees(t *testing.T) {
	cred := loadCredential(t)
	resp := loadAuthorizeResponse(t)
	c, b := cred.claims, resp.body
	if b.PrincipalID != c.Sub || b.OrgID != c.Org || b.WorkspaceID != c.Ws || b.Access != c.Access ||
		!slices.Equal(b.Spaces, c.Spaces.Read) || !slices.Equal(b.WriteSpaces, c.Spaces.Write) ||
		!slices.Equal(b.ApproverSpaces, c.Spaces.Approve) {
		t.Errorf("credential.v1.json and authorize-response.v1.json describe different decisions")
	}
	// (expires_at and exp are independent example timestamps in the two
	// fixtures, so they are not compared; the client's expires_at handling is
	// pinned in TestContractAuthorizeResponse.)

	k := newTestKey(t, cred.header.Kid)
	jwks, _ := json.Marshal(map[string]any{"keys": []any{k.jwk(t)}})
	hdr := http.Header{}
	for name, v := range resp.Headers {
		hdr.Set(name, v)
	}
	fs := newFixtureServer(t, func() (int, http.Header, []byte) { return resp.Status, hdr, resp.Body })
	fs.setJWKS(jwks)
	clk := &clock{t: time.Unix(c.Iat, 0)}
	log, lb := newLogger()
	m, err := jumpdrive.New(jumpdrive.Config{
		BaseURL: fs.base(), SharedSecret: "svc-secret", Mode: jumpdrive.VerifyDual,
		Issuer: c.Iss, Logger: log, Now: clk.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	token := signRaw(t, k, compact(t, cred.raw.Header), compact(t, cred.raw.Claims))
	d, err := m.Authenticate(bg, token)
	if err != nil {
		t.Fatalf("dual over the vendored fixtures: %v", err)
	}
	if !m.CanWrite(d, domain.SpaceID(c.Spaces.Write[0])) {
		t.Errorf("dual decision lost the write set: %+v", d.Principal)
	}
	if strings.Contains(lb.String(), "mismatch") {
		t.Errorf("dual logged a mismatch over agreeing fixtures:\n%s", lb)
	}
}
