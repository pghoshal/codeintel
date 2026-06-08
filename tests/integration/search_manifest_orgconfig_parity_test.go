//go:build integration

// Slice S.10 parity tests — SearchContext + _RepoToSearchContext
// + OrgCodeIntelConfig + RepoIndexManifest +
// RepoIndexManifestFile + RepoSemanticChunkManifest.
//
// Verifies each table's column count, indexes, and FK cascade
// behavior matches the legacy reference schema. Per-column
// shape diffs are governed by 0019_search_manifest_orgconfig.sql
// (the canonical spec); these tests guard against migration
// drift.
package integration

import (
	"context"
	"reflect"
	"sort"
	"testing"
	"time"

	"codeintel/internal/db"
	"codeintel/internal/migrate"
)

func TestMigrate_S10ColumnCounts(t *testing.T) {
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
		{"SearchContext", 6},
		{"_RepoToSearchContext", 2},
		{"OrgCodeIntelConfig", 6},
		{"RepoIndexManifest", 28},
		{"RepoIndexManifestFile", 11},
		{"RepoSemanticChunkManifest", 13},
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

func TestMigrate_S10Indexes(t *testing.T) {
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

	expected := map[string][]indexSpec{
		"SearchContext": {
			{"SearchContext_pkey", true},
			{"SearchContext_name_orgId_key", true},
		},
		"_RepoToSearchContext": {
			{"_RepoToSearchContext_AB_pkey", true},
			{"_RepoToSearchContext_B_index", false},
		},
		"OrgCodeIntelConfig": {
			{"OrgCodeIntelConfig_pkey", true},
			{"OrgCodeIntelConfig_orgId_idx", false},
			{"OrgCodeIntelConfig_orgId_key", true},
		},
		"RepoIndexManifest": {
			{"RepoIndexManifest_pkey", true},
			{"RepoIndexManifest_indexJobId_idx", false},
			{"RepoIndexManifest_orgId_workspaceId_repoId_branch_status_idx", false},
			{"RepoIndexManifest_repoId_workspaceId_branch_commitHash_idx", false},
			{"RepoIndexManifest_status_createdAt_idx", false},
		},
		"RepoIndexManifestFile": {
			{"RepoIndexManifestFile_pkey", true},
			{"RepoIndexManifestFile_manifestId_contentHash_idx", false},
			{"RepoIndexManifestFile_manifestId_path_key", true},
			{"RepoIndexManifestFile_path_idx", false},
		},
		"RepoSemanticChunkManifest": {
			{"RepoSemanticChunkManifest_pkey", true},
			{"RepoSemanticChunkManifest_contentHash_promptVersion_modelId_sch", false},
			{"RepoSemanticChunkManifest_manifestId_filePath_idx", false},
			{"RepoSemanticChunkManifest_manifestId_filePath_startLine_endLine", true},
		},
	}
	for table, want := range expected {
		t.Run(table, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT c.relname, i.indisunique FROM pg_index i
				JOIN pg_class c ON c.oid = i.indexrelid
				JOIN pg_class t ON t.oid = i.indrelid WHERE t.relname = $1
			`, table)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var got []indexSpec
			for rows.Next() {
				var s indexSpec
				if err := rows.Scan(&s.name, &s.isUniq); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, s)
			}
			sort.Slice(got, func(i, j int) bool { return got[i].name < got[j].name })
			sort.Slice(want, func(i, j int) bool { return want[i].name < want[j].name })
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s idx:\n GOT  %+v\nWANT %+v", table, got, want)
			}
		})
	}
}

func TestMigrate_S10FKs(t *testing.T) {
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
		name             string
		onUpdate         string
		onDelete         string
	}{
		{"SearchContext_orgId_fkey", "c", "c"},
		{"_RepoToSearchContext_A_fkey", "c", "c"},
		{"_RepoToSearchContext_B_fkey", "c", "c"},
		{"OrgCodeIntelConfig_orgId_fkey", "c", "c"},
		// RepoIndexManifest has 3 FKs incl SET NULL on indexJobId.
		{"RepoIndexManifest_indexJobId_fkey", "c", "n"},
		{"RepoIndexManifest_orgId_fkey", "c", "c"},
		{"RepoIndexManifest_repoId_orgId_fkey", "c", "c"},
		{"RepoIndexManifestFile_manifestId_fkey", "c", "c"},
		{"RepoSemanticChunkManifest_manifestId_fkey", "c", "c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var onUpdate, onDelete string
			var count int
			err := pool.QueryRow(ctx, `
				SELECT confupdtype::text, confdeltype::text, COUNT(*) OVER ()
				FROM pg_constraint WHERE conname = $1
			`, tc.name).Scan(&onUpdate, &onDelete, &count)
			if err != nil {
				t.Fatalf("FK %q: %v", tc.name, err)
			}
			if count != 1 {
				t.Errorf("expected 1, got %d", count)
			}
			if onUpdate != tc.onUpdate || onDelete != tc.onDelete {
				t.Errorf("cascade: got %s/%s want %s/%s",
					onUpdate, onDelete, tc.onUpdate, tc.onDelete)
			}
		})
	}
}
