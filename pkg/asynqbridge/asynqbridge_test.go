package asynqbridge

import (
	"bytes"
	"crypto/tls"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
)

// TestRedisOptFromURL_RequiresURL pins ErrURLRequired so the
// caller can branch by sentinel rather than parsing strings.
func TestRedisOptFromURL_RequiresURL(t *testing.T) {
	_, err := RedisOptFromURL("")
	if !errors.Is(err, ErrURLRequired) {
		t.Errorf("got %v, want ErrURLRequired", err)
	}
}

// TestRedisOptFromURL_PlainTCP covers the happy path: a plain
// redis:// URL with userinfo + DB. The asynq opt must capture
// every field — drift here breaks production deployments that
// rely on AUTH or non-zero DB selection.
func TestRedisOptFromURL_PlainTCP(t *testing.T) {
	opt, err := RedisOptFromURL("redis://user:pass@127.0.0.1:6380/3")
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	got, ok := opt.(asynq.RedisClientOpt)
	if !ok {
		t.Fatalf("type: got %T, want asynq.RedisClientOpt", opt)
	}
	if got.Addr != "127.0.0.1:6380" {
		t.Errorf("Addr: got %q, want 127.0.0.1:6380", got.Addr)
	}
	if got.Username != "user" {
		t.Errorf("Username: got %q, want user", got.Username)
	}
	if got.Password != "pass" {
		t.Errorf("Password: got %q, want pass", got.Password)
	}
	if got.DB != 3 {
		t.Errorf("DB: got %d, want 3", got.DB)
	}
	if got.TLSConfig != nil {
		t.Errorf("TLSConfig: got non-nil for plain redis://")
	}
}

// TestRedisOptFromURL_IPv6 confirms the bracketed IPv6 host
// survives the round-trip — same correctness concern that
// motivated the net.SplitHostPort fix in pkg/redisclient.
func TestRedisOptFromURL_IPv6(t *testing.T) {
	opt, err := RedisOptFromURL("redis://[::1]:6379/0")
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	got := opt.(asynq.RedisClientOpt)
	if got.Addr != "[::1]:6379" {
		t.Errorf("IPv6 Addr: got %q, want [::1]:6379", got.Addr)
	}
}

// TestRedisOptFromURL_TLS confirms rediss:// surfaces a non-nil
// TLSConfig so asynq's internal go-redis client uses TLS.
func TestRedisOptFromURL_TLS(t *testing.T) {
	opt, err := RedisOptFromURL("rediss://user:pass@host:6379/0")
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	got := opt.(asynq.RedisClientOpt)
	if got.TLSConfig == nil {
		t.Error("TLSConfig: got nil for rediss://, want non-nil")
	}
}

// TestRedisOptFromURL_TLSMinVersion locks the security floor —
// the bridge MUST not negotiate TLS below 1.2. Regression bait:
// go-redis's ParseURL returns a TLSConfig with MinVersion=0
// (Go stdlib default, which allows TLS 1.0/1.1); the bridge
// must explicitly pin to tls.VersionTLS12 so that asynq's Redis
// dial uses the same floor as pkg/redisclient.toRedisOptions
// does for the non-asynq Redis client.
func TestRedisOptFromURL_TLSMinVersion(t *testing.T) {
	opt, err := RedisOptFromURL("rediss://h:6379/0")
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	got := opt.(asynq.RedisClientOpt)
	if got.TLSConfig == nil {
		t.Fatal("TLSConfig: got nil for rediss://")
	}
	if got.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Errorf("TLSConfig.MinVersion: got 0x%x, want 0x%x (tls.VersionTLS12)",
			got.TLSConfig.MinVersion, tls.VersionTLS12)
	}
}

// TestRedisOptFromURL_RejectsBadScheme covers the defense-in-
// depth scheme guard. http://, postgres://, ftp:// all reject
// with ErrURLScheme.
func TestRedisOptFromURL_RejectsBadScheme(t *testing.T) {
	cases := []string{
		"http://h:6379",
		"postgres://u:p@h:5432",
		"ftp://h",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := RedisOptFromURL(raw)
			if !errors.Is(err, ErrURLScheme) {
				t.Errorf("scheme %q: got %v, want ErrURLScheme", raw, err)
			}
		})
	}
}

// TestRedisOptFromURL_RejectsBadURL exercises the parse-error
// branch — go-redis's ParseURL is the underlying validator.
func TestRedisOptFromURL_RejectsBadURL(t *testing.T) {
	_, err := RedisOptFromURL("not a url at all")
	if err == nil {
		t.Error("expected error for malformed URL, got nil")
	}
	if errors.Is(err, ErrURLRequired) {
		t.Error("malformed URL should not return ErrURLRequired sentinel")
	}
}

// TestSlogLogger_LevelRouting confirms each asynq logger method
// emits at the matching slog level. The test parses the JSON
// handler's output to assert the level field.
func TestSlogLogger_LevelRouting(t *testing.T) {
	cases := []struct {
		name      string
		call      func(*SlogLogger)
		wantLevel string
	}{
		{"debug", func(l *SlogLogger) { l.Debug("ping") }, "DEBUG"},
		{"info", func(l *SlogLogger) { l.Info("ready") }, "INFO"},
		{"warn", func(l *SlogLogger) { l.Warn("retry") }, "WARN"},
		{"error", func(l *SlogLogger) { l.Error("boom") }, "ERROR"},
		{"fatal", func(l *SlogLogger) { l.Fatal("oh no") }, "ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			base := slog.New(slog.NewJSONHandler(&buf,
				&slog.HandlerOptions{Level: slog.LevelDebug}))
			l := &SlogLogger{Base: base}
			tc.call(l)
			out := buf.String()
			if !strings.Contains(out, `"level":"`+tc.wantLevel+`"`) {
				t.Errorf("level: missing %q in output %q", tc.wantLevel, out)
			}
		})
	}
}

// TestSlogLogger_SingleMsgKey + TestSlogLogger_ComponentTag pin
// the JSON-handler output shape: exactly one "msg" key per line
// (no duplicate from a colliding extra-attr) and a stable
// "component":"asynq" tag for grep-based log filtering.
//
// Regression bait: an earlier draft passed the asynq message
// as an extra slog attribute named "msg", which collided with
// slog's own message field and produced two "msg":"..." keys in
// the JSON output.
func TestSlogLogger_SingleMsgKey(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	l := &SlogLogger{Base: base}
	l.Info("Starting processing")
	out := buf.String()
	if got := strings.Count(out, `"msg":`); got != 1 {
		t.Errorf("msg key count: got %d, want exactly 1\nfull: %s", got, out)
	}
	if !strings.Contains(out, `"msg":"Starting processing"`) {
		t.Errorf("msg payload missing or wrong: %s", out)
	}
}

func TestSlogLogger_ComponentTag(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	l := &SlogLogger{Base: base}
	l.Info("ready")
	out := buf.String()
	if !strings.Contains(out, `"component":"asynq"`) {
		t.Errorf("component tag missing: %s", out)
	}
}

// TestSlogLogger_TagStableAcrossCalls confirms the sync.Once-
// cached tagged logger keeps emitting exactly one component
// tag per line across many calls — defends against a future
// refactor that accidentally re-tags or double-tags.
func TestSlogLogger_TagStableAcrossCalls(t *testing.T) {
	var buf bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&buf,
		&slog.HandlerOptions{Level: slog.LevelDebug}))
	l := &SlogLogger{Base: base}
	for i := 0; i < 5; i++ {
		l.Info("ping")
	}
	out := buf.String()
	tagHits := strings.Count(out, `"component":"asynq"`)
	if tagHits != 5 {
		t.Errorf("component-tag count: got %d, want 5 (one per call)\nfull:\n%s",
			tagHits, out)
	}
}

// TestSlogLogger_NilSafe covers the defensive paths — a zero
// SlogLogger or one with a nil Base must not panic. asynq calls
// the logger during shutdown / failure paths where a partially-
// constructed Server could hold a nil logger ref.
func TestSlogLogger_NilSafe(t *testing.T) {
	var nilPtr *SlogLogger
	nilPtr.Info("should not panic")

	zero := &SlogLogger{}
	zero.Error("should not panic")
}
