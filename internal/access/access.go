// Package access is the identity seam. It defines who a caller is, what they may
// see (a HARD ACL, pushed into store SQL as a WHERE clause) and how a result is
// presented (SOFT, droppable lenses applied after the ACL). Two implementations
// live under it: starchart (self-contained principals + spaces + lenses) and
// jumpdrive (delegates to Jumpdrive's permission model). The store never knows
// which one it is talking to — it only ever receives a Filter.
//
// This package defines the contract only; the implementations come later.
package access

import (
	"context"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// PrincipalID identifies an authenticated caller.
type PrincipalID = domain.PrincipalID

// Principal is a resolved caller. Restricted marks a principal (e.g. a child
// account) for whom certain types/spaces are a HARD boundary — enforced in SQL,
// never as a droppable lens.
//
// The three space sets are carried ON THE DECISION, never cached per principal:
// with per-request credentials (jumpdrive: one workspace-bound token per call) the
// same principal can hold different rights in two concurrent requests, so the
// answer to CanWrite/CanApprove must come from the request's own decision.
type Principal struct {
	ID PrincipalID
	// Spaces is the READ set: what FilterFor pushes into SQL as the space ACL.
	Spaces []domain.SpaceID
	// WriteSpaces is where the principal may assert (CanWrite). Empty means
	// read-only (fail closed).
	WriteSpaces []domain.SpaceID
	// ApproverSpaces is where the principal may decide governed writes
	// (CanApprove). Empty means no approve rights (fail closed).
	ApproverSpaces []domain.SpaceID
	Restricted     bool
}

// Lens is a SOFT, named query-time filter. It can hide (dampen) results but never
// reveal anything the hard ACL forbids; toggling one on or off is always safe.
// Child SAFETY is never a lens — that is a restricted Principal (above).
type Lens struct {
	ID             string
	HideTypes      []domain.Type
	HidePredicates []domain.Predicate
}

// Decision is the resolved capability of one principal for one request: who they
// are plus the lenses currently active.
type Decision struct {
	Principal Principal
	Lenses    []Lens
	// Source traces the decision back to the request that produced it (for
	// jumpdrive: the control-plane org, workspace and credential jti). Every
	// write made under the decision records it as Provenance.Source, so an edge
	// can be matched to the audited call. Empty when the identity backend has
	// nothing to add (starchart).
	Source string
}

// ContainsSpace reports whether s is in spaces. It is the membership test every
// model uses for the read, write and approve sets.
func ContainsSpace(spaces []domain.SpaceID, s domain.SpaceID) bool {
	for _, x := range spaces {
		if x == s {
			return true
		}
	}
	return false
}

// Guarded is the SQL-filterable subset of any node or edge — deliberately narrow
// so the same predicate expresses in Go (CanRead) and in SQL (AccessFilter).
type Guarded struct {
	Space      domain.SpaceID
	Owner      domain.PrincipalID
	Visibility domain.Visibility
	Type       domain.Type      // for a restricted principal's type gating
	Predicate  domain.Predicate // edges only
}

// Filter is the value object the access model builds and the store consumes as a
// WHERE clause. It is what makes access default-deny as SQL SHAPE (like the
// broker's eligibility JOIN) rather than a fragile post-filter — a row the
// principal may not see is never returned, even mid-traversal.
type Filter struct {
	Principal   domain.PrincipalID
	Spaces      []domain.SpaceID
	Restricted  bool
	DenyTypes   []domain.Type // a restricted principal's forbidden @types
	AllowPublic bool
}

// Model resolves identity and answers the hard-ACL questions. Reads pass the
// derived Filter into the store; write tools consult CanWrite/CanApprove before
// the store. Deny-by-default: no token → no principal → nothing visible.
//
// Authenticate takes the caller's request context, so a backend that calls out
// (jumpdrive's authorizer, its JWKS) is cancelled with the request.
type Model interface {
	Authenticate(ctx context.Context, bearer string) (Decision, error)
	FilterFor(d Decision) Filter
	CanRead(d Decision, g Guarded) bool
	CanWrite(d Decision, space domain.SpaceID) bool
	CanApprove(d Decision, space domain.SpaceID) bool
}

type requestIDKey struct{}

// WithRequestID returns ctx carrying the inbound X-Request-Id, so a backend that
// calls out can propagate it.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request id WithRequestID stored, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
