package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeintel/internal/db"
)

type recordingSearchBackend struct {
	calls []SearchRequest
	body  json.RawMessage
	err   error
}

func TestSearch_BackendTypedErrorsMapToStableHTTP(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"unavailable", ErrSearchBackendUnavailable, http.StatusServiceUnavailable, "SEARCH_BACKEND_UNAVAILABLE"},
		{"invalid_query", ErrSearchInvalidQuery, http.StatusBadRequest, "SEARCH_INVALID_QUERY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, hash := ownerKey(t)
			spy := &upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			}
			srv := newSearchTestServer(spy, &recordingSearchBackend{err: errors.Join(tc.err, errors.New("detail"))})

			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, searchRequest(t, `{"query":"otlp"}`))

			if rec.Code != tc.status {
				t.Fatalf("status: got %d body=%q, want %d", rec.Code, rec.Body.String(), tc.status)
			}
			if !strings.Contains(rec.Body.String(), tc.code) {
				t.Fatalf("body should include %s: %s", tc.code, rec.Body.String())
			}
		})
	}
}

func (r *recordingSearchBackend) Search(_ context.Context, req SearchRequest) (json.RawMessage, error) {
	r.calls = append(r.calls, req)
	if r.err != nil {
		return nil, r.err
	}
	if r.body == nil {
		return json.RawMessage(`{"results":[]}`), nil
	}
	return r.body, nil
}

func newSearchTestServer(spy *upsertConnectionSpy, backend SearchBackend) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		SearchBackend: backend,
	})
}

func searchRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestSearch_ConfiguredBackend_ReturnsRawJSONAndScopesOrg(t *testing.T) {
	_, hash := ownerKey(t)
	backend := &recordingSearchBackend{body: json.RawMessage(`{"results":[{"file":"collector/exporter.go"}],"stats":{"durationMs":7}}`)}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newSearchTestServer(spy, backend)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, searchRequest(t, `{"query":"otlp exporter","count":10}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"results":[{"file":"collector/exporter.go"}],"stats":{"durationMs":7}}` {
		t.Fatalf("body: got %s", got)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("backend calls: got %d, want 1", len(backend.calls))
	}
	call := backend.calls[0]
	if call.OrgID != 7 || call.OrgDomain != "orga" || call.Query != "otlp exporter" {
		t.Fatalf("backend scoped request wrong: %+v", call)
	}
	if got, _ := call.Options["count"].(float64); got != 10 {
		t.Fatalf("option count: got %v, want 10", call.Options["count"])
	}
}

func TestSearch_NoBackend_Returns503NotFakeEmptyResults(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newSearchTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, searchRequest(t, `{"query":"otlp"}`))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d body=%q, want 503", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "SEARCH_BACKEND_NOT_CONFIGURED") {
		t.Fatalf("body should identify missing search backend: %s", got)
	}
}

func TestSearch_RejectsMissingQuery(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newSearchTestServer(spy, &recordingSearchBackend{})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, searchRequest(t, `{"count":10}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d body=%q, want 400", rec.Code, rec.Body.String())
	}
}

func TestSearch_NonOwnerCanSearchWithinOwnOrg(t *testing.T) {
	_, hash := memberKey(t)
	backend := &recordingSearchBackend{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validMemberLookup(hash)},
	}
	srv := newSearchTestServer(spy, backend)

	req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":"otlp"}`))
	req.Header.Set("X-Api-Key", "cik_membersec")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if len(backend.calls) != 1 || backend.calls[0].OrgID != 7 {
		t.Fatalf("backend should receive member's org scope, calls=%+v", backend.calls)
	}
}

func TestSearch_UnknownAPIKeyReturns401(t *testing.T) {
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupErr: db.ErrApiKeyNotFound},
	}
	srv := newSearchTestServer(spy, &recordingSearchBackend{})

	req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(`{"query":"otlp"}`))
	req.Header.Set("X-Api-Key", "cik_unknown")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d body=%q, want 401", rec.Code, rec.Body.String())
	}
}
