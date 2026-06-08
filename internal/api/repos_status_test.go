package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// recordingStatusFetcher captures every Fetch call so the test
// can assert tenant-id scoping. Same shape as recordingSyncer.
type recordingStatusFetcher struct {
	calls    []recordingStatusCall
	response RepoStatusResponse
	err      error
}
type recordingStatusCall struct {
	OrgID, RepoID   int32
	RequestedBranch string
}

func (r *recordingStatusFetcher) Fetch(_ context.Context, orgID, repoID int32, requestedBranch string) (RepoStatusResponse, error) {
	r.calls = append(r.calls, recordingStatusCall{orgID, repoID, requestedBranch})
	return r.response, r.err
}

func newRepoStatusServer(spy *upsertConnectionSpy, fetcher RepoStatusFetcher) *Server {
	return NewServer(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:           spy,
		EncryptionKey:     "0123456789abcdef0123456789abcdef",
		RepoStatusFetcher: fetcher,
	})
}

func repoStatusRequest(t *testing.T, id int) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodGet, "/api/repos/"+itoa(id)+"/status", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	return req
}

func TestGetRepoStatus_OwnerHappy_Returns200(t *testing.T) {
	_, hash := ownerKey(t)
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	displayName := "Display Name"
	indexedHash := "abc123"
	latest := "COMPLETED"
	fetcher := &recordingStatusFetcher{response: RepoStatusResponse{
		ID:                      42,
		Name:                    "owner/repo",
		DisplayName:             &displayName,
		IndexedAt:               &now,
		IndexedCommitHash:       &indexedHash,
		LatestIndexingJobStatus: &latest,
		UpdatedAt:               now,
		Jobs: []RepoStatusJob{{
			ID: "job-1", Type: "REMOVE_INDEX", Status: "COMPLETED",
			CreatedAt: now, UpdatedAt: now,
		}},
	}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newRepoStatusServer(spy, fetcher)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, repoStatusRequest(t, 42))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q want 200", rec.Code, rec.Body.String())
	}
	var resp RepoStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode body: %v body=%q", err, rec.Body.String())
	}
	if resp.ID != 42 || resp.Name != "owner/repo" {
		t.Errorf("repo fields: got id=%d name=%q", resp.ID, resp.Name)
	}
	if resp.LatestIndexingJobStatus == nil || *resp.LatestIndexingJobStatus != "COMPLETED" {
		t.Errorf("latestIndexingJobStatus: got %v want COMPLETED", resp.LatestIndexingJobStatus)
	}
	if len(resp.Jobs) != 1 || resp.Jobs[0].ID != "job-1" {
		t.Errorf("jobs: got %+v", resp.Jobs)
	}
	if len(fetcher.calls) != 1 {
		t.Fatalf("fetcher should fire once, got %d", len(fetcher.calls))
	}
	if fetcher.calls[0].OrgID != 7 || fetcher.calls[0].RepoID != 42 {
		t.Errorf("fetcher call args: got %+v want {OrgID:7, RepoID:42}", fetcher.calls[0])
	}
}

func TestGetRepoStatus_NoFetcherWired_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newRepoStatusServer(spy, nil) // nil -> NoopRepoStatusFetcher
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, repoStatusRequest(t, 42))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d body=%q want 404", rec.Code, rec.Body.String())
	}
}

func TestGetRepoStatus_FetcherReturnsNotFound_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	fetcher := &recordingStatusFetcher{err: ErrRepoStatusNotFound}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newRepoStatusServer(spy, fetcher)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, repoStatusRequest(t, 42))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
}

func TestManifestQualityIssuesTreatsRepoRewriteAsTruthfulZoektStrategy(t *testing.T) {
	rewrite := "FULL_REPO_REWRITE"
	full := "FULL_REPO"
	supersededAt := time.Date(2026, 6, 5, 10, 0, 0, 0, time.UTC)
	rewriteIssues := manifestQualityIssues(RepoIndexManifestRow{
		ID:               "manifest-rewrite",
		Status:           "READY",
		FileCount:        2,
		FileRowCount:     2,
		ChangedFileCount: 1,
		ZoektStrategy:    &rewrite,
	})
	if len(rewriteIssues) != 0 {
		t.Fatalf("FULL_REPO_REWRITE should not be reported as failed delta: %+v", rewriteIssues)
	}
	fullIssues := manifestQualityIssues(RepoIndexManifestRow{
		ID:               "manifest-full",
		Status:           "READY",
		FileCount:        2,
		FileRowCount:     2,
		ChangedFileCount: 1,
		ZoektStrategy:    &full,
	})
	if len(fullIssues) != 1 || fullIssues[0] != "Zoekt reindex used full-repo strategy for changed files" {
		t.Fatalf("legacy FULL_REPO changed-file issue = %+v", fullIssues)
	}
	firstFullIssues := manifestQualityIssues(RepoIndexManifestRow{
		ID:             "manifest-first-full",
		Status:         "READY",
		FileCount:      2,
		FileRowCount:   2,
		AddedFileCount: 2,
		ZoektStrategy:  &full,
	})
	if len(firstFullIssues) != 0 {
		t.Fatalf("first full index added-file FULL_REPO should be healthy: %+v", firstFullIssues)
	}
	topLevelIssues := repoManifestQualityIssues([]RepoIndexManifestRow{
		{
			ID:               "manifest-old",
			Status:           "READY",
			SupersededAt:     &supersededAt,
			ChangedFileCount: 1,
			ZoektStrategy:    &full,
			QualityIssues:    fullIssues,
		},
		{
			ID:            "manifest-current",
			Status:        "READY",
			ZoektStrategy: &rewrite,
		},
	})
	if len(topLevelIssues) != 0 {
		t.Fatalf("superseded legacy manifest should not poison current top-level health: %+v", topLevelIssues)
	}
}

func TestGetRepoStatus_NonIntegerID_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	fetcher := &recordingStatusFetcher{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newRepoStatusServer(spy, fetcher)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/repos/abc/status", nil)
	req.Header.Set("X-Api-Key", "cik_ownersec")
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	if len(fetcher.calls) != 0 {
		t.Errorf("fetcher should not fire on 400, got %d calls", len(fetcher.calls))
	}
}

func TestGetRepoStatus_NoAuth_Returns401(t *testing.T) {
	_, hash := ownerKey(t)
	fetcher := &recordingStatusFetcher{}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newRepoStatusServer(spy, fetcher)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/repos/42/status", nil)
	// no X-Api-Key / Authorization header
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
}

func TestGetRepoStatus_OmitemptyOnNullColumns(t *testing.T) {
	// Repo never indexed: indexedAt, indexedCommitHash,
	// latestIndexingJobStatus all NULL -> JSON should drop the
	// keys (not emit "null").
	_, hash := ownerKey(t)
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	fetcher := &recordingStatusFetcher{response: RepoStatusResponse{
		ID:        42,
		Name:      "owner/repo",
		UpdatedAt: now,
		Jobs:      []RepoStatusJob{},
	}}
	spy := &upsertConnectionSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newRepoStatusServer(spy, fetcher)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, repoStatusRequest(t, 42))
	body := rec.Body.String()
	for _, key := range []string{
		`"displayName"`, `"defaultBranch"`,
		`"indexedAt"`, `"indexedCommitHash"`,
		`"latestIndexingJobStatus"`,
	} {
		if containsKey(body, key) {
			t.Errorf("omitempty broken: body still contains %s\nbody=%s", key, body)
		}
	}
	if !containsKey(body, `"jobs":[]`) {
		t.Errorf("jobs should serialise as [], got body=%s", body)
	}
}

func containsKey(haystack, needle string) bool {
	// substring match — fine because the response is small JSON
	// and we only check unique key spellings.
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
