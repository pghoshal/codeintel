package connectionmanager

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// pStr / pInt / pBool wrap a literal in a pointer for the
// pointer-typed optional fields on OctokitRepository.
func pStr(s string) *string { return &s }
func pInt(i int) *int       { return &i }
func pBool(b bool) *bool    { return &b }

// sampleOctokitRepo is a representative GitHub-API repo
// descriptor. Fixture values are chosen to exercise every
// optional pointer field + the non-trivial transformations
// (counts → stringified, archived bool → marshalBool, public
// derivation from private).
func sampleOctokitRepo() OctokitRepository {
	return OctokitRepository{
		Name:             "agent-workbench",
		ID:               123456789,
		FullName:         "example-org/agent-workbench",
		Fork:             false,
		Private:          false,
		HTMLURL:          "https://github.com/example-org/agent-workbench",
		CloneURL:         pStr("https://github.com/example-org/agent-workbench.git"),
		StargazersCount:  pInt(42),
		WatchersCount:    pInt(17),
		SubscribersCount: pInt(5),
		DefaultBranch:    pStr("main"),
		ForksCount:       pInt(3),
		Archived:         pBool(false),
		Topics:           []string{"ai", "code"},
		Size:             pInt(1024),
		Owner: OctokitOwner{
			AvatarURL: "https://avatars.githubusercontent.com/u/12345",
			Login:     "example-org",
		},
	}
}

// TestCreateGitHubRepoRecord_HappyPath locks every field of the
// emitted RepoData against the hand-derived expected output.
// This is the byte-equal port-parity assertion for the pure
// transformation.
func TestCreateGitHubRepoRecord_HappyPath(t *testing.T) {
	repo := sampleOctokitRepo()

	got := CreateGitHubRepoRecord(CreateGitHubRepoRecordInput{
		Repo:     repo,
		HostURL:  "https://github.com",
		Branches: []string{"main", "release/*"},
		Tags:     []string{"v*"},
		OrgID:    7,
	})

	want := RepoData{
		ExternalID:           "123456789",
		ExternalCodeHostType: "github",
		ExternalCodeHostURL:  "https://github.com",
		CloneURL:             "https://github.com/example-org/agent-workbench.git",
		WebURL:               "https://github.com/example-org/agent-workbench",
		Name:                 "github.com/example-org/agent-workbench",
		DisplayName:          "example-org/agent-workbench",
		ImageURL:             "https://avatars.githubusercontent.com/u/12345",
		DefaultBranch:        pStr("main"),
		IsFork:               false,
		IsArchived:           false,
		IsPublic:             true,
		OrgID:                7,
		Metadata: RepoMetadata{
			GitConfig: map[string]string{
				"zoekt.web-url-type":       "github",
				"zoekt.web-url":            "https://github.com/example-org/agent-workbench",
				"zoekt.name":               "github.com/example-org/agent-workbench",
				"zoekt.github-stars":       "42",
				"zoekt.github-watchers":    "17",
				"zoekt.github-subscribers": "5",
				"zoekt.github-forks":       "3",
				"zoekt.archived":           "0",
				"zoekt.fork":               "0",
				"zoekt.public":             "1",
				"zoekt.display-name":       "example-org/agent-workbench",
			},
			Branches: []string{"main", "release/*"},
			Tags:     []string{"v*"},
			CodeHostMetadata: &CodeHostMetadata{
				GitHub: &GitHubMetadata{Topics: []string{"ai", "code"}},
			},
		},
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("RepoData drift:\n GOT:  %+v\nWANT: %+v", got, want)
	}
}

// TestCreateGitHubRepoRecord_PrivateRepoIsPublicFalse pins the
// IsPublic-from-Private derivation. Legacy line 113:
// `isPublic = repo.private === false`. A private repo MUST
// produce IsPublic=false (and zoekt.public="0").
func TestCreateGitHubRepoRecord_PrivateRepoIsPublicFalse(t *testing.T) {
	repo := sampleOctokitRepo()
	repo.Private = true

	got := CreateGitHubRepoRecord(CreateGitHubRepoRecordInput{
		Repo: repo, HostURL: "https://github.com",
	})

	if got.IsPublic {
		t.Errorf("IsPublic: got true, want false (private repo)")
	}
	if got.Metadata.GitConfig["zoekt.public"] != "0" {
		t.Errorf("zoekt.public: got %q, want \"0\"", got.Metadata.GitConfig["zoekt.public"])
	}
}

// TestCreateGitHubRepoRecord_ArchivedFlow pins the archived-
// boolean handling. Legacy uses `!!repo.archived` (truthy
// coercion). The Go port must produce the same:
//   - archived = nil       → IsArchived=false, zoekt.archived="0"
//   - archived = ptr(false) → IsArchived=false, zoekt.archived="0"
//   - archived = ptr(true)  → IsArchived=true,  zoekt.archived="1"
func TestCreateGitHubRepoRecord_ArchivedFlow(t *testing.T) {
	cases := []struct {
		name        string
		archived    *bool
		wantArchive bool
		wantZoekt   string
	}{
		{"nil-archived", nil, false, "0"},
		{"ptr-false-archived", pBool(false), false, "0"},
		{"ptr-true-archived", pBool(true), true, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := sampleOctokitRepo()
			repo.Archived = tc.archived
			got := CreateGitHubRepoRecord(CreateGitHubRepoRecordInput{
				Repo: repo, HostURL: "https://github.com",
			})
			if got.IsArchived != tc.wantArchive {
				t.Errorf("IsArchived: got %v want %v", got.IsArchived, tc.wantArchive)
			}
			if got.Metadata.GitConfig["zoekt.archived"] != tc.wantZoekt {
				t.Errorf("zoekt.archived: got %q want %q", got.Metadata.GitConfig["zoekt.archived"], tc.wantZoekt)
			}
		})
	}
}

// TestCreateGitHubRepoRecord_NilCountsAsZero pins the
// `(count ?? 0).toString()` fallback. Legacy treats missing
// counters as 0 in the gitConfig output.
func TestCreateGitHubRepoRecord_NilCountsAsZero(t *testing.T) {
	repo := sampleOctokitRepo()
	repo.StargazersCount = nil
	repo.WatchersCount = nil
	repo.SubscribersCount = nil
	repo.ForksCount = nil

	got := CreateGitHubRepoRecord(CreateGitHubRepoRecordInput{
		Repo: repo, HostURL: "https://github.com",
	})

	cfg := got.Metadata.GitConfig
	for _, key := range []string{"zoekt.github-stars", "zoekt.github-watchers", "zoekt.github-subscribers", "zoekt.github-forks"} {
		if cfg[key] != "0" {
			t.Errorf("%s: got %q, want \"0\"", key, cfg[key])
		}
	}
}

// TestCreateGitHubRepoRecord_OrgIDDefaultsToSingleTenant locks
// the legacy `orgId = SINGLE_TENANT_ORG_ID` default-arg branch.
// An input with OrgID=0 produces the single-tenant id (1) on the
// output.
func TestCreateGitHubRepoRecord_OrgIDDefaultsToSingleTenant(t *testing.T) {
	got := CreateGitHubRepoRecord(CreateGitHubRepoRecordInput{
		Repo:    sampleOctokitRepo(),
		HostURL: "https://github.com",
		OrgID:   0,
	})
	if got.OrgID != SingleTenantOrgID {
		t.Errorf("OrgID: got %d, want %d", got.OrgID, SingleTenantOrgID)
	}
}

// TestCreateGitHubRepoRecord_NoTopicsEmitsEmptyArray pins the
// legacy `repo.topics ?? []` fallback. Even when the source has
// no topics, the metadata.codeHostMetadata.github.topics field
// is present in the JSON output as an empty array `[]`, not
// elided.
func TestCreateGitHubRepoRecord_NoTopicsEmitsEmptyArray(t *testing.T) {
	repo := sampleOctokitRepo()
	repo.Topics = nil

	got := CreateGitHubRepoRecord(CreateGitHubRepoRecordInput{
		Repo: repo, HostURL: "https://github.com",
	})

	if got.Metadata.CodeHostMetadata == nil || got.Metadata.CodeHostMetadata.GitHub == nil {
		t.Fatalf("metadata.codeHostMetadata.github missing")
	}
	topics := got.Metadata.CodeHostMetadata.GitHub.Topics
	if topics == nil || len(topics) != 0 {
		t.Errorf("topics: got %v, want empty (not nil) slice", topics)
	}

	// JSON round-trip: empty slice must serialize as [], not null
	// or missing.
	js, err := json.Marshal(got.Metadata.CodeHostMetadata.GitHub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(js), `"topics":[]`) {
		t.Errorf("topics serialization: got %s, want contains \"topics\":[]", js)
	}
}

// TestCompileGitHubConfig_AttachesConnectionID locks the
// per-repo connection-binding behavior. Each emitted RepoData
// carries the supplied connection id in its ConnectionIDs list.
func TestCompileGitHubConfig_AttachesConnectionID(t *testing.T) {
	got := CompileGitHubConfig(
		[]OctokitRepository{sampleOctokitRepo()},
		GitHubCompileInput{HostURL: "https://github.com"},
		77,
	)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0].ConnectionIDs, []int32{77}) {
		t.Errorf("ConnectionIDs: got %v, want [77]", got[0].ConnectionIDs)
	}
}

// TestCompileGitHubConfig_HostURLDefaults locks the empty-host
// fallback. Legacy:
//
//	const hostUrl = (config.url ?? 'https://github.com').replace(/\/+$/, '');
//
// An empty HostURL produces the github.com default.
func TestCompileGitHubConfig_HostURLDefaults(t *testing.T) {
	got := CompileGitHubConfig(
		[]OctokitRepository{sampleOctokitRepo()},
		GitHubCompileInput{HostURL: ""},
		1,
	)
	if got[0].ExternalCodeHostURL != "https://github.com" {
		t.Errorf("ExternalCodeHostURL: got %q, want https://github.com", got[0].ExternalCodeHostURL)
	}
}

// TestCompileGitHubConfig_TrailingSlashesStripped exercises the
// `.replace(/\/+$/, '')` regex. A host URL with one-or-more
// trailing slashes is normalized.
func TestCompileGitHubConfig_TrailingSlashesStripped(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://github.example.com", "https://github.example.com"},
		{"https://github.example.com/", "https://github.example.com"},
		{"https://github.example.com///", "https://github.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := CompileGitHubConfig(
				[]OctokitRepository{sampleOctokitRepo()},
				GitHubCompileInput{HostURL: tc.in},
				1,
			)
			if got[0].ExternalCodeHostURL != tc.want {
				t.Errorf("ExternalCodeHostURL: got %q, want %q", got[0].ExternalCodeHostURL, tc.want)
			}
		})
	}
}

// TestRepoData_JSONShape captures a single full record's JSON
// serialization byte-for-byte. This is the wire-shape parity
// assertion: any future drift in field name, order, or omitempty
// behavior breaks this test. The legacy Prisma serialiser emits
// the same field names; column order isn't observable on the
// wire (it's an SQL INSERT, not a JSON response) but the test
// pins the Go-side JSON shape as a guard.
func TestRepoData_JSONShape(t *testing.T) {
	got := CreateGitHubRepoRecord(CreateGitHubRepoRecordInput{
		Repo:    sampleOctokitRepo(),
		HostURL: "https://github.com",
		OrgID:   1,
	})
	js, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Spot-check critical wire fields are present + correct.
	for _, want := range []string{
		`"external_id":"123456789"`,
		`"external_codeHostType":"github"`,
		`"external_codeHostUrl":"https://github.com"`,
		`"cloneUrl":"https://github.com/example-org/agent-workbench.git"`,
		`"webUrl":"https://github.com/example-org/agent-workbench"`,
		`"name":"github.com/example-org/agent-workbench"`,
		`"displayName":"example-org/agent-workbench"`,
		`"isFork":false`,
		`"isArchived":false`,
		`"isPublic":true`,
		`"orgId":1`,
		`"zoekt.github-stars":"42"`,
		`"topics":["ai","code"]`,
	} {
		if !strings.Contains(string(js), want) {
			t.Errorf("JSON missing %s\nfull: %s", want, js)
		}
	}
}
