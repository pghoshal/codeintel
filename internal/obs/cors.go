// CORS middleware. Lets browser-based clients (Atom console, the
// codeintel web UI when split from this binary, dev-mode localhost)
// call /api/* across origins. Strict allow-list — wildcards must be
// configured explicitly via "*" entries.
//
// Implementation notes:
//   - Allowed origins are an exact-match set; "*" enables wildcard.
//   - Preflight (OPTIONS) requests short-circuit with 204 + the
//     Access-Control-* headers. The inner handler is NEVER invoked
//     on preflight (so probe / metrics / read handlers don't see
//     phantom OPTIONS traffic).
//   - Vary: Origin is always emitted so caches don't conflate
//     cross-origin responses.
package obs

import (
	"net/http"
	"strings"
)

// CORSConfig is the per-instance allow-list + options.
type CORSConfig struct {
	// AllowedOrigins is the set of exact origins permitted. A single
	// entry of "*" enables wildcard mode (any origin admitted) —
	// dangerous in production; for dev only.
	AllowedOrigins []string
	// AllowedMethods defaults to a sensible API set.
	AllowedMethods []string
	// AllowedHeaders includes the auth header by default.
	AllowedHeaders []string
	// MaxAgeSeconds is the preflight cache duration. Default 600.
	MaxAgeSeconds int
	// AllowCredentials enables `Access-Control-Allow-Credentials:
	// true`. Cannot combine with wildcard origin per the spec.
	AllowCredentials bool
}

// CORSMiddleware carries the resolved config + a fast lookup set.
type CORSMiddleware struct {
	cfg          CORSConfig
	originSet    map[string]struct{}
	wildcard     bool
	methodsStr   string
	headersStr   string
	maxAgeHeader string
}

// NewCORSMiddleware constructs a CORSMiddleware. nil-safe — calling
// .Wrap on a nil receiver returns the passthrough handler.
func NewCORSMiddleware(cfg CORSConfig) *CORSMiddleware {
	if len(cfg.AllowedMethods) == 0 {
		cfg.AllowedMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"}
	}
	if len(cfg.AllowedHeaders) == 0 {
		cfg.AllowedHeaders = []string{"Authorization", "Content-Type", "X-Api-Key", "X-Request-Id"}
	}
	if cfg.MaxAgeSeconds == 0 {
		cfg.MaxAgeSeconds = 600
	}
	m := &CORSMiddleware{
		cfg:        cfg,
		originSet:  make(map[string]struct{}),
		methodsStr: strings.Join(cfg.AllowedMethods, ", "),
		headersStr: strings.Join(cfg.AllowedHeaders, ", "),
	}
	m.maxAgeHeader = intToStr(cfg.MaxAgeSeconds)
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			m.wildcard = true
		} else {
			m.originSet[o] = struct{}{}
		}
	}
	return m
}

// intToStr avoids strconv import; the Max-Age value is bounded so a
// 12-digit decimal is plenty.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// originAllowed reports whether the supplied request Origin is
// permitted by the configured allow-list. Empty Origin (same-origin
// request) is always allowed.
func (m *CORSMiddleware) originAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if m.wildcard {
		return true
	}
	_, ok := m.originSet[origin]
	return ok
}

// Wrap returns a middleware that:
//   - On preflight (OPTIONS with Origin + Access-Control-Request-*
//     headers): emits the Access-Control-* response set + 204 and
//     returns WITHOUT calling the inner handler.
//   - On a normal CORS request (Origin set, method != OPTIONS):
//     emits Access-Control-Allow-Origin (echo of the request origin)
//     and Vary: Origin, then invokes the inner handler.
//   - On same-origin requests (no Origin header): pass-through.
//
// Disallowed origins still pass through to the handler but WITHOUT
// the Allow-Origin header, so the browser's CORS check rejects the
// response — the canonical CORS-failure behaviour.
//
// nil receiver: returns the supplied handler unchanged (CORS
// disabled).
func (m *CORSMiddleware) Wrap(h http.HandlerFunc) http.HandlerFunc {
	if m == nil {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Always vary on Origin so caches don't conflate.
		w.Header().Add("Vary", "Origin")

		if m.originAllowed(origin) && origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			if m.cfg.AllowCredentials && !m.wildcard {
				w.Header().Set("Access-Control-Allow-Credentials", "true")
			}
		}

		// Preflight: OPTIONS with Access-Control-Request-Method.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Set("Access-Control-Allow-Methods", m.methodsStr)
			// Echo the requested headers when the client supplies them;
			// otherwise expose the default set.
			if hdrs := r.Header.Get("Access-Control-Request-Headers"); hdrs != "" {
				w.Header().Set("Access-Control-Allow-Headers", hdrs)
			} else {
				w.Header().Set("Access-Control-Allow-Headers", m.headersStr)
			}
			w.Header().Set("Access-Control-Max-Age", m.maxAgeHeader)
			w.WriteHeader(http.StatusNoContent)
			return
		}

		h(w, r)
	}
}
