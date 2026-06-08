//go:build integration

// Phase B.5: HTTP-level dynamic-config E2E.
//
// Exercises the connection CRUD + sync flow at the WIRE
// boundary, not via direct DB inserts. Proves:
//
//   1. POST /api/connections (real HTTP, Bearer auth, JSON body)
//      creates a Connection row.
//   2. GET /api/connections returns it scoped to the org.
//   3. POST /api/connections/{id}/sync enqueues an asynq task.
//   4. The worker drains the task -> Repo row appears.
//
// This is the binding gate for the user's "dynamic to configure
// through api" directive: every state mutation goes through HTTP,
// not direct SQL. Combined with the multi-VCS E2E (B.4-iii),
// it covers both the API-config dimension and the multi-codehost
// dimension of "1 org can have multiple VCS connections,
// dynamically configured".
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
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/backend/connectionmanager"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/internal/obs"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// TestConnectionSync_HTTPDynamicConfigE2E is the B.5 binding gate.
func TestConnectionSync_HTTPDynamicConfigE2E(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := os.Getenv(envRedisURL)
	if redisURL == "" {
		t.Skipf("%s unset; skipping HTTP E2E", envRedisURL)
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

	// Fake GitHub server.
	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/orgs/http-org/repos" {
			w.WriteHeader(404)
			return
		}
		w.WriteHeader(200)
		fmt.Fprintf(w, `[{
		  "id": 5555, "name": "http-repo",
		  "full_name": "http-org/http-repo",
		  "fork": false, "private": false,
		  "html_url": "https://fake/http-org/http-repo",
		  "clone_url": "https://fake/http-org/http-repo.git",
		  "stargazers_count": 0,
		  "default_branch": "main", "topics": ["go"],
		  "owner": {"login": "http-org", "avatar_url": "https://fake/avatar"}
		}]`)
	}))
	defer ghSrv.Close()

	// Insert Org + User + OWNER ApiKey via SQL (test
	// bootstrap; the API key is the trust anchor that lets
	// subsequent HTTP requests work).
	orgName := "http-e2e-" + uuid.NewString()[:8]
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
	`, userID, orgName+"-owner@test.local", "http-e2e-owner"); err != nil {
		t.Fatalf("insert User: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')
	`, userID, orgID); err != nil {
		t.Fatalf("insert UserToOrg: %v", err)
	}
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString() // 36 chars; treated as opaque
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	if _, err := pool.Exec(ctx, `
		INSERT INTO "ApiKey" (hash, name, "orgId", "createdById")
		VALUES ($1, $2, $3, $4)
	`, apiKeyHash, "http-e2e-key", orgID, userID); err != nil {
		t.Fatalf("insert ApiKey: %v", err)
	}

	// Wire the asynq client + worker handler. The worker drains
	// tasks the HTTP-API enqueues.
	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	_, _ = inspector.DeleteAllPendingTasks(asynqueues.QueueConnectionSync)

	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := connectionmanager.NewHandler(pool.Pool, silent)
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueConnectionSync, handler.AsynqHandlerFunc())
	asynqServer := asynq.NewServer(opt, asynq.Config{
		Concurrency:     1,
		Queues:          asynqueues.DefaultPriorities(),
		Logger:          &asynqbridge.SlogLogger{Base: silent},
		ShutdownTimeout: 5 * time.Second,
	})
	go func() { _ = asynqServer.Run(mux) }()
	defer asynqServer.Shutdown()
	time.Sleep(200 * time.Millisecond)

	// Build the HTTP api.Server with the live pool + the
	// AsynqConnectionSyncer wired (so POST sync routes to
	// the queue, not a Noop).
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encryptionKey,
		Metrics:           obs.NewMetrics(),
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		ConnectionSyncer:  api.NewAsynqConnectionSyncer(pool, asynqClient),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	bearer := "Bearer " + auth.ApiKeyPrefix + apiSecret

	// =====================================================================
	// 1. POST /api/connections (HTTP create)
	// =====================================================================
	createBody := map[string]any{
		"name": "http-conn",
		"config": map[string]any{
			"type": "github",
			"url":  ghSrv.URL,
			"orgs": []string{"http-org"},
		},
		"sync": false, // we sync explicitly below to keep the test linear
	}
	postPayload, _ := json.Marshal(createBody)
	req, _ := http.NewRequestWithContext(ctx, "POST", httpSrv.URL+"/api/connections", bytes.NewReader(postPayload))
	req.Header.Set("Authorization", bearer)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/connections: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST status: got %d, body: %s", resp.StatusCode, bodyBytes)
	}
	var createResp struct {
		ID int32 `json:"id"`
	}
	if err := json.Unmarshal(bodyBytes, &createResp); err != nil {
		t.Fatalf("decode POST response: %v\nbody: %s", err, bodyBytes)
	}
	if createResp.ID == 0 {
		t.Fatalf("POST response missing connection id\nbody: %s", bodyBytes)
	}
	connID := createResp.ID
	t.Logf("created connection id=%d via POST /api/connections", connID)

	// =====================================================================
	// 2. GET /api/connections (HTTP list, tenant-scoped)
	// =====================================================================
	req, _ = http.NewRequestWithContext(ctx, "GET", httpSrv.URL+"/api/connections", nil)
	req.Header.Set("Authorization", bearer)
	resp, err = httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/connections: %v", err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status: got %d, body: %s", resp.StatusCode, bodyBytes)
	}
	var listResp []struct {
		ID             int32  `json:"id"`
		Name           string `json:"name"`
		ConnectionType string `json:"connectionType"`
	}
	if err := json.Unmarshal(bodyBytes, &listResp); err != nil {
		t.Fatalf("decode GET response: %v\nbody: %s", err, bodyBytes)
	}
	t.Run("GET-returns-just-created-connection", func(t *testing.T) {
		found := false
		for _, c := range listResp {
			if c.ID == connID && c.Name == "http-conn" && c.ConnectionType == "github" {
				found = true
			}
		}
		if !found {
			t.Errorf("GET did not include the just-created connection: %+v", listResp)
		}
	})

	// =====================================================================
	// 3. POST /api/connections/{id}/sync (HTTP sync trigger)
	// =====================================================================
	syncURL := fmt.Sprintf("%s/api/connections/%d/sync", httpSrv.URL, connID)
	req, _ = http.NewRequestWithContext(ctx, "POST", syncURL, nil)
	req.Header.Set("Authorization", bearer)
	resp, err = httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST sync: %v", err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST sync status: got %d, body: %s", resp.StatusCode, bodyBytes)
	}
	var syncResp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(bodyBytes, &syncResp); err != nil {
		t.Fatalf("decode sync response: %v\nbody: %s", err, bodyBytes)
	}
	if syncResp.JobID == "" {
		t.Fatalf("sync response missing jobId\nbody: %s", bodyBytes)
	}
	t.Logf("sync triggered via POST /api/connections/%d/sync -> jobId=%s", connID, syncResp.JobID)

	// =====================================================================
	// 4. Poll until the worker COMPLETES the job
	// =====================================================================
	deadline := time.Now().Add(15 * time.Second)
	var finalStatus string
	for time.Now().Before(deadline) {
		var s string
		if err := pool.QueryRow(ctx, `
			SELECT status::text FROM "ConnectionSyncJob" WHERE id = $1
		`, syncResp.JobID).Scan(&s); err != nil {
			t.Fatalf("poll job: %v", err)
		}
		if s == "COMPLETED" || s == "FAILED" {
			finalStatus = s
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if finalStatus != "COMPLETED" {
		t.Fatalf("job final status: got %q want COMPLETED", finalStatus)
	}

	// =====================================================================
	// 5. End-state assertions
	// =====================================================================
	t.Run("repo-upserted-via-HTTP-driven-sync", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "Repo"
			WHERE external_id = '5555' AND "orgId" = $1
		`, orgID).Scan(&count)
		if err != nil {
			t.Fatalf("count Repo: %v", err)
		}
		if count != 1 {
			t.Errorf("expected 1 Repo, got %d", count)
		}
	})

	t.Run("RepoToConnection-binding-via-HTTP", func(t *testing.T) {
		var count int
		err := pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM "RepoToConnection" rtc
			JOIN "Repo" r ON r.id = rtc."repoId"
			WHERE r.external_id = '5555' AND rtc."connectionId" = $1
		`, connID).Scan(&count)
		if err != nil {
			t.Fatalf("count binding: %v", err)
		}
		if count != 1 {
			t.Errorf("expected 1 binding, got %d", count)
		}
	})

	t.Logf("HTTP dynamic-config E2E: POST -> GET -> sync -> worker -> Repo " +
		"completed entirely via real HTTP requests against the live server")
}
