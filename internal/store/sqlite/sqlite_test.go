package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
	"github.com/rarebit-one/jumpdrive-index/internal/store/conformance"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := sqlite.Open(sqlite.Options{
		Path:       filepath.Join(t.TempDir(), "jdx.db"),
		Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestSQLiteConformance runs the whole cross-adapter matrix against SQLite. The
// same suite will run against the Postgres adapter, keeping the two honest.
func TestSQLiteConformance(t *testing.T) {
	conformance.RunStoreSuite(t, openStore)
}

func TestMigrateIsIdempotent(t *testing.T) {
	st := openStore(t)
	// openStore already migrated once; a second call must be a clean no-op.
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}

func TestOpenRejectsBadInput(t *testing.T) {
	if _, err := sqlite.Open(sqlite.Options{Path: "", Thresholds: domain.Thresholds{AutoMerge: 0.9, Review: 0.8}}); err == nil {
		t.Error("empty path should be rejected")
	}
	if _, err := sqlite.Open(sqlite.Options{
		Path:       filepath.Join(t.TempDir(), "x.db"),
		Thresholds: domain.Thresholds{AutoMerge: 0.5, Review: 0.9}, // AutoMerge must exceed Review
	}); err == nil {
		t.Error("invalid thresholds should be rejected")
	}
}
