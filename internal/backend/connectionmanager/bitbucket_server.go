// Bitbucket Server compile + fetcher. Direct port of the SERVER
// branch of compileBitbucketConfig (repoCompileUtils.ts) +
// isBitbucketServerPublicAccessEnabled (bitbucket.ts:807-828)
// + serverGetReposForProjects (bitbucket.ts:490).
//
// Server semantics vs cloud (legacy preserves these verbatim):
//   - ExternalID is the numeric id (toString'd), not a uuid.
//   - displayName = "<projectKey>/<repoSlug>".
//   - IsArchived comes from the `archived` field (server has it;
//     cloud doesn't).
//   - IsFork = origin !== undefined (server has an origin
//     pointer for forks).
//   - IsPublic is gated by the cluster-level
//     `feature.public.access` flag - we probe a public repo
//     anonymously to verify the flag is on. If the probe fails
//     all "public" repos are treated as private.
//   - CloneURL = links.clone[name=='http'].href.
//   - WebURL = links.self[0].href with trailing /browse stripped.
//   - codeHostMetadata.bitbucketServer = {projectKey, repoSlug}.
//
// No SDK is used for server: the ktrysmt SDK is cloud-only, and
// the Server REST is simple enough to call directly.
package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

// BitbucketServerRepo captures the subset of the Server REST
// repo shape the legacy compile path reads.
type BitbucketServerRepo struct {
	ID            int64                  `json:"id"`
	Name          string                 `json:"name"`
	Slug          string                 `json:"slug"`
	Public        bool                   `json:"public"`
	Archived      bool                   `json:"archived"`
	DefaultBranch *string                `json:"defaultBranch,omitempty"`
	Origin        any                    `json:"origin,omitempty"` // presence -> fork
	Project       *BitbucketServerProj   `json:"project,omitempty"`
	Links         *BitbucketServerLinks  `json:"links,omitempty"`
}

type BitbucketServerProj struct {
	Key string `json:"key"`
}

type BitbucketServerLinks struct {
	Clone []BitbucketServerLink `json:"clone,omitempty"`
	Self  []BitbucketServerLink `json:"self,omitempty"`
}

type BitbucketServerLink struct {
	Href string `json:"href"`
	Name string `json:"name,omitempty"`
}

// ErrBitbucketServerMissingLinks is returned when a server repo
// has no links block. Same role as the cloud equivalent.
var ErrBitbucketServerMissingLinks = errors.New("connectionmanager: bitbucket server repo missing links")

// ErrBitbucketServerMissingCloneHTTP is returned when the
// server repo has no clone link with name == "http". Legacy
// throws on this branch.
var ErrBitbucketServerMissingCloneHTTP = errors.New("connectionmanager: bitbucket server repo missing http clone link")

// ErrBitbucketServerMissingProject is returned when the server
// repo has no project key. displayName requires it.
var ErrBitbucketServerMissingProject = errors.New("connectionmanager: bitbucket server repo missing project key")

// CreateBitbucketServerRepoRecordInput is the per-server-repo
// transform input.
type CreateBitbucketServerRepoRecordInput struct {
	Repo                       BitbucketServerRepo
	HostURL                    string
	Branches                   []string
	Tags                       []string
	OrgID                      int32
	ServerPublicAccessEnabled  bool
}

// CreateBitbucketServerRepoRecord transforms one server repo
// into a RepoData.
func CreateBitbucketServerRepoRecord(in CreateBitbucketServerRepoRecordInput) (RepoData, error) {
	orgID := in.OrgID
	if orgID == 0 {
		orgID = SingleTenantOrgID
	}
	if in.Repo.Project == nil || in.Repo.Project.Key == "" {
		return RepoData{}, ErrBitbucketServerMissingProject
	}
	if in.Repo.Links == nil {
		return RepoData{}, ErrBitbucketServerMissingLinks
	}

	projectKey := in.Repo.Project.Key
	repoSlug := in.Repo.Slug
	displayName := projectKey + "/" + repoSlug

	repoNameRoot := stripHTTPScheme(canonicalURLString(in.HostURL))
	repoName := path.Join(repoNameRoot, displayName)

	// CloneURL: find the http clone link.
	cloneURL := ""
	for _, l := range in.Repo.Links.Clone {
		if l.Name == "http" {
			cloneURL = l.Href
			break
		}
	}
	if cloneURL == "" {
		return RepoData{}, ErrBitbucketServerMissingCloneHTTP
	}

	// WebURL: self[0].href, with trailing /browse stripped
	// (legacy line 478).
	if len(in.Repo.Links.Self) == 0 || in.Repo.Links.Self[0].Href == "" {
		return RepoData{}, ErrBitbucketServerMissingLinks
	}
	webURL := strings.TrimSuffix(in.Repo.Links.Self[0].Href, "/browse")
	webURL = strings.TrimSuffix(webURL, "/browse/")

	isFork := in.Repo.Origin != nil
	// IsPublic is gated by both the per-repo public flag AND
	// the cluster-level feature.public.access setting (legacy
	// line 502-503).
	isPublic := in.ServerPublicAccessEnabled && in.Repo.Public

	gitConfig := map[string]string{
		"zoekt.web-url-type": "bitbucket-server",
		"zoekt.web-url":      webURL,
		"zoekt.name":         repoName,
		"zoekt.archived":     marshalBoolValue(in.Repo.Archived),
		"zoekt.fork":         marshalBoolValue(isFork),
		"zoekt.public":       marshalBoolValue(isPublic),
		"zoekt.display-name": displayName,
	}

	return RepoData{
		ExternalID:           strconv.FormatInt(in.Repo.ID, 10),
		ExternalCodeHostType: "bitbucket-server",
		ExternalCodeHostURL:  in.HostURL,
		CloneURL:             cloneURL,
		WebURL:               webURL,
		Name:                 repoName,
		DisplayName:          displayName,
		DefaultBranch:        in.Repo.DefaultBranch,
		IsFork:               isFork,
		IsArchived:           in.Repo.Archived,
		IsPublic:             isPublic,
		OrgID:                orgID,
		Metadata: RepoMetadata{
			GitConfig: gitConfig,
			Branches:  in.Branches,
			Tags:      in.Tags,
			CodeHostMetadata: &CodeHostMetadata{
				BitbucketServer: &BitbucketServerMetadata{
					ProjectKey: projectKey,
					RepoSlug:   repoSlug,
				},
			},
		},
	}, nil
}

// CompileBitbucketServerConfig is the per-repo loop wrapper.
func CompileBitbucketServerConfig(repos []BitbucketServerRepo, in GitHubCompileInput, connectionID int32, serverPublicAccessEnabled bool) ([]RepoData, []string) {
	hostURL := stripTrailingSlashes(in.HostURL)

	out := make([]RepoData, 0, len(repos))
	var warnings []string
	for i := range repos {
		rec, err := CreateBitbucketServerRepoRecord(CreateBitbucketServerRepoRecordInput{
			Repo:                      repos[i],
			HostURL:                   hostURL,
			Branches:                  in.Branches,
			Tags:                      in.Tags,
			ServerPublicAccessEnabled: serverPublicAccessEnabled,
		})
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("bitbucket-server repo %s/%s skipped: %v",
				bbsProjectKeyOrUnknown(repos[i]), repos[i].Slug, err))
			continue
		}
		rec.ConnectionIDs = []int32{connectionID}
		out = append(out, rec)
	}
	return out, warnings
}

func bbsProjectKeyOrUnknown(r BitbucketServerRepo) string {
	if r.Project != nil && r.Project.Key != "" {
		return r.Project.Key
	}
	return "unknown"
}

// ===========================================================
// Live HTTP fetcher (no SDK)
// ===========================================================

// BitbucketServerFetcher returns the server-API result for a
// connection config. Injectable so tests + the codeintel-backend
// init() share the same hook shape as the cloud fetcher.
type BitbucketServerFetcher func(ctx context.Context, cfg BitbucketConnectionConfig) (bitbucketServerFetchResult, error)

type bitbucketServerFetchResult struct {
	Repos                     []BitbucketServerRepo
	ServerPublicAccessEnabled bool
	Warnings                  []string
}

var (
	bitbucketServerFetcherMu sync.RWMutex
	bitbucketServerFetcher   BitbucketServerFetcher
)

// SetBitbucketServerFetcher registers the production fetcher.
func SetBitbucketServerFetcher(f BitbucketServerFetcher) {
	bitbucketServerFetcherMu.Lock()
	bitbucketServerFetcher = f
	bitbucketServerFetcherMu.Unlock()
}

// bitbucketServerPage is the Bitbucket Server pagination wire
// shape. `start`/`limit` query params + `nextPageStart`/`isLastPage`
// in the response.
type bitbucketServerPage struct {
	Values        []json.RawMessage `json:"values"`
	IsLastPage    bool              `json:"isLastPage"`
	NextPageStart *int              `json:"nextPageStart,omitempty"`
}

const bitbucketServerPageSize = 1000 // legacy default

// FetchReposForBitbucketServerProject lists every repo in
// `projectKey` via the bitbucket Server REST API. Hand-rolled
// HTTP — no SDK because the ktrysmt library is cloud-only and
// the Server REST is straightforward.
func FetchReposForBitbucketServerProject(ctx context.Context, httpClient *http.Client, baseURL, token, projectKey string) ([]BitbucketServerRepo, *FetchWarning, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if projectKey == "" {
		return nil, nil, errors.New("connectionmanager: bitbucket server project key is empty")
	}
	if baseURL == "" {
		return nil, nil, errors.New("connectionmanager: bitbucket server base URL is empty")
	}

	var (
		allRepos []BitbucketServerRepo
		start    = 0
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrFetchAborted, err)
		}
		u := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos?limit=%d&start=%d",
			stripTrailingSlashes(baseURL), projectKey, bitbucketServerPageSize, start)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("get %s: %w", u, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, &FetchWarning{Message: fmt.Sprintf("Project %s not found or no access", projectKey)}, nil
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, nil, fmt.Errorf("bitbucket server %s: status %d", u, resp.StatusCode)
		}
		var page bitbucketServerPage
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, nil, fmt.Errorf("decode page: %w", err)
		}
		for _, raw := range page.Values {
			var r BitbucketServerRepo
			if err := json.Unmarshal(raw, &r); err == nil {
				allRepos = append(allRepos, r)
			}
		}
		if page.IsLastPage || page.NextPageStart == nil {
			break
		}
		start = *page.NextPageStart
	}
	return allRepos, nil, nil
}

// IsBitbucketServerPublicAccessEnabled probes a known-public
// repo anonymously to verify the cluster-level
// `feature.public.access` flag is enabled. Direct port of
// bitbucket.ts:807-828. Returns false on any non-2xx or
// transport error.
func IsBitbucketServerPublicAccessEnabled(ctx context.Context, httpClient *http.Client, baseURL string, probe BitbucketServerRepo) bool {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if probe.Project == nil || probe.Project.Key == "" || probe.Slug == "" {
		return false
	}
	u := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s",
		stripTrailingSlashes(baseURL), probe.Project.Key, probe.Slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	// NO Authorization header - the whole point is to test
	// anonymous access.
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// fetchBitbucketServerReposViaHTTP is the live server fetcher.
// Registered via init() into the orchestrator's
// BitbucketServerFetcher slot.
func fetchBitbucketServerReposViaHTTP(ctx context.Context, cfg BitbucketConnectionConfig) (bitbucketServerFetchResult, error) {
	if cfg.URL == "" {
		return bitbucketServerFetchResult{},
			errors.New("connectionmanager: bitbucket server config.url is required")
	}
	httpClient := http.DefaultClient
	var (
		all      []BitbucketServerRepo
		warnings []string
	)
	for _, projectKey := range cfg.Projects {
		repos, warn, err := FetchReposForBitbucketServerProject(ctx, httpClient, cfg.URL, cfg.Token, projectKey)
		if err != nil {
			return bitbucketServerFetchResult{}, fmt.Errorf("FetchReposForBitbucketServerProject %q: %w", projectKey, err)
		}
		if warn != nil {
			warnings = append(warnings, warn.Message)
			continue
		}
		all = append(all, repos...)
	}

	// Probe the public-access flag using the first repo that's
	// flagged as public. If none are public, leave flag = true
	// (no-op; nothing to gate on).
	publicAccess := true
	for i := range all {
		if all[i].Public {
			publicAccess = IsBitbucketServerPublicAccessEnabled(ctx, httpClient, cfg.URL, all[i])
			break
		}
	}

	return bitbucketServerFetchResult{
		Repos:                     all,
		ServerPublicAccessEnabled: publicAccess,
		Warnings:                  warnings,
	}, nil
}

func init() {
	SetBitbucketServerFetcher(fetchBitbucketServerReposViaHTTP)
}
