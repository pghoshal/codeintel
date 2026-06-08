//go:build integration

// Phase B.1d live end-to-end test for the connection-sync
// worker handler. Wires:
//
//   1. A live codeintel Postgres seeded with an Org +
//      Connection (config pointing at a local httptest GitHub
//      server) + a PENDING ConnectionSyncJob.
//   2. The local httptest server returns a single fake repo.
//   3. The connectionmanager.Handler is invoked directly with a
//      synthesized asynq.Task.
//   4. Asserts the resulting DB state:
//        - ConnectionSyncJob updated to COMPLETED.
//        - Connection.syncedAt stamped.
//        - One Repo row with the expected shape.
//        - RepoToConnection binding present.
//
// This is the first true user-visible product-flow E2E: it
// proves the codeintel-backend can ingest a real GitHub
// response, transform it through the legacy-equivalent pipeline,
// and persist parity-correct rows.
//
// Gated on CODEINTEL_TEST_POSTGRES_URL (set automatically when
// the dev stack is up via make stack-up).
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
	"testing"
	"time"

	"codeintel/internal/backend/connectionmanager"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/connectionsync"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConnectionSync_LiveE2E is the binding Phase B.1d gate.
// Runs the full happy path end-to-end against live Postgres +
// a fake GitHub HTTP server.
func TestConnectionSync_LiveE2E(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Bootstrap the schema.
	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	// Stand up the fake GitHub server. Returns a single repo
	// for the "test-org" enumeration.
	ghServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/orgs/test-org/repos" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, `[{
		  "id": 999,
		  "name": "test-repo",
		  "full_name": "test-org/test-repo",
		  "fork": false,
		  "private": false,
		  "html_url": "https://test-host/test-org/test-repo",
		  "clone_url": "https://test-host/test-org/test-repo.git",
		  "stargazers_count": 7,
		  "watchers_count": 3,
		  "default_branch": "main",
		  "topics": ["ai"],
		  "owner": {"login": "test-org", "avatar_url": "https://test-host/avatar"}
		}]`)
	}))
	defer ghServer.Close()

	// Insert minimal fixture rows: Org → Connection → ConnectionSyncJob.
	// Use a uuid suffix so re-runs don't trip the Org_domain_key
	// unique constraint left over from a prior pass.
	orgName := "e2e-org-" + uuid.NewString()[:8]
	orgID := insertOrg(t, ctx, pool.Pool, orgName)
	jobID := uuid.NewString()
	connID := insertGitHubConnection(t, ctx, pool.Pool, orgID, ghServer.URL)
	insertPendingJob(t, ctx, pool.Pool, jobID, connID)

	// Build the handler + the asynq task.
	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := connectionmanager.NewHandler(pool.Pool, silentLogger)
	payload, err := connectionsync.Marshal(connectionsync.TaskPayload{
		JobID:          jobID,
		ConnectionID:   connID,
		ConnectionName: "e2e-conn",
		OrgID:          orgID,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	task := asynq.NewTask("connection.sync", payload)

	// Invoke the handler directly (no real asynq Server in the
	// loop — that's a separate integration concern. This test
	// targets the handler-business-logic E2E).
	if err := handler.Handle(ctx, task); err != nil {
		t.Fatalf("handler.Handle: %v", err)
	}

	// Assertions on resulting DB state.
	t.Run("job-marked-completed", func(t *testing.T) {
		var status string
		var warnings []string
		err := pool.QueryRow(ctx, `
			SELECT status::text, COALESCE("warningMessages", ARRAY[]::text[])
			FROM "ConnectionSyncJob" WHERE id = $1
		`, jobID).Scan(&status, &warnings)
		if err != nil {
			t.Fatalf("query job: %v", err)
		}
		if status != "COMPLETED" {
			t.Errorf("job status: got %q, want COMPLETED", status)
		}
		if len(warnings) != 0 {
			t.Errorf("expected no warnings, got %v", warnings)
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
			t.Errorf("syncedAt should be stamped after successful sync")
		}
	})

	t.Run("repo-upserted-with-correct-shape", func(t *testing.T) {
		var (
			externalID, codeHostType, codeHostURL, cloneURL, name, displayName string
			isFork, isArchived, isPublic                                       bool
			metadata                                                           []byte
		)
		err := pool.QueryRow(ctx, `
			SELECT external_id, "external_codeHostType"::text, "external_codeHostUrl",
			       "cloneUrl", name, "displayName", "isFork", "isArchived",
			       "isPublic", metadata
			FROM "Repo" WHERE external_id = '999' AND "orgId" = $1
		`, orgID).Scan(&externalID, &codeHostType, &codeHostURL, &cloneURL,
			&name, &displayName, &isFork, &isArchived, &isPublic, &metadata)
		if err != nil {
			t.Fatalf("query repo: %v", err)
		}
		if codeHostType != "github" {
			t.Errorf("codeHostType: got %q", codeHostType)
		}
		if cloneURL != "https://test-host/test-org/test-repo.git" {
			t.Errorf("cloneURL: got %q", cloneURL)
		}
		if name != ghHostName(ghServer.URL)+"/test-org/test-repo" {
			t.Errorf("name: got %q, want includes the host", name)
		}
		if displayName != "test-org/test-repo" {
			t.Errorf("displayName: got %q", displayName)
		}
		if !isPublic || isFork || isArchived {
			t.Errorf("flags: isPublic=%v isFork=%v isArchived=%v", isPublic, isFork, isArchived)
		}
		// metadata: spot-check the embedded gitConfig.
		var md struct {
			GitConfig map[string]string `json:"gitConfig"`
		}
		if err := json.Unmarshal(metadata, &md); err != nil {
			t.Fatalf("metadata unmarshal: %v", err)
		}
		if md.GitConfig["zoekt.github-stars"] != "7" {
			t.Errorf("zoekt.github-stars: got %q", md.GitConfig["zoekt.github-stars"])
		}
	})

	t.Run("RepoToConnection-binding-created", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "RepoToConnection" rtc
			JOIN "Repo" r ON r.id = rtc."repoId"
			WHERE r.external_id = '999' AND rtc."connectionId" = $1
		`, connID).Scan(&count)
		if err != nil {
			t.Fatalf("query binding: %v", err)
		}
		if count != 1 {
			t.Errorf("RepoToConnection count: got %d, want 1", count)
		}
	})
}

// insertOrg creates an Org row with auto-generated id; returns
// the id.
func insertOrg(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) int32 {
	t.Helper()
	var id int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt")
		VALUES ($1, $2, NOW()) RETURNING id
	`, name, name+".test").Scan(&id); err != nil {
		t.Fatalf("insertOrg: %v", err)
	}
	return id
}

// insertGitHubConnection creates a github-type Connection
// pointing at the fake httptest URL.
func insertGitHubConnection(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID int32, ghURL string) int32 {
	t.Helper()
	cfg := map[string]any{
		"url":  ghURL,
		"orgs": []string{"test-org"},
	}
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal cfg: %v", err)
	}
	var id int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "e2e-conn", cfgJSON, "github", orgID).Scan(&id); err != nil {
		t.Fatalf("insertConnection: %v", err)
	}
	return id
}

// insertPendingJob inserts a ConnectionSyncJob with status
// PENDING for the worker to pick up.
func insertPendingJob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, jobID string, connID int32) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "ConnectionSyncJob" (id, "connectionId", status, "updatedAt")
		VALUES ($1, $2, 'PENDING', NOW())
	`, jobID, connID); err != nil {
		t.Fatalf("insertJob: %v", err)
	}
}

// ghHostName strips the scheme from a URL for the test's
// expected name comparison. e.g. "http://127.0.0.1:12345" →
// "127.0.0.1:12345".
func ghHostName(u string) string {
	for _, p := range []string{"https://", "http://"} {
		if len(u) > len(p) && bytes.HasPrefix([]byte(u), []byte(p)) {
			return u[len(p):]
		}
	}
	return u
}
