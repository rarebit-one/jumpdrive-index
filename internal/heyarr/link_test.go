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

func TestLinkHelpersProduceStableKeys(t *testing.T) {
	cases := []struct {
		got  domain.ExternalID
		want string
	}{
		{heyarr.WorkExternalID("w1"), "heyarr-work:w1"},
		{heyarr.EditionExternalID("e1"), "heyarr-edition:e1"},
		{heyarr.AssetExternalID("a1"), "heyarr-asset:a1"},
		{heyarr.Blake3ExternalID("b3"), "heyarr-blake3:b3"},
	}
	for _, tc := range cases {
		if got := tc.got.Key(); got != tc.want {
			t.Errorf("Key() = %q, want %q", got, tc.want)
		}
		if !heyarr.IsHeyarrScheme(tc.got.Scheme) {
			t.Errorf("%q should be recognised as a heyarr scheme", tc.got.Scheme)
		}
	}
	if heyarr.IsHeyarrScheme("tmdb") {
		t.Error("tmdb is not a heyarr bridge scheme")
	}
}

func TestForeignExternalID(t *testing.T) {
	x, ok := heyarr.ForeignExternalID(" TMDB ", " 603 ")
	if !ok {
		t.Fatal("a non-empty scheme/value should map")
	}
	if x.Key() != "tmdb:603" {
		t.Errorf("Key() = %q, want tmdb:603 (lower-cased, trimmed)", x.Key())
	}
	if _, ok := heyarr.ForeignExternalID("", "x"); ok {
		t.Error("empty scheme must not map")
	}
}

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

	// A search hit as heyarr would return it → the reference-by-id anchor.
	hit := heyarr.Work{WorkID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", Title: "Alien"}
	link := hit.ExternalID()

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
	// The heyarr id reads back off the node via heyarr.IDs.
	got := heyarr.IDs(hits[0])
	if len(got) != 1 || got[0].Key() != "heyarr-work:0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b" {
		t.Errorf("HeyarrIDs = %v, want the single heyarr-work anchor", got)
	}
}
