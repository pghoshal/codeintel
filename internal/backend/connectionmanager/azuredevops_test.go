package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func sampleAzureDevOpsRepo() AzureDevOpsRepo {
	return AzureDevOpsRepo{
		ID:            "11112222-3333-4444-5555-666677778888",
		Name:          "agent-workbench",
		RemoteURL:     "https://dev.azure.com/example-org/AI/_git/agent-workbench",
		WebURL:        "https://dev.azure.com/example-org/AI/_git/agent-workbench",
		DefaultBranch: pStr("refs/heads/main"),
		IsFork:        false,
		Project: &AzureDevOpsProject{
			ID:         "p1",
			Name:       "AI",
			Visibility: "public",
		},
	}
}

// TestCreateAzureDevOpsRepoRecord_HappyPath locks every field.
func TestCreateAzureDevOpsRepoRecord_HappyPath(t *testing.T) {
	got, err := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
		Repo:    sampleAzureDevOpsRepo(),
		HostURL: "https://dev.azure.com",
		OrgID:   21,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	want := RepoData{
		ExternalID:           "11112222-3333-4444-5555-666677778888",
		ExternalCodeHostType: "azuredevops",
		ExternalCodeHostURL:  "https://dev.azure.com",
		// Legacy: cloneUrl == webUrl.
		CloneURL:    "https://dev.azure.com/example-org/AI/_git/agent-workbench",
		WebURL:      "https://dev.azure.com/example-org/AI/_git/agent-workbench",
		Name:        "dev.azure.com/AI/agent-workbench",
		DisplayName: "AI/agent-workbench",
		// No image URL surfaced (legacy null).
		ImageURL:      "",
		DefaultBranch: pStr("refs/heads/main"),
		IsFork:        false,
		IsArchived:    false,
		IsPublic:      true,
		OrgID:         21,
		Metadata: RepoMetadata{
			GitConfig: map[string]string{
				"zoekt.web-url-type": "azuredevops",
				"zoekt.web-url":      "https://dev.azure.com/example-org/AI/_git/agent-workbench",
				"zoekt.name":         "dev.azure.com/AI/agent-workbench",
				"zoekt.archived":     "0",
				"zoekt.fork":         "0",
				"zoekt.public":       "1",
				"zoekt.display-name": "AI/agent-workbench",
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("drift:\n GOT:  %+v\nWANT: %+v", got, want)
	}
}

// TestCreateAzureDevOpsRepoRecord_PrivateProjectIsNotPublic pins
// the visibility-driven IsPublic.
func TestCreateAzureDevOpsRepoRecord_PrivateProjectIsNotPublic(t *testing.T) {
	r := sampleAzureDevOpsRepo()
	r.Project.Visibility = "private"
	got, _ := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
		Repo: r, HostURL: "https://dev.azure.com",
	})
	if got.IsPublic {
		t.Errorf("IsPublic: got true, want false")
	}
}

// TestCreateAzureDevOpsRepoRecord_FallbackWebURL pins the
// constructed-when-absent webURL branch (legacy line 806).
func TestCreateAzureDevOpsRepoRecord_FallbackWebURL(t *testing.T) {
	r := sampleAzureDevOpsRepo()
	r.WebURL = "" // force fallback path
	got, err := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
		Repo: r, HostURL: "https://dev.azure.com",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := "https://dev.azure.com/AI/_git/agent-workbench"
	if got.WebURL != want {
		t.Errorf("WebURL: got %q want %q", got.WebURL, want)
	}
	// cloneUrl mirrors webUrl, so the fallback applies to both.
	if got.CloneURL != want {
		t.Errorf("CloneURL: got %q want %q", got.CloneURL, want)
	}
}

// TestCreateAzureDevOpsRepoRecord_ForkDetection.
func TestCreateAzureDevOpsRepoRecord_ForkDetection(t *testing.T) {
	cases := []struct {
		name  string
		isFork bool
	}{
		{"not-fork", false},
		{"is-fork", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleAzureDevOpsRepo()
			r.IsFork = tc.isFork
			got, _ := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
				Repo: r, HostURL: "https://dev.azure.com",
			})
			if got.IsFork != tc.isFork {
				t.Errorf("IsFork: got %v want %v", got.IsFork, tc.isFork)
			}
			wantZoekt := "0"
			if tc.isFork {
				wantZoekt = "1"
			}
			if got.Metadata.GitConfig["zoekt.fork"] != wantZoekt {
				t.Errorf("zoekt.fork: got %q want %q", got.Metadata.GitConfig["zoekt.fork"], wantZoekt)
			}
		})
	}
}

// TestCreateAzureDevOpsRepoRecord_MissingProjectErrors.
func TestCreateAzureDevOpsRepoRecord_MissingProjectErrors(t *testing.T) {
	r := sampleAzureDevOpsRepo()
	r.Project = nil
	_, err := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
		Repo: r, HostURL: "https://dev.azure.com",
	})
	if !errors.Is(err, ErrAzureDevOpsMissingProject) {
		t.Errorf("got %v want ErrAzureDevOpsMissingProject", err)
	}
}

// TestCreateAzureDevOpsRepoRecord_MissingRemoteURLErrors.
func TestCreateAzureDevOpsRepoRecord_MissingRemoteURLErrors(t *testing.T) {
	r := sampleAzureDevOpsRepo()
	r.RemoteURL = ""
	_, err := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
		Repo: r, HostURL: "https://dev.azure.com",
	})
	if !errors.Is(err, ErrAzureDevOpsMissingRemoteURL) {
		t.Errorf("got %v want ErrAzureDevOpsMissingRemoteURL", err)
	}
}

// TestCreateAzureDevOpsRepoRecord_MissingIDErrors.
func TestCreateAzureDevOpsRepoRecord_MissingIDErrors(t *testing.T) {
	r := sampleAzureDevOpsRepo()
	r.ID = ""
	_, err := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
		Repo: r, HostURL: "https://dev.azure.com",
	})
	if !errors.Is(err, ErrAzureDevOpsMissingID) {
		t.Errorf("got %v want ErrAzureDevOpsMissingID", err)
	}
}

// TestCompileAzureDevOpsConfig_DefaultsToCloud.
func TestCompileAzureDevOpsConfig_DefaultsToCloud(t *testing.T) {
	records, _ := CompileAzureDevOpsConfig(
		[]AzureDevOpsRepo{sampleAzureDevOpsRepo()},
		GitHubCompileInput{HostURL: ""},
		1,
	)
	if records[0].ExternalCodeHostURL != "https://dev.azure.com" {
		t.Errorf("ExternalCodeHostURL: got %q", records[0].ExternalCodeHostURL)
	}
}

// TestCompileAzureDevOpsConfig_SkipsErrorsAsWarnings.
func TestCompileAzureDevOpsConfig_SkipsErrorsAsWarnings(t *testing.T) {
	good := sampleAzureDevOpsRepo()
	bad := sampleAzureDevOpsRepo()
	bad.Project = nil
	bad.Name = "missing-project"

	records, warnings := CompileAzureDevOpsConfig(
		[]AzureDevOpsRepo{good, bad},
		GitHubCompileInput{HostURL: "https://dev.azure.com"},
		42,
	)
	if len(records) != 1 {
		t.Errorf("records: got %d want 1", len(records))
	}
	if len(warnings) != 1 {
		t.Errorf("warnings: got %d want 1: %v", len(warnings), warnings)
	}
	if len(warnings) > 0 && !strings.Contains(warnings[0], "missing-project") {
		t.Errorf("warning text: %q", warnings[0])
	}
}

// TestCompileAzureDevOpsFromConfig_NoFetcherErrors.
// After the SDK-fetcher init() registration, the default
// behaviour is to route through fetchAzureDevOpsReposViaHTTP.
// This test temporarily nils the fetcher to verify the
// ErrAzureDevOpsFetcherNotConfigured guard still works for
// callers that explicitly unregister.
func TestCompileAzureDevOpsFromConfig_NoFetcherErrors(t *testing.T) {
	SetAzureDevOpsFetcher(nil)
	defer SetAzureDevOpsFetcher(fetchAzureDevOpsReposViaHTTP)

	_, _, err := CompileAzureDevOpsFromConfig(context.Background(),
		AzureDevOpsConnectionConfig{}, 1)
	if !errors.Is(err, ErrAzureDevOpsFetcherNotConfigured) {
		t.Errorf("got %v want ErrAzureDevOpsFetcherNotConfigured", err)
	}
}

// TestCompileAzureDevOpsFromConfig_InjectedFetcher.
func TestCompileAzureDevOpsFromConfig_InjectedFetcher(t *testing.T) {
	SetAzureDevOpsFetcher(func(ctx context.Context, cfg AzureDevOpsConnectionConfig) (azureDevOpsFetchResult, error) {
		return azureDevOpsFetchResult{Repos: []AzureDevOpsRepo{sampleAzureDevOpsRepo()}}, nil
	})
	defer SetAzureDevOpsFetcher(nil)

	records, _, err := CompileAzureDevOpsFromConfig(context.Background(),
		AzureDevOpsConnectionConfig{URL: "https://dev.azure.com"}, 19)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records: got %d want 1", len(records))
	}
	if records[0].ConnectionIDs == nil || records[0].ConnectionIDs[0] != 19 {
		t.Errorf("ConnectionIDs: %+v", records[0].ConnectionIDs)
	}
}

// TestResolveAzureDevOpsHostURL.
func TestResolveAzureDevOpsHostURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  AzureDevOpsConnectionConfig
		want string
	}{
		{"empty-defaults", AzureDevOpsConnectionConfig{}, "https://dev.azure.com"},
		{"trailing-slash-stripped", AzureDevOpsConnectionConfig{URL: "https://dev.azure.com/"}, "https://dev.azure.com"},
		{"self-hosted", AzureDevOpsConnectionConfig{URL: "https://tfs.example.com/tfs"}, "https://tfs.example.com/tfs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveAzureDevOpsHostURL(tc.cfg); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestAzureDevOpsJSONShape spot-check.
func TestAzureDevOpsJSONShape(t *testing.T) {
	got, _ := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
		Repo: sampleAzureDevOpsRepo(), HostURL: "https://dev.azure.com",
	})
	js, _ := json.Marshal(got)
	for _, want := range []string{
		`"external_codeHostType":"azuredevops"`,
		`"zoekt.web-url-type":"azuredevops"`,
		`"isPublic":true`,
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON missing %s\nfull: %s", want, js)
		}
	}
}
