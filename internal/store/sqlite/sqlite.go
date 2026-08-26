// Package sqlite is the homelab / Starchart storage adapter: a pure-Go
// (modernc.org/sqlite, CGO off) implementation of store.Store. Vectors are
// float32 BLOBs searched by brute-force Go KNN — no sqlite-vec, so the static
// binary / distroless story holds.
//
// Concurrency mirrors heyarr: a single-writer connection (_txlock=immediate, so
// a write transaction takes its lock up front and the resolve→append→project
// window runs uninterrupted) plus a reader pool, both over WAL. This is why the
// adapter needs no advisory lock — the single writer already serialises the
// resolve race the Postgres adapter guards with pg_advisory_xact_lock.
package sqlite

import (
	"context"
	"database/sql"
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
	_ "modernc.org/sqlite"

	"github.com/rarebit-one/jumpdrive-index/internal/access"
	"github.com/rarebit-one/jumpdrive-index/internal/domain"
	"github.com/rarebit-one/jumpdrive-index/internal/store"
)

//go:embed all:migrations
var migrationsFS embed.FS

// Store is the SQLite adapter.
type Store struct {
	write *sql.DB // SetMaxOpenConns(1): the single writer
	read  *sql.DB // reader pool
	now   func() time.Time
	newID func() string
	th    domain.Thresholds
}

// compile-time proof the adapter satisfies the whole interface.
var _ store.Store = (*Store)(nil)

// Options configures Open. Now and NewID are injectable for deterministic tests.
type Options struct {
	Path       string
	Thresholds domain.Thresholds
	Now        func() time.Time
	NewID      func() string
}

const (
	writePragmas = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_txlock=immediate"
	readPragmas  = "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
)

// Open opens (creating if needed) the database at opts.Path. It does NOT migrate
// — migrations are a separate phase (store.Migrate), never run at boot.
func Open(opts Options) (*Store, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("%w: empty sqlite path", store.ErrInvalidInput)
	}
	if err := opts.Thresholds.Validate(); err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite", "file:"+opts.Path+writePragmas)
	if err != nil {
		return nil, fmt.Errorf("open write handle: %w", err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", "file:"+opts.Path+readPragmas)
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("open read handle: %w", err)
	}
	s := &Store{write: w, read: r, now: opts.Now, newID: opts.NewID, th: opts.Thresholds}
	if s.now == nil {
		s.now = time.Now
	}
	if s.newID == nil {
		s.newID = func() string { return uuid.Must(uuid.NewV7()).String() }
	}
	if err := s.verifyPragmas(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// verifyPragmas asserts WAL + foreign_keys actually took effect (a DSN pragma can
// silently no-op) — heyarr's boot-time posture.
func (s *Store) verifyPragmas() error {
	for name, db := range map[string]*sql.DB{"write": s.write, "read": s.read} {
		var jm string
		if err := db.QueryRow("PRAGMA journal_mode").Scan(&jm); err != nil {
			return fmt.Errorf("verify %s journal_mode: %w", name, err)
		}
		if !strings.EqualFold(jm, "wal") {
			return fmt.Errorf("verify %s: journal_mode=%q, want wal", name, jm)
		}
		var fk int
		if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
			return fmt.Errorf("verify %s foreign_keys: %w", name, err)
		}
		if fk != 1 {
			return fmt.Errorf("verify %s: foreign_keys=%d, want 1", name, fk)
		}
	}
	return nil
}

// Close closes both handles.
func (s *Store) Close() error {
	return errors.Join(s.write.Close(), s.read.Close())
}

// Migrate applies every embedded migration not yet recorded, each in its own
// transaction, in filename order. Idempotent.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.write.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var seen int
		if err := s.write.QueryRowContext(ctx,
			`SELECT 1 FROM schema_migrations WHERE version = ?`, name).Scan(&seen); err == nil {
			continue // already applied
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		body, err := migrationsFS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.write.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			name, s.tsNow()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// ProjectionHead returns the last fact seq folded into the projection. Because
// folding is synchronous (in the same transaction as the fact append), this is
// max(facts.seq) in normal operation; the boot check compares the two.
func (s *Store) ProjectionHead(ctx context.Context) (int64, error) {
	var head int64
	err := s.read.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM facts`).Scan(&head)
	return head, err
}

// RebuildProjection discards and re-folds the projection from the fact log. The
// fold is mechanical: a fact's `subject` already records which entity/edge the
// write resolved onto, so no resolve decision is re-run. It shares
// upsertEntitySnapshot / upsertEdgeSnapshot / mergeEntitiesTx with the online
// write path, so the two cannot diverge.
func (s *Store) RebuildProjection(ctx context.Context) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		`DELETE FROM edges`, `DELETE FROM embeddings`, `DELETE FROM entity_labels`,
		`DELETE FROM entity_external_ids`, `DELETE FROM entities`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("rebuild clear: %w", err)
		}
	}

	rows, err := tx.QueryContext(ctx,
		`SELECT kind, subject, payload, created_at FROM facts ORDER BY seq`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var kind, subject, payload, createdAt string
		if err := rows.Scan(&kind, &subject, &payload, &createdAt); err != nil {
			return err
		}
		ts, _ := time.Parse(time.RFC3339Nano, createdAt)
		if err := s.foldFact(ctx, tx, domain.FactKind(kind), subject, []byte(payload), ts); err != nil {
			return fmt.Errorf("rebuild fold %s/%s: %w", kind, subject, err)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- shared helpers ----

func (s *Store) tsNow() string { return s.now().UTC().Format(time.RFC3339Nano) }

func fmtTS(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// encodeVec / decodeVec serialize a float32 vector as little-endian bytes.
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

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// accessWhere compiles an access.Filter into a WHERE fragment + args for a
// table with visibility/space/owner (and, for entities, type) columns. This is
// the hard ACL as SQL SHAPE — a forbidden row is never returned. NOTE: the
// Restricted type-deny clause assumes a `type` column (entities); edge access
// filtering (Neighbors) is a later milestone.
func accessWhere(af access.Filter, alias string) (string, []any) {
	var ors []string
	var args []any
	if af.AllowPublic {
		ors = append(ors, alias+".visibility = 'public'")
	}
	if len(af.Spaces) > 0 {
		ors = append(ors, fmt.Sprintf("(%s.visibility = 'space' AND %s.space IN (%s))",
			alias, alias, placeholders(len(af.Spaces))))
		for _, sp := range af.Spaces {
			args = append(args, string(sp))
		}
	}
	ors = append(ors, fmt.Sprintf("(%s.visibility = 'private' AND %s.owner = ?)", alias, alias))
	args = append(args, string(af.Principal))

	where := "(" + strings.Join(ors, " OR ") + ")"
	if af.Restricted && len(af.DenyTypes) > 0 {
		where += fmt.Sprintf(" AND %s.type NOT IN (%s)", alias, placeholders(len(af.DenyTypes)))
		for _, t := range af.DenyTypes {
			args = append(args, string(t))
		}
	}
	return where, args
}

// queryer abstracts *sql.DB and *sql.Tx for read helpers usable in both.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
