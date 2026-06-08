//go:build integration

// Slice S.5 parity tests — AccountPermissionSyncJob,
// AccountToRepoPermission, RepoPermissionSyncJob table shapes
// byte-equal vs legacy. Also confirms each table's enum-typed
// columns wired to the right enum type from S.1.
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

var accountPermissionSyncJobExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"status", "USER-DEFINED", "AccountPermissionSyncJobStatus", "NO",
		`'PENDING'::"AccountPermissionSyncJobStatus"`},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"updatedAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"completedAt", "timestamp without time zone", "timestamp", "YES", ""},
	{"errorMessage", "text", "text", "YES", ""},
	{"accountId", "text", "text", "NO", ""},
}

var accountToRepoPermissionExpected = []expectedColumn{
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"repoId", "integer", "int4", "NO", ""},
	{"accountId", "text", "text", "NO", ""},
	{"source", "USER-DEFINED", "PermissionSyncSource", "NO",
		`'ACCOUNT_DRIVEN'::"PermissionSyncSource"`},
}

var repoPermissionSyncJobExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"status", "USER-DEFINED", "RepoPermissionSyncJobStatus", "NO",
		`'PENDING'::"RepoPermissionSyncJobStatus"`},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"updatedAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"completedAt", "timestamp without time zone", "timestamp", "YES", ""},
	{"errorMessage", "text", "text", "YES", ""},
	{"repoId", "integer", "int4", "NO", ""},
}

// TestMigrate_PermissionSyncTablesShape asserts every column on
// every permission-sync table matches legacy, including the
// enum-typed columns and their default values.
func TestMigrate_PermissionSyncTablesShape(t *testing.T) {
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
		want  []expectedColumn
	}{
		{"AccountPermissionSyncJob", accountPermissionSyncJobExpected},
		{"AccountToRepoPermission", accountToRepoPermissionExpected},
		{"RepoPermissionSyncJob", repoPermissionSyncJobExpected},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT column_name, data_type, udt_name, is_nullable, COALESCE(column_default, '')
				FROM   information_schema.columns
				WHERE  table_name = $1
				ORDER BY ordinal_position
			`, tc.table)
			if err != nil {
				t.Fatalf("query %s: %v", tc.table, err)
			}
			defer rows.Close()
			var got []expectedColumn
			for rows.Next() {
				var c expectedColumn
				if err := rows.Scan(&c.name, &c.dataType, &c.udtName, &c.isNullable, &c.defaultExp); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, c)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s column drift:\n GOT:  %+v\nWANT: %+v", tc.table, got, tc.want)
			}
		})
	}
}

// TestMigrate_PermissionSyncTablesIndexes locks the PKs for
// each permission-sync table. The composite PK on
// AccountToRepoPermission is the load-bearing legacy invariant
// (no separate id column).
func TestMigrate_PermissionSyncTablesIndexes(t *testing.T) {
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
		"AccountPermissionSyncJob": {
			{"AccountPermissionSyncJob_pkey", true},
		},
		"AccountToRepoPermission": {
			{"AccountToRepoPermission_pkey", true},
		},
		"RepoPermissionSyncJob": {
			{"RepoPermissionSyncJob_pkey", true},
		},
	}
	for table, want := range expected {
		t.Run(table, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT c.relname, i.indisunique
				FROM   pg_index i
				JOIN   pg_class c ON c.oid = i.indexrelid
				JOIN   pg_class t ON t.oid = i.indrelid
				WHERE  t.relname = $1
			`, table)
			if err != nil {
				t.Fatalf("query %s indexes: %v", table, err)
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
				t.Errorf("%s indexes:\n GOT:  %+v\nWANT: %+v", table, got, want)
			}
		})
	}

	// Confirm AccountToRepoPermission's PK covers exactly 2
	// columns (repoId, accountId).
	t.Run("AccountToRepoPermission_pkey_columns", func(t *testing.T) {
		var col1, col2 string
		err := pool.QueryRow(ctx, `
			SELECT
			  a1.attname,
			  a2.attname
			FROM   pg_index i
			JOIN   pg_class c ON c.oid = i.indexrelid
			JOIN   pg_attribute a1 ON a1.attrelid = i.indrelid AND a1.attnum = i.indkey[0]
			JOIN   pg_attribute a2 ON a2.attrelid = i.indrelid AND a2.attnum = i.indkey[1]
			WHERE  c.relname = 'AccountToRepoPermission_pkey'
		`).Scan(&col1, &col2)
		if err != nil {
			t.Fatalf("pkey columns lookup: %v", err)
		}
		if col1 != "repoId" || col2 != "accountId" {
			t.Errorf("composite PK columns: got (%q, %q), want (\"repoId\", \"accountId\")", col1, col2)
		}
	})
}

// TestMigrate_PermissionSyncTablesFKs asserts all 4 FK
// constraints exist with cascade-cascade behavior.
func TestMigrate_PermissionSyncTablesFKs(t *testing.T) {
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

	want := []string{
		"AccountPermissionSyncJob_accountId_fkey",
		"AccountToRepoPermission_accountId_fkey",
		"AccountToRepoPermission_repoId_fkey",
		"RepoPermissionSyncJob_repoId_fkey",
	}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			var (
				onUpdate, onDelete string
				count              int
			)
			err := pool.QueryRow(ctx, `
				SELECT confupdtype::text, confdeltype::text, COUNT(*) OVER ()
				FROM   pg_constraint
				WHERE  conname = $1
			`, name).Scan(&onUpdate, &onDelete, &count)
			if err != nil {
				t.Fatalf("FK %q lookup: %v", name, err)
			}
			if count != 1 {
				t.Errorf("expected exactly 1 constraint matching %q, got %d", name, count)
			}
			if onUpdate != "c" || onDelete != "c" {
				t.Errorf("cascade: got onUpdate=%q onDelete=%q, want both \"c\" (CASCADE)",
					onUpdate, onDelete)
			}
		})
	}
}
