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

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

type searchContextsSpy struct {
	fakeAuthQuerier

	listRows     []db.SearchContextRow
	replaceOrgID int32
	replaceInput []db.SearchContextInput
	replaceErr   error
}

func (s *searchContextsSpy) ListOrgSearchContexts(ctx context.Context, orgID int32) ([]db.SearchContextRow, error) {
	if s.listRows == nil {
		return []db.SearchContextRow{}, nil
	}
	return s.listRows, nil
}

func (s *searchContextsSpy) ReplaceOrgSearchContexts(ctx context.Context, orgID int32, contexts []db.SearchContextInput) error {
	s.replaceOrgID = orgID
	s.replaceInput = contexts
	return s.replaceErr
}

func newSearchContextsTestServer(spy *searchContextsSpy) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
	})
}

func TestSearchContextsPut_ArrayContractNormalizesRepos(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &searchContextsSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newSearchContextsTestServer(spy)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/search-contexts", strings.NewReader(`{"contexts":[{"name":"tenant-main","description":"Tenant","repos":["repo-a","repo-a"],"include":["repo-b"]}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "cik_"+secret)
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"success":true}` {
		t.Errorf("body: got %s want success", got)
	}
	if spy.replaceOrgID != 7 {
		t.Errorf("replaceOrgID: got %d want 7", spy.replaceOrgID)
	}
	if len(spy.replaceInput) != 1 {
		t.Fatalf("replaceInput length: got %d want 1", len(spy.replaceInput))
	}
	in := spy.replaceInput[0]
	if in.Name != "tenant-main" {
		t.Errorf("Name: got %q want tenant-main", in.Name)
	}
	if len(in.RepoNames) != 2 || in.RepoNames[0] != "repo-b" || in.RepoNames[1] != "repo-a" {
		t.Errorf("RepoNames: got %v want [repo-b repo-a]", in.RepoNames)
	}
	cfg := in.Config.(map[string]any)
	include := cfg["include"].([]string)
	if len(include) != 2 || include[0] != "repo-b" || include[1] != "repo-a" {
		t.Errorf("config.include: got %v want [repo-b repo-a]", include)
	}
}

func TestSearchContextsGet_ReturnsRepoNames(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	desc := "Tenant"
	spy := &searchContextsSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.SearchContextRow{{
			ID:            3,
			Name:          "tenant-main",
			Description:   &desc,
			Config:        json.RawMessage(`{"include":["repo-a"]}`),
			IsDeclarative: false,
			RepoNames:     []string{"repo-a"},
		}},
	}
	srv := newSearchContextsTestServer(spy)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/search-contexts", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%q, want 200", rec.Code, rec.Body.String())
	}
	want := `[{"id":3,"name":"tenant-main","description":"Tenant","config":{"include":["repo-a"]},"isDeclarative":false,"repoNames":["repo-a"]}]`
	if got := rec.Body.String(); got != want {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, want)
	}
}
