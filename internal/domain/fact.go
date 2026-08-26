package domain

import "time"

// FactKind enumerates the append-only events the graph is built from. Everything
// queryable (the entities/edges/embeddings projection) is derivable purely by
// folding facts in Seq order — the projection is a disposable cache, never the
// system of record.
type FactKind string

// FactKind values — the append-only events the graph folds from.
const (
	FactEntityAsserted  FactKind = "entity.asserted"
	FactEntityRetracted FactKind = "entity.retracted"
	FactEdgeAsserted    FactKind = "edge.asserted"
	FactEdgeRetracted   FactKind = "edge.retracted"
	// FactEntityMerged records that resolve decided two ids are the same thing:
	// the dropped id's edges are re-pointed and its external ids unioned onto the
	// kept id. A merge is far dearer to undo than a duplicate, so it is only ever
	// emitted on strong evidence (an exact external-id collision, or a vector
	// score above the conservative auto-merge floor).
	FactEntityMerged FactKind = "entity.merged"
)

// Valid reports whether k is a known fact kind.
func (k FactKind) Valid() bool {
	switch k {
	case FactEntityAsserted, FactEntityRetracted,
		FactEdgeAsserted, FactEdgeRetracted, FactEntityMerged:
		return true
	default:
		return false
	}
}

// WriterID names an append stream (e.g. "mcp:kate", "http:svc", "import:tmdb").
// Idempotency is scoped per writer: the same DedupeKey from two different
// writers is two intentional writes, not a collision.
type WriterID string

// Fact is one immutable record in the per-graph append-only log. Seq is a
// monotonic, gapless-by-construction version (the heyarr events pattern);
// (Writer, DedupeKey) is unique, so a replay is a no-op rather than a duplicate
// node. Payload is the full asserted snapshot of the entity or edge, so the
// projection can be rebuilt from the log alone.
type Fact struct {
	Seq       int64
	ID        string // UUIDv7, stable across replay
	Kind      FactKind
	Subject   string // EntityID or EdgeID (as string) this fact is about
	Writer    WriterID
	DedupeKey string
	Payload   []byte // JSON snapshot of the entity/edge
	Actor     PrincipalID
	CreatedAt time.Time
}
