// Gitea API fetcher. Direct port of getGiteaReposFromConfig
// (gitea.ts:15-X)'s org-listing path using code.gitea.io/sdk/gitea.
//
// On import, init() registers the SDK-backed fetcher with the
// orchestrator's package-level slot so any "gitea" connection
// dispatched by the handler routes through here. This keeps the
// codeintel-backend binary self-contained — no caller wiring
// required.
package connectionmanager

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"code.gitea.io/sdk/gitea"
)

// gitea pagination page size. Preserved from the legacy default.
const giteaReposPerPage = 50

// FetchReposForGiteaOrg lists every repo in `org` via the
// authenticated gitea.Client. Same shape as FetchReposForOrg
// (github) and FetchProjectsForGroup (gitlab). Each page fetch
// is wrapped in WithRetry so a transient 5xx / network blip
// doesn't fail the entire enumeration.
func FetchReposForGiteaOrg(ctx context.Context, client *gitea.Client, org string) ([]GiteaRepo, *FetchWarning, error) {
	if client == nil {
		return nil, nil, errors.New("connectionmanager: gitea.Client is nil")
	}
	if org == "" {
		return nil, nil, errors.New("connectionmanager: org name is empty")
	}

	page := 1
	var all []GiteaRepo
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrFetchAborted, err)
		}
		type result struct {
			repos []*gitea.Repository
			resp  *gitea.Response
			warn  *FetchWarning
		}
		currentPage := page
		res, err := WithRetry(ctx, RetryConfig{}, func(ctx context.Context, attempt int) (result, error) {
			repos, resp, perr := client.ListOrgRepos(org, gitea.ListOrgReposOptions{
				ListOptions: gitea.ListOptions{Page: currentPage, PageSize: giteaReposPerPage},
			})
			if perr != nil {
				if resp != nil && resp.StatusCode == http.StatusNotFound {
					return result{warn: &FetchWarning{Message: fmt.Sprintf("Org %s not found or no access", org)}}, nil
				}
				if resp != nil && resp.StatusCode >= 500 {
					return result{}, &RetryableHTTPError{StatusCode: resp.StatusCode, URL: org, Body: perr.Error()}
				}
				return result{}, fmt.Errorf("ListOrgRepos %q page=%d: %w", org, currentPage, perr)
			}
			return result{repos: repos, resp: resp}, nil
		})
		if err != nil {
			return nil, nil, err
		}
		if res.warn != nil {
			return nil, res.warn, nil
		}
		repos, resp := res.repos, res.resp
		for i := range repos {
			all = append(all, mapGiteaRepo(repos[i]))
		}
		// Stop when the page is short (gitea returns < pageSize
		// only on the last page) or when the response Link header
		// has no next.
		if len(repos) < giteaReposPerPage {
			break
		}
		if resp != nil && resp.NextPage > 0 {
			page = resp.NextPage
			continue
		}
		page++
	}
	return all, nil, nil
}

// mapGiteaRepo flattens *gitea.Repository into our GiteaRepo.
func mapGiteaRepo(r *gitea.Repository) GiteaRepo {
	if r == nil {
		return GiteaRepo{}
	}
	out := GiteaRepo{
		ID:       r.ID,
		Name:     r.Name,
		FullName: r.FullName,
		Fork:     r.Fork,
		Private:  r.Private,
		Internal: r.Internal,
		HTMLURL:  r.HTMLURL,
	}
	if r.CloneURL != "" {
		s := r.CloneURL
		out.CloneURL = &s
	}
	if r.DefaultBranch != "" {
		s := r.DefaultBranch
		out.DefaultBranch = &s
	}
	archived := r.Archived
	out.Archived = &archived
	if r.Owner != nil {
		out.Owner = &GiteaOwner{AvatarURL: r.Owner.AvatarURL}
	}
	return out
}

// buildGiteaClient mirrors buildGitHubClient: anonymous or
// token-authed, optional self-hosted base URL.
func buildGiteaClient(ctx context.Context, cfg GiteaConnectionConfig) (*gitea.Client, error) {
	base := stripTrailingSlashes(cfg.URL)
	if base == "" {
		base = "https://gitea.com"
	}
	opts := []gitea.ClientOption{gitea.SetContext(ctx)}
	if cfg.Token != "" {
		opts = append(opts, gitea.SetToken(cfg.Token))
	}
	c, err := gitea.NewClient(base, opts...)
	if err != nil {
		return nil, fmt.Errorf("gitea.NewClient: %w", err)
	}
	return c, nil
}

// fetchGiteaReposViaSDK is the live fetcher registered with
// SetGiteaFetcher in init(). Builds a client, loops over the
// config's orgs, accumulates warnings.
func fetchGiteaReposViaSDK(ctx context.Context, cfg GiteaConnectionConfig) (giteaFetchResult, error) {
	client, err := buildGiteaClient(ctx, cfg)
	if err != nil {
		return giteaFetchResult{}, err
	}
	var (
		all      []GiteaRepo
		warnings []string
	)
	for _, org := range cfg.Orgs {
		repos, warn, fetchErr := FetchReposForGiteaOrg(ctx, client, org)
		if fetchErr != nil {
			return giteaFetchResult{}, fmt.Errorf("FetchReposForGiteaOrg %q: %w", org, fetchErr)
		}
		if warn != nil {
			warnings = append(warnings, warn.Message)
			continue
		}
		all = append(all, repos...)
	}
	return giteaFetchResult{Repos: all, Warnings: warnings}, nil
}

func init() {
	// Register the SDK-backed fetcher so the handler's gitea
	// dispatch branch routes through it. The orchestrator's
	// nil-fetcher branch surfaces a typed error if this init
	// hasn't run — guards against the package being imported
	// for its types only.
	SetGiteaFetcher(fetchGiteaReposViaSDK)
}
