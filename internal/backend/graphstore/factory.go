package graphstore

import (
	"context"
	"log/slog"
	"os"

	"codeintel/pkg/graphschema"
	"codeintel/pkg/nebulaclient"
	nebula "github.com/vesoft-inc/nebula-go/v3"
)

// CreateFromEnv is the codeintel-backend startup entry point for
// the graph writer. Direct port of
// packages/backend/src/codeGraph/store.ts:139-158
// (createCodeGraphStore), reworked around the codeintel env
// convention (CODEINTEL_NEBULA_* — slice 40 baseline) and the
// existing pkg/nebulaclient wrapper instead of a hand-rolled
// admin/data client pair.
//
// Decision rules:
//
//   - CODEINTEL_NEBULA_ADDR unset → returns an
//     UnconfiguredCodeGraphStore. The backend logs the
//     diagnostic and proceeds — a deployment without a graph
//     backend is a valid configuration (writes return SKIPPED
//     silently).
//   - CODEINTEL_NEBULA_ADDR set but invalid / unreachable →
//     returns an UnavailableCodeGraphStore wrapping the typed
//     error. Graph queue writes retry instead of terminally
//     skipping work from one unhealthy pod.
//   - Happy path → NebulaCodeGraphStore wrapping a space-
//     prefixed executor over the live nebulaclient pool.
//
// The returned Closer's Close() shuts the underlying pool;
// always non-nil so callers can `defer closer.Close()`
// unconditionally.
func CreateFromEnv(ctx context.Context, logger *slog.Logger) (Store, Closer) {
	if logger == nil {
		logger = slog.Default()
	}

	if os.Getenv(nebulaclient.EnvAddr) == "" {
		store := NewUnconfiguredStore(nebulaclient.EnvAddr + " is not configured")
		logger.Info("graph writer disabled: env var not set",
			"env", nebulaclient.EnvAddr,
		)
		return store, noopCloser{}
	}

	cfg, err := nebulaclient.LoadConfigFromEnv()
	if err != nil {
		store := NewUnavailableStore("env validation failed: " + err.Error())
		logger.Error("graph writer unavailable: env validation failed", "err", err)
		return store, noopCloser{}
	}

	client, err := nebulaclient.New(ctx, cfg, logger)
	if err != nil {
		store := NewUnavailableStore("nebula client init failed: " + err.Error())
		logger.Error("graph writer unavailable: client init failed", "err", err)
		return store, noopCloser{}
	}

	// The codeintel space MUST be bootstrapped before the store
	// can issue per-call USE-prefixed statements. Bootstrap is
	// idempotent — re-runs against an existing space are no-ops.
	if err := graphschema.Bootstrap(ctx, client, graphschema.BootstrapOptions{}); err != nil {
		client.Close()
		store := NewUnavailableStore("graph schema bootstrap failed: " + err.Error())
		logger.Error("graph writer unavailable: bootstrap failed", "err", err)
		return store, noopCloser{}
	}

	executor := &spacePrefixedExecutor{
		client: client,
		prefix: "USE `" + graphschema.SpaceName + "`; ",
	}
	logger.Info("graph writer ready",
		"addr_count", len(cfg.Addrs),
		"space", graphschema.SpaceName,
	)
	return New(executor, logger), &clientCloser{client: client}
}

// Closer is the local lifecycle interface CreateFromEnv returns
// alongside the store. Kept narrow (just Close) so callers can
// `defer closer.Close()` without dragging the io package in.
type Closer interface {
	Close() error
}

// noopCloser is returned alongside an UnconfiguredCodeGraphStore
// so caller-side `defer closer.Close()` stays uniform.
type noopCloser struct{}

func (noopCloser) Close() error { return nil }

// clientCloser wraps a *nebulaclient.Client's Close method into
// the local Closer interface. nebulaclient.Client.Close has no
// error return; this adapter always returns nil.
type clientCloser struct {
	client *nebulaclient.Client
}

func (c *clientCloser) Close() error {
	if c == nil || c.client == nil {
		return nil
	}
	c.client.Close()
	return nil
}

// spacePrefixedExecutor adapts a bare *nebulaclient.Client
// (space-agnostic per slice 40 design) to the store's
// expectation that every statement runs inside the codeintel
// space. Prepends `USE codeintel; ` to each Execute call —
// nebula-go's Execute accepts ;-separated batches and returns
// the final statement's ResultSet, so the prefix is invisible
// to callers.
type spacePrefixedExecutor struct {
	client *nebulaclient.Client
	prefix string
}

// Execute satisfies NgqlExecutor. The supplied context bounds
// the round-trip via nebulaclient.Client.Execute's existing
// goroutine + select pattern.
func (e *spacePrefixedExecutor) Execute(ctx context.Context, stmt string) (*nebula.ResultSet, error) {
	return e.client.Execute(ctx, e.prefix+stmt)
}
