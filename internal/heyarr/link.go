package heyarr

import (
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// SchemeWork is the heyarr bridge external-id scheme for a semantic title (a
// movie/series/album/book), identified by a heyarr UUIDv7. jumpdrive-index
// REFERENCES a heyarr title by this scheme and reads details through heyarr MCP
// at query time — it never copies heyarr's catalogue into the graph.
//
// domain.ExternalID's scheme set is open, so this needs no allow-list change;
// naming it as a constant keeps every caller (and the drift gate) consistent.
const SchemeWork = "heyarr-work"

// WorkExternalID anchors an entity to a heyarr work by id.
func WorkExternalID(id string) domain.ExternalID {
	return domain.ExternalID{Scheme: SchemeWork, Value: id}
}
