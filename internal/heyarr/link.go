package heyarr

import (
	"strings"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// The heyarr bridge external-id schemes. jumpdrive-index REFERENCES a heyarr
// title by one of these ids and reads the title's details through heyarr MCP at
// query time — it never copies heyarr's catalogue into the graph. The four
// schemes are the join altitudes from the plan:
//
//   - work    — the semantic title (a movie/series/album/book), heyarr UUIDv7.
//   - edition — a specific version/cut of a work, heyarr UUIDv7.
//   - asset   — a concrete file, heyarr UUIDv7.
//   - blake3  — a content-addressed blob hash (byte identity; survives a remux).
//
// domain.ExternalID's scheme set is open, so these need no allow-list change;
// naming them as constants keeps every caller (and the drift gate) consistent.
const (
	SchemeWork    = "heyarr-work"
	SchemeEdition = "heyarr-edition"
	SchemeAsset   = "heyarr-asset"
	SchemeBlake3  = "heyarr-blake3"
)

// WorkExternalID anchors an entity to a heyarr work by id.
func WorkExternalID(id string) domain.ExternalID {
	return domain.ExternalID{Scheme: SchemeWork, Value: id}
}

// EditionExternalID anchors an entity to a heyarr edition by id.
func EditionExternalID(id string) domain.ExternalID {
	return domain.ExternalID{Scheme: SchemeEdition, Value: id}
}

// AssetExternalID anchors an entity to a heyarr asset (file) by id.
func AssetExternalID(id string) domain.ExternalID {
	return domain.ExternalID{Scheme: SchemeAsset, Value: id}
}

// Blake3ExternalID anchors an entity to a heyarr blob by its blake3 content hash.
func Blake3ExternalID(hash string) domain.ExternalID {
	return domain.ExternalID{Scheme: SchemeBlake3, Value: hash}
}

// IsHeyarrScheme reports whether scheme is one of the heyarr bridge schemes.
func IsHeyarrScheme(scheme string) bool {
	switch scheme {
	case SchemeWork, SchemeEdition, SchemeAsset, SchemeBlake3:
		return true
	default:
		return false
	}
}

// IDs returns the heyarr bridge external ids carried by an entity, in input
// order — the reverse of the linking helpers, for reading a link back off a node.
func IDs(e domain.Entity) []domain.ExternalID {
	var out []domain.ExternalID
	for _, x := range e.ExternalIDs {
		if IsHeyarrScheme(x.Scheme) {
			out = append(out, x)
		}
	}
	return out
}

// ForeignExternalID maps one of heyarr's OWN external ids (from Work.ExternalIDs,
// e.g. {"tmdb":"603"}) to a domain.ExternalID under that upstream scheme. This is
// the tmdb/imdb reconciliation join that heyarr ADR-0050 unlocks: a jumpdrive-
// index entity and a heyarr work can be matched on a shared tmdb id. The scheme
// is lower-cased for a stable Key(); an empty scheme or value yields ok=false.
func ForeignExternalID(scheme, value string) (domain.ExternalID, bool) {
	scheme = strings.ToLower(strings.TrimSpace(scheme))
	value = strings.TrimSpace(value)
	if scheme == "" || value == "" {
		return domain.ExternalID{}, false
	}
	return domain.ExternalID{Scheme: scheme, Value: value}, true
}
