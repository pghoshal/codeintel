package connectionmanager

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestFetchReposForAzureDevOpsOrg_HappyPath asserts the
// 2-endpoint flow: /_apis/projects then /_apis/git/repositories
// per project.
func TestFetchReposForAzureDevOpsOrg_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Auth: must be Basic ":<token>".
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Basic ") {
			t.Errorf("missing Basic auth: %q", auth)
		}
		decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
		if string(decoded) != ":my-pat" {
			t.Errorf("auth decode: got %q want :my-pat", decoded)
		}
		switch {
		case r.URL.Path == "/myorg/_apis/projects":
			w.WriteHeader(200)
			fmt.Fprint(w, `{"count":2,"value":[
			  {"id":"p1","name":"AI","visibility":"public"},
			  {"id":"p2","name":"Infra","visibility":"private"}
			]}`)
		case r.URL.Path == "/myorg/p1/_apis/git/repositories":
			w.WriteHeader(200)
			fmt.Fprint(w, `{"count":1,"value":[
			  {"id":"r-ai-1","name":"agent-workbench","remoteUrl":"https://x/myorg/AI/_git/agent-workbench",
			   "webUrl":"https://x/myorg/AI/_git/agent-workbench","isFork":false,
			   "project":{"id":"p1","name":"AI","visibility":"public"}}
			]}`)
		case r.URL.Path == "/myorg/p2/_apis/git/repositories":
			w.WriteHeader(200)
			fmt.Fprint(w, `{"count":1,"value":[
			  {"id":"r-inf-1","name":"deploy","remoteUrl":"https://x/myorg/Infra/_git/deploy",
			   "isFork":true,
			   "project":{"id":"p2","name":"Infra","visibility":"private"}}
			]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForAzureDevOpsOrg(ctx, srv.Client(), srv.URL, "my-pat", "myorg")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if warn != nil {
		t.Errorf("got warning: %+v", warn)
	}
	if len(repos) != 2 {
		t.Fatalf("got %d repos want 2", len(repos))
	}
	// Verify project info backfilled correctly on each repo.
	gotProj := map[string]string{}
	for _, r := range repos {
		gotProj[r.ID] = r.Project.Name + ":" + r.Project.Visibility
	}
	if gotProj["r-ai-1"] != "AI:public" {
		t.Errorf("AI project: %q", gotProj["r-ai-1"])
	}
	if gotProj["r-inf-1"] != "Infra:private" {
		t.Errorf("Infra project: %q", gotProj["r-inf-1"])
	}
}

// TestFetchReposForAzureDevOpsOrg_404Warning pins the
// org-not-found branch.
func TestFetchReposForAzureDevOpsOrg_404Warning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Org not found"}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForAzureDevOpsOrg(ctx, srv.Client(), srv.URL, "pat", "missing-org")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if repos != nil {
		t.Errorf("expected nil repos")
	}
	if warn == nil || warn.Message != "Organization missing-org not found or no access" {
		t.Errorf("warning: %+v", warn)
	}
}

// TestFetchReposForAzureDevOpsOrg_5xxRetried: serve 503 twice
// then 200. WithRetry should drive 3 calls.
func TestFetchReposForAzureDevOpsOrg_5xxRetried(t *testing.T) {
	var callsProjects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/myorg/_apis/projects" {
			n := callsProjects.Add(1)
			if n < 3 {
				w.WriteHeader(503)
				w.Write([]byte(`busy`))
				return
			}
			w.WriteHeader(200)
			fmt.Fprint(w, `{"count":0,"value":[]}`)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForAzureDevOpsOrg(ctx, srv.Client(), srv.URL, "pat", "myorg")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if warn != nil {
		t.Errorf("warn: %+v", warn)
	}
	if len(repos) != 0 {
		t.Errorf("repos: %d", len(repos))
	}
	if callsProjects.Load() != 3 {
		t.Errorf("calls: got %d want 3", callsProjects.Load())
	}
}

// TestFetchReposForAzureDevOpsOrg_RequiresToken.
func TestFetchReposForAzureDevOpsOrg_RequiresToken(t *testing.T) {
	_, _, err := FetchReposForAzureDevOpsOrg(context.Background(), nil, "https://x", "", "org")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("expected token-required error, got %v", err)
	}
}

// TestFetchReposForAzureDevOpsOrg_PerProjectErrorAsWarning:
// one project returns 500 on the per-project /repositories
// call AFTER retries exhaust → that project surfaces as a
// warning + others succeed.
func TestFetchReposForAzureDevOpsOrg_PerProjectErrorAsWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/myorg/_apis/projects":
			w.WriteHeader(200)
			fmt.Fprint(w, `{"count":2,"value":[
			  {"id":"p1","name":"GoodProj","visibility":"public"},
			  {"id":"p-broken","name":"BadProj","visibility":"public"}
			]}`)
		case r.URL.Path == "/myorg/p1/_apis/git/repositories":
			w.WriteHeader(200)
			fmt.Fprint(w, `{"count":1,"value":[
			  {"id":"good-1","name":"good","remoteUrl":"https://x/good"}
			]}`)
		case r.URL.Path == "/myorg/p-broken/_apis/git/repositories":
			// Always 500 - retries exhaust.
			w.WriteHeader(500)
			w.Write([]byte(`busted`))
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForAzureDevOpsOrg(ctx, srv.Client(), srv.URL, "pat", "myorg")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if warn != nil {
		t.Errorf("top-level warn: %+v", warn)
	}
	if len(repos) != 1 || repos[0].ID != "good-1" {
		t.Errorf("expected just the good repo, got %+v", repos)
	}
}

// TestBuildAzureDevOpsOrgURL pins URL composition.
func TestBuildAzureDevOpsOrgURL(t *testing.T) {
	cases := []struct {
		base, org, want string
	}{
		{"https://dev.azure.com", "myorg", "https://dev.azure.com/myorg"},
		{"https://dev.azure.com/", "myorg", "https://dev.azure.com/myorg"},
		{"https://tfs.example.com/tfs", "myorg", "https://tfs.example.com/tfs/myorg"},
	}
	for _, tc := range cases {
		t.Run(tc.base, func(t *testing.T) {
			if got := buildAzureDevOpsOrgURL(tc.base, tc.org); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestFetchAzureDevOpsReposViaHTTP_RegisteredByInit confirms
// the package init() registered the live fetcher. With no Orgs
// in the config the fetcher returns an empty result rather
// than ErrAzureDevOpsFetcherNotConfigured.
func TestFetchAzureDevOpsReposViaHTTP_RegisteredByInit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, _, err := CompileAzureDevOpsFromConfig(ctx, AzureDevOpsConnectionConfig{
		Token: "pat",
		Orgs:  nil,
	}, 1)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("records: got %d want 0", len(records))
	}
}
