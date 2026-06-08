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

// TestRequestID_PreservesClientSupplied locks the upstream-proxy
// behaviour: when an upstream sets X-Request-Id, that value flows
// through unchanged into the response and context.
func TestRequestID_PreservesClientSupplied(t *testing.T) {
	const wantID = "trace-1234"
	var observedFromCtx string
	h := WithRequestID(func(w http.ResponseWriter, r *http.Request) {
		observedFromCtx = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(RequestIDHeader, wantID)
	rec := httptest.NewRecorder()
	h(rec, req)

	if observedFromCtx != wantID {
		t.Errorf("ctx id: got %q, want %q", observedFromCtx, wantID)
	}
	if got := rec.Header().Get(RequestIDHeader); got != wantID {
		t.Errorf("response header: got %q, want %q", got, wantID)
	}
}

// TestRequestID_GeneratesUUIDWhenMissing confirms the auto-generation
// path: missing header → fresh UUIDv4 stamped on both context and
// response header.
func TestRequestID_GeneratesUUIDWhenMissing(t *testing.T) {
	var observedFromCtx string
	h := WithRequestID(func(w http.ResponseWriter, r *http.Request) {
		observedFromCtx = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if observedFromCtx == "" {
		t.Fatal("expected a generated request id in context, got empty")
	}
	if got := rec.Header().Get(RequestIDHeader); got != observedFromCtx {
		t.Errorf("response header %q != ctx id %q", got, observedFromCtx)
	}
	// UUIDv4 is 36 chars including hyphens.
	if len(observedFromCtx) != 36 {
		t.Errorf("expected UUIDv4 (36 chars), got %d-char id: %q", len(observedFromCtx), observedFromCtx)
	}
}

// TestRequestID_DistinctPerRequest confirms two requests without
// client-supplied ids receive DIFFERENT generated values.
func TestRequestID_DistinctPerRequest(t *testing.T) {
	var seen []string
	h := WithRequestID(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, RequestIDFromContext(r.Context()))
	})
	for i := 0; i < 5; i++ {
		h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/t", nil))
	}
	uniq := map[string]struct{}{}
	for _, id := range seen {
		uniq[id] = struct{}{}
	}
	if len(uniq) != 5 {
		t.Fatalf("expected 5 distinct ids, got %d (seen=%v)", len(uniq), seen)
	}
}

// TestRequestIDFromContext_MissingMiddlewareReturnsEmpty locks the
// fall-back contract: callers reading the id from a context that
// did not pass through WithRequestID get "" instead of a panic.
func TestRequestIDFromContext_MissingMiddlewareReturnsEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/t", nil)
	if got := RequestIDFromContext(req.Context()); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestAccessLog_EmitsExpectedFields parses the JSON log output and
// asserts every required field is present + carries a sensible
// value.
func TestAccessLog_EmitsExpectedFields(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	innerCalled := false
	wrapped := WithRequestID(WithAccessLog(logger, "/test/{id}", func(w http.ResponseWriter, r *http.Request) {
		innerCalled = true
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("ok"))
	}))
	req := httptest.NewRequest(http.MethodPost, "/test/42", nil)
	req.RemoteAddr = "10.0.0.42:8080"
	req.Header.Set(RequestIDHeader, "req-abc")
	rec := httptest.NewRecorder()
	wrapped(rec, req)

	if !innerCalled {
		t.Fatal("inner handler was not invoked")
	}

	var entry map[string]any
	if err := json.NewDecoder(&buf).Decode(&entry); err != nil {
		t.Fatalf("decode log line: %v (raw=%q)", err, buf.String())
	}
	expectations := map[string]any{
		"method":      "POST",
		"route":       "/test/{id}",
		"status":      float64(http.StatusTeapot),
		"request_id":  "req-abc",
		"remote_addr": "10.0.0.42:8080",
		"msg":         "request",
	}
	for k, want := range expectations {
		if got, ok := entry[k]; !ok || got != want {
			t.Errorf("log field %s: got %v, want %v (full entry: %v)", k, got, want, entry)
		}
	}
	if d, ok := entry["duration_ms"]; !ok {
		t.Errorf("log missing duration_ms field; full entry: %v", entry)
	} else if v, ok := d.(float64); !ok || v < 0 {
		t.Errorf("duration_ms not a non-negative number: %v", d)
	}
}

// TestAccessLog_DefaultsTo200WhenNoExplicitWriteHeader confirms
// the stdlib-implicit 200 surfaces correctly through the access log.
func TestAccessLog_DefaultsTo200WhenNoExplicitWriteHeader(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	wrapped := WithRequestID(WithAccessLog(logger, "/d", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hi")
	}))
	wrapped(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/d", nil))
	if !strings.Contains(buf.String(), `"status":200`) {
		t.Errorf("log missing status:200 default; raw: %s", buf.String())
	}
}
