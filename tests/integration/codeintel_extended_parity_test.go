//go:build integration

// Slice S.9 parity tests — CodeIntel extended tables. Verifies
// each table's column count, index set, and FK cascade
// behavior matches the legacy reference schema. The migration
// 0018_codeintel_extended_tables.sql is the canonical spec for
// column shapes; this test guards against the migration itself
// drifting.
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

// TestMigrate_CodeIntelExtendedColumnCounts asserts each table
// has the expected column count from legacy. A column-count
// drift hits this; deeper col-by-col diffs are deferred to
// per-feature parity slices since the new tables aren't yet
// load-bearing for any shipped feature.
func TestMigrate_CodeIntelExtendedColumnCounts(t *testing.T) {
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

	// Legacy column counts captured 2026-05-24.
	cases := []struct {
		table string
		count int
	}{
		{"CodeIntelToolchain", 14},
		{"CodeIntelLanguageIndex", 18},
		{"CodeIntelSymbol", 18},
		{"CodeIntelOccurrence", 17},
		{"CodeIntelRelationship", 12},
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
				t.Errorf("%s column count: got %d, want %d", tc.table, count, tc.count)
			}
		})
	}
}

// TestMigrate_CodeIntelExtendedIndexes locks every legacy
// index name + uniqueness for each new table.
func TestMigrate_CodeIntelExtendedIndexes(t *testing.T) {
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
		"CodeIntelToolchain": {
			{"CodeIntelToolchain_pkey", true},
			{"CodeIntelToolchain_fingerprint_idx", false},
			{"CodeIntelToolchain_fingerprint_key", true},
			{"CodeIntelToolchain_workerClass_language_indexer_idx", false},
		},
		"CodeIntelLanguageIndex": {
			{"CodeIntelLanguageIndex_pkey", true},
			{"CodeIntelLanguageIndex_codeIntelIndexId_language_projectRoot_in", true},
			{"CodeIntelLanguageIndex_status_idx", false},
			{"CodeIntelLanguageIndex_toolchainId_idx", false},
			{"CodeIntelLanguageIndex_workerClass_status_idx", false},
		},
		"CodeIntelSymbol": {
			{"CodeIntelSymbol_pkey", true},
			{"CodeIntelSymbol_codeIntelIndexId_symbol_key", true},
			{"CodeIntelSymbol_languageIndexId_idx", false},
			{"CodeIntelSymbol_orgId_repoId_displayName_idx", false},
			{"CodeIntelSymbol_orgId_repoId_symbol_idx", false},
		},
		"CodeIntelOccurrence": {
			{"CodeIntelOccurrence_pkey", true},
			{"CodeIntelOccurrence_codeIntelIndexId_symbol_role_idx", false},
			{"CodeIntelOccurrence_languageIndexId_idx", false},
			{"CodeIntelOccurrence_orgId_repoId_filePath_idx", false},
			{"CodeIntelOccurrence_orgId_repoId_symbol_role_idx", false},
		},
		"CodeIntelRelationship": {
			{"CodeIntelRelationship_pkey", true},
			{"CodeIntelRelationship_codeIntelIndexId_sourceSymbol_targetSymbo", true},
			{"CodeIntelRelationship_languageIndexId_idx", false},
			{"CodeIntelRelationship_orgId_repoId_sourceSymbol_idx", false},
			{"CodeIntelRelationship_orgId_repoId_targetSymbol_idx", false},
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

// TestMigrate_CodeIntelExtendedFKs locks every FK + cascade
// behavior. CodeIntelLanguageIndex.toolchainId is the one
// SET NULL exception ('n'); everything else is CASCADE ('c').
func TestMigrate_CodeIntelExtendedFKs(t *testing.T) {
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
		name     string
		onUpdate string
		onDelete string
	}{
		// LanguageIndex - 2 FKs, toolchainId is SET NULL.
		{"CodeIntelLanguageIndex_codeIntelIndexId_fkey", "c", "c"},
		{"CodeIntelLanguageIndex_toolchainId_fkey", "c", "n"},
		// Symbol - 3 FKs.
		{"CodeIntelSymbol_codeIntelIndexId_fkey", "c", "c"},
		{"CodeIntelSymbol_orgId_fkey", "c", "c"},
		{"CodeIntelSymbol_repoId_fkey", "c", "c"},
		// Occurrence - 3 FKs.
		{"CodeIntelOccurrence_codeIntelIndexId_fkey", "c", "c"},
		{"CodeIntelOccurrence_orgId_fkey", "c", "c"},
		{"CodeIntelOccurrence_repoId_fkey", "c", "c"},
		// Relationship - 3 FKs.
		{"CodeIntelRelationship_codeIntelIndexId_fkey", "c", "c"},
		{"CodeIntelRelationship_orgId_fkey", "c", "c"},
		{"CodeIntelRelationship_repoId_fkey", "c", "c"},
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
