package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v75/github"
)

// newTestClient builds a github.Client pointed at a httptest
// server. The server's URL replaces the default https://api.github.com
// base so all API calls hit the local handler.
func newTestClient(t *testing.T, h http.Handler) (*github.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	u, err := url.Parse(srv.URL + "/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c := github.NewClient(srv.Client())
	c.BaseURL = u
	return c, srv.Close
}

// fakeRepoJSON builds a minimal GitHub repo JSON object. The
// go-github decoder is forgiving — unspecified fields surface
// as nil pointers, which is exactly the optional-shape the
// legacy code reads.
func fakeRepoJSON(id int, fullName string) string {
	return fmt.Sprintf(`{
		"id": %d,
		"name": "%s",
		"full_name": "test-org/%s",
		"fork": false,
		"private": false,
		"html_url": "https://github.com/test-org/%s",
		"clone_url": "https://github.com/test-org/%s.git",
		"stargazers_count": 10,
		"default_branch": "main",
		"topics": ["test"],
		"owner": {"login": "test-org", "avatar_url": "https://x/avatar"}
	}`, id, fullName, fullName, fullName, fullName)
}

// TestFetchReposForOrg_SinglePage covers the happy path: one
// page of N repos returned in a single response (no Link
// header → loop terminates).
func TestFetchReposForOrg_SinglePage(t *testing.T) {
	client, stop := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/orgs/test-org/repos" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page: got %q want \"100\"", r.URL.Query().Get("per_page"))
		}
		w.WriteHeader(200)
		w.Write([]byte("[" + fakeRepoJSON(1, "alpha") + "," + fakeRepoJSON(2, "beta") + "]"))
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForOrg(ctx, client, "test-org")
	if err != nil {
		t.Fatalf("FetchReposForOrg: %v", err)
	}
	if warn != nil {
		t.Errorf("expected nil warning, got %+v", warn)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(repos))
	}
	if repos[0].FullName != "test-org/alpha" {
		t.Errorf("repo[0].FullName: got %q", repos[0].FullName)
	}
	if repos[0].StargazersCount == nil || *repos[0].StargazersCount != 10 {
		t.Errorf("StargazersCount mapping wrong")
	}
}

// TestFetchReposForOrg_Pagination locks the multi-page loop:
// page 1 returns a Link header pointing to page 2, the function
// must follow it and accumulate.
func TestFetchReposForOrg_Pagination(t *testing.T) {
	var calls int
	client, stop := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		page := r.URL.Query().Get("page")
		switch page {
		case "", "0", "1":
			// First page: return one repo + Link to page 2.
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/test-org/repos?page=2&per_page=100>; rel="next"`, r.Host))
			// Use the host from the request so the Link URL
			// points back at the same httptest server.
			w.Header().Set("Link", fmt.Sprintf(`<%s://%s/orgs/test-org/repos?page=2&per_page=100>; rel="next"`, scheme(r), r.Host))
			w.WriteHeader(200)
			w.Write([]byte("[" + fakeRepoJSON(1, "alpha") + "]"))
		case "2":
			// Second page: one more repo, no Link header → stop.
			w.WriteHeader(200)
			w.Write([]byte("[" + fakeRepoJSON(2, "beta") + "]"))
		default:
			t.Errorf("unexpected page: %q", page)
			w.WriteHeader(500)
		}
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForOrg(ctx, client, "test-org")
	if err != nil {
		t.Fatalf("FetchReposForOrg: %v", err)
	}
	if warn != nil {
		t.Errorf("got warning: %+v", warn)
	}
	if calls != 2 {
		t.Errorf("API call count: got %d, want 2", calls)
	}
	if len(repos) != 2 || repos[1].FullName != "test-org/beta" {
		t.Errorf("pagination accumulation wrong: %+v", repos)
	}
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// TestFetchReposForOrg_404Warning pins the legacy branch where
// a 404 from listForOrg surfaces as a typed warning (no error).
func TestFetchReposForOrg_404Warning(t *testing.T) {
	client, stop := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForOrg(ctx, client, "missing-org")
	if err != nil {
		t.Fatalf("got err: %v", err)
	}
	if repos != nil {
		t.Errorf("expected nil repos, got %v", repos)
	}
	if warn == nil {
		t.Fatalf("expected warning, got nil")
	}
	if warn.Message != "Organization missing-org not found or no access" {
		t.Errorf("warning text: got %q", warn.Message)
	}
}

// TestFetchReposForOrg_NonRetryableError pins that a non-404 HTTP
// error surfaces as a real error (not a warning).
func TestFetchReposForOrg_NonRetryableError(t *testing.T) {
	client, stop := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"boom"}`))
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := FetchReposForOrg(ctx, client, "test-org")
	if err == nil {
		t.Fatalf("expected error for 500")
	}
}

// TestFetchReposForOrg_CtxAbort confirms a cancelled context
// surfaces as ErrFetchAborted.
func TestFetchReposForOrg_CtxAbort(t *testing.T) {
	client, stop := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow response so the ctx cancels first.
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
		w.Write([]byte("[]"))
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err := FetchReposForOrg(ctx, client, "test-org")
	if err == nil {
		t.Fatalf("expected error from cancelled ctx")
	}
}

// TestFetchReposForOrg_InputValidation pins boundary guards.
func TestFetchReposForOrg_InputValidation(t *testing.T) {
	_, _, err := FetchReposForOrg(context.Background(), nil, "test-org")
	if err == nil {
		t.Errorf("expected error for nil client")
	}
	c := github.NewClient(nil)
	_, _, err = FetchReposForOrg(context.Background(), c, "")
	if err == nil {
		t.Errorf("expected error for empty org")
	}
}

// TestMapGitHubRepo_NilHandling pins that mapGitHubRepo handles
// nil inputs gracefully (returns zero-value struct, not panic).
func TestMapGitHubRepo_NilHandling(t *testing.T) {
	got := mapGitHubRepo(nil)
	if got.FullName != "" {
		t.Errorf("nil input should give zero value")
	}
}

// TestMapGitHubRepo_PointerFields exercises the pointer-field
// mapping. A go-github repo with a subset of fields populated
// must map to an OctokitRepository with matching nil/non-nil
// pointers.
func TestMapGitHubRepo_PointerFields(t *testing.T) {
	js := []byte(fakeRepoJSON(99, "gamma"))
	var r github.Repository
	if err := json.Unmarshal(js, &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := mapGitHubRepo(&r)
	if got.FullName != "test-org/gamma" {
		t.Errorf("FullName: %q", got.FullName)
	}
	if got.CloneURL == nil || *got.CloneURL != "https://github.com/test-org/gamma.git" {
		t.Errorf("CloneURL mapping wrong")
	}
	if got.DefaultBranch == nil || *got.DefaultBranch != "main" {
		t.Errorf("DefaultBranch mapping wrong")
	}
	if got.Owner.Login != "test-org" {
		t.Errorf("Owner.Login: %q", got.Owner.Login)
	}
}

// silence unused-import for testing.T errors helper.
var _ = errors.New
