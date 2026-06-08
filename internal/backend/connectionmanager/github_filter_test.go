package connectionmanager

import "testing"

func pInt64(v int64) *int64 { return &v }

// baseRepo is the per-test fixture. Each test mutates one field.
func baseRepo() OctokitRepository {
	return OctokitRepository{
		Name:     "agent-workbench",
		ID:       1,
		FullName: "example-org/agent-workbench",
		Fork:     false,
		Private:  false,
		HTMLURL:  "https://github.com/example-org/agent-workbench",
		CloneURL: pStr("https://github.com/example-org/agent-workbench.git"),
		Topics:   []string{"ai", "code"},
		Size:     pInt(1024),
		Owner:    OctokitOwner{AvatarURL: "x", Login: "example-org"},
	}
}

// TestShouldExcludeRepo_NoCloneURL pins branch 1: a repo
// without clone_url is always excluded (legacy line 498).
func TestShouldExcludeRepo_NoCloneURL(t *testing.T) {
	r := baseRepo()
	r.CloneURL = nil
	if !ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r}) {
		t.Errorf("expected exclude when clone_url missing")
	}
}

// TestShouldExcludeRepo_ForksFlag pins branch 2: exclude.forks=true
// excludes forks. A non-fork repo with the same flag is kept.
func TestShouldExcludeRepo_ForksFlag(t *testing.T) {
	r := baseRepo()
	r.Fork = true
	excludeForks := &GitHubExcludeRules{Forks: pBool(true)}
	if !ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Exclude: excludeForks}) {
		t.Errorf("expected exclude for fork with exclude.forks=true")
	}
	r.Fork = false
	if ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Exclude: excludeForks}) {
		t.Errorf("expected keep for non-fork with exclude.forks=true")
	}
}

// TestShouldExcludeRepo_ArchivedFlag pins branch 3.
func TestShouldExcludeRepo_ArchivedFlag(t *testing.T) {
	r := baseRepo()
	r.Archived = pBool(true)
	excludeArchived := &GitHubExcludeRules{Archived: pBool(true)}
	if !ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Exclude: excludeArchived}) {
		t.Errorf("expected exclude for archived")
	}
	r.Archived = pBool(false)
	if ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Exclude: excludeArchived}) {
		t.Errorf("expected keep for non-archived")
	}
}

// TestShouldExcludeRepo_ReposGlob pins branch 4: full_name
// glob match against exclude.repos.
func TestShouldExcludeRepo_ReposGlob(t *testing.T) {
	r := baseRepo() // full_name = "example-org/agent-workbench"
	cases := []struct {
		name     string
		patterns []string
		exclude  bool
	}{
		{"exact match", []string{"example-org/agent-workbench"}, true},
		{"star segment", []string{"example-org/*"}, true},
		{"doublestar", []string{"**/agent-workbench"}, true},
		{"no match", []string{"someorg/*"}, false},
		{"multiple patterns one hit", []string{"foo/*", "example-org/*"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &GitHubExcludeRules{Repos: tc.patterns}
			got := ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Exclude: ex})
			if got != tc.exclude {
				t.Errorf("got %v want %v", got, tc.exclude)
			}
		})
	}
}

// TestShouldExcludeRepo_ExcludeTopics pins branch 5: any
// repo-topic matching any pattern excludes. Case-folded.
func TestShouldExcludeRepo_ExcludeTopics(t *testing.T) {
	r := baseRepo() // topics = ["ai", "code"]
	cases := []struct {
		name     string
		patterns []string
		exclude  bool
	}{
		{"exact match", []string{"ai"}, true},
		{"case-fold", []string{"AI"}, true},
		{"glob", []string{"co*"}, true},
		{"no match", []string{"web"}, false},
		{"empty repo topics", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := r
			if tc.name == "empty repo topics" {
				rr.Topics = nil
			}
			ex := &GitHubExcludeRules{Topics: tc.patterns}
			got := ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: rr, Exclude: ex})
			if got != tc.exclude {
				t.Errorf("got %v want %v", got, tc.exclude)
			}
		})
	}
}

// TestShouldExcludeRepo_IncludeTopics pins branch 6: when
// include.topics is set, at least one repo-topic MUST match.
// A non-matching repo is excluded.
func TestShouldExcludeRepo_IncludeTopics(t *testing.T) {
	r := baseRepo() // topics = ["ai", "code"]
	cases := []struct {
		name     string
		patterns []string
		exclude  bool
	}{
		{"matching include", []string{"ai"}, false},
		{"case-fold match", []string{"CODE"}, false},
		{"no match excludes", []string{"web"}, true},
		{"glob match", []string{"co*"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inc := &GitHubIncludeRules{Topics: tc.patterns}
			got := ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Include: inc})
			if got != tc.exclude {
				t.Errorf("got %v want %v", got, tc.exclude)
			}
		})
	}
}

// TestShouldExcludeRepo_IncludeTopicsRepoHasNone pins the
// edge case where include.topics is set but the repo has no
// topics: legacy excludes (no match possible).
func TestShouldExcludeRepo_IncludeTopicsRepoHasNone(t *testing.T) {
	r := baseRepo()
	r.Topics = nil
	inc := &GitHubIncludeRules{Topics: []string{"ai"}}
	if !ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Include: inc}) {
		t.Errorf("expected exclude when include.topics set + repo has no topics")
	}
}

// TestShouldExcludeRepo_SizeMinMax pins branch 7: the
// KB→bytes ×1000 conversion + min/max comparison. Legacy
// line 542: `repo.size * 1000`.
//
// Test fixture: repo.size = 1024 (KB) → 1_024_000 bytes.
func TestShouldExcludeRepo_SizeMinMax(t *testing.T) {
	r := baseRepo() // size = 1024 KB
	cases := []struct {
		name    string
		size    *GitHubSize
		exclude bool
	}{
		{"under min excludes", &GitHubSize{Min: pInt64(2_000_000)}, true},
		{"over max excludes", &GitHubSize{Max: pInt64(500_000)}, true},
		{"within range keeps", &GitHubSize{Min: pInt64(500_000), Max: pInt64(2_000_000)}, false},
		{"exact min keeps (not <)", &GitHubSize{Min: pInt64(1_024_000)}, false},
		{"exact max keeps (not >)", &GitHubSize{Max: pInt64(1_024_000)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ex := &GitHubExcludeRules{Size: tc.size}
			got := ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Exclude: ex})
			if got != tc.exclude {
				t.Errorf("got %v want %v", got, tc.exclude)
			}
		})
	}
}

// TestShouldExcludeRepo_SizeWithNoSizeOnRepo pins the
// !repo.size early-return: legacy line 543 short-circuits
// when repo.size is missing.
func TestShouldExcludeRepo_SizeWithNoSizeOnRepo(t *testing.T) {
	r := baseRepo()
	r.Size = nil
	ex := &GitHubExcludeRules{Size: &GitHubSize{Min: pInt64(1_000_000)}}
	if ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r, Exclude: ex}) {
		t.Errorf("expected keep when repo.size missing (size rule ignored)")
	}
}

// TestShouldExcludeRepo_KeepsWithNoExcludeOrInclude is the
// happy path: a typical public repo with no filters set
// passes through.
func TestShouldExcludeRepo_KeepsWithNoExcludeOrInclude(t *testing.T) {
	r := baseRepo()
	if ShouldExcludeRepo(ShouldExcludeRepoInput{Repo: r}) {
		t.Errorf("expected keep for vanilla repo with no filters")
	}
}
