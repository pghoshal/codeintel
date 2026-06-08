package repoindexstatus

import (
	"encoding/json"
	"testing"
)

// "applies the same split model to branch status" — direct
// port of indexStatus.test.ts:65-77.
func TestBuildBranchIndexStatus_OlderIndexUsableOnFailedReindex(t *testing.T) {
	defaultBranch := "main"
	indexedAt := "2026-05-22T00:00:00Z"
	meta, _ := json.Marshal(map[string]any{"indexedRevisions": []string{"refs/heads/main"}})
	repo := RepoInput{
		Metadata:      meta,
		DefaultBranch: &defaultBranch,
		IndexedAt:     &indexedAt,
	}
	status := BuildBranchIndexStatus(repo, "main", &LatestJob{
		ID: "job-failed", Type: JobTypeIndex, Status: JobStatusFailed,
	})

	if status.Status != StateIndexed {
		t.Errorf("Status: got %q want indexed", status.Status)
	}
	if status.Color != ColorGreen {
		t.Errorf("Color: got %q want green", status.Color)
	}
	if status.ActiveIndexStatus != StateIndexed {
		t.Errorf("ActiveIndexStatus: got %q want indexed", status.ActiveIndexStatus)
	}
	if status.LatestRunStatus != RunFailed {
		t.Errorf("LatestRunStatus: got %q want failed", status.LatestRunStatus)
	}
	if status.LatestRunBlocksActiveIndex {
		t.Errorf("LatestRunBlocksActiveIndex: got true want false")
	}
}

func TestBuildBranchIndexStatus_DisallowedByPolicy(t *testing.T) {
	defaultBranch := "main"
	indexedAt := "2026-05-22T00:00:00Z"
	meta, _ := json.Marshal(map[string]any{
		"branches":         []string{"main", "release/*"},
		"indexedRevisions": []string{"refs/heads/main"},
	})
	repo := RepoInput{
		Metadata:      meta,
		DefaultBranch: &defaultBranch,
		IndexedAt:     &indexedAt,
	}

	t.Run("allowed branch", func(t *testing.T) {
		s := BuildBranchIndexStatus(repo, "main", nil)
		if !s.AllowedByPolicy {
			t.Errorf("main: allowedByPolicy=false")
		}
		if !s.Indexed {
			t.Errorf("main: indexed=false")
		}
		if s.Revision != "refs/heads/main" {
			t.Errorf("main: revision=%q", s.Revision)
		}
	})
	t.Run("disallowed branch", func(t *testing.T) {
		s := BuildBranchIndexStatus(repo, "feature/new", nil)
		if s.AllowedByPolicy {
			t.Errorf("feature/new: allowedByPolicy=true want false")
		}
		if s.Indexed {
			t.Errorf("feature/new: indexed=true want false")
		}
	})
}

func TestBuildBranchIndexStatus_BranchRefIsCorrect(t *testing.T) {
	defaultBranch := "main"
	repo := RepoInput{
		Metadata:      json.RawMessage(`{}`),
		DefaultBranch: &defaultBranch,
	}
	for _, in := range []string{"main", "refs/heads/main"} {
		s := BuildBranchIndexStatus(repo, in, nil)
		if s.Branch != "main" || s.Revision != "refs/heads/main" {
			t.Errorf("in=%q -> Branch=%q Revision=%q", in, s.Branch, s.Revision)
		}
	}
}

func TestBuildBranchIndexStatus_LatestJobFieldsForwarded(t *testing.T) {
	defaultBranch := "main"
	repo := RepoInput{
		Metadata:      json.RawMessage(`{}`),
		DefaultBranch: &defaultBranch,
	}
	s := BuildBranchIndexStatus(repo, "main", &LatestJob{
		ID: "j1", Type: JobTypeIndex, Status: JobStatusCompleted,
	})
	if s.LatestJobID == nil || *s.LatestJobID != "j1" {
		t.Errorf("LatestJobID: %v", s.LatestJobID)
	}
	if s.LatestJobType == nil || *s.LatestJobType != JobTypeIndex {
		t.Errorf("LatestJobType: %v", s.LatestJobType)
	}
	if s.LatestJobStatus == nil || *s.LatestJobStatus != JobStatusCompleted {
		t.Errorf("LatestJobStatus: %v", s.LatestJobStatus)
	}
}

// IndexedAt is only emitted on the response when the branch is
// indexed — mirrors legacy `indexed ? repo.indexedAt ?? undefined
// : undefined` from indexStatus.ts:204.
func TestBuildBranchIndexStatus_IndexedAtOnlyWhenIndexed(t *testing.T) {
	defaultBranch := "main"
	indexedAt := "2026-05-22T00:00:00Z"
	meta, _ := json.Marshal(map[string]any{"indexedRevisions": []string{"refs/heads/main"}})

	t.Run("indexed branch", func(t *testing.T) {
		repo := RepoInput{Metadata: meta, DefaultBranch: &defaultBranch, IndexedAt: &indexedAt}
		s := BuildBranchIndexStatus(repo, "main", nil)
		if s.IndexedAt == nil {
			t.Errorf("IndexedAt: got nil want set")
		}
	})
	t.Run("not-indexed branch", func(t *testing.T) {
		repo := RepoInput{Metadata: meta, DefaultBranch: &defaultBranch} // indexedAt nil
		s := BuildBranchIndexStatus(repo, "main", nil)
		if s.IndexedAt != nil {
			t.Errorf("IndexedAt: got set want nil")
		}
	})
}

func TestBuildKnownBranchIndexStatuses_UnionDeduped(t *testing.T) {
	defaultBranch := "main"
	indexedAt := "2026-05-22T00:00:00Z"
	meta, _ := json.Marshal(map[string]any{
		"branches":         []string{"main", "release/v1", "feature/*"}, // glob excluded
		"indexedRevisions": []string{"refs/heads/main", "refs/heads/develop", "refs/tags/v1"},
	})
	repo := RepoInput{
		Metadata:      meta,
		DefaultBranch: &defaultBranch,
		IndexedAt:     &indexedAt,
	}
	got := BuildKnownBranchIndexStatuses(repo, nil)
	// Expected union (first-seen order):
	//   main (default) -> already in configured -> dedup
	//   release/v1 (from configured)
	//   develop (from indexedRevisions)
	// feature/* excluded (glob).
	// refs/tags/v1 excluded (not a branch).
	if len(got) != 3 {
		t.Fatalf("got %d branches, want 3: %+v", len(got), branchNames(got))
	}
	if got[0].Branch != "main" {
		t.Errorf("got[0].Branch=%q want main", got[0].Branch)
	}
	if got[1].Branch != "release/v1" {
		t.Errorf("got[1].Branch=%q want release/v1", got[1].Branch)
	}
	if got[2].Branch != "develop" {
		t.Errorf("got[2].Branch=%q want develop", got[2].Branch)
	}
}

func TestBuildKnownBranchIndexStatuses_NoDefaultBranch(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{
		"branches": []string{"main"},
	})
	repo := RepoInput{Metadata: meta} // no defaultBranch
	got := BuildKnownBranchIndexStatuses(repo, nil)
	if len(got) != 1 || got[0].Branch != "main" {
		t.Errorf("got %+v", branchNames(got))
	}
}

func TestBuildKnownBranchIndexStatuses_OnlyIndexedRevisionsBranches(t *testing.T) {
	meta, _ := json.Marshal(map[string]any{
		"indexedRevisions": []string{"refs/heads/main", "refs/tags/v1", "abc123"},
	})
	repo := RepoInput{Metadata: meta}
	got := BuildKnownBranchIndexStatuses(repo, nil)
	if len(got) != 1 || got[0].Branch != "main" {
		t.Errorf("only main should appear; got %+v", branchNames(got))
	}
}

func branchNames(bs []BranchIndexStatus) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Branch
	}
	return out
}
