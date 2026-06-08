// Package redisclient is the cross-binary Redis client wrapper.
// Ports packages/backend/src/redis.ts to Go — the legacy file
// constructs an ioredis client from a URL + optional TLS knobs;
// the Go port mirrors that shape using
// github.com/redis/go-redis/v9 (the standard Go Redis client +
// the one asynq builds on internally).
//
// Used by:
//   - codeintel-backend's asynq Client (enqueue side) and
//     Server (worker side).
//   - codeintel-app's asynq Client when a handler enqueues a
//     job (e.g., POST /api/connections/{id}/sync).
//   - Future cache layers (search-result cache, session
//     store) — out of slice scope but the package shape
//     supports them.
//
// Documented divergences from the TS source:
//
//   - Legacy `redis.ts:48` sets `maxRetriesPerRequest: null` —
//     ioredis requires this for BullMQ's BRPOPLPUSH-style
//     blocking pops. The Go port does NOT mirror it: asynq
//     manages its own go-redis client for blocking-pop work
//     (separate from this wrapper), so the wrapper itself
//     stays at go-redis defaults. Non-asynq callers (cache,
//     /readyz probe) get the default per-call retry which
//     matches operator expectation.
//   - Five legacy TLS knobs are NOT ported on the Go side
//     because crypto/tls handles them via different surfaces:
//       legacy `secureProtocol`       — TLS version is set
//                                       via `MinVersion` /
//                                       `MaxVersion`.
//       legacy `ciphers`              — `CipherSuites` field.
//                                       Not exposed in
//                                       Config yet — add
//                                       when a production
//                                       deployment requires
//                                       a non-default suite.
//       legacy `honorCipherOrder`     — `PreferServerCipherSuites`.
//                                       Server-side knob;
//                                       N/A for client.
//       legacy `key_passphrase`       — Go expects keys
//                                       decrypted at load
//                                       time via
//                                       `LoadX509KeyPair`;
//                                       wrap with
//                                       `pem.Decrypt` if
//                                       needed at the call
//                                       site.
//       legacy `checkServerIdentity:
//         () => undefined`             — equivalent to
//                                       `InsecureSkipVerify=true`,
//                                       which is exposed via
//                                       `TLSInsecureSkipVerify`.
package redisclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Env-var names. Operator-facing; documented in
// codeintel/README.md alongside the rest of the env surface.
const (
	EnvURL = "CODEINTEL_REDIS_URL"

	// TLS knobs. Mirror legacy REDIS_TLS_* names but under the
	// CODEINTEL_ prefix.
	EnvTLSEnabled            = "CODEINTEL_REDIS_TLS_ENABLED"
	EnvTLSCAPath             = "CODEINTEL_REDIS_TLS_CA_PATH"
	EnvTLSCertPath           = "CODEINTEL_REDIS_TLS_CERT_PATH"
	EnvTLSKeyPath            = "CODEINTEL_REDIS_TLS_KEY_PATH"
	EnvTLSRejectUnauthorized = "CODEINTEL_REDIS_TLS_REJECT_UNAUTHORIZED"
	EnvTLSServerName         = "CODEINTEL_REDIS_TLS_SERVERNAME"
)

// Sentinel errors. errors.Is-friendly so callers can branch
// without parsing strings.
var (
	ErrURLRequired = errors.New("redisclient: CODEINTEL_REDIS_URL is required")
	ErrURLInvalid  = errors.New("redisclient: CODEINTEL_REDIS_URL is malformed")
)

// Config is the dependency surface. The zero value is unusable
// on purpose — operators construct via LoadConfigFromEnv (prod)
// or set fields directly (tests).
type Config struct {
	// URL is the redis:// or rediss:// dial string. rediss://
	// switches the dialer to TLS automatically; the explicit
	// TLSEnabled flag is the override for "force TLS even on
	// a plain redis:// URL" (e.g., when the URL comes from a
	// secret store that doesn't include the scheme).
	URL string

	// TLSEnabled forces TLS regardless of URL scheme. Mirrors
	// the legacy `env.REDIS_TLS_ENABLED === "true"` branch in
	// redis.ts line 6.
	TLSEnabled bool

	// TLSCAPath / TLSCertPath / TLSKeyPath are optional paths
	// to PEM files for mTLS or custom CA validation.
	TLSCAPath   string
	TLSCertPath string
	TLSKeyPath  string

	// TLSInsecureSkipVerify disables hostname verification.
	// Mirrors `rejectUnauthorized=false` in the legacy. NOT
	// recommended for production; the env var carries an
	// explicit operator opt-in.
	TLSInsecureSkipVerify bool

	// TLSServerName overrides the SNI / verified hostname when
	// the URL host differs from the certificate's CN
	// (e.g., behind a load balancer).
	TLSServerName string

	// DialTimeout / ReadTimeout / WriteTimeout bound the
	// per-operation network waits. Reasonable defaults set
	// via WithDefaults.
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
}

// LoadConfigFromEnv reads every env var listed above and
// produces a typed Config. Missing required vars return the
// matching sentinel error.
func LoadConfigFromEnv() (Config, error) {
	raw := os.Getenv(EnvURL)
	if raw == "" {
		return Config{}, ErrURLRequired
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "redis" && parsed.Scheme != "rediss") {
		return Config{}, fmt.Errorf("%w: scheme must be redis:// or rediss://", ErrURLInvalid)
	}

	cfg := Config{
		URL:                   raw,
		TLSEnabled:            strings.EqualFold(os.Getenv(EnvTLSEnabled), "true"),
		TLSCAPath:             os.Getenv(EnvTLSCAPath),
		TLSCertPath:           os.Getenv(EnvTLSCertPath),
		TLSKeyPath:            os.Getenv(EnvTLSKeyPath),
		TLSServerName:         os.Getenv(EnvTLSServerName),
		TLSInsecureSkipVerify: strings.EqualFold(os.Getenv(EnvTLSRejectUnauthorized), "false"),
	}
	return cfg.WithDefaults(), nil
}

// WithDefaults fills zero-valued timing fields with operationally-
// reasonable defaults. Calling code (test or env loader) hands
// off a Config; WithDefaults is idempotent.
func (c Config) WithDefaults() Config {
	if c.DialTimeout <= 0 {
		c.DialTimeout = 5 * time.Second
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 3 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 3 * time.Second
	}
	return c
}

// LogValue implements slog.LogValuer so an accidental
// logger.Info("startup", "cfg", cfg) emits a redacted form. The
// URL is masked to scheme+host:port (no userinfo / no password
// segment); TLS paths emit as file presence flags only.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", redactURL(c.URL)),
		slog.Bool("tls_enabled", c.TLSEnabled),
		slog.Bool("tls_ca_set", c.TLSCAPath != ""),
		slog.Bool("tls_cert_set", c.TLSCertPath != ""),
		slog.Bool("tls_key_set", c.TLSKeyPath != ""),
		slog.Bool("tls_skip_verify", c.TLSInsecureSkipVerify),
		slog.String("tls_servername", c.TLSServerName),
		slog.Duration("dial_timeout", c.DialTimeout),
		slog.Duration("read_timeout", c.ReadTimeout),
		slog.Duration("write_timeout", c.WriteTimeout),
	)
}

// redactURL strips the userinfo segment so a Redis URL like
// `redis://user:secret@host:6379/0` logs as
// `redis://host:6379/0`. Defensive — operators inevitably log
// configs.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	u.User = nil
	return u.String()
}

// String mirrors LogValue so `%v` / `%+v` / `fmt.Errorf("%v", cfg)`
// can't leak credentials accidentally.
func (c Config) String() string {
	return c.LogValue().String()
}

// toRedisOptions converts the typed Config into go-redis's
// ParseURL-then-augment form. Returns the *redis.Options the
// client constructor wants.
//
// The flow:
//
//  1. redis.ParseURL parses the URL into base options (host,
//     port, db, password from the URL's userinfo).
//  2. The Config's per-op timeouts override the defaults.
//  3. TLS is wired if the URL scheme is rediss:// OR if
//     Config.TLSEnabled is true.
//  4. Optional CA / cert / key files are loaded and pinned.
func (c Config) toRedisOptions() (*redis.Options, error) {
	opts, err := redis.ParseURL(c.URL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrURLInvalid, err)
	}
	opts.DialTimeout = c.DialTimeout
	opts.ReadTimeout = c.ReadTimeout
	opts.WriteTimeout = c.WriteTimeout

	useTLS := strings.HasPrefix(c.URL, "rediss://") || c.TLSEnabled
	if !useTLS {
		return opts, nil
	}

	tlsCfg := &tls.Config{
		InsecureSkipVerify: c.TLSInsecureSkipVerify,
		ServerName:         c.TLSServerName,
		MinVersion:         tls.VersionTLS12,
	}

	if c.TLSCAPath != "" {
		caBytes, err := os.ReadFile(c.TLSCAPath)
		if err != nil {
			return nil, fmt.Errorf("redisclient: load CA from %q: %w", c.TLSCAPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caBytes) {
			return nil, fmt.Errorf("redisclient: CA at %q is not valid PEM", c.TLSCAPath)
		}
		tlsCfg.RootCAs = pool
	}

	if c.TLSCertPath != "" || c.TLSKeyPath != "" {
		if c.TLSCertPath == "" || c.TLSKeyPath == "" {
			return nil, errors.New("redisclient: both TLSCertPath and TLSKeyPath must be set for mTLS")
		}
		cert, err := tls.LoadX509KeyPair(c.TLSCertPath, c.TLSKeyPath)
		if err != nil {
			return nil, fmt.Errorf("redisclient: load mTLS cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	opts.TLSConfig = tlsCfg
	return opts, nil
}

// validate is the boundary guard used by New + tests.
func (c Config) validate() error {
	if c.URL == "" {
		return ErrURLRequired
	}
	u, err := url.Parse(c.URL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrURLInvalid, err)
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return fmt.Errorf("%w: scheme %q is neither redis:// nor rediss://", ErrURLInvalid, u.Scheme)
	}
	// net.SplitHostPort handles IPv6 brackets — the hand-rolled
	// LastIndex(":") approach the earlier draft used broke
	// `redis://[::1]:6379`.
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrURLInvalid, err)
	}
	if host == "" || portStr == "" {
		return fmt.Errorf("%w: host or port empty in %q", ErrURLInvalid, u.Host)
	}
	// Reject empty / non-numeric / out-of-range port without
	// dragging strconv into the package's main file.
	var port int
	if _, scanErr := fmt.Sscanf(portStr, "%d", &port); scanErr != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("%w: port %q must be 1-65535", ErrURLInvalid, portStr)
	}
	return nil
}
