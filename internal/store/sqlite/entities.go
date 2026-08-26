package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

const entityCols = `id, type, props, space, owner, visibility,
	prov_asserter, prov_method, prov_source, prov_confidence, prov_asserted_at,
	created_at, updated_at`

// AppendEntityFact runs resolve-before-create and folds the result INTO the
// projection in one write transaction (single-writer + _txlock=immediate, so the
// resolve→append→project window cannot race another writer).
func (s *Store) AppendEntityFact(ctx context.Context, in store.AppendEntityInput) (store.ResolveResult, error) {
	cand := in.Candidate
	if !domain.KnownType(cand.Type) {
		return store.ResolveResult{}, fmt.Errorf("%w: unknown @type %q", store.ErrInvalidInput, cand.Type)
	}
	if !cand.Visibility.Valid() {
		return store.ResolveResult{}, fmt.Errorf("%w: invalid visibility %q", store.ErrInvalidInput, cand.Visibility)
	}
	policy := in.Policy
	if policy == "" {
		policy = domain.ResolveAuto
	}
	if !policy.Valid() {
		return store.ResolveResult{}, domain.ErrInvalidPolicy
	}
	cand.Provenance = normalizeProvenance(cand.Provenance, s.now())

	dedupeKey := in.DedupeKey
	if dedupeKey == "" {
		name := extractName(cand.Props)
		if domain.RandomKeyNeeded(cand.ExternalIDs, name) {
			dedupeKey = s.newID()
		} else {
			dedupeKey = domain.DeriveDedupeKey(cand.Type, cand.ExternalIDs, name)
		}
	}
	ts := s.now()

	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return store.ResolveResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency short-circuit: a prior assertion under (writer, dedupe_key).
	var priorSubject string
	err = tx.QueryRowContext(ctx,
		`SELECT subject FROM facts WHERE writer=? AND dedupe_key=? AND kind='entity.asserted' ORDER BY seq LIMIT 1`,
		string(in.Writer), dedupeKey).Scan(&priorSubject)
	switch {
	case err == nil:
		ent, e := getEntityByID(ctx, tx, domain.EntityID(priorSubject))
		if e != nil {
			return store.ResolveResult{}, e
		}
		return store.ResolveResult{Entity: ent, Action: domain.ActionAttach, MatchKind: domain.MatchNone}, store.ErrDuplicateFact
	case errors.Is(err, sql.ErrNoRows):
		// fall through to resolve
	default:
		return store.ResolveResult{}, err
	}

	// Resolve inputs. Vector neighbours are a later milestone (SemanticSearch),
	// so only the external-id path is exercised here; resolve falls back to
	// insert-new when there is no external match.
	extHits, err := externalHitsTx(ctx, tx, cand.ExternalKeys())
	if err != nil {
		return store.ResolveResult{}, err
	}
	// Vector neighbours only when an external id did not already resolve it (they
	// are authoritative) and the candidate carries an embedding. The KNN is
	// unfiltered — dedup must consider all same-type candidates, like the
	// external-id lookup above.
	var neighbors []domain.ScoredMatch
	if policy == domain.ResolveAuto && len(extHits) == 0 && len(cand.Embeddings) > 0 {
		emb := cand.Embeddings[0]
		neighbors, err = s.knnByModel(ctx, tx, emb.Model, cand.Type, nil, emb.Vector, 5)
		if err != nil {
			return store.ResolveResult{}, err
		}
	}
	decision, err := domain.Resolve(cand, domain.ResolveInputs{ExternalIDHits: extHits, VectorNeighbors: neighbors}, policy, s.th)
	if err != nil {
		return store.ResolveResult{}, err
	}

	var result store.ResolveResult
	switch decision.Action {
	case domain.ActionInsertNew, domain.ActionInsertFlagged:
		cand.ID = domain.EntityID(s.newID())
		if err := upsertEntitySnapshot(ctx, tx, cand, ts); err != nil {
			return store.ResolveResult{}, err
		}
		if err := s.appendEntityFact(ctx, tx, cand, in.Writer, dedupeKey, in.Actor, ts); err != nil {
			return store.ResolveResult{}, err
		}
		// A review-band vector hit: create the node, but record an INFERRED
		// sameAs? edge to the near-duplicate so a human/agent can later confirm or
		// reject the identity. We never auto-merge in this band (a false merge is
		// dear to undo); the flag is a soft signal, not a decision.
		if decision.Action == domain.ActionInsertFlagged && decision.FlagTo != "" {
			edge := domain.Edge{
				ID:         domain.EdgeID(s.newID()),
				Predicate:  "sameAs?",
				From:       cand.ID,
				To:         decision.FlagTo,
				Space:      cand.Space,
				Owner:      cand.Owner,
				Visibility: cand.Visibility,
				Provenance: domain.Provenance{
					Asserter: string(in.Writer), Method: domain.Inferred,
					Source: "resolve:vector-review", Confidence: decision.FlagScore, AssertedAt: ts,
				},
			}
			if err := upsertEdgeSnapshot(ctx, tx, edge, ts); err != nil {
				return store.ResolveResult{}, err
			}
			payload, err := json.Marshal(edge)
			if err != nil {
				return store.ResolveResult{}, err
			}
			ek := fmt.Sprintf("%s|sameas|%s", dedupeKey, decision.FlagTo)
			if err := s.insertFact(ctx, tx, domain.FactEdgeAsserted, string(edge.ID), in.Writer, ek, payload, in.Actor, ts); err != nil {
				return store.ResolveResult{}, err
			}
		}
		result = store.ResolveResult{Action: decision.Action, MatchKind: decision.MatchKind}

	case domain.ActionAttach:
		cand.ID = decision.Target
		if err := upsertEntitySnapshot(ctx, tx, cand, ts); err != nil {
			return store.ResolveResult{}, err
		}
		if err := s.appendEntityFact(ctx, tx, cand, in.Writer, dedupeKey, in.Actor, ts); err != nil {
			return store.ResolveResult{}, err
		}
		result = store.ResolveResult{Action: domain.ActionAttach, MatchKind: decision.MatchKind}

	case domain.ActionMerge:
		keep := decision.Target
		for _, drop := range decision.MergeTargets[1:] {
			if err := mergeEntitiesTx(ctx, tx, keep, drop, ts); err != nil {
				return store.ResolveResult{}, err
			}
			mk := fmt.Sprintf("%s|merge|%s", dedupeKey, drop)
			if err := s.appendMergeFact(ctx, tx, keep, drop, in.Writer, mk, in.Actor, ts); err != nil {
				return store.ResolveResult{}, err
			}
			result.MergedFrom = append(result.MergedFrom, drop)
		}
		cand.ID = keep
		if err := upsertEntitySnapshot(ctx, tx, cand, ts); err != nil {
			return store.ResolveResult{}, err
		}
		if err := s.appendEntityFact(ctx, tx, cand, in.Writer, dedupeKey, in.Actor, ts); err != nil {
			return store.ResolveResult{}, err
		}
		result.Action = domain.ActionMerge
		result.MatchKind = decision.MatchKind
	}

	ent, err := getEntityByID(ctx, tx, cand.ID)
	if err != nil {
		return store.ResolveResult{}, err
	}
	result.Entity = ent
	if err := tx.Commit(); err != nil {
		return store.ResolveResult{}, err
	}
	return result, nil
}

// upsertEntitySnapshot applies one entity snapshot: it INSERTs the row on first
// sight (setting props + created_at) and, on a re-assertion (an attach), unions
// the external ids and embeddings and bumps updated_at WITHOUT clobbering the
// canonical props. Shared by the online write path and RebuildProjection, so the
// two fold identically. e.ID is the resolved target id.
func upsertEntitySnapshot(ctx context.Context, tx *sql.Tx, e domain.Entity, ts time.Time) error {
	tsS := fmtTS(ts)
	p := normalizeProvenance(e.Provenance, ts)

	var exists int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM entities WHERE id=?`, string(e.ID)).Scan(&exists)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		props := e.Props
		if len(props) == 0 {
			props = json.RawMessage("{}")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO entities(`+entityCols+`)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			string(e.ID), string(e.Type), string(props), string(e.Space), string(e.Owner), string(e.Visibility),
			p.Asserter, string(p.Method), p.Source, p.Confidence, fmtTS(p.AssertedAt), tsS, tsS,
		); err != nil {
			return fmt.Errorf("insert entity: %w", err)
		}
	case err != nil:
		return err
	default:
		// Re-assertion (an attach): UNION the candidate's JSON-LD props into the
		// stored props — existing values WIN on a key conflict, the candidate's
		// NEW keys are added — so re-asserted detail reaches the projection, not
		// just the fact log. json_patch(candidate, existing) folds deterministically
		// (shared with RebuildProjection). An empty candidate bag is a no-op.
		if len(e.Props) > 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE entities SET props=json_patch(?, props), updated_at=? WHERE id=?`,
				string(e.Props), tsS, string(e.ID)); err != nil {
				return fmt.Errorf("union props: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE entities SET updated_at=? WHERE id=?`, tsS, string(e.ID)); err != nil {
			return err
		}
	}

	for _, x := range e.ExternalIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO entity_external_ids(entity_id, scheme, value, key) VALUES(?,?,?,?)`,
			string(e.ID), x.Scheme, x.Value, x.Key()); err != nil {
			return fmt.Errorf("insert external id: %w", err)
		}
	}
	for _, emb := range e.Embeddings {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO embeddings(entity_id, model, field, dim, vector) VALUES(?,?,?,?,?)`,
			string(e.ID), emb.Model, emb.Field, len(emb.Vector), encodeVec(emb.Vector)); err != nil {
			return fmt.Errorf("insert embedding: %w", err)
		}
	}
	// Keep the full-text index in sync from the STORED props (reflects an attach's
	// json_patch union), so search and the projection fold stay consistent.
	return syncEntityFTS(ctx, tx, e.ID)
}

// appendEntityFact writes one entity.asserted fact whose payload is the snapshot
// (with its resolved id). Translates a (writer, dedupe_key) collision to
// ErrDuplicateFact.
func (s *Store) appendEntityFact(ctx context.Context, tx *sql.Tx, snapshot domain.Entity, writer domain.WriterID, dedupeKey string, actor domain.PrincipalID, ts time.Time) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return s.insertFact(ctx, tx, domain.FactEntityAsserted, string(snapshot.ID), writer, dedupeKey, payload, actor, ts)
}

func (s *Store) appendMergeFact(ctx context.Context, tx *sql.Tx, keep, drop domain.EntityID, writer domain.WriterID, dedupeKey string, actor domain.PrincipalID, ts time.Time) error {
	payload, _ := json.Marshal(struct {
		Keep domain.EntityID `json:"keep"`
		Drop domain.EntityID `json:"drop"`
	}{keep, drop})
	return s.insertFact(ctx, tx, domain.FactEntityMerged, string(drop), writer, dedupeKey, payload, actor, ts)
}

func (s *Store) insertFact(ctx context.Context, tx *sql.Tx, kind domain.FactKind, subject string, writer domain.WriterID, dedupeKey string, payload []byte, actor domain.PrincipalID, ts time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO facts(id, kind, subject, writer, dedupe_key, payload, actor, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		s.newID(), string(kind), subject, string(writer), dedupeKey, string(payload), string(actor), fmtTS(ts))
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return store.ErrDuplicateFact
	}
	return err
}

// foldFact applies one logged fact to the projection during RebuildProjection.
func (s *Store) foldFact(ctx context.Context, tx *sql.Tx, kind domain.FactKind, subject string, payload []byte, ts time.Time) error {
	switch kind {
	case domain.FactEntityAsserted:
		var e domain.Entity
		if err := json.Unmarshal(payload, &e); err != nil {
			return err
		}
		return upsertEntitySnapshot(ctx, tx, e, ts)
	case domain.FactEntityRetracted:
		_, err := tx.ExecContext(ctx, `DELETE FROM entities WHERE id=?`, subject)
		return err
	case domain.FactEdgeAsserted:
		var ed domain.Edge
		if err := json.Unmarshal(payload, &ed); err != nil {
			return err
		}
		return upsertEdgeSnapshot(ctx, tx, ed, ts)
	case domain.FactEdgeRetracted:
		_, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE id=?`, subject)
		return err
	case domain.FactEntityMerged:
		var m struct {
			Keep domain.EntityID `json:"keep"`
			Drop domain.EntityID `json:"drop"`
		}
		if err := json.Unmarshal(payload, &m); err != nil {
			return err
		}
		return mergeEntitiesTx(ctx, tx, m.Keep, m.Drop, ts)
	default:
		return fmt.Errorf("fold: unknown fact kind %q", kind)
	}
}

// mergeEntitiesTx folds `drop` into `keep`: moves external ids / embeddings,
// re-points edges, then deletes `drop`. Shared by MergeEntities and the merged-fact
// fold.
func mergeEntitiesTx(ctx context.Context, tx *sql.Tx, keep, drop domain.EntityID, ts time.Time) error {
	if keep == drop {
		return nil
	}
	// Move drop's external ids / labels / embeddings to keep by REASSIGNMENT, not
	// insert-and-cascade-delete. The old "INSERT OR IGNORE … SELECT FROM drop"
	// then "DELETE drop" lost drop's UNIQUE external keys: the insert collided
	// with drop's still-present row (ignored), then the cascade deleted it. Here we
	// first delete only the rows keep ALREADY has (the dups), then UPDATE the rest
	// onto keep (no conflict — keep lacks them and drop is the sole holder), so no
	// key is ever lost.
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM entity_external_ids WHERE entity_id=? AND key IN (SELECT key FROM entity_external_ids WHERE entity_id=?)`, []any{string(drop), string(keep)}},
		{`UPDATE entity_external_ids SET entity_id=? WHERE entity_id=?`, []any{string(keep), string(drop)}},
		{`DELETE FROM entity_labels WHERE entity_id=? AND label IN (SELECT label FROM entity_labels WHERE entity_id=?)`, []any{string(drop), string(keep)}},
		{`UPDATE entity_labels SET entity_id=? WHERE entity_id=?`, []any{string(keep), string(drop)}},
		{`DELETE FROM embeddings WHERE entity_id=? AND (model, field) IN (SELECT model, field FROM embeddings WHERE entity_id=?)`, []any{string(drop), string(keep)}},
		{`UPDATE embeddings SET entity_id=? WHERE entity_id=?`, []any{string(keep), string(drop)}},
		{`UPDATE edges SET from_id=? WHERE from_id=?`, []any{string(keep), string(drop)}},
		{`UPDATE edges SET to_id=? WHERE to_id=?`, []any{string(keep), string(drop)}},
		{`DELETE FROM entities WHERE id=?`, []any{string(drop)}},            // children already moved off drop
		{`DELETE FROM entities_fts WHERE entity_id=?`, []any{string(drop)}}, // FTS is standalone, no FK cascade
		{`UPDATE entities SET updated_at=? WHERE id=?`, []any{fmtTS(ts), string(keep)}},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.q, st.args...); err != nil {
			return fmt.Errorf("merge %s<-%s: %w", keep, drop, err)
		}
	}
	return nil
}

// MergeEntities folds two entities into one and records a merged fact.
func (s *Store) MergeEntities(ctx context.Context, keep, drop domain.EntityID, actor domain.PrincipalID, dedupeKey string) error {
	if dedupeKey == "" {
		dedupeKey = fmt.Sprintf("%s|merge|%s", keep, drop)
	}
	ts := s.now()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Both endpoints must exist, or a bad survivor id would silently delete a
	// valid entity and record a merge-to-nowhere.
	for _, id := range []domain.EntityID{keep, drop} {
		var one int
		switch err := tx.QueryRowContext(ctx, `SELECT 1 FROM entities WHERE id=?`, string(id)).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: merge endpoint %q", store.ErrNotFound, id)
		case err != nil:
			return err
		}
	}
	if err := mergeEntitiesTx(ctx, tx, keep, drop, ts); err != nil {
		return err
	}
	if err := s.appendMergeFact(ctx, tx, keep, drop, domain.WriterID(actor), dedupeKey, actor, ts); err != nil {
		return err
	}
	return tx.Commit()
}

// RetractEntity tombstones an entity: removes it from the projection (cascading
// its children and edges) and appends a retracted fact.
func (s *Store) RetractEntity(ctx context.Context, id domain.EntityID, actor domain.PrincipalID, dedupeKey string) error {
	if dedupeKey == "" {
		dedupeKey = fmt.Sprintf("%s|retract", id)
	}
	ts := s.now()
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM entities WHERE id=?`, string(id)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entities_fts WHERE entity_id=?`, string(id)); err != nil {
		return err
	}
	if err := s.insertFact(ctx, tx, domain.FactEntityRetracted, string(id), domain.WriterID(actor), dedupeKey, []byte(`{}`), actor, ts); err != nil {
		return err
	}
	return tx.Commit()
}

// GetEntity loads an entity by id, applying the access filter.
func (s *Store) GetEntity(ctx context.Context, af access.Filter, id domain.EntityID) (domain.Entity, error) {
	where, args := accessWhere(af, "entities")
	// Only a constant column list and the access WHERE fragment (itself built from
	// `?` placeholders) are concatenated; every value is passed as a parameter.
	q := `SELECT ` + entityCols + ` FROM entities WHERE id=? AND ` + where //nolint:gosec // G202: no user data in the SQL string; values are parameterized
	row := s.read.QueryRowContext(ctx, q, append([]any{string(id)}, args...)...)
	e, err := scanEntity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Entity{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Entity{}, err
	}
	if err := loadEntityChildren(ctx, s.read, &e); err != nil {
		return domain.Entity{}, err
	}
	return e, nil
}

// ResolveByExternalID returns the access-visible entities carrying any of keys.
func (s *Store) ResolveByExternalID(ctx context.Context, af access.Filter, keys []string) ([]domain.Entity, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	where, wargs := accessWhere(af, "e")
	// Constant column list + a `?`-placeholder count + the access WHERE fragment;
	// no user data enters the SQL string, all values are parameters.
	q := `SELECT DISTINCT ` + prefixCols("e") + `
		FROM entities e JOIN entity_external_ids x ON x.entity_id = e.id
		WHERE x.key IN (` + placeholders(len(keys)) + `) AND ` + where //nolint:gosec // G202: parameterized; concatenated parts are constant/placeholder-count
	args := make([]any, 0, len(keys)+len(wargs))
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, wargs...)
	return s.queryEntities(ctx, q, args)
}

// ---- read helpers ----

func externalHitsTx(ctx context.Context, tx *sql.Tx, keys []string) ([]domain.EntityID, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	//nolint:gosec // G202: only a `?`-placeholder count is concatenated; keys are parameters
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT e.id FROM entities e JOIN entity_external_ids x ON x.entity_id=e.id
		 WHERE x.key IN (`+placeholders(len(keys))+`) ORDER BY e.created_at, e.id`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []domain.EntityID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, domain.EntityID(id))
	}
	return ids, rows.Err()
}

func (s *Store) queryEntities(ctx context.Context, q string, args []any) ([]domain.Entity, error) {
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []domain.Entity
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := loadEntityChildren(ctx, s.read, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// getEntityByID reads one entity WITHOUT an access filter (internal use, inside a
// write tx).
func getEntityByID(ctx context.Context, q queryer, id domain.EntityID) (domain.Entity, error) {
	row := q.QueryRowContext(ctx, `SELECT `+entityCols+` FROM entities WHERE id=?`, string(id))
	e, err := scanEntity(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Entity{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Entity{}, err
	}
	if err := loadEntityChildren(ctx, q, &e); err != nil {
		return domain.Entity{}, err
	}
	return e, nil
}

type scanner interface{ Scan(dest ...any) error }

func scanEntity(row scanner) (domain.Entity, error) {
	var (
		e                    domain.Entity
		id, typ, space, own  string
		vis, method          string
		props                string
		asserter, source     string
		conf                 float64
		assertedAt, cAt, uAt string
	)
	if err := row.Scan(&id, &typ, &props, &space, &own, &vis,
		&asserter, &method, &source, &conf, &assertedAt, &cAt, &uAt); err != nil {
		return domain.Entity{}, err
	}
	e.ID = domain.EntityID(id)
	e.Type = domain.Type(typ)
	e.Props = json.RawMessage(props)
	e.Space = domain.SpaceID(space)
	e.Owner = domain.PrincipalID(own)
	e.Visibility = domain.Visibility(vis)
	e.Provenance = domain.Provenance{
		Asserter: asserter, Method: domain.AssertMethod(method), Source: source, Confidence: conf,
		AssertedAt: parseTS(assertedAt),
	}
	e.CreatedAt = parseTS(cAt)
	e.UpdatedAt = parseTS(uAt)
	return e, nil
}

func loadEntityChildren(ctx context.Context, q queryer, e *domain.Entity) error {
	xrows, err := q.QueryContext(ctx, `SELECT scheme, value FROM entity_external_ids WHERE entity_id=? ORDER BY key`, string(e.ID))
	if err != nil {
		return err
	}
	defer func() { _ = xrows.Close() }()
	for xrows.Next() {
		var x domain.ExternalID
		if err := xrows.Scan(&x.Scheme, &x.Value); err != nil {
			return err
		}
		e.ExternalIDs = append(e.ExternalIDs, x)
	}
	if err := xrows.Err(); err != nil {
		return err
	}

	erows, err := q.QueryContext(ctx, `SELECT model, field, vector FROM embeddings WHERE entity_id=? ORDER BY model, field`, string(e.ID))
	if err != nil {
		return err
	}
	defer func() { _ = erows.Close() }()
	for erows.Next() {
		var model, field string
		var blob []byte
		if err := erows.Scan(&model, &field, &blob); err != nil {
			return err
		}
		e.Embeddings = append(e.Embeddings, domain.Embedding{Model: model, Field: field, Vector: decodeVec(blob)})
	}
	return erows.Err()
}

func prefixCols(alias string) string {
	parts := strings.Split(entityCols, ",")
	for i := range parts {
		parts[i] = alias + "." + strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ", ")
}

// ---- small value helpers ----

func normalizeProvenance(p domain.Provenance, ts time.Time) domain.Provenance {
	if p.Method == "" {
		p.Method = domain.Asserted
	}
	if p.Confidence == 0 && p.Method == domain.Asserted {
		p.Confidence = 1.0
	}
	if p.AssertedAt.IsZero() {
		p.AssertedAt = ts
	}
	return p
}

func extractName(props json.RawMessage) string {
	if len(props) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(props, &m); err != nil {
		return ""
	}
	if n, ok := m["name"].(string); ok {
		return n
	}
	return ""
}

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}
