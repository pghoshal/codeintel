// Kubernetes liveness + readiness probe handlers.
//
//	GET /healthz — liveness. Always 200 if the process is up. No DB,
//	  no auth. Failure means "kubelet, please restart this pod."
//
//	GET /readyz — readiness. Pings the database. Failure means
//	  "load balancer, please remove this pod from rotation until I
//	  recover."
//
// Both responses are tiny static JSON bodies. They never appear in
// the request-latency histogram (the routes() registration skips the
// metrics middleware for probe endpoints — probes happen O(1/sec)
// per pod and would otherwise dominate the histogram percentiles).
package api

import (
	"context"
	"net/http"
	"time"
)

// healthzBody is the pre-marshalled liveness response. Tiny enough
// that marshalling per-request would be silly — 13 bytes.
var healthzBody = []byte(`{"status":"live"}`)
var readyzOKBody = []byte(`{"status":"ready"}`)
var readyzNotReadyBody = []byte(`{"status":"not-ready"}`)

// handleLiveness returns 200 unconditionally. The kubelet calls this
// every few seconds; failing it triggers a pod restart.
func (s *Server) handleLiveness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(healthzBody)
}

// handleReadiness pings the configured DBPinger with a tight (1 s)
// timeout. Failure → 503 + `{"status":"not-ready"}` so the LB stops
// sending traffic; recovery → 200 + `{"status":"ready"}` and traffic
// resumes.
//
// When Config.DBPinger is nil (e.g. constructor that doesn't pass
// one), the handler conservatively returns 503 — "I'm up but I
// cannot confirm the DB is reachable, do not send me traffic."
func (s *Server) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DBPinger == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(readyzNotReadyBody)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 1*time.Second)
	defer cancel()
	if err := s.cfg.DBPinger.Ping(ctx); err != nil {
		s.healthLogger.Warn("readiness probe failed db ping", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(readyzNotReadyBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(readyzOKBody)
}
