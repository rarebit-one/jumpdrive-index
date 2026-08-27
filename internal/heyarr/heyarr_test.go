package heyarr_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/heyarr"
	"github.com/rarebit-one/jumpdrive-index/internal/secret"
)

// captured records what the mock heyarr server saw, so a test can assert the
// outbound JSON-RPC request and headers are well-formed.
type captured struct {
	auth      string
	requestID string
	ctype     string
	method    string
	toolName  string
	args      map[string]any
}

// mockServer stands up an httptest server that decodes one tools/call request,
// records it, and replies with respond(id) — the raw JSON-RPC response body.
func mockServer(t *testing.T, cap *captured, respond func(id json.RawMessage) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.auth = r.Header.Get("Authorization")
		cap.requestID = r.Header.Get("X-Request-Id")
		cap.ctype = r.Header.Get("Content-Type")

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("server: decode request: %v", err)
		}
		if req.JSONRPC != "2.0" {
			t.Errorf("server: jsonrpc = %q, want 2.0", req.JSONRPC)
		}
		cap.method = req.Method
		cap.toolName = req.Params.Name
		cap.args = req.Params.Arguments

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respond(req.ID)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// result builds a tools/call success envelope carrying structuredContent.
func result(id json.RawMessage, structured string) string {
	return `{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[{"type":"text","text":` +
		jsonString(structured) + `}],"structuredContent":` + structured + `,"isError":false}}`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func newClient(t *testing.T, url, token string) *heyarr.Client {
	t.Helper()
	c, err := heyarr.New(heyarr.Options{
		BaseURL:      url,
		Token:        secret.Value(token),
		NewRequestID: func() string { return "req-123" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestSearchContentHappyPath(t *testing.T) {
	var cap captured
	srv := mockServer(t, &cap, func(id json.RawMessage) string {
		// The REAL heyarr search_content envelope: works/work_id/count/truncated,
		// no external_ids (heyarr selects only id/content_type/title/year).
		return result(id, `{"works":[{"work_id":"w1","content_type":"movie","title":"Alien","year":1979}],"count":1,"truncated":true}`)
	})
	c := newClient(t, srv.URL, "test-token")

	res, err := c.SearchContent(context.Background(), heyarr.SearchArgs{Query: "alien", Limit: 5})
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}

	// The outbound request is a well-formed tools/call with the right headers.
	if cap.method != "tools/call" || cap.toolName != "search_content" {
		t.Errorf("request = %s/%s, want tools/call/search_content", cap.method, cap.toolName)
	}
	if cap.auth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", cap.auth)
	}
	if cap.requestID != "req-123" {
		t.Errorf("X-Request-Id = %q, want req-123", cap.requestID)
	}
	if cap.ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", cap.ctype)
	}
	if cap.args["query"] != "alien" {
		t.Errorf("args.query = %v, want alien", cap.args["query"])
	}

	// The envelope parses: works + heyarr's count/truncated signal.
	if res.Count != 1 || !res.Truncated {
		t.Errorf("envelope = count %d truncated %v, want count 1 truncated true", res.Count, res.Truncated)
	}
	if len(res.Works) != 1 {
		t.Fatalf("got %d works, want 1", len(res.Works))
	}
	w := res.Works[0]
	if w.WorkID != "w1" || w.Title != "Alien" || w.ContentType != "movie" {
		t.Errorf("work = %+v, want work_id=w1 title=Alien content_type=movie", w)
	}
	if w.Year == nil || *w.Year != 1979 {
		t.Errorf("year = %v, want 1979", w.Year)
	}
	if got := w.ExternalID().Key(); got != "heyarr-work:w1" {
		t.Errorf("ExternalID key = %q, want heyarr-work:w1", got)
	}
}

func TestSearchContentTextFallback(t *testing.T) {
	// A server that sends ONLY a text content block (no structuredContent) still
	// decodes — the client falls back to the text payload.
	var cap captured
	srv := mockServer(t, &cap, func(id json.RawMessage) string {
		payload := `{"works":[{"work_id":"w9","title":"Dune"}],"count":1,"truncated":false}`
		return `{"jsonrpc":"2.0","id":` + string(id) + `,"result":{"content":[{"type":"text","text":` +
			jsonString(payload) + `}],"isError":false}}`
	})
	c := newClient(t, srv.URL, "")

	res, err := c.SearchContent(context.Background(), heyarr.SearchArgs{Query: "dune"})
	if err != nil {
		t.Fatalf("SearchContent: %v", err)
	}
	if len(res.Works) != 1 || res.Works[0].WorkID != "w9" {
		t.Fatalf("works = %+v, want one work work_id=w9", res.Works)
	}
	// No token configured → no Authorization header sent.
	if cap.auth != "" {
		t.Errorf("Authorization = %q, want empty (no token)", cap.auth)
	}
}

func TestSearchContentEmptyIsNoMatch(t *testing.T) {
	var cap captured
	srv := mockServer(t, &cap, func(id json.RawMessage) string {
		return result(id, `{"works":[],"count":0,"truncated":false}`)
	})
	c := newClient(t, srv.URL, "t")

	res, err := c.SearchContent(context.Background(), heyarr.SearchArgs{Query: "nothing"})
	if err != nil {
		t.Fatalf("empty result should not error: %v", err)
	}
	if len(res.Works) != 0 {
		t.Errorf("got %d works, want 0 (no match)", len(res.Works))
	}
}

func TestSearchContentToolError(t *testing.T) {
	var cap captured
	srv := mockServer(t, &cap, func(id json.RawMessage) string {
		return `{"jsonrpc":"2.0","id":` + string(id) +
			`,"result":{"content":[{"type":"text","text":"catalogue unavailable"}],"isError":true}}`
	})
	c := newClient(t, srv.URL, "t")

	_, err := c.SearchContent(context.Background(), heyarr.SearchArgs{Query: "x"})
	var he *heyarr.Error
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *heyarr.Error", err)
	}
	if he.Message != "catalogue unavailable" {
		t.Errorf("message = %q, want the tool error text", he.Message)
	}
}

func TestSearchContentRPCError(t *testing.T) {
	var cap captured
	srv := mockServer(t, &cap, func(id json.RawMessage) string {
		return `{"jsonrpc":"2.0","id":` + string(id) + `,"error":{"code":-32601,"message":"unknown tool"}}`
	})
	c := newClient(t, srv.URL, "t")

	_, err := c.SearchContent(context.Background(), heyarr.SearchArgs{Query: "x"})
	var he *heyarr.Error
	if !errors.As(err, &he) || he.Code != -32601 {
		t.Fatalf("err = %v, want *heyarr.Error code -32601", err)
	}
}

func TestSearchContentHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := newClient(t, srv.URL, "t")

	_, err := c.SearchContent(context.Background(), heyarr.SearchArgs{Query: "x"})
	var he *heyarr.Error
	if !errors.As(err, &he) || he.Code != http.StatusInternalServerError {
		t.Fatalf("err = %v, want *heyarr.Error code 500", err)
	}
}

func TestSearchContentUnreachable(t *testing.T) {
	// A server that is created then immediately closed — the address refuses the
	// connection. The client must return a typed error, never panic.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := newClient(t, url, "t")

	_, err := c.SearchContent(context.Background(), heyarr.SearchArgs{Query: "x"})
	if err == nil {
		t.Fatal("unreachable heyarr should error")
	}
	var he *heyarr.Error
	if !errors.As(err, &he) {
		t.Fatalf("err = %v, want *heyarr.Error", err)
	}
}

func TestSearchContentTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{"works":[],"count":0,"truncated":false},"isError":false}}`))
	}))
	t.Cleanup(srv.Close)

	c, err := heyarr.New(heyarr.Options{BaseURL: srv.URL, Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.SearchContent(context.Background(), heyarr.SearchArgs{Query: "x"}); err == nil {
		t.Fatal("a call slower than the client timeout should error")
	}
}

func TestSearchContentEmptyQuery(t *testing.T) {
	// heyarr requires a query OR a content_type; with neither, the guard
	// short-circuits with no HTTP call at all.
	c := newClient(t, "http://127.0.0.1:1", "t")
	if _, err := c.SearchContent(context.Background(), heyarr.SearchArgs{}); err == nil {
		t.Fatal("empty query and content_type should error before any request")
	}
}

func TestSearchContentByContentTypeOnly(t *testing.T) {
	// A content_type with no query is a valid heyarr search — the guard allows it.
	var cap captured
	srv := mockServer(t, &cap, func(id json.RawMessage) string {
		return result(id, `{"works":[{"work_id":"m1","content_type":"movie","title":"Alien"}],"count":1,"truncated":false}`)
	})
	c := newClient(t, srv.URL, "t")

	res, err := c.SearchContent(context.Background(), heyarr.SearchArgs{ContentType: "movie"})
	if err != nil {
		t.Fatalf("SearchContent by content_type: %v", err)
	}
	if len(res.Works) != 1 || res.Works[0].ContentType != "movie" {
		t.Fatalf("works = %+v, want one movie", res.Works)
	}
	if cap.args["content_type"] != "movie" {
		t.Errorf("args.content_type = %v, want movie", cap.args["content_type"])
	}
}

func TestNewRejectsEmptyBaseURL(t *testing.T) {
	if _, err := heyarr.New(heyarr.Options{BaseURL: "  "}); err == nil {
		t.Error("empty BaseURL should be rejected")
	}
}

func TestTokenRedaction(t *testing.T) {
	// A secret must never render its bytes in a formatted string.
	s := secret.Value("super-secret-token")
	if got := s.String(); got == "super-secret-token" {
		t.Error("secret leaked through String()")
	}
	if s.Reveal() != "super-secret-token" {
		t.Error("Reveal must return the real value")
	}
}
