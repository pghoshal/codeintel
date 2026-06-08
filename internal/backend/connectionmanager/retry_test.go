package connectionmanager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestWithRetry_SucceedsFirstAttempt: no retry needed.
func TestWithRetry_SucceedsFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	got, err := WithRetry(context.Background(), RetryConfig{},
		func(ctx context.Context, attempt int) (int, error) {
			calls.Add(1)
			return 42, nil
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d want 42", got)
	}
	if calls.Load() != 1 {
		t.Errorf("calls: got %d want 1", calls.Load())
	}
}

// TestWithRetry_RetriesOn5xx: 2 failures + 1 success.
func TestWithRetry_RetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	got, err := WithRetry(context.Background(),
		RetryConfig{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond},
		func(ctx context.Context, attempt int) (string, error) {
			calls.Add(1)
			if attempt < 2 {
				return "", &RetryableHTTPError{StatusCode: 503, URL: "x", Body: "boom"}
			}
			return "ok", nil
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "ok" {
		t.Errorf("got %q", got)
	}
	if calls.Load() != 3 {
		t.Errorf("calls: got %d want 3", calls.Load())
	}
}

// TestWithRetry_StopsOnNon5xx: 4xx never retries.
func TestWithRetry_StopsOnNon5xx(t *testing.T) {
	var calls atomic.Int32
	_, err := WithRetry(context.Background(),
		RetryConfig{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond},
		func(ctx context.Context, attempt int) (int, error) {
			calls.Add(1)
			return 0, &RetryableHTTPError{StatusCode: 404, URL: "x", Body: "nope"}
		})
	// 404 inside RetryableHTTPError is NOT retryable per IsRetryable
	// (only 5xx is).
	if err == nil {
		t.Fatalf("expected error")
	}
	if calls.Load() != 1 {
		t.Errorf("calls: got %d want 1 (no retry on 4xx)", calls.Load())
	}
}

// TestWithRetry_StopsOnPlainError: ordinary errors aren't
// retryable.
func TestWithRetry_StopsOnPlainError(t *testing.T) {
	var calls atomic.Int32
	plain := errors.New("plain old error")
	_, err := WithRetry(context.Background(),
		RetryConfig{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond},
		func(ctx context.Context, attempt int) (int, error) {
			calls.Add(1)
			return 0, plain
		})
	if !errors.Is(err, plain) {
		t.Errorf("got %v want plain", err)
	}
	if calls.Load() != 1 {
		t.Errorf("calls: got %d want 1", calls.Load())
	}
}

// TestWithRetry_ExhaustsAttempts: all attempts return 5xx.
func TestWithRetry_ExhaustsAttempts(t *testing.T) {
	var calls atomic.Int32
	_, err := WithRetry(context.Background(),
		RetryConfig{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond},
		func(ctx context.Context, attempt int) (int, error) {
			calls.Add(1)
			return 0, &RetryableHTTPError{StatusCode: 500, URL: "x", Body: "boom"}
		})
	if err == nil {
		t.Fatalf("expected error after exhausting")
	}
	if calls.Load() != 3 {
		t.Errorf("calls: got %d want 3", calls.Load())
	}
}

// TestWithRetry_AbortsOnCtxCancel pins the cancellation path.
func TestWithRetry_AbortsOnCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	_, err := WithRetry(ctx,
		RetryConfig{MaxAttempts: 5, BaseDelay: 50 * time.Millisecond},
		func(ctx context.Context, attempt int) (int, error) {
			calls.Add(1)
			if attempt == 0 {
				// Cancel from inside the first attempt's callback.
				cancel()
			}
			return 0, &RetryableHTTPError{StatusCode: 500, URL: "x", Body: "y"}
		})
	if err == nil {
		t.Fatalf("expected error after cancel")
	}
	// First attempt runs, then cancellation prevents further attempts.
	if calls.Load() > 1 {
		t.Errorf("calls: got %d, expected at most 1 after cancel", calls.Load())
	}
}

// netTimeoutErr is a minimal net.Error implementation that
// claims Timeout()==true. The retry helper treats these as
// transient and retryable.
type netTimeoutErr struct{}

func (e netTimeoutErr) Error() string   { return "timeout" }
func (e netTimeoutErr) Timeout() bool   { return true }
func (e netTimeoutErr) Temporary() bool { return true }

var _ net.Error = netTimeoutErr{}

// TestWithRetry_RetriesOnNetTimeout: net.Error.Timeout() == true
// is retryable.
func TestWithRetry_RetriesOnNetTimeout(t *testing.T) {
	var calls atomic.Int32
	got, err := WithRetry(context.Background(),
		RetryConfig{MaxAttempts: 3, BaseDelay: 1 * time.Millisecond},
		func(ctx context.Context, attempt int) (int, error) {
			calls.Add(1)
			if attempt < 1 {
				return 0, netTimeoutErr{}
			}
			return 99, nil
		})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 99 {
		t.Errorf("got %d want 99", got)
	}
	if calls.Load() != 2 {
		t.Errorf("calls: got %d want 2", calls.Load())
	}
}

// TestIsRetryable_VariousErrors covers the classification logic.
func TestIsRetryable_VariousErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"5xx", &RetryableHTTPError{StatusCode: 503}, true},
		{"500", &RetryableHTTPError{StatusCode: 500}, true},
		{"4xx", &RetryableHTTPError{StatusCode: 404}, false},
		{"2xx-wrapped-not-retryable", &RetryableHTTPError{StatusCode: 200}, false},
		{"plain-error", errors.New("oops"), false},
		{"net-timeout", netTimeoutErr{}, true},
		{"wrapped-5xx", fmt.Errorf("outer: %w", &RetryableHTTPError{StatusCode: 502}), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}
