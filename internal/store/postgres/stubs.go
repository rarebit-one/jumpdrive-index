package postgres

import (
	"context"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// The methods below are scheduled for follow-up milestones. They return
// store.ErrNotImplemented so the adapter satisfies the full Store interface today
// (var _ store.Store holds) while the surface lands incrementally; the
// conformance suite skips a capability that reports ErrNotImplemented. They are
// already implemented in the SQLite adapter and land here next: SemanticSearch
// (pgvector), FullTextSearch (tsvector), Neighbors, RetractEdge, and the governed
// Propose / DecideProposal / ListProposals path.

// SemanticSearch will run a pgvector KNN, access-filtered.
func (s *Store) SemanticSearch(ctx context.Context, af access.Filter, q store.VectorQuery) ([]store.ScoredEntity, error) {
	return nil, store.ErrNotImplemented
}

// FullTextSearch will run a tsvector query, access-filtered.
func (s *Store) FullTextSearch(ctx context.Context, af access.Filter, q store.TextQuery) ([]store.ScoredEntity, error) {
	return nil, store.ErrNotImplemented
}

// Neighbors will run a bounded, access-filtered traversal (edges AND nodes
// filtered at every hop).
func (s *Store) Neighbors(ctx context.Context, af access.Filter, q store.NeighborQuery) (store.Subgraph, error) {
	return store.Subgraph{}, store.ErrNotImplemented
}

// RetractEdge will tombstone an edge.
func (s *Store) RetractEdge(ctx context.Context, id domain.EdgeID, actor domain.PrincipalID, dedupeKey string) error {
	return store.ErrNotImplemented
}

// Propose will hold a governed write without projecting it.
func (s *Store) Propose(ctx context.Context, p store.Proposal) (store.ProposalID, error) {
	return "", store.ErrNotImplemented
}

// DecideProposal will approve or reject a held proposal.
func (s *Store) DecideProposal(ctx context.Context, id store.ProposalID, approve bool, approver domain.PrincipalID) (store.ResolveResult, error) {
	return store.ResolveResult{}, store.ErrNotImplemented
}

// ListProposals will list held proposals.
func (s *Store) ListProposals(ctx context.Context, f store.ProposalFilter) ([]store.Proposal, error) {
	return nil, store.ErrNotImplemented
}
