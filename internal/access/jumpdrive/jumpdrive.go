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
	"sync"
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

	mu       sync.RWMutex
	approver map[domain.PrincipalID]map[domain.SpaceID]bool // cached from authorize responses
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
		approver:  make(map[domain.PrincipalID]map[domain.SpaceID]bool),
	}, nil
}

// authorizeRequest / authorizeResponse are the provisional wire contract.
type authorizeRequest struct {
	Bearer string `json:"bearer"`
}

type authorizeResponse struct {
	PrincipalID    string   `json:"principal_id"`
	Spaces         []string `json:"spaces"`
	ApproverSpaces []string `json:"approver_spaces"`
	Restricted     bool     `json:"restricted"`
}

// Authenticate delegates to the Jumpdrive authorizer and maps its response to a
// Decision. It also caches the principal's approver spaces for CanApprove.
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

	spaces := make([]domain.SpaceID, len(ar.Spaces))
	for i, s := range ar.Spaces {
		spaces[i] = domain.SpaceID(s)
	}
	pid := domain.PrincipalID(ar.PrincipalID)

	ap := make(map[domain.SpaceID]bool, len(ar.ApproverSpaces))
	for _, s := range ar.ApproverSpaces {
		ap[domain.SpaceID(s)] = true
	}
	m.mu.Lock()
	m.approver[pid] = ap
	m.mu.Unlock()

	return access.Decision{Principal: access.Principal{ID: pid, Spaces: spaces, Restricted: ar.Restricted}}, nil
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
		return containsSpace(d.Principal.Spaces, g.Space)
	case domain.VisPrivate:
		return g.Owner == d.Principal.ID
	default:
		return false
	}
}

// CanWrite reports whether the principal may write into space (space membership).
func (m *Model) CanWrite(d access.Decision, space domain.SpaceID) bool {
	return containsSpace(d.Principal.Spaces, space)
}

// CanApprove consults approver spaces cached from the principal's last authorize
// response; an unseen principal is denied (deny-by-default).
func (m *Model) CanApprove(d access.Decision, space domain.SpaceID) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.approver[d.Principal.ID][space]
}

func requestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "jdx-req"
	}
	return hex.EncodeToString(b[:])
}

func containsSpace(spaces []domain.SpaceID, s domain.SpaceID) bool {
	for _, x := range spaces {
		if x == s {
			return true
		}
	}
	return false
}

func containsType(types []domain.Type, t domain.Type) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}
