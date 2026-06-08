// Package obs holds the codeintel observability primitives:
// structured logger, request-id propagation, access logging,
// Prometheus metrics, panic recovery, rate limiting, and CORS.
// Every dependency in this package is optional from the caller's
// perspective so handlers can opt-in piecewise.
package obs

import (
	"log/slog"
	"os"
)

// NewLogger returns the process-wide JSON-formatted logger.
// Callers extend it with `.With("logger", "<name>")` at construction
// time so structured-log queries can scope by subsystem.
//
// The default level is INFO; debug output is gated by
// CODEINTEL_LOG_LEVEL=debug.
func NewLogger() *slog.Logger {
	level := slog.LevelInfo
	if os.Getenv("CODEINTEL_LOG_LEVEL") == "debug" {
		level = slog.LevelDebug
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	return slog.New(handler)
}
