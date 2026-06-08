//go:build integration

// Slice S.3c parity test — timestamp + NOT NULL reconciliation.
// Asserts no `timestamp with time zone` column remains on the
// codeintel tables that diverged from legacy, Repo's 5
// previously-nullable cols are now NOT NULL, and Repo's
// isFork/isArchived defaults are dropped.
package integration

import (
	"context"
	"testing"
	"time"

	"codeintel/internal/db"
	"codeintel/internal/migrate"
)

// TestMigrate_S3c_NoTimestamptzRemains asserts no public-schema
// table (except schema_migrations bookkeeping) carries a
// timestamp-with-time-zone column. Legacy uses
// timestamp(3) without time zone everywhere; codeintel must
// match.
func TestMigrate_S3c_NoTimestamptzRemains(t *testing.T) {
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
		SELECT table_name, column_name
		FROM   information_schema.columns
		WHERE  data_type = 'timestamp with time zone'
		  AND  table_schema = 'public'
		  AND  table_name != 'schema_migrations'
		ORDER BY table_name, column_name
	`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var drift []string
	for rows.Next() {
		var tbl, col string
		if err := rows.Scan(&tbl, &col); err != nil {
			t.Fatalf("scan: %v", err)
		}
		drift = append(drift, tbl+"."+col)
	}
	if len(drift) > 0 {
		t.Errorf("%d timestamptz columns still drift from legacy timestamp(3): %v", len(drift), drift)
	}
}

// TestMigrate_S3c_RepoNotNullTightening asserts the 5 Repo
// columns the S.3a migration deferred as NULLABLE are now
// NOT NULL matching legacy.
func TestMigrate_S3c_RepoNotNullTightening(t *testing.T) {
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

	cols := []string{"cloneUrl", "external_id", "external_codeHostUrl", "external_codeHostType", "metadata"}
	for _, col := range cols {
		t.Run(col, func(t *testing.T) {
			var nullable string
			err := pool.QueryRow(ctx, `
				SELECT is_nullable FROM information_schema.columns
				WHERE  table_name = 'Repo' AND column_name = $1
			`, col).Scan(&nullable)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if nullable != "NO" {
				t.Errorf("Repo.%s is_nullable: got %q, want \"NO\"", col, nullable)
			}
		})
	}
}

// TestMigrate_S3c_RepoBoolDefaultsDropped asserts Repo.isFork
// and Repo.isArchived no longer have a DEFAULT FALSE. Legacy
// has them NOT NULL with no default (caller MUST supply).
func TestMigrate_S3c_RepoBoolDefaultsDropped(t *testing.T) {
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

	for _, col := range []string{"isFork", "isArchived"} {
		t.Run(col, func(t *testing.T) {
			var defaultExpr *string
			err := pool.QueryRow(ctx, `
				SELECT column_default FROM information_schema.columns
				WHERE table_name = 'Repo' AND column_name = $1
			`, col).Scan(&defaultExpr)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if defaultExpr != nil {
				t.Errorf("Repo.%s should have no default; got %q", col, *defaultExpr)
			}
		})
	}
}

// TestMigrate_S3c_CreatedAtUsesCurrentTimestamp confirms a
// sample of timestamp columns now have CURRENT_TIMESTAMP
// instead of now() as the default. The two functions return
// semantically the same value but legacy uses CURRENT_TIMESTAMP
// in its DDL; we mirror that.
func TestMigrate_S3c_CreatedAtUsesCurrentTimestamp(t *testing.T) {
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

	// Sample 4 tables; the migration applied the change to all
	// affected tables so a full sweep would be redundant.
	for _, tbl := range []string{"Org", "User", "Connection", "Repo"} {
		t.Run(tbl+".createdAt", func(t *testing.T) {
			var def string
			err := pool.QueryRow(ctx, `
				SELECT column_default FROM information_schema.columns
				WHERE table_name = $1 AND column_name = 'createdAt'
			`, tbl).Scan(&def)
			if err != nil {
				t.Fatalf("lookup: %v", err)
			}
			if def != "CURRENT_TIMESTAMP" {
				t.Errorf("%s.createdAt default: got %q, want CURRENT_TIMESTAMP", tbl, def)
			}
		})
	}
}
