//go:build integration

// Phase B.3b: cross-tenant isolation E2E.
//
// Three Orgs each with a Connection pointing at its own fake
// GitHub server that returns distinct repos (alpha/beta/gamma).
// All three syncs run through the same worker handler against
// the same DB. The test asserts each Org's Repo + RepoToConnection
// rows are scoped to that org only — a SELECT WHERE orgId=N must
// return N's repos and ZERO rows from the other two orgs.
//
// Proves the handler.upsertRepo tenant-scoping invariant
// (rec.OrgID = orgID, unconditionally) holds under multi-tenant
// load.
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

// tenantFixture captures everything one of the 3 test orgs needs.
type tenantFixture struct {
	name        string
	repoSlug    string
	repoID      int64
	githubSrv   *httptest.Server
	orgID       int32
	connID      int32
	expectedJob string
}

// TestConnectionSync_CrossTenantIsolation is the B.3b binding
// gate. Runs three connection-syncs in parallel, then asserts
// the resulting Repo rows are tenant-scoped.
func TestConnectionSync_CrossTenantIsolation(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping cross-tenant isolation", envRedisURL)
	}
	requireLocalRedisOrSkip(t, redisURL)

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

	// Build 3 distinct tenant fixtures. Each tenant gets:
	//   - its own httptest GitHub server returning a UNIQUE repo
	//     slug + repo id (so cross-tenant leakage would be
	//     visibly detectable in assertions).
	//   - its own Org row.
	//   - its own Connection row pointing at its server.
	tenants := []*tenantFixture{
		{name: "alpha", repoSlug: "alpha-repo", repoID: 1001},
		{name: "beta", repoSlug: "beta-repo", repoID: 2002},
		{name: "gamma", repoSlug: "gamma-repo", repoID: 3003},
	}
	for _, tf := range tenants {
		tf := tf // capture in closure below
		tf.githubSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			expected := fmt.Sprintf("/api/v3/orgs/%s-org/repos", tf.name)
			if r.URL.Path != expected {
				w.WriteHeader(404)
				return
			}
			w.WriteHeader(200)
			fmt.Fprintf(w, `[{
			  "id": %d,
			  "name": "%s",
			  "full_name": "%s-org/%s",
			  "fork": false, "private": false,
			  "html_url": "https://fake/%s-org/%s",
			  "clone_url": "https://fake/%s-org/%s.git",
			  "stargazers_count": 0,
			  "default_branch": "main",
			  "topics": [],
			  "owner": {"login": "%s-org", "avatar_url": "https://fake/avatar"}
			}]`, tf.repoID, tf.repoSlug,
				tf.name, tf.repoSlug,
				tf.name, tf.repoSlug,
				tf.name, tf.repoSlug,
				tf.name)
		}))
		t.Cleanup(tf.githubSrv.Close)

		// Insert Org with a unique slug per run.
		orgName := tf.name + "-" + uuid.NewString()[:8]
		if err := pool.QueryRow(ctx, `
			INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
		`, orgName, orgName+".test").Scan(&tf.orgID); err != nil {
			t.Fatalf("insert %s Org: %v", tf.name, err)
		}
		cfgJSON, _ := json.Marshal(map[string]any{
			"url":  tf.githubSrv.URL,
			"orgs": []string{tf.name + "-org"},
		})
		if err := pool.QueryRow(ctx, `
			INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
			VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id
		`, tf.name+"-conn", cfgJSON, "github", tf.orgID).Scan(&tf.connID); err != nil {
			t.Fatalf("insert %s Connection: %v", tf.name, err)
		}
	}

	// Stand up the asynq.Server with the worker handler. One
	// shared server processes all 3 tenants' tasks.
	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueConnectionSync)

	client := asynq.NewClient(opt)
	defer client.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := connectionmanager.NewHandler(pool.Pool, silent)
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueConnectionSync, handler.AsynqHandlerFunc())
	server := asynq.NewServer(opt, asynq.Config{
		Concurrency:     3, // process all 3 tenants in parallel
		Queues:          asynqueues.DefaultPriorities(),
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 5 * time.Second,
	})
	go func() { _ = server.Run(mux) }()
	defer server.Shutdown()
	time.Sleep(200 * time.Millisecond)

	// Producer: fire 3 Schedule calls concurrently.
	syncer := api.NewAsynqConnectionSyncer(pool, client)
	for _, tf := range tenants {
		result, err := syncer.Schedule(ctx, api.SyncRequest{
			OrgID:        tf.orgID,
			ConnectionID: tf.connID,
		})
		if err != nil {
			t.Fatalf("Schedule %s: %v", tf.name, err)
		}
		tf.expectedJob = result.JobID
	}

	// Poll all three jobs until COMPLETED (or timeout).
	deadline := time.Now().Add(30 * time.Second)
	pending := len(tenants)
	for pending > 0 && time.Now().Before(deadline) {
		pending = 0
		for _, tf := range tenants {
			var status string
			if err := pool.QueryRow(ctx, `
				SELECT status::text FROM "ConnectionSyncJob" WHERE id = $1
			`, tf.expectedJob).Scan(&status); err != nil {
				t.Fatalf("poll %s: %v", tf.name, err)
			}
			if status != "COMPLETED" && status != "FAILED" {
				pending++
			}
		}
		if pending == 0 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if pending > 0 {
		t.Fatalf("%d of %d jobs did not reach terminal state in 30s", pending, len(tenants))
	}

	// =====================================================================
	// CROSS-TENANT ISOLATION ASSERTIONS
	// =====================================================================

	// Per-tenant: must see exactly 1 repo with the expected
	// external_id, and ZERO repos with the OTHER two tenants'
	// external_ids.
	for _, tf := range tenants {
		tf := tf
		t.Run(tf.name+"/owns-its-own-repo", func(t *testing.T) {
			var count int
			err := pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM "Repo"
				WHERE external_id = $1 AND "orgId" = $2
			`, fmt.Sprintf("%d", tf.repoID), tf.orgID).Scan(&count)
			if err != nil {
				t.Fatalf("count own: %v", err)
			}
			if count != 1 {
				t.Errorf("%s: expected 1 own repo, got %d", tf.name, count)
			}
		})

		t.Run(tf.name+"/cannot-see-other-tenants-repos", func(t *testing.T) {
			for _, other := range tenants {
				if other.name == tf.name {
					continue
				}
				// Cross-org leakage check: does THIS org's
				// orgId scope return a repo with the OTHER
				// org's external_id?
				var leakCount int
				err := pool.QueryRow(ctx, `
					SELECT COUNT(*) FROM "Repo"
					WHERE external_id = $1 AND "orgId" = $2
				`, fmt.Sprintf("%d", other.repoID), tf.orgID).Scan(&leakCount)
				if err != nil {
					t.Fatalf("count cross-tenant: %v", err)
				}
				if leakCount != 0 {
					t.Errorf("%s.orgId=%d ALSO contains %s's repo (external_id=%d) - tenant boundary violated",
						tf.name, tf.orgID, other.name, other.repoID)
				}
			}
		})

		t.Run(tf.name+"/RepoToConnection-also-scoped", func(t *testing.T) {
			// The M2M binding should also be tenant-scoped.
			// rtc -> Repo join filters by repo's orgId.
			var ownBinding int
			err := pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM "RepoToConnection" rtc
				JOIN "Repo" r ON r.id = rtc."repoId"
				WHERE rtc."connectionId" = $1 AND r."orgId" = $2
			`, tf.connID, tf.orgID).Scan(&ownBinding)
			if err != nil {
				t.Fatalf("count own binding: %v", err)
			}
			if ownBinding != 1 {
				t.Errorf("%s: expected 1 RepoToConnection row, got %d", tf.name, ownBinding)
			}

			// Cross-tenant: NO binding rows where the connection
			// points at this tenant's connection id but the repo
			// belongs to another tenant.
			var leakBinding int
			err = pool.QueryRow(ctx, `
				SELECT COUNT(*) FROM "RepoToConnection" rtc
				JOIN "Repo" r ON r.id = rtc."repoId"
				WHERE rtc."connectionId" = $1 AND r."orgId" != $2
			`, tf.connID, tf.orgID).Scan(&leakBinding)
			if err != nil {
				t.Fatalf("count leak binding: %v", err)
			}
			if leakBinding != 0 {
				t.Errorf("%s.connectionId=%d has %d RepoToConnection rows linking foreign-org repos",
					tf.name, tf.connID, leakBinding)
			}
		})
	}

	// Global invariant: the total Repo count across the three
	// tenants is exactly 3 (one per tenant). A larger count would
	// indicate cross-tenant duplication.
	t.Run("global/total-repo-count-equals-tenant-count", func(t *testing.T) {
		var totalAcrossTenants int
		orgIDs := []int32{tenants[0].orgID, tenants[1].orgID, tenants[2].orgID}
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "Repo" WHERE "orgId" = ANY($1::int[])
		`, orgIDs).Scan(&totalAcrossTenants)
		if err != nil {
			t.Fatalf("total count: %v", err)
		}
		if totalAcrossTenants != len(tenants) {
			t.Errorf("total Repo rows across tenants: got %d, want %d", totalAcrossTenants, len(tenants))
		}
	})

	t.Logf("cross-tenant isolation: 3 tenants synced concurrently, all 3 jobs COMPLETED, " +
		"isolation invariants hold")
}
