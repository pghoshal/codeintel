package obs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMetrics_Handler_ExposesPrometheusFormat verifies the /metrics
// handler emits Prometheus text-format output containing the three
// expected metric families with the codeintel_http prefix.
func TestMetrics_Handler_ExposesPrometheusFormat(t *testing.T) {
	m := NewMetrics()
	// Record one request so the metrics have non-zero samples.
	wrapped := m.WithMetrics("/test", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	wrapped(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/test", nil))

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"codeintel_http_requests_total",
		"codeintel_http_request_duration_seconds",
		"codeintel_http_inflight_requests",
		`method="GET"`,
		`route="/test"`,
		`status="200"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestWithMetrics_CapturesStatusFromInnerHandler covers the
// statusRecorder behaviour: when the inner handler writes a non-200
// status, the metric label reflects it.
func TestWithMetrics_CapturesStatusFromInnerHandler(t *testing.T) {
	m := NewMetrics()
	wrapped := m.WithMetrics("/forbidden", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("nope"))
	})
	wrapped(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/forbidden", nil))

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `route="/forbidden",status="403"`) {
		t.Errorf("expected forbidden label, got body:\n%s", body)
	}
	if !strings.Contains(body, `method="PUT"`) {
		t.Errorf("expected PUT method label, got body:\n%s", body)
	}
}

// TestWithMetrics_DefaultsTo200WhenNoExplicitWriteHeader confirms the
// stdlib semantics: a handler that Write()s without WriteHeader()
// implicitly produces a 200 — the metric must record that.
func TestWithMetrics_DefaultsTo200WhenNoExplicitWriteHeader(t *testing.T) {
	m := NewMetrics()
	wrapped := m.WithMetrics("/default", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hi")
	})
	wrapped(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/default", nil))
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `status="200"`) {
		t.Errorf("default status not captured as 200")
	}
}

// TestWithMetrics_InflightDecrementsOnPanic confirms inflight
// gauge tracking is panic-safe: a panic in the inner handler must
// still decrement the gauge via defer.
func TestWithMetrics_InflightDecrementsOnPanic(t *testing.T) {
	m := NewMetrics()
	wrapped := m.WithMetrics("/panicker", func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})
	defer func() {
		// We expect the panic to propagate; assert that even so,
		// the gauge ended at 0 (decremented by the deferred
		// inflight.Dec).
		_ = recover()
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		body := rec.Body.String()
		if !strings.Contains(body, "codeintel_http_inflight_requests 0") {
			t.Errorf("inflight gauge not reset after panic; body:\n%s", body)
		}
	}()
	wrapped(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/panicker", nil))
}

// TestNewMetrics_FreshRegistryPerCall ensures two separate NewMetrics
// calls produce independent registries. Critical so test cases don't
// leak counter state across runs (which they would under the global
// registry).
func TestNewMetrics_FreshRegistryPerCall(t *testing.T) {
	a := NewMetrics()
	b := NewMetrics()
	if a.Registry == b.Registry {
		t.Fatalf("NewMetrics must produce a fresh registry per call")
	}
}
