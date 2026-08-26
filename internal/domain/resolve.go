package domain

import (
	"errors"
	"fmt"
)

// resolve.go is the pure heart of resolve-before-create — the analogue of
// jumpdrive-broker's placement.Decide. Given a candidate entity and what the
// store found when it looked for existing matches, Resolve returns WHAT to do
// (insert / attach / merge / insert-and-flag) and WHY. It performs no I/O: the
// store does the lookups (external-id exact match, vector KNN), feeds them in,
// and then executes the returned action inside the write transaction. This split
// is what lets the dangerous logic be exhaustively unit-tested, and what a
// SQL-vs-Go drift test pins the adapters against.

// ResolvePolicy is the caller's dedup intent.
type ResolvePolicy string

const (
	// ResolveAuto: exact external-id match first, then vector similarity.
	ResolveAuto ResolvePolicy = "auto"
	// ResolveExternalOnly: exact external-id match only; never merge on a vector.
	ResolveExternalOnly ResolvePolicy = "external_only"
	// ResolveForceNew: always insert a new node (an explicit "this is distinct").
	ResolveForceNew ResolvePolicy = "force_new"
)

// Valid reports whether p is a known policy.
func (p ResolvePolicy) Valid() bool {
	switch p {
	case ResolveAuto, ResolveExternalOnly, ResolveForceNew:
		return true
	default:
		return false
	}
}

// Thresholds are the two vector-similarity bands. A false merge corrupts two real
// things and is dear to undo, so auto-merge requires strong evidence (a high
// floor) and the middle band only FLAGS a possible duplicate rather than merging.
type Thresholds struct {
	AutoMerge float64 // >= this cosine similarity: attach to the match
	Review    float64 // >= this (but < AutoMerge): insert new + an inferred sameAs? edge
}

// Validate enforces AutoMerge > Review and both in (0,1]. Config supplies these
// and is validated at boot (default-deny), never silently defaulted.
func (t Thresholds) Validate() error {
	if !(t.Review > 0 && t.Review <= 1) {
		return fmt.Errorf("resolve: Review threshold %v out of range (0,1]", t.Review)
	}
	if !(t.AutoMerge > 0 && t.AutoMerge <= 1) {
		return fmt.Errorf("resolve: AutoMerge threshold %v out of range (0,1]", t.AutoMerge)
	}
	if !(t.AutoMerge > t.Review) {
		return fmt.Errorf("resolve: AutoMerge %v must exceed Review %v", t.AutoMerge, t.Review)
	}
	return nil
}

// ScoredMatch is one vector neighbour: an existing entity and its similarity to
// the candidate.
type ScoredMatch struct {
	ID    EntityID
	Score float64
}

// ResolveInputs is what the store found. ExternalIDHits are the DISTINCT existing
// entities that already carry one of the candidate's external ids, passed
// oldest-first (so a merge keeps the oldest). VectorNeighbors are same-type
// candidates by embedding similarity (order irrelevant — Resolve takes the max).
type ResolveInputs struct {
	ExternalIDHits  []EntityID
	VectorNeighbors []ScoredMatch
}

// ResolveAction is the verdict.
type ResolveAction string

const (
	ActionInsertNew     ResolveAction = "insert_new"     // no match — create
	ActionAttach        ResolveAction = "attach"         // fold candidate into Target (union ext ids/props)
	ActionMerge         ResolveAction = "merge"          // >1 existing share an ext id → merge them, then attach
	ActionInsertFlagged ResolveAction = "insert_flagged" // create, plus an inferred sameAs? edge to FlagTo
)

// MatchKind records HOW a match was made (for provenance and for explaining the
// write back to the caller/agent).
type MatchKind string

const (
	MatchNone     MatchKind = "none"
	MatchExternal MatchKind = "exact_external"
	MatchVector   MatchKind = "vector"
)

// ResolveDecision is the full, explainable verdict. Reason is a stable,
// human-readable string asserted by the drift test alongside the action.
type ResolveDecision struct {
	Action       ResolveAction
	MatchKind    MatchKind
	Target       EntityID   // for Attach / Merge (the surviving/kept node)
	MergeTargets []EntityID // for Merge: all colliding nodes, oldest-first (keep = [0])
	FlagTo       EntityID   // for InsertFlagged: the possible-duplicate to link with sameAs?
	FlagScore    float64    // for InsertFlagged: the similarity score
	Reason       string
}

// ErrInvalidPolicy is returned for an unknown ResolvePolicy (default-deny).
var ErrInvalidPolicy = errors.New("resolve: invalid policy")

// Resolve is the pure decision. It never reads or writes anything.
//
// Precedence:
//  1. Exact external-id match is authoritative and always wins (any policy but
//     ForceNew). One hit → attach; several → merge them, then attach.
//  2. ForceNew / ExternalOnly stop before the vector stage.
//  3. Vector two-band: >= AutoMerge attaches; >= Review inserts-and-flags; else
//     inserts new.
//
// Callers must Validate() the thresholds and check policy.Valid() beforehand;
// Resolve still guards policy so a bad value can never fall through to a silent
// default.
func Resolve(candidate Entity, in ResolveInputs, policy ResolvePolicy, th Thresholds) (ResolveDecision, error) {
	if !policy.Valid() {
		return ResolveDecision{}, ErrInvalidPolicy
	}

	// 1. Exact external-id match — authoritative (skipped only for ForceNew).
	if policy != ResolveForceNew && len(in.ExternalIDHits) > 0 {
		distinct := dedupeIDs(in.ExternalIDHits)
		if len(distinct) == 1 {
			return ResolveDecision{
				Action:    ActionAttach,
				MatchKind: MatchExternal,
				Target:    distinct[0],
				Reason:    "exact external-id match to a single existing entity",
			}, nil
		}
		// >1 existing entity shares an external id with the candidate: those
		// existing nodes are themselves duplicates and must be merged.
		return ResolveDecision{
			Action:       ActionMerge,
			MatchKind:    MatchExternal,
			Target:       distinct[0], // oldest survives
			MergeTargets: distinct,
			Reason:       fmt.Sprintf("external-id collision across %d existing entities; merge then attach", len(distinct)),
		}, nil
	}

	// 2. Policy gates that stop before the vector stage.
	switch policy {
	case ResolveForceNew:
		return ResolveDecision{Action: ActionInsertNew, MatchKind: MatchNone, Reason: "force_new: insert without resolving"}, nil
	case ResolveExternalOnly:
		return ResolveDecision{Action: ActionInsertNew, MatchKind: MatchNone, Reason: "external_only: no external-id match, insert new"}, nil
	}

	// 3. Vector two-band (ResolveAuto, no external match).
	if top, ok := maxScore(in.VectorNeighbors); ok {
		switch {
		case top.Score >= th.AutoMerge:
			return ResolveDecision{
				Action:    ActionAttach,
				MatchKind: MatchVector,
				Target:    top.ID,
				Reason:    fmt.Sprintf("vector similarity %.3f >= auto-merge floor %.3f", top.Score, th.AutoMerge),
			}, nil
		case top.Score >= th.Review:
			return ResolveDecision{
				Action:    ActionInsertFlagged,
				MatchKind: MatchNone,
				FlagTo:    top.ID,
				FlagScore: top.Score,
				Reason:    fmt.Sprintf("vector similarity %.3f in review band [%.3f,%.3f): insert new + sameAs? flag", top.Score, th.Review, th.AutoMerge),
			}, nil
		}
	}

	return ResolveDecision{Action: ActionInsertNew, MatchKind: MatchNone, Reason: "no external or vector match: insert new"}, nil
}

// dedupeIDs returns the distinct ids preserving input (oldest-first) order.
func dedupeIDs(ids []EntityID) []EntityID {
	seen := make(map[EntityID]struct{}, len(ids))
	out := make([]EntityID, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// maxScore returns the highest-scoring neighbour (Resolve does not assume the
// store pre-sorted them). ok is false when there are none.
func maxScore(ns []ScoredMatch) (ScoredMatch, bool) {
	if len(ns) == 0 {
		return ScoredMatch{}, false
	}
	best := ns[0]
	for _, n := range ns[1:] {
		if n.Score > best.Score {
			best = n
		}
	}
	return best, true
}
