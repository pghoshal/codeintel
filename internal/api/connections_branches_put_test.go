package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"codeintel/pkg/audit"
	"codeintel/internal/db"
)

// branchesPutSpy plugs the merge-base read + the post-upsert meta
// re-fetch in addition to the standard upsert hook.
type branchesPutSpy struct {
	patchConnSpy
	metaRow db.ConnectionMetaRow
	metaErr error
}

func (b *branchesPutSpy) GetOrgConnectionMeta(ctx context.Context, orgID, connectionID int32) (db.ConnectionMetaRow, error) {
	if b.metaErr != nil {
		return db.ConnectionMetaRow{}, b.metaErr
	}
	return b.metaRow, nil
}

func newBranchesPutServer(spy *branchesPutSpy, syncer ConnectionSyncer, emitter audit.Emitter) *Server {
	return NewServer(Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:          spy,
		EncryptionKey:    "0123456789abcdef0123456789abcdef",
		ConnectionSyncer: syncer,
		AuditEmitter:     emitter,
	})
}

func branchesPutRequest(t *testing.T, id int, body string) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodPut, "/api/connections/"+itoa(id)+"/branches", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func mustConfig(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode fixture %s: %v", s, err)
	}
	return out
}

// TestApplyBranchPolicy_TableLocksTreeMutation locks every mode of
// the pure helper in isolation so a refactor of the handler glue
// can't silently regress the merge logic.
func TestApplyBranchPolicy_TableLocksTreeMutation(t *testing.T) {
	cases := []struct {
		name     string
		startCfg string
		mode     branchSyncMode
		branches []string
		wantCfg  string
		wantErr  bool
	}{
		{"default_strips_branches", `{"type":"github","revisions":{"branches":["main"]}}`, branchSyncModeDefault, nil, `{"type":"github"}`, false},
		{"default_idempotent", `{"type":"github"}`, branchSyncModeDefault, nil, `{"type":"github"}`, false},
		{"all_sets_wildcard", `{"type":"github"}`, branchSyncModeAll, nil, `{"type":"github","revisions":{"branches":["*"]}}`, false},
		{"all_overwrites_existing", `{"type":"github","revisions":{"branches":["main","dev"]}}`, branchSyncModeAll, nil, `{"type":"github","revisions":{"branches":["*"]}}`, false},
		{"patterns_sets_normalised", `{"type":"github"}`, branchSyncModePatterns, []string{"main", " dev ", "main"}, `{"type":"github","revisions":{"branches":["main","dev"]}}`, false},
		{"patterns_empty_errors", `{"type":"github"}`, branchSyncModePatterns, []string{}, "", true},
		{"patterns_whitespace_only_errors", `{"type":"github"}`, branchSyncModePatterns, []string{"  "}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := mustConfig(t, c.startCfg)
			got, err := applyBranchPolicy(cfg, c.mode, c.branches)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got cfg %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			want := mustConfig(t, c.wantCfg)
			if !reflect.DeepEqual(got, want) {
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				t.Fatalf("config mismatch:\n  got  %s\n  want %s", gb, wb)
			}
		})
	}
}

// TestBranchesPut_Default_StripsBranches locks the wire-level
// behaviour for the default mode + verifies the upsert was
// called with the stripped config and the audit event fired.
func TestBranchesPut_Default_StripsBranches(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	emitter := &recordingEmitter{}
	existingCfg := mustConfig(t, `{"type":"github","revisions":{"branches":["main","dev"]}}`)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: db.ConnectionListRow{
				ID:             42,
				Name:           "gh-prod",
				ConnectionType: "github",
				Config:         existingCfg,
			},
		},
		metaRow: db.ConnectionMetaRow{
			ID:             42,
			Name:           "gh-prod",
			ConnectionType: "github",
			Config:         map[string]any{"type": "github"},
			UpdatedAt:      time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC),
		},
	}
	srv := newBranchesPutServer(spy, syncer, emitter)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 42, `{"mode":"default"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if spy.upsertParams == nil {
		t.Fatalf("upsert not called")
	}
	cfg, _ := spy.upsertParams.Config.(map[string]any)
	if _, has := cfg["revisions"]; has {
		t.Errorf("default mode must strip revisions key, got %v", cfg)
	}
	if !spy.upsertParams.ResetSync {
		t.Errorf("sync defaults to true on PUT branches; ResetSync should be true")
	}
	if len(syncer.calls) != 1 {
		t.Fatalf("syncer should fire once, got %d", len(syncer.calls))
	}
	if len(emitter.events) != 1 || emitter.events[0].Action != "connection.branches_updated" {
		t.Fatalf("audit event: %+v", emitter.events)
	}
	if got, _ := emitter.events[0].Metadata["mode"].(string); got != "default" {
		t.Errorf("audit metadata mode: got %q, want default", got)
	}
}

// TestBranchesPut_All_SetsWildcard locks the all mode.
func TestBranchesPut_All_SetsWildcard(t *testing.T) {
	_, hash := ownerKey(t)
	existingCfg := mustConfig(t, `{"type":"github"}`)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: db.ConnectionListRow{
				ID: 42, Name: "x", ConnectionType: "github", Config: existingCfg,
			},
		},
		metaRow: db.ConnectionMetaRow{
			ID: 42, Name: "x", ConnectionType: "github",
			Config:    map[string]any{"type": "github", "revisions": map[string]any{"branches": []any{"*"}}},
			UpdatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 42, `{"mode":"all","sync":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	cfg, _ := spy.upsertParams.Config.(map[string]any)
	rev, _ := cfg["revisions"].(map[string]any)
	branches, _ := rev["branches"].([]any)
	if len(branches) != 1 {
		t.Fatalf("branches: got %+v, want [*]", branches)
	}
	if branches[0] != "*" {
		t.Errorf("branches[0]: got %v, want *", branches[0])
	}
}

// TestBranchesPut_Patterns_Normalises locks the patterns mode +
// dedup/trim semantics.
func TestBranchesPut_Patterns_Normalises(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: db.ConnectionListRow{
				ID: 42, Name: "x", ConnectionType: "github", Config: mustConfig(t, `{"type":"github"}`),
			},
		},
		metaRow: db.ConnectionMetaRow{ID: 42, Name: "x", ConnectionType: "github", Config: map[string]any{}, UpdatedAt: time.Now()},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 42, `{"mode":"patterns","branches":["main"," dev ","main"]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	cfg, _ := spy.upsertParams.Config.(map[string]any)
	rev, _ := cfg["revisions"].(map[string]any)
	branches, _ := rev["branches"].([]any)
	if len(branches) != 2 {
		t.Fatalf("expected normalised to 2 entries, got %v", branches)
	}
	if branches[0] != "main" || branches[1] != "dev" {
		t.Errorf("expected [main, dev], got %v", branches)
	}
}

// TestBranchesPut_PatternsEmpty_Returns400 covers the
// "patterns mode requires at least one entry" boundary.
func TestBranchesPut_PatternsEmpty_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: db.ConnectionListRow{ID: 1, Config: map[string]any{"type": "g"}},
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 1, `{"mode":"patterns"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "patterns") {
		t.Errorf("body should mention the patterns requirement; got %s", rec.Body.String())
	}
	if spy.upsertParams != nil {
		t.Fatalf("upsert must not run on invalid mode body")
	}
}

// TestBranchesPut_UnknownMode_Returns400.
func TestBranchesPut_UnknownMode_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	for _, body := range []string{`{}`, `{"mode":""}`, `{"mode":"weird"}`} {
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, branchesPutRequest(t, 1, body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: got %d, want 400 (body=%q)", body, rec.Code, rec.Body.String())
		}
	}
}

// TestBranchesPut_NotFound_Returns404 covers the merge-base miss.
func TestBranchesPut_NotFound_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingErr: db.ErrConnectionNotFound,
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 999, `{"mode":"default"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
}

// TestBranchesPut_MemberRole_Returns403.
func TestBranchesPut_MemberRole_Returns403(t *testing.T) {
	_, hash := ownerKey(t)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup},
			},
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 1, `{"mode":"default"}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can update branch sync policy."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestBranchesPut_NoCredentials_Returns401.
func TestBranchesPut_NoCredentials_Returns401(t *testing.T) {
	spy := &branchesPutSpy{}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/connections/1/branches", strings.NewReader(`{"mode":"default"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestBranchesPut_AtCapacity_Returns429.
func TestBranchesPut_AtCapacity_Returns429(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: db.ConnectionListRow{ID: 1, Config: map[string]any{"type": "g"}},
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{result: SyncResult{AlreadyAtCapacity: true}}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 1, `{"mode":"all"}`))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", rec.Code)
	}
}

// TestBranchesPut_ExistingConfigNotObject_Returns500 covers the
// defensive type-assert on the merge-base config. A row whose
// config column is a non-object value (corrupt / unexpected
// shape) must surface 500 instead of silently overwriting with a
// fresh object — operators need to investigate.
func TestBranchesPut_ExistingConfigNotObject_Returns500(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: db.ConnectionListRow{
				ID:             42,
				Name:           "x",
				ConnectionType: "github",
				Config:         "not-a-map",
			},
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 42, `{"mode":"default"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if spy.upsertParams != nil {
		t.Errorf("upsert must NOT run when existing config is malformed")
	}
}

// TestBranchesPut_OversizedBody_Returns413 confirms the 256 KiB
// MaxBytesReader gate trips on a payload past the cap.
func TestBranchesPut_OversizedBody_Returns413(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: db.ConnectionListRow{ID: 1, Config: map[string]any{"type": "g"}},
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	huge := strings.Repeat("a", 512*1024)
	body := `{"mode":"patterns","branches":["` + huge + `"]}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 1, body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", rec.Code)
	}
}

// TestBranchesPut_SyncBackendError_Returns502 confirms a hard
// syncer failure after the DB write surfaces as 502 (not 500).
func TestBranchesPut_SyncBackendError_Returns502(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: db.ConnectionListRow{ID: 42, Config: map[string]any{"type": "github"}},
		},
	}
	syncer := &recordingSyncer{err: errors.New("simulated indexer outage")}
	srv := newBranchesPutServer(spy, syncer, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 42, `{"mode":"all"}`))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated indexer outage") {
		t.Errorf("raw syncer error leaked: %s", rec.Body.String())
	}
}

// TestBranchesPut_NonIntegerID_Returns400 mirrors the GET handler
// table including int32 overflow + underflow boundaries.
func TestBranchesPut_NonIntegerID_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	secret, _ := ownerKey(t)
	cases := []string{"abc", "1.5", "2147483648", "-2147483649"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPut, "/api/connections/"+c+"/branches", strings.NewReader(`{"mode":"default"}`))
			req.Header.Set("X-Api-Key", "cik_"+secret)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("input %q: status %d, want 400", c, rec.Code)
			}
		})
	}
}

// TestBranchesPut_HappyResponseShape_LocksByteEquality byte-locks
// the full response so a future projection drift fails immediately.
func TestBranchesPut_HappyResponseShape_LocksByteEquality(t *testing.T) {
	_, hash := ownerKey(t)
	updated := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	existingCfg := mustConfig(t, `{"type":"github"}`)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
				upsertRow: db.ConnectionListRow{
					ID:             42,
					Name:           "gh-prod",
					ConnectionType: "github",
					Config: map[string]any{
						"type":      "github",
						"revisions": map[string]any{"branches": []any{"*"}},
					},
					UpdatedAt: updated,
				},
			},
			existingRow: db.ConnectionListRow{
				ID: 42, Name: "gh-prod", ConnectionType: "github", Config: existingCfg,
			},
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 42, `{"mode":"all","sync":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	want := `{"connectionId":42,"connectionName":"gh-prod","connectionType":"github","syncedAt":null,"updatedAt":"2025-06-01T12:00:00.000Z","branchPolicy":{"mode":"all","branches":["*"],"defaultBranchAlwaysIncluded":false,"maxIndexedRevisions":64}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
}

// TestBranchesPut_DBError_Returns500WithoutLeak.
func TestBranchesPut_DBError_Returns500WithoutLeak(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesPutSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingErr: errors.New("simulated db outage"),
		},
	}
	srv := newBranchesPutServer(spy, &recordingSyncer{}, &recordingEmitter{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesPutRequest(t, 1, `{"mode":"default"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated db outage") {
		t.Errorf("raw error leaked: %s", rec.Body.String())
	}
}
