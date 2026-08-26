package starchart_test

import (
	"errors"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/access/starchart"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

func newModel(t *testing.T) *starchart.Model {
	t.Helper()
	m, err := starchart.New(starchart.Config{
		RestrictedDenyTypes: []domain.Type{"Movie"},
		Principals: []starchart.PrincipalConfig{
			{Token: "kate-tok", ID: "kate", Spaces: []domain.SpaceID{"fam"}, ApproverSpaces: []domain.SpaceID{"fam"}},
			{Token: "child-tok", ID: "child", Spaces: []domain.SpaceID{"fam"}, Restricted: true},
			{Token: "bob-tok", ID: "bob"},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func mustAuth(t *testing.T, m *starchart.Model, tok string) access.Decision {
	t.Helper()
	d, err := m.Authenticate(tok)
	if err != nil {
		t.Fatalf("Authenticate(%q): %v", tok, err)
	}
	return d
}

func TestAuthenticate(t *testing.T) {
	m := newModel(t)
	if d := mustAuth(t, m, "kate-tok"); d.Principal.ID != "kate" {
		t.Errorf("id = %q, want kate", d.Principal.ID)
	}
	for _, tok := range []string{"", "unknown-tok"} {
		if _, err := m.Authenticate(tok); !errors.Is(err, starchart.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) err = %v, want ErrUnauthenticated", tok, err)
		}
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := starchart.New(starchart.Config{Principals: []starchart.PrincipalConfig{{Token: "", ID: "x"}}}); err == nil {
		t.Error("empty token should be rejected")
	}
	if _, err := starchart.New(starchart.Config{Principals: []starchart.PrincipalConfig{{Token: "t", ID: ""}}}); err == nil {
		t.Error("empty id should be rejected")
	}
	if _, err := starchart.New(starchart.Config{Principals: []starchart.PrincipalConfig{{Token: "t", ID: "a"}, {Token: "t", ID: "b"}}}); err == nil {
		t.Error("duplicate token should be rejected")
	}
}

func TestCanRead(t *testing.T) {
	m := newModel(t)
	kate := mustAuth(t, m, "kate-tok")
	child := mustAuth(t, m, "child-tok")
	bob := mustAuth(t, m, "bob-tok")

	pubMovie := access.Guarded{Visibility: domain.VisPublic, Type: "Movie"}
	pubNote := access.Guarded{Visibility: domain.VisPublic, Type: "Note"}
	famNote := access.Guarded{Visibility: domain.VisSpace, Space: "fam", Type: "Note"}
	katePriv := access.Guarded{Visibility: domain.VisPrivate, Owner: "kate", Type: "Note"}

	cases := []struct {
		name string
		d    access.Decision
		g    access.Guarded
		want bool
	}{
		{"public visible to anyone", kate, pubMovie, true},
		{"restricted denied a deny-type even when public", child, pubMovie, false},
		{"restricted still reads a non-deny public type", child, pubNote, true},
		{"space member reads space", kate, famNote, true},
		{"non-member denied space", bob, famNote, false},
		{"owner reads own private", kate, katePriv, true},
		{"non-owner denied private", bob, katePriv, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := m.CanRead(tc.d, tc.g); got != tc.want {
				t.Errorf("CanRead = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterFor(t *testing.T) {
	m := newModel(t)
	kf := m.FilterFor(mustAuth(t, m, "kate-tok"))
	if kf.Restricted || kf.DenyTypes != nil || !kf.AllowPublic || kf.Principal != "kate" {
		t.Errorf("unrestricted filter wrong: %+v", kf)
	}
	cf := m.FilterFor(mustAuth(t, m, "child-tok"))
	if !cf.Restricted || len(cf.DenyTypes) != 1 || cf.DenyTypes[0] != "Movie" {
		t.Errorf("restricted filter should carry the deny types: %+v", cf)
	}
}

func TestCanWriteAndApprove(t *testing.T) {
	m := newModel(t)
	kate := mustAuth(t, m, "kate-tok")
	bob := mustAuth(t, m, "bob-tok")

	if !m.CanWrite(kate, "fam") {
		t.Error("kate is a fam member and should be able to write there")
	}
	if m.CanWrite(bob, "fam") {
		t.Error("bob is not a fam member and must not write there")
	}
	if !m.CanApprove(kate, "fam") {
		t.Error("kate is a fam approver")
	}
	if m.CanApprove(kate, "other") {
		t.Error("kate approves only fam, not other")
	}
	if m.CanApprove(bob, "fam") {
		t.Error("bob is not an approver anywhere")
	}
}
