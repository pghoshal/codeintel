// Wire-format parity tests for GET /api/version. The test pins
// CODEINTEL_VERSION so the response is reproducible regardless of
// the caller's environment.
package parity

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeintel/internal/api"
)

// TestVersion_BodyByteEqualToGolden locks the entire response
// contract against the golden fixture: byte-equal body, exact
// Content-Type, exact HTTP status.
func TestVersion_BodyByteEqualToGolden(t *testing.T) {
	t.Setenv("CODEINTEL_VERSION", "v4.16.3")

	golden, err := os.ReadFile(filepath.Join("golden", "version.json"))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}

	server := api.NewServer(api.Config{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(server.Router())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", res.StatusCode, http.StatusOK)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: got %q, want %q", ct, "application/json")
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(golden) {
		t.Fatalf("body parity failure:\n  got:    %q (%d bytes)\n  golden: %q (%d bytes)",
			string(body), len(body), string(golden), len(golden))
	}
}

// TestVersion_ResponseSize asserts the 21-byte payload length so
// trailing-newline or pretty-print regressions fail immediately.
func TestVersion_ResponseSize(t *testing.T) {
	t.Setenv("CODEINTEL_VERSION", "v4.16.3")

	server := api.NewServer(api.Config{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(server.Router())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 21 {
		t.Fatalf("body size: got %d, want 21 (fixture is `{\"version\":\"v4.16.3\"}`)", len(body))
	}
}

// TestVersion_DefaultsWhenEnvEmpty asserts that resolveVersion
// falls back to the build-time default when CODEINTEL_VERSION is
// empty. os.Getenv returns "" for both unset and empty, so either
// case takes the fallback path.
func TestVersion_DefaultsWhenEnvEmpty(t *testing.T) {
	t.Setenv("CODEINTEL_VERSION", "")

	server := api.NewServer(api.Config{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(server.Router())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", res.StatusCode, http.StatusOK)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type: got %q, want %q", ct, "application/json")
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), `"version":"`) {
		t.Fatalf("expected a non-empty version field, got: %q", string(body))
	}
	if strings.Contains(string(body), `"version":""`) {
		t.Fatalf("default version must not be empty, got: %q", string(body))
	}
}

// TestVersion_HostileInputDoesNotBreakJSON locks the security
// contract: a CODEINTEL_VERSION value containing JSON
// metacharacters (quotes, backslashes, control chars) must NOT
// break the response envelope. encoding/json.Marshal escapes these
// correctly; this test locks that behaviour in case someone later
// swaps Marshal for a manual byte build.
func TestVersion_HostileInputDoesNotBreakJSON(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"quote", `v1.0.0"`},
		{"backslash", `v1.0.0\`},
		{"newline", "v1.0.0\n"},
		{"injection_attempt", `v1.0.0","admin":true,"x":"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEINTEL_VERSION", tc.input)

			server := api.NewServer(api.Config{
				Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
			})
			ts := httptest.NewServer(server.Router())
			defer ts.Close()

			res, err := http.Get(ts.URL + "/api/version")
			if err != nil {
				t.Fatalf("GET /api/version: %v", err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			// The response must stay a single-key JSON object with
			// the literal key "version". The value is allowed to
			// contain escape sequences. An injection that adds extra
			// top-level keys (e.g. ",admin":true) would produce a
			// body like `{"version":"v","admin":true,"x":""}` which
			// we detect by parsing the JSON and asserting the field
			// count.
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("hostile input %q produced malformed JSON: %v\nbody: %q",
					tc.input, err, string(body))
			}
			if len(decoded) != 1 {
				t.Fatalf("hostile input %q injected extra top-level keys: %#v",
					tc.input, decoded)
			}
			if _, ok := decoded["version"]; !ok {
				t.Fatalf("hostile input %q removed the version key: %#v",
					tc.input, decoded)
			}
		})
	}
}
