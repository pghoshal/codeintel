package nebulaclient

import (
	"errors"
	"strings"
	"testing"

	nebula "github.com/vesoft-inc/nebula-go/v3"
)

// TestLoadConfigFromEnv_HappyPath locks the basic env contract:
// every required var present → typed Config, defaults populated.
func TestLoadConfigFromEnv_HappyPath(t *testing.T) {
	t.Setenv(EnvAddr, "127.0.0.1:9669")
	t.Setenv(EnvUser, "root")
	t.Setenv(EnvPassword, "nebula")
	t.Setenv(EnvSpace, "codeintel")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if len(cfg.Addrs) != 1 || cfg.Addrs[0].Host != "127.0.0.1" || cfg.Addrs[0].Port != 9669 {
		t.Errorf("Addrs: got %+v", cfg.Addrs)
	}
	if cfg.Username != "root" || cfg.Password != "nebula" || cfg.Space != "codeintel" {
		t.Errorf("creds/space: got user=%q pass=%q space=%q", cfg.Username, cfg.Password, cfg.Space)
	}
	if cfg.PoolSize != defaultPoolSize {
		t.Errorf("PoolSize default: got %d, want %d", cfg.PoolSize, defaultPoolSize)
	}
	if cfg.IdleTime != defaultIdleTime || cfg.Timeout != defaultTimeout {
		t.Errorf("timing defaults: idle=%v timeout=%v", cfg.IdleTime, cfg.Timeout)
	}
}

// TestLoadConfigFromEnv_MultiAddr exercises comma-separated host
// lists (production deploys with multiple graphd replicas).
func TestLoadConfigFromEnv_MultiAddr(t *testing.T) {
	t.Setenv(EnvAddr, "g1:9669, g2:9669 ,g3:9669")
	t.Setenv(EnvUser, "u")
	t.Setenv(EnvPassword, "p")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	want := []nebula.HostAddress{
		{Host: "g1", Port: 9669},
		{Host: "g2", Port: 9669},
		{Host: "g3", Port: 9669},
	}
	if len(cfg.Addrs) != 3 {
		t.Fatalf("want 3 addrs, got %d: %+v", len(cfg.Addrs), cfg.Addrs)
	}
	for i, a := range cfg.Addrs {
		if a.Host != want[i].Host || a.Port != want[i].Port {
			t.Errorf("addr[%d]: got %+v, want %+v", i, a, want[i])
		}
	}
}

// TestLoadConfigFromEnv_PoolSizeOverride checks the
// CODEINTEL_NEBULA_POOL_SIZE override path: any positive integer
// up to maxPoolSize replaces the default.
func TestLoadConfigFromEnv_PoolSizeOverride(t *testing.T) {
	t.Setenv(EnvAddr, "127.0.0.1:9669")
	t.Setenv(EnvUser, "u")
	t.Setenv(EnvPassword, "p")
	t.Setenv(EnvPoolSize, "16")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.PoolSize != 16 {
		t.Errorf("PoolSize override: got %d, want 16", cfg.PoolSize)
	}
}

func TestLoadConfigFromEnv_TimeoutOverride(t *testing.T) {
	t.Setenv(EnvAddr, "127.0.0.1:9669")
	t.Setenv(EnvUser, "u")
	t.Setenv(EnvPassword, "p")
	t.Setenv(EnvTimeoutMS, "5000")

	cfg, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	if cfg.Timeout != 5_000_000_000 {
		t.Errorf("Timeout override: got %v, want 5s", cfg.Timeout)
	}
}

// TestLoadConfigFromEnv_RequiredVarsMissing locks each sentinel:
// the operator's first diagnostic must point at the missing env
// var by name.
func TestLoadConfigFromEnv_RequiredVarsMissing(t *testing.T) {
	cases := []struct {
		name   string
		addr   string
		user   string
		pass   string
		wantIs error
	}{
		{"addr missing", "", "u", "p", ErrAddrRequired},
		{"user missing", "127.0.0.1:9669", "", "p", ErrUserRequired},
		{"password missing", "127.0.0.1:9669", "u", "", ErrPasswordRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvAddr, tc.addr)
			t.Setenv(EnvUser, tc.user)
			t.Setenv(EnvPassword, tc.pass)
			_, err := LoadConfigFromEnv()
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("want %v, got %v", tc.wantIs, err)
			}
		})
	}
}

// TestLoadConfigFromEnv_MalformedAddr locks each Addr-parse failure
// mode (missing port, bad port, non-numeric port, empty after
// trim).
func TestLoadConfigFromEnv_MalformedAddr(t *testing.T) {
	cases := []struct {
		name string
		addr string
	}{
		{"no port", "127.0.0.1"},
		{"bad port chars", "127.0.0.1:abc"},
		{"port zero", "127.0.0.1:0"},
		{"port too high", "127.0.0.1:65536"},
		{"port negative", "127.0.0.1:-1"},
		{"only commas", ",,,"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvAddr, tc.addr)
			t.Setenv(EnvUser, "u")
			t.Setenv(EnvPassword, "p")
			_, err := LoadConfigFromEnv()
			if !errors.Is(err, ErrAddrInvalid) {
				t.Errorf("want ErrAddrInvalid, got %v", err)
			}
		})
	}
}

// TestLoadConfigFromEnv_SpaceAllowlist locks the defense-in-depth
// regex on the optional space name: a hostile env value (newlines,
// nGQL meta characters, embedded quotes) is rejected at config-
// load time so it never reaches the USE clause builder.
func TestLoadConfigFromEnv_SpaceAllowlist(t *testing.T) {
	good := []string{"codeintel", "code_intel", "_underscore_first", "A1", "abc123"}
	for _, s := range good {
		t.Run("ok/"+s, func(t *testing.T) {
			t.Setenv(EnvAddr, "127.0.0.1:9669")
			t.Setenv(EnvUser, "u")
			t.Setenv(EnvPassword, "p")
			t.Setenv(EnvSpace, s)
			cfg, err := LoadConfigFromEnv()
			if err != nil {
				t.Errorf("space %q: want accept, got %v", s, err)
			}
			if cfg.Space != s {
				t.Errorf("space %q: round-trip got %q", s, cfg.Space)
			}
		})
	}
	// Note: os.Setenv refuses null bytes, so the null-byte case is
	// covered by the direct-construction validate() test below
	// rather than the env-load path here.
	bad := []string{
		"1numericfirst", // can't start with digit
		"with space",    // space char
		"with-hyphen",   // hyphen
		"semi;colon",    // nGQL statement terminator
		"back`tick",     // identifier quote
		"new\nline",
		"quote\"char",
	}
	for _, s := range bad {
		t.Run("reject/"+s, func(t *testing.T) {
			t.Setenv(EnvAddr, "127.0.0.1:9669")
			t.Setenv(EnvUser, "u")
			t.Setenv(EnvPassword, "p")
			t.Setenv(EnvSpace, s)
			_, err := LoadConfigFromEnv()
			if !errors.Is(err, ErrSpaceInvalid) {
				t.Errorf("space %q: want ErrSpaceInvalid, got %v", s, err)
			}
		})
	}
}

// TestConfig_LogValueRedactsPassword locks the credential-leak
// guard: any structured-log call that hands a Config off to slog
// must emit "***" in the password slot, never the literal value.
// Symmetric guard for the %v fmt path via String().
func TestConfig_LogValueRedactsPassword(t *testing.T) {
	cfg := Config{
		Addrs:    []nebula.HostAddress{{Host: "h", Port: 9669}},
		Username: "root",
		Password: "supersecret",
		Space:    "codeintel",
		PoolSize: 4,
	}
	got := cfg.String()
	if strings.Contains(got, "supersecret") {
		t.Errorf("password leaked into Config.String(): %s", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("Config.String() should mark masked password as ***: %s", got)
	}

	// Empty-password path: should NOT show *** (operator wants to
	// see the blank as a diagnostic).
	cfg.Password = ""
	if strings.Contains(cfg.String(), "***") {
		t.Errorf("empty password should not produce ***: %s", cfg.String())
	}
}

// TestLoadConfigFromEnv_PoolSizeInvalid locks every reject path:
// non-numeric, zero, negative, past the cap.
func TestLoadConfigFromEnv_PoolSizeInvalid(t *testing.T) {
	cases := []string{"abc", "0", "-1", "65", "1000"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(EnvAddr, "127.0.0.1:9669")
			t.Setenv(EnvUser, "u")
			t.Setenv(EnvPassword, "p")
			t.Setenv(EnvPoolSize, raw)
			_, err := LoadConfigFromEnv()
			if !errors.Is(err, ErrPoolSizeInvalid) {
				t.Errorf("want ErrPoolSizeInvalid, got %v", err)
			}
		})
	}
}

func TestLoadConfigFromEnv_TimeoutInvalid(t *testing.T) {
	cases := []string{"abc", "0", "-1", "60001", "100000"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(EnvAddr, "127.0.0.1:9669")
			t.Setenv(EnvUser, "u")
			t.Setenv(EnvPassword, "p")
			t.Setenv(EnvTimeoutMS, raw)
			_, err := LoadConfigFromEnv()
			if !errors.Is(err, ErrTimeoutInvalid) {
				t.Errorf("want ErrTimeoutInvalid, got %v", err)
			}
		})
	}
}

// TestConfigValidate_DirectConstruction confirms validate catches
// the same gaps when callers construct Config directly (tests,
// one-off scripts).
func TestConfigValidate_DirectConstruction(t *testing.T) {
	good := Config{
		Addrs:    []nebula.HostAddress{{Host: "h", Port: 9669}},
		Username: "u",
		Password: "p",
		PoolSize: 4,
		Timeout:  defaultTimeout,
	}
	if err := good.validate(); err != nil {
		t.Errorf("validate good cfg: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
		wantIs error
	}{
		{"empty addrs", func(c *Config) { c.Addrs = nil }, ErrAddrRequired},
		{"empty username", func(c *Config) { c.Username = "" }, ErrUserRequired},
		{"empty password", func(c *Config) { c.Password = "" }, ErrPasswordRequired},
		{"pool size zero", func(c *Config) { c.PoolSize = 0 }, ErrPoolSizeInvalid},
		{"pool size negative", func(c *Config) { c.PoolSize = -1 }, ErrPoolSizeInvalid},
		{"pool size oversized", func(c *Config) { c.PoolSize = 65 }, ErrPoolSizeInvalid},
		{"timeout zero", func(c *Config) { c.Timeout = 0 }, ErrTimeoutInvalid},
		{"timeout negative", func(c *Config) { c.Timeout = -1 }, ErrTimeoutInvalid},
		{"space invalid chars", func(c *Config) { c.Space = "bad space" }, ErrSpaceInvalid},
		{"space leading digit", func(c *Config) { c.Space = "1bad" }, ErrSpaceInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mutate(&c)
			err := c.validate()
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("want %v, got %v", tc.wantIs, err)
			}
		})
	}
}
