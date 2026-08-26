package domain

import "testing"

var testTh = Thresholds{AutoMerge: 0.94, Review: 0.86}

func TestThresholdsValidate(t *testing.T) {
	if err := testTh.Validate(); err != nil {
		t.Fatalf("valid thresholds rejected: %v", err)
	}
	bad := []Thresholds{
		{AutoMerge: 0.8, Review: 0.9}, // AutoMerge must exceed Review
		{AutoMerge: 1.5, Review: 0.5}, // out of (0,1]
		{AutoMerge: 0.9, Review: 0},   // out of (0,1]
		{AutoMerge: 0.9, Review: 0.9}, // must be strictly greater
	}
	for i, th := range bad {
		if err := th.Validate(); err == nil {
			t.Errorf("case %d: expected invalid thresholds %+v to be rejected", i, th)
		}
	}
}

func TestResolve(t *testing.T) {
	e1, e2, e3 := EntityID("e1"), EntityID("e2"), EntityID("e3")
	cand := Entity{Type: "Movie"}

	cases := []struct {
		name       string
		in         ResolveInputs
		policy     ResolvePolicy
		wantAction ResolveAction
		wantKind   MatchKind
		wantTarget EntityID
		wantFlagTo EntityID
		wantMerge  []EntityID
	}{
		{
			name:       "single external hit attaches",
			in:         ResolveInputs{ExternalIDHits: []EntityID{e1}},
			policy:     ResolveAuto,
			wantAction: ActionAttach, wantKind: MatchExternal, wantTarget: e1,
		},
		{
			name:       "duplicate external hits collapse to same target (still single)",
			in:         ResolveInputs{ExternalIDHits: []EntityID{e1, e1}},
			policy:     ResolveAuto,
			wantAction: ActionAttach, wantKind: MatchExternal, wantTarget: e1,
		},
		{
			name:       "multiple distinct external hits merge, oldest kept",
			in:         ResolveInputs{ExternalIDHits: []EntityID{e2, e3}},
			policy:     ResolveAuto,
			wantAction: ActionMerge, wantKind: MatchExternal, wantTarget: e2,
			wantMerge: []EntityID{e2, e3},
		},
		{
			name:       "force_new ignores an external hit",
			in:         ResolveInputs{ExternalIDHits: []EntityID{e1}},
			policy:     ResolveForceNew,
			wantAction: ActionInsertNew, wantKind: MatchNone,
		},
		{
			name:       "external_only never consults the vector",
			in:         ResolveInputs{VectorNeighbors: []ScoredMatch{{ID: e1, Score: 0.99}}},
			policy:     ResolveExternalOnly,
			wantAction: ActionInsertNew, wantKind: MatchNone,
		},
		{
			name:       "vector above auto-merge floor attaches",
			in:         ResolveInputs{VectorNeighbors: []ScoredMatch{{ID: e1, Score: 0.97}}},
			policy:     ResolveAuto,
			wantAction: ActionAttach, wantKind: MatchVector, wantTarget: e1,
		},
		{
			name:       "vector in review band inserts and flags",
			in:         ResolveInputs{VectorNeighbors: []ScoredMatch{{ID: e1, Score: 0.90}}},
			policy:     ResolveAuto,
			wantAction: ActionInsertFlagged, wantKind: MatchNone, wantFlagTo: e1,
		},
		{
			name:       "vector below review band inserts new",
			in:         ResolveInputs{VectorNeighbors: []ScoredMatch{{ID: e1, Score: 0.80}}},
			policy:     ResolveAuto,
			wantAction: ActionInsertNew, wantKind: MatchNone,
		},
		{
			name:       "picks the highest-scoring neighbour regardless of order",
			in:         ResolveInputs{VectorNeighbors: []ScoredMatch{{ID: e1, Score: 0.70}, {ID: e2, Score: 0.97}}},
			policy:     ResolveAuto,
			wantAction: ActionAttach, wantKind: MatchVector, wantTarget: e2,
		},
		{
			name:       "no inputs inserts new",
			in:         ResolveInputs{},
			policy:     ResolveAuto,
			wantAction: ActionInsertNew, wantKind: MatchNone,
		},
		{
			name:       "external match wins over a strong vector match",
			in:         ResolveInputs{ExternalIDHits: []EntityID{e1}, VectorNeighbors: []ScoredMatch{{ID: e2, Score: 0.99}}},
			policy:     ResolveAuto,
			wantAction: ActionAttach, wantKind: MatchExternal, wantTarget: e1,
		},
	}

	seenActions := map[ResolveAction]int{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(cand, tc.in, tc.policy, testTh)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Action != tc.wantAction {
				t.Errorf("action = %q, want %q", got.Action, tc.wantAction)
			}
			if got.MatchKind != tc.wantKind {
				t.Errorf("matchKind = %q, want %q", got.MatchKind, tc.wantKind)
			}
			if tc.wantTarget != "" && got.Target != tc.wantTarget {
				t.Errorf("target = %q, want %q", got.Target, tc.wantTarget)
			}
			if tc.wantFlagTo != "" && got.FlagTo != tc.wantFlagTo {
				t.Errorf("flagTo = %q, want %q", got.FlagTo, tc.wantFlagTo)
			}
			if tc.wantMerge != nil {
				if len(got.MergeTargets) != len(tc.wantMerge) {
					t.Fatalf("mergeTargets = %v, want %v", got.MergeTargets, tc.wantMerge)
				}
				for i := range tc.wantMerge {
					if got.MergeTargets[i] != tc.wantMerge[i] {
						t.Errorf("mergeTargets[%d] = %q, want %q", i, got.MergeTargets[i], tc.wantMerge[i])
					}
				}
			}
			// Every decision must explain itself (drift-test discipline).
			if got.Reason == "" {
				t.Errorf("decision has an empty Reason")
			}
			seenActions[got.Action]++
		})
	}

	// Balance floor: the corpus must exercise more than one verdict, or a
	// "return InsertNew always" bug would pass every case above.
	if len(seenActions) < 3 {
		t.Errorf("corpus exercised only %d distinct actions (%v); need broader coverage", len(seenActions), seenActions)
	}
}

func TestResolveRejectsInvalidPolicy(t *testing.T) {
	if _, err := Resolve(Entity{}, ResolveInputs{}, ResolvePolicy("bogus"), testTh); err == nil {
		t.Fatal("expected an invalid policy to be rejected, not silently defaulted")
	}
}

func TestDeriveDedupeKey(t *testing.T) {
	// External-id-bearing entities get a STABLE key across order and repetition.
	a := DeriveDedupeKey("Movie", []ExternalID{{"tmdb", "603"}, {"imdb", "tt0133093"}}, "The Matrix")
	b := DeriveDedupeKey("Movie", []ExternalID{{"imdb", "tt0133093"}, {"tmdb", "603"}}, "the   matrix")
	if a != b {
		t.Errorf("dedupe key not stable across ext-id order / name spacing: %s vs %s", a, b)
	}
	// A different type is a different thing.
	if DeriveDedupeKey("Book", []ExternalID{{"tmdb", "603"}}, "") == DeriveDedupeKey("Movie", []ExternalID{{"tmdb", "603"}}, "") {
		t.Error("dedupe key must distinguish @type")
	}
	// No external id + no name → caller must supply a random key.
	if !RandomKeyNeeded(nil, "  ") {
		t.Error("expected RandomKeyNeeded for an anonymous entity")
	}
	if RandomKeyNeeded([]ExternalID{{"tmdb", "603"}}, "") {
		t.Error("an entity with an external id does not need a random key")
	}
}
