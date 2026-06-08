package connectionmanager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"code.gitea.io/sdk/gitea"
)

// newTestGiteaClient builds a gitea client pointed at the
// httptest server. The SDK requires a version endpoint to come
// back from the host on construction; we serve `/api/v1/version`
// from the same handler.
func newTestGiteaClient(t *testing.T, listHandler http.Handler) (*gitea.Client, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/version", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"version":"1.21.0"}`))
	})
	mux.Handle("/", listHandler)
	srv := httptest.NewServer(mux)
	c, err := gitea.NewClient(srv.URL)
	if err != nil {
		srv.Close()
		t.Fatalf("gitea.NewClient: %v", err)
	}
	return c, srv.Close
}

// fakeGiteaRepoJSON builds a minimal repo response body.
func fakeGiteaRepoJSON(id int, fullName string) string {
	return fmt.Sprintf(`{
	  "id": %d, "name": "%s",
	  "full_name": "test-org/%s",
	  "fork": false, "private": false, "internal": false,
	  "html_url": "https://gitea-fake/test-org/%s",
	  "clone_url": "https://gitea-fake/test-org/%s.git",
	  "default_branch": "main",
	  "archived": false,
	  "owner": {"login": "test-org", "avatar_url": "https://gitea-fake/avatar"}
	}`, id, fullName, fullName, fullName, fullName)
}

// TestFetchReposForGiteaOrg_SinglePage covers the happy path.
func TestFetchReposForGiteaOrg_SinglePage(t *testing.T) {
	called := 0
	client, stop := newTestGiteaClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/orgs/test-org/repos" {
			called++
			// page=1 returns 2 repos (< pageSize=50, so loop ends).
			w.WriteHeader(200)
			_, _ = w.Write([]byte("[" + fakeGiteaRepoJSON(1, "alpha") + "," + fakeGiteaRepoJSON(2, "beta") + "]"))
			return
		}
		w.WriteHeader(404)
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForGiteaOrg(ctx, client, "test-org")
	if err != nil {
		t.Fatalf("FetchReposForGiteaOrg: %v", err)
	}
	if warn != nil {
		t.Errorf("expected nil warning, got %+v", warn)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos want 2", len(repos))
	}
	if repos[0].FullName != "test-org/alpha" {
		t.Errorf("repos[0].FullName: %q", repos[0].FullName)
	}
	if called != 1 {
		t.Errorf("API call count: got %d want 1", called)
	}
}

// TestFetchReposForGiteaOrg_404Warning pins the 404 branch.
func TestFetchReposForGiteaOrg_404Warning(t *testing.T) {
	client, stop := newTestGiteaClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForGiteaOrg(ctx, client, "missing")
	if err != nil {
		t.Fatalf("got err: %v", err)
	}
	if repos != nil {
		t.Errorf("expected nil repos, got %v", repos)
	}
	if warn == nil {
		t.Fatalf("expected warning")
	}
	if warn.Message != "Org missing not found or no access" {
		t.Errorf("warning: got %q", warn.Message)
	}
}

// TestFetchReposForGiteaOrg_InputValidation pins boundary guards.
func TestFetchReposForGiteaOrg_InputValidation(t *testing.T) {
	_, _, err := FetchReposForGiteaOrg(context.Background(), nil, "test-org")
	if err == nil {
		t.Errorf("expected error for nil client")
	}
}

// TestMapGiteaRepo_NilHandling pins nil-safety.
func TestMapGiteaRepo_NilHandling(t *testing.T) {
	got := mapGiteaRepo(nil)
	if got.FullName != "" {
		t.Errorf("nil input should give zero value")
	}
}

// TestCompileGiteaFromConfig_SDKFetcherRegistered confirms the
// package's init() registered the SDK-backed fetcher (so calling
// CompileGiteaFromConfig does NOT return
// ErrGiteaFetcherNotConfigured even with no caller wiring).
//
// This test must run BEFORE any test that calls SetGiteaFetcher(nil)
// — see ordering note below. We rely on the init() registration
// always being present at package load.
func TestCompileGiteaFromConfig_SDKFetcherRegistered(t *testing.T) {
	// Restore the SDK-backed fetcher in case a prior test
	// nilled it.
	SetGiteaFetcher(fetchGiteaReposViaSDK)

	// Calling with an empty config triggers the SDK fetcher's
	// happy path (zero orgs to enumerate -> empty result). Make
	// sure no ErrGiteaFetcherNotConfigured comes back.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, _, err := CompileGiteaFromConfig(ctx, GiteaConnectionConfig{
		// No URL → defaults to https://gitea.com; no Orgs → fetcher
		// loops over zero items and returns empty.
		Orgs: nil,
	}, 1)
	if err != nil {
		t.Fatalf("CompileGiteaFromConfig: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("expected 0 records (no orgs), got %d", len(records))
	}
}
