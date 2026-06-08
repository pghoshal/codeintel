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

// upsertConnectionSpy records every UpsertOrgConnection call so
// tests can assert the bound config and computed flags. Sync
// missing-ref behaviour is also fakeable.
type upsertConnectionSpy struct {
	fakeAuthQuerier
	upsertParams *db.UpsertOrgConnectionParams
	upsertRow    db.ConnectionListRow
	upsertErr    error
	missing      []string
}

func (u *upsertConnectionSpy) UpsertOrgConnection(ctx context.Context, p db.UpsertOrgConnectionParams) (db.ConnectionListRow, error) {
	u.upsertParams = &p
	if u.upsertErr != nil {
		return db.ConnectionListRow{}, u.upsertErr
	}
	// Default returns the params surface plus a synthetic id when
	// the test didn't pre-populate upsertRow.
	if u.upsertRow.ID == 0 {
		return db.ConnectionListRow{
			ID:             101,
			Name:           p.Name,
			ConnectionType: p.ConnectionType,
			Config:         p.Config,
		}, nil
	}
	return u.upsertRow, nil
}

func (u *upsertConnectionSpy) SelectMissingOrgSecretKeys(ctx context.Context, orgID int32, keys []string) ([]string, error) {
	if u.missing == nil {
		return make([]string, 0), nil
	}
	return u.missing, nil
}

// recordingSyncer captures every Schedule call so the test can
// assert the sync was (or wasn't) invoked with the expected args.
type recordingSyncer struct {
	calls    []SyncRequest
	result   SyncResult
	err      error
}

func (r *recordingSyncer) Schedule(_ context.Context, req SyncRequest) (SyncResult, error) {
	r.calls = append(r.calls, req)
	return r.result, r.err
}

func newPostConnServer(spy *upsertConnectionSpy, syncer ConnectionSyncer) *Server {
	return NewServer(Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:          spy,
		EncryptionKey:    "0123456789abcdef0123456789abcdef",
		ConnectionSyncer: syncer,
	})
}

func ownerKey(t *testing.T) (secret, hash string) {
	t.Helper()
	secret = "ownersec"
	hash = auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	return
}

func postBody(t *testing.T, body string) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodPost, "/api/connections", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestPostConnection_OwnerCreate_Returns200WithRedactedConfig is
// the canonical happy path: a valid OWNER-authenticated request
// creates a connection, the response carries the redacted config
// (apiKey leak verified absent), and the syncer is invoked exactly
// once with the new row's id.
func TestPostConnection_OwnerCreate_Returns200WithRedactedConfig(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, syncer)

	body := `{
		"name": "gh-prod",
		"config": {
			"type": "github",
			"token": {"secretRef":"GH_TOKEN"},
			"enforcePermissions": true
		}
	}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}

	if spy.upsertParams == nil {
		t.Fatalf("UpsertOrgConnection was not called")
	}
	if spy.upsertParams.OrgID != 7 || spy.upsertParams.Name != "gh-prod" || spy.upsertParams.ConnectionType != "github" {
		t.Errorf("Upsert params wrong: %+v", spy.upsertParams)
	}
	if !spy.upsertParams.EnforcePermissions {
		t.Errorf("enforcePermissions should be true")
	}
	if !spy.upsertParams.ResetSync {
		t.Errorf("ResetSync should default to true when body omits sync")
	}
	// The persisted config must have orgId stamped on every secretRef.
	cfg, _ := spy.upsertParams.Config.(map[string]any)
	if cfg == nil {
		t.Fatalf("Config not a map: %T", spy.upsertParams.Config)
	}
	tok, _ := cfg["token"].(map[string]any)
	if tok == nil {
		t.Fatalf("token missing or not a map: %+v", cfg)
	}
	if got, _ := tok["secretRef"].(string); got != "GH_TOKEN" {
		t.Errorf("token.secretRef: got %v, want GH_TOKEN", tok["secretRef"])
	}
	if got, _ := tok["orgId"].(float64); int(got) != 7 {
		t.Errorf("token.orgId: got %v, want 7 (BindToOrg must run before persistence)", tok["orgId"])
	}

	// The response must have the redacted config — orgId stripped,
	// only the kind-discriminator surviving.
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (raw=%q)", err, rec.Body.String())
	}
	respCfg, _ := resp["config"].(map[string]any)
	if respCfg == nil {
		t.Fatalf("response config not a map")
	}
	respTok, _ := respCfg["token"].(map[string]any)
	if respTok == nil {
		t.Fatalf("response token not a map")
	}
	if _, hasOrg := respTok["orgId"]; hasOrg {
		t.Errorf("response config leaked orgId after Redact: %v", respTok)
	}
	if got, _ := respTok["secretRef"].(string); got != "GH_TOKEN" {
		t.Errorf("response secretRef: got %v, want GH_TOKEN", respTok["secretRef"])
	}

	if len(syncer.calls) != 1 {
		t.Fatalf("syncer should have been called once, got %d", len(syncer.calls))
	}
	if syncer.calls[0].OrgID != 7 || syncer.calls[0].ConnectionID != 101 {
		t.Errorf("syncer call args: got %+v, want (7, 101)", syncer.calls[0])
	}
}

// TestPostConnection_SyncTrueExplicit_SchedulesSync confirms the
// `"sync":true` body field is honoured separately from the absent
// (default-true) path so a future change to the default cannot
// silently break clients that depend on explicit-true.
func TestPostConnection_SyncTrueExplicit_SchedulesSync(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, syncer)

	body := `{"name":"gh-prod","config":{"type":"github"},"sync":true}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if len(syncer.calls) != 1 {
		t.Fatalf("syncer should have been called once for sync:true, got %d", len(syncer.calls))
	}
	if !spy.upsertParams.ResetSync {
		t.Errorf("ResetSync should be true when body says sync:true")
	}
}

// TestPostConnection_TypeNotString_Returns400 covers the
// `cfgMap["type"].(string)` boundary: a numeric or null type must
// surface 400 rather than silently coercing to empty string and
// then being rejected via the empty-check.
func TestPostConnection_TypeNotString_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, &recordingSyncer{})

	cases := []struct {
		name string
		body string
	}{
		{"type_is_number", `{"name":"gh","config":{"type":123}}`},
		{"type_is_null", `{"name":"gh","config":{"type":null}}`},
		{"type_is_bool", `{"name":"gh","config":{"type":true}}`},
		{"type_is_array", `{"name":"gh","config":{"type":["github"]}}`},
		{"type_is_object", `{"name":"gh","config":{"type":{"v":"github"}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy.upsertParams = nil
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, postBody(t, c.body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("input %q: status %d, want 400 (body=%q)", c.body, rec.Code, rec.Body.String())
			}
			if spy.upsertParams != nil {
				t.Errorf("input %q: Upsert must not run", c.body)
			}
		})
	}
}

// TestPostConnection_HappyPath_ResponseShapeComplete asserts every
// field of the connectionListItem projection lands in the response.
// The canonical happy-path test only checks a subset; this locks
// the rest so a regression in the response struct fails loudly.
func TestPostConnection_HappyPath_ResponseShapeComplete(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		upsertRow: db.ConnectionListRow{
			ID:                               42,
			Name:                             "gh-prod",
			ConnectionType:                   "github",
			Config:                           map[string]any{"type": "github"},
			IsDeclarative:                    false,
			SyncedAt:                         nil,
			EnforcePermissions:               true,
			EnforcePermissionsForPublicRepos: false,
		},
	}
	srv := newPostConnServer(spy, syncer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, `{"name":"gh-prod","config":{"type":"github","enforcePermissions":true}}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Every connectionListItem field must be present and carry the
	// stub-row value.
	if got, _ := resp["id"].(float64); int(got) != 42 {
		t.Errorf("id: got %v, want 42", resp["id"])
	}
	if got, _ := resp["name"].(string); got != "gh-prod" {
		t.Errorf("name: got %v, want gh-prod", resp["name"])
	}
	if got, _ := resp["connectionType"].(string); got != "github" {
		t.Errorf("connectionType: got %v, want github", resp["connectionType"])
	}
	if got, ok := resp["isDeclarative"].(bool); !ok || got != false {
		t.Errorf("isDeclarative: got %v, want false", resp["isDeclarative"])
	}
	if resp["syncedAt"] != nil {
		t.Errorf("syncedAt: got %v, want null", resp["syncedAt"])
	}
	if got, ok := resp["enforcePermissions"].(bool); !ok || got != true {
		t.Errorf("enforcePermissions: got %v, want true", resp["enforcePermissions"])
	}
	if got, ok := resp["enforcePermissionsForPublicRepos"].(bool); !ok || got != false {
		t.Errorf("enforcePermissionsForPublicRepos: got %v, want false", resp["enforcePermissionsForPublicRepos"])
	}
}

// TestPostConnection_SyncFalse_DoesNotSchedule confirms the sync
// argument is honoured when explicitly false.
func TestPostConnection_SyncFalse_DoesNotSchedule(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, syncer)

	body := `{"name":"gh-prod","config":{"type":"github"},"sync":false}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(syncer.calls) != 0 {
		t.Fatalf("syncer should NOT have been called when sync=false; got %d", len(syncer.calls))
	}
	if spy.upsertParams != nil && spy.upsertParams.ResetSync {
		t.Errorf("ResetSync should be false when body explicitly says sync:false")
	}
}

// TestPostConnection_AtCapacity_Returns429 locks the soft-reject
// path: when the syncer reports AlreadyAtCapacity the handler
// returns 429 with the canonical message.
func TestPostConnection_AtCapacity_Returns429(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{result: SyncResult{AlreadyAtCapacity: true}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, syncer)

	body := `{"name":"gh-prod","config":{"type":"github"}}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, body))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", rec.Code)
	}
	want := `{"statusCode":429,"errorCode":"CONNECTION_SYNC_ALREADY_SCHEDULED","message":"Connection sync was not scheduled because the tenant is at active sync capacity. Retry after the current sync jobs finish."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestPostConnection_SyncBackendError_Returns502 verifies that a
// hard syncer failure surfaces as 502 (not 500) so operators can
// distinguish the sync-backend outage from a data-layer failure.
func TestPostConnection_SyncBackendError_Returns502(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{err: errors.New("simulated indexer outage")}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, syncer)

	body := `{"name":"gh-prod","config":{"type":"github"}}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, body))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated indexer outage") {
		t.Errorf("raw syncer error leaked: %s", rec.Body.String())
	}
}

// TestPostConnection_MissingSecretRefs_Returns400 locks the
// referential-integrity check: a config that points at an
// undefined secret blocks the write.
func TestPostConnection_MissingSecretRefs_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		missing:         []string{"GHOST_A"},
	}
	srv := newPostConnServer(spy, syncer)

	body := `{"name":"x","config":{"type":"y","token":{"secretRef":"GHOST_A"}}}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d (body=%q), want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "GHOST_A") {
		t.Errorf("body missing GHOST_A diagnostic: %s", rec.Body.String())
	}
	if spy.upsertParams != nil {
		t.Fatalf("UpsertOrgConnection must NOT run when refs are missing")
	}
	if len(syncer.calls) != 0 {
		t.Fatalf("syncer must NOT run when refs are missing")
	}
}

// TestPostConnection_InvalidBody covers the structural-validation
// path: missing name, missing config, missing type, non-object
// config, oversize body, malformed JSON.
func TestPostConnection_InvalidBody(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, &recordingSyncer{})

	cases := []struct {
		name string
		body string
	}{
		{"not_json", `oops`},
		{"missing_name", `{"config":{"type":"github"}}`},
		{"empty_name", `{"name":"","config":{"type":"github"}}`},
		{"missing_config", `{"name":"gh"}`},
		{"config_not_object", `{"name":"gh","config":"not-an-object"}`},
		{"missing_type", `{"name":"gh","config":{}}`},
		{"empty_type", `{"name":"gh","config":{"type":""}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy.upsertParams = nil
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, postBody(t, c.body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status: got %d, want 400 (body=%q)", rec.Code, rec.Body.String())
			}
			if spy.upsertParams != nil {
				t.Errorf("Upsert must not run on invalid body")
			}
		})
	}
}

// TestPostConnection_OversizedBody_Returns413 confirms the
// MaxBytesReader gate.
func TestPostConnection_OversizedBody_Returns413(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, &recordingSyncer{})

	huge := strings.Repeat("a", 512*1024)
	body := `{"name":"gh","config":{"type":"github","note":"` + huge + `"}}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", rec.Code)
	}
}

// TestPostConnection_MemberRole_Returns403 confirms the OWNER
// guard.
func TestPostConnection_MemberRole_Returns403(t *testing.T) {
	_, hash := ownerKey(t)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &upsertConnectionSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup}}
	srv := newPostConnServer(spy, &recordingSyncer{})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, `{"name":"x","config":{"type":"y"}}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can configure code host connections."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestPostConnection_NoCredentials_Returns401 covers the 401 path.
func TestPostConnection_NoCredentials_Returns401(t *testing.T) {
	spy := &upsertConnectionSpy{}
	srv := newPostConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/connections", strings.NewReader(`{}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestPostConnection_UpsertError_Returns500 covers a DB outage.
func TestPostConnection_UpsertError_Returns500(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		upsertErr:       errors.New("simulated db outage"),
	}
	srv := newPostConnServer(spy, &recordingSyncer{})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, `{"name":"x","config":{"type":"y"}}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated db outage") {
		t.Errorf("raw DB error leaked: %s", rec.Body.String())
	}
}

// TestPostConnection_NoConfiguredSyncer_UsesNoop confirms the
// default-syncer path: even with Config.ConnectionSyncer = nil
// the request succeeds without crashing.
func TestPostConnection_NoConfiguredSyncer_UsesNoop(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newPostConnServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, `{"name":"x","config":{"type":"y"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}
