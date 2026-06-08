// GitLab API fetcher. Direct port of getGitLabReposFromConfig's
// group-listing path (gitlab.ts). Single-group-at-a-time
// function using github.com/xanzy/go-gitlab. Maps the gitlab
// client's *gitlab.Project struct into our GitLabProject shape.
//
// Same scope split as the GitHub side:
//   - This slice (B.4-ii): per-group enumeration.
//   - B.4-iii: handler dispatch on connectionType.
//   - Later slice: user-listing / project-listing discovery
//     paths; GitLab App / OAuth flows; retry policy.
package connectionmanager

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	gitlab "github.com/xanzy/go-gitlab"
)

// gitlabProjectsPerPage is the legacy page size used by
// @gitbeaker. Preserved for parity — drift here changes the
// per_page query parameter.
const gitlabProjectsPerPage = 100

// FetchProjectsForGroup lists every project in `group` via the
// authenticated gitlab.Client. Pagination is the per_page+page
// loop pattern matching the github helper.
//
// Returns:
//   - (projects, nil, nil) on success.
//   - (nil, &FetchWarning, nil) when GitLab returns 404 (the
//     legacy gitlab.ts 404 branch surfaces as a warning).
//   - (nil, nil, err) on other errors.
func FetchProjectsForGroup(ctx context.Context, client *gitlab.Client, group string) ([]GitLabProject, *FetchWarning, error) {
	if client == nil {
		return nil, nil, errors.New("connectionmanager: gitlab.Client is nil")
	}
	if group == "" {
		return nil, nil, errors.New("connectionmanager: group name is empty")
	}

	opts := &gitlab.ListGroupProjectsOptions{
		ListOptions: gitlab.ListOptions{PerPage: gitlabProjectsPerPage},
		// IncludeSubGroups true matches the legacy default of
		// flattening sub-groups into the top-level listing —
		// otherwise nested projects in subgroups go missing.
		IncludeSubGroups: gitlab.Ptr(true),
	}

	var allProjects []GitLabProject
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrFetchAborted, err)
		}
		page, resp, err := client.Groups.ListGroupProjects(group, opts, gitlab.WithContext(ctx))
		if err != nil {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				return nil, &FetchWarning{
					Message: fmt.Sprintf("Group %s not found or no access", group),
				}, nil
			}
			return nil, nil, fmt.Errorf("FetchProjectsForGroup %q: %w", group, err)
		}
		for i := range page {
			allProjects = append(allProjects, mapGitLabProject(page[i]))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return allProjects, nil, nil
}

// mapGitLabProject flattens the go-gitlab *gitlab.Project into
// our GitLabProject shape. The go-gitlab struct is value-typed
// (not pointer-soup like go-github), so the mapping is simpler
// than the github mirror: copy fields by value, capture
// fork-detection presence by checking ForkedFromProject != nil.
func mapGitLabProject(p *gitlab.Project) GitLabProject {
	if p == nil {
		return GitLabProject{}
	}
	out := GitLabProject{
		ID:                int64(p.ID),
		Name:              p.Name,
		PathWithNamespace: p.PathWithNamespace,
		HTTPURLToRepo:     p.HTTPURLToRepo,
		Visibility:        string(p.Visibility),
		Topics:            p.Topics,
	}
	if p.DefaultBranch != "" {
		s := p.DefaultBranch
		out.DefaultBranch = &s
	}
	archived := p.Archived
	out.Archived = &archived
	if p.StarCount != 0 {
		stars := p.StarCount
		out.StargazersCount = &stars
	}
	if p.ForksCount != 0 {
		forks := p.ForksCount
		out.ForksCount = &forks
	}
	if p.AvatarURL != "" {
		s := p.AvatarURL
		out.AvatarURL = &s
	}
	if p.ForkedFromProject != nil {
		// Presence signal — concrete value isn't used by the
		// compile path; carry the struct pointer through.
		out.ForkedFromProject = p.ForkedFromProject
	}
	return out
}
