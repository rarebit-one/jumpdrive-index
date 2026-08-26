package domain

// This file is the schema.org PROFILE: a curated allow-list of the types and
// predicates the graph accepts. schema.org is used as a vocabulary, not an
// enforced schema — we adopt its names (which LLMs already know) but validate
// only the @type and the predicate, keeping the property bag itself flexible.
//
// The list is deliberately small to start (media + people + the YouTube linking
// case) and grows by explicit addition. Unknown types/predicates are rejected at
// the boundary (default-deny) rather than silently stored, so the graph cannot
// drift into an un-queryable mush.

// allowedTypes is the accepted set of schema.org @types. Grow by adding here.
var allowedTypes = map[Type]struct{}{
	"Thing":        {}, // fallback for a genuinely untyped node
	"Movie":        {},
	"TVSeries":     {},
	"TVEpisode":    {},
	"VideoObject":  {}, // a YouTube analysis video
	"Clip":         {}, // a timecoded span of a VideoObject (startOffset/endOffset)
	"Person":       {},
	"Organization": {},
	"Event":        {},
	"CreativeWork": {},
	"Product":      {},
	"Book":         {},
	"Article":      {},
	"Note":         {},
	// Channel is not a schema.org type; a YouTube channel is modelled as an
	// Organization (or a CreativeWorkSeries) — kept out of the list on purpose.
}

// allowedPredicates is the accepted set of edge predicates (schema.org
// properties). Grow by adding here.
var allowedPredicates = map[Predicate]struct{}{
	"about":     {}, // VideoObject --about--> Movie (the core YouTube link)
	"mentions":  {},
	"subjectOf": {},
	"hasPart":   {}, // Movie --hasPart--> Clip
	"isPartOf":  {},
	"actor":     {},
	"director":  {},
	"author":    {},
	"creator":   {},
	"about?":    {}, // inferred/low-confidence variant, distinct from asserted "about"
	"sameAs":    {}, // an asserted identity link
	"sameAs?":   {}, // an INFERRED possible-duplicate link (resolve's review band)
	"relatedTo": {},
}

// KnownType reports whether t is in the accepted @type allow-list.
func KnownType(t Type) bool {
	_, ok := allowedTypes[t]
	return ok
}

// KnownPredicate reports whether p is in the accepted predicate allow-list.
func KnownPredicate(p Predicate) bool {
	_, ok := allowedPredicates[p]
	return ok
}
