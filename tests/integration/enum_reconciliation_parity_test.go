//go:build integration

// Slice S.3b parity tests — destructive enum-column reconciliation
// on existing tables. Asserts the eight columns (CodeGraphIndex
// provider/status, CodeIntelIndex kind/status, Repo
// external_codeHostType/latestIndexingJobStatus, RepoIndexingJob
// type/status) have the expected enum types after migrations,
// and the codeintel-specific indexes not in legacy are gone.
package integration

import (
	"context"
	"testing"
	"time"

	"codeintel/internal/db"
	"codeintel/internal/migrate"
)

// TestMigrate_S3b_EnumColumns asserts the eight enum-converted
// columns carry their proper enum types post-migration.
func TestMigrate_S3b_EnumColumns(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	cases := []struct {
		table  string
		column string
		udt    string
	}{
		{"CodeGraphIndex", "provider", "CodeGraphProvider"},
		{"CodeGraphIndex", "status", "CodeGraphIndexStatus"},
		{"CodeIntelIndex", "kind", "CodeIntelIndexKind"},
		{"CodeIntelIndex", "status", "CodeIntelIndexStatus"},
		{"Repo", "external_codeHostType", "CodeHostType"},
		{"Repo", "latestIndexingJobStatus", "RepoIndexingJobStatus"},
		{"RepoIndexingJob", "type", "RepoIndexingJobType"},
		{"RepoIndexingJob", "status", "RepoIndexingJobStatus"},
	}
	for _, tc := range cases {
		t.Run(tc.table+"."+tc.column, func(t *testing.T) {
			var udt string
			err := pool.QueryRow(ctx, `
				SELECT udt_name FROM information_schema.columns
				WHERE table_name = $1 AND column_name = $2
			`, tc.table, tc.column).Scan(&udt)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if udt != tc.udt {
				t.Errorf("udt_name: got %q, want %q (column still text-typed, cast did not apply)", udt, tc.udt)
			}
		})
	}
}

// TestMigrate_S3b_MissingCodeGraphIndexIndexes asserts the 7
// previously-missing indexes (incl the 8-column unique that
// S.8 FKs to) now exist on CodeGraphIndex.
func TestMigrate_S3b_MissingCodeGraphIndexIndexes(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	expected := []struct {
		name   string
		isUniq bool
	}{
		{"CodeGraphIndex_id_orgId_repoId_commitHash_key", true},
		{"CodeGraphIndex_id_orgId_repoId_commitHash_provider_schemaVersio", true},
		{"CodeGraphIndex_id_orgId_repoId_key", true},
		{"CodeGraphIndex_id_orgId_repoId_provider_schemaVersion_builderVe", true},
		{"CodeGraphIndex_deleteAfter_status_idx", false},
		{"CodeGraphIndex_orgId_repoId_commitHash_idx", false},
		{"CodeGraphIndex_orgId_repoId_status_idx", false},
	}
	for _, tc := range expected {
		t.Run(tc.name, func(t *testing.T) {
			var (
				isUniq bool
				count  int
			)
			err := pool.QueryRow(ctx, `
				SELECT i.indisunique, COUNT(*) OVER ()
				FROM pg_index i
				JOIN pg_class c ON c.oid = i.indexrelid
				WHERE c.relname = $1
			`, tc.name).Scan(&isUniq, &count)
			if err != nil {
				t.Fatalf("idx %q lookup: %v", tc.name, err)
			}
			if count != 1 {
				t.Errorf("expected 1 match, got %d", count)
			}
			if isUniq != tc.isUniq {
				t.Errorf("isunique: got %v want %v", isUniq, tc.isUniq)
			}
		})
	}
}

// TestMigrate_S3b_CodeIntelIndexMissingIndex asserts the
// orgId_repoId_revision index landed.
func TestMigrate_S3b_CodeIntelIndexMissingIndex(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_class WHERE relname = 'CodeIntelIndex_orgId_repoId_revision_idx'
	`).Scan(&count); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if count != 1 {
		t.Errorf("CodeIntelIndex_orgId_repoId_revision_idx missing")
	}
}

// TestMigrate_S3b_DroppedCodeintelOnlyIndexes asserts the
// 8 codeintel-specific indexes that diverged from legacy are
// dropped (including the partial RepoIndexingJob_failed_updatedAt_idx
// that was blocking the enum cast).
func TestMigrate_S3b_DroppedCodeintelOnlyIndexes(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}
	dropped := []string{
		"RepoIndexingJob_failed_updatedAt_idx",
		"RepoIndexingJob_repoId_createdAt_idx",
		"CodeGraphIndex_repoId_updatedAt_idx",
		"Connection_orgId_idx",
		"ApiKey_orgId_idx",
		"ConnectionSyncJob_connectionId_createdAt_idx",
		"RepoToConnection_connectionId_idx",
	}
	for _, name := range dropped {
		t.Run(name, func(t *testing.T) {
			var count int
			if err := pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM pg_class WHERE relname = $1
			`, name).Scan(&count); err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if count != 0 {
				t.Errorf("idx %q still exists; drop did not apply", name)
			}
		})
	}
}
