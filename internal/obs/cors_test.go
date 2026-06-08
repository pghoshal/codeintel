package obs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCORS_PreflightShortCircuits confirms OPTIONS with the
// Access-Control-Request-Method header returns 204 + the canonical
// CORS response set, and the inner handler is NOT invoked.
func TestCORS_PreflightShortCircuits(t *testing.T) {
	m := NewCORSMiddleware(CORSConfig{
		AllowedOrigins: []string{"https://app.example.com"},
	})
	innerCalled := false
	h := m.Wrap(func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
	})
	req := httptest.NewRequest(http.MethodOptions, "/api/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "PUT")
	req.Header.Set("Access-Control-Request-Headers", "X-Custom")
	rec := httptest.NewRecorder()
	h(rec, req)

	if innerCalled {
		t.Fatal("preflight must not invoke inner handler")
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: got %d, want 204", rec.Code)
	}
	for hk, want := range map[string]string{
		"Access-Control-Allow-Origin":  "https://app.example.com",
		"Access-Control-Allow-Methods": "GET, POST, PUT, PATCH, DELETE, OPTIONS",
		"Access-Control-Allow-Headers": "X-Custom",
		"Access-Control-Max-Age":       "600",
	} {
		if got := rec.Header().Get(hk); got != want {
			t.Errorf("%s: got %q, want %q", hk, got, want)
		}
	}
	if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("Vary header missing Origin: %q", rec.Header().Get("Vary"))
	}
}

// TestCORS_DisallowedOriginNoAllowHeader confirms that an
// unrecognised Origin gets the request passed through to the inner
// handler but WITHOUT Allow-Origin — the browser's CORS gate
// rejects the response (no covert admission).
func TestCORS_DisallowedOriginNoAllowHeader(t *testing.T) {
	m := NewCORSMiddleware(CORSConfig{AllowedOrigins: []string{"https://app.example.com"}})
	h := m.Wrap(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://evil.attacker.com")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (handler still runs; browser blocks the response)", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin: got %q, want empty for disallowed origin", got)
	}
}

// TestCORS_SameOriginPassthrough confirms requests without an Origin
// header (same-origin or non-browser) skip the CORS-specific
// headers entirely.
func TestCORS_SameOriginPassthrough(t *testing.T) {
	m := NewCORSMiddleware(CORSConfig{AllowedOrigins: []string{"https://app.example.com"}})
	called := false
	h := m.Wrap(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if !called {
		t.Fatal("inner handler must be called for non-CORS request")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("non-CORS request should not get Allow-Origin; got %q", got)
	}
}

// TestCORS_WildcardOriginAdmitsAll confirms `*` enables wildcard.
func TestCORS_WildcardOriginAdmitsAll(t *testing.T) {
	m := NewCORSMiddleware(CORSConfig{AllowedOrigins: []string{"*"}})
	h := m.Wrap(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://anywhere.example")
	rec := httptest.NewRecorder()
	h(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://anywhere.example" {
		t.Errorf("wildcard should echo origin: got %q", got)
	}
}

// TestCORS_NormalRequestAddsAllowOriginHeader confirms a regular
// GET with an allowed Origin gets the Allow-Origin header AND the
// inner handler is invoked.
func TestCORS_NormalRequestAddsAllowOriginHeader(t *testing.T) {
	m := NewCORSMiddleware(CORSConfig{
		AllowedOrigins:   []string{"https://app.example.com"},
		AllowCredentials: true,
	})
	called := false
	h := m.Wrap(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	rec := httptest.NewRecorder()
	h(rec, req)
	if !called {
		t.Fatal("inner handler must run for normal CORS request")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("Allow-Origin: got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Allow-Credentials: got %q, want true", got)
	}
}

// TestCORS_NilMiddlewareIsPassthrough confirms a nil CORSMiddleware
// returns the handler unchanged (CORS disabled gracefully).
func TestCORS_NilMiddlewareIsPassthrough(t *testing.T) {
	var m *CORSMiddleware
	called := false
	h := m.Wrap(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	if !called {
		t.Fatal("nil middleware must passthrough")
	}
}

// TestCORS_IntToStr exercises the inline helper for both positive
// and zero values used in Max-Age headers.
func TestCORS_IntToStr(t *testing.T) {
	cases := map[int]string{0: "0", 1: "1", 600: "600", 999999: "999999", -42: "-42"}
	for n, want := range cases {
		if got := intToStr(n); got != want {
			t.Errorf("intToStr(%d): got %q, want %q", n, got, want)
		}
	}
}
