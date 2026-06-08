// Gitea pure-function compile path. Direct port of
// compileGiteaConfig (repoCompileUtils.ts:250-318).
//
// Gitea-specific semantics vs GitHub:
//   - The "internal" boolean exists (org-scoped visibility tier
//     between public and private). isPublic = !internal &&
//     !private.
//   - The clone URL gets HOST-swapped (not protocol-swapped
//     like gitlab) — legacy line 266 sets cloneUrl.host =
//     configUrl.host, preserving the scheme.
//   - The metadata.gitConfig carries fewer zoekt.* keys (7 vs
//     github's 11) because the gitea listing API doesn't
//     expose star/fork/watcher counters in the repo response.
//   - The legacy emits no codeHostMetadata.gitea block (unlike
//     github + gitlab which both have one).
//
// Live API fetch lands in a later slice (will use
// code.gitea.io/sdk/gitea). This slice covers the pure-function
// transform + handler dispatch so an org's gitea Connection
// type can be wired end-to-end as soon as the fetcher lands.
package connectionmanager

import (
	"net/url"
	"path"
	"strconv"
)

// GiteaRepo captures the subset of the Gitea API repo shape
// the legacy compile path reads. Mirrors the gitea-js
// Repository type by field.
type GiteaRepo struct {
	ID            int64       `json:"id"`
	Name          string      `json:"name"`
	FullName      string      `json:"full_name"`
	Fork          bool        `json:"fork"`
	Private       bool        `json:"private"`
	Internal      bool        `json:"internal"` // gitea-specific tier
	HTMLURL       string      `json:"html_url"`
	CloneURL      *string     `json:"clone_url,omitempty"`
	DefaultBranch *string     `json:"default_branch,omitempty"`
	Archived      *bool       `json:"archived,omitempty"`
	Owner         *GiteaOwner `json:"owner,omitempty"`
}

// GiteaOwner mirrors the nested owner shape — only avatar_url
// is read by the compile path.
type GiteaOwner struct {
	AvatarURL string `json:"avatar_url"`
}

// GiteaConnectionConfig mirrors the legacy GiteaConnectionConfig
// (@schemas/v3/gitea.type). Only fields the compile + orchestrator
// paths use.
type GiteaConnectionConfig struct {
	URL          string             `json:"url,omitempty"` // default https://gitea.com
	Token        string             `json:"token,omitempty"`
	Orgs         []string           `json:"orgs,omitempty"`
	Repos        []string           `json:"repos,omitempty"`
	Users        []string           `json:"users,omitempty"`
	Revisions    *GitHubRevisions   `json:"revisions,omitempty"`
}

// CreateGiteaRepoRecordInput is the per-repo transform input.
type CreateGiteaRepoRecordInput struct {
	Repo     GiteaRepo
	HostURL  string
	Branches []string
	Tags     []string
	OrgID    int32
}

// CreateGiteaRepoRecord is the per-repo transformation. Direct
// port of repoCompileUtils.ts:263-312 .map() callback.
func CreateGiteaRepoRecord(in CreateGiteaRepoRecordInput) RepoData {
	orgID := in.OrgID
	if orgID == 0 {
		orgID = SingleTenantOrgID
	}

	repoDisplayName := in.Repo.FullName
	repoNameRoot := stripHTTPScheme(canonicalURLString(in.HostURL))
	repoName := path.Join(repoNameRoot, repoDisplayName)

	// clone URL: legacy line 264-266 swaps the HOST (not the
	// protocol). The host of the configured URL overrides the
	// host returned by the gitea API (which can drift behind a
	// proxy).
	cloneURL := ""
	if in.Repo.CloneURL != nil {
		cloneURL = overlayHostFromHost(*in.Repo.CloneURL, in.HostURL)
	}

	// isPublic = repo.internal === false && repo.private === false
	isPublic := !in.Repo.Internal && !in.Repo.Private
	isArchived := in.Repo.Archived != nil && *in.Repo.Archived

	gitConfig := map[string]string{
		"zoekt.web-url-type":  "gitea",
		"zoekt.web-url":       in.Repo.HTMLURL,
		"zoekt.name":          repoName,
		"zoekt.archived":      marshalBool(in.Repo.Archived),
		"zoekt.fork":          marshalBoolValue(in.Repo.Fork),
		"zoekt.public":        marshalBoolValue(isPublic),
		"zoekt.display-name":  repoDisplayName,
	}

	imageURL := ""
	if in.Repo.Owner != nil {
		imageURL = in.Repo.Owner.AvatarURL
	}

	return RepoData{
		ExternalID:           strconv.FormatInt(in.Repo.ID, 10),
		ExternalCodeHostType: "gitea",
		ExternalCodeHostURL:  in.HostURL,
		CloneURL:             cloneURL,
		WebURL:               in.Repo.HTMLURL,
		Name:                 repoName,
		DisplayName:          repoDisplayName,
		ImageURL:             imageURL,
		DefaultBranch:        in.Repo.DefaultBranch,
		IsFork:               in.Repo.Fork,
		IsArchived:           isArchived,
		IsPublic:             isPublic,
		OrgID:                orgID,
		Metadata: RepoMetadata{
			GitConfig: gitConfig,
			Branches:  in.Branches,
			Tags:      in.Tags,
			// Note: legacy emits no codeHostMetadata.gitea block,
			// unlike github/gitlab. Preserve that omission for
			// parity.
		},
	}
}

// CompileGiteaConfig is the per-repo loop wrapper. HostURL
// defaults to https://gitea.com (legacy line 258).
func CompileGiteaConfig(repos []GiteaRepo, in GitHubCompileInput, connectionID int32) []RepoData {
	hostURL := stripTrailingSlashes(in.HostURL)
	if hostURL == "" {
		hostURL = "https://gitea.com"
	}
	out := make([]RepoData, 0, len(repos))
	for i := range repos {
		rec := CreateGiteaRepoRecord(CreateGiteaRepoRecordInput{
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

// overlayHostFromHost swaps the host of cloneRaw with the host
// from hostRaw, preserving the rest of the URL. Legacy lines
// 264-266:
//
//	const configUrl = new URL(hostUrl);
//	const cloneUrl = new URL(repo.clone_url!);
//	cloneUrl.host = configUrl.host;
//
// Empty / malformed inputs return the raw clone URL unchanged
// (consistent with the github + gitlab helpers' tolerant
// behaviour).
func overlayHostFromHost(cloneRaw, hostRaw string) string {
	clone, err := url.Parse(cloneRaw)
	if err != nil || clone.Scheme == "" {
		return cloneRaw
	}
	host, err := url.Parse(hostRaw)
	if err != nil || host.Host == "" {
		return clone.String()
	}
	clone.Host = host.Host
	return clone.String()
}
