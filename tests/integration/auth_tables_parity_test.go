//go:build integration

// Slice S.4 parity tests — Account / AccountRequest / Invite /
// VerificationToken table shapes byte-equal vs the legacy
// reference schema.
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

// expectedAuthCol mirrors expectedColumn from the Zoekt parity
// test but lives in this file to avoid the (test-package
// unexported) duplication blocking cross-file refactor.

// accountExpected mirrors legacy_schema.Account 1:1.
var accountExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"userId", "text", "text", "NO", ""},
	{"type", "text", "text", "NO", ""},
	{"provider", "text", "text", "NO", ""},
	{"providerAccountId", "text", "text", "NO", ""},
	{"refresh_token", "text", "text", "YES", ""},
	{"access_token", "text", "text", "YES", ""},
	{"expires_at", "integer", "int4", "YES", ""},
	{"token_type", "text", "text", "YES", ""},
	{"scope", "text", "text", "YES", ""},
	{"id_token", "text", "text", "YES", ""},
	{"session_state", "text", "text", "YES", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"updatedAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"permissionSyncedAt", "timestamp without time zone", "timestamp", "YES", ""},
	{"issuerUrl", "text", "text", "YES", ""},
	{"tokenRefreshErrorMessage", "text", "text", "YES", ""},
}

var accountRequestExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"requestedById", "text", "text", "NO", ""},
	{"orgId", "integer", "int4", "NO", ""},
}

var inviteExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"recipientEmail", "text", "text", "NO", ""},
	{"hostUserId", "text", "text", "NO", ""},
	{"orgId", "integer", "int4", "NO", ""},
}

var verificationTokenExpected = []expectedColumn{
	{"identifier", "text", "text", "NO", ""},
	{"token", "text", "text", "NO", ""},
	{"expires", "timestamp without time zone", "timestamp", "NO", ""},
}

// TestMigrate_AuthTablesShape asserts every column on every
// auth table matches legacy.
func TestMigrate_AuthTablesShape(t *testing.T) {
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
		{"Account", accountExpected},
		{"AccountRequest", accountRequestExpected},
		{"Invite", inviteExpected},
		{"VerificationToken", verificationTokenExpected},
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

// TestMigrate_AuthTablesIndexes locks every legacy index name +
// uniqueness flag on each auth table.
func TestMigrate_AuthTablesIndexes(t *testing.T) {
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
		"Account": {
			{"Account_pkey", true},
			{"Account_provider_providerAccountId_key", true},
		},
		"AccountRequest": {
			{"AccountRequest_pkey", true},
			{"AccountRequest_requestedById_key", true},
			{"AccountRequest_requestedById_orgId_key", true},
		},
		"Invite": {
			{"Invite_pkey", true},
			{"Invite_recipientEmail_orgId_key", true},
		},
		"VerificationToken": {
			// Legacy has NO pkey — only the unique compound idx.
			{"VerificationToken_identifier_token_key", true},
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
}

// TestMigrate_AuthTablesFKs asserts every FK constraint exists
// with the correct cascade behavior.
func TestMigrate_AuthTablesFKs(t *testing.T) {
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

	// All five FKs use ON UPDATE CASCADE ('c') ON DELETE CASCADE ('c').
	want := []string{
		"Account_userId_fkey",
		"AccountRequest_orgId_fkey",
		"AccountRequest_requestedById_fkey",
		"Invite_hostUserId_fkey",
		"Invite_orgId_fkey",
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
