package service_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/access/starchart"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/service"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
	"github.com/rarebit-one/jumpdrive-index/internal/store/sqlite"
)

var ctx = context.Background()

// kate: writes + approves in "fam". bob: authenticated but no write/approve there.
func newService(t *testing.T) *service.Service {
	t.Helper()
	st, err := sqlite.Open(sqlite.Options{
		Path:       filepath.Join(t.TempDir(), "svc.db"),
		Thresholds: domain.Thresholds{AutoMerge: 0.94, Review: 0.86},
	})
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	am, err := starchart.New(starchart.Config{Principals: []starchart.PrincipalConfig{
		{Token: "kate-tok", ID: "kate", Spaces: []domain.SpaceID{"fam"}, ApproverSpaces: []domain.SpaceID{"fam"}},
		{Token: "bob-tok", ID: "bob"},
	}})
	if err != nil {
		t.Fatalf("starchart.New: %v", err)
	}
	return service.New(st, am)
}

func movie(name string, ext ...domain.ExternalID) service.CreateEntityInput {
	return service.CreateEntityInput{
		Type: "Movie", Props: []byte(`{"name":"` + name + `"}`),
		Space: "fam", Visibility: "space", ExternalIDs: ext, Policy: domain.ResolveAuto,
	}
}

func TestSearch(t *testing.T) {
	s := newService(t)
	if _, err := s.CreateEntity(ctx, "kate-tok", service.CreateEntityInput{
		Type: "Movie", Props: []byte(`{"name":"Alien","abstract":"a chestburster erupts"}`),
		Space: "fam", Visibility: "space", Policy: domain.ResolveAuto,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// kate (member of fam) finds the space-scoped entity.
	hits, err := s.Search(ctx, "kate-tok", service.SearchQuery{Text: "chestburster", Limit: 10})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("kate got %d hits, want 1", len(hits))
	}
	// bob (not in fam) does not — search is access-filtered.
	if bob, _ := s.Search(ctx, "bob-tok", service.SearchQuery{Text: "chestburster", Limit: 10}); len(bob) != 0 {
		t.Errorf("bob got %d hits, want 0", len(bob))
	}
	if _, err := s.Search(ctx, "nope", service.SearchQuery{Text: "x"}); !errors.Is(err, service.ErrUnauthenticated) {
		t.Errorf("unauthenticated search: err=%v, want ErrUnauthenticated", err)
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	s := newService(t)
	if _, err := s.CreateEntity(ctx, "nope", movie("X")); !errors.Is(err, service.ErrUnauthenticated) {
		t.Errorf("bad token: err=%v, want ErrUnauthenticated", err)
	}
	if _, err := s.GetEntity(ctx, "", "id"); !errors.Is(err, service.ErrUnauthenticated) {
		t.Errorf("empty token read: err=%v, want ErrUnauthenticated", err)
	}
}

func TestCreateRequiresWriteAccess(t *testing.T) {
	s := newService(t)
	// bob is authenticated but has no write access to "fam".
	if _, err := s.CreateEntity(ctx, "bob-tok", movie("X")); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("bob write to fam: err=%v, want ErrForbidden", err)
	}
	// kate can.
	if _, err := s.CreateEntity(ctx, "kate-tok", movie("Alien")); err != nil {
		t.Errorf("kate write to fam: %v", err)
	}
}

func TestReadIsAccessFiltered(t *testing.T) {
	s := newService(t)
	res, err := s.CreateEntity(ctx, "kate-tok", movie("Alien"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// kate (member of fam) sees the space-scoped entity...
	if _, err := s.GetEntity(ctx, "kate-tok", res.Entity.ID); err != nil {
		t.Errorf("kate should read her space entity: %v", err)
	}
	// ...bob (not in fam) does not.
	if _, err := s.GetEntity(ctx, "bob-tok", res.Entity.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bob read of a fam-space entity: err=%v, want ErrNotFound", err)
	}
	// Owner is stamped from the caller, not the input.
	got, _ := s.GetEntity(ctx, "kate-tok", res.Entity.ID)
	if got.Owner != "kate" {
		t.Errorf("owner = %q, want kate (from the authenticated caller)", got.Owner)
	}
}

func TestLinkAuthorized(t *testing.T) {
	s := newService(t)
	a, _ := s.CreateEntity(ctx, "kate-tok", movie("Alien"))
	b, _ := s.CreateEntity(ctx, "kate-tok", service.CreateEntityInput{
		Type: "VideoObject", Props: []byte(`{"name":"analysis"}`), Space: "fam", Visibility: "space", Policy: domain.ResolveAuto,
	})
	edge, err := s.Link(ctx, "kate-tok", service.LinkInput{
		Predicate: "about", From: b.Entity.ID, To: a.Entity.ID, Space: "fam", Visibility: "space",
	})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if edge.ID == "" || edge.Owner != "kate" {
		t.Errorf("edge = %+v, want minted id + owner kate", edge)
	}
	// bob cannot link into fam.
	if _, err := s.Link(ctx, "bob-tok", service.LinkInput{Predicate: "about", From: b.Entity.ID, To: a.Entity.ID, Space: "fam", Visibility: "space"}); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("bob link: err=%v, want ErrForbidden", err)
	}
}

func TestGovernedProposeThenApprove(t *testing.T) {
	s := newService(t)
	tmdb := domain.ExternalID{Scheme: "tmdb", Value: "603"}
	// bob may PROPOSE (open to any authenticated caller) even without write access.
	pid, err := s.ProposeEntity(ctx, "bob-tok", movie("Proposed", tmdb))
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	// Not projected until approved.
	if hits, _ := s.ResolveByExternalID(ctx, "kate-tok", []string{tmdb.Key()}); len(hits) != 0 {
		t.Errorf("held proposal must not be projected; found %d", len(hits))
	}
	// bob cannot approve (not an approver of fam).
	if _, err := s.DecideProposal(ctx, "bob-tok", pid, true); !errors.Is(err, service.ErrForbidden) {
		t.Errorf("bob approve: err=%v, want ErrForbidden", err)
	}
	// kate (approver of fam) can.
	if _, err := s.DecideProposal(ctx, "kate-tok", pid, true); err != nil {
		t.Fatalf("kate approve: %v", err)
	}
	if hits, _ := s.ResolveByExternalID(ctx, "kate-tok", []string{tmdb.Key()}); len(hits) != 1 {
		t.Errorf("after approval the entity should be projected; found %d", len(hits))
	}
}
