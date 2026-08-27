package postgres

import (
	"context"
	"strings"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

// accessWhereEdge compiles an access.Filter into a WHERE fragment for an EDGE
// alias: the same hard ACL as accessWhere but WITHOUT the @type deny-gate (which
// applies to nodes, not edges), so an edge is filtered on its own visibility.
func accessWhereEdge(af access.Filter, alias string, ab *argList) string {
	return accessClause(af, alias, false, ab)
}

// Neighbors runs a bounded, access-filtered, undirected traversal outward from a
// start entity. THE safety property: at every hop the incident-edge query filters
// BOTH the edge and the far endpoint node by the access filter, so
//
//   - a node the caller cannot see is never traversed through and never appears —
//     it cannot bridge two visible nodes, and
//   - an edge more private than its endpoints is hidden on its own (edges carry
//     their own visibility, filtered independently of the nodes).
//
// The start entity must itself be visible, or ErrNotFound is returned. maxHops is
// clamped to [1,3]; limit caps the returned entities. This mirrors the SQLite
// adapter's iterative BFS exactly (a recursive CTE would have to re-apply the
// per-hop edge AND node filter at every level anyway).
func (s *Store) Neighbors(ctx context.Context, af access.Filter, q store.NeighborQuery) (store.Subgraph, error) {
	maxHops := q.MaxHops
	if maxHops <= 0 {
		maxHops = 1
	}
	if maxHops > 3 {
		maxHops = 3
	}

	start, err := s.GetEntity(ctx, af, q.Start) // access-filtered: hidden start => ErrNotFound
	if err != nil {
		return store.Subgraph{}, err
	}

	sub := store.Subgraph{Entities: []domain.Entity{start}}
	visited := map[domain.EntityID]bool{start.ID: true}
	seenEdges := map[domain.EdgeID]bool{}
	frontier := []domain.EntityID{start.ID}

	for hop := 0; hop < maxHops && len(frontier) > 0; hop++ {
		var next []domain.EntityID
		for _, nid := range frontier {
			edges, others, err := s.incidentVisible(ctx, af, nid, q.Predicates)
			if err != nil {
				return store.Subgraph{}, err
			}
			for i := range edges {
				e := edges[i]
				if seenEdges[e.ID] {
					continue
				}
				seenEdges[e.ID] = true
				sub.Edges = append(sub.Edges, e)

				oid := others[i]
				if visited[oid] {
					continue
				}
				visited[oid] = true
				other, err := getEntityByID(ctx, s.pool, oid) // already access-checked by the join
				if err != nil {
					return store.Subgraph{}, err
				}
				sub.Entities = append(sub.Entities, other)
				next = append(next, oid)
				if q.Limit > 0 && len(sub.Entities) >= q.Limit {
					return sub, nil
				}
			}
		}
		frontier = next
	}
	return sub, nil
}

// incidentVisible returns the edges incident to nodeID (either direction) whose
// edge AND far endpoint both pass the access filter, plus the far endpoint id for
// each. Self-loops are skipped. If predicates is non-empty, only those predicates
// are returned.
func (s *Store) incidentVisible(ctx context.Context, af access.Filter, nodeID domain.EntityID, predicates []domain.Predicate) ([]domain.Edge, []domain.EntityID, error) {
	ab := &argList{}
	nidPh := ab.add(string(nodeID)) // one bound value, referenced three times below
	edgeWhere := accessWhereEdge(af, "ed", ab)
	nodeWhere := accessWhere(af, "other", ab)

	// The far endpoint is the OTHER end of the edge; the JOIN both resolves it and
	// (with nodeWhere) enforces that it is visible. Only a constant column list and
	// the two access WHERE fragments (their own $n placeholders) are concatenated;
	// every value is a bound parameter.
	sqlStr := `SELECT ` + qualifyCols(edgeCols, "ed") + ` FROM edges ed JOIN entities other ON other.id = CASE WHEN ed.from_id = ` + nidPh + ` THEN ed.to_id ELSE ed.from_id END WHERE (ed.from_id = ` + nidPh + ` OR ed.to_id = ` + nidPh + `) AND ` + edgeWhere + ` AND ` + nodeWhere //nolint:gosec // G202: parameterized; concatenated parts are constant/placeholder text

	if len(predicates) > 0 {
		ph := make([]string, len(predicates))
		for i, p := range predicates {
			ph[i] = ab.add(string(p))
		}
		sqlStr += ` AND ed.predicate IN (` + strings.Join(ph, ",") + `)`
	}

	rows, err := s.pool.Query(ctx, sqlStr, ab.vals...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var edges []domain.Edge
	var others []domain.EntityID
	for rows.Next() {
		e, err := scanEdgeRow(rows)
		if err != nil {
			return nil, nil, err
		}
		other := e.To
		if e.To == nodeID {
			other = e.From
		}
		if other == nodeID {
			continue // self-loop
		}
		edges = append(edges, e)
		others = append(others, other)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return edges, others, nil
}

// qualifyCols prefixes each column in a comma-separated list with an alias.
func qualifyCols(cols, alias string) string {
	parts := strings.Split(cols, ",")
	for i := range parts {
		parts[i] = alias + "." + strings.TrimSpace(parts[i])
	}
	return strings.Join(parts, ", ")
}
