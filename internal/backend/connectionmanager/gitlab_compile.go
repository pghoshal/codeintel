// GitLab pure-function compile path. Direct port of
// compileGitlabConfig (repoCompileUtils.ts:163-248) — the
// per-project transformation that turns GitLab API project
// shapes into RepoData records.
//
// Live GitLab API fetch (getGitLabReposFromConfig) is deferred
// to Phase B.4-ii. The asynq handler dispatch for "gitlab"
// connection type lands in B.4-iii.
package connectionmanager

import (
	"net/url"
	"path"
	"strconv"
	"strings"
)

// CreateGitLabRepoRecordInput is the legacy named-argument
// shape (the GitLab code is structured a bit differently from
// GitHub — the legacy inlines the per-project loop rather than
// extracting a helper — but we expose the same shape for
// parallelism with CreateGitHubRepoRecord).
type CreateGitLabRepoRecordInput struct {
	Project  GitLabProject
	HostURL  string
	Branches []string
	Tags     []string
	// OrgID falls back to SingleTenantOrgID when zero (same
	// pattern as the GitHub helper).
	OrgID int32
}

// CreateGitLabRepoRecord is the per-project transformation.
// Direct port of the .map() callback at repoCompileUtils.ts:
// 176-242. Pure function — same inputs produce byte-equal
// outputs.
func CreateGitLabRepoRecord(in CreateGitLabRepoRecordInput) RepoData {
	orgID := in.OrgID
	if orgID == 0 {
		orgID = SingleTenantOrgID
	}

	// projectUrl = `${hostUrl}/${project.path_with_namespace}`
	projectURL := strings.TrimRight(in.HostURL, "/") + "/" + in.Project.PathWithNamespace

	// repoNameRoot = stripHTTPScheme(canonicalURL(hostUrl))
	repoNameRoot := stripHTTPScheme(canonicalURLString(in.HostURL))
	repoDisplayName := in.Project.PathWithNamespace
	repoName := path.Join(repoNameRoot, repoDisplayName)

	// cloneUrl: legacy line 178-179. Take http_url_to_repo,
	// overlay the protocol from hostUrl. Reproduce verbatim —
	// the protocol-swap is the load-bearing transform here
	// (GitLab's API returns http:// even when the host is
	// https://, so the codeintel-emitted clone URL has to
	// match the host's scheme).
	cloneURL := overlayProtocolFromHost(in.Project.HTTPURLToRepo, in.HostURL)

	isFork := in.Project.ForkedFromProject != nil
	// isPublic includes "internal" — legacy lines 185-187. The
	// permission-filtering rationale is in the legacy comment;
	// we preserve the semantics to keep the port byte-equal.
	isPublic := in.Project.Visibility == "public" || in.Project.Visibility == "internal"

	// Avatar URL: legacy lines 191-193. project.avatar_url is
	// not directly accessible with PATs; the legacy emits a
	// projects-API endpoint pointer when avatar_url is set.
	// Empty string when no avatar.
	avatarURL := ""
	if in.Project.AvatarURL != nil && *in.Project.AvatarURL != "" {
		avatarURL = strings.TrimRight(in.HostURL, "/") +
			"/api/v4/projects/" + strconv.FormatInt(in.Project.ID, 10) + "/avatar"
	}

	gitConfig := map[string]string{
		"zoekt.web-url-type":  "gitlab",
		"zoekt.web-url":       projectURL,
		"zoekt.name":          repoName,
		"zoekt.gitlab-stars":  strconv.Itoa(ptrIntOrZero(in.Project.StargazersCount)),
		"zoekt.gitlab-forks":  strconv.Itoa(ptrIntOrZero(in.Project.ForksCount)),
		"zoekt.archived":      marshalBool(in.Project.Archived),
		"zoekt.fork":          marshalBoolValue(isFork),
		"zoekt.public":        marshalBoolValue(isPublic),
		"zoekt.display-name":  repoDisplayName,
	}

	// Topics fallback (legacy line 235: `project.topics ?? []`)
	topics := in.Project.Topics
	if topics == nil {
		topics = []string{}
	}

	metadata := RepoMetadata{
		GitConfig: gitConfig,
		Branches:  in.Branches,
		Tags:      in.Tags,
		CodeHostMetadata: &CodeHostMetadata{
			GitLab: &GitLabMetadata{Topics: topics},
		},
	}

	isArchived := in.Project.Archived != nil && *in.Project.Archived

	return RepoData{
		ExternalID:           strconv.FormatInt(in.Project.ID, 10),
		ExternalCodeHostType: "gitlab",
		ExternalCodeHostURL:  in.HostURL,
		CloneURL:             cloneURL,
		WebURL:               projectURL,
		Name:                 repoName,
		DisplayName:          repoDisplayName,
		ImageURL:             avatarURL,
		DefaultBranch:        in.Project.DefaultBranch,
		IsFork:               isFork,
		IsArchived:           isArchived,
		IsPublic:             isPublic,
		OrgID:                orgID,
		Metadata:             metadata,
	}
}

// CompileGitLabConfig is the per-project loop equivalent of
// CompileGitHubConfig. Takes a pre-fetched GitLabProject slice;
// the live fetch lands in B.4-ii.
//
// HostURL defaults to https://gitlab.com (legacy line 171) and
// has trailing slashes stripped.
func CompileGitLabConfig(projects []GitLabProject, in GitHubCompileInput, connectionID int32) []RepoData {
	hostURL := stripTrailingSlashes(in.HostURL)
	if hostURL == "" {
		hostURL = "https://gitlab.com"
	}

	out := make([]RepoData, 0, len(projects))
	for i := range projects {
		rec := CreateGitLabRepoRecord(CreateGitLabRepoRecordInput{
			Project:  projects[i],
			HostURL:  hostURL,
			Branches: in.Branches,
			Tags:     in.Tags,
		})
		rec.ConnectionIDs = []int32{connectionID}
		out = append(out, rec)
	}
	return out
}

// overlayProtocolFromHost takes a clone URL (legacy
// project.http_url_to_repo) and replaces its scheme with the
// scheme of hostURL. Mirrors the legacy:
//
//	const cloneUrl = new URL(project.http_url_to_repo);
//	cloneUrl.protocol = new URL(hostUrl).protocol;
//
// On a malformed input the function returns the raw clone URL
// unchanged (mirrors JS's tolerant URL constructor at this
// boundary; an upstream malformed value flows through rather
// than failing the whole compile).
func overlayProtocolFromHost(cloneRaw, hostRaw string) string {
	clone, err := url.Parse(cloneRaw)
	if err != nil || clone.Scheme == "" {
		return cloneRaw
	}
	host, err := url.Parse(hostRaw)
	if err != nil || host.Scheme == "" {
		return clone.String()
	}
	clone.Scheme = host.Scheme
	return clone.String()
}
