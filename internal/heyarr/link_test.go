package heyarr_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/heyarr"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

// TestReferenceByIDRoundTrip proves a jumpdrive-index entity anchored to a heyarr
// work by id resolves back through the real store by that external key — the
// reference-by-id contract, with no store change and no copy of heyarr's data.
func TestReferenceByIDRoundTrip(t *testing.T) {
	st, err := sqlite.Open(sqlite.Options{
		Path:       filepath.Join(t.TempDir(), "jdx.db"),
		Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The reference-by-id anchor ingest attaches after get_external_ids resolves.
	const workID = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	link := heyarr.WorkExternalID(workID)

	res, err := st.AppendEntityFact(ctx, store.AppendEntityInput{
		Candidate: domain.Entity{
			Type:        "Movie",
			Props:       json.RawMessage(`{"name":"Alien"}`),
			Visibility:  "public",
			Owner:       "kate",
			ExternalIDs: []domain.ExternalID{link},
		},
		Writer: "kate", DedupeKey: "link-1", Actor: "kate", Policy: domain.ResolveAuto,
	})
	if err != nil {
		t.Fatalf("AppendEntityFact: %v", err)
	}

	hits, err := st.ResolveByExternalID(ctx, access.Filter{Principal: "kate", AllowPublic: true}, []string{link.Key()})
	if err != nil {
		t.Fatalf("ResolveByExternalID: %v", err)
	}
	if len(hits) != 1 || hits[0].ID != res.Entity.ID {
		t.Fatalf("resolve by %q returned %v, want the linked entity", link.Key(), hits)
	}
	// The heyarr-work id reads back directly off the external ids of the node.
	ids := hits[0].ExternalIDs
	if len(ids) != 1 || ids[0].Key() != "heyarr-work:"+workID {
		t.Errorf("ExternalIDs = %v, want the single heyarr-work anchor", ids)
	}
}
