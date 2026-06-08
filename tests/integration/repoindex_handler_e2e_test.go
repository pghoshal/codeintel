//go:build integration

// Phase C.2 live E2E: schedule a REMOVE_INDEX job through the
// asynq queue, let the live worker handler process it, assert
// the DB cascade dropped every per-repo index row.
//
// Same shape as the connection-sync round-trip test (B.3a):
// real Postgres + real Redis + a separate asynq.Server running
// the worker.
package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"codeintel/internal/backend/repoindexmanager"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"
	"codeintel/pkg/repoindex"
	"codeintel/pkg/repopaths"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

func TestRepoIndexHandler_RemoveIndex_FullRoundTrip(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := redisURLFromEnv(t)
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

	// Fixtures: Org + Repo + a few "indexed" rows the cleanup
	// must drop.
	orgName := "c2-removeix-" + uuid.NewString()[:8]
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
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
		"removerepo-"+uuid.NewString()[:6],
		"https://example/remove.git",
		"ext-"+uuid.NewString()[:6],
		"github",
		"https://github.com",
		string(emptyMD),
		orgID,
	).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}

	// Seed CodeIntelIndex + CodeGraphIndex + RepoIndexManifest
	// rows so the cleanup has something to delete + the test
	// can verify the cascade actually fired.
	cigID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeGraphIndex" (
		    id, "repoId", "orgId", provider, status, "commitHash", "workspaceId",
		    "schemaVersion", "builderVersion", "updatedAt"
		) VALUES (
		    $1, $2, $3,
		    'NEBULA'::"CodeGraphProvider",
		    'READY'::"CodeGraphIndexStatus",
		    'abc123', 'ws', 1, 'v1', NOW()
		)
	`, cigID, repoID, orgID); err != nil {
		t.Fatalf("seed CodeGraphIndex: %v", err)
	}
	ciID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeIntelIndex" (
		    id, "repoId", "orgId", kind, status, "workspaceId", branch, revision, "commitHash", "updatedAt"
		) VALUES (
		    $1, $2, $3,
		    'SCIP'::"CodeIntelIndexKind",
		    'READY'::"CodeIntelIndexStatus",
		    'ws', 'main', 'main', 'abc123', NOW()
		)
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

	// Insert the RepoIndexingJob row in PENDING.
	jobID := uuid.NewString()
	store := repoindexmanager.NewStore(pool)
	if err := store.InsertPending(ctx, jobID, repoID, string(repoindex.JobTypeRemoveIndex)); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}

	// C.3 filesystem setup: seed a tmpdir-backed clone dir and a
	// matching set of Zoekt shard files. The handler should
	// rm -rf the clone dir and remove every shard matching the
	// orgId_repoId prefix.
	dataCacheDir := t.TempDir()
	repoFlatPath := filepath.Join(dataCacheDir, "repos", strconv.Itoa(int(repoID)))
	if err := os.MkdirAll(repoFlatPath, 0o755); err != nil {
		t.Fatalf("mkdir repoFlatPath: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoFlatPath, "README.md"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write README in clone dir: %v", err)
	}
	indexDir := filepath.Join(dataCacheDir, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("mkdir indexDir: %v", err)
	}
	prefix := repopaths.ShardPrefix(orgID, repoID) // e.g. "12_48"
	// Two shards belonging to this repo + one shard from a
	// different repo to prove the prefix filter is precise.
	if err := os.WriteFile(filepath.Join(indexDir, prefix+"_main_0.zoekt"), []byte("shard1"), 0o644); err != nil {
		t.Fatalf("write shard1: %v", err)
	}
	if err := os.WriteFile(filepath.Join(indexDir, prefix+"_main_1.zoekt"), []byte("shard2"), 0o644); err != nil {
		t.Fatalf("write shard2: %v", err)
	}
	foreignShardName := strconv.Itoa(int(orgID)) + "_999999_main_0.zoekt"
	if err := os.WriteFile(filepath.Join(indexDir, foreignShardName), []byte("nope"), 0o644); err != nil {
		t.Fatalf("write foreign shard: %v", err)
	}

	// Wire the asynq server with the worker handler.
	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueRepoIndex)

	client := asynq.NewClient(opt)
	defer client.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	pathsCfg := repopaths.Config{DataCacheDir: dataCacheDir}
	handler := repoindexmanager.NewHandler(store, pathsCfg, silent)
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueRepoIndex, handler.AsynqHandlerFunc())
	server := asynq.NewServer(opt, asynq.Config{
		Concurrency:     1,
		Queues:          asynqueues.DefaultPriorities(),
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 5 * time.Second,
	})
	go func() { _ = server.Run(mux) }()
	defer server.Shutdown()
	time.Sleep(200 * time.Millisecond)

	// Enqueue the REMOVE_INDEX task.
	payload, err := repoindex.Marshal(repoindex.TaskPayload{
		Type:     repoindex.JobTypeRemoveIndex,
		JobID:    jobID,
		RepoID:   repoID,
		OrgID:    orgID,
		RepoName: "removerepo",
	})
	if err != nil {
		t.Fatalf("payload marshal: %v", err)
	}
	task := asynq.NewTask(asynqueues.QueueRepoIndex, payload)
	if _, err := client.EnqueueContext(ctx, task, asynq.Queue(asynqueues.QueueRepoIndex)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Poll the job to COMPLETED.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		_ = pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexingJob" WHERE id = $1`, jobID).Scan(&status)
		if status == "COMPLETED" {
			break
		}
		if status == "FAILED" {
			var errMsg string
			_ = pool.QueryRow(ctx, `SELECT COALESCE("errorMessage", '') FROM "RepoIndexingJob" WHERE id = $1`, jobID).Scan(&errMsg)
			t.Fatalf("job FAILED: %s", errMsg)
		}
		time.Sleep(100 * time.Millisecond)
	}
	var finalStatus string
	_ = pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexingJob" WHERE id = $1`, jobID).Scan(&finalStatus)
	if finalStatus != "COMPLETED" {
		t.Fatalf("final status: got %q want COMPLETED", finalStatus)
	}

	t.Run("CodeGraphIndex rows for repo dropped", func(t *testing.T) {
		var n int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphIndex" WHERE "repoId" = $1`, repoID).Scan(&n)
		if n != 0 {
			t.Errorf("CodeGraphIndex: got %d rows want 0", n)
		}
	})
	t.Run("CodeIntelIndex rows for repo dropped", func(t *testing.T) {
		var n int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeIntelIndex" WHERE "repoId" = $1`, repoID).Scan(&n)
		if n != 0 {
			t.Errorf("CodeIntelIndex: got %d rows want 0", n)
		}
	})
	t.Run("RepoIndexManifest rows for repo dropped", func(t *testing.T) {
		var n int
		_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "RepoIndexManifest" WHERE "repoId" = $1`, repoID).Scan(&n)
		if n != 0 {
			t.Errorf("RepoIndexManifest: got %d rows want 0", n)
		}
	})
	t.Run("clone directory removed", func(t *testing.T) {
		if _, err := os.Stat(repoFlatPath); !os.IsNotExist(err) {
			t.Errorf("clone dir still exists: stat err = %v", err)
		}
	})
	t.Run("matching shard files removed", func(t *testing.T) {
		for _, name := range []string{prefix + "_main_0.zoekt", prefix + "_main_1.zoekt"} {
			if _, err := os.Stat(filepath.Join(indexDir, name)); !os.IsNotExist(err) {
				t.Errorf("shard %s still exists: stat err = %v", name, err)
			}
		}
	})
	t.Run("non-matching shard preserved", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(indexDir, foreignShardName)); err != nil {
			t.Errorf("foreign shard %s incorrectly removed: %v", foreignShardName, err)
		}
	})
	t.Run("Repo indexed-state cleared", func(t *testing.T) {
		var indexedAt, indexedHash, latestStatus *string
		_ = pool.QueryRow(ctx, `
			SELECT "indexedAt"::text, "indexedCommitHash", "latestIndexingJobStatus"::text
			FROM "Repo" WHERE id = $1
		`, repoID).Scan(&indexedAt, &indexedHash, &latestStatus)
		if indexedAt != nil {
			t.Errorf("Repo.indexedAt: got %v want nil", *indexedAt)
		}
		if indexedHash != nil {
			t.Errorf("Repo.indexedCommitHash: got %v want nil", *indexedHash)
		}
		// latestIndexingJobStatus is refreshed from the most-recent
		// RepoIndexingJob row, which is COMPLETED. (CleanupRepoDBState
		// cleared it, then RefreshRepoLatestIndexingJobStatus
		// repopulated it.)
		if latestStatus == nil || *latestStatus != "COMPLETED" {
			t.Errorf("Repo.latestIndexingJobStatus: got %v want COMPLETED", latestStatus)
		}
	})

	t.Logf("REMOVE_INDEX full round-trip: enqueue -> worker -> cleanup -> COMPLETED in <15s; "+
		"repoId=%d cleaned cleanly", repoID)
}

// redisURLFromEnv mirrors the helper used by other live tests.
func redisURLFromEnv(t *testing.T) string {
	t.Helper()
	url := os.Getenv("CODEINTEL_REDIS_URL")
	if url == "" {
		t.Skip("CODEINTEL_REDIS_URL unset; skipping repo-index live E2E")
	}
	return url
}

// strconv pin so the import survives go vet's unused check.
var _ = strconv.Itoa
