//go:build integration

// Phase B.4-iii: multi-VCS per-org E2E.
//
// One Org owns TWO Connections:
//   - github connection pointing at a fake-GitHub server
//     returning 1 repo (id=8888, full_name=test-org/gh-repo).
//   - gitlab connection pointing at a fake-GitLab server
//     returning 1 project (id=9999, path=test-group/gl-repo).
//
// Both Connections are sync'd through the same worker handler
// against the same DB. Asserts both repos land scoped to the
// same orgId, the codeHostType column distinguishes them, and
// the M2M binding correctly points each repo at its respective
// Connection (no cross-binding).
//
// Closes the user's "1 org can have multiple VCS connections"
// claim end-to-end.
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

// TestConnectionSync_MultiVCSPerOrg is the B.4-iii binding gate.
func TestConnectionSync_MultiVCSPerOrg(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping multi-VCS test", envRedisURL)
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

	// Fake GitHub server: GET /api/v3/orgs/test-org/repos → 1 repo.
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/orgs/test-org/repos" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, `[{
		  "id": 8888, "name": "gh-repo",
		  "full_name": "test-org/gh-repo",
		  "fork": false, "private": false,
		  "html_url": "https://gh-fake/test-org/gh-repo",
		  "clone_url": "https://gh-fake/test-org/gh-repo.git",
		  "stargazers_count": 100,
		  "default_branch": "main", "topics": ["go"],
		  "owner": {"login": "test-org", "avatar_url": "https://gh-fake/avatar"}
		}]`)
	}))
	defer ghSrv.Close()

	// Fake GitLab server: GET /api/v4/groups/test-group/projects → 1 project.
	glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/groups/test-group/projects" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, `[{
		  "id": 9999, "name": "gl-repo",
		  "path_with_namespace": "test-group/gl-repo",
		  "http_url_to_repo": "https://gl-fake/test-group/gl-repo.git",
		  "default_branch": "main",
		  "visibility": "public",
		  "archived": false, "topics": ["rust"],
		  "star_count": 50, "forks_count": 10,
		  "avatar_url": ""
		}]`)
	}))
	defer glSrv.Close()

	// Insert ONE Org with TWO Connections.
	orgName := "multivcs-" + uuid.NewString()[:8]
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	// GitHub connection.
	ghCfgJSON, _ := json.Marshal(map[string]any{
		"url":  ghSrv.URL,
		"orgs": []string{"test-org"},
	})
	var ghConnID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id
	`, "gh-conn", ghCfgJSON, "github", orgID).Scan(&ghConnID); err != nil {
		t.Fatalf("insert GH Connection: %v", err)
	}
	// GitLab connection (same Org!).
	glCfgJSON, _ := json.Marshal(map[string]any{
		"url":    glSrv.URL,
		"groups": []string{"test-group"},
	})
	var glConnID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id
	`, "gl-conn", glCfgJSON, "gitlab", orgID).Scan(&glConnID); err != nil {
		t.Fatalf("insert GL Connection: %v", err)
	}

	// Asynq Server with the worker handler.
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
		Concurrency:     2,
		Queues:          asynqueues.DefaultPriorities(),
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 5 * time.Second,
	})
	go func() { _ = server.Run(mux) }()
	defer server.Shutdown()
	time.Sleep(200 * time.Millisecond)

	// Schedule both syncs.
	syncer := api.NewAsynqConnectionSyncer(pool, client)
	ghResult, err := syncer.Schedule(ctx, api.SyncRequest{OrgID: orgID, ConnectionID: ghConnID})
	if err != nil {
		t.Fatalf("Schedule GH: %v", err)
	}
	glResult, err := syncer.Schedule(ctx, api.SyncRequest{OrgID: orgID, ConnectionID: glConnID})
	if err != nil {
		t.Fatalf("Schedule GL: %v", err)
	}

	// Poll both jobs.
	waitForCompletion := func(jobID, label string) {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			var s string
			if err := pool.QueryRow(ctx, `
				SELECT status::text FROM "ConnectionSyncJob" WHERE id = $1
			`, jobID).Scan(&s); err != nil {
				t.Fatalf("poll %s: %v", label, err)
			}
			if s == "COMPLETED" {
				return
			}
			if s == "FAILED" {
				t.Fatalf("%s job FAILED", label)
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s did not complete in 15s", label)
	}
	waitForCompletion(ghResult.JobID, "github")
	waitForCompletion(glResult.JobID, "gitlab")

	// =====================================================================
	// MULTI-VCS PER-ORG ASSERTIONS
	// =====================================================================

	t.Run("both-repos-belong-to-same-org", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "Repo" WHERE "orgId" = $1
		`, orgID).Scan(&count)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 2 {
			t.Errorf("expected 2 Repo rows for org, got %d", count)
		}
	})

	t.Run("github-repo-has-correct-codeHostType", func(t *testing.T) {
		var codeHostType string
		err := pool.QueryRow(ctx, `
			SELECT "external_codeHostType"::text FROM "Repo"
			WHERE external_id = '8888' AND "orgId" = $1
		`, orgID).Scan(&codeHostType)
		if err != nil {
			t.Fatalf("query GH repo: %v", err)
		}
		if codeHostType != "github" {
			t.Errorf("codeHostType: got %q want github", codeHostType)
		}
	})

	t.Run("gitlab-repo-has-correct-codeHostType", func(t *testing.T) {
		var codeHostType string
		err := pool.QueryRow(ctx, `
			SELECT "external_codeHostType"::text FROM "Repo"
			WHERE external_id = '9999' AND "orgId" = $1
		`, orgID).Scan(&codeHostType)
		if err != nil {
			t.Fatalf("query GL repo: %v", err)
		}
		if codeHostType != "gitlab" {
			t.Errorf("codeHostType: got %q want gitlab", codeHostType)
		}
	})

	t.Run("RepoToConnection-bindings-are-per-connection", func(t *testing.T) {
		// GH repo bound to GH connection only.
		var ghBoundToGH int
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "RepoToConnection" rtc
			JOIN "Repo" r ON r.id = rtc."repoId"
			WHERE r.external_id = '8888' AND rtc."connectionId" = $1
		`, ghConnID).Scan(&ghBoundToGH)
		if ghBoundToGH != 1 {
			t.Errorf("GH repo binding to GH connection: got %d want 1", ghBoundToGH)
		}
		// GH repo NOT bound to GL connection.
		var ghBoundToGL int
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "RepoToConnection" rtc
			JOIN "Repo" r ON r.id = rtc."repoId"
			WHERE r.external_id = '8888' AND rtc."connectionId" = $1
		`, glConnID).Scan(&ghBoundToGL)
		if ghBoundToGL != 0 {
			t.Errorf("GH repo cross-bound to GL connection: got %d want 0", ghBoundToGL)
		}
		// GL repo bound to GL connection only.
		var glBoundToGL int
		_ = pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "RepoToConnection" rtc
			JOIN "Repo" r ON r.id = rtc."repoId"
			WHERE r.external_id = '9999' AND rtc."connectionId" = $1
		`, glConnID).Scan(&glBoundToGL)
		if glBoundToGL != 1 {
			t.Errorf("GL repo binding to GL connection: got %d want 1", glBoundToGL)
		}
	})

	t.Run("metadata-preserves-codehost-specific-keys", func(t *testing.T) {
		// GH repo has zoekt.github-stars; GL repo has zoekt.gitlab-stars.
		var ghMeta, glMeta []byte
		err := pool.QueryRow(ctx, `
			SELECT metadata FROM "Repo" WHERE external_id = '8888' AND "orgId" = $1
		`, orgID).Scan(&ghMeta)
		if err != nil {
			t.Fatalf("GH metadata: %v", err)
		}
		err = pool.QueryRow(ctx, `
			SELECT metadata FROM "Repo" WHERE external_id = '9999' AND "orgId" = $1
		`, orgID).Scan(&glMeta)
		if err != nil {
			t.Fatalf("GL metadata: %v", err)
		}
		type wrap struct {
			GitConfig map[string]string `json:"gitConfig"`
		}
		var ghParsed, glParsed wrap
		_ = json.Unmarshal(ghMeta, &ghParsed)
		_ = json.Unmarshal(glMeta, &glParsed)
		if ghParsed.GitConfig["zoekt.github-stars"] != "100" {
			t.Errorf("GH metadata.zoekt.github-stars: got %q want 100",
				ghParsed.GitConfig["zoekt.github-stars"])
		}
		if glParsed.GitConfig["zoekt.gitlab-stars"] != "50" {
			t.Errorf("GL metadata.zoekt.gitlab-stars: got %q want 50",
				glParsed.GitConfig["zoekt.gitlab-stars"])
		}
		// Sanity: the codehost keys are mutually exclusive.
		if _, has := ghParsed.GitConfig["zoekt.gitlab-stars"]; has {
			t.Errorf("GH metadata leaked zoekt.gitlab-stars key")
		}
		if _, has := glParsed.GitConfig["zoekt.github-stars"]; has {
			t.Errorf("GL metadata leaked zoekt.github-stars key")
		}
	})

	t.Logf("multi-VCS: one org owns 2 connections (github + gitlab), both syncs COMPLETED, " +
		"per-codehost metadata preserved, M2M bindings tenant-scoped")
}
