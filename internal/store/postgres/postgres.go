// Package postgres is the hosted storage adapter: a pgx/v5 + pgxpool
// implementation of store.Store that passes the SAME conformance matrix as the
// SQLite adapter, proving the storage seam. It mirrors the SQLite semantics
// exactly; the divergences are dialect (jsonb/bytea/bigserial, $n placeholders)
// and concurrency (pg_advisory_xact_lock serialises the resolve→append→project
// window, where SQLite relies on its single writer).
//
// This is the CORE: append/resolve/get/merge/retract-entity/rebuild + the
// access-filtered reads. Vector search, full-text, traversal, edge retraction
// and the governed-write path are store.ErrNotImplemented stubs (the conformance
// suite skips them) and land in follow-ups.
package postgres

import (
	"context"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

//go:embed all:migrations
var migrationsFS embed.FS

// Store is the Postgres adapter.
type Store struct {
	pool  *pgxpool.Pool
	now   func() time.Time
	newID func() string
	th    domain.Thresholds
}

var _ store.Store = (*Store)(nil)

// Options configures Open. Now and NewID are injectable for deterministic tests.
type Options struct {
	DSN        string
	Thresholds domain.Thresholds
	Now        func() time.Time
	NewID      func() string
}

// Open connects a pool to the database at opts.DSN. It does NOT migrate —
// migrations are a separate phase (store.Migrate).
func Open(ctx context.Context, opts Options) (*Store, error) {
	if opts.DSN == "" {
		return nil, fmt.Errorf("%w: empty postgres DSN", store.ErrInvalidInput)
	}
	if err := opts.Thresholds.Validate(); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	s := &Store{pool: pool, now: opts.Now, newID: opts.NewID, th: opts.Thresholds}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = func() string { return uuid.Must(uuid.NewV7()).String() }
	}
	return s, nil
}

// Close closes the pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// Migrate applies every embedded migration not yet recorded, each in its own
// transaction, in filename order. The migration bodies are executed with no bound
// arguments, so pgx uses the simple query protocol and multi-statement DDL runs
// as one command.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var seen bool
		err := s.pool.QueryRow(ctx, `SELECT true FROM schema_migrations WHERE version = $1`, name).Scan(&seen)
		if err == nil {
			continue // already applied
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		body, err := migrationsFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)`, name, s.tsNow()); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// ProjectionHead returns the last fact seq folded into the projection. Folding is
// synchronous (same tx as the append), so this is max(facts.seq).
func (s *Store) ProjectionHead(ctx context.Context) (int64, error) {
	var head int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM facts`).Scan(&head)
	return head, err
}

// RebuildProjection discards and re-folds the projection from the fact log. The
// fold is mechanical — a fact's subject records the resolved id — and shares
// upsertEntitySnapshot / upsertEdgeSnapshot / mergeEntitiesTx with the online
// write path, so the two cannot diverge.
func (s *Store) RebuildProjection(ctx context.Context) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, stmt := range []string{
		`DELETE FROM edges`, `DELETE FROM embeddings`, `DELETE FROM entity_labels`,
		`DELETE FROM entity_external_ids`, `DELETE FROM entities`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("rebuild clear: %w", err)
		}
	}

	rows, err := tx.Query(ctx, `SELECT kind, subject, payload, created_at FROM facts ORDER BY seq`)
	if err != nil {
		return err
	}
	type factRow struct {
		kind, subject, createdAt string
		payload                  []byte
	}
	var facts []factRow
	for rows.Next() {
		var fr factRow
		if err := rows.Scan(&fr.kind, &fr.subject, &fr.payload, &fr.createdAt); err != nil {
			rows.Close()
			return err
		}
		facts = append(facts, fr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, fr := range facts {
		ts, _ := time.Parse(time.RFC3339Nano, fr.createdAt)
		if err := s.foldFact(ctx, tx, domain.FactKind(fr.kind), fr.subject, fr.payload, ts); err != nil {
			return fmt.Errorf("rebuild fold %s/%s: %w", fr.kind, fr.subject, err)
		}
	}
	return tx.Commit(ctx)
}

// ---- shared helpers ----

func (s *Store) tsNow() string { return s.now().UTC().Format(time.RFC3339Nano) }

func fmtTS(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTS(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

func encodeVec(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// argList builds $n placeholders while accumulating their values, so dynamic
// queries (the access clause, IN lists) number their parameters correctly.
type argList struct{ vals []any }

func (a *argList) add(v any) string {
	a.vals = append(a.vals, v)
	return fmt.Sprintf("$%d", len(a.vals))
}

// accessWhere compiles an access.Filter into a WHERE fragment for an ENTITY
// alias (the hard ACL as SQL SHAPE), appending its parameters to ab. The
// edge-alias variant (no @type gate) lands with the Neighbors traversal.
func accessWhere(af access.Filter, alias string, ab *argList) string {
	return accessClause(af, alias, true, ab)
}

func accessClause(af access.Filter, alias string, gateTypes bool, ab *argList) string {
	var ors []string
	if af.AllowPublic {
		ors = append(ors, alias+".visibility = 'public'")
	}
	if len(af.Spaces) > 0 {
		ph := make([]string, len(af.Spaces))
		for i, sp := range af.Spaces {
			ph[i] = ab.add(string(sp))
		}
		ors = append(ors, fmt.Sprintf("(%s.visibility = 'space' AND %s.space IN (%s))", alias, alias, strings.Join(ph, ",")))
	}
	ors = append(ors, fmt.Sprintf("(%s.visibility = 'private' AND %s.owner = %s)", alias, alias, ab.add(string(af.Principal))))

	where := "(" + strings.Join(ors, " OR ") + ")"
	if gateTypes && af.Restricted && len(af.DenyTypes) > 0 {
		ph := make([]string, len(af.DenyTypes))
		for i, t := range af.DenyTypes {
			ph[i] = ab.add(string(t))
		}
		where += fmt.Sprintf(" AND %s.type NOT IN (%s)", alias, strings.Join(ph, ","))
	}
	return where
}

// querier abstracts *pgxpool.Pool and pgx.Tx for read helpers usable in both.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

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
