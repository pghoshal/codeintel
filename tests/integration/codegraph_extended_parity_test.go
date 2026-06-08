//go:build integration

// Slice S.8 parity tests — CodeGraph extended tables. Verifies
// each table's column count, indexes, and FK presence (incl
// the 8-column composite FK to CodeGraphIndex) match the
// legacy reference schema.
package integration

import (
	"context"
	"testing"
	"time"

	"codeintel/internal/db"
	"codeintel/internal/migrate"
)

func TestMigrate_CodeGraphExtendedColumnCounts(t *testing.T) {
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
		table string
		count int
	}{
		{"CodeGraphAnchor", 21},
		{"CodeGraphRevision", 13},
		{"CodeGraphSemanticEdge", 30},
		{"CodeGraphSemanticFact", 29},
		{"CodeGraphSemanticHyperedge", 30},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			var count int
			err := pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM information_schema.columns WHERE table_name = $1
			`, tc.table).Scan(&count)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if count != tc.count {
				t.Errorf("%s col count: got %d want %d", tc.table, count, tc.count)
			}
		})
	}
}

// TestMigrate_CodeGraphExtended8ColCompositeFKs is the
// load-bearing assertion: each of the 5 new tables has its
// 8-column composite FK to CodeGraphIndex. These FKs require
// the matching unique index on CodeGraphIndex which S.3b
// created.
func TestMigrate_CodeGraphExtended8ColCompositeFKs(t *testing.T) {
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

	// Each FK name is truncated by PostgreSQL to 63 chars
	// matching the legacy form. Each FK references 8 columns.
	fks := []string{
		"CodeGraphAnchor_graphIndexId_orgId_repoId_commitHash_provider_s",
		"CodeGraphRevision_codeGraphIndexId_orgId_repoId_commitHash_prov",
		"CodeGraphSemanticEdge_graphIndexId_orgId_repoId_commitHash_prov",
		"CodeGraphSemanticFact_graphIndexId_orgId_repoId_commitHash_prov",
		"CodeGraphSemanticHyperedge_graphIndexId_orgId_repoId_commitHash",
	}
	for _, name := range fks {
		t.Run(name, func(t *testing.T) {
			var (
				refTable      string
				keyColCount   int
				onUpdate      string
				onDelete      string
			)
			err := pool.QueryRow(ctx, `
				SELECT
				  rcl.relname,
				  array_length(con.conkey::int[], 1),
				  con.confupdtype::text,
				  con.confdeltype::text
				FROM pg_constraint con
				JOIN pg_class rcl ON rcl.oid = con.confrelid
				WHERE con.conname = $1
			`, name).Scan(&refTable, &keyColCount, &onUpdate, &onDelete)
			if err != nil {
				t.Fatalf("FK %q lookup: %v", name, err)
			}
			if refTable != "CodeGraphIndex" {
				t.Errorf("refTable: got %q want CodeGraphIndex", refTable)
			}
			if keyColCount != 8 {
				t.Errorf("FK columns: got %d want 8", keyColCount)
			}
			if onUpdate != "c" || onDelete != "c" {
				t.Errorf("cascade: got %s/%s want c/c", onUpdate, onDelete)
			}
		})
	}
}

// TestMigrate_CodeGraphExtendedRepoFKs asserts each new table's
// composite (repoId, orgId) FK to Repo(id, orgId) works (this
// relies on S.3a's Repo_id_orgId_key unique idx).
func TestMigrate_CodeGraphExtendedRepoFKs(t *testing.T) {
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

	// 3 tables use _repoId_fkey, 2 use _repoId_orgId_fkey
	// per legacy naming. Both are composite (repoId, orgId)
	// to Repo(id, orgId).
	cases := []struct {
		name string
	}{
		{"CodeGraphAnchor_repoId_fkey"},
		{"CodeGraphRevision_repoId_fkey"},
		{"CodeGraphSemanticEdge_repoId_orgId_fkey"},
		{"CodeGraphSemanticFact_repoId_orgId_fkey"},
		{"CodeGraphSemanticHyperedge_repoId_orgId_fkey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				refTable    string
				keyColCount int
				onUpdate    string
				onDelete    string
			)
			err := pool.QueryRow(ctx, `
				SELECT rcl.relname, array_length(con.conkey::int[], 1),
				       con.confupdtype::text, con.confdeltype::text
				FROM pg_constraint con
				JOIN pg_class rcl ON rcl.oid = con.confrelid
				WHERE con.conname = $1
			`, tc.name).Scan(&refTable, &keyColCount, &onUpdate, &onDelete)
			if err != nil {
				t.Fatalf("FK %q: %v", tc.name, err)
			}
			if refTable != "Repo" {
				t.Errorf("refTable: got %q want Repo", refTable)
			}
			if keyColCount != 2 {
				t.Errorf("col count: got %d want 2", keyColCount)
			}
			if onUpdate != "c" || onDelete != "c" {
				t.Errorf("cascade: got %s/%s", onUpdate, onDelete)
			}
		})
	}
}
