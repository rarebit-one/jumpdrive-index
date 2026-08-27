package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

const entityCols = `id, type, props, space, owner, visibility,
	prov_asserter, prov_method, prov_source, prov_confidence, prov_asserted_at,
	created_at, updated_at`

// AppendEntityFact runs resolve-before-create and folds the result into the
// projection in one transaction. pg_advisory_xact_lock serialises the
// resolve→append→project window against concurrent writers (the PG analogue of
// SQLite's single writer).
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.ResolveResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialise the resolve window on the dedupe key (writer-scoped). The
	// separator must NOT be a NUL byte: Postgres text (and hashtext) rejects NUL,
	// so a "\x00" separator would fail every write. A "|" collision only ever
	// over-serialises two unrelated writes, which is harmless.
	lockKey := string(in.Writer) + "|" + dedupeKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1)::bigint)`, lockKey); err != nil {
		return store.ResolveResult{}, err
	}

	// Idempotency short-circuit.
	var prior string
	err = tx.QueryRow(ctx,
		`SELECT subject FROM facts WHERE writer=$1 AND dedupe_key=$2 AND kind='entity.asserted' ORDER BY seq LIMIT 1`,
		string(in.Writer), dedupeKey).Scan(&prior)
	switch {
	case err == nil:
		ent, e := getEntityByID(ctx, tx, domain.EntityID(prior))
		if e != nil {
			return store.ResolveResult{}, e
		}
		return store.ResolveResult{Entity: ent, Action: domain.ActionAttach, MatchKind: domain.MatchNone}, store.ErrDuplicateFact
	case errors.Is(err, pgx.ErrNoRows):
		// fall through
	default:
		return store.ResolveResult{}, err
	}

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
		// A review-band vector hit: create the node, but record an INFERRED sameAs?
		// edge to the near-duplicate so a human/agent can later confirm or reject the
		// identity. We never auto-merge in this band (a false merge is dear to undo);
		// the flag is a soft signal, not a decision.
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
	if err := tx.Commit(ctx); err != nil {
		return store.ResolveResult{}, err
	}
	return result, nil
}

// upsertEntitySnapshot applies one entity snapshot: INSERT on first sight, else a
// re-assertion (an attach) that UNIONs the JSON-LD props (existing keys win, the
// candidate's new keys are added — jsonb `candidate || existing`) and unions
// external ids / embeddings, bumping updated_at. Shared by the online path and
// RebuildProjection so the two fold identically. e.ID is the resolved target id.
func upsertEntitySnapshot(ctx context.Context, tx pgx.Tx, e domain.Entity, ts time.Time) error {
	tsS := fmtTS(ts)
	p := normalizeProvenance(e.Provenance, ts)
	props := e.Props
	if len(props) == 0 {
		props = json.RawMessage("{}")
	}

	var exists bool
	err := tx.QueryRow(ctx, `SELECT true FROM entities WHERE id=$1`, string(e.ID)).Scan(&exists)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `INSERT INTO entities(`+entityCols+`)
			VALUES($1,$2,$3::jsonb,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			string(e.ID), string(e.Type), string(props), string(e.Space), string(e.Owner), string(e.Visibility),
			p.Asserter, string(p.Method), p.Source, p.Confidence, fmtTS(p.AssertedAt), tsS, tsS,
		); err != nil {
			return fmt.Errorf("insert entity: %w", err)
		}
	case err != nil:
		return err
	default:
		// Attach: union props (existing wins on conflict) and bump updated_at.
		if _, err := tx.Exec(ctx,
			`UPDATE entities SET props = ($1::jsonb || props), updated_at=$2 WHERE id=$3`,
			string(props), tsS, string(e.ID)); err != nil {
			return fmt.Errorf("union entity props: %w", err)
		}
	}

	for _, x := range e.ExternalIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO entity_external_ids(entity_id, scheme, value, key) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			string(e.ID), x.Scheme, x.Value, x.Key()); err != nil {
			return fmt.Errorf("insert external id: %w", err)
		}
	}
	for _, emb := range e.Embeddings {
		// The BYTEA `vector` is the round-trip / rebuild source of truth (byte-parallel
		// with SQLite); the pgvector `embedding` is the search index, written from the
		// same float32s via a `$n::vector` text-literal cast (NULL when empty).
		var embArg any
		if len(emb.Vector) > 0 {
			embArg = vecLiteral(emb.Vector)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO embeddings(entity_id, model, field, dim, vector, embedding) VALUES($1,$2,$3,$4,$5,$6::vector)
			 ON CONFLICT (entity_id, model, field) DO UPDATE SET dim=EXCLUDED.dim, vector=EXCLUDED.vector, embedding=EXCLUDED.embedding`,
			string(e.ID), emb.Model, emb.Field, len(emb.Vector), encodeVec(emb.Vector), embArg); err != nil {
			return fmt.Errorf("insert embedding: %w", err)
		}
	}
	// Keep the full-text index in sync from the STORED props (reflects an attach's
	// jsonb-union), so search and the projection fold stay consistent — the analogue
	// of the SQLite adapter's syncEntityFTS.
	return syncEntitySearchText(ctx, tx, e.ID)
}

// syncEntitySearchText refreshes the tsvector for an entity from its CURRENTLY
// STORED props (not the candidate's), so an attach's jsonb-union is reflected. If
// the entity is gone (a merge/retract deleted it) the row — and its column — is
// already gone, so this is a no-op.
func syncEntitySearchText(ctx context.Context, tx pgx.Tx, id domain.EntityID) error {
	var props []byte
	var typ string
	err := tx.QueryRow(ctx, `SELECT props, type FROM entities WHERE id=$1`, string(id)).Scan(&props, &typ)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	text := extractSearchText(props, typ)
	_, err = tx.Exec(ctx, `UPDATE entities SET search_text = to_tsvector('simple', $1) WHERE id=$2`, text, string(id))
	return err
}

func (s *Store) appendEntityFact(ctx context.Context, tx pgx.Tx, snapshot domain.Entity, writer domain.WriterID, dedupeKey string, actor domain.PrincipalID, ts time.Time) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	return s.insertFact(ctx, tx, domain.FactEntityAsserted, string(snapshot.ID), writer, dedupeKey, payload, actor, ts)
}

func (s *Store) appendMergeFact(ctx context.Context, tx pgx.Tx, keep, drop domain.EntityID, writer domain.WriterID, dedupeKey string, actor domain.PrincipalID, ts time.Time) error {
	payload, _ := json.Marshal(struct {
		Keep domain.EntityID `json:"keep"`
		Drop domain.EntityID `json:"drop"`
	}{keep, drop})
	return s.insertFact(ctx, tx, domain.FactEntityMerged, string(drop), writer, dedupeKey, payload, actor, ts)
}

func (s *Store) insertFact(ctx context.Context, tx pgx.Tx, kind domain.FactKind, subject string, writer domain.WriterID, dedupeKey string, payload []byte, actor domain.PrincipalID, ts time.Time) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO facts(id, kind, subject, writer, dedupe_key, payload, actor, created_at)
		 VALUES($1,$2,$3,$4,$5,$6::jsonb,$7,$8)`,
		s.newID(), string(kind), subject, string(writer), dedupeKey, string(payload), string(actor), fmtTS(ts))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return store.ErrDuplicateFact
	}
	return err
}

// foldFact applies one logged fact to the projection during RebuildProjection.
func (s *Store) foldFact(ctx context.Context, tx pgx.Tx, kind domain.FactKind, subject string, payload []byte, ts time.Time) error {
	switch kind {
	case domain.FactEntityAsserted:
		var e domain.Entity
		if err := json.Unmarshal(payload, &e); err != nil {
			return err
		}
		return upsertEntitySnapshot(ctx, tx, e, ts)
	case domain.FactEntityRetracted:
		_, err := tx.Exec(ctx, `DELETE FROM entities WHERE id=$1`, subject)
		return err
	case domain.FactEdgeAsserted:
		var ed domain.Edge
		if err := json.Unmarshal(payload, &ed); err != nil {
			return err
		}
		return upsertEdgeSnapshot(ctx, tx, ed, ts)
	case domain.FactEdgeRetracted:
		_, err := tx.Exec(ctx, `DELETE FROM edges WHERE id=$1`, subject)
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

// mergeEntitiesTx folds `drop` into `keep`, PRESERVING drop's external ids /
// labels / embeddings. The order matters: first delete from drop the rows keep
// already holds (the real duplicates), THEN reassign the rest to keep (no unique
// conflict, since keep lacks them and drop is their sole holder), THEN re-point
// edges and delete drop. A naive INSERT...SELECT-then-DELETE would lose drop's
// unique keys (the insert collides with drop's still-present row and is skipped,
// then the cascade deletes it).
func mergeEntitiesTx(ctx context.Context, tx pgx.Tx, keep, drop domain.EntityID, ts time.Time) error {
	if keep == drop {
		return nil
	}
	stmts := []struct {
		q    string
		args []any
	}{
		// external ids
		{`DELETE FROM entity_external_ids d WHERE d.entity_id=$1
		   AND EXISTS (SELECT 1 FROM entity_external_ids k WHERE k.entity_id=$2 AND k.key=d.key)`, []any{string(drop), string(keep)}},
		{`UPDATE entity_external_ids SET entity_id=$1 WHERE entity_id=$2`, []any{string(keep), string(drop)}},
		// labels
		{`DELETE FROM entity_labels d WHERE d.entity_id=$1
		   AND EXISTS (SELECT 1 FROM entity_labels k WHERE k.entity_id=$2 AND k.label=d.label)`, []any{string(drop), string(keep)}},
		{`UPDATE entity_labels SET entity_id=$1 WHERE entity_id=$2`, []any{string(keep), string(drop)}},
		// embeddings
		{`DELETE FROM embeddings d WHERE d.entity_id=$1
		   AND EXISTS (SELECT 1 FROM embeddings k WHERE k.entity_id=$2 AND k.model=d.model AND k.field=d.field)`, []any{string(drop), string(keep)}},
		{`UPDATE embeddings SET entity_id=$1 WHERE entity_id=$2`, []any{string(keep), string(drop)}},
		// edges re-point to keep
		{`UPDATE edges SET from_id=$1 WHERE from_id=$2`, []any{string(keep), string(drop)}},
		{`UPDATE edges SET to_id=$1 WHERE to_id=$2`, []any{string(keep), string(drop)}},
		// drop the merged entity, then touch keep
		{`DELETE FROM entities WHERE id=$1`, []any{string(drop)}},
		{`UPDATE entities SET updated_at=$1 WHERE id=$2`, []any{fmtTS(ts), string(keep)}},
	}
	for _, st := range stmts {
		if _, err := tx.Exec(ctx, st.q, st.args...); err != nil {
			return fmt.Errorf("merge %s<-%s: %w", keep, drop, err)
		}
	}
	return nil
}

// MergeEntities folds two entities into one, validating BOTH endpoints exist
// first (a merge to/from a missing node is ErrNotFound, never a silent delete),
// and records a merged fact.
func (s *Store) MergeEntities(ctx context.Context, keep, drop domain.EntityID, actor domain.PrincipalID, dedupeKey string) error {
	if dedupeKey == "" {
		dedupeKey = fmt.Sprintf("%s|merge|%s", keep, drop)
	}
	ts := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, id := range []domain.EntityID{keep, drop} {
		var ok bool
		err := tx.QueryRow(ctx, `SELECT true FROM entities WHERE id=$1`, string(id)).Scan(&ok)
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
	}

	if err := mergeEntitiesTx(ctx, tx, keep, drop, ts); err != nil {
		return err
	}
	if err := s.appendMergeFact(ctx, tx, keep, drop, domain.WriterID(actor), dedupeKey, actor, ts); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RetractEntity tombstones an entity (cascading its children and edges) and
// appends a retracted fact.
func (s *Store) RetractEntity(ctx context.Context, id domain.EntityID, actor domain.PrincipalID, dedupeKey string) error {
	if dedupeKey == "" {
		dedupeKey = fmt.Sprintf("%s|retract", id)
	}
	ts := s.now()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `DELETE FROM entities WHERE id=$1`, string(id)); err != nil {
		return err
	}
	if err := s.insertFact(ctx, tx, domain.FactEntityRetracted, string(id), domain.WriterID(actor), dedupeKey, []byte(`{}`), actor, ts); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetEntity loads an entity by id, applying the access filter.
func (s *Store) GetEntity(ctx context.Context, af access.Filter, id domain.EntityID) (domain.Entity, error) {
	ab := &argList{}
	idPh := ab.add(string(id))
	where := accessWhere(af, "entities", ab)
	q := `SELECT ` + entityCols + ` FROM entities WHERE id=` + idPh + ` AND ` + where //nolint:gosec // G202: constant column list + access WHERE fragment (own $n placeholders); values are parameters
	e, err := scanEntityRow(s.pool.QueryRow(ctx, q, ab.vals...))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Entity{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Entity{}, err
	}
	if err := loadEntityChildren(ctx, s.pool, &e); err != nil {
		return domain.Entity{}, err
	}
	return e, nil
}

// ResolveByExternalID returns the access-visible entities carrying any of keys.
func (s *Store) ResolveByExternalID(ctx context.Context, af access.Filter, keys []string) ([]domain.Entity, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ab := &argList{}
	ph := make([]string, len(keys))
	for i, k := range keys {
		ph[i] = ab.add(k)
	}
	where := accessWhere(af, "e", ab)
	q := `SELECT DISTINCT ` + prefixCols("e") + `
		FROM entities e JOIN entity_external_ids x ON x.entity_id = e.id
		WHERE x.key IN (` + strings.Join(ph, ",") + `) AND ` + where //nolint:gosec // G202: placeholder lists + access WHERE fragment; values are parameters
	return s.queryEntities(ctx, q, ab.vals)
}

// ---- read helpers ----

func externalHitsTx(ctx context.Context, tx pgx.Tx, keys []string) ([]domain.EntityID, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ab := &argList{}
	ph := make([]string, len(keys))
	for i, k := range keys {
		ph[i] = ab.add(k)
	}
	q := `SELECT DISTINCT e.id, e.created_at FROM entities e JOIN entity_external_ids x ON x.entity_id=e.id
		WHERE x.key IN (` + strings.Join(ph, ",") + `) ORDER BY e.created_at, e.id` //nolint:gosec // G202: placeholder list only; keys are parameters
	rows, err := tx.Query(ctx, q, ab.vals...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []domain.EntityID
	for rows.Next() {
		var id, createdAt string
		if err := rows.Scan(&id, &createdAt); err != nil {
			return nil, err
		}
		ids = append(ids, domain.EntityID(id))
	}
	return ids, rows.Err()
}

func (s *Store) queryEntities(ctx context.Context, q string, args []any) ([]domain.Entity, error) {
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var out []domain.Entity
	for rows.Next() {
		e, err := scanEntityRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := loadEntityChildren(ctx, s.pool, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func getEntityByID(ctx context.Context, q querier, id domain.EntityID) (domain.Entity, error) {
	e, err := scanEntityRow(q.QueryRow(ctx, `SELECT `+entityCols+` FROM entities WHERE id=$1`, string(id)))
	if errors.Is(err, pgx.ErrNoRows) {
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

type rowScanner interface{ Scan(dest ...any) error }

func scanEntityRow(row rowScanner) (domain.Entity, error) {
	var (
		e                    domain.Entity
		id, typ, space, own  string
		vis, method          string
		props                []byte
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

func loadEntityChildren(ctx context.Context, q querier, e *domain.Entity) error {
	xrows, err := q.Query(ctx, `SELECT scheme, value FROM entity_external_ids WHERE entity_id=$1 ORDER BY key`, string(e.ID))
	if err != nil {
		return err
	}
	for xrows.Next() {
		var x domain.ExternalID
		if err := xrows.Scan(&x.Scheme, &x.Value); err != nil {
			xrows.Close()
			return err
		}
		e.ExternalIDs = append(e.ExternalIDs, x)
	}
	xrows.Close()
	if err := xrows.Err(); err != nil {
		return err
	}

	erows, err := q.Query(ctx, `SELECT model, field, vector FROM embeddings WHERE entity_id=$1 ORDER BY model, field`, string(e.ID))
	if err != nil {
		return err
	}
	for erows.Next() {
		var model, field string
		var blob []byte
		if err := erows.Scan(&model, &field, &blob); err != nil {
			erows.Close()
			return err
		}
		e.Embeddings = append(e.Embeddings, domain.Embedding{Model: model, Field: field, Vector: decodeVec(blob)})
	}
	erows.Close()
	return erows.Err()
}

func prefixCols(alias string) string {
	parts := strings.Split(entityCols, ",")
	for i := range parts {
		parts[i] = alias + "." + strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ", ")
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
