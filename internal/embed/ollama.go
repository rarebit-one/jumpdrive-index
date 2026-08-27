package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Ollama embeds via a local Ollama server's /api/embeddings endpoint — the
// natural provider for a self-hosted homelab (no API key, runs beside heyarr).
type Ollama struct {
	baseURL string
	model   string
	http    httpDoer
}

// httpDoer is the minimal HTTP surface, for testability.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// OllamaOption configures NewOllama.
type OllamaOption func(*Ollama)

// WithHTTPClient overrides the default HTTP client (used in tests).
func WithHTTPClient(d httpDoer) OllamaOption { return func(o *Ollama) { o.http = d } }

// NewOllama builds an Ollama embedder. baseURL is e.g. http://127.0.0.1:11434;
// model is the embedding model name (e.g. "nomic-embed-text").
func NewOllama(baseURL, model string, opts ...OllamaOption) (*Ollama, error) {
	if baseURL == "" || model == "" {
		return nil, fmt.Errorf("embed: ollama requires baseURL and model")
	}
	o := &Ollama{baseURL: baseURL, model: model, http: &http.Client{Timeout: 30 * time.Second}}
	for _, opt := range opts {
		opt(o)
	}
	return o, nil
}

// Model returns the descriptor stored on embeddings produced here.
func (o *Ollama) Model() string { return o.model }

// Embed embeds each text (Ollama's API is one prompt per call).
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := o.embedOne(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (o *Ollama) embedOne(ctx context.Context, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]string{"model": o.model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: ollama request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: ollama status %d", resp.StatusCode)
	}
	var out struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed: decode ollama response: %w", err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("embed: ollama returned an empty embedding")
	}
	vec := make([]float32, len(out.Embedding))
	for i, f := range out.Embedding {
		vec[i] = float32(f)
	}
	return vec, nil
}
