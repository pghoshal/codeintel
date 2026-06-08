// Gerrit compile + fetcher + dispatch in one file. Direct port
// of gerrit.ts + compileGerritConfig (repoCompileUtils.ts:320-403).
//
// Gerrit specifics preserved verbatim:
//   - Pure REST, no SDK.
//   - Response body is JSON prefixed with `)]}\n` (XSSI guard);
//     parse only after stripping.
//   - Pagination via the `_more_projects` boolean on any project
//     value in the response object. Offset query param `?S=N`.
//   - displayName == project.name (single component, no namespace).
//   - defaultBranch is intentionally nil (legacy comment: gerrit
//     API doesn't surface it without a separate query; the
//     indexer sets it from the clone).
//   - isFork/isArchived always false, isPublic always true
//     (legacy hardcodes — gerrit's access model isn't exposed
//     via this listing API).
//   - cloneUrl = hostUrl + "/" + url.PathEscape(project.name).
//   - webUrl may be a `/plugins/gitiles/...` path; if so, prepend
//     hostUrl. Other paths pass through.
//   - zoekt.web-url-type is "gitiles" (NOT "gerrit") — the legacy
//     wire shape; preserved for byte-equal output.
package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
)

// GerritProjectState mirrors legacy line 13.
type GerritProjectState string

const (
	GerritProjectActive   GerritProjectState = "ACTIVE"
	GerritProjectReadOnly GerritProjectState = "READ_ONLY"
	GerritProjectHidden   GerritProjectState = "HIDDEN"
)

// GerritProject captures the legacy GerritProject struct (lines
// 21-26). Note the legacy distinguishes GerritProjectInfo (the
// raw API value) from GerritProject (post-name-flattening).
type GerritProject struct {
	Name     string             `json:"name"`
	ID       string             `json:"id"`
	State    GerritProjectState `json:"state,omitempty"`
	WebLinks []GerritWebLink    `json:"web_links,omitempty"`
}

type GerritWebLink struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// GerritConnectionConfig mirrors the legacy
// GerritConnectionConfig (@schemas/v3/gerrit.type) subset.
type GerritConnectionConfig struct {
	URL       string              `json:"url"` // REQUIRED for gerrit; no cloud default
	Projects  []string            `json:"projects,omitempty"`
	Exclude   *GerritExcludeRules `json:"exclude,omitempty"`
	Revisions *GitHubRevisions    `json:"revisions,omitempty"`
}

// GerritExcludeRules is gerrit's narrower exclude shape - no
// forks/archived booleans; only the projects glob list.
type GerritExcludeRules struct {
	Projects []string `json:"projects,omitempty"`
}

// CreateGerritRepoRecordInput is the per-project transform input.
type CreateGerritRepoRecordInput struct {
	Project  GerritProject
	HostURL  string
	Branches []string
	Tags     []string
	OrgID    int32
}

// CreateGerritRepoRecord transforms one gerrit project into a
// RepoData. Direct port of repoCompileUtils.ts:331-396 callback.
func CreateGerritRepoRecord(in CreateGerritRepoRecordInput) RepoData {
	orgID := in.OrgID
	if orgID == 0 {
		orgID = SingleTenantOrgID
	}

	hostURL := stripTrailingSlashes(in.HostURL)
	repoNameRoot := stripHTTPScheme(canonicalURLString(hostURL))
	repoDisplayName := in.Project.Name
	repoName := path.Join(repoNameRoot, repoDisplayName)

	// CloneURL = hostUrl + "/" + encodeURIComponent(project.name)
	// Path-escape because gerrit allows slashes inside project
	// names (path-style); the encoded form is what the legacy
	// emits via `new URL(path.join(hostUrl, encodeURIComponent(name)))`.
	cloneURL := hostURL + "/" + url.PathEscape(in.Project.Name)

	// WebURL derivation: take the first web_link if present.
	// If its URL starts with "/plugins/gitiles/" prepend the
	// host. Otherwise pass through.
	var webURL string
	if len(in.Project.WebLinks) > 0 {
		raw := in.Project.WebLinks[0].URL
		if strings.HasPrefix(raw, "/plugins/gitiles/") {
			webURL = canonicalURLString(hostURL + raw)
		} else {
			webURL = raw
		}
	}

	gitConfig := map[string]string{
		"zoekt.web-url-type": "gitiles", // intentional: zoekt's renderer type, not "gerrit"
		"zoekt.web-url":      webURL,    // legacy emits "" when null
		"zoekt.name":         repoName,
		"zoekt.archived":     marshalBoolValue(false),
		"zoekt.fork":         marshalBoolValue(false),
		"zoekt.public":       marshalBoolValue(true),
		"zoekt.display-name": repoDisplayName,
	}

	return RepoData{
		ExternalID:           in.Project.ID,
		ExternalCodeHostType: "gerrit",
		ExternalCodeHostURL:  hostURL,
		CloneURL:             cloneURL,
		WebURL:               webURL,
		Name:                 repoName,
		DisplayName:          repoDisplayName,
		// DefaultBranch nil by design (legacy line 366-368).
		IsFork:     false,
		IsArchived: false,
		IsPublic:   true,
		OrgID:      orgID,
		Metadata: RepoMetadata{
			GitConfig: gitConfig,
			Branches:  in.Branches,
			Tags:      in.Tags,
			// No codeHostMetadata.gerrit block in legacy.
		},
	}
}

// CompileGerritConfig is the per-project loop wrapper.
func CompileGerritConfig(projects []GerritProject, in GitHubCompileInput, connectionID int32) []RepoData {
	hostURL := stripTrailingSlashes(in.HostURL)
	out := make([]RepoData, 0, len(projects))
	for i := range projects {
		rec := CreateGerritRepoRecord(CreateGerritRepoRecordInput{
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

// =====================================================================
// HTTP fetcher
// =====================================================================

// xssiPrefix is gerrit's response prefix per
// https://gerrit-review.googlesource.com/Documentation/rest-api.html#output
const xssiPrefix = ")]}'\n"

// GerritFetcher is the injectable fetcher hook.
type GerritFetcher func(ctx context.Context, cfg GerritConnectionConfig) (gerritFetchResult, error)

type gerritFetchResult struct {
	Projects []GerritProject
	Warnings []string
}

// ErrGerritFetcherNotConfigured is returned when no fetcher is
// registered.
var ErrGerritFetcherNotConfigured = errors.New("connectionmanager: gerrit fetcher not configured")

var (
	gerritFetcherMu sync.RWMutex
	gerritFetcherV  GerritFetcher
)

// SetGerritFetcher registers the production fetcher.
func SetGerritFetcher(f GerritFetcher) {
	gerritFetcherMu.Lock()
	gerritFetcherV = f
	gerritFetcherMu.Unlock()
}

// CompileGerritFromConfig is the orchestrator entrypoint.
func CompileGerritFromConfig(ctx context.Context, cfg GerritConnectionConfig, connectionID int32) ([]RepoData, []string, error) {
	if cfg.URL == "" {
		return nil, nil, errors.New("connectionmanager: gerrit config.url is required")
	}
	gerritFetcherMu.RLock()
	f := gerritFetcherV
	gerritFetcherMu.RUnlock()
	if f == nil {
		return nil, nil, ErrGerritFetcherNotConfigured
	}
	fetched, err := f(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	in := GitHubCompileInput{HostURL: stripTrailingSlashes(cfg.URL)}
	if cfg.Revisions != nil {
		in.Branches = cfg.Revisions.Branches
		in.Tags = cfg.Revisions.Tags
	}
	return CompileGerritConfig(fetched.Projects, in, connectionID), fetched.Warnings, nil
}

// FetchAllGerritProjects pages through /projects/?S=N stripping
// the XSSI guard prefix from each response body before parsing.
// Pagination terminates when no project in the response has
// `_more_projects=true`. The legacy uses (S = total-so-far);
// preserved for parity.
//
// Returns one warning per HTTP non-2xx (after the first the
// loop stops to avoid runaway).
func FetchAllGerritProjects(ctx context.Context, httpClient *http.Client, baseURL string) ([]GerritProject, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if baseURL == "" {
		return nil, errors.New("connectionmanager: gerrit baseURL is empty")
	}
	base := stripTrailingSlashes(baseURL)
	endpoint := base + "/projects/"

	var (
		all   []GerritProject
		start = 0
	)
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrFetchAborted, err)
		}
		u := fmt.Sprintf("%s?S=%d", endpoint, start)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		req.Header.Set("Accept", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", u, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("gerrit %s: status %d", u, resp.StatusCode)
		}
		// Strip XSSI prefix.
		text := strings.TrimPrefix(string(body), xssiPrefix)
		// Some deployments omit the trailing newline in the
		// prefix; defensively strip the shorter form too.
		text = strings.TrimPrefix(text, ")]}'")

		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			return nil, fmt.Errorf("gerrit decode: %w", err)
		}

		hasMore := false
		for name, v := range raw {
			// Decode the per-project shape with the
			// _more_projects flag as a discriminator.
			var info struct {
				ID           string             `json:"id"`
				State        GerritProjectState `json:"state,omitempty"`
				WebLinks     []GerritWebLink    `json:"web_links,omitempty"`
				MoreProjects bool               `json:"_more_projects,omitempty"`
			}
			if err := json.Unmarshal(v, &info); err != nil {
				continue
			}
			if info.MoreProjects {
				hasMore = true
			}
			all = append(all, GerritProject{
				Name:     name,
				ID:       info.ID,
				State:    info.State,
				WebLinks: info.WebLinks,
			})
		}
		start += len(raw)
		if !hasMore || len(raw) == 0 {
			break
		}
	}
	return all, nil
}

// fetchGerritProjectsViaHTTP is the production fetcher. Applies
// the include/exclude project globs after the fetch (legacy
// lines 44-58).
func fetchGerritProjectsViaHTTP(ctx context.Context, cfg GerritConnectionConfig) (gerritFetchResult, error) {
	projects, err := FetchAllGerritProjects(ctx, http.DefaultClient, cfg.URL)
	if err != nil {
		return gerritFetchResult{}, err
	}

	// Include filter: config.projects retains only matching
	// project names.
	if len(cfg.Projects) > 0 {
		filtered := projects[:0]
		for _, p := range projects {
			if matchAny(p.Name, cfg.Projects, false) {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}

	// Exclude filter: drop any whose name matches an exclude
	// glob.
	if cfg.Exclude != nil && len(cfg.Exclude.Projects) > 0 {
		filtered := projects[:0]
		for _, p := range projects {
			if !matchAny(p.Name, cfg.Exclude.Projects, false) {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}

	return gerritFetchResult{Projects: projects}, nil
}

func init() {
	SetGerritFetcher(fetchGerritProjectsViaHTTP)
}
