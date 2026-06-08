//go:build integration

// Phase B.3 live integration test for the asynq-backed
// ConnectionSyncer. Asserts the end-to-end producer path:
//
//   1. Caller invokes Schedule(ctx, SyncRequest).
//   2. A ConnectionSyncJob row is inserted in PENDING status
//      with the supplied connectionId + a uuid jobID.
//   3. An asynq task is enqueued on the connection-sync-queue
//      with the matching payload.
//
// Gated on CODEINTEL_TEST_POSTGRES_URL + CODEINTEL_REDIS_URL.
// When run together with the existing
// TestConnectionSync_LiveE2E (which invokes the worker handler
// directly) the full pipeline is proved: producer enqueues →
// consumer processes → DB state matches expectations.
package integration

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"
	"codeintel/pkg/connectionsync"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

const envRedisURL = "CODEINTEL_REDIS_URL"

// TestAsynqConnectionSyncer_Schedule_LiveProducerPath is the
// Phase B.3 binding gate. Inserts an Org + Connection, calls
// AsynqConnectionSyncer.Schedule, asserts the resulting DB row
// + enqueued asynq task both look correct.
func TestAsynqConnectionSyncer_Schedule_LiveProducerPath(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping live producer-path test", envRedisURL)
	}

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

	// Insert fixtures.
	orgName := "b3-syncer-" + uuid.NewString()[:8]
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	connName := "b3-conn"
	cfgJSON, _ := json.Marshal(map[string]any{"orgs": []string{"some-org"}})
	var connID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id
	`, connName, cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}

	// Build the asynq client + inspector. Inspector lets the test
	// read enqueued tasks directly without spinning up a Server.
	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	requireLocalRedisOrSkip(t, redisURL)

	client := asynq.NewClient(opt)
	defer client.Close()
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()

	// Pre-clean the queue so previously-enqueued tasks don't
	// confuse the assertion. Safe because we already gated on
	// loopback Redis above.
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueConnectionSync)

	// Schedule!
	syncer := api.NewAsynqConnectionSyncer(pool, client)
	result, err := syncer.Schedule(ctx, api.SyncRequest{
		OrgID:        orgID,
		ConnectionID: connID,
	})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if result.JobID == "" {
		t.Fatalf("Schedule returned empty JobID")
	}

	t.Run("ConnectionSyncJob-row-inserted-PENDING", func(t *testing.T) {
		var status string
		var jobConnID int32
		err := pool.QueryRow(ctx, `
			SELECT status::text, "connectionId"
			FROM "ConnectionSyncJob" WHERE id = $1
		`, result.JobID).Scan(&status, &jobConnID)
		if err != nil {
			t.Fatalf("query job: %v", err)
		}
		if status != "PENDING" {
			t.Errorf("status: got %q want PENDING", status)
		}
		if jobConnID != connID {
			t.Errorf("connectionId: got %d want %d", jobConnID, connID)
		}
	})

	t.Run("asynq-task-enqueued-with-matching-payload", func(t *testing.T) {
		// Inspector lists pending tasks on the queue. We expect
		// exactly one (we cleaned above).
		tasks, err := inspector.ListPendingTasks(asynqueues.QueueConnectionSync)
		if err != nil {
			t.Fatalf("ListPendingTasks: %v", err)
		}
		if len(tasks) != 1 {
			t.Fatalf("pending task count: got %d want 1", len(tasks))
		}
		var got connectionsync.TaskPayload
		if err := json.Unmarshal(tasks[0].Payload, &got); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if got.JobID != result.JobID {
			t.Errorf("payload.JobID: got %q want %q", got.JobID, result.JobID)
		}
		if got.ConnectionID != connID {
			t.Errorf("payload.ConnectionID: got %d want %d", got.ConnectionID, connID)
		}
		if got.OrgID != orgID {
			t.Errorf("payload.OrgID: got %d want %d", got.OrgID, orgID)
		}
		if got.ConnectionName != connName {
			t.Errorf("payload.ConnectionName: got %q want %q", got.ConnectionName, connName)
		}
	})

	t.Cleanup(func() {
		// Drop the queue entry so successive runs start fresh.
		_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueConnectionSync)
	})
}

// requireLocalRedisOrSkip is the destructive-action guard
// mirrored from internal/asynqsmoke. The producer-path test
// calls DeleteAllPendingTasks on the connection-sync queue
// for pre-cleanup; refuse to run against a non-loopback Redis
// without explicit opt-in.
func requireLocalRedisOrSkip(t *testing.T, rawURL string) {
	t.Helper()
	for _, ok := range []string{"127.0.0.1", "::1", "localhost"} {
		if strings.Contains(rawURL, "@"+ok) || strings.Contains(rawURL, "/"+ok) || strings.Contains(rawURL, "//"+ok) {
			return
		}
	}
	if strings.EqualFold(os.Getenv("CODEINTEL_ASYNQSMOKE_DESTRUCTIVE"), "true") {
		t.Logf("non-local Redis + destructive opt-in present; proceeding")
		return
	}
	t.Skipf("non-loopback Redis URL %q; refusing destructive pre-cleanup", rawURL)
}
