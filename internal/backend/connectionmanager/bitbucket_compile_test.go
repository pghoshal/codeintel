package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// sampleBitbucketCloudRepo mirrors a representative API response.
func sampleBitbucketCloudRepo() BitbucketCloudRepo {
	return BitbucketCloudRepo{
		UUID:      "{12345678-1234-1234-1234-123456789abc}",
		Name:      "agent-workbench",
		FullName:  "example-org/agent-workbench",
		IsPrivate: false,
		Project:   &BitbucketCloudProject{Key: "AI"},
		Mainbranch: &BitbucketCloudBranch{Name: "main"},
		Links: &BitbucketCloudLinks{
			HTML: &BitbucketCloudHref{Href: "https://bitbucket.org/example-org/agent-workbench"},
			Clone: []BitbucketCloudClone{
				{Href: "https://bitbucket.org/example-org/agent-workbench.git", Name: "https"},
			},
		},
	}
}

// TestCreateBitbucketCloudRepoRecord_HappyPath locks every field.
func TestCreateBitbucketCloudRepoRecord_HappyPath(t *testing.T) {
	got, err := CreateBitbucketCloudRepoRecord(CreateBitbucketCloudRepoRecordInput{
		Repo:    sampleBitbucketCloudRepo(),
		HostURL: "https://bitbucket.org",
		OrgID:   13,
	})
	if err != nil {
		t.Fatalf("CreateBitbucketCloudRepoRecord: %v", err)
	}

	wantDisplay := "example-org/AI/agent-workbench"
	want := RepoData{
		ExternalID:           "{12345678-1234-1234-1234-123456789abc}",
		ExternalCodeHostType: "bitbucket-cloud",
		ExternalCodeHostURL:  "https://bitbucket.org",
		CloneURL:             "https://bitbucket.org/example-org/agent-workbench",
		WebURL:               "https://bitbucket.org/example-org/agent-workbench",
		Name:                 "bitbucket.org/" + wantDisplay,
		DisplayName:          wantDisplay,
		DefaultBranch:        pStr("main"),
		IsFork:               false,
		IsArchived:           false, // cloud never archived in this slice
		IsPublic:             true,
		OrgID:                13,
		Metadata: RepoMetadata{
			GitConfig: map[string]string{
				"zoekt.web-url-type": "bitbucket-cloud",
				"zoekt.web-url":      "https://bitbucket.org/example-org/agent-workbench",
				"zoekt.name":         "bitbucket.org/" + wantDisplay,
				"zoekt.archived":     "0",
				"zoekt.fork":         "0",
				"zoekt.public":       "1",
				"zoekt.display-name": wantDisplay,
			},
			CodeHostMetadata: &CodeHostMetadata{
				BitbucketCloud: &BitbucketCloudMetadata{
					Workspace: "example-org",
					RepoSlug:  "agent-workbench",
				},
			},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("drift:\n GOT:  %+v\nWANT: %+v", got, want)
	}
}

// TestCreateBitbucketCloudRepoRecord_PrivateRepoIsPublicFalse
// pins isPublic = !is_private.
func TestCreateBitbucketCloudRepoRecord_PrivateRepoIsPublicFalse(t *testing.T) {
	r := sampleBitbucketCloudRepo()
	r.IsPrivate = true
	got, err := CreateBitbucketCloudRepoRecord(CreateBitbucketCloudRepoRecordInput{
		Repo: r, HostURL: "https://bitbucket.org",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got.IsPublic {
		t.Errorf("IsPublic: got true, want false")
	}
}

// TestCreateBitbucketCloudRepoRecord_ForkDetection pins
// `parent !== undefined` mapping to IsFork via Parent != nil.
func TestCreateBitbucketCloudRepoRecord_ForkDetection(t *testing.T) {
	cases := []struct {
		name   string
		parent any
		want   bool
	}{
		{"nil-parent-not-fork", nil, false},
		{"non-nil-parent-fork", map[string]any{"name": "upstream"}, true},
		{"empty-struct-parent-fork", struct{}{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleBitbucketCloudRepo()
			r.Parent = tc.parent
			got, _ := CreateBitbucketCloudRepoRecord(CreateBitbucketCloudRepoRecordInput{
				Repo: r, HostURL: "https://bitbucket.org",
			})
			if got.IsFork != tc.want {
				t.Errorf("IsFork: got %v want %v", got.IsFork, tc.want)
			}
		})
	}
}

// TestCreateBitbucketCloudRepoRecord_MissingProjectFallsBackToUnknown
// pins legacy line 498: project key defaults to "unknown".
func TestCreateBitbucketCloudRepoRecord_MissingProjectFallsBackToUnknown(t *testing.T) {
	r := sampleBitbucketCloudRepo()
	r.Project = nil
	got, _ := CreateBitbucketCloudRepoRecord(CreateBitbucketCloudRepoRecordInput{
		Repo: r, HostURL: "https://bitbucket.org",
	})
	if got.DisplayName != "example-org/unknown/agent-workbench" {
		t.Errorf("DisplayName: got %q want example-org/unknown/agent-workbench", got.DisplayName)
	}
}

// TestCreateBitbucketCloudRepoRecord_MissingFullNameErrors pins
// the typed error for the legacy "missing full_name" throw.
func TestCreateBitbucketCloudRepoRecord_MissingFullNameErrors(t *testing.T) {
	r := sampleBitbucketCloudRepo()
	r.FullName = ""
	_, err := CreateBitbucketCloudRepoRecord(CreateBitbucketCloudRepoRecordInput{
		Repo: r, HostURL: "https://bitbucket.org",
	})
	if !errors.Is(err, ErrBitbucketCloudMissingFullName) {
		t.Errorf("expected ErrBitbucketCloudMissingFullName, got %v", err)
	}
}

// TestCreateBitbucketCloudRepoRecord_MissingLinksErrors pins
// the missing-links error path.
func TestCreateBitbucketCloudRepoRecord_MissingLinksErrors(t *testing.T) {
	r := sampleBitbucketCloudRepo()
	r.Links = nil
	_, err := CreateBitbucketCloudRepoRecord(CreateBitbucketCloudRepoRecordInput{
		Repo: r, HostURL: "https://bitbucket.org",
	})
	if !errors.Is(err, ErrBitbucketCloudMissingLinks) {
		t.Errorf("expected ErrBitbucketCloudMissingLinks, got %v", err)
	}
}

// TestCompileBitbucketCloudConfig_SkipsErrorsAsWarnings: legacy
// throws per-repo errors; codeintel surfaces them as warnings
// + drops the row rather than failing the batch. Validates two
// good repos + one missing-full_name produces 2 records + 1
// warning.
func TestCompileBitbucketCloudConfig_SkipsErrorsAsWarnings(t *testing.T) {
	good := sampleBitbucketCloudRepo()
	good2 := sampleBitbucketCloudRepo()
	good2.UUID = "{2222}"
	good2.FullName = "example-org/other"
	bad := sampleBitbucketCloudRepo()
	bad.FullName = ""
	bad.UUID = "{3333}"

	records, warnings := CompileBitbucketCloudConfig(
		[]BitbucketCloudRepo{good, bad, good2},
		GitHubCompileInput{HostURL: "https://bitbucket.org"},
		42,
	)
	if len(records) != 2 {
		t.Errorf("expected 2 records, got %d", len(records))
	}
	if len(warnings) != 1 {
		t.Errorf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if len(warnings) > 0 && !strings.Contains(warnings[0], "missing full_name") {
		t.Errorf("warning text: %q", warnings[0])
	}
}

// TestCompileBitbucketCloudConfig_DefaultsToBitbucketOrg pins
// (config.url ?? 'https://bitbucket.org') fallback.
func TestCompileBitbucketCloudConfig_DefaultsToBitbucketOrg(t *testing.T) {
	records, _ := CompileBitbucketCloudConfig(
		[]BitbucketCloudRepo{sampleBitbucketCloudRepo()},
		GitHubCompileInput{HostURL: ""},
		1,
	)
	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
	if records[0].ExternalCodeHostURL != "https://bitbucket.org" {
		t.Errorf("ExternalCodeHostURL: got %q", records[0].ExternalCodeHostURL)
	}
}

// TestBitbucketCloudRepoData_JSONShape spot-checks JSON.
func TestBitbucketCloudRepoData_JSONShape(t *testing.T) {
	got, _ := CreateBitbucketCloudRepoRecord(CreateBitbucketCloudRepoRecordInput{
		Repo:    sampleBitbucketCloudRepo(),
		HostURL: "https://bitbucket.org",
		OrgID:   1,
	})
	js, _ := json.Marshal(got)
	for _, want := range []string{
		`"external_codeHostType":"bitbucket-cloud"`,
		`"workspace":"example-org"`,
		`"repoSlug":"agent-workbench"`,
		`"zoekt.web-url-type":"bitbucket-cloud"`,
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON missing %s\nfull: %s", want, js)
		}
	}
}

// TestCompileBitbucketFromConfig_NoFetcher pins
// ErrBitbucketFetcherNotConfigured for an unwired call.
func TestCompileBitbucketFromConfig_NoFetcher(t *testing.T) {
	SetBitbucketCloudFetcher(nil)
	defer SetBitbucketCloudFetcher(nil)

	_, _, err := CompileBitbucketFromConfig(context.Background(),
		BitbucketConnectionConfig{DeploymentType: BitbucketDeploymentCloud}, 1)
	if !errors.Is(err, ErrBitbucketFetcherNotConfigured) {
		t.Errorf("expected ErrBitbucketFetcherNotConfigured, got %v", err)
	}
}

// TestCompileBitbucketFromConfig_ServerBranchRoutes confirms
// the server branch is reachable after B.7-iii landed the
// server port. The default fetcher hits the empty Projects
// list (zero loops -> empty result) so this exercises the
// non-error path.
func TestCompileBitbucketFromConfig_ServerBranchRoutes(t *testing.T) {
	// init() in bitbucket_server.go registers the default
	// HTTP fetcher; with no Projects in the config it returns
	// an empty result rather than a fetch error.
	records, _, err := CompileBitbucketFromConfig(context.Background(),
		BitbucketConnectionConfig{
			DeploymentType: BitbucketDeploymentServer,
			URL:            "https://bb.example.com",
			Projects:       nil,
		}, 1)
	if err != nil {
		t.Fatalf("server branch: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("expected 0 records (no projects), got %d", len(records))
	}
}

// TestCompileBitbucketFromConfig_InjectedFetcher pins the
// register-then-compile flow.
func TestCompileBitbucketFromConfig_InjectedFetcher(t *testing.T) {
	SetBitbucketCloudFetcher(func(ctx context.Context, cfg BitbucketConnectionConfig) (bitbucketCloudFetchResult, error) {
		return bitbucketCloudFetchResult{Repos: []BitbucketCloudRepo{sampleBitbucketCloudRepo()}}, nil
	})
	defer SetBitbucketCloudFetcher(nil)

	records, _, err := CompileBitbucketFromConfig(context.Background(),
		BitbucketConnectionConfig{DeploymentType: BitbucketDeploymentCloud, URL: "https://bitbucket.org"}, 7)
	if err != nil {
		t.Fatalf("CompileBitbucketFromConfig: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].ConnectionIDs == nil || records[0].ConnectionIDs[0] != 7 {
		t.Errorf("ConnectionIDs: %+v", records[0].ConnectionIDs)
	}
}
