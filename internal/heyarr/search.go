package heyarr

import (
	"context"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
)

// Work is one heyarr catalogue hit, decoded from a search_content result. The
// fields are heyarr's real output — its handler selects exactly
// id/content_type/title/year (verified against heyarr-core
// internal/api/mcp/reads.go). Only what jumpdrive-index needs to LINK by
// reference is modelled; unknown fields are ignored so heyarr can grow its
// payload without breaking this client.
//
// NB: external_ids (tmdb/imdb) is deliberately NOT here — search_content does not
// return it. That reconciliation data becomes available only via the
// get_external_ids MCP tool once heyarr ADR-0050 (heyarr PR #349) lands; the
// generic Call primitive consumes it in one line then. Decoding a field heyarr
// never sends would be the same guess this struct was corrected away from.
type Work struct {
	WorkID      string `json:"work_id"`        // heyarr work UUIDv7 → a heyarr-work external id
	ContentType string `json:"content_type"`   // movie | series | music | book
	Title       string `json:"title"`          //
	Year        *int64 `json:"year,omitempty"` // optional — heyarr omits it when unknown
}

// SearchResult is the search_content envelope: the matching works plus heyarr's
// truncation signal. Truncated is not decoration — a list heyarr cut but did not
// flag is one an agent would wrongly treat as exhaustive, so it is surfaced.
type SearchResult struct {
	Works     []Work `json:"works"`
	Count     int    `json:"count"`
	Truncated bool   `json:"truncated"`
}

// SearchArgs parameterises a search_content call. heyarr requires at least one of
// Query / ContentType; Limit is an optional narrowing hint passed through.
type SearchArgs struct {
	Query       string `json:"query,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// SearchContent calls heyarr's search_content tool and returns the matching
// works together with heyarr's count/truncated signal. An empty match is a
// zero-value SearchResult with a nil error — the caller reads that as "no heyarr
// match". A transport, protocol, or tool error is a typed *Error and never a
// panic; a caller applying heyarr ADR-0025 degradation may treat that error as
// "no match" too, but it stays distinguishable from a genuine empty result.
func (c *Client) SearchContent(ctx context.Context, args SearchArgs) (SearchResult, error) {
	if args.Query == "" && args.ContentType == "" {
		return SearchResult{}, &Error{Op: "search_content", Message: "need a query or a content_type"}
	}
	var res SearchResult
	if err := c.Call(ctx, "search_content", args, &res); err != nil {
		return SearchResult{}, err
	}
	return res, nil
}

// ExternalID returns the reference-by-id anchor for this heyarr work — a
// domain.ExternalID under the heyarr-work scheme. Linking a jumpdrive-index
// entity to a heyarr title means attaching THIS, never copying w's fields.
func (w Work) ExternalID() domain.ExternalID {
	return WorkExternalID(w.WorkID)
}
