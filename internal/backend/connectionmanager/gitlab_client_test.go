package connectionmanager

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gitlab "github.com/xanzy/go-gitlab"
)

// newTestGitLabClient builds a go-gitlab client pointed at a
// httptest server.
func newTestGitLabClient(t *testing.T, h http.Handler) (*gitlab.Client, func()) {
	t.Helper()
	srv := httptest.NewServer(h)
	c, err := gitlab.NewClient("", gitlab.WithBaseURL(srv.URL+"/api/v4"))
	if err != nil {
		t.Fatalf("gitlab.NewClient: %v", err)
	}
	return c, srv.Close
}

// fakeGitLabProjectJSON builds a minimal project response.
func fakeGitLabProjectJSON(id int, path string) string {
	return fmt.Sprintf(`{
	  "id": %d,
	  "name": "%s",
	  "path_with_namespace": "alpha-group/%s",
	  "http_url_to_repo": "http://gitlab.example.com/alpha-group/%s.git",
	  "default_branch": "main",
	  "visibility": "public",
	  "archived": false,
	  "topics": ["go"],
	  "star_count": 5,
	  "forks_count": 2,
	  "avatar_url": ""
	}`, id, path, path, path)
}

// TestFetchProjectsForGroup_SinglePage covers the happy path.
func TestFetchProjectsForGroup_SinglePage(t *testing.T) {
	client, stop := newTestGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/groups/alpha-group/projects" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page: got %q want 100", r.URL.Query().Get("per_page"))
		}
		if r.URL.Query().Get("include_subgroups") != "true" {
			t.Errorf("include_subgroups: got %q want true", r.URL.Query().Get("include_subgroups"))
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, "[%s,%s]", fakeGitLabProjectJSON(1, "alpha"), fakeGitLabProjectJSON(2, "beta"))
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projects, warn, err := FetchProjectsForGroup(ctx, client, "alpha-group")
	if err != nil {
		t.Fatalf("FetchProjectsForGroup: %v", err)
	}
	if warn != nil {
		t.Errorf("got warning: %+v", warn)
	}
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}
	if projects[0].PathWithNamespace != "alpha-group/alpha" {
		t.Errorf("projects[0].PathWithNamespace: %q", projects[0].PathWithNamespace)
	}
	if projects[0].StargazersCount == nil || *projects[0].StargazersCount != 5 {
		t.Errorf("StargazersCount mapping wrong")
	}
}

// TestFetchProjectsForGroup_404Warning pins the 404 branch.
func TestFetchProjectsForGroup_404Warning(t *testing.T) {
	client, stop := newTestGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"404 Not Found"}`))
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	projects, warn, err := FetchProjectsForGroup(ctx, client, "missing")
	if err != nil {
		t.Fatalf("got err: %v", err)
	}
	if projects != nil {
		t.Errorf("expected nil projects, got %v", projects)
	}
	if warn == nil {
		t.Fatalf("expected warning")
	}
	if warn.Message != "Group missing not found or no access" {
		t.Errorf("warning: got %q", warn.Message)
	}
}

// TestFetchProjectsForGroup_NonRetryableError pins 500 paths
// as real errors.
func TestFetchProjectsForGroup_NonRetryableError(t *testing.T) {
	client, stop := newTestGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"message":"boom"}`))
	}))
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, err := FetchProjectsForGroup(ctx, client, "alpha")
	if err == nil {
		t.Fatalf("expected error for 500")
	}
}

// TestFetchProjectsForGroup_InputValidation pins boundary
// guards.
func TestFetchProjectsForGroup_InputValidation(t *testing.T) {
	_, _, err := FetchProjectsForGroup(context.Background(), nil, "alpha")
	if err == nil {
		t.Errorf("expected error for nil client")
	}
	c, _ := gitlab.NewClient("", gitlab.WithBaseURL("http://localhost/api/v4"))
	_, _, err = FetchProjectsForGroup(context.Background(), c, "")
	if err == nil {
		t.Errorf("expected error for empty group")
	}
}

// TestMapGitLabProject_NilHandling pins nil-safety.
func TestMapGitLabProject_NilHandling(t *testing.T) {
	got := mapGitLabProject(nil)
	if got.PathWithNamespace != "" {
		t.Errorf("nil input should give zero value")
	}
}
