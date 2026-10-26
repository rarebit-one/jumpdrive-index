package service_test

// The cross-space matrix (#748, docs/index-principal-adr.md "Test plan (Index)")
// drives the service through the REAL jumpdrive access model against a fake
// control-plane /authorize, with the space shapes the control plane issues:
// principal A holds jd:ws:A and jd:org:A, principal B holds org B's. It pins
// that one tenant can neither read, write into, nor link against another's
// space, and that a link attempt is not an existence oracle.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/access/jumpdrive"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/service"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

const (
	wsA  domain.SpaceID = "jd:ws:aaaaaaaa-0000-4000-8000-000000000001"
	orgA domain.SpaceID = "jd:org:aaaaaaaa-0000-4000-8000-0000000000a0"
	wsB  domain.SpaceID = "jd:ws:bbbbbbbb-0000-4000-8000-000000000001"
	orgB domain.SpaceID = "jd:org:bbbbbbbb-0000-4000-8000-0000000000b0"
)

const (
	orgAID = "aaaaaaaa-0000-4000-8000-0000000000a0"
	wsAID  = "aaaaaaaa-0000-4000-8000-000000000001"
	orgBID = "bbbbbbbb-0000-4000-8000-0000000000b0"
	wsBID  = "bbbbbbbb-0000-4000-8000-000000000001"
)

// decisions the fake control plane hands out, by bearer.
var crossSpaceDecisions = map[string]map[string]any{
	// alice: org A admin, full access. Writes her workspace and the org space.
	"alice-full": decision("jd:acct:alice", orgAID, wsAID, []domain.SpaceID{wsA, orgA}, []domain.SpaceID{wsA, orgA}, []domain.SpaceID{wsA}, "full"),
	// alice again, with an mcp:read token: same reads, no writes, no approves.
	"alice-read": decision("jd:acct:alice", orgAID, wsAID, []domain.SpaceID{wsA, orgA}, nil, nil, "read"),
	// andy: a workspace writer in org A (not an org admin).
	"andy-full": decision("jd:acct:andy", orgAID, wsAID, []domain.SpaceID{wsA, orgA}, []domain.SpaceID{wsA}, nil, "full"),
	// bea: org B, full access.
	"bea-full": decision("jd:acct:bea", orgBID, wsBID, []domain.SpaceID{wsB, orgB}, []domain.SpaceID{wsB}, []domain.SpaceID{wsB}, "full"),
}

func decision(pid, org, ws string, read, write, approve []domain.SpaceID, level string) map[string]any {
	d := map[string]any{
		"contract": "jumpdrive-authorize/v1", "principal_id": pid,
		"spaces": read, "restricted": false,
		"org_id": org, "workspace_id": ws, "access": level,
	}
	// Absent write/approve sets must fail closed, so leave them out entirely
	// rather than sending [].
	if write != nil {
		d["write_spaces"] = write
	}
	if approve != nil {
		d["approver_spaces"] = approve
	}
	return d
}

// recordingStore captures what the service hands the store for edge writes, so
// the test can assert the writer and actor (which the edge itself does not carry).
type recordingStore struct {
	store.Store
	mu    sync.Mutex
	edges []store.AppendEdgeInput
}

func (r *recordingStore) AppendEdgeFact(ctx context.Context, in store.AppendEdgeInput) (domain.Edge, error) {
	r.mu.Lock()
	r.edges = append(r.edges, in)
	r.mu.Unlock()
	return r.Store.AppendEdgeFact(ctx, in)
}

func newCrossSpaceService(t *testing.T) (*service.Service, *recordingStore) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Bearer string `json:"bearer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		d, ok := crossSpaceDecisions[req.Bearer]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_credential"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(d)
	}))
	t.Cleanup(srv.Close)

	am, err := jumpdrive.New(jumpdrive.Config{BaseURL: srv.URL, SharedSecret: "svc-secret"})
	if err != nil {
		t.Fatalf("jumpdrive.New: %v", err)
	}
	st, err := sqlite.Open(sqlite.Options{
		Path:       filepath.Join(t.TempDir(), "xspace.db"),
		Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86},
	})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rec := &recordingStore{Store: st}
	return service.New(rec, am, nil), rec
}

func mustCreate(t *testing.T, s *service.Service, bearer, name string, space domain.SpaceID, vis domain.Visibility) domain.EntityID {
	t.Helper()
	res, err := s.CreateEntity(ctx, bearer, service.CreateEntityInput{
		Type: "Movie", Props: []byte(`{"name":"` + name + `"}`),
		Space: space, Visibility: vis, Policy: domain.ResolveAuto,
	})
	if err != nil {
		t.Fatalf("%s create %s in %s: %v", bearer, name, space, err)
	}
	return res.Entity.ID
}

func link(from, to domain.EntityID, space domain.SpaceID) service.LinkInput {
	return service.LinkInput{Predicate: "about", From: from, To: to, Space: space, Visibility: domain.VisSpace}
}

func TestCrossSpaceReadsAreFiltered(t *testing.T) {
	s, _ := newCrossSpaceService(t)
	b := mustCreate(t, s, "bea-full", "Bea Private Cut", wsB, domain.VisSpace)

	if _, err := s.GetEntity(ctx, "alice-full", b); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("A GetEntity(B space entity): err=%v, want ErrNotFound", err)
	}
	hits, err := s.Search(ctx, "alice-full", service.SearchQuery{Text: "Bea Private Cut", Limit: 10})
	if err != nil {
		t.Fatalf("A search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("A search sees %d of B's space entities, want 0", len(hits))
	}
	// B still reads its own.
	if _, err := s.GetEntity(ctx, "bea-full", b); err != nil {
		t.Errorf("B GetEntity(own): %v", err)
	}
}

// A public entity is readable across orgs. This is intentional and documented
// (the ingested shared catalogue, e.g. tmdb:, is public); only platform admins
// may author public edges through the control plane.
func TestCrossSpacePublicIsReadableAcrossOrgs(t *testing.T) {
	s, _ := newCrossSpaceService(t)
	pub := mustCreate(t, s, "bea-full", "Catalogue Movie", wsB, domain.VisPublic)
	if _, err := s.GetEntity(ctx, "alice-full", pub); err != nil {
		t.Errorf("A GetEntity(B public entity): %v, want readable", err)
	}
	// ...so A may link to it from her own space.
	a := mustCreate(t, s, "alice-full", "Alice Note", wsA, domain.VisSpace)
	if _, err := s.Link(ctx, "alice-full", link(a, pub, wsA)); err != nil {
		t.Errorf("A link to a public B entity: %v", err)
	}
}

func TestCrossSpaceLinkIsNotAnExistenceOracle(t *testing.T) {
	s, _ := newCrossSpaceService(t)
	a := mustCreate(t, s, "alice-full", "Alice Movie", wsA, domain.VisSpace)
	a2 := mustCreate(t, s, "alice-full", "Alice Other", wsA, domain.VisSpace)
	b := mustCreate(t, s, "bea-full", "Bea Movie", wsB, domain.VisSpace)
	const missing domain.EntityID = "0195f3c2-0000-7000-8000-00000000dead"

	cases := map[string]service.LinkInput{
		"to an unreadable B entity":   link(a, b, wsA),
		"from an unreadable B entity": link(b, a, wsA),
		"to a missing id":             link(a, missing, wsA),
		"from a missing id":           link(missing, a, wsA),
	}
	var msgs []string
	for name, in := range cases {
		_, err := s.Link(ctx, "alice-full", in)
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("Link %s: err=%v, want ErrNotFound", name, err)
			continue
		}
		msgs = append(msgs, err.Error())
		// ProposeLink shares the gate.
		if _, err := s.ProposeLink(ctx, "alice-full", in); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("ProposeLink %s: err=%v, want ErrNotFound", name, err)
		}
	}
	for _, m := range msgs {
		if m != msgs[0] {
			t.Errorf("not-found messages differ (%q vs %q): unreadable and missing must be indistinguishable", m, msgs[0])
		}
	}
	for _, id := range []domain.EntityID{b, missing} {
		if len(msgs) > 0 && strings.Contains(msgs[0], string(id)) {
			t.Errorf("not-found message %q echoes an id", msgs[0])
		}
	}
	// The same pair inside A's own space links fine.
	if _, err := s.Link(ctx, "alice-full", link(a, a2, wsA)); err != nil {
		t.Errorf("A link within A: %v", err)
	}
}

func TestCrossSpaceWritesNeedTheWriteSet(t *testing.T) {
	s, _ := newCrossSpaceService(t)
	a := mustCreate(t, s, "alice-full", "Alice Movie", wsA, domain.VisSpace)
	a2 := mustCreate(t, s, "alice-full", "Alice Other", wsA, domain.VisSpace)

	// Writing into another org's space is forbidden, even with readable endpoints.
	if _, err := s.Link(ctx, "alice-full", link(a, a2, wsB)); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("A Link(space=B): err=%v, want ErrForbidden", err)
	}
	if _, err := s.ProposeLink(ctx, "alice-full", link(a, a2, wsB)); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("A ProposeLink(space=B): err=%v, want ErrForbidden", err)
	}
	if _, err := s.ProposeEntity(ctx, "alice-full", service.CreateEntityInput{
		Type: "Movie", Props: []byte(`{"name":"x"}`), Space: wsB, Visibility: domain.VisSpace, Policy: domain.ResolveAuto,
	}); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("A ProposeEntity(space=B): err=%v, want ErrForbidden", err)
	}

	// A read-only (mcp:read) credential reads A's spaces but writes nothing.
	if _, err := s.GetEntity(ctx, "alice-read", a); err != nil {
		t.Errorf("read-only A GetEntity(own): %v", err)
	}
	if _, err := s.Link(ctx, "alice-read", link(a, a2, wsA)); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("read-only Link: err=%v, want ErrForbidden", err)
	}
	if _, err := s.ProposeLink(ctx, "alice-read", link(a, a2, wsA)); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("read-only ProposeLink: err=%v, want ErrForbidden", err)
	}
	if _, err := s.ProposeEntity(ctx, "alice-read", service.CreateEntityInput{
		Type: "Movie", Props: []byte(`{"name":"y"}`), Space: wsA, Visibility: domain.VisSpace, Policy: domain.ResolveAuto,
	}); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("read-only ProposeEntity: err=%v, want ErrForbidden", err)
	}
	if _, err := s.CreateEntity(ctx, "alice-read", service.CreateEntityInput{
		Type: "Movie", Props: []byte(`{"name":"z"}`), Space: wsA, Visibility: domain.VisSpace, Policy: domain.ResolveAuto,
	}); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("read-only CreateEntity: err=%v, want ErrForbidden", err)
	}

	// Reading the org space does not grant writing it: andy is a workspace
	// writer, not an org admin.
	if _, err := s.Link(ctx, "andy-full", link(a, a2, orgA)); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("andy Link(space=org A): err=%v, want ErrForbidden", err)
	}
	if _, err := s.Link(ctx, "andy-full", link(a, a2, wsA)); err != nil {
		t.Errorf("andy Link(space=ws A): %v", err)
	}
}

func TestCrossSpaceProvenanceIsTheCaller(t *testing.T) {
	s, rec := newCrossSpaceService(t)
	a := mustCreate(t, s, "alice-full", "Alice Movie", wsA, domain.VisSpace)
	a2 := mustCreate(t, s, "alice-full", "Alice Other", wsA, domain.VisSpace)

	edge, err := s.Link(ctx, "alice-full", link(a, a2, wsA))
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if edge.Owner != "jd:acct:alice" || edge.Provenance.Asserter != "jd:acct:alice" {
		t.Errorf("edge owner/asserter = %q/%q, want jd:acct:alice", edge.Owner, edge.Provenance.Asserter)
	}
	wantSrc := "jumpdrive;jd_org=" + orgAID + ";jd_ws=" + wsAID
	if edge.Provenance.Source != wantSrc {
		t.Errorf("edge source = %q, want %q", edge.Provenance.Source, wantSrc)
	}
	got := rec.edges[len(rec.edges)-1]
	if got.Writer != "svc:jd:acct:alice" || got.Actor != "jd:acct:alice" {
		t.Errorf("writer/actor = %q/%q, want svc:jd:acct:alice / jd:acct:alice", got.Writer, got.Actor)
	}
	ent, err := s.GetEntity(ctx, "alice-full", a)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ent.Owner != "jd:acct:alice" || ent.Provenance.Source != wantSrc {
		t.Errorf("entity owner/source = %q/%q, want jd:acct:alice/%q", ent.Owner, ent.Provenance.Source, wantSrc)
	}
}

func TestCrossSpaceDedupeIsPerPrincipalAndSpace(t *testing.T) {
	s, _ := newCrossSpaceService(t)
	a := mustCreate(t, s, "alice-full", "Alice Movie", wsA, domain.VisSpace)
	a2 := mustCreate(t, s, "alice-full", "Alice Other", wsA, domain.VisSpace)

	first, err := s.Link(ctx, "alice-full", link(a, a2, wsA))
	if err != nil {
		t.Fatalf("alice link: %v", err)
	}
	// Two principals' identical links don't dedupe into each other: andy gets
	// his own edge, not ErrDuplicateFact pointing at alice's.
	second, err := s.Link(ctx, "andy-full", link(a, a2, wsA))
	if err != nil {
		t.Fatalf("andy identical link: %v, want a separate edge", err)
	}
	if second.ID == first.ID || second.Owner != "jd:acct:andy" {
		t.Errorf("andy's edge = %+v, want his own edge (alice's is %s)", second, first.ID)
	}
	// The same user linking the same pair in a second space is a new edge too.
	third, err := s.Link(ctx, "alice-full", link(a, a2, orgA))
	if err != nil {
		t.Fatalf("alice same pair in org space: %v", err)
	}
	if third.ID == first.ID {
		t.Error("the same pair in two spaces collapsed into one edge")
	}
	// A true replay (same principal, same space, same pair) still dedupes.
	if _, err := s.Link(ctx, "alice-full", link(a, a2, wsA)); !errors.Is(err, store.ErrDuplicateFact) {
		t.Errorf("replay: err=%v, want ErrDuplicateFact", err)
	}
}

// Concurrent requests from one principal holding different rights (two
// workspace-bound credentials) never see each other's approve or write rights:
// the sets travel on each request's decision, with no per-principal cache.
func TestCrossSpaceConcurrentDecisionsDoNotShareRights(t *testing.T) {
	s, _ := newCrossSpaceService(t)
	a := mustCreate(t, s, "alice-full", "Alice Movie", wsA, domain.VisSpace)
	a2 := mustCreate(t, s, "alice-full", "Alice Other", wsA, domain.VisSpace)

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := s.Link(ctx, "alice-full", service.LinkInput{
				Predicate: "about", From: a, To: a2, Space: wsA, Visibility: domain.VisSpace,
				DedupeKey: "k-" + strings.Repeat("x", i%3),
			}); err != nil && !errors.Is(err, store.ErrDuplicateFact) {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := s.Link(ctx, "alice-read", link(a, a2, wsA)); !errors.Is(err, service.ErrForbidden) {
				errs <- errors.New("read-only alice gained write rights: " + errString(err))
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
