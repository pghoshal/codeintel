package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type recordingMCPBackend struct {
	calls []MCPRequest
	resp  MCPResponse
	err   error
}

func (r *recordingMCPBackend) Handle(_ context.Context, req MCPRequest) (MCPResponse, error) {
	r.calls = append(r.calls, req)
	if r.err != nil {
		return MCPResponse{}, r.err
	}
	if r.resp.Body == nil {
		return MCPResponse{Body: []byte(`{"jsonrpc":"2.0","result":{}}`)}, nil
	}
	return r.resp, nil
}

func newMCPTestServer(spy *upsertConnectionSpy, backend MCPBackend) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		MCPBackend:    backend,
	})
}

func mcpRequest(t *testing.T, method, domain, body string) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(method, "/api/"+domain+"/mcp", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestMCP_ConfiguredBackend_ReturnsRawResponseAndScopesOrg(t *testing.T) {
	_, hash := ownerKey(t)
	backend := &recordingMCPBackend{resp: MCPResponse{
		StatusCode:  202,
		ContentType: "application/json",
		Headers:     http.Header{"MCP-Session-Id": []string{"session-1"}},
		Body:        []byte(`{"jsonrpc":"2.0","result":{"tools":[]}}`),
	}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newMCPTestServer(spy, backend)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, mcpRequest(t, http.MethodPost, "orga", `{"jsonrpc":"2.0","method":"tools/list","id":1}`))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status: got %d body=%q, want 202", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("MCP-Session-Id"); got != "session-1" {
		t.Fatalf("session header: got %q, want session-1", got)
	}
	if got := rec.Body.String(); got != `{"jsonrpc":"2.0","result":{"tools":[]}}` {
		t.Fatalf("body: got %s", got)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("backend calls: got %d, want 1", len(backend.calls))
	}
	call := backend.calls[0]
	if call.OrgID != 7 || call.OrgDomain != "orga" || call.Method != http.MethodPost {
		t.Fatalf("backend scoped request wrong: %+v", call)
	}
	if string(call.Body) != `{"jsonrpc":"2.0","method":"tools/list","id":1}` {
		t.Fatalf("body forwarded wrong: %s", string(call.Body))
	}
}

func TestMCP_NoBackendReturns503NotFakeToolList(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newMCPTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, mcpRequest(t, http.MethodPost, "orga", `{}`))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d body=%q, want 503", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); !strings.Contains(got, "MCP_BACKEND_NOT_CONFIGURED") {
		t.Fatalf("body should identify missing MCP backend: %s", got)
	}
}

func TestMCP_DomainMismatchReturns403BeforeBackend(t *testing.T) {
	_, hash := ownerKey(t)
	backend := &recordingMCPBackend{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newMCPTestServer(spy, backend)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, mcpRequest(t, http.MethodPost, "orgb", `{}`))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d body=%q, want 403", rec.Code, rec.Body.String())
	}
	if len(backend.calls) != 0 {
		t.Fatalf("backend must not be called on domain mismatch: %+v", backend.calls)
	}
}

func TestMCP_GETUsesSameTenantBoundary(t *testing.T) {
	_, hash := ownerKey(t)
	backend := &recordingMCPBackend{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newMCPTestServer(spy, backend)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, mcpRequest(t, http.MethodGet, "orga", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status: got %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if len(backend.calls) != 1 {
		t.Fatalf("backend calls: got %d, want 1", len(backend.calls))
	}
	if backend.calls[0].Method != http.MethodGet {
		t.Fatalf("methods forwarded wrong: %+v", backend.calls)
	}
}
