//go:build integration

// Slice S.7 parity tests — Chat + ChatAccess.
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

var chatExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"name", "text", "text", "YES", ""},
	{"createdById", "text", "text", "YES", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"updatedAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"orgId", "integer", "int4", "NO", ""},
	{"visibility", "USER-DEFINED", "ChatVisibility", "NO", `'PRIVATE'::"ChatVisibility"`},
	{"messages", "jsonb", "jsonb", "NO", ""},
	{"anonymousCreatorId", "text", "text", "YES", ""},
}

var chatAccessExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"chatId", "text", "text", "NO", ""},
	{"userId", "text", "text", "NO", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
}

func TestMigrate_ChatTablesShape(t *testing.T) {
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
		{"Chat", chatExpected},
		{"ChatAccess", chatAccessExpected},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT column_name, data_type, udt_name, is_nullable, COALESCE(column_default, '')
				FROM information_schema.columns WHERE table_name = $1 ORDER BY ordinal_position
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
				t.Errorf("%s drift:\n GOT:  %+v\nWANT: %+v", tc.table, got, tc.want)
			}
		})
	}
}

func TestMigrate_ChatTablesIndexes(t *testing.T) {
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
		"Chat":       {{"Chat_pkey", true}},
		"ChatAccess": {{"ChatAccess_pkey", true}, {"ChatAccess_chatId_userId_key", true}},
	}
	for table, want := range expected {
		t.Run(table, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT c.relname, i.indisunique
				FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
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
				t.Errorf("%s:\n GOT %+v\nWANT %+v", table, got, want)
			}
		})
	}
}

func TestMigrate_ChatTablesFKs(t *testing.T) {
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
		"Chat_createdById_fkey", "Chat_orgId_fkey",
		"ChatAccess_chatId_fkey", "ChatAccess_userId_fkey",
	}
	for _, name := range want {
		t.Run(name, func(t *testing.T) {
			var onUpdate, onDelete string
			var count int
			err := pool.QueryRow(ctx, `
				SELECT confupdtype::text, confdeltype::text, COUNT(*) OVER ()
				FROM pg_constraint WHERE conname = $1
			`, name).Scan(&onUpdate, &onDelete, &count)
			if err != nil {
				t.Fatalf("FK lookup: %v", err)
			}
			if count != 1 {
				t.Errorf("expected 1 match, got %d", count)
			}
			if onUpdate != "c" || onDelete != "c" {
				t.Errorf("cascade: got %s/%s want c/c", onUpdate, onDelete)
			}
		})
	}
}
