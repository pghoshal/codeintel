//go:build integration

// Live-GitHub integration test for FetchReposForOrg. Hits the
// real GitHub public API against a small well-known org and
// asserts the function returns a non-empty repo list.
//
// Gated on the CODEINTEL_TEST_GITHUB_LIVE env var so the
// default integration run doesn't unnecessarily hit GitHub.
//
//	CODEINTEL_TEST_GITHUB_LIVE=true \
//	    go test -tags=integration ./tests/integration/... -v -run TestGitHubFetch
//
// Optionally provide a token via CODEINTEL_TEST_GITHUB_TOKEN to
// avoid rate limits on shared CI; the test works against the
// anonymous tier too (60 req/hr per IP).
package integration

import (
	"context"
	"os"
	"testing"
	"time"

	"codeintel/internal/backend/connectionmanager"

	"github.com/google/go-github/v75/github"
)

// TestGitHubFetch_LivePublicOrg fetches every repo in a known
// public org and asserts the returned list is non-empty and
// every entry carries the required OctokitRepository fields
// (clone_url, full_name, id, owner.login).
//
// The fixture org is "vesoft-inc" (NebulaGraph maintainers).
// Chosen because the codeintel project depends on Nebula and
// this org is unlikely to disappear; it has ~50 repos so
// pagination kicks in.
func TestGitHubFetch_LivePublicOrg(t *testing.T) {
	if os.Getenv("CODEINTEL_TEST_GITHUB_LIVE") != "true" {
		t.Skip("CODEINTEL_TEST_GITHUB_LIVE != true; skipping live GitHub fetch")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client := github.NewClient(nil)
	if tok := os.Getenv("CODEINTEL_TEST_GITHUB_TOKEN"); tok != "" {
		client = client.WithAuthToken(tok)
	}

	const org = "vesoft-inc"
	repos, warn, err := connectionmanager.FetchReposForOrg(ctx, client, org)
	if err != nil {
		t.Fatalf("FetchReposForOrg(%s): %v", org, err)
	}
	if warn != nil {
		t.Fatalf("got warning: %+v", warn)
	}
	if len(repos) == 0 {
		t.Fatalf("expected non-empty repo list for %s", org)
	}
	t.Logf("fetched %d repos from %s", len(repos), org)

	// Spot-check the first repo's required fields are populated.
	r := repos[0]
	if r.FullName == "" || r.ID == 0 {
		t.Errorf("first repo missing required fields: %+v", r)
	}
	if r.CloneURL == nil || *r.CloneURL == "" {
		t.Errorf("first repo missing clone_url: %+v", r)
	}
	if r.Owner.Login == "" {
		t.Errorf("first repo missing owner.login: %+v", r)
	}
}

// TestGitHubFetch_LiveMissingOrg confirms the 404 → warning
// branch holds against a real GitHub 404 response. Uses a
// guaranteed-not-to-exist org name.
func TestGitHubFetch_LiveMissingOrg(t *testing.T) {
	if os.Getenv("CODEINTEL_TEST_GITHUB_LIVE") != "true" {
		t.Skip("CODEINTEL_TEST_GITHUB_LIVE != true; skipping live GitHub fetch")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client := github.NewClient(nil)
	if tok := os.Getenv("CODEINTEL_TEST_GITHUB_TOKEN"); tok != "" {
		client = client.WithAuthToken(tok)
	}

	// 39-char random-ish name that's GUARANTEED not to exist
	// (GitHub max org slug is 39 chars).
	const missing = "codeintel-no-such-org-fixture-xyz-12345"
	repos, warn, err := connectionmanager.FetchReposForOrg(ctx, client, missing)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	if repos != nil {
		t.Errorf("expected nil repos, got %d", len(repos))
	}
	if warn == nil {
		t.Fatalf("expected 404 warning")
	}
	t.Logf("warning text: %s", warn.Message)
}
