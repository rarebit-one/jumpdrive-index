package heyarr

import "context"

// ExternalID is one heyarr external-identifier mapping, as heyarr's
// get_external_ids MCP tool returns it (heyarr ADR-0050). The uniform row carries
// both the catalogue id (source/value) and the heyarr entity it belongs to, so a
// caller can read it either way: reverse (find the entity_id for a source+value)
// or forward (read the ids of a known work/edition).
type ExternalID struct {
	Source     string `json:"source"`
	Value      string `json:"value"`
	EntityType string `json:"entity_type"` // "work" | "edition"
	EntityID   string `json:"entity_id"`
}

// ExternalIDsRequest selects a get_external_ids lookup: EITHER an entity ref
// (WorkID or EditionID — forward) OR a Source+Value pair (reverse). Zero-valued
// fields are omitted from the wire arguments, so the request carries exactly the
// one mode heyarr expects.
type ExternalIDsRequest struct {
	WorkID    string `json:"work_id,omitempty"`
	EditionID string `json:"edition_id,omitempty"`
	Source    string `json:"source,omitempty"`
	Value     string `json:"value,omitempty"`
}

// externalIDsResult is get_external_ids's structured payload: a flat list, empty
// on no match.
type externalIDsResult struct {
	ExternalIDs []ExternalID `json:"external_ids"`
}

// GetExternalIDs calls heyarr's get_external_ids tool (ScopeRead, ADR-0050) and
// returns its uniform list of mappings — forward (an entity's ids) or reverse
// (which work/edition carries a source+value). An unknown id is heyarr's
// documented "no match": an empty slice with a nil error, never a failure. The
// request/response shapes are pinned by the vendored contract in contract/ and
// the drift test in contract_test.go.
func (c *Client) GetExternalIDs(ctx context.Context, req ExternalIDsRequest) ([]ExternalID, error) {
	var out externalIDsResult
	if err := c.Call(ctx, "get_external_ids", req, &out); err != nil {
		return nil, err
	}
	return out.ExternalIDs, nil
}
