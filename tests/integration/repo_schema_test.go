//go:build integration

// Live-Postgres integration test for migration 0010 — the Repo
// schema extension the Phase B.1d connection-sync worker depends
// on. Asserts every column + the matching unique index actually
// exists in the database after migrate.Apply runs against a real
// Postgres.
//
// Operator workflow:
//
//	export CODEINTEL_TEST_POSTGRES_URL="postgres://USER:PASS@127.0.0.1:5433/codeintel?sslmode=disable"
//	cd codeintel && go test -tags=integration ./tests/integration/...
//
// What this test proves:
//
//   - The 0010 migration applies cleanly against a freshly-bootstrapped
//     codeintel database (every prior migration also applies).
//   - The legacy Repo columns the worker will write
//     (cloneUrl / external_id / external_codeHostUrl / metadata /
//     isPublic / isAutoCleanupDisabled) all exist with the correct
//     SQL types + nullability.
//   - The (external_id, external_codeHostUrl, orgId) unique index
//     exists. This index is what the worker's per-repo upsert
//     keys on.
//
// What this test does NOT prove:
//
//   - The eventual NOT NULL tightening for the cloneUrl /
//     external_id / external_codeHostUrl / metadata columns — that
//     lands in a follow-up migration after Phase B.1d has been
//     observed setting them on every write.
//   - That the worker actually writes to these columns (Phase B.1d
//     is the slice where that wires up).
package integration

import (
	"context"
	"testing"
	"time"

	"codeintel/internal/db"
	"codeintel/internal/migrate"
)

// expectedRepoColumn captures the legacy Prisma schema's column
// shape for each column the Phase B.1 worker depends on. The
// information_schema.columns row for each must match.
type expectedRepoColumn struct {
	name       string
	dataType   string // information_schema.columns.data_type value
	isNullable string // "YES" or "NO"
}

// TestMigrate_AppliesAllMigrationsAgainstLivePostgres is the
// happy-path E2E gate for the migration runner — proves that
// every embedded migration applies in order against a real
// Postgres without error.
func TestMigrate_AppliesAllMigrationsAgainstLivePostgres(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                    dsn,
		AllowInsecureRemoteDSN: true,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	applied, err := migrate.Apply(ctx, pool)
	if err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}
	t.Logf("migrate.Apply ran %d migrations: %v", len(applied), applied)

	// A second Apply on the same DB must be a no-op (idempotency).
	applied2, err := migrate.Apply(ctx, pool)
	if err != nil {
		t.Fatalf("migrate.Apply (second run): %v", err)
	}
	if len(applied2) != 0 {
		t.Errorf("second Apply ran %d migrations, want 0 (idempotency)", len(applied2))
	}
}

// TestMigrate_RepoConnectionSyncColumns asserts every Repo column
// the connection-sync worker writes exists with the correct shape
// after migrations have applied. Lock for Phase B.1d.
func TestMigrate_RepoConnectionSyncColumns(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                    dsn,
		AllowInsecureRemoteDSN: true,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	// Apply migrations (idempotent if already applied).
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	// Per the legacy Prisma schema (packages/db/prisma/schema.prisma
	// model Repo, lines 55-58 + 74-76). Columns added by 0010 are
	// nullable for now (worker fills them on insert; NOT NULL
	// tightening is a follow-up after Phase B.1d).
	expected := []expectedRepoColumn{
		// Originally landed as NULLABLE in B.1a (0010). Slice S.3c
		// (0023) tightened to NOT NULL once the deferral was safe.
		{"cloneUrl", "text", "NO"},
		{"external_id", "text", "NO"},
		{"external_codeHostUrl", "text", "NO"},
		{"metadata", "jsonb", "NO"},
		{"isPublic", "boolean", "NO"},
		{"isAutoCleanupDisabled", "boolean", "NO"},
	}

	for _, want := range expected {
		t.Run(want.name, func(t *testing.T) {
			var dataType, isNullable string
			err := pool.QueryRow(ctx, `
				SELECT data_type, is_nullable
				FROM information_schema.columns
				WHERE table_name = 'Repo' AND column_name = $1
			`, want.name).Scan(&dataType, &isNullable)
			if err != nil {
				t.Fatalf("column %q not found in Repo: %v", want.name, err)
			}
			if dataType != want.dataType {
				t.Errorf("column %q: data_type got %q, want %q",
					want.name, dataType, want.dataType)
			}
			if isNullable != want.isNullable {
				t.Errorf("column %q: is_nullable got %q, want %q",
					want.name, isNullable, want.isNullable)
			}
		})
	}
}

// TestMigrate_RepoExternalIdUniqueIndex confirms the
// (external_id, external_codeHostUrl, orgId) unique index exists
// — the worker's per-repo upsert keys on this tuple.
func TestMigrate_RepoExternalIdUniqueIndex(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                    dsn,
		AllowInsecureRemoteDSN: true,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	// Index landed under the short name in 0010, then renamed to
	// the legacy-shaped `_external_codeHostUrl_` form in 0013 (S.3a).
	const expectedIndexName = "Repo_external_id_external_codeHostUrl_orgId_key"
	var (
		isUnique bool
		colCount int
	)
	err = pool.QueryRow(ctx, `
		SELECT i.indisunique, array_length(i.indkey::int[], 1)
		FROM   pg_index i
		JOIN   pg_class c ON c.oid = i.indexrelid
		WHERE  c.relname = $1
	`, expectedIndexName).Scan(&isUnique, &colCount)
	if err != nil {
		t.Fatalf("index %q not found: %v", expectedIndexName, err)
	}
	if !isUnique {
		t.Errorf("index %q exists but is not UNIQUE", expectedIndexName)
	}
	if colCount != 3 {
		t.Errorf("index %q covers %d columns, want 3 (external_id, external_codeHostUrl, orgId)",
			expectedIndexName, colCount)
	}
}
