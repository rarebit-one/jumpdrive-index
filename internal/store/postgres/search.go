package postgres

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// vecLiteral formats a float32 slice as a pgvector text literal ("[1,0.5]"), so a
// bound TEXT parameter cast `$n::vector` needs no pgvector Go dependency. An empty
// slice yields "[]", but callers pass NULL for absent vectors rather than this.
func vecLiteral(v []float32) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, f := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(f), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// knnByModel ranks stored embeddings in ONE model space by pgvector cosine
// distance to the query, returning the BEST (nearest) score per entity, sorted
// best-first, capped at limit. pgvector's `<=>` is cosine DISTANCE (1 - cosine
// similarity); the returned Score is `1 - distance`, so it matches SQLite's Go
// cosine (higher is better). Filtering by model FIRST guarantees every compared
// vector shares one dimension (a model space is "name@dim"), so an unmodified
// `vector` column is safe. Access is applied in SQL BEFORE the rank
// (filter-then-rank); af == nil is the internal, unfiltered form used by
// resolve-before-create (dedup must see all candidates, like externalHitsTx).
func (s *Store) knnByModel(ctx context.Context, q querier, model string, typ domain.Type, af *access.Filter, query []float32, limit int) ([]domain.ScoredMatch, error) {
	ab := &argList{}
	qPh := ab.add(vecLiteral(query))
	modelPh := ab.add(model)
	// Only constant text, the `$n::vector` cast and the access WHERE fragment (its
	// own $n placeholders) are concatenated; every value is a bound parameter.
	sqlStr := `SELECT m.entity_id, MIN(m.embedding <=> ` + qPh + `::vector) AS dist
		FROM embeddings m JOIN entities e ON e.id = m.entity_id
		WHERE m.model = ` + modelPh + ` AND m.embedding IS NOT NULL`
	if typ != "" {
		sqlStr += ` AND e.type = ` + ab.add(string(typ))
	}
	if af != nil {
		sqlStr += ` AND ` + accessWhere(*af, "e", ab)
	}
	sqlStr += ` GROUP BY m.entity_id ORDER BY dist, m.entity_id`
	if limit > 0 {
		sqlStr += ` LIMIT ` + ab.add(limit)
	}

	rows, err := q.Query(ctx, sqlStr, ab.vals...) //nolint:gosec // G202: parameterized; concatenated parts are constant text + $n placeholders
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ScoredMatch
	for rows.Next() {
		var id string
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, err
		}
		score := 1 - dist
		if score < 0 {
			score = 0
		}
		if score > 1 {
			score = 1
		}
		out = append(out, domain.ScoredMatch{ID: domain.EntityID(id), Score: score})
	}
	return out, rows.Err()
}

// SemanticSearch runs an access-filtered pgvector KNN over the query's model space
// and returns the matching entities with their cosine scores.
func (s *Store) SemanticSearch(ctx context.Context, af access.Filter, q store.VectorQuery) ([]store.ScoredEntity, error) {
	if q.Model == "" || len(q.Vector) == 0 {
		return nil, nil
	}
	matches, err := s.knnByModel(ctx, s.pool, q.Model, q.Type, &af, q.Vector, q.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]store.ScoredEntity, 0, len(matches))
	for _, m := range matches {
		// The entity passed the access filter inside knnByModel, so load it directly.
		e, err := getEntityByID(ctx, s.pool, m.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, store.ScoredEntity{Entity: e, Score: m.Score})
	}
	return out, nil
}

// FullTextSearch runs a tsvector query over entity text, access-filtered
// (filter-then-rank), ranked by ts_rank_cd (best first). The returned Score is the
// rank (higher is better), matching SemanticSearch's convention.
func (s *Store) FullTextSearch(ctx context.Context, af access.Filter, q store.TextQuery) ([]store.ScoredEntity, error) {
	if strings.TrimSpace(q.Text) == "" {
		return nil, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 20
	}
	ab := &argList{}
	qPh := ab.add(q.Text)
	where := accessWhere(af, "e", ab)
	// Only constant text, the plainto_tsquery calls (bound $n) and the access WHERE
	// fragment are concatenated; every value is a bound parameter.
	sqlStr := `SELECT e.id, ts_rank_cd(e.search_text, plainto_tsquery('simple', ` + qPh + `)) AS score
		FROM entities e
		WHERE e.search_text @@ plainto_tsquery('simple', ` + qPh + `) AND ` + where
	if q.Type != "" {
		sqlStr += ` AND e.type = ` + ab.add(string(q.Type))
	}
	sqlStr += ` ORDER BY score DESC, e.id LIMIT ` + ab.add(limit)

	rows, err := s.pool.Query(ctx, sqlStr, ab.vals...) //nolint:gosec // G202: parameterized; concatenated parts are constant text + $n placeholders
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.ScoredEntity
	for rows.Next() {
		var id string
		var score float64
		if err := rows.Scan(&id, &score); err != nil {
			return nil, err
		}
		e, err := getEntityByID(ctx, s.pool, domain.EntityID(id))
		if err != nil {
			return nil, err
		}
		out = append(out, store.ScoredEntity{Entity: e, Score: score})
	}
	return out, rows.Err()
}

// extractSearchText gathers the @type and every top-level string property value
// into one searchable blob (order is irrelevant to the tsvector), mirroring the
// SQLite adapter's FTS text exactly.
func extractSearchText(props []byte, typ string) string {
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
