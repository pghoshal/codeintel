package api

import (
	"net/http"
)

// healthResponseBytes is the wire body for GET /api/health.
// Pre-computed at package init so the request path is a single
// Write — no JSON encode in the hot loop.
var healthResponseBytes = []byte(`{"status":"ok"}`)

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	// healthLogger is bound once at server construction so the
	// per-request path doesn't pay a slog .With() allocation.
	// The log level is Debug because liveness probes hit this
	// route ~1 Hz per pod; INFO on every hit inflates log volume
	// for zero operational value. Operators wanting verbose
	// health-probe logs can flip CODEINTEL_LOG_LEVEL=debug.
	s.healthLogger.Debug("health check")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(healthResponseBytes)
}
