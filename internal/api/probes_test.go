package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"codeintel/internal/obs"
)

// fakeDBPinger lets tests simulate Ping outcomes.
type fakeDBPinger struct{ err error }

func (f *fakeDBPinger) Ping(ctx context.Context) error { return f.err }

func newProbeServer(pinger DBPinger, withMetrics bool) *Server {
	cfg := Config{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		DBPinger: pinger,
	}
	if withMetrics {
		cfg.Metrics = obs.NewMetrics()
	}
	return NewServer(cfg)
}

// TestHealthz_AlwaysReturns200 locks the liveness contract — no
// dependencies, no auth, body byte-equal.
func TestHealthz_AlwaysReturns200(t *testing.T) {
	srv := newProbeServer(nil, false)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"live"}` {
		t.Fatalf("body: got %q, want %q", got, `{"status":"live"}`)
	}
}

// TestReadyz_WithoutPinger_Returns503 confirms the conservative
// default — no pinger means we cannot prove the DB is reachable,
// so we report not-ready (the LB removes this pod from rotation).
func TestReadyz_WithoutPinger_Returns503(t *testing.T) {
	srv := newProbeServer(nil, false)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"not-ready"}` {
		t.Fatalf("body: got %q", got)
	}
}

// TestReadyz_HappyPath confirms a successful DB ping yields 200 +
// ready body.
func TestReadyz_HappyPath(t *testing.T) {
	srv := newProbeServer(&fakeDBPinger{}, false)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{"status":"ready"}` {
		t.Fatalf("body: got %q", got)
	}
}

// TestReadyz_PingFailureReturns503 covers the DB-outage path.
func TestReadyz_PingFailureReturns503(t *testing.T) {
	srv := newProbeServer(&fakeDBPinger{err: errors.New("connection refused")}, false)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
}

// TestMetricsEndpoint_MountedWhenConfigured asserts /metrics returns
// 200 when Config.Metrics is supplied and 404 otherwise.
func TestMetricsEndpoint_MountedWhenConfigured(t *testing.T) {
	with := newProbeServer(nil, true)
	without := newProbeServer(nil, false)

	rec := httptest.NewRecorder()
	with.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("with Metrics: status %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	without.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("without Metrics: status %d, want 404", rec.Code)
	}
}

// TestMetrics_RecordsApiRouteTraffic confirms the WithMetrics wiring
// in routes() actually fires on a non-probe request — i.e. a /api/...
// call increments the metric.
func TestMetrics_RecordsApiRouteTraffic(t *testing.T) {
	srv := newProbeServer(nil, true)

	// Hit /healthz — but healthz is NOT wrapped by metrics, so it
	// should NOT appear in the histogram. Use /api/version instead,
	// which IS wrapped.
	req := httptest.NewRequest(http.MethodGet, "/api/version", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	// Scrape /metrics and check the version route is present.
	mRec := httptest.NewRecorder()
	srv.Router().ServeHTTP(mRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := mRec.Body.String()
	if !contains(body, `route="/api/version"`) {
		t.Fatalf("/metrics does not show /api/version traffic; body:\n%s", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
