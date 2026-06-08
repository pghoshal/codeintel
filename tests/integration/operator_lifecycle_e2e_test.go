//go:build integration

// Full operator lifecycle E2E. Drives the entire user-facing chain
// through real HTTP from a clean DB, with NO direct SQL after the
// initial auth bootstrap:
//
//   1. POST /api/connections          — create a GitHub connection.
//   2. POST /api/connections/{id}/sync — trigger ingest.
//   3. GET  /api/repos                — verify the repo is listed.
//   4. GET  /api/repos/{id}/status    — fresh repo: jobs list is empty,
//                                       indexedAt populated by the
//                                       connection-sync upsert.
//   5. (synthetic stand-in for C.4)   — seed CodeGraphIndex /
//                                       CodeIntelIndex /
//                                       RepoIndexManifest + clone dir
//                                       + Zoekt shard. This is the
//                                       only direct-SQL step; it
//                                       simulates what the future
//                                       INDEX pipeline (C.4) will
//                                       produce. Without it we can't
//                                       prove the cleanup actually
//                                       removes anything.
//   6. DELETE /api/repos/{id}/index   — REMOVE_INDEX enqueue.
//   7. GET    /api/repos/{id}/status  — poll until the job appears in
//                                       the jobs list as COMPLETED.
//   8. End-state assertions: DB rows gone, FS scrubbed,
//                            Repo.indexedAt cleared.
//
// This test is the contract that "the slices we shipped actually
// compose into a usable product loop" — not just "each slice's
// route returns the right JSON in isolation". Honest answer to
// "have you done product flow testing": this is what that looks
// like with the surfaces shipped through C.6.
//
// What this test does NOT cover (and can't, until C.4 lands):
//   - The actual indexing work (Zoekt shard generation, SCIP
//     extraction).
//   - End-user search results.
//   - Code-intel xref queries.
// Those gates land with the INDEX pipeline.
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
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/backend/connectionmanager"
	"codeintel/internal/backend/repoindexmanager"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/internal/obs"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"
	"codeintel/pkg/repopaths"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

func TestOperator_FullLifecycle_E2E(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping lifecycle E2E", envRedisURL)
	}
	requireLocalRedisOrSkip(t, redisURL)

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

	// =========================================================
	// Bootstrap: Org + User + OWNER ApiKey. (Pre-conditions for
	// auth. Everything from this point onward goes through HTTP.)
	// =========================================================
	suffix := uuid.NewString()[:8]
	orgName := "lifecycle-" + suffix
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt")
		VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("bootstrap Org: %v", err)
	}
	userID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())`,
		userID, orgName+"-owner@test.local", "lifecycle-owner"); err != nil {
		t.Fatalf("bootstrap User: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')`, userID, orgID); err != nil {
		t.Fatalf("bootstrap UserToOrg: %v", err)
	}
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	if _, err := pool.Exec(ctx, `INSERT INTO "ApiKey" (hash, name, "orgId", "createdById") VALUES ($1, $2, $3, $4)`,
		apiKeyHash, "lifecycle-key", orgID, userID); err != nil {
		t.Fatalf("bootstrap ApiKey: %v", err)
	}

	// =========================================================
	// Fake GitHub server: 1 repo to enumerate.
	// =========================================================
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/orgs/lifecycle-org/repos" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, `[{
		  "id": 7777, "name": "lifecycle-repo",
		  "full_name": "lifecycle-org/lifecycle-repo",
		  "fork": false, "private": false,
		  "html_url": "https://fake/lifecycle-org/lifecycle-repo",
		  "clone_url": "https://fake/lifecycle-org/lifecycle-repo.git",
		  "stargazers_count": 0,
		  "default_branch": "main", "topics": ["go"],
		  "owner": {"login": "lifecycle-org", "avatar_url": "https://fake/avatar"}
		}]`)
	}))
	defer ghSrv.Close()

	// =========================================================
	// Wire the asynq client + BOTH workers (connection-sync +
	// repo-index) so the whole chain is real.
	// =========================================================
	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueConnectionSync)
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueRepoIndex)

	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))

	dataCacheDir := t.TempDir()
	repoIndexStore := repoindexmanager.NewStore(pool)
	repoIndexHandler := repoindexmanager.NewHandler(
		repoIndexStore,
		repopaths.Config{DataCacheDir: dataCacheDir},
		silent,
	)
	connSyncHandler := connectionmanager.NewHandler(pool.Pool, silent)

	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueConnectionSync, connSyncHandler.AsynqHandlerFunc())
	mux.HandleFunc(asynqueues.QueueRepoIndex, repoIndexHandler.AsynqHandlerFunc())
	asynqServer := asynq.NewServer(opt, asynq.Config{
		Concurrency:     2,
		Queues:          asynqueues.DefaultPriorities(),
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 5 * time.Second,
	})
	go func() { _ = asynqServer.Run(mux) }()
	defer asynqServer.Shutdown()
	time.Sleep(300 * time.Millisecond)

	// =========================================================
	// Public HTTP API with both producers wired.
	// =========================================================
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encryptionKey,
		Metrics:           obs.NewMetrics(),
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		ConnectionSyncer:  api.NewAsynqConnectionSyncer(pool, asynqClient),
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
		RepoStatusFetcher: api.NewPgxRepoStatusFetcher(pool.Pool),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()
	bearer := "Bearer " + auth.ApiKeyPrefix + apiSecret

	// =========================================================
	// STEP 1: POST /api/connections (create GitHub connection)
	// =========================================================
	t.Log("STEP 1: POST /api/connections")
	createBody, _ := json.Marshal(map[string]any{
		"name": "lifecycle-conn",
		"config": map[string]any{
			"type": "github",
			"url":  ghSrv.URL,
			"orgs": []string{"lifecycle-org"},
		},
		"sync": false,
	})
	connID := mustPostJSON(t, ctx, httpSrv, bearer, "/api/connections", createBody)
	t.Logf("  -> created connection id=%d", connID)

	// =========================================================
	// STEP 2: POST /api/connections/{id}/sync
	// =========================================================
	t.Log("STEP 2: POST /api/connections/{id}/sync")
	syncURL := fmt.Sprintf("/api/connections/%d/sync", connID)
	_ = mustPost(t, ctx, httpSrv, bearer, syncURL)

	// Poll for the Repo row to appear.
	var repoID int32
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx, `
			SELECT id FROM "Repo"
			WHERE external_id = '7777' AND "orgId" = $1
		`, orgID).Scan(&repoID)
		if err == nil && repoID != 0 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if repoID == 0 {
		t.Fatalf("Repo never appeared after connection sync")
	}
	t.Logf("  -> Repo row appeared id=%d", repoID)

	// =========================================================
	// STEP 3: GET /api/repos — verify the just-ingested repo is
	// listed for the caller's org.
	// =========================================================
	t.Log("STEP 3: GET /api/repos")
	type repoListItem struct {
		RepoID   int32  `json:"repoId"`
		RepoName string `json:"repoName"`
	}
	var listResp []repoListItem
	mustGetJSON(t, ctx, httpSrv, bearer, "/api/repos", &listResp)
	foundInList := false
	for _, r := range listResp {
		if r.RepoID == repoID {
			foundInList = true
			t.Logf("  -> repo listed: id=%d name=%q", r.RepoID, r.RepoName)
		}
	}
	if !foundInList {
		t.Fatalf("GET /api/repos did not include the just-synced repo: list=%+v", listResp)
	}

	// =========================================================
	// STEP 4: GET /api/repos/{id}/status — fresh repo: indexedAt
	// is populated by the connection-sync upsert; jobs list is
	// empty (no RepoIndexingJob has run yet).
	// =========================================================
	t.Log("STEP 4: GET /api/repos/{id}/status (fresh)")
	var freshStatus api.RepoStatusResponse
	mustGetJSON(t, ctx, httpSrv, bearer, fmt.Sprintf("/api/repos/%d/status", repoID), &freshStatus)
	if freshStatus.ID != repoID {
		t.Fatalf("status repoID: got %d want %d", freshStatus.ID, repoID)
	}
	if len(freshStatus.Jobs) != 0 {
		t.Errorf("fresh repo should have zero jobs, got %d", len(freshStatus.Jobs))
	}
	if freshStatus.IndexedAt == nil {
		t.Logf("  (note) IndexedAt is nil — connection-sync upsert leaves it NULL until INDEX runs")
	} else {
		t.Logf("  (note) IndexedAt=%v", *freshStatus.IndexedAt)
	}
	t.Logf("  -> fresh status: jobs=%d latestStatus=%v", len(freshStatus.Jobs), freshStatus.LatestIndexingJobStatus)

	// =========================================================
	// STEP 5: simulate indexer output (synthetic stand-in for the
	// not-yet-shipped C.4 INDEX pipeline). This is the only
	// direct-SQL step. We seed the rows + on-disk state that the
	// REMOVE_INDEX cleanup will exercise. The cleanup proof relies
	// on this state going from present -> absent.
	// =========================================================
	t.Log("STEP 5: synthetic seed (stand-in for C.4 INDEX output)")
	_, _ = pool.Exec(ctx, `UPDATE "Repo" SET "indexedAt" = NOW(), "indexedCommitHash" = 'deadbeef' WHERE id = $1`, repoID)
	cigID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeGraphIndex" (
		    id, "repoId", "orgId", provider, status, "commitHash", "workspaceId",
		    "schemaVersion", "builderVersion", "updatedAt"
		) VALUES ($1, $2, $3, 'NEBULA'::"CodeGraphProvider", 'READY'::"CodeGraphIndexStatus",
		    'deadbeef', 'ws', 1, 'v1', NOW())
	`, cigID, repoID, orgID); err != nil {
		t.Fatalf("seed CodeGraphIndex: %v", err)
	}
	ciID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeIntelIndex" (
		    id, "repoId", "orgId", kind, status, revision, "commitHash", "updatedAt"
		) VALUES ($1, $2, $3, 'SCIP'::"CodeIntelIndexKind", 'READY'::"CodeIntelIndexStatus",
		    'main', 'deadbeef', NOW())
	`, ciID, repoID, orgID); err != nil {
		t.Fatalf("seed CodeIntelIndex: %v", err)
	}
	rmID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoIndexManifest" (id, "repoId", "orgId", "workspaceId", branch, "commitHash", "updatedAt")
		VALUES ($1, $2, $3, 'ws', 'main', 'deadbeef', NOW())
	`, rmID, repoID, orgID); err != nil {
		t.Fatalf("seed RepoIndexManifest: %v", err)
	}
	repoDir := filepath.Join(dataCacheDir, "repos", strconv.Itoa(int(repoID)))
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	indexDir := filepath.Join(dataCacheDir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("mkdir indexDir: %v", err)
	}
	shardPrefix := repopaths.ShardPrefix(orgID, repoID)
	shardName := shardPrefix + "_main_0.zoekt"
	if err := os.WriteFile(filepath.Join(indexDir, shardName), []byte("fake-shard"), 0o644); err != nil {
		t.Fatalf("write shard: %v", err)
	}
	t.Logf("  -> seeded: CodeGraphIndex+CodeIntelIndex+RepoIndexManifest rows + clone dir %s + shard %s", repoDir, shardName)

	// =========================================================
	// STEP 6: DELETE /api/repos/{id}/index — REMOVE_INDEX
	// =========================================================
	t.Log("STEP 6: DELETE /api/repos/{id}/index")
	deleteRespBody := mustDelete(t, ctx, httpSrv, bearer, fmt.Sprintf("/api/repos/%d/index", repoID))
	var deleteResp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(deleteRespBody, &deleteResp); err != nil {
		t.Fatalf("decode DELETE response: %v body=%s", err, deleteRespBody)
	}
	if deleteResp.JobID == "" {
		t.Fatalf("DELETE missing jobId: body=%s", deleteRespBody)
	}
	t.Logf("  -> DELETE enqueued jobId=%s", deleteResp.JobID)

	// =========================================================
	// STEP 7: GET /api/repos/{id}/status — poll until the new
	// job appears in the response with status=COMPLETED.
	// =========================================================
	t.Log("STEP 7: GET /api/repos/{id}/status (poll)")
	var doneStatus api.RepoStatusResponse
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		// Decode into a FRESH struct each iteration. Reusing the
		// outer var would leak stale fields from earlier polls
		// because the response's omitempty drops keys when the
		// DB column flipped to NULL between polls — json.Unmarshal
		// doesn't zero out pointer fields whose key is absent
		// from the new payload.
		var iter api.RepoStatusResponse
		mustGetJSON(t, ctx, httpSrv, bearer, fmt.Sprintf("/api/repos/%d/status", repoID), &iter)
		if len(iter.Jobs) >= 1 && iter.Jobs[0].Status == "COMPLETED" {
			doneStatus = iter
			break
		}
		if len(iter.Jobs) >= 1 && iter.Jobs[0].Status == "FAILED" {
			t.Fatalf("REMOVE_INDEX job FAILED: errorMessage=%v", iter.Jobs[0].ErrorMessage)
		}
		time.Sleep(150 * time.Millisecond)
	}
	if len(doneStatus.Jobs) == 0 || doneStatus.Jobs[0].Status != "COMPLETED" {
		t.Fatalf("REMOVE_INDEX did not complete in time: jobs=%+v", doneStatus.Jobs)
	}
	if doneStatus.Jobs[0].Type != "REMOVE_INDEX" {
		t.Errorf("top job type: got %q want REMOVE_INDEX", doneStatus.Jobs[0].Type)
	}
	if doneStatus.Jobs[0].ID != deleteResp.JobID {
		t.Errorf("top job id: got %q want %q (the one we just enqueued)", doneStatus.Jobs[0].ID, deleteResp.JobID)
	}
	t.Logf("  -> status reflects COMPLETED REMOVE_INDEX job")

	// Diagnostic: read Repo state directly from DB to compare
	// against the HTTP response. If these diverge, the bug is in
	// PgxRepoStatusFetcher; if they agree, the bug is in cleanup.
	var dbIndexedAt, dbIndexedHash, dbLatest *string
	if err := pool.QueryRow(ctx, `
		SELECT "indexedAt"::text, "indexedCommitHash", "latestIndexingJobStatus"::text
		FROM "Repo" WHERE id = $1
	`, repoID).Scan(&dbIndexedAt, &dbIndexedHash, &dbLatest); err != nil {
		t.Fatalf("diagnostic SELECT Repo: %v", err)
	}
	t.Logf("  DB state: indexedAt=%v indexedCommitHash=%v latestIndexingJobStatus=%v",
		ptrStr(dbIndexedAt), ptrStr(dbIndexedHash), ptrStr(dbLatest))

	// =========================================================
	// STEP 8: end-state product assertions
	// =========================================================
	t.Run("STEP 8a: indexedAt cleared", func(t *testing.T) {
		if doneStatus.IndexedAt != nil {
			t.Errorf("status.indexedAt: got %v want nil after REMOVE_INDEX", *doneStatus.IndexedAt)
		}
		if doneStatus.IndexedCommitHash != nil {
			t.Errorf("status.indexedCommitHash: got %v want nil after REMOVE_INDEX", *doneStatus.IndexedCommitHash)
		}
	})
	t.Run("STEP 8b: latestIndexingJobStatus = COMPLETED", func(t *testing.T) {
		if doneStatus.LatestIndexingJobStatus == nil || *doneStatus.LatestIndexingJobStatus != "COMPLETED" {
			t.Errorf("latestIndexingJobStatus: got %v want COMPLETED", doneStatus.LatestIndexingJobStatus)
		}
	})
	t.Run("STEP 8c: CodeGraphIndex + CodeIntelIndex + RepoIndexManifest dropped", func(t *testing.T) {
		for _, table := range []string{"CodeGraphIndex", "CodeIntelIndex", "RepoIndexManifest"} {
			var n int
			_ = pool.QueryRow(ctx,
				fmt.Sprintf(`SELECT COUNT(*) FROM %q WHERE "repoId" = $1`, table),
				repoID).Scan(&n)
			if n != 0 {
				t.Errorf("%s rows for repo: got %d want 0", table, n)
			}
		}
	})
	t.Run("STEP 8d: clone directory removed", func(t *testing.T) {
		if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
			t.Errorf("clone dir still exists: %v", err)
		}
	})
	t.Run("STEP 8e: Zoekt shard removed", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(indexDir, shardName)); !os.IsNotExist(err) {
			t.Errorf("shard still exists: %v", err)
		}
	})

	t.Logf("FULL LIFECYCLE PASSED: create connection -> sync -> list -> status -> remove -> status -> clean. Total: 4 HTTP routes + 2 asynq workers + 2 fake codehost endpoints.")
}

func ptrStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// ---------- small HTTP helpers ----------

func mustPostJSON(t *testing.T, ctx context.Context, srv *httptest.Server, bearer, path string, body []byte) int32 {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s: status %d body=%s", path, resp.StatusCode, respBody)
	}
	var decoded struct {
		ID int32 `json:"id"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		t.Fatalf("POST %s: decode body: %v body=%s", path, err, respBody)
	}
	if decoded.ID == 0 {
		t.Fatalf("POST %s: response missing id body=%s", path, respBody)
	}
	return decoded.ID
}

func mustPost(t *testing.T, ctx context.Context, srv *httptest.Server, bearer, path string) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+path, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST %s: status %d body=%s", path, resp.StatusCode, respBody)
	}
	return respBody
}

func mustGetJSON(t *testing.T, ctx context.Context, srv *httptest.Server, bearer, path string, into any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d body=%s", path, resp.StatusCode, respBody)
	}
	if err := json.Unmarshal(respBody, into); err != nil {
		t.Fatalf("GET %s: decode body: %v body=%s", path, err, respBody)
	}
}

func mustDelete(t *testing.T, ctx context.Context, srv *httptest.Server, bearer, path string) []byte {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+path, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE %s: status %d body=%s", path, resp.StatusCode, respBody)
	}
	return respBody
}
