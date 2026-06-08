// GitHub orchestrator: stitches the FetchReposForOrg fetcher,
// ShouldExcludeRepo filter, and CompileGitHubConfig compile
// path into a single high-level entrypoint that matches the
// legacy getGitHubReposFromConfig (github.ts:151-223).
//
// Scope of this slice (Phase B.1c-iii):
//
//   - Orgs discovery path only — config.repos and config.users
//     paths land in later slices. The orgs path is the most
//     common case and is the smallest viable end-to-end.
//   - Anonymous + token-based auth. No GitHub App (defer to
//     B.1c-iv).
//   - No isAuthenticated() pre-check (defer; the live API call
//     itself surfaces auth errors).
//   - No retry wrapper (defer to B.1c-v); transient errors
//     surface to the caller.
package connectionmanager

import (
	"context"
	"fmt"
	"net/url"

	"github.com/google/go-github/v75/github"
)

// gitHubCloudHostname mirrors the legacy
// `GITHUB_CLOUD_HOSTNAME = "github.com"` constant
// (github.ts:14). Anonymous/cloud requests use this default
// when config.url is unset.
const gitHubCloudHostname = "github.com"

// GitHubConnectionConfig captures the legacy
// GithubConnectionConfig (@schemas/v3/github.type) subset this
// orchestrator reads. Every Zod-schema-validated field in the
// legacy maps to one Go field; fields not used by the orgs
// path are intentionally omitted and land in later slices.
type GitHubConnectionConfig struct {
	// URL is the optional self-hosted GitHub Enterprise base
	// URL (e.g., "https://github.example.com"). Unset means
	// public github.com.
	URL string `json:"url,omitempty"`

	// Token is the optional bearer token. Caller resolves any
	// secret-store dereferences (legacy
	// `getTokenFromConfig(config.token)`) before calling the
	// orchestrator.
	Token string `json:"token,omitempty"`

	// Orgs is the list of org slugs to enumerate.
	Orgs []string `json:"orgs,omitempty"`

	// Topics is the include.topics filter — a repo must carry
	// at least one matching topic to be kept.
	Topics []string `json:"topics,omitempty"`

	// Exclude is the negative-match filter.
	Exclude *GitHubExcludeRules `json:"exclude,omitempty"`

	// Revisions carries the optional branch / tag globs that
	// land in the emitted RepoData.Metadata.Branches / Tags.
	Revisions *GitHubRevisions `json:"revisions,omitempty"`
}

// GitHubRevisions mirrors the legacy `config.revisions` shape.
type GitHubRevisions struct {
	Branches []string `json:"branches,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// GetReposResult is the legacy `{ repos, warnings }` return
// shape.
type GetReposResult struct {
	Repos    []OctokitRepository
	Warnings []string
}

// GetGitHubReposFromConfig is the direct port of
// getGitHubReposFromConfig (github.ts:151-223) — the
// orchestrator the caller (connection-sync worker) invokes
// with a parsed connection config + a context. Returns the
// list of OctokitRepository records the compile path should
// turn into RepoData rows, plus any non-fatal warnings (404
// orgs etc.) accumulated along the way.
//
// Auth path: if cfg.Token is non-empty, the client is
// constructed with a bearer-token transport; otherwise the
// anonymous tier is used. Self-hosted Enterprise URLs are
// supported via go-github's NewEnterpriseClient — the same
// pattern as legacy `octokit({ baseUrl, auth })`.
func GetGitHubReposFromConfig(ctx context.Context, cfg GitHubConnectionConfig) (GetReposResult, error) {
	client, err := buildGitHubClient(cfg)
	if err != nil {
		return GetReposResult{}, fmt.Errorf("buildGitHubClient: %w", err)
	}

	var (
		repos    []OctokitRepository
		warnings []string
	)

	// Orgs discovery loop. Legacy lines 186-190.
	for _, org := range cfg.Orgs {
		orgRepos, warn, fetchErr := FetchReposForOrg(ctx, client, org)
		if fetchErr != nil {
			return GetReposResult{}, fmt.Errorf("FetchReposForOrg %q: %w", org, fetchErr)
		}
		if warn != nil {
			warnings = append(warnings, warn.Message)
			continue
		}
		repos = append(repos, orgRepos...)
	}

	// Filter via ShouldExcludeRepo. Legacy lines 204-215.
	filtered := repos[:0]
	include := &GitHubIncludeRules{Topics: cfg.Topics}
	for i := range repos {
		excluded := ShouldExcludeRepo(ShouldExcludeRepoInput{
			Repo:    repos[i],
			Include: include,
			Exclude: cfg.Exclude,
		})
		if !excluded {
			filtered = append(filtered, repos[i])
		}
	}

	return GetReposResult{Repos: filtered, Warnings: warnings}, nil
}

// buildGitHubClient constructs a *github.Client honoring the
// legacy `createOctokitFromToken({ token, url })` semantics:
//
//   - No URL + no token → anonymous github.com.
//   - URL set         → Enterprise endpoint via NewEnterpriseClient.
//   - Token set       → bearer-token auth via WithAuthToken.
//   - URL + Token     → both, in either order.
//
// Returns an error only for malformed Enterprise URLs.
func buildGitHubClient(cfg GitHubConnectionConfig) (*github.Client, error) {
	c := github.NewClient(nil)
	if cfg.Token != "" {
		c = c.WithAuthToken(cfg.Token)
	}
	if cfg.URL == "" {
		return c, nil
	}
	// Enterprise. go-github's WithEnterpriseURLs takes
	// baseURL + uploadURL and normalises both. uploadURL =
	// baseURL covers the legacy single-endpoint case.
	if _, err := url.Parse(cfg.URL); err != nil {
		return nil, fmt.Errorf("invalid URL %q: %w", cfg.URL, err)
	}
	ent, err := c.WithEnterpriseURLs(cfg.URL, cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("WithEnterpriseURLs %q: %w", cfg.URL, err)
	}
	return ent, nil
}

// ResolveHostURL mirrors the legacy line 152-154 / 65 pattern:
// derive the trailing-slash-stripped host URL from the optional
// config.url; default to "https://github.com" for cloud. The
// compile path uses this for the ExternalCodeHostURL column.
func ResolveHostURL(cfg GitHubConnectionConfig) string {
	if cfg.URL == "" {
		return "https://" + gitHubCloudHostname
	}
	return stripTrailingSlashes(cfg.URL)
}

// CompileFromConfig is the high-level "fetch then compile"
// convenience the worker calls. Builds RepoData records ready
// for upsert. Equivalent to the full
// compileGithubConfig (repoCompileUtils.ts:57-89) flow
// minus the connection-binding-via-Prisma-nested-write (Go side
// records the connectionID list).
func CompileFromConfig(ctx context.Context, cfg GitHubConnectionConfig, connectionID int32) ([]RepoData, []string, error) {
	fetched, err := GetGitHubReposFromConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	in := GitHubCompileInput{
		HostURL: ResolveHostURL(cfg),
	}
	if cfg.Revisions != nil {
		in.Branches = cfg.Revisions.Branches
		in.Tags = cfg.Revisions.Tags
	}
	return CompileGitHubConfig(fetched.Repos, in, connectionID), fetched.Warnings, nil
}
