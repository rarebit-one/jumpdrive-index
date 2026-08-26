package sqlite

import (
	"context"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// The methods below are scheduled for later milestones. They return
// store.ErrNotImplemented so the adapter satisfies the full Store interface
// today (var _ store.Store holds) while the surface lands incrementally; the
// conformance suite skips a capability that reports ErrNotImplemented.

// FullTextSearch will run FTS5 over an entities_fts virtual table synced at fold
// time.
func (s *Store) FullTextSearch(ctx context.Context, af access.Filter, q store.TextQuery) ([]store.ScoredEntity, error) {
	return nil, store.ErrNotImplemented
}

// Propose holds a governed write (a proposal + approval gate) without projecting
// it, mirroring jumpdrive-web's KnowledgePromotion.
func (s *Store) Propose(ctx context.Context, p store.Proposal) (store.ProposalID, error) {
	return "", store.ErrNotImplemented
}

// DecideProposal approves or rejects a held proposal, replaying an approved one
// through the normal resolve path.
func (s *Store) DecideProposal(ctx context.Context, id store.ProposalID, approve bool, approver domain.PrincipalID) (store.ResolveResult, error) {
	return store.ResolveResult{}, store.ErrNotImplemented
}

// ListProposals lists held proposals, optionally filtered by space.
func (s *Store) ListProposals(ctx context.Context, f store.ProposalFilter) ([]store.Proposal, error) {
	return nil, store.ErrNotImplemented
}
