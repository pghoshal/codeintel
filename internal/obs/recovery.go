// Panic-recovery middleware. Catches any panic that escapes a
// handler, logs it with the request-id + stack trace, and emits a
// 500 ServiceError envelope to the client. Without this, a single
// bad handler could crash the entire process — net/http's default
// behaviour is to log the stack and abruptly close the connection,
// leaving the client with no response body.
//
// Order of wrapping (outermost ring first):
//
//	WithPanicRecovery -> WithRequestID -> WithAccessLog -> WithMetrics -> handler
//
// Recovery sits outside RequestID so the recovered handler's
// access-log line + metric still fire (the inner handler returned,
// even abnormally). The recovery handler short-circuits the response
// envelope independently.
package obs

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// recoveryBody is the pre-marshalled 500 envelope. Tiny, static, so
// allocating once at init beats marshalling per-panic.
var recoveryBody = []byte(`{"statusCode":500,"errorCode":"UNEXPECTED_ERROR","message":"An unexpected error occurred"}`)

// WithPanicRecovery wraps the supplied handler in a defer-recover
// block. On panic:
//
//  1. The stack trace + panic value + request id are logged at
//     ERROR level so on-call has the full context.
//  2. If the response has not yet been written (status was never
//     set), a 500 + service-error envelope is emitted.
//  3. If a partial response was already written, the recovery
//     cannot rewrite the status code — but it still logs and
//     prevents the panic from crashing the process.
//
// The middleware NEVER re-panics — recovered errors are absorbed by
// design.
func WithPanicRecovery(logger *slog.Logger, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if logger != nil {
				logger.Error(
					"handler panic",
					"err", p,
					"request_id", RequestIDFromContext(r.Context()),
					"method", r.Method,
					"path", r.URL.Path,
					"stack", string(debug.Stack()),
				)
			}
			// Only write the envelope if the inner handler did not
			// already start the response. Writing twice would corrupt
			// the HTTP framing — `headerWrote` tracks the state.
			if !rec.headerWrote {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write(recoveryBody)
			}
		}()
		h(rec, r)
	}
}
