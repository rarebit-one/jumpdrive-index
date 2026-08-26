// Package domain is the pure vocabulary of the knowledge graph: entities,
// edges, the append-only facts they are built from, and the pure decisions
// (resolve-before-create) that govern writes.
//
// It imports nothing from the DB, HTTP, or access layers — exactly like
// jumpdrive-broker's internal/domain and internal/placement. Everything here is
// deterministic and unit-testable without a database. IDs are treated as opaque
// strings; minting them (UUIDv7) is the store layer's job, not the domain's, so
// that the resolve decision stays a pure function of its inputs.
package domain

import "time"

// EntityID is an internal, stable identifier for an entity (a UUIDv7 string,
// minted by the store). The domain never generates one; it only carries it.
type EntityID string

// Type is a schema.org @type, e.g. "Movie", "Person", "VideoObject". The set of
// accepted types is an allow-list (see schema.go) — unknown types are rejected
// at the boundary (default-deny), never silently stored.
type Type string

// Visibility is the HARD access floor on a node or edge. It is enforced in SQL
// (the AccessFilter WHERE clause), never as a soft, droppable lens. An edge
// carries its own Visibility independent of its endpoints.
type Visibility string

const (
	VisPrivate Visibility = "private" // owner (+ explicit grants) only
	VisSpace   Visibility = "space"   // any principal who is a member of the entity's Space
	VisPublic  Visibility = "public"  // any authenticated principal
)

// Valid reports whether v is a known visibility. Unknown values are rejected at
// the boundary rather than defaulted.
func (v Visibility) Valid() bool {
	switch v {
	case VisPrivate, VisSpace, VisPublic:
		return true
	default:
		return false
	}
}

// SpaceID scopes an entity/edge to an organizational context (personal, a
// family space, a project, an SME client). Scoping is not access: a node can be
// in a space and still be private. Access defaults may flow from space
// membership, but Visibility is the hard floor.
type SpaceID string

// PrincipalID identifies who owns / asserted something. In the standalone
// (Starchart) build a principal is a local account; in the embedded build it is
// resolved from Jumpdrive's identity model.
type PrincipalID string

// AssertMethod distinguishes a fact a human/tool directly asserted from one the
// system inferred. Keeping them distinct is what lets a reader trust a graph
// that mixes hand-entered facts with AI guesses.
type AssertMethod string

const (
	Asserted AssertMethod = "asserted"
	Inferred AssertMethod = "inferred"
)

// Valid reports whether m is a known assert method.
func (m AssertMethod) Valid() bool {
	return m == Asserted || m == Inferred
}

// Provenance records who/what asserted a fact, when, and how confidently. Every
// entity and edge carries it. Provenance ("who said this") is deliberately
// separate from Visibility ("who may see this").
type Provenance struct {
	Asserter   string       // principal id OR agent/tool name (e.g. "mcp:kate", "ingest:yt-dlp")
	Method     AssertMethod // asserted | inferred
	Source     string       // free text / run id — where the assertion came from
	Confidence float64      // 0..1; an asserted fact defaults to 1.0
	AssertedAt time.Time
}

// ExternalID anchors an entity to an identifier in the outside world (or another
// homelab system). These are the dedup and cross-system join keys — the single
// most valuable discipline for keeping the graph well-linked. An entity without
// one is a future duplicate.
//
// Known schemes include "wikidata" (Q-ids), "tmdb", "imdb", and the heyarr
// bridge schemes "heyarr-work"/"heyarr-edition"/"heyarr-asset" (UUIDv7) and
// "heyarr-blake3" (a content-addressed blob hash — byte identity that survives a
// remux). The scheme set is open; Key() is the canonical comparison form.
type ExternalID struct {
	Scheme string
	Value  string
}

// Key is the canonical "scheme:value" join key used for exact-match dedup.
func (x ExternalID) Key() string { return x.Scheme + ":" + x.Value }

// Embedding is one vector for one (model, field) of an entity. The Model
// descriptor ("name@dim") is stored so a model change is detectable and drives a
// projection rebuild+re-embed rather than silently mixing incompatible spaces.
// Storage differs per adapter (pgvector column vs a float32 blob + Go KNN) but
// the domain shape is identical.
type Embedding struct {
	Model  string    // e.g. "text-embedding-3-small@1536"
	Field  string    // what was embedded, e.g. "name" or "abstract"
	Vector []float32 // len == the model's dimension
}

// Entity is a node in the graph: a typed thing with a JSON-LD property bag,
// external-id anchors, provenance, scoping/ownership/visibility, and zero or
// more embeddings. Props is an opaque JSON-LD bag (validated only for its @type
// and a small predicate allow-list) so the ontology can grow without schema
// migrations; the searchable fields are extracted from it at projection time.
type Entity struct {
	ID          EntityID
	Type        Type
	Props       []byte // JSON-LD property bag (json.RawMessage semantics)
	ExternalIDs []ExternalID
	Provenance  Provenance
	Space       SpaceID
	Owner       PrincipalID
	Visibility  Visibility
	Embeddings  []Embedding
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Deleted     bool // tombstone: excluded from the projection, retained in the fact log
}

// ExternalKeys returns the canonical join keys of e's external ids, in input
// order. Convenience for the resolve path and store lookups.
func (e Entity) ExternalKeys() []string {
	keys := make([]string, 0, len(e.ExternalIDs))
	for _, x := range e.ExternalIDs {
		keys = append(keys, x.Key())
	}
	return keys
}
