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
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

type connectionsSpy struct {
	fakeAuthQuerier
	rows []db.ConnectionListRow
	err  error
}

func (c *connectionsSpy) ListOrgConnectionsForRead(ctx context.Context, orgID int32) ([]db.ConnectionListRow, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.rows == nil {
		return make([]db.ConnectionListRow, 0), nil
	}
	return c.rows, nil
}

func newConnectionsTestServer(spy *connectionsSpy) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
	})
}

// TestConnections_Empty_Returns200WithEmptyArray locks the empty
// case: zero connections -> 200 + `[]`.
func TestConnections_Empty_Returns200WithEmptyArray(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &connectionsSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newConnectionsTestServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]" {
		t.Fatalf("body: got %q, want []", got)
	}
}

// TestConnections_RedactsSensitiveConfigFields locks the security
// contract: every config blob must pipe through Redact before
// serialisation. The handler MUST NEVER echo back unredacted
// secretRef / env / googleCloudSecret records.
func TestConnections_RedactsSensitiveConfigFields(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)

	// Decoded JSON config carrying TWO leak surfaces:
	// 1. A secretRef with orgId sibling (post-bind output)
	// 2. A literal secret-like scalar at top level.
	cfg := mustDecodeJSON(t, []byte(`{
		"auth": {"token": {"secretRef":"GH_TOKEN","orgId":7}},
		"hardcodedSecret": "DO_NOT_LEAK_ME",
		"branches": ["main"]
	}`))
	syncedAt := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	spy := &connectionsSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		rows: []db.ConnectionListRow{{
			ID:                               42,
			Name:                             "gh-prod",
			ConnectionType:                   "github",
			Config:                           cfg,
			IsDeclarative:                    false,
			SyncedAt:                         &syncedAt,
			EnforcePermissions:               true,
			EnforcePermissionsForPublicRepos: false,
		}},
	}
	srv := newConnectionsTestServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Redaction critical: orgId sibling must be stripped, but
	// "branches" (non-secret content) survives.
	if !strings.Contains(body, `"secretRef":"GH_TOKEN"`) {
		t.Errorf("response missing legitimate secretRef field: %s", body)
	}
	if strings.Contains(body, `"orgId":7`) {
		t.Errorf("orgId sibling LEAKED in redacted output: %s", body)
	}
	if strings.Contains(body, "DO_NOT_LEAK_ME") {
		t.Errorf("literal scalar secret LEAKED in redacted output: %s", body)
	}
	if !strings.Contains(body, `"hardcodedSecret":"[REDACTED]"`) {
		t.Errorf("literal scalar secret key should be masked: %s", body)
	}
	if !strings.Contains(body, `"branches":["main"]`) {
		t.Errorf("non-secret fields should pass through: %s", body)
	}
	// Standard fields are present.
	for _, want := range []string{
		`"id":42`,
		`"name":"gh-prod"`,
		`"connectionType":"github"`,
		`"isDeclarative":false`,
		`"syncedAt":"2025-06-01T12:00:00.000Z"`,
		`"enforcePermissions":true`,
		`"enforcePermissionsForPublicRepos":false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody: %s", want, body)
		}
	}
}

// TestConnections_NullSyncedAtSerializesAsNull confirms that a row
// with no SyncedAt emits `"syncedAt":null` rather than omitting the
// field — every selected column must appear in the JSON output.
func TestConnections_NullSyncedAtSerializesAsNull(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &connectionsSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		rows: []db.ConnectionListRow{{
			ID: 1, Name: "x", ConnectionType: "github",
			Config: map[string]any{},
		}},
	}
	srv := newConnectionsTestServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got, present := decoded[0]["syncedAt"]; !present || got != nil {
		t.Errorf("syncedAt: got %v (present=%v), want null", got, present)
	}
}

// TestConnections_MemberRole_Returns403 confirms the OWNER guard:
// the canonical "Only organization owners can list connection
// configuration." message must be byte-equal.
func TestConnections_MemberRole_Returns403(t *testing.T) {
	const secret = "membersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &connectionsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup}}
	srv := newConnectionsTestServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can list connection configuration."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestConnections_NoCredentials_Returns401 covers the 401 path.
func TestConnections_NoCredentials_Returns401(t *testing.T) {
	spy := &connectionsSpy{}
	srv := newConnectionsTestServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/connections", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestConnections_DBError_Returns500WithoutLeak covers an outage.
func TestConnections_DBError_Returns500WithoutLeak(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &connectionsSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		err:             errors.New("simulated db outage"),
	}
	srv := newConnectionsTestServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/connections", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated db outage") {
		t.Errorf("raw db error LEAKED: %s", rec.Body.String())
	}
}
