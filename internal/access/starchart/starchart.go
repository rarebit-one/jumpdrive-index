// Package starchart is the self-contained access model for the standalone
// (homelab) Starchart build: principals, their spaces, and their approver rights
// are held locally, with no dependency on an external authorizer.
//
// The ACL rules here MUST agree byte-for-byte with the SQLite adapter's
// accessWhere clause (public / space / private + a restricted @type gate) — that
// agreement is the whole point of the hard ACL: the same decision in Go
// (CanRead) and in SQL (the Filter → WHERE clause).
//
// NOTE: the principal registry is in-memory, seeded from Config at boot.
// Persisting principals/spaces to the SQLite database is a later milestone.
package starchart

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// ErrUnauthenticated is returned for an empty or unknown bearer token
// (deny-by-default: no token → no principal → nothing visible).
var ErrUnauthenticated = errors.New("starchart: unknown or empty bearer token")

// PrincipalConfig declares one principal and the raw token that authenticates it.
type PrincipalConfig struct {
	Token          string
	ID             domain.PrincipalID
	Spaces         []domain.SpaceID
	Restricted     bool
	ApproverSpaces []domain.SpaceID
}

// Config seeds the model. RestrictedDenyTypes are the @types a restricted
// principal (e.g. a child account) may never read — a HARD boundary, not a lens.
type Config struct {
	Principals          []PrincipalConfig
	RestrictedDenyTypes []domain.Type
}

type principal struct {
	id             domain.PrincipalID
	spaces         []domain.SpaceID
	restricted     bool
	approverSpaces map[domain.SpaceID]bool
}

// Model is the self-contained access.Model.
type Model struct {
	byToken   map[string]principal             // key: sha256 hex of the raw token
	byID      map[domain.PrincipalID]principal // for approver lookups (Decision carries no approver info)
	denyTypes []domain.Type
}

var _ access.Model = (*Model)(nil)

// New builds a Model from cfg, hashing each token into the registry. It rejects
// empty tokens/ids and duplicate tokens.
func New(cfg Config) (*Model, error) {
	m := &Model{
		byToken:   make(map[string]principal, len(cfg.Principals)),
		byID:      make(map[domain.PrincipalID]principal, len(cfg.Principals)),
		denyTypes: append([]domain.Type(nil), cfg.RestrictedDenyTypes...),
	}
	for i, pc := range cfg.Principals {
		if pc.Token == "" {
			return nil, fmt.Errorf("starchart: principal %d has an empty token", i)
		}
		if pc.ID == "" {
			return nil, fmt.Errorf("starchart: principal %d has an empty id", i)
		}
		dig := tokenDigest(pc.Token)
		if _, dup := m.byToken[dig]; dup {
			return nil, fmt.Errorf("starchart: duplicate token for principal %q", pc.ID)
		}
		ap := make(map[domain.SpaceID]bool, len(pc.ApproverSpaces))
		for _, s := range pc.ApproverSpaces {
			ap[s] = true
		}
		p := principal{
			id:             pc.ID,
			spaces:         append([]domain.SpaceID(nil), pc.Spaces...),
			restricted:     pc.Restricted,
			approverSpaces: ap,
		}
		m.byToken[dig] = p
		m.byID[pc.ID] = p
	}
	return m, nil
}

func tokenDigest(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// Authenticate resolves a bearer token to a Decision, or ErrUnauthenticated.
func (m *Model) Authenticate(bearer string) (access.Decision, error) {
	if bearer == "" {
		return access.Decision{}, ErrUnauthenticated
	}
	p, ok := m.byToken[tokenDigest(bearer)]
	if !ok {
		return access.Decision{}, ErrUnauthenticated
	}
	return access.Decision{Principal: access.Principal{ID: p.id, Spaces: p.spaces, Restricted: p.restricted}}, nil
}

// FilterFor builds the SQL-pushdown Filter for a decision.
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

// CanRead is the hard ACL, mirroring the SQLite accessWhere clause exactly.
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
		return false // unknown visibility → deny
	}
}

// CanWrite reports whether the principal may write into space (space membership).
func (m *Model) CanWrite(d access.Decision, space domain.SpaceID) bool {
	return containsSpace(d.Principal.Spaces, space)
}

// CanApprove reports whether the principal may approve governed writes in space.
// It re-resolves by id because a Decision carries no approver information.
func (m *Model) CanApprove(d access.Decision, space domain.SpaceID) bool {
	p, ok := m.byID[d.Principal.ID]
	if !ok {
		return false
	}
	return p.approverSpaces[space]
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
