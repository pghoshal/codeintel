// Branch-level status helpers. Direct port of indexStatus.ts:
//   - BuildBranchIndexStatus  (lines 174-209)
//   - BuildKnownBranchIndexStatuses (lines 211-228)
//
// Same split model as BuildRepoIndexSummary: per-branch
// `activeIndexStatus` reflects whether THIS branch's revision is
// in indexedRevisions, independent of the latest job's state.
package repoindexstatus

import (
	"regexp"
)

// BranchIndexStatus mirrors the legacy struct (indexStatus.ts:22-39).
// JSON tags reproduce the legacy property names verbatim.
type BranchIndexStatus struct {
	Branch                     string            `json:"branch"`
	Revision                   string            `json:"revision"`
	AllowedByPolicy            bool              `json:"allowedByPolicy"`
	Indexed                    bool              `json:"indexed"`
	Status                     RepoIndexState    `json:"status"`
	Color                      RepoIndexColor    `json:"color"`
	ActiveIndexStatus          RepoIndexState    `json:"activeIndexStatus"`
	ActiveIndexStatusColor     RepoIndexColor    `json:"activeIndexStatusColor"`
	ActiveIndexUsable          bool              `json:"activeIndexUsable"`
	LatestRunStatus            RepoIndexRunState `json:"latestRunStatus"`
	LatestRunStatusColor       RepoIndexColor    `json:"latestRunStatusColor"`
	LatestRunBlocksActiveIndex bool              `json:"latestRunBlocksActiveIndex"`

	// Optional fields — emitted only when present (legacy uses
	// `undefined` to omit; Go uses pointer + omitempty).
	IndexedAt       *string    `json:"indexedAt,omitempty"`
	LatestJobID     *string    `json:"latestJobId,omitempty"`
	LatestJobType   *JobType   `json:"latestJobType,omitempty"`
	LatestJobStatus *JobStatus `json:"latestJobStatus,omitempty"`
}

// BuildBranchIndexStatus is the per-branch version of
// BuildRepoIndexSummary. Direct port of indexStatus.ts:174-209.
func BuildBranchIndexStatus(repo RepoInput, branch string, latestJob *LatestJob) BranchIndexStatus {
	branchName := StripBranchRevision(branch)
	revision := BranchRevision(branchName)
	indexedRevisions := GetPolicyVisibleIndexedRevisions(repo)
	allowed := IsBranchAllowedByIndexPolicy(repo, branchName)

	indexed := allowed && repo.IndexedAt != nil && containsString(indexedRevisions, revision)
	activeIndexStatus := StateNotIndexed
	if indexed {
		activeIndexStatus = StateIndexed
	}
	latestRunStatus := getLatestRunState(repo, latestJob)
	status := resolveVisibleStatus(activeIndexStatus, latestRunStatus)

	out := BranchIndexStatus{
		Branch:                 branchName,
		Revision:               revision,
		AllowedByPolicy:        allowed,
		Indexed:                indexed,
		Status:                 status,
		Color:                  colorForState(status),
		ActiveIndexStatus:      activeIndexStatus,
		ActiveIndexStatusColor: colorForState(activeIndexStatus),
		ActiveIndexUsable:      activeIndexStatus == StateIndexed,
		LatestRunStatus:        latestRunStatus,
		LatestRunStatusColor:   colorForRunState(latestRunStatus),
		LatestRunBlocksActiveIndex: latestRunStatus == RunIndexing ||
			latestRunStatus == RunRemoving ||
			(latestRunStatus == RunFailed && activeIndexStatus != StateIndexed),
	}
	if indexed && repo.IndexedAt != nil {
		s := *repo.IndexedAt
		out.IndexedAt = &s
	}
	if latestJob != nil {
		id := latestJob.ID
		ty := latestJob.Type
		st := latestJob.Status
		out.LatestJobID = &id
		out.LatestJobType = &ty
		out.LatestJobStatus = &st
	}
	return out
}

// globMetaRE matches the glob meta characters legacy filters
// out (indexStatus.ts:220). When a "branch" entry in metadata
// contains any of these it's treated as a pattern, not a
// concrete branch — and excluded from the known-branches union.
var globMetaRE = regexp.MustCompile(`[*?\[\]{}()!+@]`)

// BuildKnownBranchIndexStatuses enumerates the union of:
//   - defaultBranch (if any)
//   - configured branches from metadata.branches that are NOT
//     globs (no *?\[\]{}()!+@)
//   - indexed branches from metadata.indexedRevisions
//
// and maps each through BuildBranchIndexStatus. Duplicates are
// de-duplicated preserving first-seen order. Direct port of
// indexStatus.ts:211-228.
func BuildKnownBranchIndexStatuses(repo RepoInput, latestJob *LatestJob) []BranchIndexStatus {
	md := ParseMetadata(repo.Metadata)
	indexedBranchNames := make([]string, 0, len(md.IndexedRevisions))
	for _, rev := range md.IndexedRevisions {
		if !startsWith(rev, "refs/heads/") {
			continue
		}
		indexedBranchNames = append(indexedBranchNames, StripBranchRevision(rev))
	}
	configuredBranchNames := make([]string, 0, len(md.Branches))
	for _, b := range md.Branches {
		if b == "*" || globMetaRE.MatchString(b) {
			continue
		}
		configuredBranchNames = append(configuredBranchNames, b)
	}

	candidates := make([]string, 0, 1+len(configuredBranchNames)+len(indexedBranchNames))
	if repo.DefaultBranch != nil && *repo.DefaultBranch != "" {
		candidates = append(candidates, *repo.DefaultBranch)
	}
	candidates = append(candidates, configuredBranchNames...)
	candidates = append(candidates, indexedBranchNames...)

	// De-dup preserving first-seen order. Legacy uses
	// [...new Set(candidates)] which preserves insertion order
	// per the JS spec — our manual loop matches that.
	seen := make(map[string]struct{}, len(candidates))
	out := make([]BranchIndexStatus, 0, len(candidates))
	for _, b := range candidates {
		if _, ok := seen[b]; ok {
			continue
		}
		seen[b] = struct{}{}
		out = append(out, BuildBranchIndexStatus(repo, b, latestJob))
	}
	return out
}

// startsWith is a fast prefix check that avoids the strings
// package overhead in a hot loop.
func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
