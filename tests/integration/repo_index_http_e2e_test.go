//go:build integration

// Phase C.5 live E2E: full HTTP DELETE /api/repos/{id}/index ->
// AsynqRepoIndexer -> Redis -> worker handler ->
// CleanupRepoDBState + CleanupRepoFilesystem -> COMPLETED.
//
// Same shape as connection_sync_http_e2e_test.go (B.5): the test
// goes through the real wire boundary (HTTP), not via direct
// service calls, so it locks the user's "dynamic to configure
// through api" invariant for the repo-index route.
package integration

import (
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
	"codeintel/internal/backend/astartifact"
	backendgraphstore "codeintel/internal/backend/graphstore"
	"codeintel/internal/backend/indexartifacts"
	"codeintel/internal/backend/indexcore"
	"codeintel/internal/backend/indexexecutor"
	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/internal/backend/repoindexmanager"
	"codeintel/internal/backend/scipartifact"
	"codeintel/internal/backend/zoektartifact"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/internal/obs"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"
	"codeintel/pkg/graphschema"
	"codeintel/pkg/repopaths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

func TestRepoIndex_HTTPDeleteFullRoundTrip(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping repo-index HTTP E2E", envRedisURL)
	}
	requireLocalRedisOrSkip(t, redisURL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	// ---------- Auth fixture ----------
	orgName := "c5-http-" + uuid.NewString()[:8]
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt")
		VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	userID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())
	`, userID, orgName+"-owner@test.local", "c5-http-owner"); err != nil {
		t.Fatalf("insert User: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')
	`, userID, orgID); err != nil {
		t.Fatalf("insert UserToOrg: %v", err)
	}
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	if _, err := pool.Exec(ctx, `
		INSERT INTO "ApiKey" (hash, name, "orgId", "createdById")
		VALUES ($1, $2, $3, $4)
	`, apiKeyHash, "c5-http-key", orgID, userID); err != nil {
		t.Fatalf("insert ApiKey: %v", err)
	}

	// ---------- Repo + indexed-state fixtures ----------
	var repoID int32
	emptyMD, _ := json.Marshal(map[string]any{})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "indexedAt", "indexedCommitHash",
		    "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, NOW(), 'abc123', $7, NOW())
		RETURNING id
	`,
		"c5-repo-"+uuid.NewString()[:6],
		"https://example/c5.git",
		"ext-"+uuid.NewString()[:6],
		"github",
		"https://github.com",
		string(emptyMD),
		orgID,
	).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}

	// Attach the Repo to a Connection so it passes the legacy
	// connections.some({}) filter (P.1 parity fix).
	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "c5-conn-"+uuid.NewString()[:6], cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())
	`, connID, repoID); err != nil {
		t.Fatalf("insert RepoToConnection: %v", err)
	}

	cigID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeGraphIndex" (
		    id, "repoId", "orgId", provider, status, "commitHash", "workspaceId",
		    "schemaVersion", "builderVersion", "updatedAt"
		) VALUES ($1, $2, $3, 'NEBULA'::"CodeGraphProvider", 'READY'::"CodeGraphIndexStatus",
		    'abc123', 'ws', 1, 'v1', NOW())
	`, cigID, repoID, orgID); err != nil {
		t.Fatalf("seed CodeGraphIndex: %v", err)
	}
	ciID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeIntelIndex" (
		    id, "repoId", "orgId", kind, status, "workspaceId", branch, revision, "commitHash", "updatedAt"
		) VALUES ($1, $2, $3, 'SCIP'::"CodeIntelIndexKind", 'READY'::"CodeIntelIndexStatus",
		    'ws', 'main', 'main', 'abc123', NOW())
	`, ciID, repoID, orgID); err != nil {
		t.Fatalf("seed CodeIntelIndex: %v", err)
	}
	rmID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoIndexManifest" (id, "repoId", "orgId", "workspaceId", branch, "commitHash", "updatedAt")
		VALUES ($1, $2, $3, 'ws', 'main', 'abc123', NOW())
	`, rmID, repoID, orgID); err != nil {
		t.Fatalf("seed RepoIndexManifest: %v", err)
	}

	// ---------- Filesystem fixture ----------
	dataCacheDir := t.TempDir()
	repoDir := filepath.Join(dataCacheDir, "repos", strconv.Itoa(int(repoID)))
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repoDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("c5"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	indexDir := filepath.Join(dataCacheDir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("mkdir indexDir: %v", err)
	}
	prefix := repopaths.ShardPrefix(orgID, repoID)
	shardName := prefix + "_main_0.zoekt"
	if err := os.WriteFile(filepath.Join(indexDir, shardName), []byte("shard"), 0o644); err != nil {
		t.Fatalf("write shard: %v", err)
	}

	// ---------- Asynq client + worker handler ----------
	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueRepoIndex)

	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	workerStore := repoindexmanager.NewStore(pool)
	workerHandler := repoindexmanager.NewHandler(workerStore, repopaths.Config{DataCacheDir: dataCacheDir}, silent)
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueRepoIndex, workerHandler.AsynqHandlerFunc())
	asynqServer := asynq.NewServer(opt, asynq.Config{
		Concurrency:     1,
		Queues:          asynqueues.DefaultPriorities(),
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 5 * time.Second,
	})
	go func() { _ = asynqServer.Run(mux) }()
	defer asynqServer.Shutdown()
	time.Sleep(200 * time.Millisecond)

	// ---------- API server with the AsynqRepoIndexer wired ----------
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encryptionKey,
		Metrics:           obs.NewMetrics(),
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()
	bearer := "Bearer " + auth.ApiKeyPrefix + apiSecret

	// ---------- Issue DELETE /api/repos/{id}/index ----------
	deleteURL := fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE status: got %d body=%s", resp.StatusCode, bodyBytes)
	}
	var deleteResp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(bodyBytes, &deleteResp); err != nil {
		t.Fatalf("decode DELETE response: %v body=%s", err, bodyBytes)
	}
	if deleteResp.JobID == "" {
		t.Fatalf("DELETE missing jobId: body=%s", bodyBytes)
	}
	t.Logf("DELETE -> jobId=%s", deleteResp.JobID)

	// ---------- Poll job to COMPLETED ----------
	deadline := time.Now().Add(15 * time.Second)
	var finalStatus string
	for time.Now().Before(deadline) {
		var s string
		_ = pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexingJob" WHERE id = $1`, deleteResp.JobID).Scan(&s)
		if s == "COMPLETED" || s == "FAILED" {
			finalStatus = s
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if finalStatus != "COMPLETED" {
		var errMsg string
		_ = pool.QueryRow(ctx, `SELECT COALESCE("errorMessage", '') FROM "RepoIndexingJob" WHERE id = $1`, deleteResp.JobID).Scan(&errMsg)
		t.Fatalf("job final status: got %q (errorMessage=%q) want COMPLETED", finalStatus, errMsg)
	}

	// ---------- Assertions ----------
	t.Run("CodeGraphIndex rows dropped", func(t *testing.T) {
		var n int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphIndex" WHERE "repoId" = $1`, repoID).Scan(&n)
		if n != 0 {
			t.Errorf("CodeGraphIndex rows: got %d want 0", n)
		}
	})
	t.Run("CodeIntelIndex rows dropped", func(t *testing.T) {
		var n int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeIntelIndex" WHERE "repoId" = $1`, repoID).Scan(&n)
		if n != 0 {
			t.Errorf("CodeIntelIndex rows: got %d want 0", n)
		}
	})
	t.Run("clone dir removed", func(t *testing.T) {
		if _, err := os.Stat(repoDir); !os.IsNotExist(err) {
			t.Errorf("clone dir still exists: %v", err)
		}
	})
	t.Run("shard file removed", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(indexDir, shardName)); !os.IsNotExist(err) {
			t.Errorf("shard still exists: %v", err)
		}
	})

	t.Logf("HTTP -> queue -> worker -> DB+FS cleanup full round-trip for repo %d", repoID)
}

func TestRepoIndex_HTTPPostPlansSplitIndexAndActivates(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := redisURLForExecutorE2E()
	requireLoopbackRedisOrFatal(t, redisURL)
	t.Setenv("CODEINTEL_INDEX_PLAN_SCIP_WORKER_CLASSES", "none")

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
	cancelStaleIntegrationDispatchRows(t, ctx, pool)

	orgName := "c5-http-post-" + uuid.NewString()[:8]
	workspaceID := "atom-ws-" + orgName
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "atomWorkspaceId", "updatedAt")
		VALUES ($1, $2, $3, NOW()) RETURNING id
	`, orgName, orgName+".test", workspaceID).Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	userID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())
	`, userID, orgName+"-owner@test.local", "c5-http-post-owner"); err != nil {
		t.Fatalf("insert User: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')
	`, userID, orgID); err != nil {
		t.Fatalf("insert UserToOrg: %v", err)
	}
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	if _, err := pool.Exec(ctx, `
		INSERT INTO "ApiKey" (hash, name, "orgId", "createdById")
		VALUES ($1, $2, $3, $4)
	`, apiKeyHash, "c5-http-post-key", orgID, userID); err != nil {
		t.Fatalf("insert ApiKey: %v", err)
	}

	cloneURL, expectedCommit := buildHTTPPostIndexRemote(t, t.TempDir())
	var repoID int32
	metadata, _ := json.Marshal(map[string]any{"branches": []string{"master"}})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt", "defaultBranch"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW(), 'master')
		RETURNING id
	`, "c5-post-repo-"+uuid.NewString()[:6], cloneURL, "ext-"+uuid.NewString()[:6], "github", "https://github.com", string(metadata), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}
	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "c5-post-conn-"+uuid.NewString()[:6], cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())
	`, connID, repoID); err != nil {
		t.Fatalf("insert RepoToConnection: %v", err)
	}

	dataCacheDir := t.TempDir()
	artifactRoot := t.TempDir()
	listenAddr := freeTCPAddr(t)
	stopExecutor := startRustExecutorForTest(t, ctx, listenAddr, dataCacheDir, artifactRoot)
	defer stopExecutor()

	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	flushRedisDBForTest(t, redisURL)
	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()

	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	subjobStore := indexsubjobs.NewStore(pool.Pool)
	validator, err := indexexecutor.NewFilesystemArtifactValidator(artifactRoot)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactValidator: %v", err)
	}
	graphStore, closeGraph := graphStoreForHTTPIndexTest(ctx, t, silent)
	defer closeGraph()
	astStore, err := astartifact.NewStore(pool.Pool, graphStore, nil, artifactRoot)
	if err != nil {
		t.Fatalf("New AST store: %v", err)
	}
	zoektStore, err := zoektartifact.NewStore(pool.Pool, repopaths.Config{DataCacheDir: dataCacheDir}, artifactRoot)
	if err != nil {
		t.Fatalf("New Zoekt store: %v", err)
	}
	runner, err := indexexecutor.NewGRPCRunner(listenAddr, 30*time.Second)
	if err != nil {
		t.Fatalf("NewGRPCRunner: %v", err)
	}
	defer runner.Close()
	executorHandler, err := indexexecutor.NewHandler(subjobStore, runner, silent, indexexecutor.Config{
		LeaseDuration:     time.Minute,
		HeartbeatInterval: 5 * time.Second,
		LeaseOwner:        "integration-http-post-executor",
		ArtifactValidator: validator,
		ArtifactIngestor: indexartifacts.NewRouter(map[indexsubjobtask.Layer]indexartifacts.Ingestor{
			indexsubjobtask.LayerZoekt:         zoektStore,
			indexsubjobtask.LayerASTTreeSitter: astStore,
		}),
	})
	if err != nil {
		t.Fatalf("New executor handler: %v", err)
	}
	coreHandler, err := indexcore.NewHandler(pool.Pool, subjobStore, silent, indexcore.Config{
		LeaseDuration: time.Minute,
		LeaseOwner:    "integration-http-post-core",
		Graph:         graphStore,
	})
	if err != nil {
		t.Fatalf("New core handler: %v", err)
	}
	repoIndexHandler := repoindexmanager.NewHandler(repoindexmanager.NewStore(pool), repopaths.Config{DataCacheDir: dataCacheDir}, silent)
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueRepoIndex, repoIndexHandler.AsynqHandlerFunc())
	mux.HandleFunc(asynqueues.QueueIndexCore, func(ctx context.Context, task *asynq.Task) error {
		payload, err := indexsubjobtask.Unmarshal(task.Payload())
		if err != nil || payload.Layer == indexsubjobtask.LayerZoekt || payload.Layer == indexsubjobtask.LayerASTTreeSitter {
			return executorHandler.Handle(ctx, task)
		}
		return coreHandler.Handle(ctx, task)
	})
	asynqServer := asynq.NewServer(opt, asynq.Config{
		Concurrency:     2,
		Queues:          map[string]int{asynqueues.QueueRepoIndex: 1, asynqueues.QueueIndexCore: 1},
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 2 * time.Second,
	})
	go func() { _ = asynqServer.Run(mux) }()
	defer func() {
		asynqServer.Stop()
		asynqServer.Shutdown()
	}()
	time.Sleep(200 * time.Millisecond)

	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encryptionKey,
		Metrics:           obs.NewMetrics(),
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID), nil)
	req.Header.Set("Authorization", "Bearer "+auth.ApiKeyPrefix+apiSecret)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST index: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST index status=%d body=%s", resp.StatusCode, bodyBytes)
	}
	var postResp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(bodyBytes, &postResp); err != nil {
		t.Fatalf("decode POST response: %v body=%s", err, bodyBytes)
	}
	if postResp.JobID == "" {
		t.Fatalf("POST index missing jobId: body=%s", bodyBytes)
	}

	dispatcher := indexsubjobs.NewDispatcher(subjobStore, asynqClient)
	waitForIndexJobCompleted(t, ctx, pool, dispatcher, postResp.JobID, orgID, repoID)

	snapshotPath := repopaths.Config{DataCacheDir: dataCacheDir}.RevisionSnapshotPath(orgID, repoID, expectedCommit)
	if _, err := os.Stat(filepath.Join(snapshotPath, "src", "orders.ts")); err != nil {
		t.Fatalf("revision snapshot missing orders.ts: %v", err)
	}
	var manifestStatus, graphStatus, indexedCommit string
	var factRows, edgeRows, succeeded int
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexManifest" WHERE "indexJobId" = $1`, postResp.JobID).Scan(&manifestStatus); err != nil {
		t.Fatalf("query manifest: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "CodeGraphIndex" WHERE "orgId" = $1 AND "repoId" = $2 AND "commitHash" = $3`, orgID, repoID, expectedCommit).Scan(&graphStatus); err != nil {
		t.Fatalf("query graph: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE("indexedCommitHash", '') FROM "Repo" WHERE id = $1`, repoID).Scan(&indexedCommit); err != nil {
		t.Fatalf("query repo indexed commit: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphSemanticFact" WHERE "orgId" = $1 AND "repoId" = $2`, orgID, repoID).Scan(&factRows); err != nil {
		t.Fatalf("query graph facts: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphSemanticEdge" WHERE "orgId" = $1 AND "repoId" = $2`, orgID, repoID).Scan(&edgeRows); err != nil {
		t.Fatalf("query graph edges: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeIntelIndexSubjob" WHERE "repoIndexingJobId" = $1 AND status = 'SUCCEEDED'`, postResp.JobID).Scan(&succeeded); err != nil {
		t.Fatalf("query succeeded subjobs: %v", err)
	}
	if manifestStatus != "READY" || graphStatus != "READY" || indexedCommit != expectedCommit || factRows == 0 || edgeRows == 0 || succeeded != 4 {
		t.Fatalf("POST index activation manifest=%s graph=%s indexedCommit=%s wantCommit=%s facts=%d edges=%d succeeded=%d",
			manifestStatus, graphStatus, indexedCommit, expectedCommit, factRows, edgeRows, succeeded)
	}
}

func TestRepoIndex_HTTPPostRunsGoSCIPWorkerAndActivates(t *testing.T) {
	scipGoPath := "/tmp/codeintel-scip-tools/scip-go"
	if _, err := os.Stat(scipGoPath); err == nil {
		t.Setenv("PATH", filepath.Dir(scipGoPath)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	if _, err := exec.LookPath("scip-go"); err != nil {
		t.Skipf("scip-go not available; install with GOBIN=/tmp/codeintel-scip-tools go install github.com/scip-code/scip-go/cmd/scip-go@latest")
	}

	dsn := requireDSN(t)
	redisURL := redisURLForExecutorE2E()
	requireLoopbackRedisOrFatal(t, redisURL)
	t.Setenv("CODEINTEL_INDEX_PLAN_SCIP_WORKER_CLASSES", "go")

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
	cancelStaleIntegrationDispatchRows(t, ctx, pool)

	orgName := "c5-http-go-scip-" + uuid.NewString()[:8]
	workspaceID := "atom-ws-" + orgName
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "atomWorkspaceId", "updatedAt")
		VALUES ($1, $2, $3, NOW()) RETURNING id
	`, orgName, orgName+".test", workspaceID).Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	userID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())
	`, userID, orgName+"-owner@test.local", "c5-http-go-owner"); err != nil {
		t.Fatalf("insert User: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')
	`, userID, orgID); err != nil {
		t.Fatalf("insert UserToOrg: %v", err)
	}
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	if _, err := pool.Exec(ctx, `
		INSERT INTO "ApiKey" (hash, name, "orgId", "createdById")
		VALUES ($1, $2, $3, $4)
	`, apiKeyHash, "c5-http-go-key", orgID, userID); err != nil {
		t.Fatalf("insert ApiKey: %v", err)
	}

	cloneURL, expectedCommit := buildHTTPPostGoIndexRemote(t, t.TempDir())
	var repoID int32
	metadata, _ := json.Marshal(map[string]any{"branches": []string{"master"}})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt", "defaultBranch"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW(), 'master')
		RETURNING id
	`, "c5-go-scip-repo-"+uuid.NewString()[:6], cloneURL, "ext-"+uuid.NewString()[:6], "github", "https://github.com", string(metadata), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}
	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "c5-go-scip-conn-"+uuid.NewString()[:6], cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())
	`, connID, repoID); err != nil {
		t.Fatalf("insert RepoToConnection: %v", err)
	}

	dataCacheDir := t.TempDir()
	artifactRoot := t.TempDir()
	listenAddr := freeTCPAddr(t)
	stopExecutor := startRustExecutorForTest(t, ctx, listenAddr, dataCacheDir, artifactRoot)
	defer stopExecutor()

	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	flushRedisDBForTest(t, redisURL)
	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()

	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	subjobStore := indexsubjobs.NewStore(pool.Pool)
	validator, err := indexexecutor.NewFilesystemArtifactValidator(artifactRoot)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactValidator: %v", err)
	}
	graphStore, closeGraph := graphStoreForHTTPIndexTest(ctx, t, silent)
	defer closeGraph()
	astStore, err := astartifact.NewStore(pool.Pool, graphStore, nil, artifactRoot)
	if err != nil {
		t.Fatalf("New AST store: %v", err)
	}
	zoektStore, err := zoektartifact.NewStore(pool.Pool, repopaths.Config{DataCacheDir: dataCacheDir}, artifactRoot)
	if err != nil {
		t.Fatalf("New Zoekt store: %v", err)
	}
	scipStore, err := scipartifact.NewStore(pool.Pool, repopaths.Config{DataCacheDir: dataCacheDir}, artifactRoot)
	if err != nil {
		t.Fatalf("New SCIP store: %v", err)
	}
	runner, err := indexexecutor.NewGRPCRunner(listenAddr, 45*time.Second)
	if err != nil {
		t.Fatalf("NewGRPCRunner: %v", err)
	}
	defer runner.Close()
	astExecutorHandler, err := indexexecutor.NewHandler(subjobStore, runner, silent, indexexecutor.Config{
		LeaseDuration:     time.Minute,
		HeartbeatInterval: 5 * time.Second,
		LeaseOwner:        "integration-http-go-ast-executor",
		ArtifactValidator: validator,
		ArtifactIngestor: indexartifacts.NewRouter(map[indexsubjobtask.Layer]indexartifacts.Ingestor{
			indexsubjobtask.LayerZoekt:         zoektStore,
			indexsubjobtask.LayerASTTreeSitter: astStore,
		}),
	})
	if err != nil {
		t.Fatalf("New AST executor handler: %v", err)
	}
	scipExecutorHandler, err := indexexecutor.NewHandler(subjobStore, runner, silent, indexexecutor.Config{
		LeaseDuration:     time.Minute,
		HeartbeatInterval: 5 * time.Second,
		LeaseOwner:        "integration-http-go-scip-executor",
		ArtifactValidator: validator,
		ArtifactIngestor:  scipStore,
	})
	if err != nil {
		t.Fatalf("New SCIP executor handler: %v", err)
	}
	coreHandler, err := indexcore.NewHandler(pool.Pool, subjobStore, silent, indexcore.Config{
		LeaseDuration: time.Minute,
		LeaseOwner:    "integration-http-go-core",
		Graph:         graphStore,
	})
	if err != nil {
		t.Fatalf("New core handler: %v", err)
	}
	repoIndexHandler := repoindexmanager.NewHandler(repoindexmanager.NewStore(pool), repopaths.Config{DataCacheDir: dataCacheDir}, silent)
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueRepoIndex, repoIndexHandler.AsynqHandlerFunc())
	mux.HandleFunc(asynqueues.QueueIndexCore, func(ctx context.Context, task *asynq.Task) error {
		payload, err := indexsubjobtask.Unmarshal(task.Payload())
		if err != nil || payload.Layer == indexsubjobtask.LayerZoekt || payload.Layer == indexsubjobtask.LayerASTTreeSitter {
			return astExecutorHandler.Handle(ctx, task)
		}
		return coreHandler.Handle(ctx, task)
	})
	mux.HandleFunc(asynqueues.QueueIndexSCIPGo, scipExecutorHandler.AsynqHandlerFunc())
	asynqServer := asynq.NewServer(opt, asynq.Config{
		Concurrency: 3,
		Queues: map[string]int{
			asynqueues.QueueRepoIndex:   1,
			asynqueues.QueueIndexCore:   1,
			asynqueues.QueueIndexSCIPGo: 1,
		},
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 2 * time.Second,
	})
	go func() { _ = asynqServer.Run(mux) }()
	defer func() {
		asynqServer.Stop()
		asynqServer.Shutdown()
	}()
	time.Sleep(200 * time.Millisecond)

	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encryptionKey,
		Metrics:           obs.NewMetrics(),
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID), nil)
	req.Header.Set("Authorization", "Bearer "+auth.ApiKeyPrefix+apiSecret)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST index: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		t.Fatalf("POST index status=%d body=%s", resp.StatusCode, bodyBytes)
	}
	var postResp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(bodyBytes, &postResp); err != nil {
		t.Fatalf("decode POST response: %v body=%s", err, bodyBytes)
	}
	if postResp.JobID == "" {
		t.Fatalf("POST index missing jobId: body=%s", bodyBytes)
	}

	dispatcher := indexsubjobs.NewDispatcher(subjobStore, asynqClient)
	waitForIndexJobCompleted(t, ctx, pool, dispatcher, postResp.JobID, orgID, repoID)

	var manifestStatus, graphStatus, scipStatus string
	var symbolRows, occurrenceRows, relationshipRows, scipSemanticEdges, succeeded, skipped int
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexManifest" WHERE "indexJobId" = $1`, postResp.JobID).Scan(&manifestStatus); err != nil {
		t.Fatalf("query manifest: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "CodeGraphIndex" WHERE "orgId" = $1 AND "repoId" = $2 AND "commitHash" = $3`, orgID, repoID, expectedCommit).Scan(&graphStatus); err != nil {
		t.Fatalf("query graph: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "CodeIntelIndex" WHERE "orgId" = $1 AND "repoId" = $2 AND "commitHash" = $3 AND kind = 'SCIP'`, orgID, repoID, expectedCommit).Scan(&scipStatus); err != nil {
		t.Fatalf("query SCIP parent index: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeIntelSymbol" s JOIN "CodeIntelLanguageIndex" li ON li.id = s."codeIntelLanguageIndexId" JOIN "CodeIntelIndex" ci ON ci.id = li."codeIntelIndexId" WHERE ci."orgId" = $1 AND ci."repoId" = $2 AND ci."commitHash" = $3`, orgID, repoID, expectedCommit).Scan(&symbolRows); err != nil {
		t.Fatalf("query SCIP symbols: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeIntelOccurrence" o JOIN "CodeIntelLanguageIndex" li ON li.id = o."codeIntelLanguageIndexId" JOIN "CodeIntelIndex" ci ON ci.id = li."codeIntelIndexId" WHERE ci."orgId" = $1 AND ci."repoId" = $2 AND ci."commitHash" = $3`, orgID, repoID, expectedCommit).Scan(&occurrenceRows); err != nil {
		t.Fatalf("query SCIP occurrences: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeIntelRelationship" r JOIN "CodeIntelLanguageIndex" li ON li.id = r."codeIntelLanguageIndexId" JOIN "CodeIntelIndex" ci ON ci.id = li."codeIntelIndexId" WHERE ci."orgId" = $1 AND ci."repoId" = $2 AND ci."commitHash" = $3`, orgID, repoID, expectedCommit).Scan(&relationshipRows); err != nil {
		t.Fatalf("query SCIP relationships: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphSemanticEdge" WHERE "orgId" = $1 AND "repoId" = $2 AND "commitHash" = $3 AND source = 'scip'`, orgID, repoID, expectedCommit).Scan(&scipSemanticEdges); err != nil {
		t.Fatalf("query SCIP semantic graph edges: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeIntelIndexSubjob" WHERE "repoIndexingJobId" = $1 AND status = 'SUCCEEDED'`, postResp.JobID).Scan(&succeeded); err != nil {
		t.Fatalf("query succeeded subjobs: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeIntelIndexSubjob" WHERE "repoIndexingJobId" = $1 AND status = 'SKIPPED'`, postResp.JobID).Scan(&skipped); err != nil {
		t.Fatalf("query skipped subjobs: %v", err)
	}
	t.Logf("Go SCIP product path job=%s repo=%d commit=%s manifest=%s graph=%s scip=%s symbols=%d occurrences=%d relationships=%d scipSemanticEdges=%d graphWrites=%d succeeded=%d skipped=%d",
		postResp.JobID, repoID, expectedCommit, manifestStatus, graphStatus, scipStatus, symbolRows, occurrenceRows, relationshipRows, scipSemanticEdges, graphStore.calls, succeeded, skipped)
	if manifestStatus != "READY" || graphStatus != "READY" || scipStatus != "READY" ||
		symbolRows == 0 || occurrenceRows == 0 || relationshipRows == 0 || scipSemanticEdges == 0 || graphStore.calls < 2 || succeeded != 5 || skipped != 0 {
		t.Fatalf("Go SCIP product path manifest=%s graph=%s scip=%s symbols=%d occurrences=%d relationships=%d scipSemanticEdges=%d graphWrites=%d succeeded=%d skipped=%d",
			manifestStatus, graphStatus, scipStatus, symbolRows, occurrenceRows, relationshipRows, scipSemanticEdges, graphStore.calls, succeeded, skipped)
	}
}

func TestRepoIndex_HTTPDelete_RepoInDifferentOrg_Returns404(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping", envRedisURL)
	}
	requireLocalRedisOrSkip(t, redisURL)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	// OrgA + an ApiKey under OrgA's owner. Domain uniqueness via
	// the per-test suffix — re-runs against a shared DB would
	// silently collide on "a.test" otherwise.
	suffix := uuid.NewString()[:6]
	var orgA int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt")
		VALUES ($1, $2, NOW()) RETURNING id
	`, "c5-x-a-"+suffix, "c5-a-"+suffix+".test").Scan(&orgA); err != nil {
		t.Fatalf("insert OrgA: %v", err)
	}
	userID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())`,
		userID, "c5-a-"+suffix+"@test.local", "a-owner"); err != nil {
		t.Fatalf("insert User: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')`, userID, orgA); err != nil {
		t.Fatalf("insert UserToOrg: %v", err)
	}
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	if _, err := pool.Exec(ctx, `INSERT INTO "ApiKey" (hash, name, "orgId", "createdById") VALUES ($1, $2, $3, $4)`,
		apiKeyHash, "a-key-"+suffix, orgA, userID); err != nil {
		t.Fatalf("insert ApiKey: %v", err)
	}

	// OrgB owns a Repo. OrgA's owner should NOT be able to remove
	// its index.
	var orgB int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt")
		VALUES ($1, $2, NOW()) RETURNING id
	`, "c5-x-b-"+suffix, "c5-b-"+suffix+".test").Scan(&orgB); err != nil {
		t.Fatalf("insert OrgB: %v", err)
	}
	emptyMD, _ := json.Marshal(map[string]any{})
	var foreignRepoID int32
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`, "foreign-"+uuid.NewString()[:6], "https://example/foreign.git",
		"ext-"+uuid.NewString()[:6], "github", "https://github.com",
		string(emptyMD), orgB).Scan(&foreignRepoID)

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
		SingleTenantOrgID: orgA,
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	deleteURL := fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, foreignRepoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, deleteURL, nil)
	req.Header.Set("Authorization", "Bearer "+auth.ApiKeyPrefix+apiSecret)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE status: got %d (body=%s) want 404 for cross-org repo", resp.StatusCode, bodyBytes)
	}

	// Belt + suspenders: no RepoIndexingJob row was created.
	var jobCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "RepoIndexingJob" WHERE "repoId" = $1`, foreignRepoID).Scan(&jobCount)
	if jobCount != 0 {
		t.Errorf("expected 0 RepoIndexingJob rows for foreign repo, got %d", jobCount)
	}
}

func buildHTTPPostIndexRemote(t *testing.T, root string) (cloneURL, expectedHead string) {
	t.Helper()
	wt := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(wt, "src"), 0o755); err != nil {
		t.Fatalf("mkdir source tree: %v", err)
	}
	repo, err := git.PlainInit(wt, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "src", "orders.ts"), []byte(`
export function createOrder(command: { id: string }) {
  return { id: command.id, status: "created" };
}

export function handleOrderRoute(input: { id: string }) {
  return createOrder(input);
}
`), 0o644); err != nil {
		t.Fatalf("write orders.ts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "audit.py"), []byte(`
def emit_order_created(order_id):
    return {"event": "order.created", "id": order_id}
`), 0o644); err != nil {
		t.Fatalf("write audit.py: %v", err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := tree.Add("."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := tree.Commit("seed mixed-language order flow", &git.CommitOptions{
		Author: &object.Signature{Name: "integration", Email: "integration@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("repo head: %v", err)
	}
	bare := filepath.Join(root, "remote.git")
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: wt}); err != nil {
		t.Fatalf("PlainClone bare: %v", err)
	}
	return "file://" + bare, head.Hash().String()
}

func buildHTTPPostGoIndexRemote(t *testing.T, root string) (cloneURL, expectedHead string) {
	t.Helper()
	wt := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(wt, "orders"), 0o755); err != nil {
		t.Fatalf("mkdir go source tree: %v", err)
	}
	repo, err := git.PlainInit(wt, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "go.mod"), []byte("module example.com/orders\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "orders", "orders.go"), []byte(`package orders

type Command struct {
	ID string
}

type Order struct {
	ID     string
	Status string
}

func CreateOrder(command Command) Order {
	return Order{ID: command.ID, Status: "created"}
}

func HandleOrderRoute(id string) Order {
	return CreateOrder(Command{ID: id})
}
`), 0o644); err != nil {
		t.Fatalf("write orders.go: %v", err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := tree.Add("."); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := tree.Commit("seed go order flow", &git.CommitOptions{
		Author: &object.Signature{Name: "integration", Email: "integration@example.com", When: time.Now()},
	}); err != nil {
		t.Fatalf("git commit: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("repo head: %v", err)
	}
	bare := filepath.Join(root, "remote.git")
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: wt}); err != nil {
		t.Fatalf("PlainClone bare: %v", err)
	}
	return "file://" + bare, head.Hash().String()
}

type countingGraphStore struct {
	inner backendgraphstore.Store
	calls int
}

func graphStoreForHTTPIndexTest(ctx context.Context, t *testing.T, logger *slog.Logger) (*countingGraphStore, func()) {
	t.Helper()
	if os.Getenv("CODEINTEL_NEBULA_ADDR") != "" {
		store, closeFn := backendgraphstore.CreateFromEnv(ctx, logger)
		return &countingGraphStore{inner: store}, func() { _ = closeFn.Close() }
	}
	return &countingGraphStore{inner: &fakeRenderedGraphStore{}}, func() {}
}

func (s *countingGraphStore) WriteRenderedStatements(ctx context.Context, input backendgraphstore.RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error) {
	s.calls++
	return s.inner.WriteRenderedStatements(ctx, input)
}

func (s *countingGraphStore) WriteSnapshot(ctx context.Context, snapshot graphschema.CodeGraphSnapshot) (graphschema.CodeGraphWriteResult, error) {
	return s.inner.WriteSnapshot(ctx, snapshot)
}

func (s *countingGraphStore) MarkSnapshotForDeletion(ctx context.Context, input graphschema.CodeGraphDeleteInput) error {
	return s.inner.MarkSnapshotForDeletion(ctx, input)
}
