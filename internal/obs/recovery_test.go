package obs

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPanicRecovery_EmitsServiceErrorEnvelope locks the
// happy-recovery shape: 500 status + canonical JSON body, even when
// the inner handler panicked before any header was written.
func TestPanicRecovery_EmitsServiceErrorEnvelope(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	wrapped := WithPanicRecovery(logger, func(w http.ResponseWriter, r *http.Request) {
		panic("boom — bad pointer")
	})

	rec := httptest.NewRecorder()
	wrapped(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	want := `{"statusCode":500,"errorCode":"UNEXPECTED_ERROR","message":"An unexpected error occurred"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q", got)
	}
	// Log line should carry the panic message.
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("log missing panic value; raw:\n%s", buf.String())
	}
	// Log line should include a stack trace.
	if !strings.Contains(buf.String(), "stack") {
		t.Errorf("log missing stack key; raw:\n%s", buf.String())
	}
}

// TestPanicRecovery_PreservesRequestIDInLog confirms the recovered
// log line carries the request id so on-call can correlate it
// with the access log + metric series.
func TestPanicRecovery_PreservesRequestIDInLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	wrapped := WithRequestID(WithPanicRecovery(logger, func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(RequestIDHeader, "trace-1")
	wrapped(httptest.NewRecorder(), req)

	var entry map[string]any
	if err := json.NewDecoder(&buf).Decode(&entry); err != nil {
		t.Fatalf("decode log: %v", err)
	}
	if got, _ := entry["request_id"].(string); got != "trace-1" {
		t.Errorf("request_id: got %v, want trace-1 (entry: %v)", got, entry)
	}
}

// TestPanicRecovery_DoesNotRewriteWhenHeaderAlreadyWritten covers
// the partial-response case: if the handler wrote a status before
// panicking, the recovery middleware logs but cannot replace the
// already-sent header. The HTTP framing must stay valid.
func TestPanicRecovery_DoesNotRewriteWhenHeaderAlreadyWritten(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	wrapped := WithPanicRecovery(logger, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted) // already wrote 202
		_, _ = io.WriteString(w, "partial")
		panic("post-write panic")
	})
	rec := httptest.NewRecorder()
	wrapped(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	// Status stays 202 — the recovery never overwrote it.
	if rec.Code != http.StatusAccepted {
		t.Errorf("status: got %d, want 202 (recovery must not rewrite after WriteHeader)", rec.Code)
	}
	// But the panic was logged.
	if !strings.Contains(buf.String(), "post-write panic") {
		t.Errorf("panic not logged after partial write; raw:\n%s", buf.String())
	}
}

// TestPanicRecovery_NormalReturnDoesNotLog confirms the no-panic
// path: no error log emitted, response passes through verbatim.
func TestPanicRecovery_NormalReturnDoesNotLog(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	wrapped := WithPanicRecovery(logger, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "fine")
	})
	rec := httptest.NewRecorder()
	wrapped(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if buf.Len() != 0 {
		t.Errorf("non-panic path should not log; got: %s", buf.String())
	}
}
