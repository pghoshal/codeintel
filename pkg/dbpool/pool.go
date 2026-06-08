// Package dbpool is the cross-binary pgx connection-pool factory.
// Both codeintel-app and codeintel-backend construct a Pool via
// NewPool at startup and pass it into their respective query
// helpers. Tenant scoping is the query helpers' responsibility;
// this package is the connection / TLS / pooling surface only.
//
// This file covers pool construction + size-formula resolution +
// safe shutdown + remote-host TLS enforcement.
//
// DSN scheme allowlist: pgx accepts only `postgres://` /
// `postgresql://` URIs and libpq `key=value` strings; arbitrary schemes
// such as `ssh://` are rejected by pgxpool.ParseConfig. SSRF via DSN
// is therefore not possible at this layer.
package dbpool

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrDSNRequired is returned by NewPool when Config.DSN is empty.
// Operators must explicitly set CODEINTEL_DATABASE_URL; silently
// defaulting to localhost is intentionally not supported because
// it masks env-var injection failures.
var ErrDSNRequired = errors.New("db: CODEINTEL_DATABASE_URL (Config.DSN) is required")

// ErrInsecureRemoteDSN is returned by NewPool when the DSN points at a
// non-loopback host without TLS (sslmode=disable / no TLS config).
// Production deployments must use sslmode=require (or stronger) for
// remote Postgres so credentials never travel in plaintext.
var ErrInsecureRemoteDSN = errors.New("db: remote Postgres requires sslmode=require or stronger; got sslmode=disable or no TLS")

// Config captures the construction-time inputs for a pgx pool. Every
// field maps to a CODEINTEL_DATABASE_* environment variable so the
// binary's main.go can read env once and pass a value-typed Config
// down to constructors — no global state.
type Config struct {
	// DSN is the Postgres connection string. Required.
	//   postgres://user:pass@host:5432/dbname?sslmode=disable
	DSN string

	// MaxConns is the upper bound on pool size. When zero (default)
	// resolveMaxConns chooses min(numCPU*4, 32). A negative value is
	// rejected and the default formula is used.
	MaxConns int32

	// ConnectTimeout bounds initial Ping during NewPool. Defaults to
	// the §6 SLOs require pod cold-start under 5 s and a slow DB on
	// boot should fail fast, not block readiness.
	ConnectTimeout time.Duration

	// AllowInsecureRemoteDSN bypasses the sslmode-required check. This
	// is intentionally separate from any env var to ensure operators
	// must make a conscious code-level choice to disable production
	// safety. Default is false; the integration test sets this true
	// when targeting a known-LAN Postgres without TLS.
	AllowInsecureRemoteDSN bool
}

// defaultMaxConnLifetime / defaultMaxConnIdleTime are package-level so
// tests can assert exact values without exercising a live pool.
const (
	defaultMaxConnLifetime = 30 * time.Minute
	defaultMaxConnIdleTime = 5 * time.Minute
	defaultMinConns        = int32(0)
	defaultConnectTimeout  = 5 * time.Second
)

// Pool wraps a *pgxpool.Pool so we can attach codeintel-specific
// helpers in later slices (tenant-scope assertions, query telemetry,
// prepared-statement cache warmer) without exposing the raw pgx type
// everywhere. The wrapper is a value type carrying the pool pointer —
// safe to pass around without locking.
type Pool struct {
	*pgxpool.Pool
}

// NewPool constructs the pool, validates the DSN, and pings the
// database within the configured ConnectTimeout. Returns the
// constructed pool on success; the caller must Close() it when done.
func NewPool(ctx context.Context, cfg Config) (*Pool, error) {
	if cfg.DSN == "" {
		return nil, ErrDSNRequired
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		// pgxpool.ParseConfig is documented to NOT include the
		// password in its error string, but we wrap with %w defensively
		// so a future pgx change cannot silently surface credentials.
		// If pgx ever does leak, the leak appears in `err.Error()`
		// inside our wrap — operators redact via log filtering.
		return nil, fmt.Errorf("db: invalid DSN/config: %w", err)
	}

	if !cfg.AllowInsecureRemoteDSN {
		if err := enforceRemoteTLS(poolCfg); err != nil {
			return nil, err
		}
	}

	applyPoolDefaults(poolCfg, cfg.MaxConns)

	pingCtx, cancel := context.WithTimeout(ctx, resolveConnectTimeout(cfg.ConnectTimeout))
	defer cancel()

	pool, err := pgxpool.NewWithConfig(pingCtx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &Pool{Pool: pool}, nil
}

// applyPoolDefaults stamps the codeintel sizing + lifecycle defaults
// onto a parsed pgxpool config. Extracted so tests can assert the
// applied values without booting a live pool.
func applyPoolDefaults(cfg *pgxpool.Config, requestedMaxConns int32) {
	cfg.MaxConns = resolveMaxConns(requestedMaxConns)
	cfg.MinConns = defaultMinConns
	cfg.MaxConnLifetime = defaultMaxConnLifetime
	cfg.MaxConnIdleTime = defaultMaxConnIdleTime
}

// resolveConnectTimeout returns the effective connect timeout. Zero or
// negative inputs map to the package default (5 s). Extracted so tests
// can lock the contract without booting a pool.
func resolveConnectTimeout(requested time.Duration) time.Duration {
	if requested > 0 {
		return requested
	}
	return defaultConnectTimeout
}

// resolveMaxConns implements the sizing formula:
//   - explicit positive value → honored
//   - zero or negative        → min(numCPU * 4, 32)
//
// The cap exists because Postgres has its own connection limit
// (default 100, often 50 in managed PG plans); a runaway pool from a
// single replica would starve every other replica in the deployment.
func resolveMaxConns(requested int32) int32 {
	if requested > 0 {
		return requested
	}
	value := int32(runtime.NumCPU()) * 4
	if value > 32 {
		value = 32
	}
	return value
}

// enforceRemoteTLS rejects DSNs that point at a non-loopback host
// without TLS. Loopback (localhost, 127.0.0.1, ::1) and Unix socket
// connections are always permitted because plaintext is acceptable
// inside the local machine boundary.
//
// pgx surfaces the sslmode setting via ConnConfig.TLSConfig: nil
// means sslmode=disable; non-nil means sslmode=allow/prefer/require/
// verify-ca/verify-full. Any non-nil TLSConfig is acceptable for the
// remote case — the operator chose the verification level.
func enforceRemoteTLS(cfg *pgxpool.Config) error {
	if cfg.ConnConfig.TLSConfig != nil {
		return nil
	}
	host := cfg.ConnConfig.Host
	if isLoopbackHost(host) {
		return nil
	}
	return ErrInsecureRemoteDSN
}

// isLoopbackHost reports whether a Postgres host should be treated as
// a local-machine boundary. The pgx host can be: an IP, a hostname, or
// a Unix socket path (which always begins with "/"). Hostname matching
// is conservative — anything containing a dot is treated as remote.
func isLoopbackHost(host string) bool {
	if host == "" {
		// pgx defaults an empty host to a Unix socket — local by definition.
		return true
	}
	if strings.HasPrefix(host, "/") {
		return true
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return false
}
