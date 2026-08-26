package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// FullTextSearch runs an FTS5 MATCH over entity text, access-filtered
// (filter-then-rank), ranked by bm25 (best first). The returned Score is -bm25
// so higher is better, matching SemanticSearch's convention.
func (s *Store) FullTextSearch(ctx context.Context, af access.Filter, q store.TextQuery) ([]store.ScoredEntity, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	where, wargs := accessWhere(af, "e")

	//nolint:gosec // G202: only the access WHERE fragment (its own ? placeholders) and an optional type filter are concatenated; every value is a parameter
	sqlStr := `SELECT e.id, bm25(entities_fts) AS score
		FROM entities_fts JOIN entities e ON e.id = entities_fts.entity_id
		WHERE entities_fts MATCH ? AND ` + where
	args := []any{q.Text}
	args = append(args, wargs...)
	if q.Type != "" {
		sqlStr += ` AND e.type = ?`
		args = append(args, string(q.Type))
	}
	sqlStr += ` ORDER BY score LIMIT ?`
	args = append(args, limit)

	rows, err := s.read.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []store.ScoredEntity
	for rows.Next() {
		var id string
		var bm25 float64
		if err := rows.Scan(&id, &bm25); err != nil {
			return nil, err
		}
		e, err := getEntityByID(ctx, s.read, domain.EntityID(id))
		if err != nil {
			return nil, err
		}
		out = append(out, store.ScoredEntity{Entity: e, Score: -bm25})
	}
	return out, rows.Err()
}

// syncEntityFTS refreshes the FTS row for an entity from its CURRENTLY STORED
// props (not the candidate's), so an attach's json_patch union is reflected. It
// is called at the end of upsertEntitySnapshot; if the entity is gone its FTS row
// is removed. Rebuild replays it deterministically via the same upsert path.
func syncEntityFTS(ctx context.Context, tx *sql.Tx, id domain.EntityID) error {
	var props, typ string
	err := tx.QueryRowContext(ctx, `SELECT props, type FROM entities WHERE id=?`, string(id)).Scan(&props, &typ)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, derr := tx.ExecContext(ctx, `DELETE FROM entities_fts WHERE entity_id=?`, string(id))
		return derr
	case err != nil:
		return err
	}
	text := extractSearchText(json.RawMessage(props), typ)
	if _, err := tx.ExecContext(ctx, `DELETE FROM entities_fts WHERE entity_id=?`, string(id)); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO entities_fts(entity_id, text) VALUES(?,?)`, string(id), text)
	return err
}

// extractSearchText gathers the @type and every top-level string property value
// into one searchable blob. Order is irrelevant to FTS.
func extractSearchText(props json.RawMessage, typ string) string {
	parts := []string{typ}
	var m map[string]any
	if json.Unmarshal(props, &m) == nil {
		for _, v := range m {
			if str, ok := v.(string); ok && str != "" {
				parts = append(parts, str)
			}
		}
	}
	return strings.Join(parts, " ")
}
