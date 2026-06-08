//go:build integration

// Live-Postgres integration test for the pgx pool. Runs only with
// the `integration` build tag so unit-test suites stay hermetic.
//
// Operator workflow:
//
//	export CODEINTEL_TEST_POSTGRES_URL="postgres://USER:PASS@127.0.0.1:5432/DB?sslmode=disable"
//	cd codeintel && go test -tags=integration ./tests/integration/...
//
// The DSN is supplied via env var. The test fails with a clear
// message if the env var is unset — never t.Skip. Integration
// tests either run for real or fail loudly.
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"codeintel/internal/db"
)

const envPostgresURL = "CODEINTEL_TEST_POSTGRES_URL"

func requireDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(envPostgresURL)
	if dsn == "" {
		t.Fatalf("integration tests require %s to be set (e.g. postgres://user:pass@127.0.0.1:5432/db?sslmode=disable)", envPostgresURL)
	}
	return dsn
}

// TestDBPool_ConnectsToLivePostgres validates the full pool
// lifecycle against a real Postgres: construct, Ping (done by
// NewPool), run SELECT 1, close.
func TestDBPool_ConnectsToLivePostgres(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                    dsn,
		AllowInsecureRemoteDSN: true,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	if one != 1 {
		t.Fatalf("SELECT 1 returned %d, want 1", one)
	}
}

// TestDBPool_ReportsServerVersion exercises a slightly richer query
// so a Postgres version drift is visible at test time.
func TestDBPool_ReportsServerVersion(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                    dsn,
		AllowInsecureRemoteDSN: true,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	var version string
	if err := pool.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		t.Fatalf("SHOW server_version: %v", err)
	}
	if version == "" {
		t.Fatalf("expected non-empty server_version")
	}
	t.Logf("postgres server_version: %s", version)
}

// TestDBPool_RejectsUnreachableHost confirms NewPool surfaces a
// wrapped "db: ping" error within the ConnectTimeout when the host
// is unreachable. Uses 192.0.2.1 (TEST-NET-1, RFC 5737, documented
// as unroutable).
func TestDBPool_RejectsUnreachableHost(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{
		DSN:                    "postgres://u:p@192.0.2.1:5432/db?sslmode=disable&connect_timeout=1",
		AllowInsecureRemoteDSN: true,
		ConnectTimeout:         2 * time.Second,
	})
	if pool != nil {
		pool.Close()
	}
	if err == nil {
		t.Fatalf("expected error from unreachable host, got pool=%v", pool)
	}
	t.Logf("unreachable-host error (expected): %v", err)
}
