// GitHub API fetch path. Direct port of getReposForOrgs
// (github.ts:370-431). Per-org pagination using
// github.com/google/go-github/v75; the legacy octokit call is
// `octokit.paginate.iterator(repos.listForOrg)`. The Go port
// follows the same per_page=100 page size + checks the context
// between pages.
//
// Auth / GitHub App / retry are intentionally NOT in scope for
// this slice — the caller passes in a *github.Client and is
// responsible for its construction (token, base URL, retry
// wrapper). Phase B.1c-iii lands the orchestrator that builds
// the client from a connection config and threads retry.
package connectionmanager

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/go-github/v75/github"
)

// reposPerPage is the legacy octokit page size (line 384).
// Preserved verbatim for parity — drift here changes the
// pagination request shape on the wire (per_page query
// parameter).
const reposPerPage = 100

// FetchWarning is the per-call non-fatal warning shape. The
// legacy returns `{ type: 'warning', warning: string }`; the
// Go port surfaces this as a typed sentinel that callers
// flatten into the broader `warnings []string` accumulator.
type FetchWarning struct {
	Message string
}

// ErrFetchAborted is the typed error the legacy DOMException
// 'AbortError' maps to. errors.Is-friendly so callers can
// distinguish operator-cancelled from real errors.
var ErrFetchAborted = errors.New("connectionmanager: fetch aborted")

// FetchReposForOrg lists every repo in `org` via the
// authenticated github.Client. Pagination is server-side via
// the `per_page=100` + Page bumping pattern; between pages we
// re-check ctx.Err() to honour cancellation.
//
// Returns:
//   - (repos, nil, nil) on success.
//   - (nil, &FetchWarning, nil) when GitHub returns 404 for the
//     org (matches legacy line 412 — operator-visible warning,
//     not a hard error).
//   - (nil, nil, err) on transport / non-404 HTTP errors.
//   - (nil, nil, ErrFetchAborted) when ctx is cancelled
//     mid-page.
func FetchReposForOrg(ctx context.Context, client *github.Client, org string) ([]OctokitRepository, *FetchWarning, error) {
	if client == nil {
		return nil, nil, errors.New("connectionmanager: github.Client is nil")
	}
	if org == "" {
		return nil, nil, errors.New("connectionmanager: org name is empty")
	}

	opts := &github.RepositoryListByOrgOptions{
		ListOptions: github.ListOptions{PerPage: reposPerPage},
	}

	var allRepos []OctokitRepository
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrFetchAborted, err)
		}
		page, resp, err := client.Repositories.ListByOrg(ctx, org, opts)
		if err != nil {
			// 404 → typed warning.
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil, &FetchWarning{Message: fmt.Sprintf("Organization %s not found or no access", org)}, nil
			}
			return nil, nil, fmt.Errorf("FetchReposForOrg: listForOrg %q: %w", org, err)
		}
		for i := range page {
			allRepos = append(allRepos, mapGitHubRepo(page[i]))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allRepos, nil, nil
}

// mapGitHubRepo flattens the go-github *github.Repository (a
// pointer-soup struct mirroring the API's deep optional shape)
// into our OctokitRepository. Each field maps 1:1 — drift here
// is the canonical place where Octokit semantics surface as our
// types.
//
// Pointer-typed legacy fields (clone_url, default_branch, etc.)
// preserve their nullability from the upstream API; required
// fields default to the Go zero value when go-github returns nil
// (legacy octokit semantics with destructuring + `??` defaults).
func mapGitHubRepo(r *github.Repository) OctokitRepository {
	if r == nil {
		return OctokitRepository{}
	}
	out := OctokitRepository{
		Name:             stringVal(r.Name),
		ID:               int64Val(r.ID),
		FullName:         stringVal(r.FullName),
		Fork:             boolVal(r.Fork),
		Private:          boolVal(r.Private),
		HTMLURL:          stringVal(r.HTMLURL),
		CloneURL:         r.CloneURL,
		StargazersCount:  intPtrFromIntPtr(r.StargazersCount),
		WatchersCount:    intPtrFromIntPtr(r.WatchersCount),
		SubscribersCount: intPtrFromIntPtr(r.SubscribersCount),
		DefaultBranch:    r.DefaultBranch,
		ForksCount:       intPtrFromIntPtr(r.ForksCount),
		Archived:         r.Archived,
		Topics:           r.Topics,
		Size:             intPtrFromIntPtr(r.Size),
	}
	if r.Owner != nil {
		out.Owner = OctokitOwner{
			AvatarURL: stringVal(r.Owner.AvatarURL),
			Login:     stringVal(r.Owner.Login),
		}
	}
	return out
}

func stringVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func boolVal(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

func int64Val(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// intPtrFromIntPtr converts go-github's *int field to our *int.
// Both are pointer-typed so the value passes through; the
// indirection exists to centralise the nil-check + signature
// match.
func intPtrFromIntPtr(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
