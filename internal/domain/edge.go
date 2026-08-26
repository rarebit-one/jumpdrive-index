package domain

import "time"

// EdgeID is an internal, stable identifier for an edge (a UUIDv7 string minted
// by the store).
type EdgeID string

// Predicate is a schema.org property naming a directed relationship, e.g.
// "about", "actor", "director", "subjectOf", "hasPart". Like Type, the accepted
// set is an allow-list (see schema.go).
type Predicate string

// Edge is a FIRST-CLASS relationship between two entities. It is not a mere
// foreign key: it carries its own provenance and — load-bearing — its own
// Visibility, independent of the nodes it connects. "X is treating Y" can be
// more sensitive than either X or Y existing, so an edge can be strictly more
// private than both of its endpoints. Traversal (store.Neighbors) must therefore
// filter edges AND nodes at every hop, so a hidden node can never bridge two
// visible ones.
type Edge struct {
	ID         EdgeID
	Predicate  Predicate
	From       EntityID
	To         EntityID
	Props      []byte // optional JSON-LD qualifiers (e.g. a Clip's startOffset/endOffset)
	Provenance Provenance
	Space      SpaceID
	Owner      PrincipalID
	Visibility Visibility // NOT derived from endpoints
	CreatedAt  time.Time
	Deleted    bool
}
