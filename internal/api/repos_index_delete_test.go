package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// memberKey + validMemberLookup mirror ownerKey + validOwnerLookup
// but resolve to OrgRole.MEMBER so the OWNER-only gate on this
// route can be exercised.
func memberKey(t *testing.T) (secret, hash string) {
	t.Helper()
	secret = "membersec"
	hash = auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	return
}

func validMemberLookup(hash string) db.AuthLookup {
	row := validOwnerLookup(hash)
	row.Role = "MEMBER"
	return row
}

// recordingRepoIndexer captures every Schedule call so the test
// can assert the route invoked the indexer with the expected
// args. Same shape as recordingSyncer.
type recordingRepoIndexer struct {
	calls  []RepoIndexRequest
	result RepoIndexResult
	err    error
}

func (r *recordingRepoIndexer) Schedule(_ context.Context, req RepoIndexRequest) (RepoIndexResult, error) {
	r.calls = append(r.calls, req)
	return r.result, r.err
}

func newDeleteRepoIndexServer(spy *upsertConnectionSpy, indexer RepoIndexer) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		RepoIndexer:   indexer,
	})
}

func deleteRepoIndexRequest(t *testing.T, id int) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/repos/"+itoa(id)+"/index", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	return req
}

func postRepoIndexRequest(t *testing.T, id int) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodPost, "/api/repos/"+itoa(id)+"/index", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	return req
}

// TestDeleteRepoIndex_OwnerHappy_Returns200WithJobID asserts the
// canonical success: 200 + {"jobId":"<id>"} and the indexer was
// invoked with (orgId, repoId, REMOVE_INDEX).
func TestDeleteRepoIndex_OwnerHappy_Returns200WithJobID(t *testing.T) {
	_, hash := ownerKey(t)
	indexer := &recordingRepoIndexer{result: RepoIndexResult{JobID: "rix-7"}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, deleteRepoIndexRequest(t, 42))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"jobId":"rix-7"}` {
		t.Fatalf("body: got %q, want {\"jobId\":\"rix-7\"}", got)
	}
	if len(indexer.calls) != 1 {
		t.Fatalf("indexer should fire once, got %d", len(indexer.calls))
	}
	call := indexer.calls[0]
	if call.OrgID != 7 || call.RepoID != 42 || call.Kind != RepoIndexJobKindRemoveIndex {
		t.Errorf("indexer call args: got %+v, want (7, 42, REMOVE_INDEX)", call)
	}
}

func TestPostRepoIndex_OwnerHappy_Returns200WithJobID(t *testing.T) {
	_, hash := ownerKey(t)
	indexer := &recordingRepoIndexer{result: RepoIndexResult{JobID: "rix-9"}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postRepoIndexRequest(t, 42))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"jobId":"rix-9"}` {
		t.Fatalf("body: got %q, want {\"jobId\":\"rix-9\"}", got)
	}
	if len(indexer.calls) != 1 {
		t.Fatalf("indexer should fire once, got %d", len(indexer.calls))
	}
	call := indexer.calls[0]
	if call.OrgID != 7 || call.RepoID != 42 || call.Kind != RepoIndexJobKindIndex {
		t.Errorf("indexer call args: got %+v, want (7, 42, INDEX)", call)
	}
}

func TestPostRepoIndex_BodyBranchPassesRefToScheduler(t *testing.T) {
	_, hash := ownerKey(t)
	indexer := &recordingRepoIndexer{result: RepoIndexResult{JobID: "rix-branch"}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodPost, "/api/repos/42/index", strings.NewReader(`{"branch":"release-a"}`))
	req.Header.Set("X-Api-Key", "cik_"+secret)
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if len(indexer.calls) != 1 || indexer.calls[0].Ref != "release-a" {
		t.Fatalf("scheduler ref mismatch: %+v", indexer.calls)
	}
}

func TestDeleteRepoIndex_QueryRefPassesRefToScheduler(t *testing.T) {
	_, hash := ownerKey(t)
	indexer := &recordingRepoIndexer{result: RepoIndexResult{JobID: "rix-delete-branch"}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodDelete, "/api/repos/42/index?ref=refs/heads/release-a", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if len(indexer.calls) != 1 || indexer.calls[0].Ref != "refs/heads/release-a" {
		t.Fatalf("scheduler ref mismatch: %+v", indexer.calls)
	}
}

// TestDeleteRepoIndex_NoConfiguredIndexer_Returns503 asserts the
// route fails closed when no durable backend scheduler is wired. Atom
// must never receive 2xx for a remove-index action that did not create
// a pollable job.
func TestDeleteRepoIndex_NoConfiguredIndexer_Returns503(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, nil) // nil -> NoopRepoIndexer
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, deleteRepoIndexRequest(t, 42))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); !contains(got, "REPO_INDEXER_NOT_CONFIGURED") {
		t.Fatalf("body: got %q, want REPO_INDEXER_NOT_CONFIGURED", got)
	}
}

func TestDeleteRepoIndex_EmptyJobID_Returns503(t *testing.T) {
	_, hash := ownerKey(t)
	indexer := &recordingRepoIndexer{result: RepoIndexResult{}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, deleteRepoIndexRequest(t, 42))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); !contains(got, "REPO_INDEXER_EMPTY_JOB_ID") || !contains(got, "durable job id") {
		t.Fatalf("body: got %q, want empty job id error", got)
	}
}

func TestPostRepoIndex_NoConfiguredIndexer_Returns503(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, postRepoIndexRequest(t, 42))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); !contains(got, "REPO_INDEXER_NOT_CONFIGURED") {
		t.Fatalf("body: got %q, want REPO_INDEXER_NOT_CONFIGURED", got)
	}
}

// TestDeleteRepoIndex_NonOwner_Returns403 asserts the legacy
// OWNER-only gate is preserved (parity with the legacy DELETE
// /api/repos/[id]/index route).
func TestDeleteRepoIndex_NonOwner_Returns403(t *testing.T) {
	_, hash := memberKey(t)
	indexer := &recordingRepoIndexer{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validMemberLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/repos/42/index", nil)
	req.Header.Set("X-Api-Key", "cik_membersec")
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	if len(indexer.calls) != 0 {
		t.Errorf("indexer should not fire on 403, got %d calls", len(indexer.calls))
	}
}

// TestDeleteRepoIndex_NonIntegerID_Returns400 — :id="abc" fails
// the strconv.ParseInt before the indexer is even consulted.
func TestDeleteRepoIndex_NonIntegerID_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	indexer := &recordingRepoIndexer{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/repos/abc/index", nil)
	req.Header.Set("X-Api-Key", "cik_ownersec")
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if len(indexer.calls) != 0 {
		t.Errorf("indexer should not fire on 400, got %d calls", len(indexer.calls))
	}
}

// TestDeleteRepoIndex_RepoNotFound_Returns404 — indexer returns
// ErrRepoNotFound (the repo isn't in the org); route surfaces as
// 404 NOT_FOUND to match the legacy contract.
func TestDeleteRepoIndex_RepoNotFound_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	indexer := &recordingRepoIndexer{err: ErrRepoNotFound}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, deleteRepoIndexRequest(t, 42))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d (body=%q), want 404", rec.Code, rec.Body.String())
	}
}

// TestDeleteRepoIndex_AtCapacity_Returns429 — the future
// tenant-capacity check (not yet implemented in
// AsynqRepoIndexer) surfaces as 429 when set.
func TestDeleteRepoIndex_AtCapacity_Returns429(t *testing.T) {
	_, hash := ownerKey(t)
	indexer := &recordingRepoIndexer{result: RepoIndexResult{AlreadyAtCapacity: true}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newDeleteRepoIndexServer(spy, indexer)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, deleteRepoIndexRequest(t, 42))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want 429", rec.Code)
	}
}

// TestRepoIndexJobKind_Valid covers the three legacy enum values
// + an explicit rejection case for a malformed string.
func TestRepoIndexJobKind_Valid(t *testing.T) {
	for _, k := range []RepoIndexJobKind{
		RepoIndexJobKindIndex,
		RepoIndexJobKindCleanup,
		RepoIndexJobKindRemoveIndex,
	} {
		if !k.Valid() {
			t.Errorf("Valid(%q) = false, want true", k)
		}
	}
	if RepoIndexJobKind("nonsense").Valid() {
		t.Errorf("Valid(nonsense) = true, want false")
	}
}
