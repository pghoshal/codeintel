package auth

import (
	"errors"
	"testing"
)

// TestParseApiKey_AcceptsCanonicalPrefix locks the `cik_` prefix path.
// Issued keys take the form `cik_<64-hex-secret>`; the auth layer
// strips the prefix and returns the raw secret for HashSecret to
// consume.
func TestParseApiKey_AcceptsCanonicalPrefix(t *testing.T) {
	const raw = "cik_deadbeef"
	got, err := ParseApiKey(raw)
	if err != nil {
		t.Fatalf("ParseApiKey(%q): %v", raw, err)
	}
	if got != "deadbeef" {
		t.Fatalf("ParseApiKey(%q): got secret %q, want %q", raw, got, "deadbeef")
	}
}

// TestParseApiKey_RejectsUnknownPrefix asserts a key without the
// recognised prefix is rejected at the boundary instead of silently
// passing through to a digest-mismatch 401. Returning a typed
// sentinel lets the middleware distinguish "malformed" from "wrong"
// for log-fingerprint stability.
func TestParseApiKey_RejectsUnknownPrefix(t *testing.T) {
	cases := []string{
		"github_pat_AAA",
		"Bearer cik_x", // accidentally including the scheme
		"deadbeef",     // no prefix at all
		"xyz-deadbeef", // wrong separator (dash instead of underscore)
		"CIK_deadbeef", // wrong case
	}
	for _, raw := range cases {
		if _, err := ParseApiKey(raw); !errors.Is(err, ErrInvalidApiKey) {
			t.Errorf("ParseApiKey(%q): got %v, want ErrInvalidApiKey", raw, err)
		}
	}
}

// TestParseApiKey_RejectsEmptySecret defends against keys that are
// well-formed prefix but carry no actual secret payload.
func TestParseApiKey_RejectsEmptySecret(t *testing.T) {
	if _, err := ParseApiKey("cik_"); !errors.Is(err, ErrInvalidApiKey) {
		t.Errorf("ParseApiKey(\"cik_\"): got %v, want ErrInvalidApiKey", err)
	}
}

// TestParseApiKey_RejectsEmptyInput is the obvious boundary — empty
// input is a malformed call from the middleware.
func TestParseApiKey_RejectsEmptyInput(t *testing.T) {
	if _, err := ParseApiKey(""); !errors.Is(err, ErrInvalidApiKey) {
		t.Fatalf("ParseApiKey(\"\"): got %v, want ErrInvalidApiKey", err)
	}
}

// TestApiKeyPrefixFrozen pins the wire-format prefix value so a
// future refactor can't accidentally rename it and silently
// invalidate every outstanding key.
func TestApiKeyPrefixFrozen(t *testing.T) {
	if ApiKeyPrefix != "cik_" {
		t.Errorf("ApiKeyPrefix drift: got %q, want %q", ApiKeyPrefix, "cik_")
	}
}
