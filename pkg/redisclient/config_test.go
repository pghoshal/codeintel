package redisclient

import (
	"errors"
	"strings"
	"testing"
)

// TestLoadConfigFromEnv_RequiresURL locks the
// ErrURLRequired sentinel — operator missing the env var must
// see a precise diagnostic.
func TestLoadConfigFromEnv_RequiresURL(t *testing.T) {
	t.Setenv(EnvURL, "")
	_, err := LoadConfigFromEnv()
	if !errors.Is(err, ErrURLRequired) {
		t.Errorf("got %v, want ErrURLRequired", err)
	}
}

// TestLoadConfigFromEnv_RejectsInvalidScheme confirms the scheme
// guard. Plain http:// / postgres:// / ftp:// all reject.
func TestLoadConfigFromEnv_RejectsInvalidScheme(t *testing.T) {
	cases := []string{
		"http://localhost:6379",
		"postgres://x:y@h:5432/d",
		"ftp://h",
		"not a url",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(EnvURL, raw)
			_, err := LoadConfigFromEnv()
			if !errors.Is(err, ErrURLInvalid) {
				t.Errorf("scheme %q: got %v, want ErrURLInvalid", raw, err)
			}
		})
	}
}

// TestLoadConfigFromEnv_AcceptsRedisAndRediss covers the two
// valid schemes the legacy redis.ts handles (plain + TLS).
func TestLoadConfigFromEnv_AcceptsRedisAndRediss(t *testing.T) {
	cases := []string{
		"redis://127.0.0.1:6379",
		"rediss://127.0.0.1:6379",
		"redis://user:pass@127.0.0.1:6379/3",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(EnvURL, raw)
			cfg, err := LoadConfigFromEnv()
			if err != nil {
				t.Errorf("scheme %q: %v", raw, err)
			}
			if cfg.URL != raw {
				t.Errorf("URL round-trip: got %q, want %q", cfg.URL, raw)
			}
		})
	}
}

// TestConfig_LogValueRedactsPassword locks the credential-mask
// invariant. A logger.Info("cfg", cfg) with a URL containing
// userinfo MUST emit a redacted form.
func TestConfig_LogValueRedactsPassword(t *testing.T) {
	cfg := Config{URL: "redis://user:supersecret@127.0.0.1:6379/0"}
	got := cfg.String()
	if strings.Contains(got, "supersecret") {
		t.Errorf("password leaked: %s", got)
	}
	if strings.Contains(got, "user") {
		t.Errorf("username leaked: %s", got)
	}
	if !strings.Contains(got, "127.0.0.1:6379") {
		t.Errorf("host info should survive: %s", got)
	}
}

// TestConfig_LogValueEmptyURL handles the zero-Config case
// without panicking.
func TestConfig_LogValueEmptyURL(t *testing.T) {
	cfg := Config{}
	got := cfg.String()
	if strings.Contains(got, "<unparseable>") {
		t.Errorf("empty URL should not produce <unparseable>: %s", got)
	}
}

// TestConfig_TLSEnabledForwardsOverride confirms an explicit
// TLSEnabled=true forces TLS even when the URL is plain redis://
// (i.e., when the URL comes from a secret store without scheme).
func TestConfig_TLSEnabledForwardsOverride(t *testing.T) {
	t.Setenv(EnvURL, "redis://127.0.0.1:6379")
	t.Setenv(EnvTLSEnabled, "true")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if !cfg.TLSEnabled {
		t.Errorf("TLSEnabled should be true")
	}
}

// TestConfig_TLSInsecureSkipVerify maps the legacy
// rejectUnauthorized=false branch to the Go flag.
func TestConfig_TLSInsecureSkipVerify(t *testing.T) {
	t.Setenv(EnvURL, "rediss://127.0.0.1:6379")
	t.Setenv(EnvTLSRejectUnauthorized, "false")
	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if !cfg.TLSInsecureSkipVerify {
		t.Errorf("rejectUnauthorized=false should produce TLSInsecureSkipVerify=true")
	}
}

// TestConfig_WithDefaults idempotency: zero-valued timing
// fields get reasonable defaults; pre-set fields pass through
// unchanged.
func TestConfig_WithDefaults(t *testing.T) {
	zero := Config{URL: "redis://127.0.0.1:6379"}.WithDefaults()
	if zero.DialTimeout <= 0 || zero.ReadTimeout <= 0 || zero.WriteTimeout <= 0 {
		t.Errorf("defaults not applied: %+v", zero)
	}
	again := zero.WithDefaults()
	if again != zero {
		t.Errorf("not idempotent: first=%+v second=%+v", zero, again)
	}
}

// TestConfig_Validate covers the boundary guards. Each rejection
// path uses a distinct sentinel.
func TestConfig_Validate(t *testing.T) {
	if err := (Config{}).validate(); !errors.Is(err, ErrURLRequired) {
		t.Errorf("empty URL: got %v, want ErrURLRequired", err)
	}
	if err := (Config{URL: "redis://no-port"}).validate(); !errors.Is(err, ErrURLInvalid) {
		t.Errorf("no port: got %v, want ErrURLInvalid", err)
	}
	if err := (Config{URL: "redis://h:0"}).validate(); !errors.Is(err, ErrURLInvalid) {
		t.Errorf("port 0: got %v, want ErrURLInvalid", err)
	}
	if err := (Config{URL: "redis://h:65536"}).validate(); !errors.Is(err, ErrURLInvalid) {
		t.Errorf("port 65536: got %v, want ErrURLInvalid", err)
	}
	if err := (Config{URL: "redis://h:6379"}).validate(); err != nil {
		t.Errorf("valid URL: got %v, want nil", err)
	}
	// IPv6 bracketed form — net.SplitHostPort handles this; the
	// hand-rolled LastIndex(":") splitter the earlier draft used
	// broke here.
	if err := (Config{URL: "redis://[::1]:6379"}).validate(); err != nil {
		t.Errorf("IPv6 URL: got %v, want nil", err)
	}
	if err := (Config{URL: "rediss://[2001:db8::1]:6380"}).validate(); err != nil {
		t.Errorf("IPv6 TLS URL: got %v, want nil", err)
	}
}

// TestRedactURL_IPv6 confirms redaction handles IPv6 hosts
// without dropping the brackets — log output should remain
// parseable.
func TestRedactURL_IPv6(t *testing.T) {
	got := redactURL("redis://user:secret@[::1]:6379/0")
	if got != "redis://[::1]:6379/0" {
		t.Errorf("IPv6 redact: got %q, want %q", got, "redis://[::1]:6379/0")
	}
}
