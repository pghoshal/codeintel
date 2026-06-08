package nebulaclient

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	nebula "github.com/vesoft-inc/nebula-go/v3"
)

// TestNew_RejectsInvalidConfig confirms the validate boundary
// fires before any network attempt: bad config → typed error, no
// pool created.
func TestNew_RejectsInvalidConfig(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	_, err := New(ctx, Config{}, logger)
	if !errors.Is(err, ErrAddrRequired) {
		t.Errorf("empty cfg: want ErrAddrRequired, got %v", err)
	}

	_, err = New(ctx, Config{
		Addrs:    []nebula.HostAddress{{Host: "h", Port: 9669}},
		Username: "",
		Password: "p",
		PoolSize: 4,
	}, logger)
	if !errors.Is(err, ErrUserRequired) {
		t.Errorf("missing user: want ErrUserRequired, got %v", err)
	}
}

// TestNew_PoolInitFailureSurfacesError confirms an unreachable
// graphd surfaces as a New error rather than queueing every later
// Execute behind a dead pool. We point at a closed loopback port
// (using 1 = privileged, definitely not bound) and verify New
// returns within the ping context's deadline.
func TestNew_PoolInitFailureSurfacesError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2_500_000_000) // 2.5s
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg := Config{
		Addrs:    []nebula.HostAddress{{Host: "127.0.0.1", Port: 1}},
		Username: "root",
		Password: "nebula",
		PoolSize: 1,
	}
	_, err := New(ctx, cfg, logger)
	if err == nil {
		t.Fatalf("want New to fail against a closed port, got nil error")
	}
	// Surface should clearly identify the wrapper origin.
	if !strings.Contains(err.Error(), "nebulaclient") {
		t.Errorf("error should be wrapper-scoped: %v", err)
	}
}

// TestQuoteSpace_HappyAndHostile locks the nGQL identifier-quoting
// helper. Operator-supplied space names never reach graphd
// unquoted; embedded backticks are doubled; control chars cause
// an empty quoted name (graphd rejects, visible failure).
func TestQuoteSpace_HappyAndHostile(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "codeintel", "`codeintel`"},
		{"with underscore", "code_intel", "`code_intel`"},
		{"with backtick", "weird`name", "`weird``name`"},
		{"with newline", "evil\nspace", "``"},
		{"with cr", "evil\rspace", "``"},
		{"with null byte", "evil\x00space", "``"},
		{"empty", "", "``"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := quoteSpace(tc.in)
			if got != tc.want {
				t.Errorf("quoteSpace(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestExecute_EmptyStatementRejected locks the boundary guard so a
// caller bug that builds an empty nGQL string doesn't waste a
// pool round-trip.
func TestExecute_EmptyStatementRejected(t *testing.T) {
	// Constructing a Client without a real pool means we set the
	// fields directly. Execute's empty-string check fires before
	// any pool dispatch.
	c := &Client{
		cfg:    Config{Username: "u", Password: "p"},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	_, err := c.Execute(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty statement") {
		t.Errorf("want empty-statement error, got %v", err)
	}
}

// TestExecute_RespectsContextCancellation confirms a cancelled
// context returns ctx.Err() without dispatching to the pool. The
// nil-pool Client would otherwise panic when GetSession is called,
// so a clean ctx-err return proves the early check fires.
func TestExecute_RespectsContextCancellation(t *testing.T) {
	c := &Client{
		cfg:    Config{Username: "u", Password: "p"},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Execute(ctx, "YIELD 1")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

// TestClose_NilSafe locks the defensive shape: Close on a nil
// Client (e.g., after a failed New) is a no-op.
func TestClose_NilSafe(t *testing.T) {
	var c *Client
	c.Close() // must not panic
}
