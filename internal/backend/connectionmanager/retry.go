// Shared retry helper for the codehost fetchers. Mirrors the
// legacy fetchWithRetry pattern (utils.ts) without dragging in
// a third-party retry library.
//
// Policy:
//   - Up to N attempts (default 3).
//   - Exponential backoff with jitter: 200ms, ~400ms, ~800ms.
//   - Retry only on transient classes:
//       * net.Error with Timeout() == true.
//       * HTTP 5xx surfaced via *RetryableHTTPError.
//       * SDK-wrapped transport errors flagged via shouldRetry.
//   - Never retry on:
//       * 4xx HTTP errors (caller's fault).
//       * context cancellation.
//
// Callers wrap their per-attempt closure in WithRetry. The
// closure returns the value + an optional "retryable" flag via
// RetryableHTTPError or a net.Error.
package connectionmanager

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"
)

// RetryConfig captures the per-call policy. Zero values fall
// back to defaults at WithRetry call time.
type RetryConfig struct {
	MaxAttempts int           // default 3
	BaseDelay   time.Duration // default 200ms
	MaxDelay    time.Duration // default 2s (caps the exponential)
}

const (
	defaultRetryMaxAttempts = 3
	defaultRetryBaseDelay   = 200 * time.Millisecond
	defaultRetryMaxDelay    = 2 * time.Second
)

// RetryableHTTPError is the typed-error a fetcher returns from
// its per-attempt closure to signal a 5xx-style transient error.
// 4xx errors should NOT use this wrapper - they're caller
// errors and shouldn't be retried.
type RetryableHTTPError struct {
	StatusCode int
	URL        string
	Body       string
}

func (e *RetryableHTTPError) Error() string {
	return fmt.Sprintf("retryable HTTP %d at %s: %s", e.StatusCode, e.URL, e.Body)
}

// IsRetryable returns true when err represents a transient
// failure worth re-trying. Walks the error chain.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	// Typed sentinel.
	var rhe *RetryableHTTPError
	if errors.As(err, &rhe) {
		return rhe.StatusCode >= 500 && rhe.StatusCode < 600
	}
	// net.Error timeouts.
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	// EOF on response read is treated as transient.
	if errors.Is(err, errClosedConnection) {
		return true
	}
	return false
}

// errClosedConnection is a sentinel callers can return when
// they detect a closed-connection signal that warrants a
// retry. Most HTTP clients surface this via io.ErrUnexpectedEOF
// or connection-reset net.OpError; this sentinel exists for
// callers that want to flag a custom-detected close.
var errClosedConnection = errors.New("connectionmanager: closed connection (retry)")

// WithRetry runs fn up to cfg.MaxAttempts times, sleeping with
// exponential backoff + jitter between attempts. Stops early
// on:
//   - First non-retryable error.
//   - Context cancellation.
//   - First nil-error return (success).
//
// Returns the last error encountered (or fn's nil-success).
func WithRetry[T any](ctx context.Context, cfg RetryConfig, fn func(ctx context.Context, attempt int) (T, error)) (T, error) {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultRetryMaxAttempts
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = defaultRetryBaseDelay
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = defaultRetryMaxDelay
	}

	var (
		zero    T
		lastErr error
	)
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return zero, lastErr
			}
			return zero, err
		}
		v, err := fn(ctx, attempt)
		if err == nil {
			return v, nil
		}
		lastErr = err
		if !IsRetryable(err) {
			return zero, err
		}
		if attempt == cfg.MaxAttempts-1 {
			break
		}
		// Compute backoff with jitter: base * 2^attempt + [0..base).
		backoff := cfg.BaseDelay << attempt
		if backoff > cfg.MaxDelay {
			backoff = cfg.MaxDelay
		}
		jitter := time.Duration(rand.Int63n(int64(cfg.BaseDelay)))
		sleep := backoff + jitter
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(sleep):
		}
	}
	return zero, lastErr
}
