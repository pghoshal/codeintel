package connectionmanager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-github/v75/github"
)

// buildTestClient is a helper that points a github.Client at a
// local httptest.Server. Same pattern as github_client_test.go.
func buildTestClient(t *testing.T, h http.Handler) (*github.Client, func()) {
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

// TestGetGitHubReposFromConfig_HappyPath wires the orchestrator
// against an httptest server returning one repo per org. Two
// orgs in config → 2 repos accumulated; filter keeps both.
func TestGetGitHubReposFromConfig_HappyPath(t *testing.T) {
	// Stub the orchestrator's client at the package-level by
	// running the test through a sub-handler. We don't have a
	// direct way to inject the client (the orchestrator builds
	// its own), so we shim by setting the GitHub base URL via
	// the cfg.URL field — which the orchestrator threads through
	// to NewEnterpriseClient. That branch routes to our local
	// httptest server.
	called := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		// go-github Enterprise prepends /api/v3 to all paths.
		// The orgs route lands at /api/v3/orgs/<org>/repos.
		if r.URL.Path == "/api/v3/orgs/alpha-team/repos" {
			w.WriteHeader(200)
			w.Write([]byte("[" + fakeRepoJSON(1, "alpha-repo") + "]"))
			return
		}
		if r.URL.Path == "/api/v3/orgs/beta-team/repos" {
			w.WriteHeader(200)
			w.Write([]byte("[" + fakeRepoJSON(2, "beta-repo") + "]"))
			return
		}
		t.Errorf("unexpected path: %s", r.URL.Path)
		w.WriteHeader(404)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := GetGitHubReposFromConfig(ctx, GitHubConnectionConfig{
		URL:  srv.URL,
		Orgs: []string{"alpha-team", "beta-team"},
	})
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	if called != 2 {
		t.Errorf("expected 2 API calls, got %d", called)
	}
	if len(got.Repos) != 2 {
		t.Fatalf("got %d repos, want 2", len(got.Repos))
	}
	if len(got.Warnings) != 0 {
		t.Errorf("expected no warnings, got %+v", got.Warnings)
	}
}

// TestGetGitHubReposFromConfig_404WarningAggregation pins the
// per-org 404 → warning flattening. The legacy aggregates
// warnings into the top-level warnings list and continues with
// the other orgs.
func TestGetGitHubReposFromConfig_404WarningAggregation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/orgs/alpha-team/repos" {
			w.WriteHeader(200)
			w.Write([]byte("[" + fakeRepoJSON(1, "alpha-repo") + "]"))
			return
		}
		if r.URL.Path == "/api/v3/orgs/missing-org/repos" {
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := GetGitHubReposFromConfig(ctx, GitHubConnectionConfig{
		URL:  srv.URL,
		Orgs: []string{"alpha-team", "missing-org"},
	})
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	if len(got.Repos) != 1 {
		t.Errorf("expected 1 repo (alpha-team only), got %d", len(got.Repos))
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "Organization missing-org not found or no access" {
		t.Errorf("warnings: %+v", got.Warnings)
	}
}

// TestGetGitHubReposFromConfig_ExcludeFilter pins that the
// orchestrator applies the ShouldExcludeRepo filter post-fetch.
// Configure exclude.forks=true and serve a fork repo; the fork
// must NOT appear in the output.
func TestGetGitHubReposFromConfig_ExcludeFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Two repos: one normal, one fork.
		w.WriteHeader(200)
		body := fmt.Sprintf(`[
		  %s,
		  {"id":99,"name":"forked","full_name":"alpha-team/forked","fork":true,"private":false,
		   "html_url":"https://github.com/alpha-team/forked",
		   "clone_url":"https://github.com/alpha-team/forked.git",
		   "owner":{"login":"alpha-team","avatar_url":"https://x"}}
		]`, fakeRepoJSON(1, "regular"))
		w.Write([]byte(body))
	}))
	defer srv.Close()

	excludeForks := pBool(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := GetGitHubReposFromConfig(ctx, GitHubConnectionConfig{
		URL:     srv.URL,
		Orgs:    []string{"alpha-team"},
		Exclude: &GitHubExcludeRules{Forks: excludeForks},
	})
	if err != nil {
		t.Fatalf("orchestrator: %v", err)
	}
	if len(got.Repos) != 1 || got.Repos[0].FullName != "test-org/regular" {
		t.Errorf("expected only the non-fork repo, got %+v", got.Repos)
	}
}

// TestResolveHostURL pins the host-URL derivation.
func TestResolveHostURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  GitHubConnectionConfig
		want string
	}{
		{"empty defaults to github.com", GitHubConnectionConfig{}, "https://github.com"},
		{"trailing slash stripped", GitHubConnectionConfig{URL: "https://github.example.com/"}, "https://github.example.com"},
		{"multi slash stripped", GitHubConnectionConfig{URL: "https://github.example.com///"}, "https://github.example.com"},
		{"clean URL passes through", GitHubConnectionConfig{URL: "https://github.example.com"}, "https://github.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveHostURL(tc.cfg); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestCompileFromConfig_EndToEnd is the full pipeline test:
// fetch → filter → compile → RepoData. Validates that the
// pieces wire together end-to-end.
func TestCompileFromConfig_EndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("[" + fakeRepoJSON(42, "production") + "]"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, warnings, err := CompileFromConfig(ctx, GitHubConnectionConfig{
		URL:  srv.URL,
		Orgs: []string{"alpha-team"},
		Revisions: &GitHubRevisions{
			Branches: []string{"main"},
		},
	}, 123)
	if err != nil {
		t.Fatalf("CompileFromConfig: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("expected no warnings, got %+v", warnings)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 RepoData, got %d", len(got))
	}
	rec := got[0]
	if rec.ExternalID != "42" {
		t.Errorf("ExternalID: got %q", rec.ExternalID)
	}
	if rec.ExternalCodeHostType != "github" {
		t.Errorf("ExternalCodeHostType: got %q", rec.ExternalCodeHostType)
	}
	if rec.ExternalCodeHostURL != srv.URL {
		t.Errorf("ExternalCodeHostURL: got %q want %q", rec.ExternalCodeHostURL, srv.URL)
	}
	if len(rec.ConnectionIDs) != 1 || rec.ConnectionIDs[0] != 123 {
		t.Errorf("ConnectionIDs: got %+v want [123]", rec.ConnectionIDs)
	}
	if rec.Metadata.Branches == nil || rec.Metadata.Branches[0] != "main" {
		t.Errorf("Metadata.Branches: got %+v", rec.Metadata.Branches)
	}
}

// TestGetGitHubReposFromConfig_ContextAbort confirms ctx
// cancellation surfaces from the inner fetcher.
func TestGetGitHubReposFromConfig_ContextAbort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(200)
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := GetGitHubReposFromConfig(ctx, GitHubConnectionConfig{
		URL:  srv.URL,
		Orgs: []string{"alpha-team"},
	})
	if err == nil {
		t.Fatalf("expected ctx-abort error")
	}
}
