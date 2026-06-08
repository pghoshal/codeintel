//go:build integration

// Phase B.3a: the full HTTP→queue→worker→DB round-trip E2E.
//
// The existing tests cover the two halves independently:
//   TestAsynqConnectionSyncer_Schedule_LiveProducerPath (B.3)
//     proves Schedule enqueues a task + writes a PENDING row.
//   TestConnectionSync_LiveE2E (B.1d)
//     invokes the worker handler directly and asserts the DB
//     state after.
//
// This test stitches them together end-to-end:
//   1. Stand up a real asynq.Server with the worker handler.
//   2. Call AsynqConnectionSyncer.Schedule — the producer side.
//   3. Poll the ConnectionSyncJob row until status=COMPLETED
//      (or fail on timeout).
//   4. Assert the DB end state: job COMPLETED, repo upserted,
//      M2M binding present.
//
// Proves the FULL wiring: serialised payload, queue pickup by
// a separate goroutine, handler invocation, transactional DB
// writes. This is the test that fails if anything in the
// producer ↔ consumer contract drifts.
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
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/backend/connectionmanager"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// TestConnectionSync_FullRoundTrip is the B.3a binding gate.
// One test, four assertions, one shared Postgres + Redis +
// fake-GitHub setup.
func TestConnectionSync_FullRoundTrip(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping full round-trip", envRedisURL)
	}
	requireLocalRedisOrSkip(t, redisURL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Live Postgres pool + migration.
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	// Fake GitHub server returns one repo per /api/v3/orgs/test-org/repos.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/orgs/test-org/repos" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, `[{
		  "id": 7777,
		  "name": "round-trip",
		  "full_name": "test-org/round-trip",
		  "fork": false, "private": false,
		  "html_url": "https://fake/test-org/round-trip",
		  "clone_url": "https://fake/test-org/round-trip.git",
		  "stargazers_count": 1,
		  "default_branch": "main",
		  "topics": [],
		  "owner": {"login": "test-org", "avatar_url": "https://fake/avatar"}
		}]`)
	}))
	defer gh.Close()

	// Insert Org + Connection. Use a fresh org-slug per run.
	orgName := "b3a-rt-" + uuid.NewString()[:8]
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	cfgJSON, _ := json.Marshal(map[string]any{
		"url":  gh.URL,
		"orgs": []string{"test-org"},
	})
	var connID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id
	`, "b3a-rt-conn", cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}

	// Asynq client + server pointing at the live Redis.
	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	// Clean any prior task for the queue so the test starts
	// from a known state.
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueConnectionSync)

	client := asynq.NewClient(opt)
	defer client.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	asynqLogger := &asynqbridge.SlogLogger{Base: silent}

	// Build the worker handler bound to the live pool, then wire
	// it onto a fresh asynq.Server's ServeMux. Server runs in its
	// own goroutine; we'll stop it via Shutdown at test cleanup.
	handler := connectionmanager.NewHandler(pool.Pool, silent)
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueConnectionSync, handler.AsynqHandlerFunc())

	server := asynq.NewServer(opt, asynq.Config{
		Concurrency:     1,
		Queues:          asynqueues.DefaultPriorities(),
		Logger:          asynqLogger,
		ShutdownTimeout: 5 * time.Second,
	})
	go func() {
		_ = server.Run(mux)
	}()
	defer server.Shutdown()
	// Brief settle delay so the Server is processing the queue
	// when Schedule fires. The server boots faster than this in
	// practice; 200ms is a generous margin.
	time.Sleep(200 * time.Millisecond)

	// Schedule via the AsynqConnectionSyncer producer.
	syncer := api.NewAsynqConnectionSyncer(pool, client)
	result, err := syncer.Schedule(ctx, api.SyncRequest{
		OrgID:        orgID,
		ConnectionID: connID,
	})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	// Poll ConnectionSyncJob.status until COMPLETED (or timeout).
	// The worker should process the task within a few hundred ms;
	// 15s timeout leaves headroom for slow CI.
	pollDeadline := time.Now().Add(15 * time.Second)
	var finalStatus string
	for time.Now().Before(pollDeadline) {
		var status string
		err := pool.QueryRow(ctx, `
			SELECT status::text FROM "ConnectionSyncJob" WHERE id = $1
		`, result.JobID).Scan(&status)
		if err != nil {
			t.Fatalf("poll job: %v", err)
		}
		if status == "COMPLETED" || status == "FAILED" {
			finalStatus = status
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if finalStatus != "COMPLETED" {
		t.Fatalf("final job status: got %q, want COMPLETED", finalStatus)
	}
	t.Logf("round-trip: job %s reached COMPLETED", result.JobID)

	// End-state assertions: same shape as the half-tests so
	// regressions are pinned at every layer.
	t.Run("repo-upserted", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "Repo"
			WHERE external_id = '7777' AND "orgId" = $1
		`, orgID).Scan(&count)
		if err != nil {
			t.Fatalf("count repo: %v", err)
		}
		if count != 1 {
			t.Errorf("expected 1 Repo row, got %d", count)
		}
	})
	t.Run("RepoToConnection-binding", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "RepoToConnection" rtc
			JOIN "Repo" r ON r.id = rtc."repoId"
			WHERE r.external_id = '7777' AND rtc."connectionId" = $1
		`, connID).Scan(&count)
		if err != nil {
			t.Fatalf("count binding: %v", err)
		}
		if count != 1 {
			t.Errorf("expected 1 RepoToConnection row, got %d", count)
		}
	})
	t.Run("connection-syncedAt-stamped", func(t *testing.T) {
		var syncedAt *time.Time
		err := pool.QueryRow(ctx, `
			SELECT "syncedAt" FROM "Connection" WHERE id = $1
		`, connID).Scan(&syncedAt)
		if err != nil {
			t.Fatalf("query connection: %v", err)
		}
		if syncedAt == nil {
			t.Errorf("syncedAt nil; sync did not complete on Connection")
		}
	})
}
