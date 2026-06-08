package nebulaclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	nebula "github.com/vesoft-inc/nebula-go/v3"
)

// ErrQueryFailed wraps a non-success ResultSet error code so the
// caller can branch via errors.Is rather than reading the Result
// struct directly.
var ErrQueryFailed = errors.New("nebulaclient: query returned a non-success error code")

// Client is the production wrapper around a nebula-go connection
// pool. Holds the pool, the credentials it authenticates with,
// the configured space (auto-USE'd at session start), and a slog
// logger bound at construction.
//
// Client is safe for concurrent use; the underlying ConnectionPool
// serialises access via its internal mutex, and Execute checks out
// a session for the duration of a single call.
type Client struct {
	mu     sync.Mutex
	pool   *nebula.ConnectionPool
	cfg    Config
	logger *slog.Logger
}

// New constructs a Client. The pool is initialised eagerly — a
// graphd that's down on first connect surfaces as a New error
// rather than a delayed Execute error, matching the readiness
// contract operators expect at process startup.
//
// The supplied context bounds the initial pool initialisation;
// after New returns the pool's own per-call timeout (Config.Timeout)
// governs every subsequent Execute.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	logger = logger.With("logger", "nebulaclient")

	pool, err := newConnectionPool(cfg, logger)
	if err != nil {
		return nil, fmt.Errorf("nebulaclient: pool init: %w", err)
	}

	// Eager Ping under the supplied context: if graphd is unreachable
	// at startup we fail fast with a typed error rather than queueing
	// every later Execute behind a stalled pool.
	c := &Client{pool: pool, cfg: cfg, logger: logger}
	if err := c.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("nebulaclient: startup ping: %w", err)
	}
	return c, nil
}

// Close drains the underlying pool. Safe to call on a partially-
// constructed Client (handled defensively in New).
func (c *Client) Close() {
	if c == nil || c.pool == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pool.Close()
	c.pool = nil
}

// Ping verifies that a fresh session against the configured
// credentials succeeds. Currently a noop nGQL statement
// (`YIELD 1`) is the cheapest valid graphd probe. The context's
// deadline (if any) bounds the round-trip; the wrapper does not
// add its own internal timeout to the ping.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.execute(ctx, "YIELD 1")
	return err
}

// Execute runs an nGQL statement and returns the raw ResultSet on
// success. The session is checked out from the pool, the optional
// USE <space> dispatched, the statement executed, and the session
// released back to the pool — all within the supplied context's
// deadline.
//
// A ResultSet with a non-success error code surfaces as
// ErrQueryFailed wrapped with the Nebula error message; transport
// errors surface raw.
func (c *Client) Execute(ctx context.Context, stmt string) (*nebula.ResultSet, error) {
	if stmt == "" {
		return nil, errors.New("nebulaclient: empty statement")
	}
	return c.execute(ctx, stmt)
}

// execute is the shared body for Ping + Execute. Splitting it lets
// Ping skip the empty-statement guard (it passes its own literal).
func (c *Client) execute(ctx context.Context, stmt string) (*nebula.ResultSet, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	session, err := c.getSession()
	if err != nil {
		return nil, fmt.Errorf("nebulaclient: get session: %w", err)
	}

	if c.cfg.Space != "" {
		if _, useErr := session.Execute("USE " + quoteSpace(c.cfg.Space) + ";"); useErr != nil {
			session.Release()
			c.logger.Error("USE space failed", "space", c.cfg.Space, "err", useErr)
			return nil, fmt.Errorf("nebulaclient: USE %s: %w", c.cfg.Space, useErr)
		}
	}

	// nebula-go's Execute does not accept a context, so honour the
	// caller's deadline by running the call on a goroutine and
	// selecting on ctx.Done. Cancellation does NOT cancel the
	// in-flight RPC; the goroutine runs to completion bounded by
	// the pool's TimeOut, and ONLY THEN releases the session.
	//
	// CRITICAL: nebula-go's *Session is not safe for concurrent
	// use, and releasing a session back to the pool while a call
	// is still in flight on it would let the next pool acquirer
	// observe an in-use session. Release MUST therefore happen
	// inside the goroutine after Execute returns, NOT via a defer
	// in the caller frame. The goroutine takes full ownership of
	// the session lifetime from this point on.
	type result struct {
		rs  *nebula.ResultSet
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		rs, runErr := session.Execute(stmt)
		session.Release()
		done <- result{rs: rs, err: runErr}
	}()

	select {
	case r := <-done:
		dur := time.Since(start)
		if r.err != nil {
			c.logger.Error("execute transport failed", "err", r.err, "duration_ms", dur.Milliseconds())
			return nil, fmt.Errorf("nebulaclient: execute: %w", r.err)
		}
		if !r.rs.IsSucceed() {
			msg := r.rs.GetErrorMsg()
			c.logger.Error("execute query failed", "code", r.rs.GetErrorCode(), "msg", msg, "duration_ms", dur.Milliseconds())
			return nil, fmt.Errorf("%w: code=%d msg=%q", ErrQueryFailed, r.rs.GetErrorCode(), msg)
		}
		c.logger.Debug("execute succeeded", "rows", r.rs.GetRowSize(), "duration_ms", dur.Milliseconds())
		return r.rs, nil
	case <-ctx.Done():
		// Goroutine continues; it will Release the session when
		// nebula-go returns (bounded by pool TimeOut). The buffered
		// channel of size 1 absorbs the eventual write without
		// blocking the orphan goroutine.
		c.logger.Warn("execute cancelled by caller context", "err", ctx.Err())
		return nil, ctx.Err()
	}
}

func newConnectionPool(cfg Config, logger *slog.Logger) (*nebula.ConnectionPool, error) {
	poolCfg := nebula.GetDefaultConf()
	poolCfg.MaxConnPoolSize = cfg.PoolSize
	poolCfg.IdleTime = cfg.IdleTime
	poolCfg.TimeOut = cfg.Timeout
	return nebula.NewConnectionPool(cfg.Addrs, poolCfg, slogNebulaLogger{logger: logger})
}

func (c *Client) getSession() (*nebula.Session, error) {
	pool := c.currentPool()
	if pool == nil {
		return nil, errors.New("client is closed")
	}
	session, err := pool.GetSession(c.cfg.Username, c.cfg.Password)
	if err == nil {
		return session, nil
	}
	c.logger.Warn("acquire session failed; resetting pool once", "err", err)
	if resetErr := c.resetPool(); resetErr != nil {
		return nil, fmt.Errorf("%v; pool reset failed: %w", err, resetErr)
	}
	pool = c.currentPool()
	if pool == nil {
		return nil, errors.New("client is closed")
	}
	session, retryErr := pool.GetSession(c.cfg.Username, c.cfg.Password)
	if retryErr != nil {
		c.logger.Error("acquire session retry failed", "err", retryErr)
		return nil, fmt.Errorf("%v; retry: %w", err, retryErr)
	}
	return session, nil
}

func (c *Client) currentPool() *nebula.ConnectionPool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pool
}

func (c *Client) resetPool() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pool == nil {
		return errors.New("client is closed")
	}
	next, err := newConnectionPool(c.cfg, c.logger)
	if err != nil {
		return err
	}
	old := c.pool
	c.pool = next
	old.Close()
	return nil
}

// quoteSpace wraps a space identifier in backticks. Nebula's nGQL
// identifier-quote character is `, mirroring MySQL. Embedded
// backticks are doubled; embedded newlines are rejected at the
// boundary (a space name should never contain one).
func quoteSpace(s string) string {
	// Defence-in-depth: a hostile space name set via env should
	// never break out of the USE clause. Forbidden chars cause the
	// caller to receive an empty-quoted name which graphd will
	// reject — visible failure mode rather than silent escape.
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' || s[i] == 0 {
			return "``"
		}
	}
	out := make([]byte, 0, len(s)+2)
	out = append(out, '`')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '`' {
			out = append(out, '`', '`')
			continue
		}
		out = append(out, c)
	}
	out = append(out, '`')
	return string(out)
}

// slogNebulaLogger bridges nebula-go's Logger interface onto
// codeintel's slog.Logger. Levels map best-effort: nebula's Info
// → slog.Debug (verbose pool churn shouldn't drown out app
// signals), Warn → Warn, Error/Fatal → Error.
type slogNebulaLogger struct {
	logger *slog.Logger
}

func (l slogNebulaLogger) Info(msg string)  { l.logger.Debug("nebula-go: " + msg) }
func (l slogNebulaLogger) Warn(msg string)  { l.logger.Warn("nebula-go: " + msg) }
func (l slogNebulaLogger) Error(msg string) { l.logger.Error("nebula-go: " + msg) }
func (l slogNebulaLogger) Fatal(msg string) { l.logger.Error("nebula-go (fatal): " + msg) }
