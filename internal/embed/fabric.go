package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/secret"
)

// Fabric embeds via the TechnoCore switchboard (Farcaster) — an
// OpenAI-compatible POST /v1/embeddings endpoint that routes the infer.embed
// capability to whichever accelerator (or cloud fallback) can serve it. It keeps
// jumpdrive-index a thin client: no embedding model runs in-process, so the
// static / CGO-off binary is preserved and the ML weight lives on the fabric, not
// the host. It is the analogue of the Ollama provider, pointed at the fabric
// rather than a local server.
//
// The Model descriptor is name@dim — the re-embed-on-model-change invariant keys
// on it (see internal/domain/entity.go). The dimension is not known until the
// endpoint first responds, so it is discovered on the first Embed and cached; in
// the service Model() is always called AFTER Embed (both the query and create
// paths embed, then stamp), so a persisted descriptor always carries the
// dimension.
type Fabric struct {
	baseURL string
	model   string
	token   secret.Value
	http    httpDoer
	dim     atomic.Int64 // 0 until the first embedding reveals the vector length
}

// FabricOption configures NewFabric.
type FabricOption func(*Fabric)

// WithFabricHTTPClient overrides the default HTTP client (used in tests).
func WithFabricHTTPClient(d httpDoer) FabricOption { return func(f *Fabric) { f.http = d } }

// NewFabric builds a Fabric embedder. baseURL is the Farcaster switchboard root
// (e.g. https://farcaster.br.thesim.family); model is the embedding model to
// request (e.g. "bge-m3"); token is the optional bearer credential. Construction
// does no I/O, so the endpoint need not be reachable at boot.
func NewFabric(baseURL, model string, token secret.Value, opts ...FabricOption) (*Fabric, error) {
	if baseURL == "" || model == "" {
		return nil, fmt.Errorf("embed: fabric requires baseURL and model")
	}
	f := &Fabric{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		token:   token,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
	for _, opt := range opts {
		opt(f)
	}
	return f, nil
}

// Model returns name@dim once the dimension is known (after the first Embed), or
// the bare name before then. In the service Embed always precedes Model, so a
// stamped descriptor always carries the dimension.
func (f *Fabric) Model() string {
	if d := f.dim.Load(); d > 0 {
		return f.model + "@" + strconv.FormatInt(d, 10)
	}
	return f.model
}

// embedRequest is the OpenAI-compatible /v1/embeddings request; input is a batch.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

// embedResponse is the OpenAI-compatible response: one datum per input, each
// carrying its own index (the order is not guaranteed to match the request).
type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
}

// Embed sends all texts in ONE OpenAI-compatible request (batched, unlike
// Ollama's one-per-call) and returns the vectors in input order.
func (f *Fabric) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: f.model, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if !f.token.IsZero() {
		req.Header.Set("Authorization", "Bearer "+f.token.Reveal())
	}

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: fabric request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embed: fabric status %d", resp.StatusCode)
	}

	var out embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("embed: decode fabric response: %w", err)
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embed: fabric returned %d vectors for %d inputs", len(out.Data), len(texts))
	}

	// Restore input order from data[].index (the OpenAI shape does not promise
	// response order matches the request).
	vecs := make([][]float32, len(texts))
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embed: fabric returned out-of-range index %d", d.Index)
		}
		if vecs[d.Index] != nil {
			return nil, fmt.Errorf("embed: fabric returned duplicate index %d", d.Index)
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embed: fabric returned an empty embedding")
		}
		v := make([]float32, len(d.Embedding))
		for i, x := range d.Embedding {
			v[i] = float32(x)
		}
		vecs[d.Index] = v
	}

	// Cache the dimension for the Model descriptor on first success.
	f.dim.CompareAndSwap(0, int64(len(vecs[0])))
	return vecs, nil
}
