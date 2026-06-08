package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// sampleBitbucketServerRepo mirrors a representative Server API
// repo. id=42, project=ENG, slug=agent-workbench, public=true,
// has http clone link + self link with /browse suffix.
func sampleBitbucketServerRepo() BitbucketServerRepo {
	return BitbucketServerRepo{
		ID:            42,
		Name:          "Agent Workbench",
		Slug:          "agent-workbench",
		Public:        true,
		Archived:      false,
		DefaultBranch: pStr("main"),
		Project:       &BitbucketServerProj{Key: "ENG"},
		Links: &BitbucketServerLinks{
			Clone: []BitbucketServerLink{
				{Href: "ssh://git@bb.example.com/eng/agent-workbench.git", Name: "ssh"},
				{Href: "https://bb.example.com/scm/eng/agent-workbench.git", Name: "http"},
			},
			Self: []BitbucketServerLink{
				{Href: "https://bb.example.com/projects/ENG/repos/agent-workbench/browse"},
			},
		},
	}
}

// TestCreateBitbucketServerRepoRecord_HappyPath locks every field.
func TestCreateBitbucketServerRepoRecord_HappyPath(t *testing.T) {
	got, err := CreateBitbucketServerRepoRecord(CreateBitbucketServerRepoRecordInput{
		Repo:                      sampleBitbucketServerRepo(),
		HostURL:                   "https://bb.example.com",
		ServerPublicAccessEnabled: true,
		OrgID:                     17,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	want := RepoData{
		ExternalID:           "42",
		ExternalCodeHostType: "bitbucket-server",
		ExternalCodeHostURL:  "https://bb.example.com",
		// Clone URL = links.clone[name=="http"].
		CloneURL: "https://bb.example.com/scm/eng/agent-workbench.git",
		// Web URL = self[0].href with trailing /browse stripped.
		WebURL:        "https://bb.example.com/projects/ENG/repos/agent-workbench",
		Name:          "bb.example.com/ENG/agent-workbench",
		DisplayName:   "ENG/agent-workbench",
		DefaultBranch: pStr("main"),
		IsFork:        false,
		IsArchived:    false,
		IsPublic:      true,
		OrgID:         17,
		Metadata: RepoMetadata{
			GitConfig: map[string]string{
				"zoekt.web-url-type": "bitbucket-server",
				"zoekt.web-url":      "https://bb.example.com/projects/ENG/repos/agent-workbench",
				"zoekt.name":         "bb.example.com/ENG/agent-workbench",
				"zoekt.archived":     "0",
				"zoekt.fork":         "0",
				"zoekt.public":       "1",
				"zoekt.display-name": "ENG/agent-workbench",
			},
			CodeHostMetadata: &CodeHostMetadata{
				BitbucketServer: &BitbucketServerMetadata{
					ProjectKey: "ENG",
					RepoSlug:   "agent-workbench",
				},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drift:\n GOT:  %+v\nWANT: %+v", got, want)
	}
}

// TestCreateBitbucketServerRepoRecord_PublicAccessGate pins the
// cluster-flag gate. public=true on the repo + publicAccessEnabled
// = false (probe failed) -> isPublic surfaces as false.
func TestCreateBitbucketServerRepoRecord_PublicAccessGate(t *testing.T) {
	cases := []struct {
		name                 string
		repoPublic           bool
		publicAccessEnabled  bool
		wantPublic           bool
	}{
		{"both-true-is-public", true, true, true},
		{"repo-public-flag-off-gates-public", true, false, false},
		{"repo-not-public-is-not-public", false, true, false},
		{"both-off", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleBitbucketServerRepo()
			r.Public = tc.repoPublic
			got, _ := CreateBitbucketServerRepoRecord(CreateBitbucketServerRepoRecordInput{
				Repo:                      r,
				HostURL:                   "https://bb.example.com",
				ServerPublicAccessEnabled: tc.publicAccessEnabled,
			})
			if got.IsPublic != tc.wantPublic {
				t.Errorf("IsPublic: got %v want %v", got.IsPublic, tc.wantPublic)
			}
		})
	}
}

// TestCreateBitbucketServerRepoRecord_OriginIsFork pins
// `origin !== undefined` semantics for IsFork.
func TestCreateBitbucketServerRepoRecord_OriginIsFork(t *testing.T) {
	cases := []struct {
		name   string
		origin any
		want   bool
	}{
		{"nil-origin-not-fork", nil, false},
		{"non-nil-origin-fork", map[string]any{"id": 99}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleBitbucketServerRepo()
			r.Origin = tc.origin
			got, _ := CreateBitbucketServerRepoRecord(CreateBitbucketServerRepoRecordInput{
				Repo: r, HostURL: "https://bb.example.com",
			})
			if got.IsFork != tc.want {
				t.Errorf("IsFork: got %v want %v", got.IsFork, tc.want)
			}
		})
	}
}

// TestCreateBitbucketServerRepoRecord_MissingProjectErrors.
func TestCreateBitbucketServerRepoRecord_MissingProjectErrors(t *testing.T) {
	r := sampleBitbucketServerRepo()
	r.Project = nil
	_, err := CreateBitbucketServerRepoRecord(CreateBitbucketServerRepoRecordInput{
		Repo: r, HostURL: "https://bb.example.com",
	})
	if !errors.Is(err, ErrBitbucketServerMissingProject) {
		t.Errorf("got %v want ErrBitbucketServerMissingProject", err)
	}
}

// TestCreateBitbucketServerRepoRecord_MissingHTTPCloneLinkErrors.
func TestCreateBitbucketServerRepoRecord_MissingHTTPCloneLinkErrors(t *testing.T) {
	r := sampleBitbucketServerRepo()
	r.Links.Clone = []BitbucketServerLink{
		{Href: "ssh://only", Name: "ssh"},
	}
	_, err := CreateBitbucketServerRepoRecord(CreateBitbucketServerRepoRecordInput{
		Repo: r, HostURL: "https://bb.example.com",
	})
	if !errors.Is(err, ErrBitbucketServerMissingCloneHTTP) {
		t.Errorf("got %v want ErrBitbucketServerMissingCloneHTTP", err)
	}
}

// TestCreateBitbucketServerRepoRecord_BrowseSuffixStripped pins
// legacy line 478 - the self link's trailing /browse is removed
// so we can append /browse, /commits, etc. later.
func TestCreateBitbucketServerRepoRecord_BrowseSuffixStripped(t *testing.T) {
	cases := []struct {
		name   string
		selfHref string
		want   string
	}{
		{"with-trailing-browse", "https://bb.example.com/projects/X/repos/y/browse", "https://bb.example.com/projects/X/repos/y"},
		{"with-trailing-browse-slash", "https://bb.example.com/projects/X/repos/y/browse/", "https://bb.example.com/projects/X/repos/y"},
		{"no-browse-suffix", "https://bb.example.com/projects/X/repos/y", "https://bb.example.com/projects/X/repos/y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleBitbucketServerRepo()
			r.Links.Self = []BitbucketServerLink{{Href: tc.selfHref}}
			got, _ := CreateBitbucketServerRepoRecord(CreateBitbucketServerRepoRecordInput{
				Repo: r, HostURL: "https://bb.example.com",
			})
			if got.WebURL != tc.want {
				t.Errorf("WebURL: got %q want %q", got.WebURL, tc.want)
			}
		})
	}
}

// =====================================================================
// Server fetcher (HTTP)
// =====================================================================

// TestFetchReposForBitbucketServerProject_SinglePage_HTTP covers
// the happy path against an httptest server.
func TestFetchReposForBitbucketServerProject_SinglePage_HTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/1.0/projects/ENG/repos" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, `{
		  "values": [
		    {"id":42,"name":"Agent Workbench","slug":"agent-workbench","public":true,"archived":false,
		     "project":{"key":"ENG"},
		     "links":{"clone":[{"href":"https://x/scm/eng/agent-workbench.git","name":"http"}],
		              "self":[{"href":"https://x/projects/ENG/repos/agent-workbench/browse"}]}}
		  ],
		  "isLastPage": true
		}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForBitbucketServerProject(ctx, srv.Client(), srv.URL, "", "ENG")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if warn != nil {
		t.Errorf("got warning: %+v", warn)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos", len(repos))
	}
	if repos[0].ID != 42 || repos[0].Slug != "agent-workbench" {
		t.Errorf("repo: %+v", repos[0])
	}
}

// TestFetchReposForBitbucketServerProject_Pagination pins the
// start/limit + nextPageStart pagination loop.
func TestFetchReposForBitbucketServerProject_Pagination(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		start := r.URL.Query().Get("start")
		w.WriteHeader(200)
		switch start {
		case "", "0":
			fmt.Fprintf(w, `{"values":[{"id":1,"slug":"a","project":{"key":"ENG"},
			  "links":{"clone":[{"href":"x","name":"http"}],"self":[{"href":"y"}]}}],
			  "isLastPage":false,"nextPageStart":1}`)
		case "1":
			fmt.Fprintf(w, `{"values":[{"id":2,"slug":"b","project":{"key":"ENG"},
			  "links":{"clone":[{"href":"x","name":"http"}],"self":[{"href":"y"}]}}],
			  "isLastPage":true}`)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, _, err := FetchReposForBitbucketServerProject(ctx, srv.Client(), srv.URL, "", "ENG")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if calls != 2 {
		t.Errorf("API calls: %d want 2", calls)
	}
	if len(repos) != 2 || repos[1].Slug != "b" {
		t.Errorf("repos: %+v", repos)
	}
}

// TestFetchReposForBitbucketServerProject_404Warning.
func TestFetchReposForBitbucketServerProject_404Warning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"errors":[]}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	repos, warn, err := FetchReposForBitbucketServerProject(ctx, srv.Client(), srv.URL, "tok", "missing")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if repos != nil {
		t.Errorf("expected nil repos")
	}
	if warn == nil || warn.Message != "Project missing not found or no access" {
		t.Errorf("warning: %+v", warn)
	}
}

// TestFetchReposForBitbucketServerProject_AuthHeader confirms the
// Bearer token is attached when set.
func TestFetchReposForBitbucketServerProject_AuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"values":[],"isLastPage":true}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, _ = FetchReposForBitbucketServerProject(ctx, srv.Client(), srv.URL, "my-token", "ENG")
	if gotAuth != "Bearer my-token" {
		t.Errorf("Authorization header: got %q want %q", gotAuth, "Bearer my-token")
	}
}

// TestIsBitbucketServerPublicAccessEnabled_OK pins the probe's
// 200 -> true mapping.
func TestIsBitbucketServerPublicAccessEnabled_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Probe MUST NOT carry Authorization.
		if r.Header.Get("Authorization") != "" {
			t.Errorf("probe should not carry Authorization, got %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/rest/api/1.0/projects/ENG/repos/agent-workbench" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	got := IsBitbucketServerPublicAccessEnabled(context.Background(), srv.Client(), srv.URL, sampleBitbucketServerRepo())
	if !got {
		t.Errorf("expected true for 200 probe")
	}
}

// TestIsBitbucketServerPublicAccessEnabled_403 pins
// non-2xx -> false.
func TestIsBitbucketServerPublicAccessEnabled_403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	got := IsBitbucketServerPublicAccessEnabled(context.Background(), srv.Client(), srv.URL, sampleBitbucketServerRepo())
	if got {
		t.Errorf("expected false for 403 probe")
	}
}

// TestCompileBitbucketServerConfig_Loop pins the wrapper.
func TestCompileBitbucketServerConfig_Loop(t *testing.T) {
	records, warnings := CompileBitbucketServerConfig(
		[]BitbucketServerRepo{sampleBitbucketServerRepo()},
		GitHubCompileInput{HostURL: "https://bb.example.com"},
		88,
		true,
	)
	if len(records) != 1 {
		t.Fatalf("records: %d", len(records))
	}
	if records[0].ConnectionIDs == nil || records[0].ConnectionIDs[0] != 88 {
		t.Errorf("ConnectionIDs: %+v", records[0].ConnectionIDs)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %+v", warnings)
	}
}

// TestBitbucketServerJSONShape spot-checks the wire JSON.
func TestBitbucketServerJSONShape(t *testing.T) {
	got, _ := CreateBitbucketServerRepoRecord(CreateBitbucketServerRepoRecordInput{
		Repo:                      sampleBitbucketServerRepo(),
		HostURL:                   "https://bb.example.com",
		ServerPublicAccessEnabled: true,
	})
	js, _ := json.Marshal(got)
	for _, want := range []string{
		`"external_codeHostType":"bitbucket-server"`,
		`"projectKey":"ENG"`,
		`"repoSlug":"agent-workbench"`,
		`"zoekt.web-url-type":"bitbucket-server"`,
	} {
		if !contains(string(js), want) {
			t.Errorf("JSON missing %s\nfull: %s", want, js)
		}
	}
}
