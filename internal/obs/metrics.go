// Prometheus-format metrics + middleware. Every HTTP route the api
// package registers automatically gets request-count + latency
// histogram observability via WithMetrics(). Exposed via
// /metrics (no auth, no-cache; intended for in-cluster Prometheus
// scrape only — operators must NOT expose the port externally).
//
// Histogram buckets are tuned for the §6 SLOs in
// docs/codeintel-porting-plan.md (p50 < 5 ms, p99 < 50 ms). 14
// boundaries between 1 ms and 5 s give visible structure across two
// orders of magnitude without ballooning cardinality.
package obs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metric names follow Prometheus convention: namespace + subsystem +
// metric. The namespace "codeintel" + subsystem "http" produces names
// like `codeintel_http_requests_total` — clean, predictable, and
// rate-of-change friendly via the _total suffix.
const (
	metricNamespace = "codeintel"
	metricSubsystem = "http"
)

// Metrics carries the registered counter + histogram so callers
// (the api Server) can pass them to the middleware. Holding the
// registry on the struct lets tests construct a fresh registry per
// run, avoiding the global registry's cross-test interference.
type Metrics struct {
	Registry        *prometheus.Registry
	RequestsTotal   *prometheus.CounterVec
	LatencySeconds  *prometheus.HistogramVec
	InflightCurrent prometheus.Gauge
}

// NewMetrics constructs a Metrics with a fresh registry and registers
// the three core HTTP metrics. The returned object is safe for
// concurrent use; the Prometheus client uses lock-free atomic
// counters and per-bucket atomics for histograms.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		RequestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "requests_total",
				Help:      "Total HTTP requests grouped by method, route, and status code.",
			},
			[]string{"method", "route", "status"},
		),
		LatencySeconds: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: metricNamespace,
				Subsystem: metricSubsystem,
				Name:      "request_duration_seconds",
				Help:      "End-to-end HTTP request latency, in seconds.",
				Buckets: []float64{
					0.001, 0.002, 0.005, // sub-5ms — inside p50 SLO
					0.010, 0.025, 0.050, // 10ms-50ms — inside p99 SLO
					0.100, 0.250, 0.500, // tail
					1.000, 2.500, 5.000, // outliers
				},
			},
			[]string{"method", "route", "status"},
		),
		InflightCurrent: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricNamespace,
			Subsystem: metricSubsystem,
			Name:      "inflight_requests",
			Help:      "HTTP requests currently being served by this process.",
		}),
	}
	reg.MustRegister(m.RequestsTotal, m.LatencySeconds, m.InflightCurrent)
	return m
}

// Handler returns the /metrics endpoint handler bound to this
// Metrics' registry. Caller mounts it at the desired path.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{
		EnableOpenMetrics: false, // emit classic Prometheus text format only
	})
}

// statusRecorder wraps an http.ResponseWriter to capture the status
// code written by the inner handler. Defaults to 200 for handlers
// that never call WriteHeader (Go's stdlib semantics — the first
// Write call implicitly writes 200).
type statusRecorder struct {
	http.ResponseWriter
	statusCode  int
	headerWrote bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.headerWrote {
		return
	}
	s.statusCode = code
	s.headerWrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.headerWrote {
		s.statusCode = http.StatusOK
		s.headerWrote = true
	}
	return s.ResponseWriter.Write(b)
}

// WithMetrics wraps a handler so that every request increments the
// counter and observes the latency histogram. The `route` label
// should be the pattern-style route (e.g. "/api/secrets"), NOT the
// raw URL path — otherwise high-cardinality URLs like
// "/api/secrets/{key}" with thousands of distinct keys would
// explode the metric series count.
//
// Caller supplies the route string explicitly because Go's
// http.ServeMux does not expose the matched-pattern back to the
// handler (yet). When we migrate to chi or echo this can become
// automatic via router-supplied middleware.
func (m *Metrics) WithMetrics(route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.InflightCurrent.Inc()
		defer m.InflightCurrent.Dec()
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		h(rec, r)
		elapsed := time.Since(start).Seconds()
		status := strconv.Itoa(rec.statusCode)
		m.RequestsTotal.WithLabelValues(r.Method, route, status).Inc()
		m.LatencySeconds.WithLabelValues(r.Method, route, status).Observe(elapsed)
	}
}
