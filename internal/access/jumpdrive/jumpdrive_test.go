package jumpdrive_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/access/jumpdrive"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

func TestDelegateHappyPath(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Bearer string `json:"bearer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Bearer != "good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"principal_id":    "alice",
			"spaces":          []string{"fam", "proj"},
			"approver_spaces": []string{"fam"},
			"restricted":      true,
		})
	}))
	defer srv.Close()

	m, err := jumpdrive.New(jumpdrive.Config{
		BaseURL: srv.URL, SharedSecret: "s3cr3t", RestrictedDenyTypes: []domain.Type{"Movie"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	d, err := m.Authenticate("good")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if d.Principal.ID != "alice" || len(d.Principal.Spaces) != 2 || !d.Principal.Restricted {
		t.Errorf("mapped principal wrong: %+v", d.Principal)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Errorf("shared secret not sent: Authorization=%q", gotAuth)
	}

	// The delegate applies the SAME hard ACL as starchart.
	if m.CanRead(d, access.Guarded{Visibility: domain.VisPublic, Type: "Movie"}) {
		t.Error("restricted principal must not read a deny-type Movie")
	}
	if !m.CanRead(d, access.Guarded{Visibility: domain.VisPublic, Type: "Note"}) {
		t.Error("restricted principal should still read a non-deny public type")
	}
	// approver spaces come from the authorize response cache.
	if !m.CanApprove(d, "fam") {
		t.Error("alice approves fam")
	}
	if m.CanApprove(d, "proj") {
		t.Error("alice is a member of proj but not an approver there")
	}
	f := m.FilterFor(d)
	if !f.Restricted || len(f.DenyTypes) != 1 {
		t.Errorf("restricted filter wrong: %+v", f)
	}
}

func TestDelegateRejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	m, err := jumpdrive.New(jumpdrive.Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Authenticate("bad"); !errors.Is(err, jumpdrive.ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

func TestNewRequiresBaseURL(t *testing.T) {
	if _, err := jumpdrive.New(jumpdrive.Config{}); err == nil {
		t.Error("empty BaseURL should be rejected")
	}
}
