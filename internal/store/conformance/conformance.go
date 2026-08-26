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
	"math"
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
	t.Run("vector_resolve_two_band", func(t *testing.T) { testVectorResolve(t, open(t)) })
	t.Run("semantic_search_ranks_and_filters", func(t *testing.T) { testSemanticSearch(t, open(t)) })
	t.Run("neighbors_access_filtered_traversal", func(t *testing.T) { testNeighbors(t, open(t)) })
	t.Run("governed_propose_approve_reject", func(t *testing.T) { testProposals(t, open(t)) })
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

// vecAt returns the 2-D unit vector [c, sqrt(1-c^2)], whose cosine with the base
// vector [1,0] is exactly c — a deterministic way to hit a target similarity.
func vecAt(c float32) []float32 {
	return []float32{c, float32(math.Sqrt(float64(1 - c*c)))}
}

func mkEntityVec(typ, name, vis, owner, model string, vec []float32, ext ...domain.ExternalID) domain.Entity {
	e := mkEntity(typ, name, vis, owner, ext...)
	e.Embeddings = []domain.Embedding{{Model: model, Field: "name", Vector: vec}}
	return e
}

// vectorsSupported probes whether the adapter implements vector search, so the
// vector subtests skip (rather than fail) on an adapter that hasn't built it yet.
func vectorsSupported(st store.Store) bool {
	_, err := st.SemanticSearch(ctx, readAF("probe"),
		store.VectorQuery{Model: "probe@2", Vector: []float32{1, 0}, Limit: 1})
	return !errors.Is(err, store.ErrNotImplemented)
}

func mustEdge(t *testing.T, st store.Store, pred string, from, to domain.EntityID, vis, owner, writer, dedupe string) {
	t.Helper()
	_, err := st.AppendEdgeFact(ctx, store.AppendEdgeInput{
		Edge: domain.Edge{
			Predicate: domain.Predicate(pred), From: from, To: to,
			Visibility: domain.Visibility(vis), Owner: domain.PrincipalID(owner),
		},
		Writer: domain.WriterID(writer), DedupeKey: dedupe, Actor: domain.PrincipalID(writer),
	})
	if err != nil {
		t.Fatalf("AppendEdgeFact(%s): %v", dedupe, err)
	}
}

func hasEntityNamed(sub store.Subgraph, name string) bool {
	for _, e := range sub.Entities {
		if extractName(e.Props) == name {
			return true
		}
	}
	return false
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

// testVectorResolve exercises resolve's vector two-band. Each band uses a
// DIFFERENT @type so the same-type KNN cannot cross-match between cases, keeping
// the outcomes deterministic. Similarity to the base vector [1,0] is set exactly
// via vecAt.
func testVectorResolve(t *testing.T, st store.Store) {
	if !vectorsSupported(st) {
		t.Skip("adapter has no vector search yet")
	}
	const model = "test@2"
	base := vecAt(1.0)

	// Auto-merge band (cosine 0.97 >= 0.94): attach to the near-duplicate.
	a1 := mustAppend(t, st, mkEntityVec("Movie", "Alien", "public", "kate", model, base), "kate", "va1")
	b1 := mustAppend(t, st, mkEntityVec("Movie", "Alien (dup)", "public", "kate", model, vecAt(0.97)), "kate", "vb1")
	if b1.Action != domain.ActionAttach || b1.MatchKind != domain.MatchVector {
		t.Errorf("auto band: action=%s kind=%s, want attach/vector", b1.Action, b1.MatchKind)
	}
	if b1.Entity.ID != a1.Entity.ID {
		t.Errorf("auto band: attached to %q, want existing %q", b1.Entity.ID, a1.Entity.ID)
	}

	// Review band (0.86 <= 0.90 < 0.94): insert a NEW node, never merge.
	a2 := mustAppend(t, st, mkEntityVec("Book", "Dune", "public", "kate", model, base), "kate", "va2")
	b2 := mustAppend(t, st, mkEntityVec("Book", "Dune?", "public", "kate", model, vecAt(0.90)), "kate", "vb2")
	if b2.Action != domain.ActionInsertFlagged {
		t.Errorf("review band: action=%s, want insert_flagged", b2.Action)
	}
	if b2.Entity.ID == a2.Entity.ID {
		t.Error("review band must NOT merge — a false merge is dear to undo")
	}

	// Below the review floor (0.80 < 0.86): a plain new node, no flag.
	a3 := mustAppend(t, st, mkEntityVec("Article", "Essay", "public", "kate", model, base), "kate", "va3")
	b3 := mustAppend(t, st, mkEntityVec("Article", "Other essay", "public", "kate", model, vecAt(0.80)), "kate", "vb3")
	if b3.Action != domain.ActionInsertNew {
		t.Errorf("below band: action=%s, want insert_new", b3.Action)
	}
	if b3.Entity.ID == a3.Entity.ID {
		t.Error("below band must not attach")
	}
}

// testSemanticSearch checks the public KNN ranks by similarity and applies the
// access filter (filter-then-rank): a non-owner never sees another principal's
// private entity even if it is the closest match.
func testSemanticSearch(t *testing.T, st store.Store) {
	if !vectorsSupported(st) {
		t.Skip("adapter has no vector search yet")
	}
	const model = "srch@2"
	mustAppend(t, st, mkEntityVec("Movie", "near", "public", "kate", model, vecAt(0.98)), "kate", "s1")
	mustAppend(t, st, mkEntityVec("Movie", "far", "public", "kate", model, vecAt(0.10)), "kate", "s2")
	// alice's private entity is the CLOSEST match, but kate must not see it.
	mustAppend(t, st, mkEntityVec("Movie", "secret", "private", "alice", model, vecAt(0.99)), "alice", "s3")

	hits, err := st.SemanticSearch(ctx, readAF("kate"),
		store.VectorQuery{Model: model, Vector: vecAt(1.0), Type: "Movie", Limit: 10})
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2 (the two public movies)", len(hits))
	}
	if extractName(hits[0].Entity.Props) != "near" {
		t.Errorf("top hit = %q, want \"near\" (highest cosine)", extractName(hits[0].Entity.Props))
	}
	for _, h := range hits {
		if extractName(h.Entity.Props) == "secret" {
			t.Error("access leak: kate must not see alice's private entity in search")
		}
		if h.Score < 0 || h.Score > 1.0001 {
			t.Errorf("cosine score out of range: %v", h.Score)
		}
	}
}

// testNeighbors is THE edge-visibility safety test. One graph, two viewers:
//
//	A —e1(pub)— B —e2(pub)— C          (a plain visible chain)
//	A —e3(pub)— H(alice-private) —e4(pub)— D   (a public edge to a HIDDEN node)
//	A —e5(alice-private)— E(pub)        (a PRIVATE edge between two public nodes)
//
// kate (sees public + her own) must reach only A,B,C: the hidden node H may not
// bridge to D, and the private edge e5 may not be traversed to E even though E is
// public. alice (owns H and e5) sees the whole graph. This proves access is
// filtered per hop on edges AND nodes independently.
func testNeighbors(t *testing.T, st store.Store) {
	if _, err := st.Neighbors(ctx, readAF("probe"), store.NeighborQuery{Start: "none"}); errors.Is(err, store.ErrNotImplemented) {
		t.Skip("adapter has no traversal yet")
	}

	a := mustAppend(t, st, mkEntity("Movie", "A", "public", "kate"), "kate", "na")
	b := mustAppend(t, st, mkEntity("Movie", "B", "public", "kate"), "kate", "nb")
	c := mustAppend(t, st, mkEntity("Movie", "C", "public", "kate"), "kate", "nc")
	h := mustAppend(t, st, mkEntity("Note", "H", "private", "alice"), "alice", "nh")
	d := mustAppend(t, st, mkEntity("Movie", "D", "public", "kate"), "kate", "nd")
	e := mustAppend(t, st, mkEntity("Movie", "E", "public", "kate"), "kate", "ne")

	mustEdge(t, st, "relatedTo", a.Entity.ID, b.Entity.ID, "public", "kate", "kate", "e1")
	mustEdge(t, st, "relatedTo", b.Entity.ID, c.Entity.ID, "public", "kate", "kate", "e2")
	mustEdge(t, st, "relatedTo", a.Entity.ID, h.Entity.ID, "public", "kate", "kate", "e3")
	mustEdge(t, st, "relatedTo", h.Entity.ID, d.Entity.ID, "public", "kate", "kate", "e4")
	mustEdge(t, st, "relatedTo", a.Entity.ID, e.Entity.ID, "private", "alice", "alice", "e5")

	// kate: the restricted view.
	ksub, err := st.Neighbors(ctx, readAF("kate"), store.NeighborQuery{Start: a.Entity.ID, MaxHops: 2})
	if err != nil {
		t.Fatalf("kate Neighbors: %v", err)
	}
	for _, want := range []string{"A", "B", "C"} {
		if !hasEntityNamed(ksub, want) {
			t.Errorf("kate should reach %s", want)
		}
	}
	if hasEntityNamed(ksub, "H") {
		t.Error("LEAK: kate saw alice's private node H")
	}
	if hasEntityNamed(ksub, "D") {
		t.Error("LEAK: hidden node H bridged kate to D")
	}
	if hasEntityNamed(ksub, "E") {
		t.Error("LEAK: kate traversed a private edge to reach E")
	}

	// alice: owns H and e5, so she sees the whole graph.
	asub, err := st.Neighbors(ctx, readAF("alice"), store.NeighborQuery{Start: a.Entity.ID, MaxHops: 2})
	if err != nil {
		t.Fatalf("alice Neighbors: %v", err)
	}
	for _, want := range []string{"A", "B", "C", "H", "D", "E"} {
		if !hasEntityNamed(asub, want) {
			t.Errorf("alice should reach %s", want)
		}
	}

	// A start the caller cannot see is ErrNotFound, not an empty traversal.
	if _, err := st.Neighbors(ctx, readAF("kate"), store.NeighborQuery{Start: h.Entity.ID, MaxHops: 1}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("kate Neighbors of a hidden start: err=%v, want ErrNotFound", err)
	}
}

// testProposals exercises the governed write path: a proposal is held (not
// projected), listed, then either promoted (replayed through resolve) or
// discarded (nothing written). A decided proposal cannot be decided again.
func testProposals(t *testing.T, st store.Store) {
	proposeMovie := func(tmdb, dedupe string) store.ProposalID {
		t.Helper()
		in := store.AppendEntityInput{
			Candidate: domain.Entity{
				Type: "Movie", Props: json.RawMessage(`{"name":"Proposed"}`),
				Visibility: "public", Owner: "kate",
				ExternalIDs: []domain.ExternalID{{Scheme: "tmdb", Value: tmdb}},
			},
			Writer: "kate", DedupeKey: dedupe, Actor: "kate", Policy: domain.ResolveAuto,
		}
		payload, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("marshal input: %v", err)
		}
		id, err := st.Propose(ctx, store.Proposal{Kind: domain.FactEntityAsserted, Proposer: "kate", Space: "fam", Payload: payload})
		if errors.Is(err, store.ErrNotImplemented) {
			t.Skip("adapter has no governed writes yet")
		}
		if err != nil {
			t.Fatalf("Propose: %v", err)
		}
		return id
	}

	key := func(v string) []string { return []string{(domain.ExternalID{Scheme: "tmdb", Value: v}).Key()} }

	// --- approve path ---
	id := proposeMovie("999", "p-approve")

	pending, err := st.ListProposals(ctx, store.ProposalFilter{Space: "fam"})
	if err != nil {
		t.Fatalf("ListProposals: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id {
		t.Fatalf("pending = %v, want exactly the one proposal", pending)
	}
	// Held: nothing is projected yet.
	if hits, _ := st.ResolveByExternalID(ctx, readAF("kate"), key("999")); len(hits) != 0 {
		t.Errorf("a held proposal must not be projected; found %d entities", len(hits))
	}

	res, err := st.DecideProposal(ctx, id, true, "boss")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if res.Entity.ID == "" {
		t.Error("approved proposal returned no entity")
	}
	if hits, _ := st.ResolveByExternalID(ctx, readAF("kate"), key("999")); len(hits) != 1 {
		t.Errorf("after approval the entity should be projected; found %d", len(hits))
	}
	if pending, _ := st.ListProposals(ctx, store.ProposalFilter{Space: "fam"}); len(pending) != 0 {
		t.Errorf("approved proposal should no longer be pending; %d remain", len(pending))
	}
	// A decided proposal cannot be decided again.
	if _, err := st.DecideProposal(ctx, id, true, "boss"); !errors.Is(err, store.ErrConflict) {
		t.Errorf("re-deciding: err=%v, want ErrConflict", err)
	}

	// --- reject path ---
	rid := proposeMovie("888", "p-reject")
	if _, err := st.DecideProposal(ctx, rid, false, "boss"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if hits, _ := st.ResolveByExternalID(ctx, readAF("kate"), key("888")); len(hits) != 0 {
		t.Errorf("a rejected proposal must write nothing; found %d entities", len(hits))
	}

	// Deciding a proposal that does not exist.
	if _, err := st.DecideProposal(ctx, "no-such-proposal", true, "boss"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deciding a missing proposal: err=%v, want ErrNotFound", err)
	}
}
