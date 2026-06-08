//go:build integration

// Phase C.6 live E2E: GET /api/repos/{id}/status against real
// Postgres. Mirrors the C.5 HTTP E2E shape but exercises the
// PgxRepoStatusFetcher SQL paths (Repo single-row + 20-row
// jobs query).
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

	"github.com/google/uuid"
)

func TestRepoStatus_HTTPGet_FullProjection(t *testing.T) {
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

	// Org + OWNER ApiKey + Repo + 3 RepoIndexingJob rows in
	// reverse-creation order so the LIMIT 20 ORDER BY createdAt
	// DESC behaviour is observable.
	orgName := "c6-status-" + uuid.NewString()[:8]
	var orgID int32
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt")
		VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID)
	userID := uuid.NewString()
	_, _ = pool.Exec(ctx, `INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())`,
		userID, orgName+"-owner@test.local", "c6-owner")
	_, _ = pool.Exec(ctx, `INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')`, userID, orgID)
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	_, _ = pool.Exec(ctx, `INSERT INTO "ApiKey" (hash, name, "orgId", "createdById") VALUES ($1, $2, $3, $4)`,
		apiKeyHash, "c6-key", orgID, userID)

	emptyMD, _ := json.Marshal(map[string]any{})
	var repoID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "indexedAt", "indexedCommitHash", "latestIndexingJobStatus",
		    "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, NOW(),
		    'cafef00d', 'COMPLETED'::"RepoIndexingJobStatus", $7, NOW())
		RETURNING id
	`,
		"c6-repo-"+uuid.NewString()[:6],
		"https://example/c6.git",
		"ext-"+uuid.NewString()[:6],
		"github",
		"https://github.com",
		string(emptyMD),
		orgID,
	).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}

	// Attach the Repo to a Connection so it passes the legacy
	// connections.some({}) filter (P.7 parity fix).
	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "c6-conn-"+uuid.NewString()[:6], cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())
	`, connID, repoID); err != nil {
		t.Fatalf("insert RepoToConnection: %v", err)
	}

	// 3 jobs with deliberately staggered createdAt so ORDER BY
	// produces a known sequence (latest first).
	type seedJob struct {
		id     string
		typ    string
		status string
		ago    time.Duration
	}
	seeds := []seedJob{
		{id: uuid.NewString(), typ: "INDEX", status: "COMPLETED", ago: 30 * time.Minute},
		{id: uuid.NewString(), typ: "INDEX", status: "FAILED", ago: 60 * time.Minute},
		{id: uuid.NewString(), typ: "REMOVE_INDEX", status: "COMPLETED", ago: 90 * time.Minute},
	}
	for _, s := range seeds {
		if _, err := pool.Exec(ctx, `
			INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, "createdAt", "updatedAt")
			VALUES ($1, $2, $3::"RepoIndexingJobType", $4::"RepoIndexingJobStatus", NOW() - $5::interval, NOW())
		`, s.id, repoID, s.typ, s.status, fmt.Sprintf("%d seconds", int(s.ago.Seconds()))); err != nil {
			t.Fatalf("seed RepoIndexingJob (%s): %v", s.id, err)
		}
	}

	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encryptionKey,
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoStatusFetcher: api.NewPgxRepoStatusFetcher(pool.Pool),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	url := fmt.Sprintf("%s/api/repos/%d/status", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+auth.ApiKeyPrefix+apiSecret)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d body=%s", resp.StatusCode, bodyBytes)
	}
	var body api.RepoStatusResponse
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("decode body: %v body=%s", err, bodyBytes)
	}

	t.Run("repo fields", func(t *testing.T) {
		if body.ID != repoID {
			t.Errorf("ID: got %d want %d", body.ID, repoID)
		}
		if body.IndexedCommitHash == nil || *body.IndexedCommitHash != "cafef00d" {
			t.Errorf("IndexedCommitHash: got %v", body.IndexedCommitHash)
		}
		if body.LatestIndexingJobStatus == nil || *body.LatestIndexingJobStatus != "COMPLETED" {
			t.Errorf("LatestIndexingJobStatus: got %v", body.LatestIndexingJobStatus)
		}
		if body.IndexedAt == nil {
			t.Errorf("IndexedAt: got nil")
		}
	})
	t.Run("jobs ordered by createdAt DESC", func(t *testing.T) {
		if len(body.Jobs) != 3 {
			t.Fatalf("expected 3 jobs, got %d", len(body.Jobs))
		}
		// Most-recent (30m ago) -> seeds[0] -> first.
		// 60m ago -> seeds[1] -> second.
		// 90m ago -> seeds[2] -> third.
		if body.Jobs[0].ID != seeds[0].id {
			t.Errorf("job[0]: got %q want %q (most recent)", body.Jobs[0].ID, seeds[0].id)
		}
		if body.Jobs[1].ID != seeds[1].id {
			t.Errorf("job[1]: got %q want %q", body.Jobs[1].ID, seeds[1].id)
		}
		if body.Jobs[2].ID != seeds[2].id {
			t.Errorf("job[2]: got %q want %q (oldest)", body.Jobs[2].ID, seeds[2].id)
		}
		if body.Jobs[1].Status != "FAILED" {
			t.Errorf("job[1].Status: got %q want FAILED", body.Jobs[1].Status)
		}
	})
}

func TestRepoStatus_HTTPGet_RepoInDifferentOrg_Returns404(t *testing.T) {
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

	// Set up two orgs; caller's ApiKey is in orgA, repo is in orgB.
	// Domains include the uuid so re-runs don't collide against
	// rows left behind by earlier failing tests.
	suffix := uuid.NewString()[:6]
	var orgA int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt")
		VALUES ($1, $2, NOW()) RETURNING id
	`, "c6-x-a-"+suffix, "c6-a-"+suffix+".test").Scan(&orgA); err != nil {
		t.Fatalf("insert OrgA: %v", err)
	}
	userID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())`,
		userID, "c6-a-"+suffix+"@test.local", "a-owner"); err != nil {
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

	var orgB int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt")
		VALUES ($1, $2, NOW()) RETURNING id
	`, "c6-x-b-"+suffix, "c6-b-"+suffix+".test").Scan(&orgB); err != nil {
		t.Fatalf("insert OrgB: %v", err)
	}
	emptyMD, _ := json.Marshal(map[string]any{})
	var foreignID int32
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`, "foreign-"+uuid.NewString()[:6], "https://example/foreign.git",
		"ext-"+uuid.NewString()[:6], "github", "https://github.com",
		string(emptyMD), orgB).Scan(&foreignID)

	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encryptionKey,
		DBPinger:          pool,
		SingleTenantOrgID: orgA,
		RepoStatusFetcher: api.NewPgxRepoStatusFetcher(pool.Pool),
	})
	httpSrv := httptest.NewServer(apiSrv.Router())
	defer httpSrv.Close()

	url := fmt.Sprintf("%s/api/repos/%d/status", httpSrv.URL, foreignID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+auth.ApiKeyPrefix+apiSecret)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d (body=%s) want 404", resp.StatusCode, bodyBytes)
	}
}
