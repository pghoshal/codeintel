// Bitbucket pure-function compile path. Direct port of
// compileBitbucketConfig (repoCompileUtils.ts:404-572) covering
// the CLOUD branch. Bitbucket Server branch lands in B.7-iii.
//
// Bitbucket-specific complexity vs github/gitlab/gitea:
//   - Two deployment types (cloud + server) under one
//     ConnectionType enum value ("bitbucket"). The config carries
//     a deploymentType field that the compile path dispatches on.
//   - Cloud repos use a `uuid` external_id (string), not a
//     numeric id.
//   - displayName = "<workspace>/<projectKey>/<repoSlug>" with
//     project key falling back to "unknown" if missing.
//   - Two new zoekt URL types: "bitbucket-cloud" + "bitbucket-server".
//   - codeHostMetadata.bitbucketCloud carries {workspace, repoSlug}.
//
// Live API fetch lands in B.7-ii via ktrysmt/go-bitbucket.
package connectionmanager

import (
	"errors"
	"path"
	"strings"
)

// BitbucketDeploymentType is the legacy `deploymentType` enum on
// the connection config. Cloud + server are the only values.
type BitbucketDeploymentType string

const (
	BitbucketDeploymentCloud  BitbucketDeploymentType = "cloud"
	BitbucketDeploymentServer BitbucketDeploymentType = "server"
)

// BitbucketCloudRepo captures the subset of the Bitbucket Cloud
// API repo shape (REST 2.0) the legacy compile path reads.
type BitbucketCloudRepo struct {
	UUID      string                  `json:"uuid"`
	Name      string                  `json:"name"`
	FullName  string                  `json:"full_name"`
	IsPrivate bool                    `json:"is_private"`
	Parent    any                     `json:"parent,omitempty"` // presence-only
	Project   *BitbucketCloudProject  `json:"project,omitempty"`
	Mainbranch *BitbucketCloudBranch  `json:"mainbranch,omitempty"`
	Links     *BitbucketCloudLinks    `json:"links,omitempty"`
}

type BitbucketCloudProject struct {
	Key string `json:"key"`
}

type BitbucketCloudBranch struct {
	Name string `json:"name"`
}

type BitbucketCloudLinks struct {
	HTML  *BitbucketCloudHref   `json:"html,omitempty"`
	Clone []BitbucketCloudClone `json:"clone,omitempty"`
}

type BitbucketCloudHref struct {
	Href string `json:"href"`
}

type BitbucketCloudClone struct {
	Href string `json:"href"`
	Name string `json:"name"`
}

// BitbucketConnectionConfig mirrors the legacy
// BitbucketConnectionConfig (@schemas/v3/bitbucket.type). Subset
// the compile + orchestrator paths use.
type BitbucketConnectionConfig struct {
	URL            string                  `json:"url,omitempty"`            // default https://bitbucket.org for cloud
	Token          string                  `json:"token,omitempty"`          // app password / token
	User           string                  `json:"user,omitempty"`           // basic-auth username for cloud
	DeploymentType BitbucketDeploymentType `json:"deploymentType,omitempty"` // "cloud" | "server"; default cloud
	Workspaces     []string                `json:"workspaces,omitempty"`     // cloud: workspace slugs
	Projects       []string                `json:"projects,omitempty"`       // server: project keys
	Repos          []string                `json:"repos,omitempty"`          // explicit repo slugs
	Revisions      *GitHubRevisions        `json:"revisions,omitempty"`
}

// CreateBitbucketCloudRepoRecordInput is the per-cloud-repo
// transform input.
type CreateBitbucketCloudRepoRecordInput struct {
	Repo     BitbucketCloudRepo
	HostURL  string
	Branches []string
	Tags     []string
	OrgID    int32
}

// ErrBitbucketCloudMissingLinks is returned when a cloud repo
// surfaces with no links block — that would prevent both
// cloneUrl + webUrl derivation. Legacy throws; the Go port
// surfaces a typed error so the upsert path can skip the row
// rather than fail the entire batch.
var ErrBitbucketCloudMissingLinks = errors.New("connectionmanager: bitbucket cloud repo missing links")

// ErrBitbucketCloudMissingFullName is returned when the API
// gives us a repo without full_name. Legacy throws; Go surfaces
// typed.
var ErrBitbucketCloudMissingFullName = errors.New("connectionmanager: bitbucket cloud repo missing full_name")

// CreateBitbucketCloudRepoRecord transforms one cloud repo into
// a RepoData. Returns an error rather than panicking when
// required fields are missing.
func CreateBitbucketCloudRepoRecord(in CreateBitbucketCloudRepoRecordInput) (RepoData, error) {
	orgID := in.OrgID
	if orgID == 0 {
		orgID = SingleTenantOrgID
	}

	if in.Repo.FullName == "" {
		return RepoData{}, ErrBitbucketCloudMissingFullName
	}
	if in.Repo.Links == nil {
		return RepoData{}, ErrBitbucketCloudMissingLinks
	}

	// displayName = "<workspace>/<projectKey-or-unknown>/<repoSlug>"
	// (legacy line 497-498).
	parts := strings.SplitN(in.Repo.FullName, "/", 2)
	if len(parts) != 2 {
		return RepoData{}, ErrBitbucketCloudMissingFullName
	}
	workspace, repoSlug := parts[0], parts[1]
	projectKey := "unknown"
	if in.Repo.Project != nil && in.Repo.Project.Key != "" {
		projectKey = in.Repo.Project.Key
	}
	displayName := workspace + "/" + projectKey + "/" + repoSlug

	repoNameRoot := stripHTTPScheme(canonicalURLString(in.HostURL))
	repoName := path.Join(repoNameRoot, displayName)

	// cloneUrl: legacy line 441-442 - htmlLink.href.
	if in.Repo.Links.HTML == nil || in.Repo.Links.HTML.Href == "" {
		return RepoData{}, ErrBitbucketCloudMissingLinks
	}
	cloneURL := in.Repo.Links.HTML.Href

	// webUrl: legacy line 470 - links.html.href for cloud.
	webURL := in.Repo.Links.HTML.Href

	isPublic := !in.Repo.IsPrivate
	isFork := in.Repo.Parent != nil

	var defaultBranch *string
	if in.Repo.Mainbranch != nil && in.Repo.Mainbranch.Name != "" {
		s := in.Repo.Mainbranch.Name
		defaultBranch = &s
	}

	gitConfig := map[string]string{
		"zoekt.web-url-type": "bitbucket-cloud",
		"zoekt.web-url":      webURL,
		"zoekt.name":         repoName,
		"zoekt.archived":     marshalBoolValue(false), // cloud has no archived field
		"zoekt.fork":         marshalBoolValue(isFork),
		"zoekt.public":       marshalBoolValue(isPublic),
		"zoekt.display-name": displayName,
	}

	return RepoData{
		ExternalID:           in.Repo.UUID,
		ExternalCodeHostType: "bitbucket-cloud",
		ExternalCodeHostURL:  in.HostURL,
		CloneURL:             cloneURL,
		WebURL:               webURL,
		Name:                 repoName,
		DisplayName:          displayName,
		DefaultBranch:        defaultBranch,
		IsFork:               isFork,
		IsArchived:           false,
		IsPublic:             isPublic,
		OrgID:                orgID,
		Metadata: RepoMetadata{
			GitConfig: gitConfig,
			Branches:  in.Branches,
			Tags:      in.Tags,
			CodeHostMetadata: &CodeHostMetadata{
				BitbucketCloud: &BitbucketCloudMetadata{
					Workspace: workspace,
					RepoSlug:  repoSlug,
				},
			},
		},
	}, nil
}

// CompileBitbucketCloudConfig is the per-repo loop wrapper for
// cloud. HostURL defaults to https://bitbucket.org. Repos that
// fail validation (missing full_name / missing links) are
// dropped + reported as warnings rather than failing the whole
// batch — matches the spirit of the legacy try/catch around
// per-repo errors that gather-then-warn.
func CompileBitbucketCloudConfig(repos []BitbucketCloudRepo, in GitHubCompileInput, connectionID int32) ([]RepoData, []string) {
	hostURL := stripTrailingSlashes(in.HostURL)
	if hostURL == "" {
		hostURL = "https://bitbucket.org"
	}

	out := make([]RepoData, 0, len(repos))
	var warnings []string
	for i := range repos {
		rec, err := CreateBitbucketCloudRepoRecord(CreateBitbucketCloudRepoRecordInput{
			Repo:     repos[i],
			HostURL:  hostURL,
			Branches: in.Branches,
			Tags:     in.Tags,
		})
		if err != nil {
			warnings = append(warnings, "bitbucket-cloud repo "+repos[i].FullName+" skipped: "+err.Error())
			continue
		}
		rec.ConnectionIDs = []int32{connectionID}
		out = append(out, rec)
	}
	return out, warnings
}
