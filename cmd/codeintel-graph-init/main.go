// codeintel-graph-init is the one-shot bootstrap binary that
// creates the codeintel NebulaGraph SPACE plus every TAG / EDGE /
// TAG INDEX the graph reader and writer expect. Intended to be
// run as a Kubernetes Job once per deploy (the operator reruns
// safely — every statement uses IF NOT EXISTS).
//
// Architecture note: this is NOT one of the three service
// binaries in docs/codeintel-architecture-rules.md §1 — it's a
// one-shot init job (k8s-Job-shaped, not Deployment-shaped). The
// service-binary list intentionally excludes init / migration
// tooling.
//
// Configuration comes from the same env contract pkg/nebulaclient
// already exposes:
//
//	CODEINTEL_NEBULA_ADDR       e.g. 127.0.0.1:9669 (or comma-sep list)
//	CODEINTEL_NEBULA_USER       e.g. root
//	CODEINTEL_NEBULA_PASSWORD   (operator-rotated; default `nebula`)
//	CODEINTEL_NEBULA_POOL_SIZE  optional, default 4
//
// Bootstrap-specific overrides (also optional):
//
//	CODEINTEL_NEBULA_PARTITIONS     default 10
//	CODEINTEL_NEBULA_REPLICA_FACTOR default 1
//	CODEINTEL_NEBULA_VID_LENGTH     default 128
package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"codeintel/pkg/graphschema"
	"codeintel/pkg/nebulaclient"
)

const (
	envPartitions    = "CODEINTEL_NEBULA_PARTITIONS"
	envReplicaFactor = "CODEINTEL_NEBULA_REPLICA_FACTOR"
	envVidLength     = "CODEINTEL_NEBULA_VID_LENGTH"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(logger); err != nil {
		logger.Error("graph-init failed", "err", err)
		os.Exit(1)
	}
	logger.Info("graph-init succeeded")
}

func run(logger *slog.Logger) error {
	cfg, err := nebulaclient.LoadConfigFromEnv()
	if err != nil {
		return err
	}
	// Bootstrap statements include schema-side index builds that
	// can take longer than a YIELD-1 ping; widen the per-call
	// timeout so a slow first-deploy doesn't surface as a timeout
	// error.
	cfg.Timeout = 30 * time.Second
	logger.Info("loaded config", "cfg", cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client, err := nebulaclient.New(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer client.Close()

	opts := graphschema.BootstrapOptions{
		PartitionNum:  envIntOrDefault(logger, envPartitions, 0),
		ReplicaFactor: envIntOrDefault(logger, envReplicaFactor, 0),
		VidLength:     envIntOrDefault(logger, envVidLength, 0),
	}
	logger.Info("running graph schema bootstrap",
		"space", graphschema.SpaceName,
		"partition_num", opts.PartitionNum,
		"replica_factor", opts.ReplicaFactor,
		"vid_length", opts.VidLength,
	)
	return graphschema.Bootstrap(ctx, client, opts)
}

// envIntOrDefault returns the integer value of the named env var
// or the supplied default. An unset value uses the default
// silently; an unparseable or non-positive value uses the default
// AND emits a warning so an operator typo (e.g. PARTITIONS=ten)
// doesn't silently coast on the fallback.
func envIntOrDefault(logger *slog.Logger, name string, fallback int) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		logger.Warn("invalid env var value, using default", "name", name, "raw", raw, "fallback", fallback, "err", err)
		return fallback
	}
	return n
}
