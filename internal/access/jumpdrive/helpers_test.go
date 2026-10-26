package jumpdrive_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const testIssuer = "https://console.example.test"

var b64 = base64.RawURLEncoding

// clock is a settable time source shared by the verifier and the model.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Unix(1792980000, 0)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type testKey struct {
	kid  string
	priv *ecdsa.PrivateKey
}

func newTestKey(t *testing.T, kid string) testKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return testKey{kid: kid, priv: priv}
}

// jwk is the public half in the control plane's JWKS shape (jwks.v1.json).
func (k testKey) jwk(t *testing.T) map[string]any {
	t.Helper()
	pub, err := k.priv.PublicKey.Bytes() // 0x04 || X || Y
	if err != nil {
		t.Fatalf("PublicKey.Bytes: %v", err)
	}
	return map[string]any{
		"kty": "EC", "crv": "P-256", "use": "sig", "alg": "ES256", "kid": k.kid,
		"x": b64.EncodeToString(pub[1:33]), "y": b64.EncodeToString(pub[33:]),
	}
}

func segment(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b64.EncodeToString(b)
}

// sign produces a compact ES256 JWS over header and claims.
func (k testKey) sign(t *testing.T, header, claims map[string]any) string {
	t.Helper()
	in := segment(t, header) + "." + segment(t, claims)
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

func (k testKey) header() map[string]any {
	return map[string]any{"alg": "ES256", "kid": k.kid, "typ": "jd-index+jwt"}
}

// mint signs a valid credential issued at now, after applying edits to the
// claims (a nil value deletes the claim).
func (k testKey) mint(t *testing.T, now time.Time, edits map[string]any) string {
	t.Helper()
	return k.sign(t, k.header(), claimsAt(now, edits))
}

func claimsAt(now time.Time, edits map[string]any) map[string]any {
	c := map[string]any{
		"iss": testIssuer, "aud": "jumpdrive-index",
		"sub":       "jd:acct:alice",
		"jd_org":    "org-a",
		"jd_ws":     "ws-a",
		"jd_access": "full",
		"jd_spaces": map[string]any{
			"read":    []string{"jd:ws:ws-a", "jd:org:org-a"},
			"write":   []string{"jd:ws:ws-a"},
			"approve": []string{},
		},
		"jd_client": "claude-code-mcp",
		"jd_ver":    1,
		"jti":       "jti-1",
		"iat":       now.Unix(), "nbf": now.Unix(), "exp": now.Unix() + 60,
	}
	for k, v := range edits {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

// jwksServer serves a mutable JWKS and counts fetches.
type jwksServer struct {
	*httptest.Server
	mu   sync.Mutex
	keys []map[string]any
	down bool
	hits int
}

func newJWKSServer(t *testing.T, keys ...map[string]any) *jwksServer {
	t.Helper()
	js := &jwksServer{keys: keys}
	js.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		js.mu.Lock()
		defer js.mu.Unlock()
		js.hits++
		if js.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": js.keys})
	}))
	t.Cleanup(js.Close)
	return js
}

func (js *jwksServer) set(keys ...map[string]any) {
	js.mu.Lock()
	js.keys = keys
	js.mu.Unlock()
}

func (js *jwksServer) setDown(down bool) {
	js.mu.Lock()
	js.down = down
	js.mu.Unlock()
}

func (js *jwksServer) fetches() int {
	js.mu.Lock()
	defer js.mu.Unlock()
	return js.hits
}

// logBuffer captures slog output for assertions.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newLogger() (*slog.Logger, *logBuffer) {
	lb := &logBuffer{}
	return slog.New(slog.NewTextHandler(lb, &slog.HandlerOptions{Level: slog.LevelDebug})), lb
}
