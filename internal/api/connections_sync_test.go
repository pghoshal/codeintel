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
)

// syncConnSpy extends patchConnSpy with an overridable
// ConnectionExistsInOrg result so the sync handler's narrow
// existence check can be exercised without touching the wider
// GetOrgConnectionForUpdate machinery.
type syncConnSpy struct {
	patchConnSpy
	existsBool    bool
	existsErr     error
	existsOverride bool // when true, ConnectionExistsInOrg returns (existsBool, existsErr) instead of falling through to the existingRow fake
}

func (s *syncConnSpy) ConnectionExistsInOrg(ctx context.Context, orgID, connectionID int32) (bool, error) {
	if s.existsOverride {
		return s.existsBool, s.existsErr
	}
	if s.patchConnSpy.existingErr != nil {
		// Reuse the existingErr surface so existing tests that
		// configured patchConnSpy.existingErr continue to work
		// against this overlay.
		return false, s.patchConnSpy.existingErr
	}
	return s.patchConnSpy.existingRow.ID != 0, nil
}

func newSyncConnServer(spy *syncConnSpy, syncer ConnectionSyncer) *Server {
	return NewServer(Config{
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:          spy,
		EncryptionKey:    "0123456789abcdef0123456789abcdef",
		ConnectionSyncer: syncer,
	})
}

func syncRequest(t *testing.T, id int) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodPost, "/api/connections/"+itoa(id)+"/sync", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	return req
}

// TestSyncConnection_OwnerHappy_Returns200WithJobID locks the
// canonical success path: 200 + {"jobId":"<id>"} byte-equal, and
// the syncer was called with the right (orgId, connectionId).
func TestSyncConnection_OwnerHappy_Returns200WithJobID(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{result: SyncResult{JobID: "job-7"}}
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: existingRow(42),
		},
	}
	srv := newSyncConnServer(spy, syncer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, syncRequest(t, 42))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"jobId":"job-7"}` {
		t.Fatalf("body: got %q, want {\"jobId\":\"job-7\"}", got)
	}
	if len(syncer.calls) != 1 {
		t.Fatalf("syncer should fire once, got %d", len(syncer.calls))
	}
	if syncer.calls[0].OrgID != 7 || syncer.calls[0].ConnectionID != 42 {
		t.Errorf("syncer call args: got %+v, want (7, 42)", syncer.calls[0])
	}
}

// TestSyncConnection_NoConfiguredSyncer_ReturnsEmptyObject confirms
// the no-op syncer path produces `{}` (jobId omitted) so callers
// can detect "scheduled but no id" without ambiguity.
func TestSyncConnection_NoConfiguredSyncer_ReturnsEmptyObject(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: existingRow(42),
		},
	}
	srv := newSyncConnServer(spy, nil) // nil -> NoopConnectionSyncer fallback
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, syncRequest(t, 42))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != `{}` {
		t.Fatalf("body: got %q, want {}", got)
	}
}

// TestSyncConnection_AtCapacity_Returns429.
func TestSyncConnection_AtCapacity_Returns429(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{result: SyncResult{AlreadyAtCapacity: true}}
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: existingRow(42),
		},
	}
	srv := newSyncConnServer(spy, syncer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, syncRequest(t, 42))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", rec.Code)
	}
	want := `{"statusCode":429,"errorCode":"CONNECTION_SYNC_ALREADY_SCHEDULED","message":"Connection sync was not scheduled because the tenant is at active sync capacity. Retry after the current sync jobs finish."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestSyncConnection_SyncBackendError_Returns502.
func TestSyncConnection_SyncBackendError_Returns502(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{err: errors.New("simulated indexer outage")}
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
			existingRow: existingRow(42),
		},
	}
	srv := newSyncConnServer(spy, syncer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, syncRequest(t, 42))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated indexer outage") {
		t.Errorf("raw syncer error leaked: %s", rec.Body.String())
	}
}

// TestSyncConnection_NotFound_Returns404 covers the cross-tenant
// shielding: an unknown id (or an id owned by another org) returns
// 404. The syncer must NOT be called.
func TestSyncConnection_NotFound_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
		},
		existsOverride: true,
		existsBool:     false,
	}
	srv := newSyncConnServer(spy, syncer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, syncRequest(t, 999))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	want := `{"statusCode":404,"errorCode":"NOT_FOUND","message":"Connection not found."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
	if len(syncer.calls) != 0 {
		t.Fatalf("syncer must NOT fire when the connection is not found in the org")
	}
}

// TestSyncConnection_NonIntegerID_Returns400 covers the path
// segment validation across multiple malformed inputs including
// int32-overflow values.
func TestSyncConnection_NonIntegerID_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
		},
	}
	srv := newSyncConnServer(spy, syncer)
	secret, _ := ownerKey(t)
	cases := []string{
		"abc",
		"1.5",
		"-not-int",
		"2147483648",                  // int32 overflow (max+1)
		"-2147483649",                 // int32 underflow (min-1)
		"9999999999999999999",         // way past int64 range
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			syncer.calls = nil
			req := httptest.NewRequest(http.MethodPost, "/api/connections/"+c+"/sync", nil)
			req.Header.Set("X-Api-Key", "cik_"+secret)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("input %q: status %d, want 400", c, rec.Code)
			}
			if len(syncer.calls) != 0 {
				t.Errorf("input %q: syncer must NOT fire on invalid id", c)
			}
		})
	}
}

// TestSyncConnection_ZeroOrNegativeID_Returns404 covers the
// boundary where ParseInt succeeds but the DB layer's
// "id <= 0" guard short-circuits to "not found" without a
// round-trip. The handler must surface 404 (no row could ever
// match), the syncer must NOT fire.
func TestSyncConnection_ZeroOrNegativeID_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	syncer := &recordingSyncer{}
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
		},
		// Mimic the real ConnectionExistsInOrg behaviour for
		// non-positive ids — short-circuit to (false, nil).
		existsOverride: true,
		existsBool:     false,
	}
	srv := newSyncConnServer(spy, syncer)
	secret, _ := ownerKey(t)
	for _, id := range []string{"0", "-1"} {
		t.Run(id, func(t *testing.T) {
			syncer.calls = nil
			req := httptest.NewRequest(http.MethodPost, "/api/connections/"+id+"/sync", nil)
			req.Header.Set("X-Api-Key", "cik_"+secret)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Errorf("id %q: status %d, want 404", id, rec.Code)
			}
			if len(syncer.calls) != 0 {
				t.Errorf("id %q: syncer must NOT fire", id)
			}
		})
	}
}

// TestSyncConnection_MemberRole_Returns403.
func TestSyncConnection_MemberRole_Returns403(t *testing.T) {
	_, hash := ownerKey(t)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup},
			},
		},
	}
	srv := newSyncConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, syncRequest(t, 42))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can sync connections."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestSyncConnection_NoCredentials_Returns401.
func TestSyncConnection_NoCredentials_Returns401(t *testing.T) {
	spy := &syncConnSpy{}
	srv := newSyncConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/connections/42/sync", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestSyncConnection_DBError_Returns500WithoutLeak surfaces an
// outage during the cross-tenant existence check.
func TestSyncConnection_DBError_Returns500WithoutLeak(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &syncConnSpy{
		patchConnSpy: patchConnSpy{
			upsertConnectionSpy: upsertConnectionSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			},
		},
		existsOverride: true,
		existsErr:      errors.New("simulated db outage"),
	}
	srv := newSyncConnServer(spy, &recordingSyncer{})
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, syncRequest(t, 42))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated db outage") {
		t.Errorf("raw db error leaked: %s", rec.Body.String())
	}
}

// Ensure unused imports for the spy package compile when no test
// uses them directly (db / context references in nested types).
var _ context.Context
var _ db.AuthLookup
