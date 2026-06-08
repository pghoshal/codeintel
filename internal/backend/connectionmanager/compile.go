// Pure-function compile path that turns code-host repo
// descriptors into RepoData records. Direct port of
// packages/backend/src/repoCompileUtils.ts:
//
//   - createGitHubRepoRecord (lines 91-161)
//   - the per-repo loop in compileGithubConfig (lines 67-83)
//
// Live GitHub API fetch (getGitHubReposFromConfig) is deferred
// to Phase B.1c. The asynq handler that orchestrates the call
// site is deferred to Phase B.1d.
package connectionmanager

import (
	"net/url"
	"path"
	"strconv"
	"strings"
)

// CreateGitHubRepoRecordInput captures the legacy named-argument
// surface for createGitHubRepoRecord. Mirrors the destructuring
// pattern at repoCompileUtils.ts:91-105.
type CreateGitHubRepoRecordInput struct {
	Repo                  OctokitRepository
	HostURL               string
	Branches              []string
	Tags                  []string
	IsAutoCleanupDisabled *bool
	// OrgID falls back to SingleTenantOrgID when zero. Matches
	// the legacy default-arg semantics at line 97.
	OrgID int32
}

// CreateGitHubRepoRecord is the per-repo transformation. Pure
// function: same inputs → byte-equal outputs. Direct port of
// createGitHubRepoRecord (repoCompileUtils.ts:91-161).
func CreateGitHubRepoRecord(in CreateGitHubRepoRecordInput) RepoData {
	orgID := in.OrgID
	if orgID == 0 {
		orgID = SingleTenantOrgID
	}

	// repoNameRoot = new URL(hostUrl).toString().replace(/^https?:\/\//, '')
	// JS's URL.toString() reattaches a trailing slash for a
	// bare-host URL; Go's url.Parse + String preserves the input
	// shape. We match JS by:
	//   1. Parsing the URL.
	//   2. Producing the canonical String form.
	//   3. Stripping the leading http(s):// prefix.
	repoNameRoot := stripHTTPScheme(canonicalURLString(in.HostURL))

	repoDisplayName := in.Repo.FullName
	// path.Join collapses repeat slashes which matches JS
	// path.join semantics for this input shape.
	repoName := path.Join(repoNameRoot, repoDisplayName)

	// new URL(repo.clone_url!).toString()
	// JS's URL constructor canonicalises the input (adds a
	// trailing slash for bare hosts, lowercases the scheme/host,
	// etc.). For typical GitHub clone URLs the canonical form is
	// identical to the input; we still pipe through Go's
	// url.Parse to catch malformed inputs early.
	cloneURL := ""
	if in.Repo.CloneURL != nil {
		cloneURL = canonicalURLString(*in.Repo.CloneURL)
	}

	isPublic := !in.Repo.Private

	// Metadata.gitConfig values are stringified counts. Legacy
	// uses `(value ?? 0).toString()` for all four counters. Go
	// port produces the same digits via strconv.Itoa.
	gitConfig := map[string]string{
		"zoekt.web-url-type":       "github",
		"zoekt.web-url":            in.Repo.HTMLURL,
		"zoekt.name":               repoName,
		"zoekt.github-stars":       strconv.Itoa(ptrIntOrZero(in.Repo.StargazersCount)),
		"zoekt.github-watchers":    strconv.Itoa(ptrIntOrZero(in.Repo.WatchersCount)),
		"zoekt.github-subscribers": strconv.Itoa(ptrIntOrZero(in.Repo.SubscribersCount)),
		"zoekt.github-forks":       strconv.Itoa(ptrIntOrZero(in.Repo.ForksCount)),
		"zoekt.archived":           marshalBool(in.Repo.Archived),
		"zoekt.fork":               marshalBoolValue(in.Repo.Fork),
		"zoekt.public":             marshalBoolValue(isPublic),
		"zoekt.display-name":       repoDisplayName,
	}

	// metadata.codeHostMetadata.github.topics: legacy emits
	// `repo.topics ?? []`. Empty array surfaces as an empty
	// JSON array `[]`, not the field's absence — matching
	// legacy behaviour.
	topics := in.Repo.Topics
	if topics == nil {
		topics = []string{}
	}

	metadata := RepoMetadata{
		GitConfig: gitConfig,
		Branches:  in.Branches,
		Tags:      in.Tags,
		CodeHostMetadata: &CodeHostMetadata{
			GitHub: &GitHubMetadata{Topics: topics},
		},
	}

	// IsArchived mirrors `!!repo.archived` (legacy line 128) —
	// coerce nullable bool to false-on-null.
	isArchived := in.Repo.Archived != nil && *in.Repo.Archived

	record := RepoData{
		ExternalID:            strconv.FormatInt(in.Repo.ID, 10),
		ExternalCodeHostType:  "github",
		ExternalCodeHostURL:   in.HostURL,
		CloneURL:              cloneURL,
		WebURL:                in.Repo.HTMLURL,
		Name:                  repoName,
		DisplayName:           repoDisplayName,
		ImageURL:              in.Repo.Owner.AvatarURL,
		DefaultBranch:         in.Repo.DefaultBranch,
		IsFork:                in.Repo.Fork,
		IsArchived:            isArchived,
		IsPublic:              isPublic,
		IsAutoCleanupDisabled: in.IsAutoCleanupDisabled,
		OrgID:                 orgID,
		Metadata:              metadata,
	}
	return record
}

// CompileGitHubConfig is the per-repo loop from
// compileGithubConfig (repoCompileUtils.ts:57-89). Takes a
// pre-fetched OctokitRepository slice; the live GitHub API fetch
// lands in Phase B.1c.
//
// Each output record carries the supplied connectionID in its
// ConnectionIDs list — matching the legacy
// `connections: { create: { connectionId } }` nested Prisma
// write.
//
// HostURL handling: legacy line 65 does
// `(config.url ?? 'https://github.com').replace(/\/+$/, '')`.
// The Go port mirrors that — caller supplies the resolved
// HostURL, and we strip trailing slashes here.
func CompileGitHubConfig(repos []OctokitRepository, in GitHubCompileInput, connectionID int32) []RepoData {
	hostURL := stripTrailingSlashes(in.HostURL)
	if hostURL == "" {
		hostURL = "https://github.com"
	}

	out := make([]RepoData, 0, len(repos))
	for i := range repos {
		rec := CreateGitHubRepoRecord(CreateGitHubRepoRecordInput{
			Repo:     repos[i],
			HostURL:  hostURL,
			Branches: in.Branches,
			Tags:     in.Tags,
		})
		rec.ConnectionIDs = []int32{connectionID}
		out = append(out, rec)
	}
	return out
}

// stripHTTPScheme mirrors the legacy `replace(/^https?:\/\//, '')`.
// Case-insensitive match per the JS regex flag (default for
// http/https schemes is lowercase but a canonical URL always is).
func stripHTTPScheme(s string) string {
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return s[len("https://"):]
	case strings.HasPrefix(lower, "http://"):
		return s[len("http://"):]
	default:
		return s
	}
}

// canonicalURLString re-emits the URL through Go's url.Parse.
// For a well-formed URL the round-trip is the identity. For a
// malformed input we return the raw string unchanged so the
// legacy behaviour of "garbage-in, garbage-out" is preserved
// at this boundary (legacy `new URL(...)` would throw — the
// codeintel port doesn't have throw, so we keep the string).
func canonicalURLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return raw
	}
	return u.String()
}

// stripTrailingSlashes mirrors the legacy
// `.replace(/\/+$/, '')` — strip one-or-more trailing forward
// slashes.
func stripTrailingSlashes(s string) string {
	return strings.TrimRight(s, "/")
}

// marshalBool mirrors packages/backend/src/utils.ts marshalBool:
// `!!value ? '1' : '0'`. Returns "1" for non-nil true, "0"
// otherwise.
func marshalBool(b *bool) string {
	if b != nil && *b {
		return "1"
	}
	return "0"
}

// marshalBoolValue is the same as marshalBool but takes a value
// (not a pointer). Used for fields the legacy code reads as
// required booleans (e.g., repo.fork).
func marshalBoolValue(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ptrIntOrZero dereferences an optional integer pointer,
// returning 0 when nil. Matches the legacy `(value ?? 0)` form.
func ptrIntOrZero(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
