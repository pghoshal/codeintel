package dbpool

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestNewPool_RejectsEmptyDSN locks the contract that NewPool
// refuses an empty DSN. The pool reads CODEINTEL_DATABASE_URL.
// Empty input must produce a clear error, not a silent
// connection-refused at first query time.
func TestNewPool_RejectsEmptyDSN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, Config{DSN: ""})
	if err == nil {
		if pool != nil {
			pool.Close()
		}
		t.Fatalf("NewPool with empty DSN must error; got pool=%v", pool)
	}
	if !errors.Is(err, ErrDSNRequired) {
		t.Fatalf("expected ErrDSNRequired, got %v", err)
	}
}

// TestNewPool_RejectsMalformedDSN locks the contract that a malformed
// DSN fails fast at construction, not lazily on first query. pgx parses
// the DSN in pgxpool.ParseConfig — we surface the error directly.
func TestNewPool_RejectsMalformedDSN(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	pool, err := NewPool(ctx, Config{DSN: "not-a-real-dsn-without-scheme"})
	if pool != nil {
		pool.Close()
	}
	if err == nil {
		t.Fatalf("NewPool with malformed DSN must error")
	}
	if !strings.Contains(err.Error(), "DSN") && !strings.Contains(err.Error(), "dsn") && !strings.Contains(err.Error(), "config") {
		t.Fatalf("expected error to reference the DSN/config; got %q", err.Error())
	}
}

// TestNewPool_HostileDSNs locks the contract that ParseConfig either
// rejects a hostile DSN cleanly OR returns a usable config without
// panicking. This is the security regression test for the
// pgx-parses-untrusted-input path.
func TestNewPool_HostileDSNs(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
	}{
		{"sql_injection_in_path", `postgres://u:p@h/db?x='; DROP TABLE--`},
		{"very_long", "postgres://u:p@h/db?z=" + strings.Repeat("a", 65536)},
		{"non_utf8_bytes", "postgres://u:p@h/db?\xff\xfe\xfd=1"},
		{"missing_scheme", "just-some-junk"},
		{"empty_query_value", "postgres://u:p@h/db?x="},
		{"only_scheme", "postgres://"},
		{"ssh_scheme", "ssh://attacker/db"},
		{"http_scheme", "http://attacker/db"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("NewPool panicked on hostile DSN %q: %v", tc.dsn, r)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			pool, _ := NewPool(ctx, Config{DSN: tc.dsn, AllowInsecureRemoteDSN: true})
			if pool != nil {
				pool.Close()
			}
			// Either err is non-nil (preferred), or pool was constructed
			// and pgx will fail at first query on a clearly invalid host.
			// Both outcomes are acceptable — what we forbid is a panic.
		})
	}
}

// TestResolveMaxConns_DefaultsToCPUx4Capped32 locks the pool-sizing
// formula recorded in docs/codeintel-porting-plan.md §7:
// size; the Go port chooses a per-host size by default and lets
// CODEINTEL_DATABASE_MAX_CONNS override.
func TestResolveMaxConns_DefaultsToCPUx4Capped32(t *testing.T) {
	cpus := runtime.NumCPU()
	expected := int32(cpus * 4)
	if expected > 32 {
		expected = 32
	}
	got := resolveMaxConns(0)
	if got != expected {
		t.Fatalf("resolveMaxConns(0): got %d, want %d (numCPU=%d)", got, expected, cpus)
	}
}

// TestResolveMaxConns_ExplicitOverrideHonored confirms an operator
// setting CODEINTEL_DATABASE_MAX_CONNS=N gets exactly N, not the
// default formula.
func TestResolveMaxConns_ExplicitOverrideHonored(t *testing.T) {
	got := resolveMaxConns(7)
	if got != 7 {
		t.Fatalf("resolveMaxConns(7): got %d, want 7", got)
	}
}

// TestResolveMaxConns_NegativeOverrideClampsToDefault prevents an
// invalid configuration from disabling the pool (size 0 in pgx means
// "default to runtime.NumCPU()" but our SLO requires explicit sizing).
func TestResolveMaxConns_NegativeOverrideClampsToDefault(t *testing.T) {
	cpus := runtime.NumCPU()
	expected := int32(cpus * 4)
	if expected > 32 {
		expected = 32
	}
	got := resolveMaxConns(-5)
	if got != expected {
		t.Fatalf("resolveMaxConns(-5): got %d, want %d", got, expected)
	}
}

// TestResolveConnectTimeout_DefaultsTo5s locks the cold-start budget:
// when Config.ConnectTimeout is zero, NewPool waits up to 5 s for the
// initial Ping. The plan §6 cold-start SLO is < 5 s; a slower DB
// should fail readiness, not block boot.
func TestResolveConnectTimeout_DefaultsTo5s(t *testing.T) {
	if got := resolveConnectTimeout(0); got != 5*time.Second {
		t.Fatalf("resolveConnectTimeout(0): got %v, want 5s", got)
	}
	if got := resolveConnectTimeout(-1 * time.Second); got != 5*time.Second {
		t.Fatalf("resolveConnectTimeout(-1s): got %v, want 5s (negative → default)", got)
	}
	if got := resolveConnectTimeout(2 * time.Second); got != 2*time.Second {
		t.Fatalf("resolveConnectTimeout(2s): got %v, want 2s (explicit honored)", got)
	}
}

// TestApplyPoolDefaults_StampsAllFields confirms the codeintel
// lifecycle defaults land on a parsed pgxpool config. The values are
// asserted exactly so a future change to one constant fails the test
// and forces a porting-plan amendment.
func TestApplyPoolDefaults_StampsAllFields(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost/db")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	applyPoolDefaults(cfg, 0)
	if cfg.MinConns != defaultMinConns {
		t.Fatalf("MinConns: got %d, want %d", cfg.MinConns, defaultMinConns)
	}
	if cfg.MaxConns < 1 {
		t.Fatalf("MaxConns must be positive, got %d", cfg.MaxConns)
	}
	if cfg.MaxConnLifetime != defaultMaxConnLifetime {
		t.Fatalf("MaxConnLifetime: got %v, want %v", cfg.MaxConnLifetime, defaultMaxConnLifetime)
	}
	if cfg.MaxConnIdleTime != defaultMaxConnIdleTime {
		t.Fatalf("MaxConnIdleTime: got %v, want %v", cfg.MaxConnIdleTime, defaultMaxConnIdleTime)
	}
}

// TestApplyPoolDefaults_RespectsExplicitMaxConns checks the override
// path: when an operator sets CODEINTEL_DATABASE_MAX_CONNS, that value
// wins over the formula.
func TestApplyPoolDefaults_RespectsExplicitMaxConns(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u:p@localhost/db")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	applyPoolDefaults(cfg, 11)
	if cfg.MaxConns != 11 {
		t.Fatalf("MaxConns: got %d, want 11 (explicit override)", cfg.MaxConns)
	}
}

// TestEnforceRemoteTLS_BlocksRemotePlaintext locks the security
// contract: a non-loopback host with no TLS must be rejected.
func TestEnforceRemoteTLS_BlocksRemotePlaintext(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u:p@db.prod.example.com/db?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.ConnConfig.TLSConfig != nil {
		t.Fatalf("preconditions: expected nil TLSConfig with sslmode=disable; got %+v", cfg.ConnConfig.TLSConfig)
	}
	if err := enforceRemoteTLS(cfg); !errors.Is(err, ErrInsecureRemoteDSN) {
		t.Fatalf("enforceRemoteTLS on remote plaintext: got %v, want ErrInsecureRemoteDSN", err)
	}
}

// TestEnforceRemoteTLS_AllowsLoopbackPlaintext confirms loopback hosts
// keep working without TLS (typical dev workflow).
func TestEnforceRemoteTLS_AllowsLoopbackPlaintext(t *testing.T) {
	cases := []string{
		"postgres://u:p@localhost/db?sslmode=disable",
		"postgres://u:p@127.0.0.1/db?sslmode=disable",
		// Unix-socket DSNs use libpq key=value format (host=/tmp/pg).
		// pgx accepts these, isLoopbackHost classifies the leading-slash
		// host as loopback — tested directly in TestIsLoopbackHost_Table.
		"host=/tmp/pg dbname=db user=u password=p sslmode=disable",
	}
	for _, dsn := range cases {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("ParseConfig %s: %v", dsn, err)
		}
		if err := enforceRemoteTLS(cfg); err != nil {
			t.Fatalf("enforceRemoteTLS on %s: got %v, want nil", dsn, err)
		}
	}
}

// TestEnforceRemoteTLS_AllowsRemoteWithTLS confirms a remote host with
// sslmode=require (or stronger) passes.
func TestEnforceRemoteTLS_AllowsRemoteWithTLS(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://u:p@db.prod.example.com/db?sslmode=require")
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.ConnConfig.TLSConfig == nil {
		t.Fatalf("preconditions: expected non-nil TLSConfig with sslmode=require")
	}
	if err := enforceRemoteTLS(cfg); err != nil {
		t.Fatalf("enforceRemoteTLS on remote+TLS: got %v, want nil", err)
	}
}

// TestNewPool_RejectsRemotePlaintextByDefault is the end-to-end
// equivalent of the TLS unit tests — confirms NewPool refuses a
// remote-plaintext DSN before attempting any network I/O.
func TestNewPool_RejectsRemotePlaintextByDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	pool, err := NewPool(ctx, Config{DSN: "postgres://u:p@db.prod.example.com/db?sslmode=disable"})
	if pool != nil {
		pool.Close()
	}
	if !errors.Is(err, ErrInsecureRemoteDSN) {
		t.Fatalf("expected ErrInsecureRemoteDSN, got %v", err)
	}
}

// TestIsLoopbackHost_Table table-tests the host classifier so the
// blast radius of the sslmode policy is provable.
func TestIsLoopbackHost_Table(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"", true},
		{"/tmp/pg", true},
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{"0.0.0.0", true},
		{"db.prod.example.com", false},
		{"10.0.0.5", false},
		{"my-host", false},
	}
	for _, tc := range cases {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Errorf("isLoopbackHost(%q): got %v, want %v", tc.host, got, tc.want)
		}
	}
}
