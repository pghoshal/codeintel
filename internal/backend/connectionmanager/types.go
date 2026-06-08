// Package connectionmanager hosts the codeintel-backend
// connection-sync worker: the function that turns a connection
// config + a list of code-host repository objects into the
// normalized Repo rows the worker upserts.
//
// This is the Go port of packages/backend/src/connectionManager.ts
// + the parts of packages/backend/src/repoCompileUtils.ts that
// produce the per-host Prisma.RepoCreateInput records.
//
// Surface this slice (Phase B.1b) lands:
//
//   - OctokitRepository: the input shape from the GitHub API
//     (subset the legacy code reads).
//   - GitHubCompileInput: the per-call config the compile path
//     needs (host URL + optional branch/tag globs).
//   - RepoMetadata: the JSONB shape the legacy worker writes
//     into the Repo.metadata column.
//   - RepoData: the legacy Prisma.RepoCreateInput-equivalent
//     output record.
//   - CreateGitHubRepoRecord: pure-function port of
//     createGitHubRepoRecord (repoCompileUtils.ts:91-161).
//   - CompileGitHubConfig: pure-function port of the per-repo
//     loop in compileGithubConfig (repoCompileUtils.ts:57-89).
//     Takes pre-fetched OctokitRepository[] instead of calling
//     getGitHubReposFromConfig — the live GitHub fetch lands in
//     Phase B.1c.
//
// Deferred to later slices:
//
//   - The GitHub API fetch (Phase B.1c).
//   - The asynq handler that calls CompileGitHubConfig + upserts
//     the resulting RepoData rows (Phase B.1d).
//   - GitLab / Gitea / Gerrit / Bitbucket / Azure DevOps /
//     generic-git compile paths (later slices, same pattern).
package connectionmanager

// SingleTenantOrgID mirrors the legacy
// packages/backend/src/constants.ts SINGLE_TENANT_ORG_ID
// constant. The createGitHubRepoRecord function defaults to
// this when the caller doesn't supply an explicit orgId.
const SingleTenantOrgID int32 = 1

// OctokitRepository captures the subset of the GitHub API repo
// shape the legacy code reads. Mirrors
// packages/backend/src/github.ts OctokitRepository.
//
// Pointer-typed optional fields preserve the JS shape: a missing
// API field surfaces as a nil pointer, which the compile path
// translates to a default value (e.g., a zero count surfaces as
// "0" in metadata.gitConfig). Required fields are value-typed.
type OctokitRepository struct {
	Name              string         `json:"name"`
	ID                int64          `json:"id"`
	FullName          string         `json:"full_name"`
	Fork              bool           `json:"fork"`
	Private           bool           `json:"private"`
	HTMLURL           string         `json:"html_url"`
	CloneURL          *string        `json:"clone_url,omitempty"`
	StargazersCount   *int           `json:"stargazers_count,omitempty"`
	WatchersCount     *int           `json:"watchers_count,omitempty"`
	SubscribersCount  *int           `json:"subscribers_count,omitempty"`
	DefaultBranch     *string        `json:"default_branch,omitempty"`
	ForksCount        *int           `json:"forks_count,omitempty"`
	Archived          *bool          `json:"archived,omitempty"`
	Topics            []string       `json:"topics,omitempty"`
	Size              *int           `json:"size,omitempty"`
	Owner             OctokitOwner   `json:"owner"`
}

// OctokitOwner mirrors the nested owner shape on
// OctokitRepository. avatar_url + login are the only fields the
// legacy compile path reads.
type OctokitOwner struct {
	AvatarURL string `json:"avatar_url"`
	Login     string `json:"login"`
}

// GitHubCompileInput is the per-call config the compile path
// needs from the broader connection-config type. The full
// connection-config schema (legacy @schemas/v3/github.type) has
// a large surface; the compile path only reads three fields.
type GitHubCompileInput struct {
	// HostURL is the trailing-slash-stripped GitHub host URL.
	// The legacy code defaults this to "https://github.com" when
	// the config omits a url field. The caller is responsible for
	// the default + the trim.
	HostURL string

	// Branches is the optional branch-glob list to capture into
	// metadata.branches. Nil means the field is omitted from the
	// emitted metadata JSON.
	Branches []string

	// Tags is the optional tag-glob list. Same nil semantics.
	Tags []string
}

// RepoMetadata is the JSONB-encoded shape the legacy worker
// writes into the Repo.metadata column. Mirrors
// packages/shared/src/types.ts repoMetadataSchema.
//
// Field tags use omitempty so missing fields collapse out of the
// emitted JSON — matching Prisma's behavior of skipping
// unspecified optional fields. The wire shape MUST byte-match
// the legacy output because consumers (Zoekt index config
// builder, repo-list endpoint) read these fields by key name.
type RepoMetadata struct {
	GitConfig        map[string]string  `json:"gitConfig,omitempty"`
	Branches         []string           `json:"branches,omitempty"`
	Tags             []string           `json:"tags,omitempty"`
	IndexedRevisions []string           `json:"indexedRevisions,omitempty"`
	ManualIndexDisabled *bool           `json:"manualIndexDisabled,omitempty"`
	CodeHostMetadata *CodeHostMetadata  `json:"codeHostMetadata,omitempty"`
}

// CodeHostMetadata is the per-host nested struct in the legacy
// repoMetadataSchema. Only the github branch is populated by
// the github compile path; other branches land when their host
// compile paths are ported.
type CodeHostMetadata struct {
	BitbucketCloud  *BitbucketCloudMetadata  `json:"bitbucketCloud,omitempty"`
	BitbucketServer *BitbucketServerMetadata `json:"bitbucketServer,omitempty"`
	GitLab          *GitLabMetadata          `json:"gitlab,omitempty"`
	GitHub          *GitHubMetadata          `json:"github,omitempty"`
}

type BitbucketCloudMetadata struct {
	Workspace string `json:"workspace"`
	RepoSlug  string `json:"repoSlug"`
}

type BitbucketServerMetadata struct {
	ProjectKey string `json:"projectKey"`
	RepoSlug   string `json:"repoSlug"`
}

type GitLabMetadata struct {
	Topics []string `json:"topics"`
}

type GitHubMetadata struct {
	Topics []string `json:"topics"`
}

// RepoData mirrors the legacy
// `Prisma.RepoCreateInput & { connections: ConnectionsCreate }`
// shape produced by compileGithubConfig. Field tags + nullability
// match legacy Prisma column types so an INSERT statement
// constructed from this struct emits the same column-value
// tuples.
//
// Optional pointer fields:
//   - DefaultBranch: legacy nullable.
//   - IsAutoCleanupDisabled: legacy optional with Boolean type.
type RepoData struct {
	// External identity (composite-unique with orgId).
	ExternalID           string `json:"external_id"`
	ExternalCodeHostType string `json:"external_codeHostType"`
	ExternalCodeHostURL  string `json:"external_codeHostUrl"`

	// Clone / display / web URLs.
	CloneURL    string `json:"cloneUrl"`
	WebURL      string `json:"webUrl"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ImageURL    string `json:"imageUrl"`

	// Branch + state flags.
	DefaultBranch         *string `json:"defaultBranch,omitempty"`
	IsFork                bool    `json:"isFork"`
	IsArchived            bool    `json:"isArchived"`
	IsPublic              bool    `json:"isPublic"`
	IsAutoCleanupDisabled *bool   `json:"isAutoCleanupDisabled,omitempty"`

	// Org binding.
	OrgID int32 `json:"orgId"`

	// Metadata JSONB blob.
	Metadata RepoMetadata `json:"metadata"`

	// Connection binding. The legacy code attaches connections
	// via a Prisma `connections: { create: { connectionId } }`
	// nested write. The Go port records the list of
	// connection ids the worker will INSERT into RepoToConnection
	// after the parent Repo row is upserted.
	ConnectionIDs []int32 `json:"connectionIds,omitempty"`
}
