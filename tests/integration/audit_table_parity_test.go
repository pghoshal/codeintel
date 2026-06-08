//go:build integration

// Audit-table rename parity test. Asserts the legacy-shaped
// "Audit" table exists after migrations, the codeintel-divergent
// "AuditEvent" is gone, every column matches legacy (including
// the brand-scrubbed `codeintelVersion` column that replaces
// the legacy column's brand-prefixed name), and all three legacy
// indexes + the FK to Org are in place.
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

var auditExpected = []expectedColumn{
	{"id", "text", "text", "NO", ""},
	{"timestamp", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"action", "text", "text", "NO", ""},
	{"actorId", "text", "text", "NO", ""},
	{"actorType", "text", "text", "NO", ""},
	{"targetId", "text", "text", "NO", ""},
	{"targetType", "text", "text", "NO", ""},
	{"codeintelVersion", "text", "text", "NO", ""},
	{"metadata", "jsonb", "jsonb", "YES", ""},
	{"orgId", "integer", "int4", "NO", ""},
}

func TestMigrate_AuditTableShape(t *testing.T) {
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

	rows, err := pool.Query(ctx, `
		SELECT column_name, data_type, udt_name, is_nullable, COALESCE(column_default, '')
		FROM   information_schema.columns
		WHERE  table_name = 'Audit'
		ORDER BY ordinal_position
	`)
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
	if !reflect.DeepEqual(got, auditExpected) {
		t.Errorf("Audit col drift:\n GOT  %+v\nWANT %+v", got, auditExpected)
	}
}

// TestMigrate_AuditEventDropped asserts the codeintel-divergent
// AuditEvent table is gone after 0022.
func TestMigrate_AuditEventDropped(t *testing.T) {
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
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM pg_tables WHERE tablename = 'AuditEvent'`,
	).Scan(&count); err != nil {
		t.Fatalf("query: %v", err)
	}
	if count != 0 {
		t.Errorf("AuditEvent table still exists; drop did not apply")
	}
}

// TestMigrate_AuditIndexes asserts the three legacy indexes
// exist with the right names + uniqueness.
func TestMigrate_AuditIndexes(t *testing.T) {
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
	want := []indexSpec{
		{"Audit_pkey", true},
		{"Audit_actorId_actorType_targetId_targetType_orgId_idx", false},
		{"idx_audit_actor_time_full", false},
		{"idx_audit_core_actions_full", false},
	}
	rows, err := pool.Query(ctx, `
		SELECT c.relname, i.indisunique FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid
		JOIN pg_class t ON t.oid = i.indrelid WHERE t.relname = 'Audit'
	`)
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
		t.Errorf("Audit idx:\n GOT  %+v\nWANT %+v", got, want)
	}
}

func TestMigrate_AuditOrgFK(t *testing.T) {
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
	var onUpdate, onDelete string
	var count int
	err = pool.QueryRow(ctx, `
		SELECT confupdtype::text, confdeltype::text, COUNT(*) OVER ()
		FROM pg_constraint WHERE conname = 'Audit_orgId_fkey'
	`).Scan(&onUpdate, &onDelete, &count)
	if err != nil {
		t.Fatalf("FK lookup: %v", err)
	}
	if count != 1 {
		t.Errorf("FK count: got %d, want 1", count)
	}
	if onUpdate != "c" || onDelete != "c" {
		t.Errorf("cascade: got %s/%s want c/c", onUpdate, onDelete)
	}
}
