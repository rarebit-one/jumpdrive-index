package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
	"github.com/rarebit-one/jumpdrive-index/internal/store/conformance"
)

// TestPostgresConformance runs the SAME cross-adapter matrix as SQLite against a
// real Postgres. It SKIPS when JDX_TEST_DATABASE_URL is unset (broker pattern),
// so `go test ./...` stays green on a laptop with no database; CI sets the URL
// against a service container and a guard asserts the subtests actually ran. The
// vector / traversal / governed-write subtests self-skip here (those methods are
// ErrNotImplemented on Postgres for now); the core subtests run.
func TestPostgresConformance(t *testing.T) {
	dsn := os.Getenv("JDX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("JDX_TEST_DATABASE_URL unset; skipping Postgres conformance")
	}
	conformance.RunStoreSuite(t, func(t *testing.T) store.Store {
		return openClean(t, dsn)
	})
}

// openClean returns a freshly-migrated store on a truncated schema, so each
// subtest starts from an empty graph (and seq restarts at 1).
func openClean(t *testing.T, dsn string) store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := Open(ctx, Options{DSN: dsn, Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := st.pool.Exec(ctx,
		`TRUNCATE facts, entities, entity_external_ids, entity_labels, embeddings, edges RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}
