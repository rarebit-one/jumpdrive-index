package jumpdrive_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	// approver spaces come from the decision itself.
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
	// The response carried no write_spaces: absent means [] (fail closed), even
	// for spaces the principal reads.
	if m.CanWrite(d, "fam") || m.CanWrite(d, "proj") {
		t.Error("absent write_spaces must grant no writes")
	}
}

// One user, two workspace-bound credentials: each decision carries only its own
// write and approve sets. The old per-principal approver cache let the latest
// Authenticate overwrite the earlier request's rights.
func TestDecisionsForOnePrincipalDoNotShareRights(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Bearer string `json:"bearer"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		ws := map[string]string{"tok-ws1": "jd:ws:1", "tok-ws2": "jd:ws:2"}[req.Bearer]
		if ws == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"principal_id":    "jd:acct:alice",
			"spaces":          []string{ws, "jd:org:a"},
			"write_spaces":    []string{ws},
			"approver_spaces": []string{ws},
			"org_id":          "a",
			"workspace_id":    strings.TrimPrefix(ws, "jd:ws:"),
		})
	}))
	defer srv.Close()
	m, err := jumpdrive.New(jumpdrive.Config{BaseURL: srv.URL, SharedSecret: "s"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, own, other := "tok-ws1", domain.SpaceID("jd:ws:1"), domain.SpaceID("jd:ws:2")
			if i%2 == 1 {
				tok, own, other = "tok-ws2", other, own
			}
			d, err := m.Authenticate(tok)
			if err != nil {
				t.Errorf("Authenticate(%s): %v", tok, err)
				return
			}
			if !m.CanApprove(d, own) || !m.CanWrite(d, own) {
				t.Errorf("%s lost its own rights in %s", tok, own)
			}
			if m.CanApprove(d, other) || m.CanWrite(d, other) {
				t.Errorf("%s gained rights in %s from a concurrent decision", tok, other)
			}
			if want := "jumpdrive;jd_org=a;jd_ws=" + strings.TrimPrefix(string(own), "jd:ws:"); d.Source != want {
				t.Errorf("source = %q, want %q", d.Source, want)
			}
		}()
	}
	wg.Wait()
}

func TestProvenanceSourceOmitsEmptyParts(t *testing.T) {
	if got := jumpdrive.ProvenanceSource("o", "w", "j", "c"); got != "jumpdrive;jd_org=o;jd_ws=w;jti=j;jd_client=c" {
		t.Errorf("full source = %q", got)
	}
	if got := jumpdrive.ProvenanceSource("o", "", "j", ""); got != "jumpdrive;jd_org=o;jti=j" {
		t.Errorf("partial source = %q", got)
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
