// Azure DevOps compile + dispatch + injectable-fetcher hook.
// Direct port of compileAzureDevOpsConfig (repoCompileUtils.ts:
// 776-855) + the relevant subset of azuredevops.ts.
//
// AzDO specifics preserved verbatim:
//   - displayName = "<projectName>/<repoName>".
//   - isPublic derived from project.visibility == "public"
//     (the SDK's ProjectVisibility enum exposes Public/Private;
//     the codeintel port carries the lower-cased string for
//     wire-parity with the legacy enum value).
//   - cloneUrl == webUrl: the legacy assigns the same URL to
//     both fields (line 814-815). AzDO returns a Git endpoint
//     via remoteUrl that doubles as the web URL.
//   - webUrl falls back to "<hostUrl>/<projectName>/_git/<repoName>"
//     when the API doesn't surface webUrl explicitly.
//   - isFork from repo.isFork (truthy-coerced via `!!`); isArchived
//     hardcoded false (AzDO has no archived concept exposed in the
//     listing API).
//   - imageUrl always null/empty (legacy line 819 sets null
//     explicitly).
//   - zoekt.web-url-type = "azuredevops".
//
// Live SDK fetcher (microsoft/azure-devops-go-api) is injectable
// via SetAzureDevOpsFetcher. The handler dispatch routes here
// already; the SDK wiring lands as a follow-up if/when the
// codeintel deployment needs live AzDO ingestion.
package connectionmanager

import (
	"context"
	"errors"
	"path"
	"strings"
	"sync"
)

// AzureDevOpsRepo captures the subset of the AzDO Git REST repo
// shape the legacy compile path reads.
type AzureDevOpsRepo struct {
	ID            string                 `json:"id"`            // GUID string
	Name          string                 `json:"name"`
	RemoteURL     string                 `json:"remoteUrl"`
	WebURL        string                 `json:"webUrl,omitempty"`
	DefaultBranch *string                `json:"defaultBranch,omitempty"`
	IsFork        bool                   `json:"isFork,omitempty"`
	Project       *AzureDevOpsProject    `json:"project,omitempty"`
}

// AzureDevOpsProject is the embedded project shape.
type AzureDevOpsProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"` // "public" | "private"
}

// AzureDevOpsConnectionConfig mirrors the legacy
// AzureDevOpsConnectionConfig (@schemas/v3/azuredevops.type)
// subset.
type AzureDevOpsConnectionConfig struct {
	URL       string                  `json:"url,omitempty"`       // default https://dev.azure.com
	Token     string                  `json:"token,omitempty"`     // PAT
	Orgs      []string                `json:"orgs,omitempty"`      // AzDO orgs (the path segment after dev.azure.com/)
	Projects  []string                `json:"projects,omitempty"`  // explicit "org/project" strings
	Revisions *GitHubRevisions        `json:"revisions,omitempty"`
}

// CreateAzureDevOpsRepoRecordInput is the per-repo transform
// input.
type CreateAzureDevOpsRepoRecordInput struct {
	Repo     AzureDevOpsRepo
	HostURL  string
	Branches []string
	Tags     []string
	OrgID    int32
}

// ErrAzureDevOpsMissingProject is the typed sentinel for the
// legacy "No project found" throw branch.
var ErrAzureDevOpsMissingProject = errors.New("connectionmanager: azuredevops repo missing project")

// ErrAzureDevOpsMissingRemoteURL is the typed sentinel for the
// "No remoteUrl found" throw branch.
var ErrAzureDevOpsMissingRemoteURL = errors.New("connectionmanager: azuredevops repo missing remoteUrl")

// ErrAzureDevOpsMissingID is the typed sentinel for the "No id
// found" throw branch.
var ErrAzureDevOpsMissingID = errors.New("connectionmanager: azuredevops repo missing id")

// CreateAzureDevOpsRepoRecord transforms one AzDO repo into a
// RepoData. Errors surface as typed sentinels rather than panics.
func CreateAzureDevOpsRepoRecord(in CreateAzureDevOpsRepoRecordInput) (RepoData, error) {
	orgID := in.OrgID
	if orgID == 0 {
		orgID = SingleTenantOrgID
	}

	if in.Repo.Project == nil {
		return RepoData{}, ErrAzureDevOpsMissingProject
	}
	if in.Repo.RemoteURL == "" {
		return RepoData{}, ErrAzureDevOpsMissingRemoteURL
	}
	if in.Repo.ID == "" {
		return RepoData{}, ErrAzureDevOpsMissingID
	}

	hostURL := stripTrailingSlashes(in.HostURL)
	repoNameRoot := stripHTTPScheme(canonicalURLString(hostURL))
	displayName := in.Repo.Project.Name + "/" + in.Repo.Name
	repoName := path.Join(repoNameRoot, displayName)

	// webUrl: legacy line 806 - use API's webUrl when set,
	// otherwise construct one. The "/_git/" path segment is
	// the AzDO Git endpoint convention.
	webURL := in.Repo.WebURL
	if webURL == "" {
		webURL = hostURL + "/" + in.Repo.Project.Name + "/_git/" + in.Repo.Name
	}

	// Visibility: legacy line 796 maps the SDK enum's Public
	// value to isPublic=true. The wire value is lowercase
	// "public" / "private" - case-insensitive compare for
	// safety on alternate-cased deployments.
	isPublic := strings.EqualFold(in.Repo.Project.Visibility, "public")

	gitConfig := map[string]string{
		"zoekt.web-url-type": "azuredevops",
		"zoekt.web-url":      webURL,
		"zoekt.name":         repoName,
		"zoekt.archived":     marshalBoolValue(false),
		"zoekt.fork":         marshalBoolValue(in.Repo.IsFork),
		"zoekt.public":       marshalBoolValue(isPublic),
		"zoekt.display-name": displayName,
	}

	return RepoData{
		ExternalID:           in.Repo.ID,
		ExternalCodeHostType: "azuredevops",
		ExternalCodeHostURL:  hostURL,
		// Legacy line 814-815: cloneUrl == webUrl.
		CloneURL:      webURL,
		WebURL:        webURL,
		Name:          repoName,
		DisplayName:   displayName,
		DefaultBranch: in.Repo.DefaultBranch,
		// Legacy line 819: imageUrl is explicitly null. Go has
		// no nil string so we leave the zero value.
		ImageURL:   "",
		IsFork:     in.Repo.IsFork,
		IsArchived: false,
		IsPublic:   isPublic,
		OrgID:      orgID,
		Metadata: RepoMetadata{
			GitConfig: gitConfig,
			Branches:  in.Branches,
			Tags:      in.Tags,
			// No codeHostMetadata.azuredevops block in legacy.
		},
	}, nil
}

// CompileAzureDevOpsConfig is the per-repo loop wrapper. HostURL
// defaults to https://dev.azure.com (legacy line 784).
func CompileAzureDevOpsConfig(repos []AzureDevOpsRepo, in GitHubCompileInput, connectionID int32) ([]RepoData, []string) {
	hostURL := stripTrailingSlashes(in.HostURL)
	if hostURL == "" {
		hostURL = "https://dev.azure.com"
	}

	out := make([]RepoData, 0, len(repos))
	var warnings []string
	for i := range repos {
		rec, err := CreateAzureDevOpsRepoRecord(CreateAzureDevOpsRepoRecordInput{
			Repo:     repos[i],
			HostURL:  hostURL,
			Branches: in.Branches,
			Tags:     in.Tags,
		})
		if err != nil {
			warnings = append(warnings, "azuredevops repo "+repos[i].Name+" skipped: "+err.Error())
			continue
		}
		rec.ConnectionIDs = []int32{connectionID}
		out = append(out, rec)
	}
	return out, warnings
}

// =====================================================================
// Injectable fetcher
// =====================================================================

// AzureDevOpsFetcher is the production fetcher hook. The live
// SDK-backed implementation (microsoft/azure-devops-go-api) lands
// as a separate slice when needed; until then a caller registers
// a fetcher and the handler dispatch routes through it.
type AzureDevOpsFetcher func(ctx context.Context, cfg AzureDevOpsConnectionConfig) (azureDevOpsFetchResult, error)

type azureDevOpsFetchResult struct {
	Repos    []AzureDevOpsRepo
	Warnings []string
}

// ErrAzureDevOpsFetcherNotConfigured is returned when no fetcher
// has been registered.
var ErrAzureDevOpsFetcherNotConfigured = errors.New("connectionmanager: azuredevops fetcher not configured")

var (
	azureDevOpsFetcherMu sync.RWMutex
	azureDevOpsFetcher   AzureDevOpsFetcher
)

// SetAzureDevOpsFetcher registers the production fetcher.
func SetAzureDevOpsFetcher(f AzureDevOpsFetcher) {
	azureDevOpsFetcherMu.Lock()
	azureDevOpsFetcher = f
	azureDevOpsFetcherMu.Unlock()
}

// CompileAzureDevOpsFromConfig is the orchestrator entrypoint.
func CompileAzureDevOpsFromConfig(ctx context.Context, cfg AzureDevOpsConnectionConfig, connectionID int32) ([]RepoData, []string, error) {
	azureDevOpsFetcherMu.RLock()
	f := azureDevOpsFetcher
	azureDevOpsFetcherMu.RUnlock()
	if f == nil {
		return nil, nil, ErrAzureDevOpsFetcherNotConfigured
	}
	fetched, err := f(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	in := GitHubCompileInput{HostURL: ResolveAzureDevOpsHostURL(cfg)}
	if cfg.Revisions != nil {
		in.Branches = cfg.Revisions.Branches
		in.Tags = cfg.Revisions.Tags
	}
	records, compileWarnings := CompileAzureDevOpsConfig(fetched.Repos, in, connectionID)
	return records, append(fetched.Warnings, compileWarnings...), nil
}

// ResolveAzureDevOpsHostURL: https://dev.azure.com is the legacy
// default.
func ResolveAzureDevOpsHostURL(cfg AzureDevOpsConnectionConfig) string {
	if cfg.URL == "" {
		return "https://dev.azure.com"
	}
	return stripTrailingSlashes(cfg.URL)
}
