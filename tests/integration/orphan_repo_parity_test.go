//go:build integration

// Phase P.1 / P.7 parity guards: an orphaned Repo (zero
// RepoToConnection rows) must return 404 from the three repo-id
// routes, matching the legacy `connections: { some: {} }` filter.
//
// These tests prove the fix in asynq_repo_indexer.go +
// repos_status.go added in the same commit.
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

// seedOrgWithOrphanRepo creates an Org + OWNER ApiKey + a Repo
// row with NO RepoToConnection. Returns (orgID, repoID,
// bearer, encryptionKey).
func seedOrgWithOrphanRepo(t *testing.T, ctx context.Context, pool *db.Pool, label string) (int32, int32, string, string) {
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
	`, "orphan-"+suffix, "https://example/orphan.git",
		"ext-"+suffix, "github", "https://github.com",
		string(emptyMD), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert orphan Repo: %v", err)
	}
	// IMPORTANT: no RepoToConnection insert. That's the
	// orphaned-row condition under test.

	return orgID, repoID, "Bearer " + auth.ApiKeyPrefix + apiSecret, encryptionKey
}

func TestParity_OrphanRepo_GetStatus_Returns404(t *testing.T) {
	dsn := requireDSN(t)
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

	orgID, repoID, bearer, encKey := seedOrgWithOrphanRepo(t, ctx, pool, "p1-status")

	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encKey,
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoStatusFetcher: api.NewPgxRepoStatusFetcher(pool.Pool),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	url := fmt.Sprintf("%s/api/repos/%d/status", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("orphan repo GET status: got %d body=%s, want 404", resp.StatusCode, bodyBytes)
	}
	t.Logf("orphan Repo (id=%d, zero RepoToConnection rows) returns 404 — matches legacy", repoID)
}

func TestParity_OrphanRepo_DeleteIndex_Returns404(t *testing.T) {
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

	orgID, repoID, bearer, encKey := seedOrgWithOrphanRepo(t, ctx, pool, "p1-delete")

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

	url := fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("orphan repo DELETE: got %d body=%s, want 404", resp.StatusCode, bodyBytes)
	}

	var jobCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "RepoIndexingJob" WHERE "repoId" = $1`, repoID).Scan(&jobCount)
	if jobCount != 0 {
		t.Errorf("orphan repo DELETE created RepoIndexingJob rows: %d (should be 0)", jobCount)
	}
	t.Logf("orphan Repo (id=%d) DELETE returns 404, no job row leaked", repoID)
}

func TestParity_OrphanRepo_PostIndex_Returns404(t *testing.T) {
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

	orgID, repoID, bearer, encKey := seedOrgWithOrphanRepo(t, ctx, pool, "p1-post")

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

	url := fmt.Sprintf("%s/api/repos/%d/index", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	req.Header.Set("Authorization", bearer)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("orphan repo POST: got %d body=%s, want 404", resp.StatusCode, bodyBytes)
	}

	var jobCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "RepoIndexingJob" WHERE "repoId" = $1`, repoID).Scan(&jobCount)
	if jobCount != 0 {
		t.Errorf("orphan repo POST created RepoIndexingJob rows: %d (should be 0)", jobCount)
	}
	t.Logf("orphan Repo (id=%d) POST returns 404, no job row leaked", repoID)
}
