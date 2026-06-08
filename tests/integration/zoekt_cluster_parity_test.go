//go:build integration

// Slice S.2 of the codeintel ↔ legacy schema-parity recovery.
// Live-Postgres tests asserting the three Zoekt-cluster tables
// (ZoektShardGroup, ZoektOrgIndex, ZoektOrgReplica) exist with
// byte-equal column shapes + indexes + FKs vs the legacy
// reference schema.
//
// Ground truth captured 2026-05-24 from the legacy reference
// postgres (database `legacy_schema`).
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

type expectedColumn struct {
	name       string
	dataType   string
	udtName    string // for enum / custom types — verified separately
	isNullable string
	defaultExp string // information_schema.columns.column_default, "" = none
}

// zoektShardGroupExpected mirrors legacy_schema.ZoektShardGroup
// 1:1. udtName is the PostgreSQL OID-level type name; for plain
// SQL types (TEXT, INTEGER, etc.) the value matches data_type
// case-folded.
var zoektShardGroupExpected = []expectedColumn{
	{"id", "integer", "int4", "NO", `nextval('"ZoektShardGroup_id_seq"'::regclass)`},
	{"name", "text", "text", "NO", ""},
	{"endpointUrls", "ARRAY", "_text", "YES", ""},
	{"description", "text", "text", "YES", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"updatedAt", "timestamp without time zone", "timestamp", "NO", ""},
}

var zoektOrgIndexExpected = []expectedColumn{
	{"id", "integer", "int4", "NO", `nextval('"ZoektOrgIndex_id_seq"'::regclass)`},
	{"storageRoot", "text", "text", "NO", ""},
	{"indexPath", "text", "text", "NO", ""},
	{"repoCachePath", "text", "text", "NO", ""},
	{"status", "USER-DEFINED", "ZoektOrgIndexStatus", "NO", `'PENDING'::"ZoektOrgIndexStatus"`},
	{"lastIndexedAt", "timestamp without time zone", "timestamp", "YES", ""},
	{"lastHealthCheckAt", "timestamp without time zone", "timestamp", "YES", ""},
	{"errorMessage", "text", "text", "YES", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"updatedAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"orgId", "integer", "int4", "NO", ""},
}

var zoektOrgReplicaExpected = []expectedColumn{
	{"id", "integer", "int4", "NO", `nextval('"ZoektOrgReplica_id_seq"'::regclass)`},
	{"endpointUrl", "text", "text", "NO", ""},
	{"nodeName", "text", "text", "YES", ""},
	{"isWriter", "boolean", "bool", "NO", "false"},
	{"priority", "integer", "int4", "NO", "0"},
	{"status", "USER-DEFINED", "ZoektOrgReplicaStatus", "NO", `'UNKNOWN'::"ZoektOrgReplicaStatus"`},
	{"lastHealthCheckAt", "timestamp without time zone", "timestamp", "YES", ""},
	{"errorMessage", "text", "text", "YES", ""},
	{"createdAt", "timestamp without time zone", "timestamp", "NO", "CURRENT_TIMESTAMP"},
	{"updatedAt", "timestamp without time zone", "timestamp", "NO", ""},
	{"orgIndexId", "integer", "int4", "NO", ""},
}

// TestMigrate_ZoektClusterTablesShape locks every column on
// the three Zoekt tables.
func TestMigrate_ZoektClusterTablesShape(t *testing.T) {
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
		{"ZoektShardGroup", zoektShardGroupExpected},
		{"ZoektOrgIndex", zoektOrgIndexExpected},
		{"ZoektOrgReplica", zoektOrgReplicaExpected},
	}
	for _, tc := range cases {
		t.Run(tc.table, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT column_name, data_type, udt_name, is_nullable, COALESCE(column_default, '')
				FROM information_schema.columns
				WHERE table_name = $1
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

// TestMigrate_ZoektClusterIndexes asserts every legacy index
// exists on the corresponding codeintel table.
func TestMigrate_ZoektClusterIndexes(t *testing.T) {
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
		"ZoektShardGroup": {
			{"ZoektShardGroup_pkey", true},
			{"ZoektShardGroup_name_key", true},
		},
		"ZoektOrgIndex": {
			{"ZoektOrgIndex_pkey", true},
			{"ZoektOrgIndex_orgId_key", true},
			{"ZoektOrgIndex_status_idx", false},
		},
		"ZoektOrgReplica": {
			{"ZoektOrgReplica_pkey", true},
			{"ZoektOrgReplica_orgIndexId_endpointUrl_key", true},
			{"ZoektOrgReplica_endpointUrl_status_idx", false},
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
			sortIndexes(got)
			sortIndexes(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s indexes:\n GOT:  %+v\nWANT: %+v", table, got, want)
			}
		})
	}
}

type indexSpec struct {
	name   string
	isUniq bool
}

func sortIndexes(s []indexSpec) {
	sort.Slice(s, func(i, j int) bool { return s[i].name < s[j].name })
}

// TestMigrate_ZoektClusterFKs asserts the two FK constraints
// exist with the correct referenced table + cascade behavior.
func TestMigrate_ZoektClusterFKs(t *testing.T) {
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

	type fkSpec struct {
		name       string
		table      string
		col        string
		refTable   string
		refCol     string
		onUpdate   string // 'a'=NO ACTION, 'c'=CASCADE, 'n'=SET NULL, ...
		onDelete   string
	}
	want := []fkSpec{
		{"ZoektOrgIndex_orgId_fkey", "ZoektOrgIndex", "orgId", "Org", "id", "c", "c"},
		{"ZoektOrgReplica_orgIndexId_fkey", "ZoektOrgReplica", "orgIndexId", "ZoektOrgIndex", "id", "c", "c"},
	}

	for _, w := range want {
		t.Run(w.name, func(t *testing.T) {
			var (
				gotTable, gotCol, gotRefTable, gotRefCol string
				gotOnUpdate, gotOnDelete                 string
			)
			err := pool.QueryRow(ctx, `
				SELECT
				  cl.relname,
				  att.attname,
				  rcl.relname,
				  ratt.attname,
				  con.confupdtype::text,
				  con.confdeltype::text
				FROM   pg_constraint con
				JOIN   pg_class cl   ON cl.oid  = con.conrelid
				JOIN   pg_class rcl  ON rcl.oid = con.confrelid
				JOIN   pg_attribute att  ON att.attrelid  = con.conrelid  AND att.attnum  = con.conkey[1]
				JOIN   pg_attribute ratt ON ratt.attrelid = con.confrelid AND ratt.attnum = con.confkey[1]
				WHERE  con.conname = $1
			`, w.name).Scan(&gotTable, &gotCol, &gotRefTable, &gotRefCol, &gotOnUpdate, &gotOnDelete)
			if err != nil {
				t.Fatalf("FK %s lookup: %v", w.name, err)
			}
			if gotTable != w.table {
				t.Errorf("table: got %q, want %q", gotTable, w.table)
			}
			if gotCol != w.col {
				t.Errorf("col: got %q, want %q", gotCol, w.col)
			}
			if gotRefTable != w.refTable {
				t.Errorf("refTable: got %q, want %q", gotRefTable, w.refTable)
			}
			if gotRefCol != w.refCol {
				t.Errorf("refCol: got %q, want %q", gotRefCol, w.refCol)
			}
			if gotOnUpdate != w.onUpdate {
				t.Errorf("onUpdate: got %q, want %q (a=NO_ACTION c=CASCADE n=SET_NULL)", gotOnUpdate, w.onUpdate)
			}
			if gotOnDelete != w.onDelete {
				t.Errorf("onDelete: got %q, want %q", gotOnDelete, w.onDelete)
			}
		})
	}
}
