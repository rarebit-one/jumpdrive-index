package sqlite

import (
	"context"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
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
