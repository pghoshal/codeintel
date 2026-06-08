package connectionmanager

import (
	"strings"

	"github.com/gobwas/glob"
)

// GitHubExcludeRules is the legacy `exclude` shape on the
// GithubConnectionConfig. Pointer-typed flags mirror legacy
// `boolean?` optional-bool semantics: nil == "not specified".
type GitHubExcludeRules struct {
	Forks    *bool       `json:"forks,omitempty"`
	Archived *bool       `json:"archived,omitempty"`
	Repos    []string    `json:"repos,omitempty"`
	Topics   []string    `json:"topics,omitempty"`
	Size     *GitHubSize `json:"size,omitempty"`
}

// GitHubSize captures the legacy `exclude.size` shape:
// optional min + max byte bounds. Pointer-typed because either
// or both may be unset.
type GitHubSize struct {
	Min *int64 `json:"min,omitempty"`
	Max *int64 `json:"max,omitempty"`
}

// GitHubIncludeRules carries the positive-match topics from the
// connection config. Legacy `include.topics` field is the only
// member.
type GitHubIncludeRules struct {
	Topics []string `json:"topics,omitempty"`
}

// ShouldExcludeRepoInput is the legacy named-argument shape from
// shouldExcludeRepo (github.ts:483-493).
type ShouldExcludeRepoInput struct {
	Repo    OctokitRepository
	Include *GitHubIncludeRules
	Exclude *GitHubExcludeRules
}

// ShouldExcludeRepo is the per-repo filter the compile path
// runs before emitting RepoData. Pure-function port of
// shouldExcludeRepo (github.ts:483-567). Returns true to skip
// the repo. Reasons for each branch land in the structured
// log; this port preserves the seven branches verbatim:
//
//  1. clone_url is undefined → exclude.
//  2. exclude.forks is true AND repo.fork → exclude.
//  3. exclude.archived is true AND repo.archived → exclude.
//  4. exclude.repos pattern matches full_name → exclude.
//  5. exclude.topics pattern matches any of repo.topics → exclude.
//  6. include.topics is set AND no repo.topics matches any
//     pattern → exclude.
//  7. exclude.size set AND repo.size (in BYTES, legacy multiplies
//     by 1000 because repo.size is in KB) falls outside min/max
//     bounds → exclude.
//
// Pattern matching uses github.com/gobwas/glob — equivalent to
// the legacy micromatch.isMatch with `*`/`**`/`?` semantics.
// Case-fold on topics matches legacy lowercasing.
func ShouldExcludeRepo(in ShouldExcludeRepoInput) bool {
	repo := in.Repo

	// 1. clone_url missing → exclude.
	if repo.CloneURL == nil || *repo.CloneURL == "" {
		return true
	}

	// 2. exclude.forks
	if in.Exclude != nil && truePtr(in.Exclude.Forks) && repo.Fork {
		return true
	}

	// 3. exclude.archived
	if in.Exclude != nil && truePtr(in.Exclude.Archived) && truePtr(repo.Archived) {
		return true
	}

	// 4. exclude.repos pattern match against full_name.
	if in.Exclude != nil && len(in.Exclude.Repos) > 0 {
		if matchAny(repo.FullName, in.Exclude.Repos, false) {
			return true
		}
	}

	// 5. exclude.topics: any repo-topic matches any config-topic
	// pattern → exclude. Case-folded on both sides per legacy
	// `topic.toLowerCase()`.
	if in.Exclude != nil && len(in.Exclude.Topics) > 0 {
		repoTopics := nilSliceOrSelf(repo.Topics)
		for _, repoTopic := range repoTopics {
			if matchAny(repoTopic, in.Exclude.Topics, true) {
				return true
			}
		}
	}

	// 6. include.topics: at least one repo-topic must match at
	// least one config-topic pattern. Missing match → exclude.
	if in.Include != nil && len(in.Include.Topics) > 0 {
		repoTopics := nilSliceOrSelf(repo.Topics)
		hit := false
		for _, repoTopic := range repoTopics {
			if matchAny(repoTopic, in.Include.Topics, true) {
				hit = true
				break
			}
		}
		if !hit {
			return true
		}
	}

	// 7. exclude.size: legacy multiplies repo.size (KB) by 1000
	// to get bytes before comparing. Reproduce verbatim.
	if in.Exclude != nil && in.Exclude.Size != nil && repo.Size != nil {
		bytes := int64(*repo.Size) * 1000
		if in.Exclude.Size.Min != nil && bytes < *in.Exclude.Size.Min {
			return true
		}
		if in.Exclude.Size.Max != nil && bytes > *in.Exclude.Size.Max {
			return true
		}
	}

	return false
}

// truePtr is the `!!boolPtr` equivalent: returns true iff the
// pointer is non-nil and dereferences to true.
func truePtr(b *bool) bool {
	return b != nil && *b
}

// nilSliceOrSelf normalises a nil slice to the empty value so
// callers can range without a separate nil-check. Matches the
// legacy `repo.topics ?? []` pattern.
func nilSliceOrSelf(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// matchAny returns true if subject matches any of the supplied
// glob patterns. When caseFold is true, both subject and each
// pattern are lowercased before compilation — matches legacy
// `topic.toLowerCase()` behaviour for the topics filters.
//
// A pattern that fails to compile is treated as a literal
// match (i.e., exact-string compare) — same as micromatch's
// fallback when given a malformed pattern.
func matchAny(subject string, patterns []string, caseFold bool) bool {
	subj := subject
	if caseFold {
		subj = strings.ToLower(subj)
	}
	for _, p := range patterns {
		pat := p
		if caseFold {
			pat = strings.ToLower(pat)
		}
		if g, err := glob.Compile(pat); err == nil {
			if g.Match(subj) {
				return true
			}
			continue
		}
		// Pattern failed to compile — fall back to literal compare.
		if subj == pat {
			return true
		}
	}
	return false
}
