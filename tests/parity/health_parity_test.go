// Wire-format parity tests for GET /api/health. The Server is
// booted in-process and the response is compared against the
// golden fixture (held under ./golden) — body bytes, Content-Type
// header, and HTTP status must all match exactly.
package parity

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"codeintel/internal/api"
)

func TestHealth_BodyByteEqualToGolden(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("golden", "health.json"))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}

	server := api.NewServer(api.Config{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})

	ts := httptest.NewServer(server.Router())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
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

// TestHealth_ResponseSize asserts the response is exactly the
// 15-byte payload. No whitespace, no indentation, no trailing
// newline. Downstream smoke harnesses depend on this.
func TestHealth_ResponseSize(t *testing.T) {
	server := api.NewServer(api.Config{
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	ts := httptest.NewServer(server.Router())
	defer ts.Close()

	res, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if len(body) != 15 {
		t.Fatalf("body size: got %d, want 15 (fixture is `{\"status\":\"ok\"}`)", len(body))
	}
}
