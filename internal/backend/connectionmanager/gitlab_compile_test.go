package connectionmanager

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// sampleGitLabProject mirrors a representative GitLab project
// response. Fields chosen to exercise the per-project transform:
// nested namespace path, optional avatar, count fields, topics,
// archived flag, fork detection via forked_from_project.
func sampleGitLabProject() GitLabProject {
	return GitLabProject{
		ID:                123,
		Name:              "agent-workbench",
		PathWithNamespace: "example-org/agent-workbench",
		HTTPURLToRepo:     "http://gitlab.example.com/example-org/agent-workbench.git",
		DefaultBranch:     pStr("main"),
		Visibility:        "public",
		Archived:          pBool(false),
		Topics:            []string{"ai", "code"},
		StargazersCount:   pInt(42),
		ForksCount:        pInt(3),
		AvatarURL:         pStr("https://gitlab.example.com/uploads/-/system/project/avatar/123/avatar.png"),
	}
}

// TestCreateGitLabRepoRecord_HappyPath locks every field of the
// emitted RepoData against the hand-derived expected output.
// Byte-equal port-parity assertion.
func TestCreateGitLabRepoRecord_HappyPath(t *testing.T) {
	project := sampleGitLabProject()
	got := CreateGitLabRepoRecord(CreateGitLabRepoRecordInput{
		Project:  project,
		HostURL:  "https://gitlab.example.com",
		Branches: []string{"main"},
		Tags:     []string{"v*"},
		OrgID:    7,
	})

	want := RepoData{
		ExternalID:           "123",
		ExternalCodeHostType: "gitlab",
		ExternalCodeHostURL:  "https://gitlab.example.com",
		// Protocol overlay: http_url_to_repo started with http://,
		// hostURL is https://, so cloneURL gets the https scheme.
		CloneURL:    "https://gitlab.example.com/example-org/agent-workbench.git",
		WebURL:      "https://gitlab.example.com/example-org/agent-workbench",
		Name:        "gitlab.example.com/example-org/agent-workbench",
		DisplayName: "example-org/agent-workbench",
		// Avatar URL is the /api/v4 endpoint, not the raw avatar_url
		// (per legacy line 191-193).
		ImageURL:      "https://gitlab.example.com/api/v4/projects/123/avatar",
		DefaultBranch: pStr("main"),
		IsFork:        false,
		IsArchived:    false,
		IsPublic:      true,
		OrgID:         7,
		Metadata: RepoMetadata{
			GitConfig: map[string]string{
				"zoekt.web-url-type":  "gitlab",
				"zoekt.web-url":       "https://gitlab.example.com/example-org/agent-workbench",
				"zoekt.name":          "gitlab.example.com/example-org/agent-workbench",
				"zoekt.gitlab-stars":  "42",
				"zoekt.gitlab-forks":  "3",
				"zoekt.archived":      "0",
				"zoekt.fork":          "0",
				"zoekt.public":        "1",
				"zoekt.display-name":  "example-org/agent-workbench",
			},
			Branches: []string{"main"},
			Tags:     []string{"v*"},
			CodeHostMetadata: &CodeHostMetadata{
				GitLab: &GitLabMetadata{Topics: []string{"ai", "code"}},
			},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("RepoData drift:\n GOT:  %+v\nWANT: %+v", got, want)
	}
}

// TestCreateGitLabRepoRecord_InternalIsPublic pins the
// legacy `visibility === 'internal'` branch: internal repos
// surface as IsPublic=true (legacy lines 185-187 with the
// permission-filtering rationale).
func TestCreateGitLabRepoRecord_InternalIsPublic(t *testing.T) {
	cases := []struct {
		visibility string
		wantPublic bool
	}{
		{"public", true},
		{"internal", true},
		{"private", false},
	}
	for _, tc := range cases {
		t.Run(tc.visibility, func(t *testing.T) {
			p := sampleGitLabProject()
			p.Visibility = tc.visibility
			got := CreateGitLabRepoRecord(CreateGitLabRepoRecordInput{
				Project: p, HostURL: "https://gitlab.example.com",
			})
			if got.IsPublic != tc.wantPublic {
				t.Errorf("visibility %q: IsPublic=%v want=%v", tc.visibility, got.IsPublic, tc.wantPublic)
			}
		})
	}
}

// TestCreateGitLabRepoRecord_ForkedDetection pins the
// `project.forked_from_project !== undefined` branch.
// Presence of any value (even empty struct) -> IsFork=true.
func TestCreateGitLabRepoRecord_ForkedDetection(t *testing.T) {
	cases := []struct {
		name     string
		forkInfo any
		wantFork bool
	}{
		{"nil-is-not-fork", nil, false},
		{"empty-struct-is-fork", struct{}{}, true},
		{"map-is-fork", map[string]any{"id": 999}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sampleGitLabProject()
			p.ForkedFromProject = tc.forkInfo
			got := CreateGitLabRepoRecord(CreateGitLabRepoRecordInput{
				Project: p, HostURL: "https://gitlab.example.com",
			})
			if got.IsFork != tc.wantFork {
				t.Errorf("IsFork: got %v want %v", got.IsFork, tc.wantFork)
			}
		})
	}
}

// TestCreateGitLabRepoRecord_AvatarURL pins the avatar derivation
// branch: when project.avatar_url is set, emit the /api/v4
// endpoint pointer; when nil/empty, emit empty string.
func TestCreateGitLabRepoRecord_AvatarURL(t *testing.T) {
	t.Run("non-nil-avatar-emits-api-endpoint", func(t *testing.T) {
		p := sampleGitLabProject()
		p.AvatarURL = pStr("not-empty")
		got := CreateGitLabRepoRecord(CreateGitLabRepoRecordInput{
			Project: p, HostURL: "https://gitlab.example.com",
		})
		if got.ImageURL != "https://gitlab.example.com/api/v4/projects/123/avatar" {
			t.Errorf("ImageURL: got %q", got.ImageURL)
		}
	})
	t.Run("nil-avatar-emits-empty", func(t *testing.T) {
		p := sampleGitLabProject()
		p.AvatarURL = nil
		got := CreateGitLabRepoRecord(CreateGitLabRepoRecordInput{
			Project: p, HostURL: "https://gitlab.example.com",
		})
		if got.ImageURL != "" {
			t.Errorf("ImageURL: got %q want empty", got.ImageURL)
		}
	})
}

// TestCreateGitLabRepoRecord_ProtocolOverlay pins the load-bearing
// scheme-swap: legacy `cloneUrl.protocol = new URL(hostUrl).protocol`.
// GitLab's API often returns http:// for http_url_to_repo even when
// the host serves https://; the emitter must overlay the host's
// scheme.
func TestCreateGitLabRepoRecord_ProtocolOverlay(t *testing.T) {
	cases := []struct {
		name    string
		clone   string
		host    string
		want    string
	}{
		{"http-clone-https-host", "http://gitlab.example.com/x/y.git", "https://gitlab.example.com", "https://gitlab.example.com/x/y.git"},
		{"https-clone-http-host", "https://gitlab.example.com/x/y.git", "http://gitlab.example.com", "http://gitlab.example.com/x/y.git"},
		{"matching-schemes", "https://gitlab.example.com/x/y.git", "https://gitlab.example.com", "https://gitlab.example.com/x/y.git"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := sampleGitLabProject()
			p.HTTPURLToRepo = tc.clone
			got := CreateGitLabRepoRecord(CreateGitLabRepoRecordInput{
				Project: p, HostURL: tc.host,
			})
			if got.CloneURL != tc.want {
				t.Errorf("CloneURL: got %q want %q", got.CloneURL, tc.want)
			}
		})
	}
}

// TestCompileGitLabConfig_LoopBindsConnectionID pins the per-
// project loop wrapper.
func TestCompileGitLabConfig_LoopBindsConnectionID(t *testing.T) {
	got := CompileGitLabConfig(
		[]GitLabProject{sampleGitLabProject()},
		GitHubCompileInput{HostURL: "https://gitlab.example.com"},
		88,
	)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0].ConnectionIDs, []int32{88}) {
		t.Errorf("ConnectionIDs: got %v want [88]", got[0].ConnectionIDs)
	}
}

// TestCompileGitLabConfig_DefaultsToGitlabCom pins the
// (config.url ?? 'https://gitlab.com') legacy fallback.
func TestCompileGitLabConfig_DefaultsToGitlabCom(t *testing.T) {
	got := CompileGitLabConfig(
		[]GitLabProject{sampleGitLabProject()},
		GitHubCompileInput{HostURL: ""},
		1,
	)
	if got[0].ExternalCodeHostURL != "https://gitlab.com" {
		t.Errorf("ExternalCodeHostURL: got %q want https://gitlab.com", got[0].ExternalCodeHostURL)
	}
}

// TestGitLabRepoData_JSONShape: spot-check the JSON serialization.
// Important keys + values, not full struct equality.
func TestGitLabRepoData_JSONShape(t *testing.T) {
	got := CreateGitLabRepoRecord(CreateGitLabRepoRecordInput{
		Project: sampleGitLabProject(),
		HostURL: "https://gitlab.example.com",
		OrgID:   1,
	})
	js, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"external_id":"123"`,
		`"external_codeHostType":"gitlab"`,
		`"webUrl":"https://gitlab.example.com/example-org/agent-workbench"`,
		`"zoekt.web-url-type":"gitlab"`,
		`"zoekt.gitlab-stars":"42"`,
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON missing %q\nfull: %s", want, js)
		}
	}
}
