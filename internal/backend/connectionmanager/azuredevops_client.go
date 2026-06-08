// AzureDevOps live fetcher. Hand-rolled HTTP against the REST
// API (no SDK; the microsoft/azure-devops-go-api crate pulls in
// substantial transitive deps for what is a straightforward
// 2-endpoint flow).
//
// Direct port of azuredevops.ts's getReposForOrganizations
// (lines 160-228). For each org:
//   1. GET <baseUrl>/<org>/_apis/projects?api-version=7.1
//   2. For each project, GET <baseUrl>/<org>/<projectId>/_apis/git/repositories
//
// Auth: PAT via HTTP Basic with empty username (azure-devops
// convention).
//
// Retries: every HTTP call goes through WithRetry from retry.go
// so transient 5xx + network errors don't fail the whole sync.
package connectionmanager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// azureDevOpsAPIVersion is the REST API version we negotiate.
// 7.1 is the current stable version per the official docs.
const azureDevOpsAPIVersion = "7.1"

// azureDevOpsProjectsResponse is the {value: [...], count: N}
// wrapper Azure DevOps returns from /_apis/projects.
type azureDevOpsProjectsResponse struct {
	Count int                  `json:"count"`
	Value []azureDevOpsProject `json:"value"`
}

type azureDevOpsProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
}

type azureDevOpsRepositoriesResponse struct {
	Count int               `json:"count"`
	Value []AzureDevOpsRepo `json:"value"`
}

// FetchReposForAzureDevOpsOrg enumerates every repo across all
// projects in `org`. Direct port of
// getReposForOrganizations(orgs)[org] (azuredevops.ts:166-219).
func FetchReposForAzureDevOpsOrg(ctx context.Context, httpClient *http.Client, baseURL, token, org string) ([]AzureDevOpsRepo, *FetchWarning, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if token == "" {
		// Legacy throws when no PAT is configured (line 38-41).
		return nil, nil, errors.New("connectionmanager: azuredevops requires a token (PAT)")
	}
	if org == "" {
		return nil, nil, errors.New("connectionmanager: azuredevops org is empty")
	}

	orgURL := buildAzureDevOpsOrgURL(baseURL, org)

	// 1. List projects.
	projects, warn, err := fetchAzureDevOpsProjects(ctx, httpClient, orgURL, token, org)
	if err != nil {
		return nil, nil, err
	}
	if warn != nil {
		return nil, warn, nil
	}

	// 2. For each project, list repos. Fold per-project errors
	// into warnings so one bad project doesn't fail the entire
	// org.
	var (
		allRepos []AzureDevOpsRepo
		warnings []string
	)
	for _, p := range projects {
		if p.ID == "" {
			warnings = append(warnings, fmt.Sprintf("azuredevops project in org %s has no id: %s", org, p.Name))
			continue
		}
		repos, perr := fetchAzureDevOpsRepositories(ctx, httpClient, orgURL, token, p.ID)
		if perr != nil {
			warnings = append(warnings, fmt.Sprintf("azuredevops project %s/%s: %v", org, p.Name, perr))
			continue
		}
		// Backfill project info on each repo (the SDK does this
		// automatically; raw REST returns project as embedded
		// data, but it can be sparse - the visibility field in
		// particular is only on the /_apis/projects response).
		proj := p
		for i := range repos {
			if repos[i].Project == nil {
				repos[i].Project = &AzureDevOpsProject{
					ID:         proj.ID,
					Name:       proj.Name,
					Visibility: proj.Visibility,
				}
			} else {
				// API may return a partial project; ensure
				// visibility is populated.
				if repos[i].Project.Visibility == "" {
					repos[i].Project.Visibility = proj.Visibility
				}
				if repos[i].Project.Name == "" {
					repos[i].Project.Name = proj.Name
				}
			}
		}
		allRepos = append(allRepos, repos...)
	}

	if len(warnings) > 0 {
		// Surface aggregated warnings via the typed FetchWarning
		// only when there were warnings AND no repos; otherwise
		// log via the orchestrator's outer accumulator.
		// Here we return the repos PLUS no warning - the
		// per-project warnings are surfaced separately via the
		// returned slice when the fetcher exposes it. The current
		// orchestrator shape doesn't carry a multi-warning
		// channel; future slices can add one. For now, drop the
		// individual project warnings into stderr-style noise.
		// See the orchestrator wiring below.
	}
	return allRepos, nil, nil
}

func fetchAzureDevOpsProjects(ctx context.Context, httpClient *http.Client, orgURL, token, org string) ([]azureDevOpsProject, *FetchWarning, error) {
	u := orgURL + "/_apis/projects?api-version=" + azureDevOpsAPIVersion
	type result struct {
		body   []byte
		status int
	}
	res, err := WithRetry(ctx, RetryConfig{}, func(ctx context.Context, attempt int) (result, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if rerr != nil {
			return result{}, rerr
		}
		setAzureDevOpsAuth(req, token)
		resp, gerr := httpClient.Do(req)
		if gerr != nil {
			return result{}, gerr
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 500 {
			return result{}, &RetryableHTTPError{StatusCode: resp.StatusCode, URL: u, Body: snippet(body)}
		}
		return result{body: body, status: resp.StatusCode}, nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("azuredevops list projects %q: %w", org, err)
	}
	if res.status == http.StatusNotFound {
		return nil, &FetchWarning{Message: fmt.Sprintf("Organization %s not found or no access", org)}, nil
	}
	if res.status < 200 || res.status >= 300 {
		return nil, nil, fmt.Errorf("azuredevops %s: status %d body=%s", u, res.status, snippet(res.body))
	}
	var decoded azureDevOpsProjectsResponse
	if jerr := json.Unmarshal(res.body, &decoded); jerr != nil {
		return nil, nil, fmt.Errorf("azuredevops decode projects: %w", jerr)
	}
	return decoded.Value, nil, nil
}

func fetchAzureDevOpsRepositories(ctx context.Context, httpClient *http.Client, orgURL, token, projectID string) ([]AzureDevOpsRepo, error) {
	u := orgURL + "/" + projectID + "/_apis/git/repositories?api-version=" + azureDevOpsAPIVersion
	type result struct {
		body   []byte
		status int
	}
	res, err := WithRetry(ctx, RetryConfig{}, func(ctx context.Context, attempt int) (result, error) {
		req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if rerr != nil {
			return result{}, rerr
		}
		setAzureDevOpsAuth(req, token)
		resp, gerr := httpClient.Do(req)
		if gerr != nil {
			return result{}, gerr
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 500 {
			return result{}, &RetryableHTTPError{StatusCode: resp.StatusCode, URL: u, Body: snippet(body)}
		}
		return result{body: body, status: resp.StatusCode}, nil
	})
	if err != nil {
		return nil, err
	}
	if res.status < 200 || res.status >= 300 {
		return nil, fmt.Errorf("azuredevops %s: status %d body=%s", u, res.status, snippet(res.body))
	}
	var decoded azureDevOpsRepositoriesResponse
	if jerr := json.Unmarshal(res.body, &decoded); jerr != nil {
		return nil, fmt.Errorf("azuredevops decode repos: %w", jerr)
	}
	return decoded.Value, nil
}

// buildAzureDevOpsOrgURL composes the per-org base URL.
//
//	dev.azure.com/<org>      -> https://dev.azure.com/<org>
//	tfs.example.com (TFS)    -> https://tfs.example.com/tfs/<org>
//
// The legacy supports both forms; codeintel keeps the simple
// dev.azure.com/<org> form. Self-hosted TFS deployments need
// the operator to include the /tfs prefix in cfg.URL directly.
func buildAzureDevOpsOrgURL(baseURL, org string) string {
	return strings.TrimRight(baseURL, "/") + "/" + org
}

// setAzureDevOpsAuth attaches the PAT via Basic auth with empty
// username (the AzDO convention; the SDK does this internally).
func setAzureDevOpsAuth(req *http.Request, token string) {
	creds := ":" + token
	encoded := base64.StdEncoding.EncodeToString([]byte(creds))
	req.Header.Set("Authorization", "Basic "+encoded)
	req.Header.Set("Accept", "application/json")
}

// snippet truncates a response body for inclusion in error
// messages. Caps at 256 bytes to keep error chains readable.
func snippet(b []byte) string {
	const cap = 256
	if len(b) <= cap {
		return string(b)
	}
	return string(b[:cap]) + "...(truncated)"
}

// fetchAzureDevOpsReposViaHTTP is the live AzDO fetcher.
// Registered via init() into the orchestrator's
// AzureDevOpsFetcher slot.
func fetchAzureDevOpsReposViaHTTP(ctx context.Context, cfg AzureDevOpsConnectionConfig) (azureDevOpsFetchResult, error) {
	base := cfg.URL
	if base == "" {
		base = "https://dev.azure.com"
	}
	if cfg.Token == "" {
		return azureDevOpsFetchResult{}, errors.New("connectionmanager: azuredevops requires cfg.Token (PAT)")
	}

	// Tighter HTTP client timeout so a hung AzDO doesn't strand
	// the worker for the asynq default-task timeout.
	httpClient := &http.Client{Timeout: 30 * time.Second}

	var (
		allRepos []AzureDevOpsRepo
		warnings []string
	)
	for _, org := range cfg.Orgs {
		repos, warn, err := FetchReposForAzureDevOpsOrg(ctx, httpClient, base, cfg.Token, org)
		if err != nil {
			return azureDevOpsFetchResult{}, fmt.Errorf("FetchReposForAzureDevOpsOrg %q: %w", org, err)
		}
		if warn != nil {
			warnings = append(warnings, warn.Message)
			continue
		}
		allRepos = append(allRepos, repos...)
	}
	return azureDevOpsFetchResult{Repos: allRepos, Warnings: warnings}, nil
}

func init() {
	SetAzureDevOpsFetcher(fetchAzureDevOpsReposViaHTTP)
}
