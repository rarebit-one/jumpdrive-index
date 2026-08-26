package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	h := httpapi.New(mcp.New(service.New(st, am)), nil)
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
