// Package asynqbridge holds the cross-binary glue that converts a
// codeintel Redis URL into an asynq.RedisConnOpt and adapts a
// *slog.Logger to asynq.Logger. Both shapes are needed by:
//
//   - codeintel-backend (asynq.Server consumer side)
//   - codeintel-app    (asynq.Client producer side, future
//     enqueue handlers)
//   - internal/asynqsmoke (E2E gate test)
//
// The package is deliberately tiny — no Client / Server wrapping,
// no enqueue-time policy. Those land where the producer or
// consumer logic lives.
//
// Why not roll the bridge into pkg/asynqueues? The asynqueues
// package is a pure name registry with zero external deps; adding
// asynq + go-redis to it would surface those transitively to
// every package that imports queue names. Keeping the bridge
// separate lets a caller (e.g., a static-analysis tool that just
// wants the queue strings) import asynqueues without dragging
// asynq in.
package asynqbridge

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// ErrURLRequired is returned when RedisOptFromURL receives an
// empty string. errors.Is-friendly so callers can branch.
var ErrURLRequired = errors.New("asynqbridge: redis URL is required")

// ErrURLScheme is returned when the URL is not a redis:// or
// rediss:// URL. Defense-in-depth — production callers route
// through pkg/redisclient which already validates this, but
// asynqbridge is package-level and may receive raw URLs from
// future callers (CLI tools, debug scripts) that haven't gone
// through that gate.
var ErrURLScheme = errors.New("asynqbridge: redis URL must be redis:// or rediss://")

// RedisOptFromURL converts a redis:// or rediss:// URL into an
// asynq.RedisClientOpt. The function is a thin wrapper around
// go-redis's ParseURL because asynq accepts a *RedisClientOpt
// (not a URL string), and asynq's own asynq.ParseRedisURI doesn't
// cover the rediss:// (TLS) + userinfo case the codeintel
// production deployment uses.
//
// Returns an asynq.RedisConnOpt interface (not the concrete
// *RedisClientOpt) so a future migration to RedisFailoverClientOpt
// (sentinel) or RedisClusterClientOpt is a single-line surface
// swap in the caller.
//
// TLS handling: redis.ParseURL respects the rediss:// scheme and
// populates *redis.Options.TLSConfig with a non-nil
// *tls.Config{}; the asynq.RedisClientOpt below copies it
// verbatim so TLS is preserved. mTLS + custom CA pinning is NOT
// handled here — those flow through pkg/redisclient.Config and
// are out-of-scope for the asynq client (asynq doesn't issue
// general Redis commands needing those knobs, only the BRPOPLPUSH-
// style blocking pops asynq's internal client owns).
func RedisOptFromURL(rawURL string) (asynq.RedisConnOpt, error) {
	if rawURL == "" {
		return nil, ErrURLRequired
	}
	// Defense-in-depth scheme guard. pkg/redisclient already
	// rejects non-redis schemes, but asynqbridge's exported
	// surface is independent and may be reached by a debug-CLI
	// or test path that doesn't route through redisclient.
	if u, perr := url.Parse(rawURL); perr != nil || (u.Scheme != "redis" && u.Scheme != "rediss") {
		return nil, ErrURLScheme
	}
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("asynqbridge: parse redis URL: %w", err)
	}
	// TLS hardening: go-redis's ParseURL populates TLSConfig
	// with no MinVersion (defaults to Go's stdlib floor — TLS
	// 1.0 client). pkg/redisclient.toRedisOptions explicitly
	// pins MinVersion = tls.VersionTLS12. The asynq Redis dial
	// must use the same floor — otherwise two clients to the
	// same Redis negotiate at different TLS versions, which is
	// a real-world security drift.
	if opts.TLSConfig != nil {
		if opts.TLSConfig.MinVersion == 0 {
			opts.TLSConfig.MinVersion = tls.VersionTLS12
		}
	}
	return asynq.RedisClientOpt{
		Network:   opts.Network,
		Addr:      opts.Addr,
		Username:  opts.Username,
		Password:  opts.Password,
		DB:        opts.DB,
		TLSConfig: opts.TLSConfig,
	}, nil
}

// SlogLogger adapts a *slog.Logger to asynq.Logger. asynq's
// internal logging hits Debug/Info/Warn/Error/Fatal; we map each
// to the matching slog level. Fatal maps to Error because slog
// doesn't have a separate Fatal level (and a real-fatal shouldn't
// be the queue library's call anyway).
//
// Usage: pass &SlogLogger{Base: someLogger} into asynq.Config.Logger.
// The adapter calls (*slog.Logger).With("component", "asynq")
// once at first emit so every line is tagged for filtering, and
// passes the asynq-side message as slog's standard "msg" field
// (no duplicate-key collision with extra attributes).
type SlogLogger struct {
	Base *slog.Logger

	once   sync.Once
	tagged *slog.Logger
}

// Debug, Info, Warn, Error, Fatal satisfy asynq.Logger. Each
// formats the variadic args via fmt.Sprint and passes the result
// as slog's message field, so the JSON-handler output has one
// "msg" key per line.
func (l *SlogLogger) Debug(args ...interface{}) { l.log(slog.LevelDebug, args...) }
func (l *SlogLogger) Info(args ...interface{})  { l.log(slog.LevelInfo, args...) }
func (l *SlogLogger) Warn(args ...interface{})  { l.log(slog.LevelWarn, args...) }
func (l *SlogLogger) Error(args ...interface{}) { l.log(slog.LevelError, args...) }
func (l *SlogLogger) Fatal(args ...interface{}) { l.log(slog.LevelError, args...) }

func (l *SlogLogger) log(level slog.Level, args ...interface{}) {
	if l == nil || l.Base == nil {
		return
	}
	l.once.Do(func() {
		l.tagged = l.Base.With("component", "asynq")
	})
	l.tagged.Log(nil, level, fmt.Sprint(args...))
}
