// GitLab orchestrator: stitches FetchProjectsForGroup, the
// (future) ShouldExcludeProject filter, and CompileGitLabConfig
// into a single high-level entrypoint paralleling
// GetGitHubReposFromConfig. Scope of this slice (B.4-iii):
//
//   - Groups discovery path only — Projects and Users paths
//     defer (same pattern as the github orchestrator's narrow
//     scope).
//   - Anonymous + token auth. No GitLab App / OAuth.
//   - Reuses GitHub's shouldExcludeRepo for the filter pass
//     (project path_with_namespace + topics map cleanly to the
//     same shape). A gitlab-specific shouldExcludeProject port
//     lands when its semantics diverge.
package connectionmanager

import (
	"context"
	"fmt"
	"net/url"

	gitlab "github.com/xanzy/go-gitlab"
)

// gitLabCloudHostname is the legacy GITLAB_CLOUD_HOSTNAME =
// "gitlab.com" default.
const gitLabCloudHostname = "gitlab.com"

// GetReposResultGitLab carries the gitlab orchestrator's output:
// a []GitLabProject slice (so the caller can run the compile
// step) + the accumulated warnings.
type GetReposResultGitLab struct {
	Projects []GitLabProject
	Warnings []string
}

// GetGitLabProjectsFromConfig is the gitlab equivalent of
// GetGitHubReposFromConfig. Builds a *gitlab.Client (anonymous
// or token-authed, optionally pointing at a self-hosted base
// URL), loops FetchProjectsForGroup over cfg.Groups, flattens
// 404 warnings.
func GetGitLabProjectsFromConfig(ctx context.Context, cfg GitLabConnectionConfig) (GetReposResultGitLab, error) {
	client, err := buildGitLabClient(cfg)
	if err != nil {
		return GetReposResultGitLab{}, fmt.Errorf("buildGitLabClient: %w", err)
	}

	var (
		projects []GitLabProject
		warnings []string
	)

	for _, group := range cfg.Groups {
		groupProjects, warn, fetchErr := FetchProjectsForGroup(ctx, client, group)
		if fetchErr != nil {
			return GetReposResultGitLab{}, fmt.Errorf("FetchProjectsForGroup %q: %w", group, fetchErr)
		}
		if warn != nil {
			warnings = append(warnings, warn.Message)
			continue
		}
		projects = append(projects, groupProjects...)
	}

	return GetReposResultGitLab{Projects: projects, Warnings: warnings}, nil
}

// buildGitLabClient mirrors buildGitHubClient: anonymous or
// token-authed, anonymous-cloud or self-hosted base URL.
func buildGitLabClient(cfg GitLabConnectionConfig) (*gitlab.Client, error) {
	var (
		c   *gitlab.Client
		err error
	)
	if cfg.URL != "" {
		if _, perr := url.Parse(cfg.URL); perr != nil {
			return nil, fmt.Errorf("invalid URL %q: %w", cfg.URL, perr)
		}
		// go-gitlab takes a base URL that ends in /api/v4.
		base := stripTrailingSlashes(cfg.URL) + "/api/v4"
		if cfg.Token != "" {
			c, err = gitlab.NewClient(cfg.Token, gitlab.WithBaseURL(base))
		} else {
			c, err = gitlab.NewClient("", gitlab.WithBaseURL(base))
		}
	} else {
		if cfg.Token != "" {
			c, err = gitlab.NewClient(cfg.Token)
		} else {
			c, err = gitlab.NewClient("")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("gitlab.NewClient: %w", err)
	}
	return c, nil
}

// ResolveGitLabHostURL is the analog of ResolveHostURL. Legacy
// defaults to https://gitlab.com when config.url is unset; we
// preserve.
func ResolveGitLabHostURL(cfg GitLabConnectionConfig) string {
	if cfg.URL == "" {
		return "https://" + gitLabCloudHostname
	}
	return stripTrailingSlashes(cfg.URL)
}

// CompileGitLabFromConfig is the gitlab equivalent of
// CompileFromConfig: fetch -> compile -> []RepoData ready for
// upsert. The asynq handler's dispatch (Phase B.4-iii main
// slice) calls this when connectionType == "gitlab".
func CompileGitLabFromConfig(ctx context.Context, cfg GitLabConnectionConfig, connectionID int32) ([]RepoData, []string, error) {
	fetched, err := GetGitLabProjectsFromConfig(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	in := GitHubCompileInput{
		HostURL: ResolveGitLabHostURL(cfg),
	}
	if cfg.Revisions != nil {
		in.Branches = cfg.Revisions.Branches
		in.Tags = cfg.Revisions.Tags
	}
	return CompileGitLabConfig(fetched.Projects, in, connectionID), fetched.Warnings, nil
}
