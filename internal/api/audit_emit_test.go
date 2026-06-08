package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"codeintel/pkg/audit"
	"codeintel/internal/auth"
)

// recordingEmitter captures every Emit call so the tests can
// assert the handlers wired the audit pipeline correctly.
type recordingEmitter struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (r *recordingEmitter) Emit(_ context.Context, ev audit.Event) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return r.err
}

// TestConnectionUpsertEmitsAudit_OnSuccess locks the POST handler's
// audit-emit contract: a successful create fires a
// "connection.upserted" event with the right actor / target /
// org / metadata fields.
func TestConnectionUpsertEmitsAudit_OnSuccess(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	emitter := &recordingEmitter{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := NewServer(Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:          spy,
		EncryptionKey:    "0123456789abcdef0123456789abcdef",
		ConnectionSyncer: syncer,
		AuditEmitter:     emitter,
	})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postBody(t, `{"name":"gh-prod","config":{"type":"github"}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.Action != "connection.upserted" {
		t.Errorf("Action: got %q, want connection.upserted", ev.Action)
	}
	if ev.ActorType != audit.ActorApiKey {
		t.Errorf("ActorType: got %q, want api_key", ev.ActorType)
	}
	if ev.ActorID != "u-1" {
		t.Errorf("ActorID: got %q, want u-1", ev.ActorID)
	}
	if ev.OrgID != 7 {
		t.Errorf("OrgID: got %d, want 7", ev.OrgID)
	}
	if ev.TargetType != audit.TargetConnection {
		t.Errorf("TargetType: got %q, want connection", ev.TargetType)
	}
	if ev.TargetID == "" {
		t.Errorf("TargetID empty")
	}
	if ev.Time.IsZero() {
		t.Errorf("Time was not stamped")
	}
}

// TestConnectionDeleteEmitsAudit_OnSuccess locks the DELETE
// handler's audit-emit contract.
func TestConnectionDeleteEmitsAudit_OnSuccess(t *testing.T) {
	_, hash := ownerKey(t)
	emitter := &recordingEmitter{}
	spy := &deleteConnSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		AuditEmitter:  emitter,
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil)
	secret, _ := ownerKey(t)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(emitter.events) != 1 || emitter.events[0].Action != "connection.deleted" {
		t.Fatalf("expected one connection.deleted event, got %+v", emitter.events)
	}
	if emitter.events[0].TargetID != "42" {
		t.Errorf("TargetID: got %q, want 42", emitter.events[0].TargetID)
	}
}

// TestConnectionDeleteEmitsNoAudit_OnFailure confirms a 404 does
// NOT fire an audit event — only successful mutations produce
// compliance records.
func TestConnectionDeleteEmitsNoAudit_OnFailure(t *testing.T) {
	_, hash := ownerKey(t)
	emitter := &recordingEmitter{}
	spy := &deleteConnSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		deleteErr:       errors.New("simulated outage"),
	}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		AuditEmitter:  emitter,
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil)
	secret, _ := ownerKey(t)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if len(emitter.events) != 0 {
		t.Errorf("audit must NOT fire on failure; got %d events", len(emitter.events))
	}
}

// TestAuditEmitFailure_DoesNotFailRequest locks the rule that an
// audit-backend outage NEVER fails a successful business operation
// — the handler logs the audit error and continues.
func TestAuditEmitFailure_DoesNotFailRequest(t *testing.T) {
	_, hash := ownerKey(t)
	emitter := &recordingEmitter{err: errors.New("simulated audit backend outage")}
	spy := &deleteConnSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		AuditEmitter:  emitter,
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil)
	secret, _ := ownerKey(t)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (audit failure must NOT bubble up)", rec.Code)
	}
	if got := rec.Body.String(); got != `{"success":true}` {
		t.Errorf("body: got %q, want {\"success\":true}", got)
	}
}

// TestAuditDefault_NoEmitterConfigured_NoCrash confirms the
// nil-safe fallback: NoopEmitter is used and the request succeeds.
func TestAuditDefault_NoEmitterConfigured_NoCrash(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &deleteConnSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		// AuditEmitter: nil
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil)
	secret, _ := ownerKey(t)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
}

// TestConnectionPatchEmitsAudit_OnSuccess locks the PATCH
// handler's audit-emit contract + the metadata shape: the
// fields-patched map and the persisted new name must land on
// the event so downstream consumers can rebuild the change set.
func TestConnectionPatchEmitsAudit_OnSuccess(t *testing.T) {
	_, hash := ownerKey(t)
	emitter := &recordingEmitter{}
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingRow: existingRow(42),
	}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		AuditEmitter:  emitter,
	})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"gh-staging"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 patched audit event, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.Action != "connection.patched" {
		t.Errorf("Action: got %q, want connection.patched", ev.Action)
	}
	if ev.OrgID != 7 {
		t.Errorf("OrgID: got %d, want 7", ev.OrgID)
	}
	if ev.TargetID == "" {
		t.Errorf("TargetID empty; want the persisted row id")
	}
	// Metadata shape: fields={name:true}, newName=<persisted>
	fields, _ := ev.Metadata["fields"].(map[string]any)
	if fields == nil {
		t.Fatalf("metadata.fields missing or not a map: %+v", ev.Metadata)
	}
	if got, _ := fields["name"].(bool); !got {
		t.Errorf("metadata.fields.name: got %v, want true", fields["name"])
	}
	if _, hasConfig := fields["config"]; hasConfig {
		t.Errorf("metadata.fields should NOT include config when not patched: %+v", fields)
	}
	if got, _ := ev.Metadata["newName"].(string); got == "" {
		t.Errorf("metadata.newName missing")
	}
}

// TestConnectionPatchEmitsNoAudit_OnFailure confirms PATCH failures
// do NOT emit. Symmetric with the DELETE test.
func TestConnectionPatchEmitsNoAudit_OnFailure(t *testing.T) {
	_, hash := ownerKey(t)
	emitter := &recordingEmitter{}
	spy := &patchConnSpy{
		upsertConnectionSpy: upsertConnectionSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		},
		existingErr: errors.New("simulated db outage"),
	}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		AuditEmitter:  emitter,
	})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, patchBody(t, 42, `{"name":"x"}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if len(emitter.events) != 0 {
		t.Errorf("PATCH audit must NOT fire on failure; got %d events", len(emitter.events))
	}
}

// TestConnectionSync_DoesNotEmitAudit locks parity with legacy:
// packages/web/src/app/api/(server)/connections/[id]/sync/route.ts
// does NOT call audit.createAudit. Inverted from the prior
// version of this test, which asserted an emit that legacy
// does not produce (P.2 in docs/codeintel-parity-gaps.md).
func TestConnectionSync_DoesNotEmitAudit(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{result: SyncResult{JobID: "job-99"}}
	emitter := &recordingEmitter{}
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: existingRow(42),
		},
	}
	srv := NewServer(Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:          spy,
		EncryptionKey:    "0123456789abcdef0123456789abcdef",
		ConnectionSyncer: syncer,
		AuditEmitter:     emitter,
	})

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, syncRequest(t, 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(emitter.events) != 0 {
		t.Errorf("legacy emits nothing on sync; got %d audit events", len(emitter.events))
	}
}

// TestAuditEmit_AutoStampsTimeAndRequestID confirms emitAudit
// auto-populates the Time and RequestID fields when the caller
// leaves them zero. Without this guard, a forgetful caller would
// emit an undatable event.
func TestAuditEmit_AutoStampsTimeAndRequestID(t *testing.T) {
	_, hash := ownerKey(t)
	emitter := &recordingEmitter{}
	spy := &deleteConnSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		AuditEmitter:  emitter,
	})

	req := httptest.NewRequest(http.MethodDelete, "/api/connections/42", nil)
	secret, _ := ownerKey(t)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("X-Request-Id", "trace-abc")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.Time.IsZero() {
		t.Errorf("Time was not auto-stamped")
	}
	if ev.RequestID != "trace-abc" {
		t.Errorf("RequestID: got %q, want trace-abc (echoed from header)", ev.RequestID)
	}
}

// TestAuditActor_PicksApiKeySourceCorrectly is a small unit check
// for the auditActor helper used by every emit call site.
func TestAuditActor_PicksApiKeySourceCorrectly(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		userID  string
		wantTyp audit.ActorType
		wantID  string
	}{
		{"api_key_user", "api_key", "u-1", audit.ActorApiKey, "u-1"},
		{"anonymous", "anonymous", "", audit.ActorSystem, ""},
		{"session", "session", "u-2", audit.ActorUser, "u-2"},
		{"unknown_source_defaults_to_user", "", "u-3", audit.ActorUser, "u-3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, typ := auditActor(auth.AuthContext{UserID: c.userID, AuthSource: c.source})
			if typ != c.wantTyp {
				t.Errorf("ActorType: got %q, want %q", typ, c.wantTyp)
			}
			if id != c.wantID {
				t.Errorf("ActorID: got %q, want %q", id, c.wantID)
			}
		})
	}
}

// keep imports referenced even if a future trim drops a handler.
var _ = strings.Contains
