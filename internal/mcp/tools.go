package mcp

import (
	"context"
	"encoding/json"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/service"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// registerTools wires the graph operations as MCP tools. Each handler unmarshals
// its arguments and delegates to the service, which authenticates the bearer and
// authorizes the call. `search` (semantic/full-text) is intentionally absent
// until the embedder + FTS land.
func (s *Server) registerTools() {
	reg := func(name, desc, schema string, h handlerFunc) {
		s.tools[name] = tool{name: name, description: desc, inputSchema: json.RawMessage(schema), handler: h}
	}

	reg("get_entity", "Fetch one entity by its internal id (if the caller may see it).",
		`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			var a struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			return s.svc.GetEntity(ctx, bearer, domain.EntityID(a.ID))
		})

	reg("resolve_external", "Find entities carrying any of the given external ids (scheme:value join keys, e.g. tmdb:603).",
		`{"type":"object","properties":{"keys":{"type":"array","items":{"type":"string"}}},"required":["keys"]}`,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			var a struct {
				Keys []string `json:"keys"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			return s.svc.ResolveByExternalID(ctx, bearer, a.Keys)
		})

	reg("get_neighbors", "Traverse outward from a start entity (undirected, access-filtered at every hop).",
		`{"type":"object","properties":{"start":{"type":"string"},"predicates":{"type":"array","items":{"type":"string"}},"max_hops":{"type":"integer"},"limit":{"type":"integer"}},"required":["start"]}`,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			var a struct {
				Start      string   `json:"start"`
				Predicates []string `json:"predicates"`
				MaxHops    int      `json:"max_hops"`
				Limit      int      `json:"limit"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			preds := make([]domain.Predicate, 0, len(a.Predicates))
			for _, p := range a.Predicates {
				preds = append(preds, domain.Predicate(p))
			}
			return s.svc.Neighbors(ctx, bearer, store.NeighborQuery{
				Start: domain.EntityID(a.Start), Predicates: preds, MaxHops: a.MaxHops, Limit: a.Limit,
			})
		})

	reg("create_entity", "Assert an entity (resolve-before-create: dedups by external id / vector). Owner is the caller.",
		entitySchema,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			in, err := parseEntity(args)
			if err != nil {
				return nil, err
			}
			return s.svc.CreateEntity(ctx, bearer, in)
		})

	reg("link", "Assert a typed edge between two entities (the edge carries its own visibility).",
		linkSchema,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			in, err := parseLink(args)
			if err != nil {
				return nil, err
			}
			return s.svc.Link(ctx, bearer, in)
		})

	reg("propose_entity", "Hold an entity assertion for approval (not projected until approved).",
		entitySchema,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			in, err := parseEntity(args)
			if err != nil {
				return nil, err
			}
			id, err := s.svc.ProposeEntity(ctx, bearer, in)
			if err != nil {
				return nil, err
			}
			return map[string]any{"proposal_id": id, "pending": true}, nil
		})

	reg("propose_link", "Hold an edge assertion for approval.",
		linkSchema,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			in, err := parseLink(args)
			if err != nil {
				return nil, err
			}
			id, err := s.svc.ProposeLink(ctx, bearer, in)
			if err != nil {
				return nil, err
			}
			return map[string]any{"proposal_id": id, "pending": true}, nil
		})

	reg("decide_proposal", "Approve or reject a held proposal (requires approve rights in its space).",
		`{"type":"object","properties":{"proposal_id":{"type":"string"},"approve":{"type":"boolean"}},"required":["proposal_id","approve"]}`,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			var a struct {
				ProposalID string `json:"proposal_id"`
				Approve    bool   `json:"approve"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return nil, err
			}
			return s.svc.DecideProposal(ctx, bearer, store.ProposalID(a.ProposalID), a.Approve)
		})

	reg("list_proposals", "List pending proposals the caller may approve (optionally scoped to a space).",
		`{"type":"object","properties":{"space":{"type":"string"}}}`,
		func(ctx context.Context, bearer string, args json.RawMessage) (any, error) {
			var a struct {
				Space string `json:"space"`
			}
			_ = json.Unmarshal(args, &a)
			return s.svc.ListProposals(ctx, bearer, store.ProposalFilter{Space: domain.SpaceID(a.Space)})
		})
}

const entitySchema = `{"type":"object","properties":{"type":{"type":"string"},"props":{"type":"object"},"external_ids":{"type":"array","items":{"type":"object","properties":{"scheme":{"type":"string"},"value":{"type":"string"}}}},"space":{"type":"string"},"visibility":{"type":"string","enum":["private","space","public"]},"policy":{"type":"string","enum":["auto","external_only","force_new"]},"dedupe_key":{"type":"string"}},"required":["type","space","visibility"]}`

const linkSchema = `{"type":"object","properties":{"predicate":{"type":"string"},"from":{"type":"string"},"to":{"type":"string"},"props":{"type":"object"},"space":{"type":"string"},"visibility":{"type":"string","enum":["private","space","public"]},"dedupe_key":{"type":"string"}},"required":["predicate","from","to","space","visibility"]}`

type entityArgs struct {
	Type        string              `json:"type"`
	Props       json.RawMessage     `json:"props"`
	ExternalIDs []domain.ExternalID `json:"external_ids"`
	Space       string              `json:"space"`
	Visibility  string              `json:"visibility"`
	Policy      string              `json:"policy"`
	DedupeKey   string              `json:"dedupe_key"`
}

func parseEntity(args json.RawMessage) (service.CreateEntityInput, error) {
	var a entityArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return service.CreateEntityInput{}, err
	}
	return service.CreateEntityInput{
		Type: domain.Type(a.Type), Props: a.Props, ExternalIDs: a.ExternalIDs,
		Space: domain.SpaceID(a.Space), Visibility: domain.Visibility(a.Visibility),
		Policy: domain.ResolvePolicy(a.Policy), DedupeKey: a.DedupeKey,
	}, nil
}

type linkArgs struct {
	Predicate  string          `json:"predicate"`
	From       string          `json:"from"`
	To         string          `json:"to"`
	Props      json.RawMessage `json:"props"`
	Space      string          `json:"space"`
	Visibility string          `json:"visibility"`
	DedupeKey  string          `json:"dedupe_key"`
}

func parseLink(args json.RawMessage) (service.LinkInput, error) {
	var a linkArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return service.LinkInput{}, err
	}
	return service.LinkInput{
		Predicate: domain.Predicate(a.Predicate), From: domain.EntityID(a.From), To: domain.EntityID(a.To),
		Props: a.Props, Space: domain.SpaceID(a.Space), Visibility: domain.Visibility(a.Visibility), DedupeKey: a.DedupeKey,
	}, nil
}
