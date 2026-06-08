// Gitea orchestrator. Until the live SDK client lands in a
// follow-up slice, CompileGiteaFromConfig delegates fetching to
// an injectable hook on the package; production wires it once
// the gitea SDK fetcher is in place. The handler dispatch can
// already route to this function, so the wiring is complete
// from the codeintel-app + codeintel-backend perspective —
// only the live API call is deferred.
package connectionmanager

import (
	"context"
	"errors"
	"sync"
)

// GiteaFetcher is the optional injection point for the live
// gitea API fetcher. Set via SetGiteaFetcher; nil means the
// fetcher hasn't been wired yet and CompileGiteaFromConfig
// returns ErrGiteaFetcherNotConfigured.
//
// This pattern lets the worker's dispatch be wired NOW even
// though the SDK-backed fetcher lands in a later slice. When
// a real gitea connection's task hits the handler, it'll fail
// loudly with the typed error instead of silently succeeding
// or panicking.
type GiteaFetcher func(ctx context.Context, cfg GiteaConnectionConfig) (giteaFetchResult, error)

type giteaFetchResult struct {
	Repos    []GiteaRepo
	Warnings []string
}

// ErrGiteaFetcherNotConfigured is the sentinel returned when
// CompileGiteaFromConfig runs but no fetcher has been
// registered.
var ErrGiteaFetcherNotConfigured = errors.New("connectionmanager: gitea fetcher not configured; see SetGiteaFetcher")

var (
	giteaFetcherMu sync.RWMutex
	giteaFetcher   GiteaFetcher
)

// SetGiteaFetcher registers the production gitea fetcher.
// Caller is responsible for happens-before ordering — typically
// called once in init() of the package that owns the live SDK
// client, before any task hits the handler.
func SetGiteaFetcher(f GiteaFetcher) {
	giteaFetcherMu.Lock()
	giteaFetcher = f
	giteaFetcherMu.Unlock()
}

// CompileGiteaFromConfig is the gitea equivalent of
// CompileFromConfig / CompileGitLabFromConfig. Resolves the
// registered fetcher (or returns ErrGiteaFetcherNotConfigured),
// invokes it, and runs the per-repo compile loop.
func CompileGiteaFromConfig(ctx context.Context, cfg GiteaConnectionConfig, connectionID int32) ([]RepoData, []string, error) {
	giteaFetcherMu.RLock()
	f := giteaFetcher
	giteaFetcherMu.RUnlock()
	if f == nil {
		return nil, nil, ErrGiteaFetcherNotConfigured
	}

	fetched, err := f(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}

	in := GitHubCompileInput{
		HostURL: ResolveGiteaHostURL(cfg),
	}
	if cfg.Revisions != nil {
		in.Branches = cfg.Revisions.Branches
		in.Tags = cfg.Revisions.Tags
	}
	return CompileGiteaConfig(fetched.Repos, in, connectionID), fetched.Warnings, nil
}

// ResolveGiteaHostURL is the analog of ResolveHostURL /
// ResolveGitLabHostURL. Defaults to https://gitea.com (legacy
// line 258).
func ResolveGiteaHostURL(cfg GiteaConnectionConfig) string {
	if cfg.URL == "" {
		return "https://gitea.com"
	}
	return stripTrailingSlashes(cfg.URL)
}
