// Bitbucket orchestrator. Same injectable-fetcher pattern as
// gitea — the SDK-backed fetcher registers via init() once the
// SDK client lands (B.7-ii). Until then, CompileBitbucketFromConfig
// surfaces ErrBitbucketFetcherNotConfigured.
//
// The dispatch on deploymentType (cloud vs server) happens
// inside the fetcher path, not in the orchestrator: the fetcher
// returns []BitbucketCloudRepo for cloud / []BitbucketServerRepo
// for server (server lands later); each compile path handles
// its own shape.
package connectionmanager

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// BitbucketCloudFetcher returns the live cloud-API result for a
// connection config. server fetcher lands separately.
type BitbucketCloudFetcher func(ctx context.Context, cfg BitbucketConnectionConfig) (bitbucketCloudFetchResult, error)

type bitbucketCloudFetchResult struct {
	Repos    []BitbucketCloudRepo
	Warnings []string
}

// ErrBitbucketFetcherNotConfigured is returned when no fetcher
// has been registered.
var ErrBitbucketFetcherNotConfigured = errors.New("connectionmanager: bitbucket fetcher not configured; see SetBitbucketCloudFetcher")

// ErrBitbucketServerNotYetSupported is the legacy sentinel for
// the server-branch unimplemented state. Kept for tests but no
// longer returned by CompileBitbucketFromConfig (server lands
// in B.7-iii); replaced by ErrBitbucketFetcherNotConfigured
// when the server fetcher isn't wired.
var ErrBitbucketServerNotYetSupported = errors.New("connectionmanager: bitbucket server deployment not yet supported")

var (
	bitbucketCloudFetcherMu sync.RWMutex
	bitbucketCloudFetcher   BitbucketCloudFetcher
)

// SetBitbucketCloudFetcher registers the production cloud
// fetcher. Called once at init() of the package owning the live
// SDK client.
func SetBitbucketCloudFetcher(f BitbucketCloudFetcher) {
	bitbucketCloudFetcherMu.Lock()
	bitbucketCloudFetcher = f
	bitbucketCloudFetcherMu.Unlock()
}

// CompileBitbucketFromConfig is the high-level "fetch then
// compile" entrypoint the handler calls when conn.ConnectionType
// is "bitbucket". Dispatches on cfg.DeploymentType.
func CompileBitbucketFromConfig(ctx context.Context, cfg BitbucketConnectionConfig, connectionID int32) ([]RepoData, []string, error) {
	deployment := cfg.DeploymentType
	if deployment == "" {
		// Legacy defaults to cloud when deploymentType is unset.
		deployment = BitbucketDeploymentCloud
	}
	if deployment != BitbucketDeploymentCloud && deployment != BitbucketDeploymentServer {
		return nil, nil, fmt.Errorf("connectionmanager: unknown bitbucket deploymentType %q", deployment)
	}

	in := GitHubCompileInput{
		HostURL: ResolveBitbucketHostURL(cfg),
	}
	if cfg.Revisions != nil {
		in.Branches = cfg.Revisions.Branches
		in.Tags = cfg.Revisions.Tags
	}

	if deployment == BitbucketDeploymentCloud {
		bitbucketCloudFetcherMu.RLock()
		f := bitbucketCloudFetcher
		bitbucketCloudFetcherMu.RUnlock()
		if f == nil {
			return nil, nil, ErrBitbucketFetcherNotConfigured
		}
		fetched, err := f(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		records, compileWarnings := CompileBitbucketCloudConfig(fetched.Repos, in, connectionID)
		return records, append(fetched.Warnings, compileWarnings...), nil
	}

	// Server branch.
	bitbucketServerFetcherMu.RLock()
	sf := bitbucketServerFetcher
	bitbucketServerFetcherMu.RUnlock()
	if sf == nil {
		return nil, nil, ErrBitbucketFetcherNotConfigured
	}
	srvFetched, err := sf(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	records, compileWarnings := CompileBitbucketServerConfig(srvFetched.Repos, in, connectionID, srvFetched.ServerPublicAccessEnabled)
	return records, append(srvFetched.Warnings, compileWarnings...), nil
}

// ResolveBitbucketHostURL defaults to https://bitbucket.org for
// cloud (legacy line 412). Server deployments MUST configure
// cfg.URL explicitly.
func ResolveBitbucketHostURL(cfg BitbucketConnectionConfig) string {
	if cfg.URL == "" {
		return "https://bitbucket.org"
	}
	return stripTrailingSlashes(cfg.URL)
}
