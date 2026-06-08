package connectionmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// sampleGerritProject mirrors a representative gerrit project.
func sampleGerritProject() GerritProject {
	return GerritProject{
		Name:  "platform/llvm/clang",
		ID:    "platform%2Fllvm%2Fclang",
		State: GerritProjectActive,
		WebLinks: []GerritWebLink{
			{Name: "gitiles", URL: "/plugins/gitiles/platform/llvm/clang"},
		},
	}
}

// TestCreateGerritRepoRecord_HappyPath locks every field
// including the gitiles-path web-URL prepend and the
// PathEscape of the project name in cloneURL.
func TestCreateGerritRepoRecord_HappyPath(t *testing.T) {
	got := CreateGerritRepoRecord(CreateGerritRepoRecordInput{
		Project:  sampleGerritProject(),
		HostURL:  "https://gerrit.example.com",
		Branches: []string{"main"},
		OrgID:    11,
	})

	want := RepoData{
		ExternalID:           "platform%2Fllvm%2Fclang",
		ExternalCodeHostType: "gerrit",
		ExternalCodeHostURL:  "https://gerrit.example.com",
		// path.PathEscape encodes "/" as "%2F".
		CloneURL: "https://gerrit.example.com/platform%2Fllvm%2Fclang",
		// gitiles path /plugins/gitiles/... prepended with host.
		WebURL:      "https://gerrit.example.com/plugins/gitiles/platform/llvm/clang",
		Name:        "gerrit.example.com/platform/llvm/clang",
		DisplayName: "platform/llvm/clang",
		IsFork:      false,
		IsArchived:  false,
		IsPublic:    true,
		OrgID:       11,
		Metadata: RepoMetadata{
			GitConfig: map[string]string{
				"zoekt.web-url-type": "gitiles", // NOT "gerrit"
				"zoekt.web-url":      "https://gerrit.example.com/plugins/gitiles/platform/llvm/clang",
				"zoekt.name":         "gerrit.example.com/platform/llvm/clang",
				"zoekt.archived":     "0",
				"zoekt.fork":         "0",
				"zoekt.public":       "1",
				"zoekt.display-name": "platform/llvm/clang",
			},
			Branches: []string{"main"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drift:\n GOT:  %+v\nWANT: %+v", got, want)
	}
}

// TestCreateGerritRepoRecord_WebURLNotGitiles pins the
// pass-through branch when the URL is not a /plugins/gitiles/
// path.
func TestCreateGerritRepoRecord_WebURLNotGitiles(t *testing.T) {
	p := sampleGerritProject()
	p.WebLinks = []GerritWebLink{{URL: "https://review.example.com/cgit/x"}}
	got := CreateGerritRepoRecord(CreateGerritRepoRecordInput{
		Project: p, HostURL: "https://gerrit.example.com",
	})
	if got.WebURL != "https://review.example.com/cgit/x" {
		t.Errorf("WebURL: got %q want absolute external pass-through", got.WebURL)
	}
}

// TestCreateGerritRepoRecord_NoWebLinksEmits empty
// zoekt.web-url + empty WebURL.
func TestCreateGerritRepoRecord_NoWebLinks(t *testing.T) {
	p := sampleGerritProject()
	p.WebLinks = nil
	got := CreateGerritRepoRecord(CreateGerritRepoRecordInput{
		Project: p, HostURL: "https://gerrit.example.com",
	})
	if got.WebURL != "" {
		t.Errorf("WebURL: got %q want empty", got.WebURL)
	}
	if got.Metadata.GitConfig["zoekt.web-url"] != "" {
		t.Errorf("zoekt.web-url: got %q want empty", got.Metadata.GitConfig["zoekt.web-url"])
	}
}

// TestCreateGerritRepoRecord_DefaultBranchIsNil pins the
// "intentionally nil" defaultBranch semantics.
func TestCreateGerritRepoRecord_DefaultBranchIsNil(t *testing.T) {
	got := CreateGerritRepoRecord(CreateGerritRepoRecordInput{
		Project: sampleGerritProject(), HostURL: "https://gerrit.example.com",
	})
	if got.DefaultBranch != nil {
		t.Errorf("DefaultBranch: got %v want nil", *got.DefaultBranch)
	}
}

// TestCompileGerritConfig_LoopBindsConnectionID pins the
// wrapper.
func TestCompileGerritConfig_LoopBindsConnectionID(t *testing.T) {
	got := CompileGerritConfig(
		[]GerritProject{sampleGerritProject()},
		GitHubCompileInput{HostURL: "https://gerrit.example.com"},
		99,
	)
	if len(got) != 1 || !reflect.DeepEqual(got[0].ConnectionIDs, []int32{99}) {
		t.Errorf("ConnectionIDs: %+v", got)
	}
}

// =====================================================================
// Live HTTP fetcher
// =====================================================================

// TestFetchAllGerritProjects_StripsXSSIPrefix is the load-bearing
// invariant: gerrit responses are prefixed with `)]}'\n` which
// must be removed before JSON parsing.
func TestFetchAllGerritProjects_StripsXSSIPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, ")]}'\n%s", `{
		  "platform/llvm/clang": {"id":"platform%2Fllvm%2Fclang","state":"ACTIVE"},
		  "android/kernel/common": {"id":"android%2Fkernel%2Fcommon","state":"ACTIVE"}
		}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := FetchAllGerritProjects(ctx, srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d projects want 2: %+v", len(got), got)
	}
	// names should be the JSON keys (legacy populates name from key).
	names := map[string]bool{got[0].Name: true, got[1].Name: true}
	if !names["platform/llvm/clang"] || !names["android/kernel/common"] {
		t.Errorf("project names: %+v", names)
	}
}

// TestFetchAllGerritProjects_Pagination pins the _more_projects
// loop + S=N offset bumping.
func TestFetchAllGerritProjects_Pagination(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		s := r.URL.Query().Get("S")
		switch s {
		case "", "0":
			w.WriteHeader(200)
			fmt.Fprintf(w, ")]}'\n%s", `{
			  "alpha": {"id":"alpha","_more_projects":true}
			}`)
		case "1":
			w.WriteHeader(200)
			fmt.Fprintf(w, ")]}'\n%s", `{
			  "beta": {"id":"beta"}
			}`)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := FetchAllGerritProjects(ctx, srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if calls != 2 {
		t.Errorf("calls: %d want 2", calls)
	}
	if len(got) != 2 {
		t.Fatalf("got %d projects", len(got))
	}
}

// TestFetchAllGerritProjects_Non2xxErrors pins HTTP-error
// surfacing.
func TestFetchAllGerritProjects_Non2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	_, err := FetchAllGerritProjects(context.Background(), srv.Client(), srv.URL)
	if err == nil {
		t.Errorf("expected error for 500")
	}
}

// TestCompileGerritFromConfig_RoundTripViaInjectedFetcher pins
// the orchestrator-level fetch+filter+compile flow.
func TestCompileGerritFromConfig_RoundTripViaInjectedFetcher(t *testing.T) {
	SetGerritFetcher(func(ctx context.Context, cfg GerritConnectionConfig) (gerritFetchResult, error) {
		return gerritFetchResult{
			Projects: []GerritProject{sampleGerritProject()},
		}, nil
	})
	defer SetGerritFetcher(fetchGerritProjectsViaHTTP) // restore default

	records, _, err := CompileGerritFromConfig(context.Background(),
		GerritConnectionConfig{URL: "https://gerrit.example.com"}, 17)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(records) != 1 || records[0].ConnectionIDs[0] != 17 {
		t.Errorf("records: %+v", records)
	}
}

// TestCompileGerritFromConfig_RequiresURL pins the URL-required
// guard (gerrit has no cloud default).
func TestCompileGerritFromConfig_RequiresURL(t *testing.T) {
	_, _, err := CompileGerritFromConfig(context.Background(), GerritConnectionConfig{}, 1)
	if err == nil {
		t.Errorf("expected error for empty URL")
	}
}

// TestGerritJSONShape spot-checks the wire format.
func TestGerritJSONShape(t *testing.T) {
	got := CreateGerritRepoRecord(CreateGerritRepoRecordInput{
		Project: sampleGerritProject(), HostURL: "https://gerrit.example.com",
	})
	js, _ := json.Marshal(got)
	for _, want := range []string{
		`"external_codeHostType":"gerrit"`,
		`"zoekt.web-url-type":"gitiles"`,
		`"isPublic":true`,
		`"isFork":false`,
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON missing %s\nfull: %s", want, js)
		}
	}
}
