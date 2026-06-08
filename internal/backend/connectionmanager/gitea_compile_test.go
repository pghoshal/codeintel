package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// sampleGiteaRepo mirrors a representative gitea API repo.
func sampleGiteaRepo() GiteaRepo {
	return GiteaRepo{
		ID:            777,
		Name:          "agent-workbench",
		FullName:      "example-org/agent-workbench",
		Fork:          false,
		Private:       false,
		Internal:      false,
		HTMLURL:       "https://gitea.example.com/example-org/agent-workbench",
		CloneURL:      pStr("https://internal-gitea/example-org/agent-workbench.git"),
		DefaultBranch: pStr("main"),
		Archived:      pBool(false),
		Owner:         &GiteaOwner{AvatarURL: "https://gitea.example.com/avatars/777"},
	}
}

// TestCreateGiteaRepoRecord_HappyPath locks every field.
func TestCreateGiteaRepoRecord_HappyPath(t *testing.T) {
	got := CreateGiteaRepoRecord(CreateGiteaRepoRecordInput{
		Repo:     sampleGiteaRepo(),
		HostURL:  "https://gitea.example.com",
		Branches: []string{"main"},
		OrgID:    9,
	})

	want := RepoData{
		ExternalID:           "777",
		ExternalCodeHostType: "gitea",
		ExternalCodeHostURL:  "https://gitea.example.com",
		// Host overlay: clone URL's internal-gitea host swapped
		// to the configured gitea.example.com host. Scheme is
		// preserved (legacy assigns host only, not scheme).
		CloneURL:    "https://gitea.example.com/example-org/agent-workbench.git",
		WebURL:      "https://gitea.example.com/example-org/agent-workbench",
		Name:        "gitea.example.com/example-org/agent-workbench",
		DisplayName: "example-org/agent-workbench",
		ImageURL:    "https://gitea.example.com/avatars/777",
		DefaultBranch: pStr("main"),
		IsFork:        false,
		IsArchived:    false,
		IsPublic:      true,
		OrgID:         9,
		Metadata: RepoMetadata{
			GitConfig: map[string]string{
				"zoekt.web-url-type":  "gitea",
				"zoekt.web-url":       "https://gitea.example.com/example-org/agent-workbench",
				"zoekt.name":          "gitea.example.com/example-org/agent-workbench",
				"zoekt.archived":      "0",
				"zoekt.fork":          "0",
				"zoekt.public":        "1",
				"zoekt.display-name":  "example-org/agent-workbench",
			},
			Branches: []string{"main"},
			// codeHostMetadata is NOT emitted for gitea (legacy
			// parity).
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("RepoData drift:\n GOT:  %+v\nWANT: %+v", got, want)
	}
}

// TestCreateGiteaRepoRecord_InternalIsNotPublic pins gitea's
// 3-tier visibility model: public OR internal OR private. Only
// (!internal && !private) is "public" in the codeintel sense.
func TestCreateGiteaRepoRecord_InternalIsNotPublic(t *testing.T) {
	cases := []struct {
		name     string
		internal bool
		private  bool
		want     bool
	}{
		{"public", false, false, true},
		{"internal-only", true, false, false},
		{"private-only", false, true, false},
		{"internal-and-private", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleGiteaRepo()
			r.Internal = tc.internal
			r.Private = tc.private
			got := CreateGiteaRepoRecord(CreateGiteaRepoRecordInput{
				Repo: r, HostURL: "https://gitea.example.com",
			})
			if got.IsPublic != tc.want {
				t.Errorf("IsPublic: got %v want %v", got.IsPublic, tc.want)
			}
		})
	}
}

// TestCreateGiteaRepoRecord_HostOverlay pins the load-bearing
// clone-URL host swap. The API may return an internal host
// behind a proxy; the codeintel-emitted clone URL must carry
// the operator-facing hostUrl's host.
func TestCreateGiteaRepoRecord_HostOverlay(t *testing.T) {
	cases := []struct {
		name  string
		clone string
		host  string
		want  string
	}{
		{
			"swaps-internal-host-for-configured",
			"https://10.0.0.5:3000/x/y.git",
			"https://gitea.example.com",
			"https://gitea.example.com/x/y.git",
		},
		{
			"preserves-scheme-from-clone-not-host",
			"http://internal:3000/x/y.git",
			"https://gitea.example.com",
			"http://gitea.example.com/x/y.git",
		},
		{
			"matching-hosts-pass-through",
			"https://gitea.example.com/x/y.git",
			"https://gitea.example.com",
			"https://gitea.example.com/x/y.git",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleGiteaRepo()
			r.CloneURL = pStr(tc.clone)
			got := CreateGiteaRepoRecord(CreateGiteaRepoRecordInput{
				Repo: r, HostURL: tc.host,
			})
			if got.CloneURL != tc.want {
				t.Errorf("CloneURL: got %q want %q", got.CloneURL, tc.want)
			}
		})
	}
}

// TestCreateGiteaRepoRecord_NoOwnerEmitsEmptyAvatar pins the
// missing-owner edge case. Legacy line 282 uses optional
// chaining: `repo.owner?.avatar_url`. Missing owner → empty
// imageURL.
func TestCreateGiteaRepoRecord_NoOwnerEmitsEmptyAvatar(t *testing.T) {
	r := sampleGiteaRepo()
	r.Owner = nil
	got := CreateGiteaRepoRecord(CreateGiteaRepoRecordInput{
		Repo: r, HostURL: "https://gitea.example.com",
	})
	if got.ImageURL != "" {
		t.Errorf("ImageURL: got %q want empty", got.ImageURL)
	}
}

// TestCompileGiteaConfig_LoopBindsConnectionID pins the loop
// wrapper.
func TestCompileGiteaConfig_LoopBindsConnectionID(t *testing.T) {
	got := CompileGiteaConfig(
		[]GiteaRepo{sampleGiteaRepo()},
		GitHubCompileInput{HostURL: "https://gitea.example.com"},
		33,
	)
	if len(got) != 1 || !reflect.DeepEqual(got[0].ConnectionIDs, []int32{33}) {
		t.Errorf("ConnectionIDs: %+v", got)
	}
}

// TestCompileGiteaConfig_DefaultsToGiteaCom pins the
// (config.url ?? 'https://gitea.com') fallback.
func TestCompileGiteaConfig_DefaultsToGiteaCom(t *testing.T) {
	got := CompileGiteaConfig(
		[]GiteaRepo{sampleGiteaRepo()},
		GitHubCompileInput{HostURL: ""},
		1,
	)
	if got[0].ExternalCodeHostURL != "https://gitea.com" {
		t.Errorf("ExternalCodeHostURL: got %q", got[0].ExternalCodeHostURL)
	}
}

// TestGiteaRepoData_JSONShape spot-checks JSON wire format.
func TestGiteaRepoData_JSONShape(t *testing.T) {
	got := CreateGiteaRepoRecord(CreateGiteaRepoRecordInput{
		Repo:    sampleGiteaRepo(),
		HostURL: "https://gitea.example.com",
		OrgID:   1,
	})
	js, _ := json.Marshal(got)
	for _, want := range []string{
		`"external_codeHostType":"gitea"`,
		`"zoekt.web-url-type":"gitea"`,
		`"isPublic":true`,
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON missing %s\nfull: %s", want, js)
		}
	}
}

// TestCompileGiteaFromConfig_NoFetcherErrors pins the typed
// sentinel returned when the fetcher hasn't been wired.
func TestCompileGiteaFromConfig_NoFetcherErrors(t *testing.T) {
	// Reset to nil so this test isolates the unconfigured branch.
	SetGiteaFetcher(nil)
	defer SetGiteaFetcher(nil)

	_, _, err := CompileGiteaFromConfig(context.Background(), GiteaConnectionConfig{}, 1)
	if !errors.Is(err, ErrGiteaFetcherNotConfigured) {
		t.Errorf("expected ErrGiteaFetcherNotConfigured, got %v", err)
	}
}

// TestCompileGiteaFromConfig_FetcherInjected pins the
// register-then-compile flow. With a fake fetcher that returns
// one GiteaRepo, the compile path yields one RepoData.
func TestCompileGiteaFromConfig_FetcherInjected(t *testing.T) {
	SetGiteaFetcher(func(ctx context.Context, cfg GiteaConnectionConfig) (giteaFetchResult, error) {
		return giteaFetchResult{
			Repos: []GiteaRepo{sampleGiteaRepo()},
		}, nil
	})
	defer SetGiteaFetcher(nil)

	records, _, err := CompileGiteaFromConfig(context.Background(),
		GiteaConnectionConfig{URL: "https://gitea.example.com"}, 5)
	if err != nil {
		t.Fatalf("CompileGiteaFromConfig: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 record, got %d", len(records))
	}
	if records[0].ConnectionIDs == nil || records[0].ConnectionIDs[0] != 5 {
		t.Errorf("ConnectionIDs: %+v", records[0].ConnectionIDs)
	}
}
