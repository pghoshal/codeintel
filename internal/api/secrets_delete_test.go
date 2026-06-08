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

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// deleteSecretSpy extends the GET-test fakeAuthQuerier with DELETE-
// path fixtures. The reference-check rows are pluggable so each test
// can express its own connection/model state without sharing fixture
// data across tests.
type deleteSecretSpy struct {
	fakeAuthQuerier
	conns        []db.ConfigOwner
	models       []db.ConfigOwner
	deleteCalls  []deleteCall
	deleteErr    error
	connsErr     error
	modelsErr    error
}
type deleteCall struct {
	OrgID int32
	Key   string
}

func (d *deleteSecretSpy) ListOrgConnectionsForRefcheck(ctx context.Context, orgID int32) ([]db.ConfigOwner, error) {
	if d.connsErr != nil {
		return nil, d.connsErr
	}
	if d.conns == nil {
		return make([]db.ConfigOwner, 0), nil
	}
	return d.conns, nil
}
func (d *deleteSecretSpy) ListOrgLanguageModelsForRefcheck(ctx context.Context, orgID int32) ([]db.ConfigOwner, error) {
	if d.modelsErr != nil {
		return nil, d.modelsErr
	}
	if d.models == nil {
		return make([]db.ConfigOwner, 0), nil
	}
	return d.models, nil
}
func (d *deleteSecretSpy) DeleteOrgSecret(ctx context.Context, orgID int32, key string) error {
	d.deleteCalls = append(d.deleteCalls, deleteCall{OrgID: orgID, Key: key})
	return d.deleteErr
}

func newDeleteTestServer(spy *deleteSecretSpy) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
	})
}

func mustDecodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode JSON: %v (%q)", err, string(b))
	}
	return out
}

// TestDeleteOrgSecret_OwnerHappyPath_Returns200 locks the byte-equal
// success response: `{"key":"GH_TOKEN","deleted":true}`.
func TestDeleteOrgSecret_OwnerHappyPath_Returns200(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)
	spy := &deleteSecretSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteTestServer(spy)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/GH_TOKEN", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	want := `{"key":"GH_TOKEN","deleted":true}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
	if len(spy.deleteCalls) != 1 {
		t.Fatalf("expected 1 DELETE call, got %d", len(spy.deleteCalls))
	}
	if spy.deleteCalls[0].OrgID != 7 || spy.deleteCalls[0].Key != "GH_TOKEN" {
		t.Errorf("DELETE called with (%d, %q), want (7, GH_TOKEN)", spy.deleteCalls[0].OrgID, spy.deleteCalls[0].Key)
	}
}

// TestDeleteOrgSecret_URLEncodedKey confirms decodeURIComponent
// parity at the path-segment layer: a request to /api/secrets/MY%2EKEY
// must decode to "MY.KEY" before validation and delete.
func TestDeleteOrgSecret_URLEncodedKey(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)
	spy := &deleteSecretSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := newDeleteTestServer(spy)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/MY%2EKEY", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if len(spy.deleteCalls) != 1 || spy.deleteCalls[0].Key != "MY.KEY" {
		t.Errorf("DELETE key: got %v, want [MY.KEY]", spy.deleteCalls)
	}
}

// TestDeleteOrgSecret_StillReferenced_Returns400 locks the refcheck
// path: any connection or model that references the key blocks
// the delete with a 400 + diagnostic body containing
// "connection:<name>" / "model:<name>".
func TestDeleteOrgSecret_StillReferenced_Returns400(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)

	connCfg := mustDecodeJSON(t, []byte(`{"auth":{"token":{"secretRef":"GH_TOKEN"}}}`))
	modelCfg := mustDecodeJSON(t, []byte(`{"apiKey":{"secretRef":"GH_TOKEN"}}`))
	spy := &deleteSecretSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		conns:           []db.ConfigOwner{{Name: "gh-prod", Config: connCfg}},
		models:          []db.ConfigOwner{{Name: "opus", Config: modelCfg}},
	}
	srv := newDeleteTestServer(spy)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/GH_TOKEN", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d (body=%q), want 400", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Secret 'GH_TOKEN' is still referenced by") {
		t.Errorf("body missing diagnostic prefix: %s", body)
	}
	if !strings.Contains(body, "connection:gh-prod") {
		t.Errorf("body missing connection diagnostic: %s", body)
	}
	if !strings.Contains(body, "model:opus") {
		t.Errorf("body missing model diagnostic: %s", body)
	}
	if len(spy.deleteCalls) != 0 {
		t.Fatalf("DeleteOrgSecret must NOT run when refcheck fails; got %d calls", len(spy.deleteCalls))
	}
}

// TestDeleteOrgSecret_NoReferences_AllowsDelete confirms the
// refcheck doesn't false-positive: a config that references a
// DIFFERENT secret must NOT block the delete.
func TestDeleteOrgSecret_NoReferences_AllowsDelete(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)

	otherCfg := mustDecodeJSON(t, []byte(`{"auth":{"token":{"secretRef":"OTHER_KEY"}}}`))
	spy := &deleteSecretSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		conns:           []db.ConfigOwner{{Name: "gh-prod", Config: otherCfg}},
	}
	srv := newDeleteTestServer(spy)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/GH_TOKEN", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(spy.deleteCalls) != 1 {
		t.Fatalf("expected 1 DELETE call, got %d", len(spy.deleteCalls))
	}
}

// TestDeleteOrgSecret_MemberRole_Returns403 confirms the OWNER
// guard applies. Body must match `"Only organization owners can
// delete secrets."` byte-for-byte.
func TestDeleteOrgSecret_MemberRole_Returns403(t *testing.T) {
	const secret = "membersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &deleteSecretSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup}}
	srv := newDeleteTestServer(spy)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/GH_TOKEN", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can delete secrets."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestDeleteOrgSecret_NoCredentials_Returns401 locks the 401 path
// for the DELETE verb.
func TestDeleteOrgSecret_NoCredentials_Returns401(t *testing.T) {
	spy := &deleteSecretSpy{}
	srv := newDeleteTestServer(spy)
	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/GH_TOKEN", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestDeleteOrgSecret_InvalidKey_Returns400 covers the Zod-equivalent
// path-segment regex: empty, too long, or disallowed chars surface 400
// without reaching the DB.
func TestDeleteOrgSecret_InvalidKey_Returns400(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)
	spy := &deleteSecretSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := newDeleteTestServer(spy)

	cases := []struct {
		name string
		url  string
	}{
		{"empty_after_decode", "/api/secrets/%20"},  // " " — fails regex
		{"too_long", "/api/secrets/" + strings.Repeat("a", 129)},
		{"disallowed_chars", "/api/secrets/has%20space"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodDelete, c.url, nil)
			req.Header.Set("X-Api-Key", "cik_"+secret)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d (body=%q), want 400", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestDeleteOrgSecret_RefcheckQueryError_Returns500 confirms that a
// refcheck query failure surfaces as 500 — never silently allow the
// delete to proceed (which would risk dangling-reference state).
func TestDeleteOrgSecret_RefcheckQueryError_Returns500(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)
	spy := &deleteSecretSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		connsErr:        errors.New("simulated db outage"),
	}
	srv := newDeleteTestServer(spy)

	req := httptest.NewRequest(http.MethodDelete, "/api/secrets/GH_TOKEN", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if len(spy.deleteCalls) != 0 {
		t.Fatalf("DELETE must NOT run when refcheck fails; got %d calls", len(spy.deleteCalls))
	}
	// Raw DB error must NOT leak to client.
	if strings.Contains(rec.Body.String(), "simulated db outage") {
		t.Errorf("raw DB error leaked: %s", rec.Body.String())
	}
}
