package redisclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/redis/go-redis/v9"
)

// Client is the production-side Redis wrapper. Holds a go-redis
// *Client plus the typed Config (kept for diagnostics + redacted
// logging).
//
// Client is safe for concurrent use — go-redis's *Client
// internally pools connections; every public method on Client
// delegates to it.
type Client struct {
	r      *redis.Client
	cfg    Config
	logger *slog.Logger
}

// New constructs a Client. The client is initialised eagerly with
// a PING under the supplied context, so a Redis-down state
// surfaces at startup rather than at first Enqueue.
//
// Caller MUST call (*Client).Close on shutdown to drain the
// connection pool.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	cfg = cfg.WithDefaults()

	opts, err := cfg.toRedisOptions()
	if err != nil {
		return nil, err
	}

	r := redis.NewClient(opts)
	c := &Client{
		r:      r,
		cfg:    cfg,
		logger: logger.With("logger", "redisclient"),
	}

	if err := c.Ping(ctx); err != nil {
		_ = r.Close()
		return nil, fmt.Errorf("redisclient: startup ping: %w", err)
	}
	return c, nil
}

// Ping verifies the Redis server is reachable + responding. Used
// by New's eager-init path and by external readiness probes
// (e.g., the future /readyz handler on codeintel-backend).
func (c *Client) Ping(ctx context.Context) error {
	if c == nil || c.r == nil {
		return errors.New("redisclient: client is nil")
	}
	if _, err := c.r.Ping(ctx).Result(); err != nil {
		return fmt.Errorf("redisclient: ping: %w", err)
	}
	return nil
}

// Close drains the connection pool. Safe to call on a nil Client
// (no-op) so caller-side `defer c.Close()` doesn't have to
// nil-check.
func (c *Client) Close() error {
	if c == nil || c.r == nil {
		return nil
	}
	return c.r.Close()
}

// Underlying returns the embedded *redis.Client. Reserved for
// packages that need direct access (asynq's options expect a
// redis.UniversalOptions or a connection-string; this accessor
// is the typed escape hatch).
//
// Production code outside the asynq wiring should NOT use this
// — operate through the Client's typed methods instead so the
// instrumentation hooks (future redis-cmd metrics) catch every
// call site.
func (c *Client) Underlying() *redis.Client {
	if c == nil {
		return nil
	}
	return c.r
}

// Config returns a redacted copy of the config the client was
// built from (the redacted-by-LogValue form). Used by /readyz
// handlers to report what Redis they're talking to without
// leaking credentials.
func (c *Client) Config() Config {
	if c == nil {
		return Config{}
	}
	out := c.cfg
	out.URL = redactURL(out.URL)
	return out
}
