// Package migrate applies embedded SQL migrations to a Postgres
// database. Migration files live under ./migrations and are named
// `<version>_<slug>.sql` where version is a zero-padded integer
// (e.g. `0001_initial.sql`). The runner discovers them at compile
// time via embed.FS, sorts by version, and applies any not yet
// recorded in the schema_migrations bookkeeping table.
//
// Version numbering convention: zero-padded to at least 4 digits
// so a lexicographic sort matches the numeric sort up to 9999
// migrations. Past that, add another zero-pad digit. Discover()
// also validates the prefix is a positive integer so a typo like
// `abc_thing.sql` fails fast at startup.
//
// Design choices:
//
//   - SQL-only migrations (no Go migrations) keep the surface
//     mechanical and reviewable.
//   - Per-migration transactions: each file runs inside its own
//     pgx Tx, so a syntax error rolls back without partially
//     applying.
//   - Idempotent: re-running the migrator after a successful
//     pass is a no-op because schema_migrations records every
//     applied version.
//   - Forward-only: there is no Down migration. Reverting a
//     production schema is a deliberate operator action, not a
//     casual command.
package migrate

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// schemaMigrationsTable is the bookkeeping table the runner uses
// to track applied versions. Created (if missing) on first run.
const schemaMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

// TxBeginner is the minimal pgx surface the runner needs.
// *pgxpool.Pool satisfies it directly.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Migration is one discovered file: its version (the filename
// prefix, e.g. "0001") and its raw SQL body.
type Migration struct {
	Version string
	Name    string
	SQL     string
}

// Discover returns every migration found under the embedded
// migrations directory, sorted by version. The slice is safe to
// mutate.
func Discover() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: read embedded migrations dir: %w", err)
	}
	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := fs.ReadFile(migrationFS, "migrations/"+name)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", name, err)
		}
		version, slug, ok := strings.Cut(strings.TrimSuffix(name, ".sql"), "_")
		if !ok || version == "" || slug == "" {
			return nil, fmt.Errorf("migrate: %s is not in <version>_<slug>.sql form", name)
		}
		// Reject non-integer prefixes early so the lexicographic
		// sort below cannot silently misorder a stray `abc_x.sql`.
		if _, err := strconv.Atoi(version); err != nil {
			return nil, fmt.Errorf("migrate: %s: version %q must be a positive integer: %w", name, version, err)
		}
		out = append(out, Migration{
			Version: version,
			Name:    name,
			SQL:     string(body),
		})
	}
	// Sort by numeric version so width drift past 9999 does not
	// produce silent misordering. Names that survived the
	// Atoi check above are guaranteed parseable.
	sort.Slice(out, func(i, j int) bool {
		vi, _ := strconv.Atoi(out[i].Version)
		vj, _ := strconv.Atoi(out[j].Version)
		return vi < vj
	})
	// Reject duplicate version numbers — two files claiming the
	// same version would have one silently skipped on apply.
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("migrate: duplicate version %q in files %s and %s", out[i].Version, out[i-1].Name, out[i].Name)
		}
	}
	return out, nil
}

// Apply discovers the embedded migrations, ensures the bookkeeping
// table exists, then applies any unapplied versions in order. Each
// applied version is recorded inside the same transaction as the
// SQL it ran, so a crash mid-apply leaves no half-applied state.
//
// Returns the list of versions actually applied on this call (the
// list is empty when everything was already current).
func Apply(ctx context.Context, db TxBeginner) ([]string, error) {
	migrations, err := Discover()
	if err != nil {
		return nil, err
	}
	if err := ensureBookkeepingTable(ctx, db); err != nil {
		return nil, err
	}
	applied, err := readAppliedVersions(ctx, db)
	if err != nil {
		return nil, err
	}
	var ran []string
	for _, m := range migrations {
		if _, done := applied[m.Version]; done {
			continue
		}
		if err := applyOne(ctx, db, m); err != nil {
			return ran, err
		}
		ran = append(ran, m.Version)
	}
	return ran, nil
}

func ensureBookkeepingTable(ctx context.Context, db TxBeginner) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin bookkeeping: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, schemaMigrationsTable); err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit bookkeeping: %w", err)
	}
	return nil
}

func readAppliedVersions(ctx context.Context, db TxBeginner) (map[string]struct{}, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("migrate: begin read applied: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read applied: %w", err)
	}
	defer rows.Close()
	applied := make(map[string]struct{})
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("migrate: scan applied: %w", err)
		}
		applied[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: rows applied: %w", err)
	}
	return applied, nil
}

func applyOne(ctx context.Context, db TxBeginner, m Migration) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrate: begin %s: %w", m.Name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("migrate: apply %s: %w", m.Name, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
		// A concurrent runner could have inserted the row first.
		// Postgres surfaces the unique-PK conflict as SQLSTATE
		// 23505 — treat that as success.
		if isUniqueViolation(err) {
			return tx.Commit(ctx)
		}
		return fmt.Errorf("migrate: record %s: %w", m.Name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrate: commit %s: %w", m.Name, err)
	}
	return nil
}

// isUniqueViolation reports whether err carries the Postgres
// unique-violation SQLSTATE.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
