package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/pkg/repoindexstatus"
)

type repoBranchesSpy struct {
	fakeAuthQuerier

	metaResult       db.ConnectionMetaRow
	metaErr          error
	updateResult     db.ConnectionListRow
	updateErr        error
	upsertParams     *db.UpsertOrgConnectionParams
	upsertResult     db.ConnectionListRow
	upsertErr        error
	metadataOrgID    int32
	metadataRepoID   int32
	metadataBranches []string
	metadataErr      error
}

func (s *repoBranchesSpy) GetOrgRepoPrimaryConnectionMeta(ctx context.Context, orgID, repoID int32) (db.ConnectionMetaRow, error) {
	if s.metaErr != nil {
		return db.ConnectionMetaRow{}, s.metaErr
	}
	return s.metaResult, nil
}

func (s *repoBranchesSpy) GetOrgRepoPrimaryConnectionForUpdate(ctx context.Context, orgID, repoID int32) (db.ConnectionListRow, error) {
	if s.updateErr != nil {
		return db.ConnectionListRow{}, s.updateErr
	}
	return s.updateResult, nil
}

func (s *repoBranchesSpy) UpsertOrgConnection(ctx context.Context, p db.UpsertOrgConnectionParams) (db.ConnectionListRow, error) {
	cp := p
	s.upsertParams = &cp
	if s.upsertErr != nil {
		return db.ConnectionListRow{}, s.upsertErr
	}
	return s.upsertResult, nil
}

func (s *repoBranchesSpy) UpdateOrgRepoBranchPolicyMetadata(ctx context.Context, orgID, repoID int32, branches []string) error {
	s.metadataOrgID = orgID
	s.metadataRepoID = repoID
	s.metadataBranches = append([]string(nil), branches...)
	return s.metadataErr
}

type staticRepoStatusFetcher struct {
	resp RepoStatusResponse
	err  error
}

func (f staticRepoStatusFetcher) Fetch(ctx context.Context, orgID, repoID int32, requestedBranch string) (RepoStatusResponse, error) {
	if f.err != nil {
		return RepoStatusResponse{}, f.err
	}
	return f.resp, nil
}

func newRepoBranchesTestServer(spy *repoBranchesSpy, fetcher RepoStatusFetcher) *Server {
	return NewServer(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:           spy,
		EncryptionKey:     "0123456789abcdef0123456789abcdef",
		RepoStatusFetcher: fetcher,
	})
}

func TestRepoBranchesGet_ComposesConnectionPolicyAndBranchStatus(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	updatedAt := time.Date(2026, 5, 28, 10, 0, 0, 0, time.UTC)
	branch := repoindexstatus.BranchIndexStatus{
		Branch:          "release-a",
		Revision:        "refs/heads/release-a",
		AllowedByPolicy: true,
		Indexed:         true,
		Status:          repoindexstatus.StateIndexed,
		Color:           repoindexstatus.ColorGreen,
	}
	spy := &repoBranchesSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		metaResult: db.ConnectionMetaRow{
			ID:             42,
			Name:           "git-fixtures",
			ConnectionType: "git",
			Config: map[string]any{
				"revisions": map[string]any{"branches": []any{"release-a"}},
			},
			UpdatedAt: updatedAt,
		},
	}
	fetcher := staticRepoStatusFetcher{resp: RepoStatusResponse{
		ID:             99,
		Name:           "fixtures/repo",
		BranchStatuses: []repoindexstatus.BranchIndexStatus{branch},
		BranchStatus:   &branch,
	}}
	srv := newRepoBranchesTestServer(spy, fetcher)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos/99/branches?branch=release-a", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got := body["repoId"]; got != float64(99) {
		t.Errorf("repoId: got %v want 99", got)
	}
	policy := body["branchPolicy"].(map[string]any)
	if got := policy["mode"]; got != "patterns" {
		t.Errorf("branchPolicy.mode: got %v want patterns", got)
	}
	status := body["branchStatus"].(map[string]any)
	if got := status["status"]; got != "indexed" {
		t.Errorf("branchStatus.status: got %v want indexed", got)
	}
}

func TestRepoBranchesPut_UpdatesPrimaryConnectionWithoutSync(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	updatedAt := time.Date(2026, 5, 28, 11, 0, 0, 0, time.UTC)
	spy := &repoBranchesSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		updateResult: db.ConnectionListRow{
			ID:             42,
			Name:           "git-fixtures",
			ConnectionType: "git",
			Config: map[string]any{
				"type":      "git",
				"revisions": map[string]any{"branches": []any{"old"}},
			},
		},
		upsertResult: db.ConnectionListRow{
			ID:             42,
			Name:           "git-fixtures",
			ConnectionType: "git",
			Config: map[string]any{
				"type":      "git",
				"revisions": map[string]any{"branches": []any{"release-a"}},
			},
			UpdatedAt: updatedAt,
		},
	}
	fetcher := staticRepoStatusFetcher{resp: RepoStatusResponse{
		ID:             99,
		Name:           "fixtures/repo",
		BranchStatuses: []repoindexstatus.BranchIndexStatus{},
	}}
	srv := newRepoBranchesTestServer(spy, fetcher)

	rec := httptest.NewRecorder()
	body := `{"mode":"patterns","branches":["release-a"," release-a "],"sync":false}`
	req := httptest.NewRequest(http.MethodPut, "/api/repos/99/branches", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "cik_"+secret)
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if spy.upsertParams == nil {
		t.Fatalf("UpsertOrgConnection was not called")
	}
	if spy.upsertParams.ResetSync {
		t.Errorf("ResetSync: got true want false")
	}
	cfg := spy.upsertParams.Config.(map[string]any)
	revisions := cfg["revisions"].(map[string]any)
	branches := revisions["branches"].([]any)
	if len(branches) != 1 || branches[0] != "release-a" {
		t.Errorf("branches: got %#v want [release-a]", branches)
	}
	if spy.metadataOrgID != 7 || spy.metadataRepoID != 99 || len(spy.metadataBranches) != 1 || spy.metadataBranches[0] != "release-a" {
		t.Errorf("repo metadata policy update: org=%d repo=%d branches=%v, want org=7 repo=99 branches=[release-a]", spy.metadataOrgID, spy.metadataRepoID, spy.metadataBranches)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	policy := decoded["branchPolicy"].(map[string]any)
	if got := policy["mode"]; got != "patterns" {
		t.Errorf("branchPolicy.mode: got %v want patterns", got)
	}
}
