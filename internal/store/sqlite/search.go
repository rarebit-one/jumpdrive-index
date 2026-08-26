package sqlite

import (
	"context"
	"math"
	"sort"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// cosine similarity of two equal-length vectors, in [-1,1]. Returns 0 for a
// length mismatch or a zero vector (no meaningful angle).
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// knnByModel scans every stored embedding in one model space (optionally
// constrained to a @type, optionally access-filtered), computes cosine against
// the query vector, and returns the BEST score per entity, sorted descending,
// capped at limit. At family/SME scale a brute-force scan is ample and keeps the
// binary pure-Go/CGO-off (no sqlite-vec). Access is applied in SQL BEFORE the
// scan (filter-then-rank), so a forbidden entity can never appear in results.
//
// af == nil is the internal, unfiltered form used by resolve-before-create (dedup
// must see all candidates, matching externalHitsTx which is also unfiltered).
func (s *Store) knnByModel(ctx context.Context, q queryer, model string, typ domain.Type, af *access.Filter, query []float32, limit int) ([]domain.ScoredMatch, error) {
	sqlStr := `SELECT m.entity_id, m.vector FROM embeddings m JOIN entities e ON e.id = m.entity_id WHERE m.model = ?`
	args := []any{model}
	if typ != "" {
		sqlStr += ` AND e.type = ?`
		args = append(args, string(typ))
	}
	if af != nil {
		where, wargs := accessWhere(*af, "e")
		sqlStr += ` AND ` + where //nolint:gosec // G202: only the access WHERE fragment (own `?` placeholders) is concatenated; values are parameters
		args = append(args, wargs...)
	}

	rows, err := q.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	best := make(map[domain.EntityID]float64)
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		c := cosine(query, decodeVec(blob))
		eid := domain.EntityID(id)
		if cur, ok := best[eid]; !ok || c > cur {
			best[eid] = c
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]domain.ScoredMatch, 0, len(best))
	for id, score := range best {
		out = append(out, domain.ScoredMatch{ID: id, Score: score})
	}
	// Deterministic order: score desc, then id asc to break ties stably.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SemanticSearch runs an access-filtered brute-force KNN over the query's model
// space and returns the matching entities with their cosine scores.
func (s *Store) SemanticSearch(ctx context.Context, af access.Filter, q store.VectorQuery) ([]store.ScoredEntity, error) {
	if q.Model == "" || len(q.Vector) == 0 {
		return nil, nil
	}
	matches, err := s.knnByModel(ctx, s.read, q.Model, q.Type, &af, q.Vector, q.Limit)
	if err != nil {
		return nil, err
	}
	out := make([]store.ScoredEntity, 0, len(matches))
	for _, m := range matches {
		// The entity passed the access filter inside knnByModel, so load it directly.
		e, err := getEntityByID(ctx, s.read, m.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, store.ScoredEntity{Entity: e, Score: m.Score})
	}
	return out, nil
}
