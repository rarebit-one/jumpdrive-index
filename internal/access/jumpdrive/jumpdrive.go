// Package jumpdrive is the embedded access model: it delegates identity
// resolution to Jumpdrive's authorizer over HTTP, then answers the hard-ACL
// questions locally with the SAME rules as the starchart model (public / space /
// private + a restricted @type gate). Only WHERE the principal comes from
// differs — here it is Jumpdrive, not a local registry.
//
// The client is shaped like jumpdrive-web's RunnerClient: base URL from config,
// an opt-in shared-secret bearer, explicit timeouts, an X-Request-Id, a typed
// error, and a narrow httpDoer seam for testing.
//
// PROVISIONAL: the Jumpdrive `/authorize` request/response contract below is not
// yet ratified against jumpdrive-web; treat it as a proposal.
package jumpdrive

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// ErrUnauthenticated is returned when the authorizer rejects a bearer token.
var ErrUnauthenticated = errors.New("jumpdrive: authorizer rejected the bearer token")

// ErrAuthorizer is returned when the authorizer call itself fails (network,
// timeout, unexpected status).
var ErrAuthorizer = errors.New("jumpdrive: authorizer request failed")

// httpDoer is the minimal HTTP seam (satisfied by *http.Client), so tests can
// drive the model against an httptest.Server or a fake.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Config configures the delegate.
type Config struct {
	BaseURL             string
	SharedSecret        string // opt-in; sent as a bearer on the authorizer call when set
	RestrictedDenyTypes []domain.Type
	Timeout             time.Duration // default 5s
	HTTPClient          httpDoer      // default *http.Client with Timeout
}

// Model is the delegating access.Model.
type Model struct {
	baseURL   string
	secret    string
	denyTypes []domain.Type
	http      httpDoer
}

var _ access.Model = (*Model)(nil)

// New builds a delegate. BaseURL is required.
func New(cfg Config) (*Model, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("jumpdrive: empty BaseURL")
	}
	h := cfg.HTTPClient
	if h == nil {
		to := cfg.Timeout
		if to == 0 {
			to = 5 * time.Second
		}
		h = &http.Client{Timeout: to}
	}
	return &Model{
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		secret:    cfg.SharedSecret,
		denyTypes: append([]domain.Type(nil), cfg.RestrictedDenyTypes...),
		http:      h,
	}, nil
}

// authorizeRequest / authorizeResponse are the provisional wire contract.
type authorizeRequest struct {
	Bearer string `json:"bearer"`
}

// Spaces is the READ set. WriteSpaces and ApproverSpaces are separate sets; a
// response that omits either grants nothing there (fail closed).
type authorizeResponse struct {
	PrincipalID    string   `json:"principal_id"`
	Spaces         []string `json:"spaces"`
	WriteSpaces    []string `json:"write_spaces"`
	ApproverSpaces []string `json:"approver_spaces"`
	Restricted     bool     `json:"restricted"`
	OrgID          string   `json:"org_id"`
	WorkspaceID    string   `json:"workspace_id"`
}

// Authenticate delegates to the Jumpdrive authorizer and maps its response to a
// Decision. The read, write and approve sets all travel on the returned
// decision; nothing is cached per principal, so two concurrent requests from one
// user in different workspaces never see each other's rights.
func (m *Model) Authenticate(bearer string) (access.Decision, error) {
	body, err := json.Marshal(authorizeRequest{Bearer: bearer})
	if err != nil {
		return access.Decision{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/authorize", bytes.NewReader(body))
	if err != nil {
		return access.Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", requestID())
	if m.secret != "" {
		req.Header.Set("Authorization", "Bearer "+m.secret)
	}

	resp, err := m.http.Do(req)
	if err != nil {
		return access.Decision{}, fmt.Errorf("%w: %w", ErrAuthorizer, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return access.Decision{}, ErrUnauthenticated
	case resp.StatusCode != http.StatusOK:
		return access.Decision{}, fmt.Errorf("%w: status %d", ErrAuthorizer, resp.StatusCode)
	}

	var ar authorizeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ar); err != nil {
		return access.Decision{}, fmt.Errorf("%w: decode: %w", ErrAuthorizer, err)
	}
	if ar.PrincipalID == "" {
		return access.Decision{}, ErrUnauthenticated
	}

	return access.Decision{
		Principal: access.Principal{
			ID:             domain.PrincipalID(ar.PrincipalID),
			Spaces:         toSpaces(ar.Spaces),
			WriteSpaces:    toSpaces(ar.WriteSpaces),
			ApproverSpaces: toSpaces(ar.ApproverSpaces),
			Restricted:     ar.Restricted,
		},
		Source: ProvenanceSource(ar.OrgID, ar.WorkspaceID, "", ""),
	}, nil
}

// ProvenanceSource renders the request trace a jumpdrive decision stamps on
// every write as Provenance.Source: the control-plane org and workspace the
// credential was bound to, its jti and the OAuth client, under their claim
// names. Empty parts are left out, so the format is
// "jumpdrive;jd_org=<uuid>;jd_ws=<uuid>;jti=<uuid>;jd_client=<id>".
func ProvenanceSource(org, ws, jti, client string) string {
	var b strings.Builder
	b.WriteString("jumpdrive")
	for _, kv := range [][2]string{{"jd_org", org}, {"jd_ws", ws}, {"jti", jti}, {"jd_client", client}} {
		if kv[1] != "" {
			b.WriteString(";" + kv[0] + "=" + kv[1])
		}
	}
	return b.String()
}

func toSpaces(in []string) []domain.SpaceID {
	out := make([]domain.SpaceID, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, domain.SpaceID(s))
		}
	}
	return out
}

// FilterFor builds the SQL-pushdown Filter (same shape as starchart).
func (m *Model) FilterFor(d access.Decision) access.Filter {
	f := access.Filter{
		Principal:   d.Principal.ID,
		Spaces:      d.Principal.Spaces,
		Restricted:  d.Principal.Restricted,
		AllowPublic: true,
	}
	if d.Principal.Restricted {
		f.DenyTypes = m.denyTypes
	}
	return f
}

// CanRead is the hard ACL, identical to starchart's and to the SQLite
// accessWhere clause.
func (m *Model) CanRead(d access.Decision, g access.Guarded) bool {
	if d.Principal.Restricted && containsType(m.denyTypes, g.Type) {
		return false
	}
	switch g.Visibility {
	case domain.VisPublic:
		return true
	case domain.VisSpace:
		return access.ContainsSpace(d.Principal.Spaces, g.Space)
	case domain.VisPrivate:
		return g.Owner == d.Principal.ID
	default:
		return false
	}
}

// CanWrite reports whether the decision grants writes into space. The write set
// is separate from the read set: a read-only (mcp:read) credential carries an
// empty one.
func (m *Model) CanWrite(d access.Decision, space domain.SpaceID) bool {
	return access.ContainsSpace(d.Principal.WriteSpaces, space)
}

// CanApprove reports whether the decision grants approving in space, from the
// approver set carried on the decision itself.
func (m *Model) CanApprove(d access.Decision, space domain.SpaceID) bool {
	return access.ContainsSpace(d.Principal.ApproverSpaces, space)
}

func requestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "jdx-req"
	}
	return hex.EncodeToString(b[:])
}

func containsType(types []domain.Type, t domain.Type) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}
