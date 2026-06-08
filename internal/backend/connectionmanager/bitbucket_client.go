// Bitbucket Cloud API fetcher. Uses ktrysmt/go-bitbucket. The
// legacy walks bitbucket.ts's workspaces flow to enumerate
// every accessible repo per workspace; the codeintel port
// mirrors that with a per-workspace loop.
//
// On import, init() registers fetchBitbucketCloudReposViaSDK
// with the orchestrator's package-level slot so the handler's
// "bitbucket" dispatch (cloud branch) routes through here
// automatically.
package connectionmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/ktrysmt/go-bitbucket"
)

// FetchReposForBitbucketCloudWorkspace lists every repo in
// `workspace` via the authenticated bitbucket client. The
// go-bitbucket SDK auto-paginates internally; we call once and
// translate every returned repo. Wrapped in WithRetry so a
// transient 5xx or network blip doesn't fail a sync that
// would have succeeded on a retry.
func FetchReposForBitbucketCloudWorkspace(ctx context.Context, client *bitbucket.Client, workspace string) ([]BitbucketCloudRepo, *FetchWarning, error) {
	if client == nil {
		return nil, nil, errors.New("connectionmanager: bitbucket.Client is nil")
	}
	if workspace == "" {
		return nil, nil, errors.New("connectionmanager: workspace is empty")
	}

	type result struct {
		resp *bitbucket.RepositoriesRes
		warn *FetchWarning
	}
	res, err := WithRetry(ctx, RetryConfig{}, func(ctx context.Context, attempt int) (result, error) {
		if cerr := ctx.Err(); cerr != nil {
			return result{}, cerr
		}
		// SDK doesn't expose a per-call context hook; ctx
		// cancellation only fires between attempts here. The
		// pre-attempt ctx.Err() check above is the primary
		// abort path.
		opts := &bitbucket.RepositoriesOptions{Owner: workspace}
		r, err := client.Repositories.ListForAccount(opts)
		if err != nil {
			if isBitbucket404(err) {
				// 404 -> warning, not retryable.
				return result{warn: &FetchWarning{Message: fmt.Sprintf("Workspace %s not found or no access", workspace)}}, nil
			}
			if isBitbucket5xx(err) {
				return result{}, &RetryableHTTPError{StatusCode: extractBitbucketStatus(err), URL: workspace, Body: err.Error()}
			}
			return result{}, fmt.Errorf("ListForAccount %q: %w", workspace, err)
		}
		return result{resp: r}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	if res.warn != nil {
		return nil, res.warn, nil
	}
	resp := res.resp
	if resp == nil {
		return nil, nil, nil
	}

	// go-bitbucket's response Items are unstructured; pipe them
	// through json round-trip so the rich BitbucketCloudRepo
	// struct decodes the nested project + links + mainbranch.
	out := make([]BitbucketCloudRepo, 0, len(resp.Items))
	for i := range resp.Items {
		raw, err := json.Marshal(resp.Items[i])
		if err != nil {
			continue
		}
		var r BitbucketCloudRepo
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil, nil
}

// isBitbucket404 best-effort detects 404 in the SDK's error
// (the SDK returns wrapped HTTP errors with no consistent typed
// shape; string match is the realistic option).
func isBitbucket404(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "404") || contains(msg, "not found")
}

// isBitbucket5xx best-effort detects a 5xx in the SDK's error
// (same string-match limitation as isBitbucket404).
func isBitbucket5xx(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return contains(msg, "500") || contains(msg, "502") || contains(msg, "503") ||
		contains(msg, "504") || contains(msg, "Server Error")
}

// extractBitbucketStatus extracts a status code from the SDK's
// error message when possible. Returns 0 if no match - the
// retry wrapper then treats it as a generic 5xx surface.
func extractBitbucketStatus(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	for _, code := range []int{500, 502, 503, 504, 429} {
		if contains(msg, fmt.Sprintf("%d", code)) {
			return code
		}
	}
	return 500
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// buildBitbucketCloudClient constructs a bitbucket.Client. The
// SDK takes username+app-password OR an OAuth-style token. We
// map:
//   - cfg.Token + cfg.User -> NewBasicAuth (user/app-password)
//   - cfg.Token alone      -> NewOAuthbearerToken
//   - neither              -> NewBasicAuth("","") (anonymous)
//
// Self-hosted URL: cfg.URL replaces the SDK's default
// api.bitbucket.org base.
func buildBitbucketCloudClient(cfg BitbucketConnectionConfig) (*bitbucket.Client, error) {
	var (
		c    *bitbucket.Client
		errC error
	)
	switch {
	case cfg.Token != "" && cfg.User != "":
		c, errC = bitbucket.NewBasicAuth(cfg.User, cfg.Token)
	case cfg.Token != "":
		c, errC = bitbucket.NewOAuthbearerToken(cfg.Token)
	default:
		c, errC = bitbucket.NewBasicAuth("", "")
	}
	if errC != nil {
		return nil, fmt.Errorf("bitbucket.NewBasicAuth: %w", errC)
	}
	if c == nil {
		return nil, errors.New("bitbucket client returned nil")
	}
	if cfg.URL != "" {
		base, perr := url.Parse(stripTrailingSlashes(cfg.URL))
		if perr != nil {
			return nil, fmt.Errorf("bitbucket client URL %q: %w", cfg.URL, perr)
		}
		c.SetApiBaseURL(*base)
	}
	return c, nil
}

// fetchBitbucketCloudReposViaSDK is the live cloud fetcher.
// Registered via init() into the orchestrator's
// BitbucketCloudFetcher slot.
func fetchBitbucketCloudReposViaSDK(ctx context.Context, cfg BitbucketConnectionConfig) (bitbucketCloudFetchResult, error) {
	client, err := buildBitbucketCloudClient(cfg)
	if err != nil {
		return bitbucketCloudFetchResult{}, fmt.Errorf("buildBitbucketCloudClient: %w", err)
	}
	var (
		all      []BitbucketCloudRepo
		warnings []string
	)
	for _, ws := range cfg.Workspaces {
		repos, warn, err := FetchReposForBitbucketCloudWorkspace(ctx, client, ws)
		if err != nil {
			return bitbucketCloudFetchResult{}, fmt.Errorf("FetchReposForBitbucketCloudWorkspace %q: %w", ws, err)
		}
		if warn != nil {
			warnings = append(warnings, warn.Message)
			continue
		}
		all = append(all, repos...)
	}
	return bitbucketCloudFetchResult{Repos: all, Warnings: warnings}, nil
}

func init() {
	SetBitbucketCloudFetcher(fetchBitbucketCloudReposViaSDK)
}
