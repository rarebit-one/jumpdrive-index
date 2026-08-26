package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

const edgeCols = `id, predicate, from_id, to_id, props, space, owner, visibility,
	prov_asserter, prov_method, prov_source, prov_confidence, prov_asserted_at, created_at`

// AppendEdgeFact asserts a first-class edge (with its own visibility/provenance)
// and folds it into the projection in one write transaction.
func (s *Store) AppendEdgeFact(ctx context.Context, in store.AppendEdgeInput) (domain.Edge, error) {
	ed := in.Edge
	if !domain.KnownPredicate(ed.Predicate) {
		return domain.Edge{}, fmt.Errorf("%w: unknown predicate %q", store.ErrInvalidInput, ed.Predicate)
	}
	if !ed.Visibility.Valid() {
		return domain.Edge{}, fmt.Errorf("%w: invalid edge visibility %q", store.ErrInvalidInput, ed.Visibility)
	}
	ed.Provenance = normalizeProvenance(ed.Provenance, s.now())
	dedupeKey := in.DedupeKey
	if dedupeKey == "" {
		dedupeKey = fmt.Sprintf("edge|%s|%s|%s", ed.Predicate, ed.From, ed.To)
	}
	ts := s.now()

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return domain.Edge{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var prior string
	err = tx.QueryRowContext(ctx,
		`SELECT subject FROM facts WHERE writer=? AND dedupe_key=? AND kind='edge.asserted' ORDER BY seq LIMIT 1`,
		string(in.Writer), dedupeKey).Scan(&prior)
	switch {
	case err == nil:
		existing, e := getEdgeByID(ctx, tx, domain.EdgeID(prior))
		if e != nil {
			return domain.Edge{}, e
		}
		return existing, store.ErrDuplicateFact
	case errors.Is(err, sql.ErrNoRows):
	default:
		return domain.Edge{}, err
	}

	ed.ID = domain.EdgeID(s.newID())
	if err := upsertEdgeSnapshot(ctx, tx, ed, ts); err != nil {
		return domain.Edge{}, err
	}
	payload, err := json.Marshal(ed)
	if err != nil {
		return domain.Edge{}, err
	}
	if err := s.insertFact(ctx, tx, domain.FactEdgeAsserted, string(ed.ID), in.Writer, dedupeKey, payload, in.Actor, ts); err != nil {
		return domain.Edge{}, err
	}
	out, err := getEdgeByID(ctx, tx, ed.ID)
	if err != nil {
		return domain.Edge{}, err
	}
	if err := tx.Commit(); err != nil {
		return domain.Edge{}, err
	}
	return out, nil
}

// upsertEdgeSnapshot writes one edge snapshot. Shared by the online write path
// and RebuildProjection. ed.ID is set.
func upsertEdgeSnapshot(ctx context.Context, tx *sql.Tx, ed domain.Edge, ts time.Time) error {
	p := normalizeProvenance(ed.Provenance, ts)
	var props any
	if len(ed.Props) > 0 {
		props = string(ed.Props)
	}
	_, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO edges(`+edgeCols+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(ed.ID), string(ed.Predicate), string(ed.From), string(ed.To), props,
		string(ed.Space), string(ed.Owner), string(ed.Visibility),
		p.Asserter, string(p.Method), p.Source, p.Confidence, fmtTS(p.AssertedAt), fmtTS(ts))
	if err != nil {
		return fmt.Errorf("insert edge: %w", err)
	}
	return nil
}

func getEdgeByID(ctx context.Context, q queryer, id domain.EdgeID) (domain.Edge, error) {
	row := q.QueryRowContext(ctx, `SELECT `+edgeCols+` FROM edges WHERE id=?`, string(id))
	ed, err := scanEdge(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Edge{}, store.ErrNotFound
	}
	return ed, err
}

func scanEdge(row scanner) (domain.Edge, error) {
	var (
		ed                      domain.Edge
		id, pred, from, to      string
		props                   sql.NullString
		space, own, vis, method string
		asserter, source        string
		conf                    float64
		assertedAt, cAt         string
	)
	if err := row.Scan(&id, &pred, &from, &to, &props, &space, &own, &vis,
		&asserter, &method, &source, &conf, &assertedAt, &cAt); err != nil {
		return domain.Edge{}, err
	}
	ed.ID = domain.EdgeID(id)
	ed.Predicate = domain.Predicate(pred)
	ed.From = domain.EntityID(from)
	ed.To = domain.EntityID(to)
	if props.Valid {
		ed.Props = json.RawMessage(props.String)
	}
	ed.Space = domain.SpaceID(space)
	ed.Owner = domain.PrincipalID(own)
	ed.Visibility = domain.Visibility(vis)
	ed.Provenance = domain.Provenance{
		Asserter: asserter, Method: domain.AssertMethod(method), Source: source,
		Confidence: conf, AssertedAt: parseTS(assertedAt),
	}
	ed.CreatedAt = parseTS(cAt)
	return ed, nil
}

// RetractEdge tombstones an edge: removes it from the projection and appends an
// edge.retracted fact (the fact log retains it).
func (s *Store) RetractEdge(ctx context.Context, id domain.EdgeID, actor domain.PrincipalID, dedupeKey string) error {
	if dedupeKey == "" {
		dedupeKey = fmt.Sprintf("%s|retract", id)
	}
	ts := s.now()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE id=?`, string(id)); err != nil {
		return err
	}
	if err := s.insertFact(ctx, tx, domain.FactEdgeRetracted, string(id), domain.WriterID(actor), dedupeKey, []byte(`{}`), actor, ts); err != nil {
		return err
	}
	return tx.Commit()
}
