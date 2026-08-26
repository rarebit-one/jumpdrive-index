package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// The governed write path (propose → approve), mirroring jumpdrive-web's
// KnowledgePromotion: a proposal HOLDS the exact serialized write intent without
// projecting anything; approving it replays that intent through the normal
// resolve-before-create path, so a proposed write and a direct write are
// identical once approved. Only entity.asserted and edge.asserted are proposable.

// Propose stores a pending proposal (its payload is the serialized
// AppendEntityInput / AppendEdgeInput) and returns its id, minting one if absent.
func (s *Store) Propose(ctx context.Context, p store.Proposal) (store.ProposalID, error) {
	if p.Kind != domain.FactEntityAsserted && p.Kind != domain.FactEdgeAsserted {
		return "", fmt.Errorf("%w: proposals accept only entity.asserted or edge.asserted, got %q", store.ErrInvalidInput, p.Kind)
	}
	if !json.Valid(p.Payload) {
		return "", fmt.Errorf("%w: proposal payload is not valid JSON", store.ErrInvalidInput)
	}
	id := p.ID
	if id == "" {
		id = store.ProposalID(s.newID())
	}
	if _, err := s.write.ExecContext(ctx,
		`INSERT INTO proposals(id, kind, proposer, space, payload, status, created_at)
		 VALUES(?,?,?,?,?, 'pending', ?)`,
		string(id), string(p.Kind), string(p.Proposer), string(p.Space), string(p.Payload), s.tsNow()); err != nil {
		return "", err
	}
	return id, nil
}

// DecideProposal approves or rejects a pending proposal. Approval atomically
// CLAIMS the proposal (a conditional update on status='pending', so two
// concurrent deciders cannot both apply it) and then replays the held intent
// through AppendEntityFact / AppendEdgeFact. Rejection discards it, writing
// nothing to the graph.
func (s *Store) DecideProposal(ctx context.Context, id store.ProposalID, approve bool, approver domain.PrincipalID) (store.ResolveResult, error) {
	var kind, payload, status string
	err := s.write.QueryRowContext(ctx,
		`SELECT kind, payload, status FROM proposals WHERE id = ?`, string(id)).Scan(&kind, &payload, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ResolveResult{}, store.ErrNotFound
	}
	if err != nil {
		return store.ResolveResult{}, err
	}
	if status != "pending" {
		return store.ResolveResult{}, fmt.Errorf("%w: proposal already %s", store.ErrConflict, status)
	}
	ts := s.tsNow()

	if !approve {
		_, err := s.write.ExecContext(ctx,
			`UPDATE proposals SET status='discarded', decided_at=?, decided_by=? WHERE id=? AND status='pending'`,
			ts, string(approver), string(id))
		return store.ResolveResult{}, err
	}

	// Claim: only the writer that flips pending->promoted proceeds to replay.
	claim, err := s.write.ExecContext(ctx,
		`UPDATE proposals SET status='promoted', decided_at=?, decided_by=? WHERE id=? AND status='pending'`,
		ts, string(approver), string(id))
	if err != nil {
		return store.ResolveResult{}, err
	}
	if n, _ := claim.RowsAffected(); n != 1 {
		return store.ResolveResult{}, fmt.Errorf("%w: proposal was decided concurrently", store.ErrConflict)
	}

	var result store.ResolveResult
	var subject string
	switch domain.FactKind(kind) {
	case domain.FactEntityAsserted:
		var in store.AppendEntityInput
		if err := json.Unmarshal([]byte(payload), &in); err != nil {
			return store.ResolveResult{}, err
		}
		result, err = s.AppendEntityFact(ctx, in)
		if err != nil && !errors.Is(err, store.ErrDuplicateFact) {
			return store.ResolveResult{}, err
		}
		subject = string(result.Entity.ID)
	case domain.FactEdgeAsserted:
		var in store.AppendEdgeInput
		if err := json.Unmarshal([]byte(payload), &in); err != nil {
			return store.ResolveResult{}, err
		}
		edge, err := s.AppendEdgeFact(ctx, in)
		if err != nil && !errors.Is(err, store.ErrDuplicateFact) {
			return store.ResolveResult{}, err
		}
		subject = string(edge.ID)
	default:
		return store.ResolveResult{}, fmt.Errorf("%w: unproposable kind %q", store.ErrInvalidInput, kind)
	}

	if _, err := s.write.ExecContext(ctx,
		`UPDATE proposals SET result_subject=? WHERE id=?`, subject, string(id)); err != nil {
		return store.ResolveResult{}, err
	}
	return result, nil
}

// ListProposals returns pending proposals, optionally scoped to one space,
// oldest first.
func (s *Store) ListProposals(ctx context.Context, f store.ProposalFilter) ([]store.Proposal, error) {
	q := `SELECT id, kind, proposer, space, payload FROM proposals WHERE status='pending'`
	var args []any
	if f.Space != "" {
		q += ` AND space = ?`
		args = append(args, string(f.Space))
	}
	q += ` ORDER BY created_at, id`

	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.Proposal
	for rows.Next() {
		var p store.Proposal
		var id, kind, proposer, space, payload string
		if err := rows.Scan(&id, &kind, &proposer, &space, &payload); err != nil {
			return nil, err
		}
		p.ID = store.ProposalID(id)
		p.Kind = domain.FactKind(kind)
		p.Proposer = domain.PrincipalID(proposer)
		p.Space = domain.SpaceID(space)
		p.Payload = []byte(payload)
		out = append(out, p)
	}
	return out, rows.Err()
}
