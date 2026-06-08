// Request-ID propagation + structured access logging. Every request
// gets a stable identifier — either supplied by the caller via
// X-Request-ID (typical when an upstream proxy already stamped one)
// or freshly generated as a UUID v4. The id flows through:
//
//  1. The request context (downstream handlers + DB calls receive it
//     via RequestIDFromContext for log correlation).
//  2. The Response header X-Request-ID (clients can quote it when
//     filing support tickets).
//  3. The structured access-log line emitted once per request at
//     INFO level when the handler returns.
//
// Order of wrapping (outer → inner):
//
//	WithRequestID -> WithAccessLog -> WithMetrics -> handler
//
// — so the access-log line carries the id, and the metrics
// histogram observes the same lifecycle.
package obs

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// contextKey is the unexported context-key type for the request id.
// Using a private type prevents accidental collision with other
// packages' string-based context keys.
type contextKey struct{ name string }

var requestIDKey = &contextKey{"request-id"}

// RequestIDHeader is the wire-format header name. Lowercase is the
// HTTP/2 norm and the de-facto industry convention.
const RequestIDHeader = "X-Request-Id"

// WithRequestID is a middleware that ensures a request-id is present
// on every request. If the inbound header is set and non-empty, it
// is preserved (allowing distributed tracing through proxies);
// otherwise a fresh UUID v4 is generated.
//
// The id is then made available on r.Context() via
// RequestIDFromContext and echoed in the response header so clients
// can correlate.
func WithRequestID(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set(RequestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		h(w, r.WithContext(ctx))
	}
}

// RequestIDFromContext extracts the request id stored on a context.
// Returns "" if the middleware was not applied — callers should fall
// back gracefully rather than panic.
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

// WithAccessLog emits one structured slog line per request at INFO
// when the inner handler returns. Fields: method, route, status,
// duration_ms, request_id, remote_addr. The line is deliberately
// terse — operators index on it via structured-log queries; the
// metrics handler already exposes percentile-rich histograms.
//
// `route` is the pattern-style route name (e.g. "/api/secrets") —
// passed by the caller because Go's stdlib mux doesn't surface the
// matched pattern back to the handler.
func WithAccessLog(logger *slog.Logger, route string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		h(rec, r)
		logger.Info(
			"request",
			"method", r.Method,
			"route", route,
			"status", rec.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFromContext(r.Context()),
			"remote_addr", r.RemoteAddr,
		)
	}
}
