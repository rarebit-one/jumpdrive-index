// Package embed is the embedding-provider boundary. The service computes
// embeddings at write time (auto-embed on create) and at read time (embed the
// search query), then hands the resulting vectors to the store — the store never
// knows which provider produced them, only the Model descriptor stored alongside
// each vector so a model change is detectable. Providers are pluggable and
// optional: with no embedder configured, the service simply skips the semantic
// path and full-text search still works.
package embed

import "context"

// Embedder turns text into vectors in one model's space.
type Embedder interface {
	// Embed returns one vector per input text, in order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model is the descriptor stored on each embedding (e.g. a provider model
	// name); vectors are only comparable within one Model.
	Model() string
}
