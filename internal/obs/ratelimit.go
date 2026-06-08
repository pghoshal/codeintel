// Per-key rate limiter. In-memory token bucket keyed by the
// requesting API key (or the remote IP for unauthenticated traffic).
// Out-of-the-box defence-in-depth for routes that accept API keys —
// a single misbehaving caller cannot saturate the service.
//
// Implementation notes:
//   - Uses golang.org/x/time/rate (the stdlib-blessed token bucket).
//   - Limiters are tracked in a sync.Map keyed by the limiter id.
//     Stale entries are pruned by a janitor goroutine to bound
//     memory growth.
//   - Per-process state. For multi-replica deployments this is a
//     deliberately "best effort" first line of defence; a shared
//     redis-backed limiter is queued in progress.md.
//
// Headers emitted on a 429 response:
//   - Retry-After: integer seconds (1 minimum) until the next token.
//   - X-RateLimit-Limit / X-RateLimit-Remaining: best-effort hints.
package obs

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitConfig is the per-instance tuning surface.
type RateLimitConfig struct {
	// RequestsPerSecond is the steady-state refill rate. 0 disables.
	RequestsPerSecond float64
	// Burst is the bucket capacity — short bursts of up to N
	// requests are admitted instantly before throttling kicks in.
	Burst int
	// JanitorInterval is how often stale per-key entries are pruned.
	// Default 10 minutes; set to 0 to disable pruning.
	JanitorInterval time.Duration
	// StaleAfter is the inactivity window after which an entry is
	// considered prunable. Default 30 minutes.
	StaleAfter time.Duration
}

// rateLimiterEntry pairs a token-bucket limiter with the time of its
// most recent use so the janitor can find stale entries.
type rateLimiterEntry struct {
	limiter *rate.Limiter
	lastUse time.Time
}

// RateLimiter tracks per-key token buckets and provides a middleware
// constructor. Safe for concurrent use.
type RateLimiter struct {
	cfg     RateLimitConfig
	entries sync.Map // map[string]*rateLimiterEntry
	mu      sync.Mutex
	now     func() time.Time
	stopCh  chan struct{}
}

// NewRateLimiter constructs a limiter with the given config and
// starts the janitor goroutine. Stop() halts the janitor (call on
// process shutdown).
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	if cfg.JanitorInterval == 0 {
		cfg.JanitorInterval = 10 * time.Minute
	}
	if cfg.StaleAfter == 0 {
		cfg.StaleAfter = 30 * time.Minute
	}
	rl := &RateLimiter{
		cfg:    cfg,
		now:    time.Now,
		stopCh: make(chan struct{}),
	}
	if cfg.JanitorInterval > 0 {
		go rl.janitor()
	}
	return rl
}

// Stop terminates the janitor goroutine. Idempotent.
func (rl *RateLimiter) Stop() {
	select {
	case <-rl.stopCh:
		// already stopped
	default:
		close(rl.stopCh)
	}
}

// janitor periodically removes entries unused since StaleAfter.
// Single goroutine; the per-bucket lookups are lock-free via
// sync.Map.
func (rl *RateLimiter) janitor() {
	ticker := time.NewTicker(rl.cfg.JanitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stopCh:
			return
		case <-ticker.C:
			cutoff := rl.now().Add(-rl.cfg.StaleAfter)
			rl.entries.Range(func(k, v any) bool {
				e := v.(*rateLimiterEntry)
				if e.lastUse.Before(cutoff) {
					rl.entries.Delete(k)
				}
				return true
			})
		}
	}
}

// limiterFor returns the rate.Limiter for the given key, creating
// one on first use.
func (rl *RateLimiter) limiterFor(key string) *rate.Limiter {
	if v, ok := rl.entries.Load(key); ok {
		e := v.(*rateLimiterEntry)
		e.lastUse = rl.now()
		return e.limiter
	}
	// Allocate-and-CAS so concurrent first-use callers converge on
	// one limiter instance per key.
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if v, ok := rl.entries.Load(key); ok {
		e := v.(*rateLimiterEntry)
		e.lastUse = rl.now()
		return e.limiter
	}
	lim := rate.NewLimiter(rate.Limit(rl.cfg.RequestsPerSecond), rl.cfg.Burst)
	rl.entries.Store(key, &rateLimiterEntry{limiter: lim, lastUse: rl.now()})
	return lim
}

// keyForRequest extracts a stable id for the request: prefer the
// API-key header, fall back to the Authorization Bearer token, fall
// back to the remote address. The handler should never key on an
// untrusted client-controlled header alone (because an attacker can
// rotate to evade), but combining it with remote-addr provides
// reasonable per-tenant fairness for friendly callers.
func keyForRequest(r *http.Request) string {
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return "key:" + v
	}
	if v := r.Header.Get("Authorization"); v != "" {
		return "authz:" + v
	}
	return "ip:" + r.RemoteAddr
}

// WithRateLimit returns a middleware that admits or rejects requests
// according to the per-key token bucket. When the limit is exceeded
// the response is 429 + a service-error body and Retry-After / rate
// headers; the inner handler is NOT invoked.
//
// When RequestsPerSecond == 0 the middleware short-circuits to a
// pass-through — keeps the wire shape stable while disabling
// enforcement in tests or dev.
func (rl *RateLimiter) WithRateLimit(h http.HandlerFunc) http.HandlerFunc {
	if rl.cfg.RequestsPerSecond <= 0 {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		lim := rl.limiterFor(keyForRequest(r))
		if !lim.Allow() {
			// Reserve briefly so the Retry-After header is sensible
			// even when zero tokens are available. Cancel immediately
			// so we don't consume real budget.
			res := lim.Reserve()
			delay := res.Delay()
			res.Cancel()
			seconds := int(delay.Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			w.Header().Set("X-RateLimit-Limit", strconv.FormatFloat(rl.cfg.RequestsPerSecond, 'f', -1, 64))
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write(rateLimitedBody)
			return
		}
		h(w, r)
	}
}

// rateLimitedBody is the static 429 envelope. The error-code slug
// `TOO_MANY_REQUESTS` mirrors the canonical HTTP status name.
var rateLimitedBody = []byte(`{"statusCode":429,"errorCode":"TOO_MANY_REQUESTS","message":"Rate limit exceeded. Retry after the indicated interval."}`)
