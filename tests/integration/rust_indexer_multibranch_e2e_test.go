//go:build integration

// Phase R.4 binding contract: a repo with multiple branches +
// metadata.branches globs gets ALL matching branches indexed
// into Zoekt, AND metadata.indexedRevisions is stamped on the
// Repo row so the Go-side branchStatuses projection (P.3c)
// reports each branch as indexed=true.
//
// Mirrors the multi-branch logic in
// packages/backend/src/repoIndexManager.ts:780-832 end-to-end.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/asynqbridge"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// buildMultiBranchRemote constructs a real local bare repo with
// THREE branches: main + feature/auth + feature/billing. The
// fake remote is the input to the multi-branch Rust indexer.
func buildMultiBranchRemote(t *testing.T, root string) (cloneURL string) {
	t.Helper()
	wt := filepath.Join(root, "src")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	repo, err := git.PlainInit(wt, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	tree, _ := repo.Worktree()
	author := &object.Signature{Name: "t", Email: "t@e", When: time.Now()}

	// main branch with a base commit.
	_ = os.WriteFile(filepath.Join(wt, "README.md"), []byte("base\n"), 0o644)
	_, _ = tree.Add("README.md")
	_, _ = tree.Commit("base", &git.CommitOptions{Author: author})

	// Determine the initial branch name (could be "master" or "main"
	// depending on git config). Force a `main` ref for the test.
	head, _ := repo.Head()
	initialBranch := head.Name().Short()
	if initialBranch != "main" {
		// Rename the initial branch to "main".
		_ = repo.Storer.RemoveReference(plumbing.NewBranchReferenceName(initialBranch))
		mainRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head.Hash())
		_ = repo.Storer.SetReference(mainRef)
		// Update HEAD to point at main.
		symRef := plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))
		_ = repo.Storer.SetReference(symRef)
	}

	// Branch off "feature/auth" + add a commit.
	if err := tree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature/auth"),
		Create: true,
	}); err != nil {
		t.Fatalf("checkout feature/auth: %v", err)
	}
	_ = os.WriteFile(filepath.Join(wt, "auth.txt"), []byte("auth feature\n"), 0o644)
	_, _ = tree.Add("auth.txt")
	_, _ = tree.Commit("feature/auth: add auth.txt", &git.CommitOptions{Author: author})

	// Back to main, branch "feature/billing" + add a commit.
	if err := tree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main")}); err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if err := tree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature/billing"),
		Create: true,
	}); err != nil {
		t.Fatalf("checkout feature/billing: %v", err)
	}
	_ = os.WriteFile(filepath.Join(wt, "billing.txt"), []byte("billing feature\n"), 0o644)
	_, _ = tree.Add("billing.txt")
	_, _ = tree.Commit("feature/billing: add billing.txt", &git.CommitOptions{Author: author})

	// Use the working tree directly via file:// — go-git's
	// PlainClone(bare=true) only copies the active branch into
	// the bare repo's `refs/heads/*` (other branches land under
	// `refs/remotes/origin/*` which the indexer's clone won't
	// pick up via the default refspec `+refs/heads/*:...`).
	// The working tree has all branches in `refs/heads/*`
	// natively because we created them in-process. In
	// production the remote is GitHub/GitLab, which exposes
	// every branch under `refs/heads/*` — same shape.
	return "file://" + wt
}

func TestParity_RustIndexer_MultiBranch_AllResolved(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset", envRedisURL)
	}
	requireLocalRedisOrSkip(t, redisURL)

	binary := locateRustIndexerBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	rootTmp := t.TempDir()
	cloneURL := buildMultiBranchRemote(t, rootTmp)
	t.Logf("multi-branch remote: %s (main + feature/auth + feature/billing)", cloneURL)

	// Bootstrap.
	suffix := uuid.NewString()[:8]
	orgName := "r4-mb-" + suffix
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	userID := uuid.NewString()
	_, _ = pool.Exec(ctx, `INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())`,
		userID, orgName+"-o@test.local", "owner")
	_, _ = pool.Exec(ctx, `INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')`, userID, orgID)
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	_, _ = pool.Exec(ctx, `INSERT INTO "ApiKey" (hash, name, "orgId", "createdById") VALUES ($1, $2, $3, $4)`,
		apiKeyHash, "key-"+suffix, orgID, userID)
	bearer := "Bearer " + auth.ApiKeyPrefix + apiSecret

	// Repo seeded with metadata.branches = ["main", "feature/*"].
	// Per legacy logic + R.4 port, this should resolve to:
	//   refs/heads/main, refs/heads/feature/auth, refs/heads/feature/billing
	repoMeta := map[string]any{
		"branches": []string{"main", "feature/*"},
	}
	metaJSON, _ := json.Marshal(repoMeta)
	var repoID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt", "defaultBranch"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW(), 'main')
		RETURNING id
	`, "r4-mb-repo-"+suffix, cloneURL, "ext-"+suffix, "github", "https://github.com",
		string(metaJSON), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}
	var connID int32
	cfg, _ := json.Marshal(map[string]any{"type": "github"})
	_ = pool.QueryRow(ctx, `INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt") VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id`,
		"r4-mb-conn-"+suffix, cfg, "github", orgID).Scan(&connID)
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)

	// Spawn Rust worker with zoekt-git-index for full multi-
	// branch shard production.
	dataCacheDir := filepath.Join(rootTmp, "data")
	_ = os.MkdirAll(dataCacheDir, 0o755)
	zoektBinary := locateOrBuildZoektBinary(t)
	rustCtx, rustCancel := context.WithCancel(ctx)
	defer rustCancel()
	rustArgs := []string{
		"worker",
		"--redis-url", redisURL,
		"--postgres-url", dsn,
		"--data-cache-dir", dataCacheDir,
		"--concurrency", "1",
	}
	if zoektBinary != "" {
		rustArgs = append(rustArgs, "--zoekt-binary", zoektBinary)
	}
	rustCmd := exec.CommandContext(rustCtx, binary, rustArgs...)
	rustCmd.Stdout = newPrefixWriter(t, "rust-mb-stdout")
	rustCmd.Stderr = newPrefixWriter(t, "rust-mb-stderr")
	if err := rustCmd.Start(); err != nil {
		t.Fatalf("start rust worker: %v", err)
	}
	defer func() {
		_ = rustCmd.Process.Signal(os.Interrupt)
		_ = rustCmd.Wait()
	}()
	time.Sleep(800 * time.Millisecond)

	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encryptionKey,
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
		RepoStatusFetcher: api.NewPgxRepoStatusFetcher(pool.Pool),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	t.Log("POST /api/repos/{id}/index (multi-branch)")
	postURL := fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, postURL, bytes.NewReader(nil))
	req.Header.Set("Authorization", bearer)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST: %d body=%s", resp.StatusCode, body)
	}
	var postResp struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(body, &postResp)
	t.Logf("jobId=%s; awaiting Rust completion", postResp.JobID)

	// Poll to COMPLETED.
	deadline := time.Now().Add(60 * time.Second)
	var finalStatus, errMsg string
	for time.Now().Before(deadline) {
		_ = pool.QueryRow(ctx, `SELECT status::text, COALESCE("errorMessage", '') FROM "RepoIndexingJob" WHERE id = $1`,
			postResp.JobID).Scan(&finalStatus, &errMsg)
		if finalStatus == "COMPLETED" || finalStatus == "FAILED" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if finalStatus != "COMPLETED" {
		t.Fatalf("job status: got %q errorMessage=%q want COMPLETED", finalStatus, errMsg)
	}

	// ---- R.4 binding contract assertions ----

	t.Run("metadata.indexedRevisions stamped with all 3 refs", func(t *testing.T) {
		var metaRaw []byte
		_ = pool.QueryRow(ctx, `SELECT metadata FROM "Repo" WHERE id = $1`, repoID).Scan(&metaRaw)
		var meta map[string]any
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			t.Fatalf("decode metadata: %v raw=%s", err, metaRaw)
		}
		rawList, ok := meta["indexedRevisions"].([]any)
		if !ok {
			t.Fatalf("indexedRevisions not an array: %v", meta["indexedRevisions"])
		}
		got := make(map[string]bool, len(rawList))
		for _, v := range rawList {
			if s, ok := v.(string); ok {
				got[s] = true
			}
		}
		for _, want := range []string{
			"refs/heads/main",
			"refs/heads/feature/auth",
			"refs/heads/feature/billing",
		} {
			if !got[want] {
				t.Errorf("indexedRevisions missing %q; got=%v", want, got)
			}
		}
		// Original metadata.branches must be preserved (the
		// serde flatten round-trip).
		if br, _ := meta["branches"].([]any); len(br) != 2 {
			t.Errorf("metadata.branches lost: %v", meta["branches"])
		}
	})

	t.Run("GET /api/repos/{id}/status reports all 3 branches as indexed", func(t *testing.T) {
		statusURL := fmt.Sprintf("%s/api/repos/%d/status", httpSrv.URL, repoID)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, statusURL, nil)
		req.Header.Set("Authorization", bearer)
		resp, err := httpSrv.Client().Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var body api.RepoStatusResponse
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("decode: %v body=%s", err, bodyBytes)
		}
		// branchStatuses[] should include main + feature/auth +
		// feature/billing, all with indexed=true (since
		// metadata.indexedRevisions now contains their refs).
		seen := map[string]bool{}
		for _, b := range body.BranchStatuses {
			if b.Indexed {
				seen[b.Branch] = true
			}
		}
		for _, want := range []string{"main", "feature/auth", "feature/billing"} {
			if !seen[want] {
				t.Errorf("branchStatuses missing indexed=true entry for %q; got=%v",
					want, branchStatusesSummary(body))
			}
		}
	})

	t.Run("Zoekt multi-branch shard produced", func(t *testing.T) {
		if zoektBinary == "" {
			t.Skip("zoekt-git-index binary not configured; can't validate shard")
		}
		indexDir := filepath.Join(dataCacheDir, "index")
		entries, err := os.ReadDir(indexDir)
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		prefix := fmt.Sprintf("%d_%d_", orgID, repoID)
		var shardCount int
		for _, e := range entries {
			if !hasExt(e.Name(), ".zoekt") {
				continue
			}
			if !startsWithStr(e.Name(), prefix) {
				continue
			}
			shardCount++
			t.Logf("found multi-branch shard: %s", e.Name())
		}
		if shardCount < 1 {
			t.Errorf("no shards matching %s* in %s", prefix, indexDir)
		}
	})

	t.Logf("R.4 multi-branch PASS: 3 branches resolved + indexed + stamped + status reflects all")
}

func branchStatusesSummary(b api.RepoStatusResponse) []string {
	out := make([]string, 0, len(b.BranchStatuses))
	for _, s := range b.BranchStatuses {
		out = append(out, fmt.Sprintf("%s(indexed=%v)", s.Branch, s.Indexed))
	}
	return out
}
