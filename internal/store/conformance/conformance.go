// Package conformance is the cross-adapter behavioural matrix. The SAME suite is
// run against every store.Store implementation (SQLite today, Postgres next), so
// the adapters cannot diverge silently — the analogue of jumpdrive-broker's
// SQL-vs-Go drift test, lifted to the whole storage seam. A behaviour that passes
// on one adapter and fails the other is caught here.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// extractName reads the "name" property from a JSON-LD bag (test convenience).
func extractName(props json.RawMessage) string {
	var m map[string]any
	if json.Unmarshal(props, &m) != nil {
		return ""
	}
	if n, ok := m["name"].(string); ok {
		return n
	}
	return ""
}

// OpenFunc returns a fresh, migrated, empty store for one subtest.
type OpenFunc func(t *testing.T) store.Store

// RunStoreSuite runs the full matrix against the adapter that open produces.
func RunStoreSuite(t *testing.T, open OpenFunc) {
	t.Run("insert_and_get", func(t *testing.T) { testInsertAndGet(t, open(t)) })
	t.Run("idempotent_replay", func(t *testing.T) { testIdempotentReplay(t, open(t)) })
	t.Run("external_id_attaches", func(t *testing.T) { testExternalAttach(t, open(t)) })
	t.Run("external_collision_merges", func(t *testing.T) { testExternalMerge(t, open(t)) })
	t.Run("access_filter_hides_private", func(t *testing.T) { testAccessFilter(t, open(t)) })
	t.Run("edge_append_and_idempotency", func(t *testing.T) { testEdge(t, open(t)) })
	t.Run("rebuild_reproduces_projection", func(t *testing.T) { testRebuild(t, open(t)) })
}

var ctx = context.Background()

func readAF(principal string, spaces ...string) access.Filter {
	af := access.Filter{Principal: domain.PrincipalID(principal), AllowPublic: true}
	for _, s := range spaces {
		af.Spaces = append(af.Spaces, domain.SpaceID(s))
	}
	return af
}

func mkEntity(typ, name, vis, owner string, ext ...domain.ExternalID) domain.Entity {
	return domain.Entity{
		Type:        domain.Type(typ),
		Props:       []byte(fmt.Sprintf(`{"name":%q}`, name)),
		Visibility:  domain.Visibility(vis),
		Owner:       domain.PrincipalID(owner),
		ExternalIDs: ext,
	}
}

func mustAppend(t *testing.T, st store.Store, e domain.Entity, writer, dedupe string) store.ResolveResult {
	t.Helper()
	res, err := st.AppendEntityFact(ctx, store.AppendEntityInput{
		Candidate: e, Writer: domain.WriterID(writer), DedupeKey: dedupe,
		Actor: domain.PrincipalID(writer), Policy: domain.ResolveAuto,
	})
	if err != nil {
		t.Fatalf("AppendEntityFact: %v", err)
	}
	return res
}

func testInsertAndGet(t *testing.T, st store.Store) {
	res := mustAppend(t, st, mkEntity("Movie", "Alien", "public", "kate"), "kate", "k1")
	if res.Action != domain.ActionInsertNew {
		t.Fatalf("action = %q, want insert_new", res.Action)
	}
	got, err := st.GetEntity(ctx, readAF("kate"), res.Entity.ID)
	if err != nil {
		t.Fatalf("GetEntity: %v", err)
	}
	if got.Type != "Movie" {
		t.Errorf("type = %q, want Movie", got.Type)
	}
	if name := extractName(got.Props); name != "Alien" {
		t.Errorf("name = %q, want Alien", name)
	}
	if head, _ := st.ProjectionHead(ctx); head != 1 {
		t.Errorf("ProjectionHead = %d, want 1", head)
	}
}

func testIdempotentReplay(t *testing.T, st store.Store) {
	first := mustAppend(t, st, mkEntity("Movie", "Alien", "public", "kate"), "kate", "same-key")

	_, err := st.AppendEntityFact(ctx, store.AppendEntityInput{
		Candidate: mkEntity("Movie", "Alien", "public", "kate"),
		Writer:    "kate", DedupeKey: "same-key", Actor: "kate", Policy: domain.ResolveAuto,
	})
	if !errors.Is(err, store.ErrDuplicateFact) {
		t.Fatalf("replay error = %v, want ErrDuplicateFact", err)
	}
	// The replay must not have created a second node or a second fact.
	if head, _ := st.ProjectionHead(ctx); head != 1 {
		t.Errorf("ProjectionHead = %d after replay, want 1", head)
	}
	got, _ := st.GetEntity(ctx, readAF("kate"), first.Entity.ID)
	if got.ID != first.Entity.ID {
		t.Errorf("replay resolved to %q, want original %q", got.ID, first.Entity.ID)
	}
}

func testExternalAttach(t *testing.T, st store.Store) {
	tmdb := domain.ExternalID{Scheme: "tmdb", Value: "603"}
	a := mustAppend(t, st, mkEntity("Movie", "The Matrix", "public", "kate", tmdb), "kate", "a")
	// Same external id, DIFFERENT dedupe key → not idempotency, must resolve+attach.
	b := mustAppend(t, st, mkEntity("Movie", "The Matrix", "public", "kate", tmdb), "kate", "b")

	if b.Action != domain.ActionAttach || b.MatchKind != domain.MatchExternal {
		t.Fatalf("second assert = %s/%s, want attach/exact_external", b.Action, b.MatchKind)
	}
	if b.Entity.ID != a.Entity.ID {
		t.Errorf("attached to %q, want the existing %q", b.Entity.ID, a.Entity.ID)
	}
	// Exactly one entity should carry the key.
	hits, _ := st.ResolveByExternalID(ctx, readAF("kate"), []string{tmdb.Key()})
	if len(hits) != 1 {
		t.Errorf("external key resolves to %d entities, want 1", len(hits))
	}
}

func testExternalMerge(t *testing.T, st store.Store) {
	x1 := domain.ExternalID{Scheme: "tmdb", Value: "1"}
	x2 := domain.ExternalID{Scheme: "imdb", Value: "tt2"}
	a := mustAppend(t, st, mkEntity("Movie", "A", "public", "kate", x1), "kate", "a")
	b := mustAppend(t, st, mkEntity("Movie", "B", "public", "kate", x2), "kate", "b")
	if a.Entity.ID == b.Entity.ID {
		t.Fatal("setup: A and B should be distinct nodes")
	}
	// A candidate carrying BOTH external ids bridges the two existing nodes.
	c := mustAppend(t, st, mkEntity("Movie", "A", "public", "kate", x1, x2), "kate", "c")
	if c.Action != domain.ActionMerge {
		t.Fatalf("action = %q, want merge", c.Action)
	}
	if len(c.MergedFrom) != 1 {
		t.Errorf("mergedFrom = %v, want exactly one dropped node", c.MergedFrom)
	}
	// The survivor carries both external ids; the dropped node is gone.
	hits1, _ := st.ResolveByExternalID(ctx, readAF("kate"), []string{x1.Key()})
	hits2, _ := st.ResolveByExternalID(ctx, readAF("kate"), []string{x2.Key()})
	if len(hits1) != 1 || len(hits2) != 1 || hits1[0].ID != hits2[0].ID {
		t.Errorf("after merge both keys should resolve to ONE surviving node; got %v / %v", hits1, hits2)
	}
	survivor := hits1[0].ID
	dropped := a.Entity.ID
	if survivor == dropped {
		dropped = b.Entity.ID
	}
	if _, err := st.GetEntity(ctx, readAF("kate"), dropped); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("dropped node %q should be gone, got err %v", dropped, err)
	}
}

func testAccessFilter(t *testing.T, st store.Store) {
	res := mustAppend(t, st, mkEntity("Note", "secret", "private", "alice"), "alice", "n1")
	if _, err := st.GetEntity(ctx, readAF("alice"), res.Entity.ID); err != nil {
		t.Errorf("owner alice should read her private note: %v", err)
	}
	if _, err := st.GetEntity(ctx, readAF("bob"), res.Entity.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("non-owner bob must NOT read alice's private note; got err %v", err)
	}
}

func testEdge(t *testing.T, st store.Store) {
	m := mustAppend(t, st, mkEntity("Movie", "Alien", "public", "kate"), "kate", "m")
	v := mustAppend(t, st, mkEntity("VideoObject", "Kroft on Alien", "public", "kate"), "kate", "v")

	edge := domain.Edge{
		Predicate: "about", From: v.Entity.ID, To: m.Entity.ID,
		Visibility: "public", Owner: "kate",
	}
	got, err := st.AppendEdgeFact(ctx, store.AppendEdgeInput{Edge: edge, Writer: "kate", DedupeKey: "e1", Actor: "kate"})
	if err != nil {
		t.Fatalf("AppendEdgeFact: %v", err)
	}
	if got.ID == "" || got.Predicate != "about" {
		t.Errorf("edge = %+v, want a minted id and predicate about", got)
	}
	// Idempotent replay of the same edge key.
	if _, err := st.AppendEdgeFact(ctx, store.AppendEdgeInput{Edge: edge, Writer: "kate", DedupeKey: "e1", Actor: "kate"}); !errors.Is(err, store.ErrDuplicateFact) {
		t.Errorf("edge replay error = %v, want ErrDuplicateFact", err)
	}
}

func testRebuild(t *testing.T, st store.Store) {
	tmdb := domain.ExternalID{Scheme: "tmdb", Value: "603"}
	a := mustAppend(t, st, mkEntity("Movie", "The Matrix", "public", "kate", tmdb), "kate", "a")
	mustAppend(t, st, mkEntity("Movie", "The Matrix", "public", "kate", tmdb), "kate", "b") // attach
	before, _ := st.GetEntity(ctx, readAF("kate"), a.Entity.ID)

	if err := st.RebuildProjection(ctx); err != nil {
		t.Fatalf("RebuildProjection: %v", err)
	}
	after, err := st.GetEntity(ctx, readAF("kate"), a.Entity.ID)
	if err != nil {
		t.Fatalf("GetEntity after rebuild: %v", err)
	}
	if after.Type != before.Type || extractName(after.Props) != extractName(before.Props) {
		t.Errorf("rebuild changed the entity: before %+v after %+v", before, after)
	}
	if len(after.ExternalIDs) != 1 || after.ExternalIDs[0].Key() != tmdb.Key() {
		t.Errorf("rebuild lost/changed external ids: %v", after.ExternalIDs)
	}
}
