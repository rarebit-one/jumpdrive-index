// Package access is the identity seam. It defines who a caller is, what they may
// see (a HARD ACL, pushed into store SQL as a WHERE clause) and how a result is
// presented (SOFT, droppable lenses applied after the ACL). Two implementations
// live under it: starchart (self-contained principals + spaces + lenses) and
// jumpdrive (delegates to Jumpdrive's permission model). The store never knows
// which one it is talking to — it only ever receives a Filter.
//
// This package defines the contract only; the implementations come later.
package access

import "github.com/rarebit-one/jumpdrive-index/internal/domain"

// PrincipalID identifies an authenticated caller.
type PrincipalID = domain.PrincipalID

// Principal is a resolved caller. Restricted marks a principal (e.g. a child
// account) for whom certain types/spaces are a HARD boundary — enforced in SQL,
// never as a droppable lens.
type Principal struct {
	ID         PrincipalID
	Spaces     []domain.SpaceID
	Restricted bool
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
type Model interface {
	Authenticate(bearer string) (Decision, error)
	FilterFor(d Decision) Filter
	CanRead(d Decision, g Guarded) bool
	CanWrite(d Decision, space domain.SpaceID) bool
	CanApprove(d Decision, space domain.SpaceID) bool
}
