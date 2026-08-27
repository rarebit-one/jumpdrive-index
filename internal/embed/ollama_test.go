package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/embed"
)

func TestOllamaEmbed(t *testing.T) {
	var gotModel, gotPrompt string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embeddings" {
			t.Errorf("path = %q, want /api/embeddings", r.URL.Path)
		}
		var body struct{ Model, Prompt string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, gotPrompt = body.Model, body.Prompt
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{0.1, 0.2, 0.3}})
	}))
	defer ts.Close()

	o, err := embed.NewOllama(ts.URL, "nomic-embed-text")
	if err != nil {
		t.Fatalf("NewOllama: %v", err)
	}
	vecs, err := o.Embed(context.Background(), []string{"a chestburster scene"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vecs) != 1 || len(vecs[0]) != 3 {
		t.Fatalf("vecs = %v, want one 3-d vector", vecs)
	}
	if gotModel != "nomic-embed-text" || gotPrompt != "a chestburster scene" {
		t.Errorf("request sent model=%q prompt=%q", gotModel, gotPrompt)
	}
	if o.Model() != "nomic-embed-text" {
		t.Errorf("Model() = %q", o.Model())
	}
}

func TestNewOllamaValidates(t *testing.T) {
	if _, err := embed.NewOllama("", "m"); err == nil {
		t.Error("empty baseURL should error")
	}
	if _, err := embed.NewOllama("http://x", ""); err == nil {
		t.Error("empty model should error")
	}
}
