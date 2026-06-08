package connectionmanager

// GitLabProject captures the subset of the GitLab API project
// shape (REST /api/v4/projects/:id) the legacy compile path
// reads. Mirrors packages/backend/src/gitlab.ts implicit shape
// + the @gitbeaker/rest ProjectSchema type the legacy code
// destructures.
//
// Pointer-typed optional fields preserve nullability from the
// upstream API; required fields are value-typed.
type GitLabProject struct {
	ID                 int64    `json:"id"`
	Name               string   `json:"name"`
	PathWithNamespace  string   `json:"path_with_namespace"`
	HTTPURLToRepo      string   `json:"http_url_to_repo"`
	DefaultBranch      *string  `json:"default_branch,omitempty"`
	Visibility         string   `json:"visibility"` // "public" | "internal" | "private"
	Archived           *bool    `json:"archived,omitempty"`
	Topics             []string `json:"topics,omitempty"`
	StargazersCount    *int     `json:"star_count,omitempty"`
	ForksCount         *int     `json:"forks_count,omitempty"`
	AvatarURL          *string  `json:"avatar_url,omitempty"`
	ForkedFromProject  any      `json:"forked_from_project,omitempty"` // presence-only, hence interface
}

// GitLabConnectionConfig is the codeintel Go mirror of the
// legacy GitlabConnectionConfig (@schemas/v3/gitlab.type). The
// orchestrator reads four fields; other fields are omitted
// until their consumers land.
type GitLabConnectionConfig struct {
	URL       string             `json:"url,omitempty"`       // default https://gitlab.com
	Token     string             `json:"token,omitempty"`     // PAT or OAuth bearer
	Groups    []string           `json:"groups,omitempty"`    // GitLab equivalent of GitHub orgs
	Projects  []string           `json:"projects,omitempty"`  // "group/project" slugs
	Users     []string           `json:"users,omitempty"`     // username enumeration
	Topics    []string           `json:"topics,omitempty"`    // include filter
	Exclude   *GitLabExcludeRules `json:"exclude,omitempty"`
	Revisions *GitHubRevisions   `json:"revisions,omitempty"` // reuses the shared branch/tag struct
}

// GitLabExcludeRules mirrors the legacy GitLab exclude shape.
// Mostly parallel to the GitHub exclude rules but with GitLab-
// specific field names (forks via fork-detection, archived,
// project-name globs, topic globs, size bounds).
type GitLabExcludeRules struct {
	Forks    *bool       `json:"forks,omitempty"`
	Archived *bool       `json:"archived,omitempty"`
	Projects []string    `json:"projects,omitempty"`
	Topics   []string    `json:"topics,omitempty"`
	Size     *GitHubSize `json:"size,omitempty"` // same shape as GitHub
}
