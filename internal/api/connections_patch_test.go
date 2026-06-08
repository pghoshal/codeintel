package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeintel/internal/db"

	"github.com/jackc/pgx/v5/pgconn"
)

// patchConnSpy extends upsertConnectionSpy with PATCH-flow hooks:
// the existing-row fetch, the name-availability check, and the
// final upsert. Records every call so the tests can assert
// behaviour precisely.
type patchConnSpy struct {
	upsertConnectionSpy
	existingRow db.ConnectionListRow
	existingErr error

	nameCheckCalls []nameCheckArgs
	nameCheckErr   error
}
type nameCheckArgs struct {
	OrgID     int32
	Name      string
	ExcludeID int32
}

func (p *patchConnSpy) GetOrgConnectionForUpdate(ctx context.Context, orgID, connectionID int32) (db.ConnectionListRow, error) {
	if p.existingErr != nil {
		return db.ConnectionListRow{}, p.existingErr
	}
	return p.existingRow, nil
}

func (p *patchConnSpy) CheckOrgConnectionNameAvailable(ctx context.Context, orgID int32, name string, excludeID int32) error {
	p.nameCheckCalls = append(p.nameCheckCalls, nameCheckArgs{OrgID: orgID, Name: name, ExcludeID: excludeID})
	return p.nameCheckErr
}

func newPatchConnServer(spy *patchConnSpy, syncer ConnectionSyncer) *Server {
	return NewServer(Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:          spy,
		EncryptionKey:    "0123456789abcdef0123456789abcdef",
		ConnectionSyncer: syncer,
	})
}

func patchBody(t *testing.T, id int, body string) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/connections/"+itoa(id), strings.NewReader(body))
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// itoa avoids strconv import in tests (keeps the import block
// minimal and stable across edits).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func existingRow(id int32) db.ConnectionListRow {
	return db.ConnectionListRow{
		ID:                               id,
		Name:                             "gh-prod",
		ConnectionType:                   "github",
		Config:                           map[string]any{"type": "github", "branches": []any{"main"}},
		IsDeclarative:                    false,
		EnforcePermissions:               true,
		EnforcePermissionsForPublicRepos: false,
	}
}

// TestPatchConnection_NameChange_Succeeds locks the happy
// rename path: the name-availability check fires with the right
// args, then the upsert writes the row with the new name. Config
// is preserved verbatim from the merge base.
func TestPatchConnection_NameChange_Succeeds(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, syncer)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"gh-staging"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if len(spy.nameCheckCalls) != 1 {
		t.Fatalf("name-availability check should fire exactly once, got %d", len(spy.nameCheckCalls))
	}
	if spy.nameCheckCalls[0].Name != "gh-staging" || spy.nameCheckCalls[0].ExcludeID != 42 {
		t.Errorf("name-check args wrong: %+v", spy.nameCheckCalls[0])
	}
	if spy.upsertParams == nil || spy.upsertParams.Name != "gh-staging" {
		t.Fatalf("upsert called with wrong name: %+v", spy.upsertParams)
	}
	// Config must come from the merge base (unchanged).
	cfg, _ := spy.upsertParams.Config.(map[string]any)
	if got, _ := cfg["type"].(string); got != "github" {
		t.Errorf("merged config type: got %v, want github (preserved from existing)", cfg["type"])
	}
	// sync must NOT have fired — body omitted the field.
	if len(syncer.calls) != 0 {
		t.Errorf("syncer should NOT fire when body has no sync field; got %d", len(syncer.calls))
	}
}

// TestPatchConnection_NameUnchanged_SkipsAvailabilityCheck
// confirms the optimisation: when name matches the existing
// value, the DB conflict-check is NOT performed (saves a round-trip).
func TestPatchConnection_NameUnchanged_SkipsAvailabilityCheck(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"gh-prod"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(spy.nameCheckCalls) != 0 {
		t.Fatalf("name-check should NOT fire when name equals existing; got %d", len(spy.nameCheckCalls))
	}
}

// TestPatchConnection_NameConflict_Returns400 locks the
// conflict-diagnostic shape.
func TestPatchConnection_NameConflict_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow:  existingRow(42),
		nameCheckErr: db.ErrConnectionNameConflict,
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"gh-staging"}`))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	want := `{"statusCode":400,"errorCode":"CONNECTION_ALREADY_EXISTS","message":"Connection 'gh-staging' already exists."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
	if spy.upsertParams != nil {
		t.Fatalf("upsert must NOT run on name conflict")
	}
}

// TestPatchConnection_ConfigChange_BindsAndRefchecks locks the
// config-change path: BindToOrg stamps orgIds onto every
// secretRef in the new config, missing-secret check runs.
func TestPatchConnection_ConfigChange_BindsAndRefchecks(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})

	body := `{"config":{"type":"gitlab","token":{"secretRef":"GL_TOKEN"}}}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if spy.upsertParams == nil {
		t.Fatalf("upsert was not called")
	}
	if spy.upsertParams.ConnectionType != "gitlab" {
		t.Errorf("ConnectionType: got %q, want gitlab", spy.upsertParams.ConnectionType)
	}
	cfg, _ := spy.upsertParams.Config.(map[string]any)
	tok, _ := cfg["token"].(map[string]any)
	if tok == nil {
		t.Fatalf("token not bound in config: %+v", cfg)
	}
	if got, _ := tok["orgId"].(float64); int(got) != 7 {
		t.Errorf("token.orgId: got %v, want 7", tok["orgId"])
	}
}

// TestPatchConnection_MissingSecretRefs_Returns400.
func TestPatchConnection_MissingSecretRefs_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			missing:         []string{"GHOST"},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"config":{"type":"gh","token":{"secretRef":"GHOST"}}}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "GHOST") {
		t.Errorf("body missing GHOST diagnostic: %s", rec.Body.String())
	}
	if spy.upsertParams != nil {
		t.Fatalf("upsert must NOT run on missing refs")
	}
}

// TestPatchConnection_SyncTrue_SchedulesSync covers the
// resync-on-PATCH flag.
func TestPatchConnection_SyncTrue_SchedulesSync(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, syncer)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"sync":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(syncer.calls) != 1 {
		t.Fatalf("syncer should have been called once, got %d", len(syncer.calls))
	}
	if !spy.upsertParams.ResetSync {
		t.Errorf("ResetSync should be true when body says sync:true")
	}
}

// TestPatchConnection_EmptyBody_Returns400 locks the "at least
// one mutable field" requirement.
func TestPatchConnection_EmptyBody_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "at least one") {
		t.Errorf("body should mention the at-least-one requirement; got %s", rec.Body.String())
	}
	if spy.upsertParams != nil {
		t.Fatalf("upsert must NOT run on empty patch")
	}
}

// TestPatchConnection_NotFound_Returns404 covers the path where
// the connection id does not exist (or belongs to another org).
func TestPatchConnection_NotFound_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingErr: db.ErrConnectionNotFound,
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 999, `{"name":"new"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	want := `{"statusCode":404,"errorCode":"NOT_FOUND","message":"Connection not found."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestPatchConnection_NonIntegerID_Returns400 covers the path-
// segment validation.
func TestPatchConnection_NonIntegerID_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodPatch, "/api/connections/abc", strings.NewReader(`{"name":"new"}`))
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

// TestPatchConnection_MemberRole_Returns403.
func TestPatchConnection_MemberRole_Returns403(t *testing.T) {
	_, hash := ownerKey(t)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup},
		},
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"x"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
}

// TestPatchConnection_NoCredentials_Returns401.
func TestPatchConnection_NoCredentials_Returns401(t *testing.T) {
	spy := &patchConnSpy{}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPatch, "/api/connections/42", strings.NewReader(`{"name":"x"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestPatchConnection_SyncOmitted_DefaultsFalse locks the
// PATCH-specific sync semantic: when the body omits the `sync`
// field, no schedule is fired AND ResetSync stays false. This is
// the opposite of POST (which defaults to true on absence) — a
// regression would silently re-sync every PATCH.
func TestPatchConnection_SyncOmitted_DefaultsFalse(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, syncer)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"gh-staging"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(syncer.calls) != 0 {
		t.Errorf("syncer must NOT fire when sync field is absent on PATCH; got %d", len(syncer.calls))
	}
	if spy.upsertParams.ResetSync {
		t.Errorf("ResetSync must default to false on PATCH when sync field is absent")
	}
}

// TestPatchConnection_AtCapacity_Returns429 locks the soft-reject
// response when the sync backend reports it is at capacity.
func TestPatchConnection_AtCapacity_Returns429(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{result: SyncResult{AlreadyAtCapacity: true}}
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, syncer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"sync":true}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", rec.Code)
	}
	want := `{"statusCode":429,"errorCode":"CONNECTION_SYNC_ALREADY_SCHEDULED","message":"Connection sync was not scheduled because the tenant is at active sync capacity. Retry after the current sync jobs finish."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestPatchConnection_SyncBackendError_Returns502 covers a hard
// syncer failure: the upsert was committed but the sync could
// not be scheduled, so the response is 502 (not 500) to flag
// the sync-backend outage to operators.
func TestPatchConnection_SyncBackendError_Returns502(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{err: errors.New("simulated indexer outage")}
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, syncer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"sync":true}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated indexer outage") {
		t.Errorf("raw syncer error leaked: %s", rec.Body.String())
	}
}

// TestPatchConnection_HappyPath_ResponseShapeComplete locks every
// connectionListItem field that lands in the response.
func TestPatchConnection_HappyPath_ResponseShapeComplete(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			upsertRow: db.ConnectionListRow{
				ID:                               42,
				Name:                             "gh-staging",
				ConnectionType:                   "github",
				Config:                           map[string]any{"type": "github"},
				IsDeclarative:                    false,
				SyncedAt:                         nil,
				EnforcePermissions:               true,
				EnforcePermissionsForPublicRepos: false,
			},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"gh-staging"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"id":42`,
		`"name":"gh-staging"`,
		`"connectionType":"github"`,
		`"isDeclarative":false`,
		`"syncedAt":null`,
		`"enforcePermissions":true`,
		`"enforcePermissionsForPublicRepos":false`,
		`"config":{`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("response body missing %q\nbody: %s", want, body)
		}
	}
}

// TestPatchConnection_ConfigTypeNotString_Returns400 covers the
// `cfgMap["type"].(string)` boundary on the PATCH config path.
func TestPatchConnection_ConfigTypeNotString_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	cases := []struct {
		name string
		body string
	}{
		{"type_is_number", `{"config":{"type":123}}`},
		{"type_is_null", `{"config":{"type":null}}`},
		{"type_is_bool", `{"config":{"type":true}}`},
		{"type_is_array", `{"config":{"type":["github"]}}`},
		{"type_is_object", `{"config":{"type":{"v":"github"}}}`},
		{"type_missing", `{"config":{}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy.upsertParams = nil
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, patchBody(t, 42, c.body))
			if rec.Code != http.StatusBadRequest {
				t.Errorf("input %q: status %d, want 400 (body=%q)", c.body, rec.Code, rec.Body.String())
			}
			if spy.upsertParams != nil {
				t.Errorf("input %q: upsert must not run", c.body)
			}
		})
	}
}

// TestPatchConnection_OversizedBody_Returns413 confirms the body
// cap protects the handler from memory exhaustion.
func TestPatchConnection_OversizedBody_Returns413(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})

	huge := strings.Repeat("a", 512*1024)
	body := `{"config":{"type":"github","note":"` + huge + `"}}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", rec.Code)
	}
}

// TestPatchConnection_EmptyStringName_Returns400 covers the
// "name supplied but blank" boundary distinct from the
// "name absent" case.
func TestPatchConnection_EmptyStringName_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":""}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "must not be empty") {
		t.Errorf("body should mention the empty-name guard; got %s", rec.Body.String())
	}
	if spy.upsertParams != nil {
		t.Fatalf("upsert must not run on empty-string name")
	}
}

// TestPatchConnection_ConcurrentNameInsert_Returns400 covers the
// race-loss path: the availability check passed, but the upsert
// hit a unique-constraint violation because a concurrent INSERT
// snuck the name in first. The handler must map SQLSTATE 23505
// to a 400 so the client knows it was a soft conflict.
func TestPatchConnection_ConcurrentNameInsert_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			upsertErr:       &pgconn.PgError{Code: "23505", Message: "duplicate key"},
		},
		existingRow: existingRow(42),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"gh-staging"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "CONNECTION_ALREADY_EXISTS") {
		t.Errorf("body should carry CONNECTION_ALREADY_EXISTS code; got %s", rec.Body.String())
	}
}

// TestPatchConnection_DBError_Returns500WithoutLeak.
func TestPatchConnection_DBError_Returns500WithoutLeak(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingErr: errors.New("simulated db outage"),
	}
	srv := newPatchConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"x"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated db outage") {
		t.Errorf("raw DB error leaked: %s", rec.Body.String())
	}
}
