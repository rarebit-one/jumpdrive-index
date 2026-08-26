package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// DeriveDedupeKey computes a deterministic idempotency key for an entity that
// did not carry an explicit one. The rule mirrors jumpdrive-web's keyed-vs-unkeyed
// split:
//
//   - An entity with external ids gets a STABLE key (a hash of its type + sorted
//     external keys), so re-asserting the same real-world thing (e.g. tmdb:603)
//     is a no-op instead of a second node — even days apart, from any writer.
//   - An entity with no external ids and no name has nothing stable to key on, so
//     the caller must supply a random key (RandomKeyNeeded reports this); hashing
//     "nothing" would make every anonymous assert false-collide into one node.
//
// The normalized display name is folded in only as a weak tiebreaker for
// externalid-less-but-named entities; exact external-id match (in resolve) is
// always the authoritative dedup, this is just the fallback key.
//
// NOTE: sha256 (stdlib) is used deliberately for this internal, dependency-free
// key. It is a stable digest, not a content-address; heyarr's blob byte-identity
// (blake3) is a separate concern we only ever *reference* by string.
func DeriveDedupeKey(t Type, externalIDs []ExternalID, displayName string) string {
	keys := make([]string, 0, len(externalIDs))
	for _, x := range externalIDs {
		keys = append(keys, x.Key())
	}
	sort.Strings(keys)

	h := sha256.New()
	h.Write([]byte("jdx.entity\x00"))
	h.Write([]byte(string(t)))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(keys, "\x00")))
	if len(keys) == 0 {
		// No external ids: fold in the normalized name as the only stable signal.
		h.Write([]byte{0})
		h.Write([]byte(normalizeName(displayName)))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RandomKeyNeeded reports whether an entity has nothing stable to key on, so the
// caller must supply a random DedupeKey rather than a derived one. Keying such an
// entity deterministically would collapse every anonymous assert into a single
// node.
func RandomKeyNeeded(externalIDs []ExternalID, displayName string) bool {
	return len(externalIDs) == 0 && normalizeName(displayName) == ""
}

// normalizeName lowercases and collapses whitespace so trivially-different
// spellings of the same name derive the same fallback key.
func normalizeName(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
