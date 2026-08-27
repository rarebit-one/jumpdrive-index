package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/access/starchart"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/httpapi"
	"github.com/rarebit-one/jumpdrive-index/internal/mcp"
	"github.com/rarebit-one/jumpdrive-index/internal/service"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := sqlite.Open(sqlite.Options{
		Path:       filepath.Join(t.TempDir(), "http.db"),
		Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	am, _ := starchart.New(starchart.Config{Principals: []starchart.PrincipalConfig{
		{Token: "kate-tok", ID: "kate", Spaces: []domain.SpaceID{"fam"}},
	}})
	h := httpapi.New(mcp.New(service.New(st, am, nil)), nil)
	ts := httptest.NewServer(h.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func post(t *testing.T, url, bearer, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return resp.StatusCode, m
}

func TestHealthEndpoints(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/health/alive", "/health/ready"} {
		resp, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatalf("get %s: %v", p, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d, want 200", p, resp.StatusCode)
		}
	}
}

func TestMCPOverHTTP(t *testing.T) {
	ts := newTestServer(t)

	// create_entity as kate (bearer carried in the Authorization header).
	code, resp := post(t, ts.URL+"/mcp", "kate-tok",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_entity","arguments":{"type":"Movie","props":{"name":"Alien"},"space":"fam","visibility":"space"}}}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	result := resp["result"].(map[string]any)
	if result["isError"].(bool) {
		t.Fatalf("create returned isError: %v", result)
	}

	// A write with no bearer is an isError tool result (authenticated deny).
	_, resp = post(t, ts.URL+"/mcp", "",
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"create_entity","arguments":{"type":"Movie","props":{"name":"X"},"space":"fam","visibility":"space"}}}`)
	if !resp["result"].(map[string]any)["isError"].(bool) {
		t.Error("unauthenticated write should be an isError tool result")
	}
}

// mcpRequest drives one POST /mcp and returns the raw response so headers (the
// Mcp-Session-Id) can be asserted alongside the JSON-RPC body.
func mcpRequest(t *testing.T, url, bearer, sessionID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	return resp
}

// TestInitializeStampsSessionID: the Streamable HTTP initialize response carries a
// generated Mcp-Session-Id header alongside the JSON-RPC result.
func TestInitializeStampsSessionID(t *testing.T) {
	ts := newTestServer(t)
	resp := mcpRequest(t, ts.URL+"/mcp", "kate-tok", "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("initialize did not stamp an Mcp-Session-Id header")
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	result, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in initialize response: %v", m)
	}
	if result["protocolVersion"] != mcp.ProtocolVersion {
		t.Errorf("protocolVersion = %v, want %s", result["protocolVersion"], mcp.ProtocolVersion)
	}
}

// TestPostWithoutSessionIDStillWorks: back-compat — the plain-POST path (the M0
// acceptance flow) carries no session id and must keep succeeding; no session
// header is invented for a non-initialize request.
func TestPostWithoutSessionIDStillWorks(t *testing.T) {
	ts := newTestServer(t)
	resp := mcpRequest(t, ts.URL+"/mcp", "kate-tok", "",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_entity","arguments":{"type":"Movie","props":{"name":"Alien"},"space":"fam","visibility":"space"}}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Mcp-Session-Id"); got != "" {
		t.Errorf("unexpected session header on a session-less POST: %q", got)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["result"].(map[string]any)["isError"].(bool) {
		t.Fatalf("create returned isError: %v", m)
	}
}

// TestPostWithSessionIDEchoes: a POST carrying an Mcp-Session-Id succeeds and the
// server echoes the id back.
func TestPostWithSessionIDEchoes(t *testing.T) {
	ts := newTestServer(t)
	const sid = "11111111-2222-3333-4444-555555555555"
	resp := mcpRequest(t, ts.URL+"/mcp", "kate-tok", sid,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_entity","arguments":{"type":"Movie","props":{"name":"Alien"},"space":"fam","visibility":"space"}}}`)
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Mcp-Session-Id"); got != sid {
		t.Errorf("echoed session id = %q, want %q", got, sid)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["result"].(map[string]any)["isError"].(bool) {
		t.Fatalf("create returned isError: %v", m)
	}
}

// TestGetOpensSSEStream: GET /mcp returns 200 text/event-stream, sends an opening
// comment, and stays open until the request context is cancelled.
func TestGetOpensSSEStream(t *testing.T) {
	ts := newTestServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer kate-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}

	// The opening SSE comment arrives; the stream then blocks.
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("read opening line: %v", err)
	}
	if !strings.HasPrefix(line, ":") {
		t.Errorf("opening line = %q, want an SSE comment", line)
	}

	// Cancelling the context ends the read cleanly (the stream was still open).
	cancel()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not close after context cancel")
	}
}

// TestAuthEnforcedOnGET: the SSE stream requires a bearer.
func TestAuthEnforcedOnGET(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/mcp")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated GET = %d, want 401", resp.StatusCode)
	}
}
