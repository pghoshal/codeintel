//go:build integration

// Phase R.2 live E2E: full HTTP → asynq → Rust indexer → Postgres
// roundtrip.
//
// The test:
//   1. Spawns the codeintel-indexer-rs `worker` subprocess
//      (built via `cargo build --release` ahead of time).
//   2. Seeds an Org + Repo + Connection + RepoToConnection +
//      a real local bare git repo as the clone source.
//   3. POSTs `/api/repos/{id}/index` over real HTTP, which
//      pushes an INDEX task to the asynq `repo-index-rust`
//      queue.
//   4. The Rust subprocess consumes via the `asynq` crate
//      (wire-compatible with hibiken/asynq).
//   5. Rust clones the repo, UPDATEs Repo.indexedCommitHash +
//      RepoIndexingJob.status COMPLETED.
//   6. The Go test polls Postgres until the job lands at
//      COMPLETED, then asserts:
//        - Repo.indexedCommitHash == the seeded remote HEAD.
//        - The on-disk clone dir exists at
//          <data_cache_dir>/repos/<repo_id>/.
//        - The RepoIndexingJob row's status === COMPLETED.
//
// Skipped if CODEINTEL_RUST_INDEXER_BIN is not set AND the
// release binary isn't at the conventional path. This is the
// binding contract for HPA: multiple Rust pods can subscribe
// to the same queue; this test exercises one pod.
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
	"strconv"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/asynqbridge"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

func TestParity_RustIndexer_E2E_FullRoundTrip(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping Rust indexer E2E", envRedisURL)
	}
	requireLocalRedisOrSkip(t, redisURL)

	binary := locateRustIndexerBinary(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	// Build a real local bare git repo as the clone source.
	rootTmp := t.TempDir()
	cloneURL, expectedHead, expectedContent := buildRustE2EFakeRemote(t, rootTmp)
	t.Logf("fake remote: %s HEAD=%s", cloneURL, expectedHead)

	// Bootstrap Org + Repo + Connection + ApiKey.
	suffix := uuid.NewString()[:8]
	orgName := "r2-e2e-" + suffix
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

	emptyMD, _ := json.Marshal(map[string]any{})
	var repoID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`, "r2-repo-"+suffix, cloneURL, "ext-"+suffix, "github", "https://github.com",
		string(emptyMD), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}
	var connID int32
	cfg, _ := json.Marshal(map[string]any{"type": "github"})
	_ = pool.QueryRow(ctx, `INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt") VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id`,
		"r2-conn-"+suffix, cfg, "github", orgID).Scan(&connID)
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)

	// Spin up the Rust worker subprocess.
	dataCacheDir := filepath.Join(rootTmp, "data")
	if err := os.MkdirAll(dataCacheDir, 0o755); err != nil {
		t.Fatalf("mkdir dataCacheDir: %v", err)
	}
	rustCtx, rustCancel := context.WithCancel(ctx)
	defer rustCancel()

	// R.3: locate (and build if needed) the zoekt-git-index
	// binary. Empty path -> Rust worker runs in clone-only
	// mode, which is the prior R.2 behaviour. With a real
	// binary path, the worker also produces Zoekt shards.
	zoektBinary := locateOrBuildZoektBinary(t)

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
	rustCmd.Stdout = newPrefixWriter(t, "rust-stdout")
	rustCmd.Stderr = newPrefixWriter(t, "rust-stderr")
	if err := rustCmd.Start(); err != nil {
		t.Fatalf("start rust worker: %v", err)
	}
	defer func() {
		// SIGTERM the worker on test exit; then wait a moment.
		_ = rustCmd.Process.Signal(os.Interrupt)
		_ = rustCmd.Wait()
	}()
	// Give the worker a beat to subscribe before enqueueing.
	time.Sleep(800 * time.Millisecond)

	// Wire the HTTP API with the asynq producer.
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
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	// POST /api/repos/{id}/index — Go enqueues INDEX task to
	// `repo-index-rust` queue; Rust subprocess consumes it.
	t.Log("POST /api/repos/{id}/index")
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
		t.Fatalf("POST status: got %d body=%s", resp.StatusCode, body)
	}
	var postResp struct {
		JobID string `json:"jobId"`
	}
	_ = json.Unmarshal(body, &postResp)
	if postResp.JobID == "" {
		t.Fatalf("POST missing jobId: %s", body)
	}
	t.Logf("enqueued jobId=%s; awaiting Rust completion", postResp.JobID)

	// Poll RepoIndexingJob.status until COMPLETED (or FAILED).
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
		t.Fatalf("job final status: got %q errorMessage=%q want COMPLETED", finalStatus, errMsg)
	}

	t.Run("Repo.indexedCommitHash matches real remote HEAD", func(t *testing.T) {
		var hash *string
		_ = pool.QueryRow(ctx, `SELECT "indexedCommitHash" FROM "Repo" WHERE id = $1`, repoID).Scan(&hash)
		if hash == nil {
			t.Fatalf("indexedCommitHash nil")
		}
		if *hash != expectedHead {
			t.Errorf("indexedCommitHash: got %s want %s", *hash, expectedHead)
		}
	})
	t.Run("Repo.indexedAt populated", func(t *testing.T) {
		var indexedAt *time.Time
		_ = pool.QueryRow(ctx, `SELECT "indexedAt" FROM "Repo" WHERE id = $1`, repoID).Scan(&indexedAt)
		if indexedAt == nil {
			t.Errorf("indexedAt nil after Rust INDEX")
		}
	})
	t.Run("Clone dir on disk with real README content", func(t *testing.T) {
		dest := filepath.Join(dataCacheDir, "repos", strconv.Itoa(int(repoID)))
		content, err := os.ReadFile(filepath.Join(dest, "README.md"))
		if err != nil {
			t.Fatalf("read README: %v", err)
		}
		if string(content) != expectedContent {
			t.Errorf("README: got %q want %q", content, expectedContent)
		}
		if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
			t.Errorf(".git dir missing: %v", err)
		}
	})
	t.Run("Repo.latestIndexingJobStatus refreshed to COMPLETED", func(t *testing.T) {
		var latest *string
		_ = pool.QueryRow(ctx, `SELECT "latestIndexingJobStatus"::text FROM "Repo" WHERE id = $1`, repoID).Scan(&latest)
		if latest == nil || *latest != "COMPLETED" {
			t.Errorf("latestIndexingJobStatus: got %v want COMPLETED", latest)
		}
	})

	// R.3 assertion: when zoekt-git-index is wired in, a real
	// `.zoekt` shard appears in <data_cache_dir>/index with
	// the `<orgId>_<repoId>` prefix. Skip cleanly when the
	// binary wasn't available (clone-only mode covered by R.2).
	t.Run("R.3 Zoekt shard produced on disk", func(t *testing.T) {
		if zoektBinary == "" {
			t.Skip("zoekt-git-index binary not configured; running in clone-only mode (R.2)")
		}
		indexDir := filepath.Join(dataCacheDir, "index")
		entries, err := os.ReadDir(indexDir)
		if err != nil {
			t.Fatalf("readdir %s: %v", indexDir, err)
		}
		prefix := fmt.Sprintf("%d_%d", orgID, repoID)
		var shardCount int
		for _, e := range entries {
			name := e.Name()
			if !hasExt(name, ".zoekt") {
				continue
			}
			if !startsWithStr(name, prefix) {
				continue
			}
			t.Logf("found shard: %s", name)
			shardCount++
		}
		if shardCount < 1 {
			t.Errorf("no zoekt shards matching prefix %q in %s; entries=%v",
				prefix, indexDir, dirNames(entries))
		}
	})

	t.Logf("R.3 PASS: HTTP -> Go asynq enqueue -> Rust worker -> clone+zoekt -> Postgres update -> jobId=%s COMPLETED", postResp.JobID)
}

func hasExt(name, ext string) bool {
	if len(name) < len(ext) {
		return false
	}
	return name[len(name)-len(ext):] == ext
}

func startsWithStr(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func dirNames(entries []os.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}

// locateOrBuildZoektBinary returns a path to a working
// zoekt-git-index binary, or "" if not available. Order:
//  1. $CODEINTEL_ZOEKT_GIT_INDEX_PATH (operator-supplied).
//  2. Build from the vendored source at third_party/zoekt/cmd/zoekt-git-index
//     into the test's tmp dir.
//  3. Skip (return "") if the vendored source isn't present.
func locateOrBuildZoektBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CODEINTEL_ZOEKT_GIT_INDEX_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			t.Logf("zoekt: using env-provided binary %s", p)
			return p
		}
		t.Logf("zoekt: env CODEINTEL_ZOEKT_GIT_INDEX_PATH=%s but stat failed; falling through", p)
	}
	wd, _ := os.Getwd()
	vendorDir := filepath.Clean(filepath.Join(wd, "..", "..", "..", "third_party", "zoekt"))
	t.Logf("zoekt: looking for vendor dir at %s", vendorDir)
	if _, err := os.Stat(filepath.Join(vendorDir, "cmd", "zoekt-git-index")); err != nil {
		t.Logf("zoekt: SKIP — third_party/zoekt not present at %s: %v", vendorDir, err)
		return ""
	}
	outDir := t.TempDir()
	outPath := filepath.Join(outDir, "zoekt-git-index")
	cmd := exec.Command("go", "build", "-o", outPath, "./cmd/zoekt-git-index")
	cmd.Dir = vendorDir
	cmd.Env = append(os.Environ(), "GOFLAGS=") // clear any -mod=vendor etc.
	t.Logf("zoekt: building via `go build` in %s -> %s", vendorDir, outPath)
	if buildOut, err := cmd.CombinedOutput(); err != nil {
		t.Logf("zoekt: SKIP — go build failed: %v\n%s", err, buildOut)
		return ""
	}
	t.Logf("zoekt: built %s", outPath)
	return outPath
}

func buildRustE2EFakeRemote(t *testing.T, root string) (cloneURL, expectedHead, expectedContent string) {
	t.Helper()
	wt := filepath.Join(root, "src")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	repo, err := git.PlainInit(wt, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	tree, _ := repo.Worktree()
	author := &object.Signature{Name: "t", Email: "t@e", When: time.Now()}
	for _, body := range []string{"alpha\n", "beta\n"} {
		_ = os.WriteFile(filepath.Join(wt, "README.md"), []byte(body), 0o644)
		_, _ = tree.Add("README.md")
		_, _ = tree.Commit("c "+body, &git.CommitOptions{Author: author})
		expectedContent = body
	}
	h, _ := repo.Head()
	expectedHead = h.Hash().String()

	bare := filepath.Join(root, "remote.git")
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: wt}); err != nil {
		t.Fatalf("PlainClone bare: %v", err)
	}
	cloneURL = "file://" + bare
	return
}

// newPrefixWriter returns an io.Writer that streams the
// subprocess output into t.Log with a prefix, so failures in
// the subprocess surface in the Go test output.
func newPrefixWriter(t *testing.T, prefix string) io.Writer {
	return &prefixWriter{t: t, prefix: prefix}
}

type prefixWriter struct {
	t      *testing.T
	prefix string
	buf    []byte
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		nl := -1
		for i, c := range w.buf {
			if c == '\n' {
				nl = i
				break
			}
		}
		if nl < 0 {
			break
		}
		line := string(w.buf[:nl])
		w.buf = w.buf[nl+1:]
		w.t.Logf("[%s] %s", w.prefix, line)
	}
	return len(p), nil
}
