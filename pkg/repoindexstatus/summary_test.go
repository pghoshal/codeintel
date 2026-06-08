package repoindexstatus

import (
	"encoding/json"
	"testing"
)

// strPtr returns a pointer to a string literal. Tiny helper so
// the test fixtures stay readable.
func strPtr(s string) *string { return &s }

// makeRepo mirrors the legacy `repo()` helper in
// indexStatus.test.ts:5-13.
func makeRepo(overrides func(*RepoInput)) RepoInput {
	defaultBranch := "main"
	indexedAt := "2026-05-22T00:00:00Z"
	meta, _ := json.Marshal(map[string]any{
		"indexedRevisions": []string{"refs/heads/main"},
	})
	r := RepoInput{
		Metadata:                meta,
		DefaultBranch:           &defaultBranch,
		IndexedAt:               &indexedAt,
		LatestIndexingJobStatus: nil,
	}
	if overrides != nil {
		overrides(&r)
	}
	return r
}

// --- Parity tests: direct ports of indexStatus.test.ts cases ---

// "keeps an older active index usable when the latest reindex
// failed" — locks the key invariant. From indexStatus.test.ts:17-31.
func TestBuildRepoIndexSummary_OlderIndexUsableWhenReindexFailed(t *testing.T) {
	summary := BuildRepoIndexSummary(makeRepo(nil), &LatestJob{
		ID: "job-failed", Type: JobTypeIndex, Status: JobStatusFailed,
	})

	if summary.Status != StateIndexed {
		t.Errorf("Status: got %q want indexed", summary.Status)
	}
	if summary.Color != ColorGreen {
		t.Errorf("Color: got %q want green", summary.Color)
	}
	if !summary.Indexed {
		t.Errorf("Indexed: got false want true")
	}
	if summary.ActiveIndexStatus != StateIndexed {
		t.Errorf("ActiveIndexStatus: got %q want indexed", summary.ActiveIndexStatus)
	}
	if !summary.ActiveIndexUsable {
		t.Errorf("ActiveIndexUsable: got false want true")
	}
	if summary.LatestRunStatus != RunFailed {
		t.Errorf("LatestRunStatus: got %q want failed", summary.LatestRunStatus)
	}
	if summary.LatestRunStatusColor != ColorRed {
		t.Errorf("LatestRunStatusColor: got %q want red", summary.LatestRunStatusColor)
	}
	if summary.LatestRunBlocksActiveIndex {
		t.Errorf("LatestRunBlocksActiveIndex: got true want false")
	}
}

// "reports failed when no active index exists" — indexStatus.test.ts:33-48.
func TestBuildRepoIndexSummary_FailedWhenNoActiveIndex(t *testing.T) {
	summary := BuildRepoIndexSummary(makeRepo(func(r *RepoInput) {
		r.IndexedAt = nil
		r.Metadata, _ = json.Marshal(map[string]any{"indexedRevisions": []string{}})
	}), &LatestJob{
		ID: "job-failed", Type: JobTypeIndex, Status: JobStatusFailed,
	})

	if summary.Status != StateFailed {
		t.Errorf("Status: got %q want failed", summary.Status)
	}
	if summary.Color != ColorRed {
		t.Errorf("Color: got %q want red", summary.Color)
	}
	if summary.Indexed {
		t.Errorf("Indexed: got true want false")
	}
	if summary.ActiveIndexStatus != StateNotIndexed {
		t.Errorf("ActiveIndexStatus: got %q want not_indexed", summary.ActiveIndexStatus)
	}
	if !summary.LatestRunBlocksActiveIndex {
		t.Errorf("LatestRunBlocksActiveIndex: got false want true")
	}
}

// "shows active work as the visible state while preserving
// active usability" — indexStatus.test.ts:50-63.
func TestBuildRepoIndexSummary_IndexingPreservesActiveUsability(t *testing.T) {
	summary := BuildRepoIndexSummary(makeRepo(nil), &LatestJob{
		ID: "job-indexing", Type: JobTypeIndex, Status: JobStatusInProgress,
	})

	if summary.Status != StateIndexing {
		t.Errorf("Status: got %q want indexing", summary.Status)
	}
	if summary.Color != ColorYellow {
		t.Errorf("Color: got %q want yellow", summary.Color)
	}
	if !summary.Indexed {
		t.Errorf("Indexed: got false want true (older index still usable)")
	}
	if summary.ActiveIndexStatus != StateIndexed {
		t.Errorf("ActiveIndexStatus: got %q want indexed", summary.ActiveIndexStatus)
	}
	if summary.LatestRunStatus != RunIndexing {
		t.Errorf("LatestRunStatus: got %q want indexing", summary.LatestRunStatus)
	}
	if !summary.LatestRunBlocksActiveIndex {
		t.Errorf("LatestRunBlocksActiveIndex: got false want true")
	}
}

// REMOVE_INDEX is in flight -> runStatus becomes "removing" not
// "indexing". indexStatus.ts:128 branch.
func TestBuildRepoIndexSummary_RemoveIndexInFlight(t *testing.T) {
	summary := BuildRepoIndexSummary(makeRepo(nil), &LatestJob{
		ID: "job-rm", Type: JobTypeRemoveIndex, Status: JobStatusInProgress,
	})
	if summary.Status != StateRemoving {
		t.Errorf("Status: got %q want removing", summary.Status)
	}
	if summary.LatestRunStatus != RunRemoving {
		t.Errorf("LatestRunStatus: got %q want removing", summary.LatestRunStatus)
	}
}

// COMPLETED job + indexed-at + indexed revisions -> green/indexed.
func TestBuildRepoIndexSummary_HappyCompleted(t *testing.T) {
	summary := BuildRepoIndexSummary(makeRepo(nil), &LatestJob{
		ID: "job-ok", Type: JobTypeIndex, Status: JobStatusCompleted,
	})
	if summary.Status != StateIndexed {
		t.Errorf("Status: got %q want indexed", summary.Status)
	}
	if summary.LatestRunStatus != RunCompleted {
		t.Errorf("LatestRunStatus: got %q want completed", summary.LatestRunStatus)
	}
}

// No job at all, never indexed -> not_indexed/none.
func TestBuildRepoIndexSummary_NeverIndexed(t *testing.T) {
	summary := BuildRepoIndexSummary(RepoInput{
		Metadata: json.RawMessage(`{}`),
	}, nil)
	if summary.Status != StateNotIndexed {
		t.Errorf("Status: got %q want not_indexed", summary.Status)
	}
	if summary.LatestRunStatus != RunNone {
		t.Errorf("LatestRunStatus: got %q want none", summary.LatestRunStatus)
	}
	if summary.LatestRunStatusColor != ColorGray {
		t.Errorf("LatestRunStatusColor: got %q want gray", summary.LatestRunStatusColor)
	}
}

// IndexedRevisions field exposes the policy-visible revisions
// 1:1 from metadata. Verifies the policy filter is wired
// through.
func TestBuildRepoIndexSummary_IndexedRevisionsField(t *testing.T) {
	summary := BuildRepoIndexSummary(makeRepo(nil), nil)
	if len(summary.IndexedRevisions) != 1 || summary.IndexedRevisions[0] != "refs/heads/main" {
		t.Errorf("IndexedRevisions: got %v want [refs/heads/main]", summary.IndexedRevisions)
	}
}

// --- Policy helpers ---

func TestIsBranchAllowedByIndexPolicy(t *testing.T) {
	defaultBranch := "main"
	cases := []struct {
		name     string
		patterns []string
		branch   string
		want     bool
	}{
		{"no patterns + default match", nil, "main", true},
		{"no patterns + non-default", nil, "feature", false},
		{"star matches all", []string{"*"}, "anything", true},
		{"exact match", []string{"main", "release"}, "main", true},
		{"glob match feature/*", []string{"feature/*"}, "feature/x", true},
		{"glob non-match", []string{"feature/*"}, "hotfix/x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta, _ := json.Marshal(map[string]any{"branches": tc.patterns})
			repo := RepoInput{Metadata: meta, DefaultBranch: &defaultBranch}
			got := IsBranchAllowedByIndexPolicy(repo, tc.branch)
			if got != tc.want {
				t.Errorf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestIsRevisionAllowedByIndexPolicy_Tags(t *testing.T) {
	defaultBranch := "main"
	meta, _ := json.Marshal(map[string]any{"tags": []string{"v*"}})
	repo := RepoInput{Metadata: meta, DefaultBranch: &defaultBranch}

	if !IsRevisionAllowedByIndexPolicy(repo, "refs/tags/v1.2.3") {
		t.Errorf("refs/tags/v1.2.3: want allowed")
	}
	if IsRevisionAllowedByIndexPolicy(repo, "refs/tags/other") {
		t.Errorf("refs/tags/other: want NOT allowed (no matching tag pattern)")
	}
}

func TestIsRevisionAllowedByIndexPolicy_NoTagsConfigured_Rejects(t *testing.T) {
	// Legacy: when tags list is empty, ALL tag revs are
	// rejected (indexStatus.ts:107-108).
	defaultBranch := "main"
	meta, _ := json.Marshal(map[string]any{})
	repo := RepoInput{Metadata: meta, DefaultBranch: &defaultBranch}
	if IsRevisionAllowedByIndexPolicy(repo, "refs/tags/v1") {
		t.Errorf("unconfigured tags: want NOT allowed")
	}
}

func TestIsRevisionAllowedByIndexPolicy_NonBranchNonTag_AllowAll(t *testing.T) {
	repo := RepoInput{}
	if !IsRevisionAllowedByIndexPolicy(repo, "abcdef0123") {
		t.Errorf("commit SHA: want allowed (permissive default)")
	}
}

func TestParseMetadata_EmptyAndCorrupt(t *testing.T) {
	if got := ParseMetadata(nil); got.Branches != nil {
		t.Errorf("nil: %+v", got)
	}
	if got := ParseMetadata(json.RawMessage("not json")); got.Branches != nil {
		t.Errorf("corrupt: %+v", got)
	}
	got := ParseMetadata(json.RawMessage(`{"branches":["main"]}`))
	if len(got.Branches) != 1 || got.Branches[0] != "main" {
		t.Errorf("happy: %+v", got)
	}
}

func TestStripBranchRevision(t *testing.T) {
	if got := StripBranchRevision("refs/heads/main"); got != "main" {
		t.Errorf("got %q", got)
	}
	if got := StripBranchRevision("main"); got != "main" {
		t.Errorf("got %q", got)
	}
}

func TestBranchRevision(t *testing.T) {
	if got := BranchRevision("main"); got != "refs/heads/main" {
		t.Errorf("got %q", got)
	}
	if got := BranchRevision("refs/heads/main"); got != "refs/heads/main" {
		t.Errorf("got %q", got)
	}
}

// strPtr ensures the helper compiles + is reachable.
var _ = strPtr
