// Package repoindexstatus is the parity port of the legacy
// repo-index status helpers from
// packages/web/src/features/repos/indexStatus.ts.
//
// Scope of this slice (P.3b in docs/codeintel-parity-gaps.md):
// only `buildRepoIndexSummary` and the supporting policy
// helpers it needs. The branch-level helpers
// (`buildBranchIndexStatus`, `buildKnownBranchIndexStatuses`)
// land in P.3c — they share machinery but the wire surfaces
// they feed are independent.
//
// Output type contract: every field on RepoIndexSummary is the
// SAME shape (name, string values) as the legacy struct so a
// client written against the legacy summary can decode this
// port's JSON byte-equal.
package repoindexstatus

import (
	"encoding/json"
	"strings"

	"github.com/gobwas/glob"
)

// RepoIndexState mirrors the legacy union type from
// indexStatus.ts:5. Five string values; any other value is a
// programming error.
type RepoIndexState string

const (
	StateIndexed    RepoIndexState = "indexed"
	StateIndexing   RepoIndexState = "indexing"
	StateFailed     RepoIndexState = "failed"
	StateNotIndexed RepoIndexState = "not_indexed"
	StateRemoving   RepoIndexState = "removing"
)

// RepoIndexColor mirrors the legacy color union (indexStatus.ts:6).
type RepoIndexColor string

const (
	ColorGreen  RepoIndexColor = "green"
	ColorYellow RepoIndexColor = "yellow"
	ColorRed    RepoIndexColor = "red"
	ColorGray   RepoIndexColor = "gray"
)

// RepoIndexRunState mirrors indexStatus.ts:7.
type RepoIndexRunState string

const (
	RunCompleted RepoIndexRunState = "completed"
	RunFailed    RepoIndexRunState = "failed"
	RunIndexing  RepoIndexRunState = "indexing"
	RunRemoving  RepoIndexRunState = "removing"
	RunNone      RepoIndexRunState = "none"
)

// JobType + JobStatus narrow strings to the legacy
// RepoIndexingJobType + RepoIndexingJobStatus enums. The HTTP
// layer reads the column as text via status::text — we keep
// the strings unparsed here.
type JobType string

const (
	JobTypeIndex       JobType = "INDEX"
	JobTypeRemoveIndex JobType = "REMOVE_INDEX"
)

type JobStatus string

const (
	JobStatusPending    JobStatus = "PENDING"
	JobStatusInProgress JobStatus = "IN_PROGRESS"
	JobStatusCompleted  JobStatus = "COMPLETED"
	JobStatusFailed     JobStatus = "FAILED"
)

// RepoInput is the minimum repo shape BuildRepoIndexSummary
// needs. Mirrors the legacy `RepoIndexStatusInput` type
// (indexStatus.ts:15-20).
//
// Metadata is the raw jsonb bytes from Repo.metadata; the port
// parses them per-call into a RepoMetadata. LatestIndexingJobStatus
// is a *string so an unindexed repo (NULL column) can be
// distinguished from an empty-string status.
type RepoInput struct {
	Metadata                json.RawMessage
	DefaultBranch           *string
	IndexedAt               *string // nullable timestamp; non-nil means "was indexed at some point"
	LatestIndexingJobStatus *string
}

// LatestJob is the optional last-RepoIndexingJob projection
// BuildRepoIndexSummary's run-state branch reads. nil is a
// valid input (the legacy `latestJob?` optional argument).
type LatestJob struct {
	ID     string
	Type   JobType
	Status JobStatus
}

// RepoIndexSummary mirrors the legacy struct (indexStatus.ts:41-52)
// 1:1. Same field names + same value semantics so the wire body
// byte-equal-matches the legacy.
type RepoIndexSummary struct {
	Status                     RepoIndexState
	Color                      RepoIndexColor
	Indexed                    bool
	IndexedRevisions           []string
	ActiveIndexStatus          RepoIndexState
	ActiveIndexStatusColor     RepoIndexColor
	ActiveIndexUsable          bool
	LatestRunStatus            RepoIndexRunState
	LatestRunStatusColor       RepoIndexColor
	LatestRunBlocksActiveIndex bool
}

// RepoMetadata is the minimum subset of the legacy
// repoMetadataSchema (packages/shared/src/types.ts:12) that
// the status helpers consume. Other fields (gitConfig,
// manualIndexDisabled, codeHostMetadata) are loaded on a
// per-use basis elsewhere and intentionally omitted here.
type RepoMetadata struct {
	Branches         []string `json:"branches,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	IndexedRevisions []string `json:"indexedRevisions,omitempty"`
}

// ParseMetadata mirrors the legacy parseMetadata helper
// (indexStatus.ts:62-65). The legacy used a zod safeParse that
// returned `{}` on validation failure; the Go port returns a
// zero-valued RepoMetadata on any JSON decode error so a
// corrupt blob can't crash the status route.
func ParseMetadata(raw json.RawMessage) RepoMetadata {
	if len(raw) == 0 {
		return RepoMetadata{}
	}
	var md RepoMetadata
	if err := json.Unmarshal(raw, &md); err != nil {
		return RepoMetadata{}
	}
	return md
}

// BranchRevision returns the fully-qualified ref name for a
// branch. Mirrors indexStatus.ts:54.
func BranchRevision(branch string) string {
	if strings.HasPrefix(branch, "refs/heads/") {
		return branch
	}
	return "refs/heads/" + branch
}

// StripBranchRevision strips the refs/heads/ prefix. Mirrors
// indexStatus.ts:56-60.
func StripBranchRevision(revision string) string {
	return strings.TrimPrefix(revision, "refs/heads/")
}

// IsBranchAllowedByIndexPolicy mirrors indexStatus.ts:81-94.
//
// Rules (verbatim):
//   - If metadata.branches is unset/empty: only the repo's
//     defaultBranch is allowed (or any branch when there's no
//     defaultBranch).
//   - If metadata.branches contains "*": every branch is allowed.
//   - Otherwise: micromatch.isMatch against the patterns
//     (gobwas/glob is the codeintel substitute — both libraries
//     implement the same {*, ?, [abc], !x, +(a|b)} syntax).
func IsBranchAllowedByIndexPolicy(repo RepoInput, branch string) bool {
	md := ParseMetadata(repo.Metadata)
	branchName := StripBranchRevision(branch)

	patterns := filterNonEmpty(md.Branches)
	if len(patterns) == 0 {
		if repo.DefaultBranch == nil || *repo.DefaultBranch == "" {
			return true
		}
		return branchName == *repo.DefaultBranch
	}
	if containsString(patterns, "*") {
		return true
	}
	return matchAnyGlob(branchName, patterns)
}

// IsRevisionAllowedByIndexPolicy mirrors indexStatus.ts:96-112.
// Handles refs/heads/* via the branch check + refs/tags/* via
// the tags glob list + every other ref shape via a permissive
// allow.
func IsRevisionAllowedByIndexPolicy(repo RepoInput, revision string) bool {
	if strings.HasPrefix(revision, "refs/heads/") {
		return IsBranchAllowedByIndexPolicy(repo, revision)
	}
	if strings.HasPrefix(revision, "refs/tags/") {
		md := ParseMetadata(repo.Metadata)
		tagPatterns := filterNonEmpty(md.Tags)
		if len(tagPatterns) == 0 {
			return false
		}
		tagName := strings.TrimPrefix(revision, "refs/tags/")
		return matchAnyGlob(tagName, tagPatterns)
	}
	return true
}

// GetPolicyVisibleIndexedRevisions filters metadata.indexedRevisions
// down to revisions that still satisfy the current index policy.
// Mirrors indexStatus.ts:114-121.
func GetPolicyVisibleIndexedRevisions(repo RepoInput) []string {
	md := ParseMetadata(repo.Metadata)
	out := make([]string, 0, len(md.IndexedRevisions))
	for _, rev := range md.IndexedRevisions {
		if rev == "" {
			continue
		}
		if IsRevisionAllowedByIndexPolicy(repo, rev) {
			out = append(out, rev)
		}
	}
	return out
}

// getLatestRunState mirrors indexStatus.ts:123-137.
func getLatestRunState(repo RepoInput, latestJob *LatestJob) RepoIndexRunState {
	var (
		jobType   JobType
		jobStatus JobStatus
	)
	if latestJob != nil {
		jobType = latestJob.Type
		jobStatus = latestJob.Status
	}
	if jobStatus == "" && repo.LatestIndexingJobStatus != nil {
		jobStatus = JobStatus(*repo.LatestIndexingJobStatus)
	}

	switch jobStatus {
	case JobStatusPending, JobStatusInProgress:
		if jobType == JobTypeRemoveIndex {
			return RunRemoving
		}
		return RunIndexing
	case JobStatusFailed:
		return RunFailed
	case JobStatusCompleted:
		return RunCompleted
	}
	return RunNone
}

// resolveVisibleStatus mirrors indexStatus.ts:139-150.
func resolveVisibleStatus(active RepoIndexState, run RepoIndexRunState) RepoIndexState {
	if run == RunIndexing || run == RunRemoving {
		return RepoIndexState(run)
	}
	if run == RunFailed && active != StateIndexed {
		return StateFailed
	}
	return active
}

func colorForState(s RepoIndexState) RepoIndexColor {
	switch s {
	case StateIndexed:
		return ColorGreen
	case StateFailed:
		return ColorRed
	case StateIndexing, StateRemoving:
		return ColorYellow
	}
	return ColorGray
}

func colorForRunState(s RepoIndexRunState) RepoIndexColor {
	switch s {
	case RunCompleted:
		return ColorGreen
	case RunFailed:
		return ColorRed
	case RunIndexing, RunRemoving:
		return ColorYellow
	}
	return ColorGray
}

// BuildRepoIndexSummary is the direct port of
// indexStatus.ts:152-172. Same control flow, same field
// derivations, same return shape.
//
// The split model legacy uses: `activeIndexStatus` reflects
// whether there's a usable index on disk regardless of the
// latest job. `latestRunStatus` reflects the most recent job's
// fate. `status` is the visible badge state — derived from both
// so an older index stays usable when a re-index fails (the key
// invariant the legacy test
// indexStatus.test.ts:17-31 locks).
func BuildRepoIndexSummary(repo RepoInput, latestJob *LatestJob) RepoIndexSummary {
	indexedRevisions := GetPolicyVisibleIndexedRevisions(repo)

	activeIndexStatus := StateNotIndexed
	if repo.IndexedAt != nil && len(indexedRevisions) > 0 {
		activeIndexStatus = StateIndexed
	}

	latestRunStatus := getLatestRunState(repo, latestJob)
	status := resolveVisibleStatus(activeIndexStatus, latestRunStatus)

	blocks := latestRunStatus == RunIndexing ||
		latestRunStatus == RunRemoving ||
		(latestRunStatus == RunFailed && activeIndexStatus != StateIndexed)

	return RepoIndexSummary{
		Status:                     status,
		Color:                      colorForState(status),
		Indexed:                    activeIndexStatus == StateIndexed,
		IndexedRevisions:           indexedRevisions,
		ActiveIndexStatus:          activeIndexStatus,
		ActiveIndexStatusColor:     colorForState(activeIndexStatus),
		ActiveIndexUsable:          activeIndexStatus == StateIndexed,
		LatestRunStatus:            latestRunStatus,
		LatestRunStatusColor:       colorForRunState(latestRunStatus),
		LatestRunBlocksActiveIndex: blocks,
	}
}

// ---- helpers ----

func filterNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// matchAnyGlob reports whether name matches any pattern. Uses
// gobwas/glob — same {*, ?, [abc], !x, +(a|b)} syntax legacy's
// micromatch supports. Patterns that fail to compile are
// silently skipped (the legacy never crashed on a bad glob; it
// just returned false for that pattern).
func matchAnyGlob(name string, patterns []string) bool {
	for _, pat := range patterns {
		g, err := glob.Compile(pat)
		if err != nil {
			continue
		}
		if g.Match(name) {
			return true
		}
	}
	return false
}
