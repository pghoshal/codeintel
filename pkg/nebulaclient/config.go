// Package nebulaclient is the cross-binary production client
// wrapper around vesoft-inc/nebula-go. Both codeintel-app
// (read path, MCP graph_* tools) and the future codeintel-graph
// binary (writer + reader services per docs/codeintel-porting-plan.md
// §3.1) construct one of these; the package therefore lives under
// pkg/ rather than internal/.
//
// The wrapper exists to keep nebula-go's lower-level Session /
// ConnectionPool surface from leaking across every call site, to
// centralise pool sizing + per-query timeout + structured logging,
// and to gate operator-supplied config (Space-name allowlist at
// env-parse time, credential redaction in slog output, eager
// startup ping). Application-level retry policy is deferred to a
// follow-up slice — nebula-go's ConnectionPool already performs
// transparent reconnect on transport-level failures.
package nebulaclient

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	nebula "github.com/vesoft-inc/nebula-go/v3"
)

// spaceNamePattern is Nebula's documented identifier grammar for
// graph spaces: letter / underscore followed by alnum / underscore,
// up to 128 chars total. Enforcing it at the config boundary closes
// every quoting / injection vector quoteSpace defends against
// — invalid space names never reach the USE clause.
var spaceNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

// Env-var names. Operators wire these via a Kubernetes Secret /
// Helm values / docker-compose env; the wrapper never reads
// credentials from source.
const (
	EnvAddr      = "CODEINTEL_NEBULA_ADDR"
	EnvUser      = "CODEINTEL_NEBULA_USER"
	EnvPassword  = "CODEINTEL_NEBULA_PASSWORD"
	EnvSpace     = "CODEINTEL_NEBULA_SPACE"
	EnvPoolSize  = "CODEINTEL_NEBULA_POOL_SIZE"
	EnvTimeoutMS = "CODEINTEL_NEBULA_TIMEOUT_MS"
)

// Pool defaults per docs/codeintel-porting-plan.md §6:
//
//	"Nebula client pool size 4 per pod"
//
// Per-query timeout aims at the p99 < 50 ms SLO for the wrapping
// API handler; 250 ms gives ~5x headroom for a real graph query
// before the request budget burns.
const (
	defaultPoolSize = 4
	defaultIdleTime = 5 * time.Minute
	defaultTimeout  = 250 * time.Millisecond
	maxPoolSize     = 64
)

// Sentinel errors for the env / validation surface. Surfaced bare
// (errors.Is friendly) so callers can branch without parsing
// error strings.
var (
	ErrAddrRequired     = errors.New("nebulaclient: CODEINTEL_NEBULA_ADDR is required")
	ErrAddrInvalid      = errors.New("nebulaclient: CODEINTEL_NEBULA_ADDR is malformed")
	ErrUserRequired     = errors.New("nebulaclient: CODEINTEL_NEBULA_USER is required")
	ErrPasswordRequired = errors.New("nebulaclient: CODEINTEL_NEBULA_PASSWORD is required")
	ErrPoolSizeInvalid  = errors.New("nebulaclient: CODEINTEL_NEBULA_POOL_SIZE must be a positive integer no greater than 64")
	ErrTimeoutInvalid   = errors.New("nebulaclient: CODEINTEL_NEBULA_TIMEOUT_MS must be a positive integer no greater than 60000")
	ErrSpaceInvalid     = errors.New("nebulaclient: CODEINTEL_NEBULA_SPACE must match [A-Za-z_][A-Za-z0-9_]{0,127}")
)

// Config carries every dependency the wrapper needs. The zero
// value is unusable on purpose — operators construct it via
// LoadConfigFromEnv (production) or by setting fields directly
// (tests / one-off scripts).
type Config struct {
	// Addrs is the list of graphd endpoints the pool dials. Multiple
	// entries enable fanout / round-robin across graphd replicas;
	// single-graphd deployments pass exactly one.
	Addrs []nebula.HostAddress

	// Username + Password authenticate against graphd. In production
	// the bootstrap `root` / `nebula` credentials MUST be rotated
	// via the operator-side `ALTER USER` flow before exposing the
	// cluster; the wrapper enforces non-empty values but does not
	// validate the credentials themselves.
	Username string
	Password string

	// Space is the optional default graph space the wrapper switches
	// into after a session is acquired. Empty disables the auto-USE
	// (callers can issue their own `USE <space>` statement).
	Space string

	// PoolSize bounds the per-process connection count. Defaults to
	// 4 per the porting-plan §6 SLO.
	PoolSize int

	// IdleTime is the time after which an idle pooled connection is
	// reaped. Defaults to 5 minutes.
	IdleTime time.Duration

	// Timeout bounds an individual nGQL Execute call. Defaults to
	// 250 ms. Callers that need a longer or shorter bound pass a
	// context with a custom deadline to Execute.
	Timeout time.Duration
}

// LoadConfigFromEnv reads every env var listed above and produces
// a Config. Missing required vars surface their sentinel error so
// the operator gets a precise diagnostic.
func LoadConfigFromEnv() (Config, error) {
	addrRaw := os.Getenv(EnvAddr)
	if addrRaw == "" {
		return Config{}, ErrAddrRequired
	}
	hosts, err := parseAddrs(addrRaw)
	if err != nil {
		return Config{}, err
	}
	user := os.Getenv(EnvUser)
	if user == "" {
		return Config{}, ErrUserRequired
	}
	pass := os.Getenv(EnvPassword)
	if pass == "" {
		return Config{}, ErrPasswordRequired
	}

	space := os.Getenv(EnvSpace)
	if space != "" && !spaceNamePattern.MatchString(space) {
		return Config{}, fmt.Errorf("%w: got %q", ErrSpaceInvalid, space)
	}

	cfg := Config{
		Addrs:    hosts,
		Username: user,
		Password: pass,
		Space:    space,
		PoolSize: defaultPoolSize,
		IdleTime: defaultIdleTime,
		Timeout:  defaultTimeout,
	}

	if raw := os.Getenv(EnvPoolSize); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n <= 0 || n > maxPoolSize {
			return Config{}, fmt.Errorf("%w: got %q", ErrPoolSizeInvalid, raw)
		}
		cfg.PoolSize = n
	}
	if raw := os.Getenv(EnvTimeoutMS); raw != "" {
		n, parseErr := strconv.Atoi(raw)
		if parseErr != nil || n <= 0 || n > 60000 {
			return Config{}, fmt.Errorf("%w: got %q", ErrTimeoutInvalid, raw)
		}
		cfg.Timeout = time.Duration(n) * time.Millisecond
	}

	return cfg, nil
}

// parseAddrs splits a comma-separated host:port list into
// nebula.HostAddress entries. Whitespace around each entry is
// tolerated so an env var like "h1:9669, h2:9669" works.
func parseAddrs(raw string) ([]nebula.HostAddress, error) {
	parts := strings.Split(raw, ",")
	out := make([]nebula.HostAddress, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		host, portStr, err := net.SplitHostPort(p)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrAddrInvalid, p, err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("%w: %q port must be 1-65535", ErrAddrInvalid, p)
		}
		out = append(out, nebula.HostAddress{Host: host, Port: port})
	}
	if len(out) == 0 {
		return nil, ErrAddrInvalid
	}
	return out, nil
}

// validate is the boundary guard New runs before constructing the
// pool. Each field gets a precise sentinel so test cases can
// pin-point regressions.
func (c Config) validate() error {
	if len(c.Addrs) == 0 {
		return ErrAddrRequired
	}
	if c.Username == "" {
		return ErrUserRequired
	}
	if c.Password == "" {
		return ErrPasswordRequired
	}
	if c.PoolSize <= 0 || c.PoolSize > maxPoolSize {
		return ErrPoolSizeInvalid
	}
	if c.Timeout <= 0 {
		return ErrTimeoutInvalid
	}
	if c.Space != "" && !spaceNamePattern.MatchString(c.Space) {
		return ErrSpaceInvalid
	}
	return nil
}

// LogValue implements slog.LogValuer so a Config caught by a
// structured log call (`logger.Info("startup", "cfg", cfg)`) emits
// a redacted group rather than dumping the password verbatim. The
// host list, username, space, and pool size are operational; the
// password is replaced with "***" (or an empty group when no
// password is set, surfacing that diagnostic clearly).
func (c Config) LogValue() slog.Value {
	hosts := make([]string, 0, len(c.Addrs))
	for _, a := range c.Addrs {
		hosts = append(hosts, fmt.Sprintf("%s:%d", a.Host, a.Port))
	}
	pwMask := ""
	if c.Password != "" {
		pwMask = "***"
	}
	return slog.GroupValue(
		slog.String("addrs", strings.Join(hosts, ",")),
		slog.String("username", c.Username),
		slog.String("password", pwMask),
		slog.String("space", c.Space),
		slog.Int("pool_size", c.PoolSize),
		slog.Duration("idle_time", c.IdleTime),
		slog.Duration("timeout", c.Timeout),
	)
}

// String produces a redacted single-line representation safe for
// `%v`/`%s` formatting. Backed by LogValue's redaction logic so a
// `fmt.Errorf("...%v", cfg)` cannot accidentally leak the password.
func (c Config) String() string {
	return c.LogValue().String()
}
