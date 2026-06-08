//go:build integration

// Slice S.3a parity tests — additive Repo column / index / FK
// extensions. Asserts the post-migration shape against the
// legacy reference schema.
package integration

import (
	"context"
	"testing"
	"time"

	"codeintel/internal/db"
	"codeintel/internal/migrate"
)

// TestMigrate_RepoAdditiveColumns asserts the three missing
// Repo columns landed with the correct shapes.
func TestMigrate_RepoAdditiveColumns(t *testing.T) {
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
		name       string
		dataType   string
		isNullable string
	}{
		{"zoektShardGroupId", "integer", "YES"},
		{"indexedCommitHash", "text", "YES"},
		{"permissionSyncedAt", "timestamp without time zone", "YES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var dataType, isNullable string
			err := pool.QueryRow(ctx, `
				SELECT data_type, is_nullable
				FROM   information_schema.columns
				WHERE  table_name = 'Repo' AND column_name = $1
			`, tc.name).Scan(&dataType, &isNullable)
			if err != nil {
				t.Fatalf("column %s lookup: %v", tc.name, err)
			}
			if dataType != tc.dataType {
				t.Errorf("data_type: got %q, want %q", dataType, tc.dataType)
			}
			if isNullable != tc.isNullable {
				t.Errorf("is_nullable: got %q, want %q", isNullable, tc.isNullable)
			}
		})
	}
}

// TestMigrate_RepoAdditiveIndexes asserts the four missing
// Repo indexes landed. Uses the legacy index names verbatim.
func TestMigrate_RepoAdditiveIndexes(t *testing.T) {
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
		name   string
		isUniq bool
	}{
		{"Repo_id_orgId_key", true},
		{"Repo_name_orgId_key", true},
		{"Repo_indexedAt_idx", false},
		{"Repo_orgId_zoektShardGroupId_idx", false},
		// Renamed from 0010's short form. Locked here so the
		// rename can't silently regress.
		{"Repo_external_id_external_codeHostUrl_orgId_key", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				isUniq bool
				count  int
			)
			err := pool.QueryRow(ctx, `
				SELECT i.indisunique, COUNT(*) OVER ()
				FROM   pg_index i
				JOIN   pg_class c ON c.oid = i.indexrelid
				WHERE  c.relname = $1
			`, tc.name).Scan(&isUniq, &count)
			if err != nil {
				t.Fatalf("index %q lookup: %v", tc.name, err)
			}
			if count != 1 {
				t.Errorf("expected exactly 1 index matching %q, got %d", tc.name, count)
			}
			if isUniq != tc.isUniq {
				t.Errorf("uniqueness: got %v, want %v", isUniq, tc.isUniq)
			}
		})
	}

	// Negative case: the 0010 wrong-name must NOT exist after S.3a.
	var staleCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM   pg_class
		WHERE  relname = 'Repo_external_id_codeHostUrl_orgId_key'
	`).Scan(&staleCount); err != nil {
		t.Fatalf("stale index lookup: %v", err)
	}
	if staleCount != 0 {
		t.Errorf("the 0010 short-name index Repo_external_id_codeHostUrl_orgId_key still exists; rename did not apply")
	}
}

// TestMigrate_RepoZoektShardGroupFK confirms the FK landed with
// the correct cascade behavior (ON UPDATE CASCADE ON DELETE SET NULL).
func TestMigrate_RepoZoektShardGroupFK(t *testing.T) {
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

	var (
		refTable             string
		onUpdate, onDelete   string
	)
	err = pool.QueryRow(ctx, `
		SELECT
		  rcl.relname,
		  con.confupdtype::text,
		  con.confdeltype::text
		FROM   pg_constraint con
		JOIN   pg_class rcl ON rcl.oid = con.confrelid
		WHERE  con.conname = 'Repo_zoektShardGroupId_fkey'
	`).Scan(&refTable, &onUpdate, &onDelete)
	if err != nil {
		t.Fatalf("FK lookup: %v", err)
	}
	if refTable != "ZoektShardGroup" {
		t.Errorf("refTable: got %q, want ZoektShardGroup", refTable)
	}
	// 'c' = CASCADE, 'n' = SET NULL per pg_constraint.confupdtype/confdeltype codes.
	if onUpdate != "c" {
		t.Errorf("onUpdate: got %q, want %q (CASCADE)", onUpdate, "c")
	}
	if onDelete != "n" {
		t.Errorf("onDelete: got %q, want %q (SET NULL)", onDelete, "n")
	}
}
