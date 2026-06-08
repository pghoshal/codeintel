//go:build integration

// Slice S.6 parity tests — OAuth tables (Client + Token +
// RefreshToken + AuthorizationCode) byte-equal vs legacy.
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

var oauthClientExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"name", "text", "text", "NO", ""},
	{"redirectUris", "ARRAY", "_text", "YES", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"logoUri", "text", "text", "YES", ""},
}

var oauthTokenExpected = []expectedColumn{
	{"hash", "text", "text", "NO", ""},
	{"clientId", "text", "text", "NO", ""},
	{"userId", "text", "text", "NO", ""},
	{"scope", "text", "text", "NO", "''::text"},
	{"expiresAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"lastUsedAt", "timestamp without time zone", "timestamp", "YES", ""},
	{"resource", "text", "text", "YES", ""},
}

var oauthRefreshTokenExpected = []expectedColumn{
	{"hash", "text", "text", "NO", ""},
	{"clientId", "text", "text", "NO", ""},
	{"userId", "text", "text", "NO", ""},
	{"scope", "text", "text", "NO", "''::text"},
	{"resource", "text", "text", "YES", ""},
	{"expiresAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
}

var oauthAuthorizationCodeExpected = []expectedColumn{
	{"codeHash", "text", "text", "NO", ""},
	{"clientId", "text", "text", "NO", ""},
	{"userId", "text", "text", "NO", ""},
	{"redirectUri", "text", "text", "NO", ""},
	{"codeChallenge", "text", "text", "NO", ""},
	{"expiresAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"resource", "text", "text", "YES", ""},
}

func TestMigrate_OAuthTablesShape(t *testing.T) {
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
		{"OAuthClient", oauthClientExpected},
		{"OAuthToken", oauthTokenExpected},
		{"OAuthRefreshToken", oauthRefreshTokenExpected},
		{"OAuthAuthorizationCode", oauthAuthorizationCodeExpected},
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
				t.Fatalf("query: %v", err)
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
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s col drift:\n GOT:  %+v\nWANT: %+v", tc.table, got, tc.want)
			}
		})
	}
}

func TestMigrate_OAuthTablesIndexes(t *testing.T) {
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
		"OAuthClient":            {{"OAuthClient_pkey", true}},
		"OAuthToken":             {{"OAuthToken_pkey", true}},
		"OAuthRefreshToken":      {{"OAuthRefreshToken_pkey", true}, {"OAuthRefreshToken_clientId_userId_idx", false}},
		"OAuthAuthorizationCode": {{"OAuthAuthorizationCode_pkey", true}},
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
				t.Fatalf("query %s: %v", table, err)
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
				t.Errorf("%s:\n GOT:  %+v\nWANT: %+v", table, got, want)
			}
		})
	}
}

func TestMigrate_OAuthTablesFKs(t *testing.T) {
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
		"OAuthToken_clientId_fkey",
		"OAuthToken_userId_fkey",
		"OAuthRefreshToken_clientId_fkey",
		"OAuthRefreshToken_userId_fkey",
		"OAuthAuthorizationCode_clientId_fkey",
		"OAuthAuthorizationCode_userId_fkey",
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
				t.Fatalf("FK %q: %v", name, err)
			}
			if count != 1 {
				t.Errorf("expected 1 match, got %d", count)
			}
			if onUpdate != "c" || onDelete != "c" {
				t.Errorf("cascade: got %q/%q, want c/c", onUpdate, onDelete)
			}
		})
	}
}
