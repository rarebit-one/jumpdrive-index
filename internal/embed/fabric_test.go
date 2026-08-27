package embed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/secret"
)

var ctx = context.Background()

// fabricEmbedServer is an httptest OpenAI-compatible /v1/embeddings endpoint. It
// records the auth header and the number of requests, and returns 2-D vectors
// DELIBERATELY out of index order to prove the client restores input order.
func fabricEmbedServer(t *testing.T, gotAuth *string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// One datum per input, each a distinct 2-D vector, emitted in REVERSE
		// index order so a naive client would scramble the mapping.
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		data := make([]datum, len(req.Input))
		for i := range req.Input {
			// vector = [index, 1] so each is identifiable by its slot.
			data[len(req.Input)-1-i] = datum{Index: i, Embedding: []float64{float64(i), 1}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "model": req.Model})
	}))
}

func TestFabric_BatchesAndRestoresOrder(t *testing.T) {
	var calls int
	srv := fabricEmbedServer(t, nil, &calls)
	defer srv.Close()

	f, err := NewFabric(srv.URL, "bge-m3", "")
	if err != nil {
		t.Fatalf("NewFabric: %v", err)
	}
	vecs, err := f.Embed(ctx, []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if calls != 1 {
		t.Errorf("batched embed made %d requests, want 1", calls)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	// Each slot i must carry vector [i, 1] despite the server's reverse order.
	for i := range vecs {
		if len(vecs[i]) != 2 || vecs[i][0] != float32(i) || vecs[i][1] != 1 {
			t.Errorf("vec[%d] = %v, want [%d 1] (order was not restored)", i, vecs[i], i)
		}
	}
}

func TestFabric_ModelIsNameAtDim(t *testing.T) {
	var calls int
	srv := fabricEmbedServer(t, nil, &calls)
	defer srv.Close()
	f, _ := NewFabric(srv.URL, "bge-m3", "")

	// Before any Embed, the dimension is unknown → bare name.
	if got := f.Model(); got != "bge-m3" {
		t.Errorf("Model() before Embed = %q, want bare %q", got, "bge-m3")
	}
	if _, err := f.Embed(ctx, []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// After Embed the descriptor carries the dimension — the re-embed invariant
	// keys on name@dim, so a model swap of the same dim is still detectable.
	if got := f.Model(); got != "bge-m3@2" {
		t.Errorf("Model() after Embed = %q, want %q", got, "bge-m3@2")
	}
}

func TestFabric_SendsBearerWhenSet(t *testing.T) {
	var auth string
	var calls int
	srv := fabricEmbedServer(t, &auth, &calls)
	defer srv.Close()

	f, _ := NewFabric(srv.URL, "bge-m3", secret.Value("tok-123"))
	if _, err := f.Embed(ctx, []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if auth != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer tok-123")
	}

	// With no token, no Authorization header is sent.
	auth = ""
	f2, _ := NewFabric(srv.URL, "bge-m3", "")
	if _, err := f2.Embed(ctx, []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if auth != "" {
		t.Errorf("Authorization sent without a token: %q", auth)
	}
}

func TestFabric_ErrorsOnBadStatusAndCountMismatch(t *testing.T) {
	// Non-200 is an error.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()
	f, _ := NewFabric(bad.URL, "bge-m3", "")
	if _, err := f.Embed(ctx, []string{"x"}); err == nil {
		t.Error("expected an error on a 500 response")
	}

	// A response with the wrong number of vectors is an error (no silent drop).
	short := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"index": 0, "embedding": []float64{1, 2}}},
			"model": "bge-m3",
		})
	}))
	defer short.Close()
	f2, _ := NewFabric(short.URL, "bge-m3", "")
	if _, err := f2.Embed(ctx, []string{"a", "b"}); err == nil {
		t.Error("expected an error when the server returns fewer vectors than inputs")
	}
}

func TestFabric_RequiresURLAndModel(t *testing.T) {
	if _, err := NewFabric("", "m", ""); err == nil {
		t.Error("expected an error with an empty baseURL")
	}
	if _, err := NewFabric("http://x", "", ""); err == nil {
		t.Error("expected an error with an empty model")
	}
}
