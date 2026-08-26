// Package store is the storage seam. It defines one Store interface behind which
// two adapters live: postgres (hosted, multi-tenant, pgvector) and sqlite
// (homelab, pure-Go, CGO-off, float32-blob embeddings + Go KNN). Each adapter
// asserts `var _ store.Store = (*Store)(nil)` and is exercised by the SAME
// behavioural matrix in store/conformance so the two cannot diverge silently.
//
// Interface discipline (from jumpdrive-broker): every atomic mutation is EXACTLY
// ONE method — the seam is drawn at the unit of atomicity — and there is NO
// exposed transaction handle. AppendEntityFact folds a fact AND advances the
// projection in one transaction; a reader can therefore never see an entity the
// log does not record, or vice-versa.
package store

import (
	"context"
	"errors"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// Sentinel errors. Callers (HTTP/MCP) translate these to status codes; the store
// never leaks a driver error shape.
var (
	ErrNotFound      = errors.New("store: not found")
	ErrConflict      = errors.New("store: conflicts with an existing row")
	ErrDuplicateFact = errors.New("store: dedupe key already appended") // idempotent replay
)

// AppendEntityInput carries a candidate entity plus the resolve policy. The store
// runs resolve-before-create (domain.Resolve) INSIDE the append transaction, so
// the dedup decision and the write cannot race.
type AppendEntityInput struct {
	Candidate domain.Entity
	Writer    domain.WriterID
	DedupeKey string // caller-supplied; the store derives one when empty (domain.DeriveDedupeKey)
	Actor     domain.PrincipalID
	Policy    domain.ResolvePolicy
}

// ResolveResult reports what the write did, so an agent can understand the
// outcome (created a node, attached to an existing one, merged duplicates...).
type ResolveResult struct {
	Entity     domain.Entity
	Action     domain.ResolveAction
	MatchKind  domain.MatchKind
	MergedFrom []domain.EntityID
}

// AppendEdgeInput carries a first-class edge to assert.
type AppendEdgeInput struct {
	Edge      domain.Edge
	Writer    domain.WriterID
	DedupeKey string
	Actor     domain.PrincipalID
}

// NeighborQuery bounds a multi-hop traversal. The AccessFilter is applied at
// EVERY hop to edges AND nodes, so a hidden node cannot bridge two visible ones.
type NeighborQuery struct {
	Start      domain.EntityID
	Predicates []domain.Predicate // empty = any allowed predicate
	MaxHops    int                // bounded (<= 3)
	Limit      int
}

// Subgraph is a traversal result.
type Subgraph struct {
	Entities []domain.Entity
	Edges    []domain.Edge
}

// VectorQuery / TextQuery drive the two search modes; hybrid fuses them in Go.
type VectorQuery struct {
	Vector []float32
	Type   domain.Type // optional same-type constraint
	Limit  int
}

type TextQuery struct {
	Text  string
	Type  domain.Type
	Limit int
}

// ScoredEntity is a search hit.
type ScoredEntity struct {
	Entity domain.Entity
	Score  float64
}

// Proposal is a held (not-yet-projected) governed write, mirroring jumpdrive's
// KnowledgePromotion: the exact inputs are stored so an approver can replay them
// through the normal resolve path.
type ProposalID string

type Proposal struct {
	ID       ProposalID
	Kind     domain.FactKind // entity.asserted | edge.asserted
	Proposer domain.PrincipalID
	Space    domain.SpaceID
	Payload  []byte // the exact AppendEntityInput/AppendEdgeInput, serialized
}

type ProposalFilter struct {
	Space domain.SpaceID
}

// Store is the whole persistence surface. Every read takes an access.AccessFilter
// that becomes a WHERE clause.
type Store interface {
	// Lifecycle.
	Migrate(ctx context.Context) error
	Close() error

	// Writes — each is one atomic method (fact append + projection advance).
	AppendEntityFact(ctx context.Context, in AppendEntityInput) (ResolveResult, error)
	AppendEdgeFact(ctx context.Context, in AppendEdgeInput) (domain.Edge, error)
	MergeEntities(ctx context.Context, keep, drop domain.EntityID, actor domain.PrincipalID, dedupeKey string) error
	RetractEntity(ctx context.Context, id domain.EntityID, actor domain.PrincipalID, dedupeKey string) error
	RetractEdge(ctx context.Context, id domain.EdgeID, actor domain.PrincipalID, dedupeKey string) error

	// Governed writes (propose → approve).
	Propose(ctx context.Context, p Proposal) (ProposalID, error)
	DecideProposal(ctx context.Context, id ProposalID, approve bool, approver domain.PrincipalID) (ResolveResult, error)
	ListProposals(ctx context.Context, f ProposalFilter) ([]Proposal, error)

	// Reads — access-filtered.
	GetEntity(ctx context.Context, af access.AccessFilter, id domain.EntityID) (domain.Entity, error)
	ResolveByExternalID(ctx context.Context, af access.AccessFilter, keys []string) ([]domain.Entity, error)
	Neighbors(ctx context.Context, af access.AccessFilter, q NeighborQuery) (Subgraph, error)
	SemanticSearch(ctx context.Context, af access.AccessFilter, q VectorQuery) ([]ScoredEntity, error)
	FullTextSearch(ctx context.Context, af access.AccessFilter, q TextQuery) ([]ScoredEntity, error)

	// Projection lifecycle — the projection is a disposable fold of the fact log.
	RebuildProjection(ctx context.Context) error
	ProjectionHead(ctx context.Context) (int64, error)
}
