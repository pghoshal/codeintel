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

	"codeintel/internal/db"
)

// connStatusSpy plugs in the meta + count + sync-job fixtures and
// captures the orgID / connectionID / limit each call receives so
// tests can lock the handler→DB argument plumbing.
type connStatusSpy struct {
	fakeAuthQuerier
	meta      db.ConnectionMetaRow
	metaErr   error
	repoCount int32
	repoErr   error
	jobs      []db.ConnectionSyncJobRow
	jobsErr   error

	// argument captures
	metaOrg   int32
	metaConn  int32
	countOrg  int32
	countConn int32
	jobsOrg   int32
	jobsConn  int32
	jobsLimit int32
}

func (c *connStatusSpy) GetOrgConnectionMeta(ctx context.Context, orgID, connectionID int32) (db.ConnectionMetaRow, error) {
	c.metaOrg, c.metaConn = orgID, connectionID
	if c.metaErr != nil {
		return db.ConnectionMetaRow{}, c.metaErr
	}
	return c.meta, nil
}

func (c *connStatusSpy) CountConnectionRepos(ctx context.Context, orgID, connectionID int32) (int32, error) {
	c.countOrg, c.countConn = orgID, connectionID
	if c.repoErr != nil {
		return 0, c.repoErr
	}
	return c.repoCount, nil
}

func (c *connStatusSpy) ListConnectionSyncJobs(ctx context.Context, orgID, connectionID, limit int32) ([]db.ConnectionSyncJobRow, error) {
	c.jobsOrg, c.jobsConn, c.jobsLimit = orgID, connectionID, limit
	if c.jobsErr != nil {
		return nil, c.jobsErr
	}
	return c.jobs, nil
}

func newStatusConnServer(spy *connStatusSpy) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
	})
}

func statusReq(t *testing.T, id int) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodGet, "/api/connections/"+itoa(id)+"/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	return req
}

// TestConnectionStatus_OwnerHappy_FullShape locks the complete wire
// response: every top-level field, the sync-job projection in
// documented wire order (id/status/createdAt/updatedAt/completedAt/
// warningMessages/errorMessage), the derived branchPolicy, and the
// latestJob projection. Uses fixed UTC timestamps so the equality
// assertion is stable.
func TestConnectionStatus_OwnerHappy_FullShape(t *testing.T) {
	_, hash := ownerKey(t)
	synced := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	updated := time.Date(2025, 2, 20, 8, 15, 0, 500_000_000, time.UTC)
	created := time.Date(2025, 2, 19, 23, 0, 0, 0, time.UTC)
	jobUpdated := time.Date(2025, 2, 19, 23, 5, 0, 0, time.UTC)
	completed := time.Date(2025, 2, 19, 23, 6, 0, 0, time.UTC)
	errMsg := "boom"
	spy := &connStatusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		meta: db.ConnectionMetaRow{
			ID:             42,
			Name:           "gh-prod",
			ConnectionType: "github",
			Config: map[string]any{
				"revisions": map[string]any{
					"branches": []any{"main", "  release/*  ", "main"},
				},
			},
			SyncedAt:  &synced,
			UpdatedAt: updated,
		},
		repoCount: 17,
		jobs: []db.ConnectionSyncJobRow{
			{
				ID:              "job-1",
				Status:          "COMPLETED",
				CreatedAt:       created,
				UpdatedAt:       jobUpdated,
				CompletedAt:     &completed,
				ErrorMessage:    nil,
				WarningMessages: []string{"a", "b"},
			},
			{
				ID:              "job-2",
				Status:          "FAILED",
				CreatedAt:       created.Add(-time.Hour),
				UpdatedAt:       created.Add(-time.Hour),
				ErrorMessage:    &errMsg,
				WarningMessages: []string{},
			},
		},
	}
	srv := newStatusConnServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, statusReq(t, 42))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	want := `{"id":42,"name":"gh-prod","connectionType":"github","syncedAt":"2025-01-15T10:30:00.000Z","updatedAt":"2025-02-20T08:15:00.500Z","syncJobs":[{"id":"job-1","status":"COMPLETED","createdAt":"2025-02-19T23:00:00.000Z","updatedAt":"2025-02-19T23:05:00.000Z","completedAt":"2025-02-19T23:06:00.000Z","warningMessages":["a","b"],"errorMessage":null},{"id":"job-2","status":"FAILED","createdAt":"2025-02-19T22:00:00.000Z","updatedAt":"2025-02-19T22:00:00.000Z","completedAt":null,"warningMessages":[],"errorMessage":"boom"}],"repoCount":17,"branchPolicy":{"mode":"patterns","branches":["main","release/*"],"defaultBranchAlwaysIncluded":false,"maxIndexedRevisions":64},"latestJob":{"id":"job-1","status":"COMPLETED","createdAt":"2025-02-19T23:00:00.000Z","updatedAt":"2025-02-19T23:05:00.000Z","completedAt":"2025-02-19T23:06:00.000Z","warningMessages":["a","b"],"errorMessage":null}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body equality:\n  got  %s\n  want %s", got, want)
	}

	// Lock the handler → DB argument plumbing for cross-tenant safety.
	wantOrg := validOwnerLookup(hash).Org.ID
	if spy.metaOrg != wantOrg || spy.metaConn != 42 {
		t.Errorf("GetOrgConnectionMeta args: got (org=%d, conn=%d), want (org=%d, conn=42)", spy.metaOrg, spy.metaConn, wantOrg)
	}
	if spy.countOrg != wantOrg || spy.countConn != 42 {
		t.Errorf("CountConnectionRepos args: got (org=%d, conn=%d), want (org=%d, conn=42)", spy.countOrg, spy.countConn, wantOrg)
	}
	if spy.jobsOrg != wantOrg || spy.jobsConn != 42 || spy.jobsLimit != syncJobLimit {
		t.Errorf("ListConnectionSyncJobs args: got (org=%d, conn=%d, limit=%d), want (org=%d, conn=42, limit=%d)", spy.jobsOrg, spy.jobsConn, spy.jobsLimit, wantOrg, syncJobLimit)
	}
}

// TestConnectionStatus_MemberRole_AllowedAsRead confirms that
// MEMBER role users can read connection status; only the
// authentication gate applies, not OWNER-role enforcement.
func TestConnectionStatus_MemberRole_AllowedAsRead(t *testing.T) {
	_, hash := ownerKey(t)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &connStatusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup},
		meta: db.ConnectionMetaRow{
			ID:             7,
			Name:           "n",
			ConnectionType: "t",
			UpdatedAt:      time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	srv := newStatusConnServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, statusReq(t, 7))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
}

// TestConnectionStatus_EmptyJobs_FullShape locks the wire response
// when SyncedAt is nil, syncJobs is empty, and latestJob is null —
// all three nullable / empty-array branches in a single exact-
// equality assertion.
func TestConnectionStatus_EmptyJobs_FullShape(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &connStatusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		meta: db.ConnectionMetaRow{
			ID:             1,
			Name:           "n",
			ConnectionType: "t",
			SyncedAt:       nil,
			UpdatedAt:      time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		repoCount: 0,
		jobs:      nil,
	}
	srv := newStatusConnServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, statusReq(t, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	want := `{"id":1,"name":"n","connectionType":"t","syncedAt":null,"updatedAt":"2025-01-01T00:00:00.000Z","syncJobs":[],"repoCount":0,"branchPolicy":{"mode":"default","branches":[],"defaultBranchAlwaysIncluded":true,"maxIndexedRevisions":64},"latestJob":null}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body equality:\n  got  %s\n  want %s", got, want)
	}
	if spy.jobsLimit != syncJobLimit {
		t.Errorf("ListConnectionSyncJobs called with limit %d, want %d", spy.jobsLimit, syncJobLimit)
	}
}

// TestConnectionStatus_NotFound_Returns404 locks the full 404 envelope.
func TestConnectionStatus_NotFound_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &connStatusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		metaErr:         db.ErrConnectionNotFound,
	}
	srv := newStatusConnServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, statusReq(t, 999))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	want := `{"statusCode":404,"errorCode":"NOT_FOUND","message":"Connection not found."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestConnectionStatus_NonIntegerID_Returns400 covers parse
// failures: non-numeric, decimal, int32 overflow, and underflow.
// One sub-case locks the full 400 envelope.
func TestConnectionStatus_NonIntegerID_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	secret, _ := ownerKey(t)
	spy := &connStatusSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := newStatusConnServer(spy)
	cases := []struct {
		input    string
		wantBody string // empty means only assert status code
	}{
		{"abc", `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"Connection id must be an integer."}`},
		{"1.5", ""},
		{"2147483648", ""},
		{"-2147483649", ""},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/connections/"+c.input+"/status", nil)
			req.Header.Set("X-Api-Key", "cik_"+secret)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("input %q: status %d, want 400", c.input, rec.Code)
			}
			if c.wantBody != "" && rec.Body.String() != c.wantBody {
				t.Errorf("input %q: body got %s, want %s", c.input, rec.Body.String(), c.wantBody)
			}
		})
	}
}

// TestConnectionStatus_Int32MaxAccepted confirms the upper int32
// boundary parses cleanly and reaches the meta lookup (which then
// 404s here since the fixture has no row).
func TestConnectionStatus_Int32MaxAccepted(t *testing.T) {
	_, hash := ownerKey(t)
	secret, _ := ownerKey(t)
	spy := &connStatusSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		metaErr:         db.ErrConnectionNotFound,
	}
	srv := newStatusConnServer(spy)
	req := httptest.NewRequest(http.MethodGet, "/api/connections/2147483647/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	if spy.metaConn != 2147483647 {
		t.Errorf("metaConn: got %d, want 2147483647", spy.metaConn)
	}
}

// TestConnectionStatus_NoCredentials_Returns401 locks the full 401
// envelope.
func TestConnectionStatus_NoCredentials_Returns401(t *testing.T) {
	spy := &connStatusSpy{}
	srv := newStatusConnServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/connections/42/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	want := `{"statusCode":401,"errorCode":"NOT_AUTHENTICATED","message":"Not authenticated"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestConnectionStatus_AuthResolutionError_Returns500 exercises the
// non-isAuthFailure branch of auth.ResolveFromHeaders — e.g. a DB
// outage during the API-key lookup.
func TestConnectionStatus_AuthResolutionError_Returns500(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &connStatusSpy{
		fakeAuthQuerier: fakeAuthQuerier{
			lookupResult: validOwnerLookup(hash),
			lookupErr:    errors.New("simulated auth DB outage"),
		},
	}
	srv := newStatusConnServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, statusReq(t, 1))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated auth DB outage") {
		t.Errorf("raw auth error leaked: %s", rec.Body.String())
	}
}

// TestConnectionStatus_DBError_Returns500WithoutLeak covers the
// three independent DB failure surfaces (meta / count / list). One
// sub-case locks the full 500 envelope.
func TestConnectionStatus_DBError_Returns500WithoutLeak(t *testing.T) {
	_, hash := ownerKey(t)
	mkBase := func() *connStatusSpy {
		return &connStatusSpy{
			fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			meta: db.ConnectionMetaRow{
				ID: 1, Name: "n", ConnectionType: "t", UpdatedAt: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			},
		}
	}
	cases := []struct {
		name     string
		fix      func(*connStatusSpy)
		wantBody string
	}{
		{"meta_err", func(s *connStatusSpy) { s.metaErr = errors.New("simulated meta outage") }, `{"statusCode":500,"errorCode":"UNEXPECTED_ERROR","message":"An unexpected error occurred"}`},
		{"count_err", func(s *connStatusSpy) { s.repoErr = errors.New("simulated count outage") }, ""},
		{"jobs_err", func(s *connStatusSpy) { s.jobsErr = errors.New("simulated jobs outage") }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy := mkBase()
			c.fix(spy)
			srv := newStatusConnServer(spy)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, statusReq(t, 1))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status: got %d, want 500", rec.Code)
			}
			for _, leak := range []string{"simulated meta outage", "simulated count outage", "simulated jobs outage"} {
				if strings.Contains(rec.Body.String(), leak) {
					t.Errorf("raw error leaked into body: %s", rec.Body.String())
				}
			}
			if c.wantBody != "" && rec.Body.String() != c.wantBody {
				t.Errorf("body: got %s, want %s", rec.Body.String(), c.wantBody)
			}
		})
	}
}
