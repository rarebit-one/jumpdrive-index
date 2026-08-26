// Package service is the authorization boundary between a surface (MCP, HTTP) and
// the store. Every operation authenticates the bearer against the access.Model,
// derives the caller's read Filter (for reads) or checks CanWrite/CanApprove (for
// writes), and only then calls the store. The store below stays access-agnostic —
// it just receives a Filter and executes. This is where "the access model gates
// the store" actually happens.
//
// NOTE (tracked): resolve-before-create in the store is still GLOBAL, not scoped
// to the caller's writable spaces — a multi-space hosted deployment wants external
// ids unique per-space, which is a schema decision deferred from here. For the
// single-tenant Starchart build it is a non-issue. Reads are already hard-filtered.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// Sentinels a surface maps to protocol errors (401 / 403).
var (
	ErrUnauthenticated = errors.New("service: unauthenticated")
	ErrForbidden       = errors.New("service: forbidden")
)

// Service wires an access model to a store.
type Service struct {
	store  store.Store
	access access.Model
}

// New builds a Service.
func New(st store.Store, am access.Model) *Service {
	return &Service{store: st, access: am}
}

// auth authenticates a bearer and returns the decision plus the derived read
// filter. A failed authentication is ErrUnauthenticated (deny-by-default).
func (s *Service) auth(bearer string) (access.Decision, access.Filter, error) {
	d, err := s.access.Authenticate(bearer)
	if err != nil {
		return access.Decision{}, access.Filter{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	return d, s.access.FilterFor(d), nil
}

// ---- reads (access-filtered) ----

// GetEntity returns an entity if the caller may see it.
func (s *Service) GetEntity(ctx context.Context, bearer string, id domain.EntityID) (domain.Entity, error) {
	_, af, err := s.auth(bearer)
	if err != nil {
		return domain.Entity{}, err
	}
	return s.store.GetEntity(ctx, af, id)
}

// ResolveByExternalID returns the access-visible entities carrying any of keys.
func (s *Service) ResolveByExternalID(ctx context.Context, bearer string, keys []string) ([]domain.Entity, error) {
	_, af, err := s.auth(bearer)
	if err != nil {
		return nil, err
	}
	return s.store.ResolveByExternalID(ctx, af, keys)
}

// Neighbors traverses outward from a start entity, access-filtered at every hop.
func (s *Service) Neighbors(ctx context.Context, bearer string, q store.NeighborQuery) (store.Subgraph, error) {
	_, af, err := s.auth(bearer)
	if err != nil {
		return store.Subgraph{}, err
	}
	return s.store.Neighbors(ctx, af, q)
}

// ---- writes (authorized) ----

// CreateEntityInput is the surface-facing shape for asserting an entity. Owner and
// asserter are taken from the authenticated caller, not the input.
type CreateEntityInput struct {
	Type        domain.Type
	Props       json.RawMessage
	ExternalIDs []domain.ExternalID
	Space       domain.SpaceID
	Visibility  domain.Visibility
	Embeddings  []domain.Embedding
	Policy      domain.ResolvePolicy
	DedupeKey   string
}

// CreateEntity asserts an entity into a space the caller may write to.
func (s *Service) CreateEntity(ctx context.Context, bearer string, in CreateEntityInput) (store.ResolveResult, error) {
	d, _, err := s.auth(bearer)
	if err != nil {
		return store.ResolveResult{}, err
	}
	if !s.access.CanWrite(d, in.Space) {
		return store.ResolveResult{}, fmt.Errorf("%w: no write access to space %q", ErrForbidden, in.Space)
	}
	return s.store.AppendEntityFact(ctx, s.entityInput(d, in))
}

// LinkInput is the surface-facing shape for asserting an edge.
type LinkInput struct {
	Predicate  domain.Predicate
	From, To   domain.EntityID
	Props      json.RawMessage
	Space      domain.SpaceID
	Visibility domain.Visibility
	DedupeKey  string
}

// Link asserts an edge in a space the caller may write to.
func (s *Service) Link(ctx context.Context, bearer string, in LinkInput) (domain.Edge, error) {
	d, _, err := s.auth(bearer)
	if err != nil {
		return domain.Edge{}, err
	}
	if !s.access.CanWrite(d, in.Space) {
		return domain.Edge{}, fmt.Errorf("%w: no write access to space %q", ErrForbidden, in.Space)
	}
	return s.store.AppendEdgeFact(ctx, s.edgeInput(d, in))
}

// ---- governed writes (propose → decide) ----

// ProposeEntity holds an entity assertion for approval. Proposing is open to any
// authenticated caller; the assertion is not projected until approved.
func (s *Service) ProposeEntity(ctx context.Context, bearer string, in CreateEntityInput) (store.ProposalID, error) {
	d, _, err := s.auth(bearer)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(s.entityInput(d, in))
	if err != nil {
		return "", err
	}
	return s.store.Propose(ctx, store.Proposal{
		Kind: domain.FactEntityAsserted, Proposer: d.Principal.ID, Space: in.Space, Payload: payload,
	})
}

// ProposeLink holds an edge assertion for approval.
func (s *Service) ProposeLink(ctx context.Context, bearer string, in LinkInput) (store.ProposalID, error) {
	d, _, err := s.auth(bearer)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(s.edgeInput(d, in))
	if err != nil {
		return "", err
	}
	return s.store.Propose(ctx, store.Proposal{
		Kind: domain.FactEdgeAsserted, Proposer: d.Principal.ID, Space: in.Space, Payload: payload,
	})
}

// DecideProposal approves or rejects a proposal. The caller must have approve
// rights in the proposal's space.
func (s *Service) DecideProposal(ctx context.Context, bearer string, id store.ProposalID, approve bool) (store.ResolveResult, error) {
	d, _, err := s.auth(bearer)
	if err != nil {
		return store.ResolveResult{}, err
	}
	space, err := s.proposalSpace(ctx, id)
	if err != nil {
		return store.ResolveResult{}, err
	}
	if !s.access.CanApprove(d, space) {
		return store.ResolveResult{}, fmt.Errorf("%w: no approve access to space %q", ErrForbidden, space)
	}
	return s.store.DecideProposal(ctx, id, approve, d.Principal.ID)
}

// ListProposals lists pending proposals the caller may approve (space-scoped when
// filter.Space is set).
func (s *Service) ListProposals(ctx context.Context, bearer string, f store.ProposalFilter) ([]store.Proposal, error) {
	d, _, err := s.auth(bearer)
	if err != nil {
		return nil, err
	}
	all, err := s.store.ListProposals(ctx, f)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, p := range all {
		if s.access.CanApprove(d, p.Space) {
			out = append(out, p)
		}
	}
	return out, nil
}

// ---- helpers ----

func (s *Service) entityInput(d access.Decision, in CreateEntityInput) store.AppendEntityInput {
	return store.AppendEntityInput{
		Candidate: domain.Entity{
			Type: in.Type, Props: in.Props, ExternalIDs: in.ExternalIDs,
			Space: in.Space, Owner: d.Principal.ID, Visibility: in.Visibility, Embeddings: in.Embeddings,
			Provenance: domain.Provenance{Asserter: string(d.Principal.ID), Method: domain.Asserted},
		},
		Writer: domain.WriterID("svc:" + string(d.Principal.ID)), Actor: d.Principal.ID,
		Policy: in.Policy, DedupeKey: in.DedupeKey,
	}
}

func (s *Service) edgeInput(d access.Decision, in LinkInput) store.AppendEdgeInput {
	return store.AppendEdgeInput{
		Edge: domain.Edge{
			Predicate: in.Predicate, From: in.From, To: in.To, Props: in.Props,
			Space: in.Space, Owner: d.Principal.ID, Visibility: in.Visibility,
			Provenance: domain.Provenance{Asserter: string(d.Principal.ID), Method: domain.Asserted},
		},
		Writer: domain.WriterID("svc:" + string(d.Principal.ID)), Actor: d.Principal.ID, DedupeKey: in.DedupeKey,
	}
}

// proposalSpace finds a pending proposal's space (for the approve check).
func (s *Service) proposalSpace(ctx context.Context, id store.ProposalID) (domain.SpaceID, error) {
	all, err := s.store.ListProposals(ctx, store.ProposalFilter{})
	if err != nil {
		return "", err
	}
	for _, p := range all {
		if p.ID == id {
			return p.Space, nil
		}
	}
	return "", store.ErrNotFound
}
