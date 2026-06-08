//go:build integration

// Phase P.8 parity guards: POST /api/repos/{id}/index and
// DELETE /api/repos/{id}/index must return 409 with the
// REPO_INDEXING_JOB_ALREADY_ACTIVE error code when a
// PENDING/IN_PROGRESS RepoIndexingJob already exists for the
// repo. Direct port of the check in
// packages/backend/src/api.ts:160-182 (indexRepo) and
// :224 (removeRepoIndex).
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/asynqbridge"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// seedRepoWithActiveJob bootstraps Org + Repo + Connection +
// RepoToConnection + one PENDING RepoIndexingJob row. Returns
// orgID, repoID, bearer, encryptionKey, existingJobID.
func seedRepoWithActiveJob(t *testing.T, ctx context.Context, pool *db.Pool, label string, status string) (int32, int32, string, string, string) {
	t.Helper()
	suffix := uuid.NewString()[:8]
	orgName := label + "-" + suffix
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

	var repoID int32
	emptyMD, _ := json.Marshal(map[string]any{})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`, "p8-"+suffix, "https://example/p8.git",
		"ext-"+suffix, "github", "https://github.com",
		string(emptyMD), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}

	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "p8-conn-"+suffix, cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())
	`, connID, repoID); err != nil {
		t.Fatalf("insert RepoToConnection: %v", err)
	}

	// Pre-existing job in the active state under test.
	existingJobID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, "updatedAt")
		VALUES ($1, $2, 'INDEX'::"RepoIndexingJobType", $3::"RepoIndexingJobStatus", NOW())
	`, existingJobID, repoID, status); err != nil {
		t.Fatalf("insert RepoIndexingJob (status=%s): %v", status, err)
	}

	return orgID, repoID, "Bearer " + auth.ApiKeyPrefix + apiSecret, encryptionKey, existingJobID
}

func TestParity_DuplicateIndex_PendingJob_Returns409(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := redisURLFromEnv(t)
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

	orgID, repoID, bearer, encKey, existingJobID :=
		seedRepoWithActiveJob(t, ctx, pool, "p8-pending", "PENDING")

	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("redis opt: %v", err)
	}
	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encKey,
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	postURL := fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, postURL, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d body=%s want 409", resp.StatusCode, bodyBytes)
	}

	var body struct {
		StatusCode int    `json:"statusCode"`
		ErrorCode  string `json:"errorCode"`
		Message    string `json:"message"`
	}
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("decode body: %v body=%s", err, bodyBytes)
	}
	if body.ErrorCode != "REPO_INDEXING_JOB_ALREADY_ACTIVE" {
		t.Errorf("errorCode: got %q want REPO_INDEXING_JOB_ALREADY_ACTIVE", body.ErrorCode)
	}
	wantMsg := fmt.Sprintf("Repo already has active INDEX job %s.", existingJobID)
	if body.Message != wantMsg {
		t.Errorf("message: got %q want %q", body.Message, wantMsg)
	}

	// And critically — no NEW RepoIndexingJob row was created.
	var n int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "RepoIndexingJob" WHERE "repoId" = $1`, repoID).Scan(&n)
	if n != 1 {
		t.Errorf("RepoIndexingJob count: got %d want 1 (the seeded PENDING row)", n)
	}
}

func TestParity_DuplicateIndex_InProgressJob_Returns409(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := redisURLFromEnv(t)
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

	orgID, repoID, bearer, encKey, _ :=
		seedRepoWithActiveJob(t, ctx, pool, "p8-inprog", "IN_PROGRESS")

	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("redis opt: %v", err)
	}
	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encKey,
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	// Both POST and DELETE should 409.
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		req, _ := http.NewRequestWithContext(ctx, method,
			fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID), nil)
		req.Header.Set("Authorization", bearer)
		resp, err := httpSrv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s status: got %d body=%s want 409", method, resp.StatusCode, bodyBytes)
		}
	}

	var n int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "RepoIndexingJob" WHERE "repoId" = $1`, repoID).Scan(&n)
	if n != 1 {
		t.Errorf("RepoIndexingJob count: got %d want 1 (the seeded IN_PROGRESS row)", n)
	}
}

// A COMPLETED job does NOT block a new INDEX — only PENDING /
// IN_PROGRESS do. This proves the status filter is precise.
func TestParity_CompletedJob_DoesNotBlockNewIndex(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := redisURLFromEnv(t)
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

	orgID, repoID, bearer, encKey, _ :=
		seedRepoWithActiveJob(t, ctx, pool, "p8-completed", "COMPLETED")

	opt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("redis opt: %v", err)
	}
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	_, _ = inspector.DeleteAllPendingTasks("repo-index-queue")
	asynqClient := asynq.NewClient(opt)
	defer asynqClient.Close()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encKey,
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoIndexer:       api.NewAsynqRepoIndexer(pool, asynqClient),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	postURL := fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, postURL, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d body=%s want 200 (COMPLETED job should not block)", resp.StatusCode, bodyBytes)
	}

	// Now there should be 2 jobs: the seeded COMPLETED one + the
	// newly-enqueued PENDING one.
	var n int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "RepoIndexingJob" WHERE "repoId" = $1`, repoID).Scan(&n)
	if n != 2 {
		t.Errorf("RepoIndexingJob count: got %d want 2", n)
	}
}
