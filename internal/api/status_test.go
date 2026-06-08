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
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

type statusSpy struct {
	fakeAuthQuerier
	rollup            db.OrgStatusRollup
	rollupErr         error
	syncFailures      []db.RecentFailedConnectionSyncJobRow
	syncFailuresErr   error
	indexFailures     []db.RecentFailedRepoIndexingJobRow
	indexFailuresErr  error

	rollupOrg         int32
	rollupCalls       int
	syncFailuresOrg   int32
	syncFailuresLim   int32
	syncFailuresCall  int
	indexFailuresOrg  int32
	indexFailuresLim  int32
	indexFailuresCall int
}

func (s *statusSpy) GetOrgStatusRollup(ctx context.Context, orgID int32) (db.OrgStatusRollup, error) {
	s.rollupOrg = orgID
	s.rollupCalls++
	return s.rollup, s.rollupErr
}

func (s *statusSpy) ListRecentFailedConnectionSyncJobs(ctx context.Context, orgID, limit int32) ([]db.RecentFailedConnectionSyncJobRow, error) {
	s.syncFailuresOrg = orgID
	s.syncFailuresLim = limit
	s.syncFailuresCall++
	return s.syncFailures, s.syncFailuresErr
}

func (s *statusSpy) ListRecentFailedRepoIndexingJobs(ctx context.Context, orgID, limit int32) ([]db.RecentFailedRepoIndexingJobRow, error) {
	s.indexFailuresOrg = orgID
	s.indexFailuresLim = limit
	s.indexFailuresCall++
	return s.indexFailures, s.indexFailuresErr
}

func newStatusServer(spy *statusSpy) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
	})
}

const fullStatusBodyEmptyFailures = `{"org":{"id":7,"name":"Atom Org A","domain":"orga"},"repos":{"total":42,"indexed":30,"indexingJobs":{"pending":2,"inProgress":3,"failed":5,"recentFailures":[]}},"connections":{"total":3,"synced":2,"syncJobs":{"pending":1,"inProgress":1,"failed":4,"recentFailures":[]}},"zoekt":{"mode":"single","orgIndex":null,"shardGroups":[],"endpoints":[]}}`

// TestStatus_OwnerHappy_FullShape locks the complete wire response
// including both empty recentFailures arrays and all scalar
// counts for repos / connections / sync-jobs / index-jobs.
func TestStatus_OwnerHappy_FullShape(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &statusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		rollup: db.OrgStatusRollup{
			RepoCount:               42,
			IndexedRepoCount:        30,
			ConnectionCount:         3,
			SyncedConnectionCount:   2,
			PendingSyncJobs:         1,
			InProgressSyncJobs:      1,
			FailedSyncJobs:          4,
			PendingRepoIndexJobs:    2,
			InProgressRepoIndexJobs: 3,
			FailedRepoIndexJobs:     5,
		},
	}
	srv := newStatusServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != fullStatusBodyEmptyFailures {
		t.Fatalf("body equality:\n  got  %s\n  want %s", got, fullStatusBodyEmptyFailures)
	}
	if spy.rollupOrg != 7 || spy.syncFailuresOrg != 7 || spy.indexFailuresOrg != 7 {
		t.Errorf("orgID args: rollup=%d, syncFailures=%d, indexFailures=%d (want 7,7,7)", spy.rollupOrg, spy.syncFailuresOrg, spy.indexFailuresOrg)
	}
	if spy.syncFailuresLim != recentFailureLimit || spy.indexFailuresLim != recentFailureLimit {
		t.Errorf("limit args: sync=%d, index=%d (want %d for both)", spy.syncFailuresLim, spy.indexFailuresLim, recentFailureLimit)
	}
	if spy.rollupCalls != 1 || spy.syncFailuresCall != 1 || spy.indexFailuresCall != 1 {
		t.Errorf("dispatch counts: rollup=%d, sync=%d, index=%d (want 1 each)", spy.rollupCalls, spy.syncFailuresCall, spy.indexFailuresCall)
	}
}

// TestStatus_FailuresPopulated_FullShape locks both populated
// recentFailures projections in the same request: sync-job
// failures (with one null + one non-null errorMessage) and
// repo-index-job failures (with one null + one non-null error).
func TestStatus_FailuresPopulated_FullShape(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	syncErr := "boom"
	indexErr := "kaboom"
	syncCreated := time.Date(2025, 2, 19, 23, 0, 0, 0, time.UTC)
	syncUpdated := time.Date(2025, 2, 20, 9, 30, 0, 500_000_000, time.UTC)
	indexCreated := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	indexUpdated := time.Date(2025, 3, 2, 11, 0, 0, 250_000_000, time.UTC)
	spy := &statusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		rollup:          db.OrgStatusRollup{},
		syncFailures: []db.RecentFailedConnectionSyncJobRow{
			{
				ID: "sync-A", ErrorMessage: &syncErr,
				CreatedAt: syncCreated, UpdatedAt: syncUpdated,
				ConnectionID: 42, ConnectionName: "gh-prod", ConnectionType: "github",
			},
		},
		indexFailures: []db.RecentFailedRepoIndexingJobRow{
			{
				ID: "idx-A", ErrorMessage: &indexErr,
				CreatedAt: indexCreated, UpdatedAt: indexUpdated,
				RepoID: 9, RepoName: "frontend",
			},
			{
				ID: "idx-B", ErrorMessage: nil,
				CreatedAt: indexCreated.Add(-time.Hour), UpdatedAt: indexCreated.Add(-time.Hour),
				RepoID: 10, RepoName: "backend",
			},
		},
	}
	srv := newStatusServer(spy)

	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	want := `{"org":{"id":7,"name":"Atom Org A","domain":"orga"},"repos":{"total":0,"indexed":0,"indexingJobs":{"pending":0,"inProgress":0,"failed":0,"recentFailures":[{"id":"idx-A","repo":{"id":9,"name":"frontend"},"errorMessage":"kaboom","createdAt":"2025-03-01T10:00:00.000Z","updatedAt":"2025-03-02T11:00:00.250Z"},{"id":"idx-B","repo":{"id":10,"name":"backend"},"errorMessage":null,"createdAt":"2025-03-01T09:00:00.000Z","updatedAt":"2025-03-01T09:00:00.000Z"}]}},"connections":{"total":0,"synced":0,"syncJobs":{"pending":0,"inProgress":0,"failed":0,"recentFailures":[{"id":"sync-A","connection":{"id":42,"name":"gh-prod","connectionType":"github"},"errorMessage":"boom","createdAt":"2025-02-19T23:00:00.000Z","updatedAt":"2025-02-20T09:30:00.500Z"}]}},"zoekt":{"mode":"single","orgIndex":null,"shardGroups":[],"endpoints":[]}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body equality:\n  got  %s\n  want %s", got, want)
	}
	if spy.syncFailuresLim != recentFailureLimit || spy.indexFailuresLim != recentFailureLimit {
		t.Errorf("limit args: sync=%d, index=%d (want %d for both)", spy.syncFailuresLim, spy.indexFailuresLim, recentFailureLimit)
	}
}

// TestStatus_MemberRoleAdmitted confirms non-OWNER members get the
// same body as OWNERs.
func TestStatus_MemberRoleAdmitted(t *testing.T) {
	const secret = "membersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &statusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup},
		rollup: db.OrgStatusRollup{
			RepoCount:               42,
			IndexedRepoCount:        30,
			ConnectionCount:         3,
			SyncedConnectionCount:   2,
			PendingSyncJobs:         1,
			InProgressSyncJobs:      1,
			FailedSyncJobs:          4,
			PendingRepoIndexJobs:    2,
			InProgressRepoIndexJobs: 3,
			FailedRepoIndexJobs:     5,
		},
	}
	srv := newStatusServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != fullStatusBodyEmptyFailures {
		t.Fatalf("MEMBER body must equal OWNER body:\n  got  %s\n  want %s", got, fullStatusBodyEmptyFailures)
	}
}

// TestStatus_GuestRoleRejectedAs401 confirms GUEST is rejected and
// no DB query is invoked.
func TestStatus_GuestRoleRejectedAs401(t *testing.T) {
	const secret = "guestsec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	lookup := validOwnerLookup(hash)
	lookup.Role = "GUEST"
	spy := &statusSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup}}
	srv := newStatusServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	want := `{"statusCode":401,"errorCode":"NOT_AUTHENTICATED","message":"Not authenticated"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
	if spy.rollupCalls != 0 || spy.syncFailuresCall != 0 || spy.indexFailuresCall != 0 {
		t.Errorf("DB must not be called for GUEST: rollup=%d, sync=%d, index=%d", spy.rollupCalls, spy.syncFailuresCall, spy.indexFailuresCall)
	}
}

// TestStatus_NoCredentials_Returns401.
func TestStatus_NoCredentials_Returns401(t *testing.T) {
	spy := &statusSpy{}
	srv := newStatusServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	want := `{"statusCode":401,"errorCode":"NOT_AUTHENTICATED","message":"Not authenticated"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
	if spy.rollupCalls != 0 || spy.syncFailuresCall != 0 || spy.indexFailuresCall != 0 {
		t.Errorf("DB must not be called pre-auth: rollup=%d, sync=%d, index=%d", spy.rollupCalls, spy.syncFailuresCall, spy.indexFailuresCall)
	}
}

// TestStatus_AuthResolutionError_Returns500.
func TestStatus_AuthResolutionError_Returns500(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &statusSpy{
		fakeAuthQuerier: fakeAuthQuerier{
			lookupResult: validOwnerLookup(hash),
			lookupErr:    errors.New("simulated auth DB outage"),
		},
	}
	srv := newStatusServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	want := `{"statusCode":500,"errorCode":"UNEXPECTED_ERROR","message":"An unexpected error occurred"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestStatus_RollupDBError_Returns500.
func TestStatus_RollupDBError_Returns500(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &statusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		rollupErr:       errors.New("simulated rollup outage"),
	}
	srv := newStatusServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	want := `{"statusCode":500,"errorCode":"UNEXPECTED_ERROR","message":"An unexpected error occurred"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestStatus_SyncFailuresDBError_Returns500.
func TestStatus_SyncFailuresDBError_Returns500(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &statusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		syncFailuresErr: errors.New("simulated sync-failures outage"),
	}
	srv := newStatusServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	want := `{"statusCode":500,"errorCode":"UNEXPECTED_ERROR","message":"An unexpected error occurred"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestStatus_IndexFailuresDBError_Returns500.
func TestStatus_IndexFailuresDBError_Returns500(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &statusSpy{
		fakeAuthQuerier:  fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		indexFailuresErr: errors.New("simulated index-failures outage"),
	}
	srv := newStatusServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	want := `{"statusCode":500,"errorCode":"UNEXPECTED_ERROR","message":"An unexpected error occurred"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestStatus_FailuresJSONEscape_FullShape locks JSON-escape
// correctness across every string field that flows from a DB row
// into the response: repo name, connection name, connectionType,
// and errorMessage all carry characters that require escaping
// (double-quote, backslash, newline, tab, unicode). A regression
// that bypasses encoding/json (e.g. fmt.Fprintf) surfaces here.
func TestStatus_FailuresJSONEscape_FullShape(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	weirdSyncErr := "boom \"with quotes\"\nand newline\tand \xe2\x9c\x93 unicode"
	weirdIndexErr := "kaboom \\backslash and \"quote\""
	syncCreated := time.Date(2025, 2, 19, 23, 0, 0, 0, time.UTC)
	syncUpdated := time.Date(2025, 2, 20, 9, 30, 0, 0, time.UTC)
	indexCreated := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)
	indexUpdated := time.Date(2025, 3, 2, 11, 0, 0, 0, time.UTC)
	spy := &statusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		rollup:          db.OrgStatusRollup{},
		syncFailures: []db.RecentFailedConnectionSyncJobRow{
			{
				ID:             "sync-X",
				ErrorMessage:   &weirdSyncErr,
				CreatedAt:      syncCreated,
				UpdatedAt:      syncUpdated,
				ConnectionID:   42,
				ConnectionName: "gh \"prod\"",
				ConnectionType: "github",
			},
		},
		indexFailures: []db.RecentFailedRepoIndexingJobRow{
			{
				ID:           "idx-X",
				ErrorMessage: &weirdIndexErr,
				CreatedAt:    indexCreated,
				UpdatedAt:    indexUpdated,
				RepoID:       9,
				RepoName:     "front\\end",
			},
		},
	}
	srv := newStatusServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	want := `{"org":{"id":7,"name":"Atom Org A","domain":"orga"},"repos":{"total":0,"indexed":0,"indexingJobs":{"pending":0,"inProgress":0,"failed":0,"recentFailures":[{"id":"idx-X","repo":{"id":9,"name":"front\\end"},"errorMessage":"kaboom \\backslash and \"quote\"","createdAt":"2025-03-01T10:00:00.000Z","updatedAt":"2025-03-02T11:00:00.000Z"}]}},"connections":{"total":0,"synced":0,"syncJobs":{"pending":0,"inProgress":0,"failed":0,"recentFailures":[{"id":"sync-X","connection":{"id":42,"name":"gh \"prod\"","connectionType":"github"},"errorMessage":"boom \"with quotes\"\nand newline\tand ✓ unicode","createdAt":"2025-02-19T23:00:00.000Z","updatedAt":"2025-02-20T09:30:00.000Z"}]}},"zoekt":{"mode":"single","orgIndex":null,"shardGroups":[],"endpoints":[]}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body equality:\n  got  %s\n  want %s", got, want)
	}
}

// TestStatus_ZoektFanoutMode_FullShape locks a complete byte-
// equal body for the fanout branch so the four-mode enum is
// covered at full-shape rigor (the "single" variant is locked by
// every other happy-path test). Future slices that add the
// `routed` and `org-directory` branches should clone this test.
func TestStatus_ZoektFanoutMode_FullShape(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &statusSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := NewServer(Config{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:            spy,
		EncryptionKey:      "0123456789abcdef0123456789abcdef",
		ZoektWebserverUrls: []string{"http://a", "http://b"},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	want := `{"org":{"id":7,"name":"Atom Org A","domain":"orga"},"repos":{"total":0,"indexed":0,"indexingJobs":{"pending":0,"inProgress":0,"failed":0,"recentFailures":[]}},"connections":{"total":0,"synced":0,"syncJobs":{"pending":0,"inProgress":0,"failed":0,"recentFailures":[]}},"zoekt":{"mode":"fanout","orgIndex":null,"shardGroups":[],"endpoints":[]}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestStatus_ZoektModeResolution locks the ternary that resolves
// the zoekt.mode wire string from the configured webserver-url
// count. With orgIndex absent and no shard groups (the current
// codeintel deployment shape) the mode collapses to "single" for
// 0 or 1 url, and "fanout" for 2+ urls.
func TestStatus_ZoektModeResolution(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	cases := []struct {
		name string
		urls []string
		want zoektMode
	}{
		{"no_urls_is_single", nil, zoektModeSingle},
		{"one_url_is_single", []string{"http://zoekt-a"}, zoektModeSingle},
		{"two_urls_is_fanout", []string{"http://a", "http://b"}, zoektModeFanout},
		{"three_urls_is_fanout", []string{"http://a", "http://b", "http://c"}, zoektModeFanout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy := &statusSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
			srv := NewServer(Config{
				Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
				Queries:            spy,
				EncryptionKey:      "0123456789abcdef0123456789abcdef",
				ZoektWebserverUrls: c.urls,
			})
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			req.Header.Set("X-Api-Key", "cik_"+secret)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", rec.Code)
			}
			expectedFragment := `"zoekt":{"mode":"` + string(c.want) + `","orgIndex":null,"shardGroups":[],"endpoints":[]}}`
			if got := rec.Body.String(); !strings.HasSuffix(got, expectedFragment) {
				t.Fatalf("expected suffix %s, got tail %s", expectedFragment, got[max(0, len(got)-len(expectedFragment)-10):])
			}
		})
	}
}

// TestStatus_ZeroCountsSerialiseCorrectly confirms zero values and
// both empty recentFailures arrays are emitted (not omitted).
func TestStatus_ZeroCountsSerialiseCorrectly(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &statusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		rollup:          db.OrgStatusRollup{},
	}
	srv := newStatusServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	want := `{"org":{"id":7,"name":"Atom Org A","domain":"orga"},"repos":{"total":0,"indexed":0,"indexingJobs":{"pending":0,"inProgress":0,"failed":0,"recentFailures":[]}},"connections":{"total":0,"synced":0,"syncJobs":{"pending":0,"inProgress":0,"failed":0,"recentFailures":[]}},"zoekt":{"mode":"single","orgIndex":null,"shardGroups":[],"endpoints":[]}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}
