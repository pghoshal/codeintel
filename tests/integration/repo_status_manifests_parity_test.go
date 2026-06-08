//go:build integration

// Phase P.3d-iv parity guards: GET /api/repos/{id}/status
// surfaces indexManifests[] (last 20, ORDER BY activatedAt DESC,
// createdAt DESC) and currentIndexManifests[] (subset where
// status=READY AND supersededAt IS NULL). Mirrors
// packages/web/src/app/api/(server)/repos/[id]/status/route.ts:148-180,225-226.
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

func TestParity_StatusResponse_IndexManifestsBlock(t *testing.T) {
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

	suffix := uuid.NewString()[:8]
	orgName := "p3dm-" + suffix
	var orgID int32
	_ = pool.QueryRow(ctx, `INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id`,
		orgName, orgName+".test").Scan(&orgID)
	userID := uuid.NewString()
	_, _ = pool.Exec(ctx, `INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())`,
		userID, orgName+"-o@test.local", "owner")
	_, _ = pool.Exec(ctx, `INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')`, userID, orgID)
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	_, _ = pool.Exec(ctx, `INSERT INTO "ApiKey" (hash, name, "orgId", "createdById") VALUES ($1, $2, $3, $4)`,
		apiKeyHash, "key-"+suffix, orgID, userID)
	bearer := "Bearer " + auth.ApiKeyPrefix + apiSecret

	emptyMD, _ := json.Marshal(map[string]any{})
	var repoID int32
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`, "p3dm-"+suffix, "https://example/p3dm.git",
		"ext-"+suffix, "github", "https://github.com",
		string(emptyMD), orgID).Scan(&repoID)
	var connID int32
	cfg, _ := json.Marshal(map[string]any{"type": "github"})
	_ = pool.QueryRow(ctx, `INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt") VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id`,
		"p3dm-conn-"+suffix, cfg, "github", orgID).Scan(&connID)
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)

	// Seed four manifests. Two READY (one active, one
	// superseded), one FAILED, one PENDING. Different
	// activatedAt values to verify the ORDER BY.
	type seed struct {
		id           string
		status       string
		branch       string
		commitHash   string
		activatedAt  *time.Time
		supersededAt *time.Time
		failedAt     *time.Time
	}
	now := time.Now().UTC()
	earlier := now.Add(-2 * time.Hour)
	yesterday := now.Add(-24 * time.Hour)
	failedAt := now.Add(-30 * time.Minute)
	manifests := []seed{
		{id: uuid.NewString(), status: "READY", branch: "main", commitHash: "h1", activatedAt: &now, supersededAt: nil},
		{id: uuid.NewString(), status: "READY", branch: "main", commitHash: "h0", activatedAt: &earlier, supersededAt: &now},
		{id: uuid.NewString(), status: "FAILED", branch: "feature", commitHash: "h2", activatedAt: nil, failedAt: &failedAt},
		{id: uuid.NewString(), status: "PENDING", branch: "release", commitHash: "h3", activatedAt: nil},
	}
	for _, m := range manifests {
		_, err := pool.Exec(ctx, `
			INSERT INTO "RepoIndexManifest" (
			    id, "repoId", "orgId",
			    status, "workspaceId", branch, "commitHash",
			    "activatedAt", "supersededAt", "failedAt",
			    "fileCount", "addedFileCount", "changedFileCount",
			    "deletedFileCount", "unchangedFileCount",
			    "updatedAt"
			) VALUES (
			    $1, $2, $3,
			    $4::"RepoIndexManifestStatus", 'ws', $5, $6,
			    $7, $8, $9,
			    100, 5, 10, 3, 82,
			    NOW()
			)
		`, m.id, repoID, orgID, m.status, m.branch, m.commitHash,
			m.activatedAt, m.supersededAt, m.failedAt)
		if err != nil {
			t.Fatalf("seed manifest %s: %v", m.id, err)
		}
		// Spread out the createdAt timestamps slightly so the
		// secondary sort is observable.
		_, _ = pool.Exec(ctx, `UPDATE "RepoIndexManifest" SET "createdAt" = $2 WHERE id = $1`,
			m.id, yesterday)
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
	req.Header.Set("Authorization", bearer)
	resp, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body=%s", resp.StatusCode, bodyBytes)
	}
	var body api.RepoStatusResponse
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	t.Run("indexManifests array has all 4 seeded rows", func(t *testing.T) {
		if len(body.IndexManifests) != 4 {
			t.Fatalf("len: got %d want 4", len(body.IndexManifests))
		}
	})

	t.Run("sorted by activatedAt DESC NULLS LAST, then createdAt DESC", func(t *testing.T) {
		// Expected order:
		//   0: READY now      (activated newest)
		//   1: READY earlier  (activated 2h ago)
		//   2: FAILED         (activated NULL — last among non-NULL)
		//   3: PENDING        (activated NULL)
		// FAILED + PENDING both have activatedAt=NULL; secondary
		// sort by createdAt DESC. We pinned both createdAt to
		// yesterday, so their relative order between themselves
		// is undefined; just assert the first two.
		got := body.IndexManifests
		if got[0].ID != manifests[0].id || got[0].Status != "READY" {
			t.Errorf("[0]: got id=%s status=%s want %s/READY", got[0].ID, got[0].Status, manifests[0].id)
		}
		if got[1].ID != manifests[1].id {
			t.Errorf("[1]: got id=%s want %s", got[1].ID, manifests[1].id)
		}
	})

	t.Run("currentIndexManifests = READY + !supersededAt", func(t *testing.T) {
		if len(body.CurrentIndexManifests) != 1 {
			t.Fatalf("len: got %d want 1", len(body.CurrentIndexManifests))
		}
		got := body.CurrentIndexManifests[0]
		if got.ID != manifests[0].id {
			t.Errorf("id: got %q want %q (the active READY one)", got.ID, manifests[0].id)
		}
		if got.SupersededAt != nil {
			t.Errorf("SupersededAt: got %v want nil", got.SupersededAt)
		}
		if got.Status != "READY" {
			t.Errorf("Status: got %q want READY", got.Status)
		}
	})

	t.Run("file-count columns round-trip", func(t *testing.T) {
		m := body.IndexManifests[0]
		if m.FileCount != 100 || m.AddedFileCount != 5 || m.ChangedFileCount != 10 ||
			m.DeletedFileCount != 3 || m.UnchangedFileCount != 82 {
			t.Errorf("file counts: total=%d add=%d chg=%d del=%d same=%d",
				m.FileCount, m.AddedFileCount, m.ChangedFileCount,
				m.DeletedFileCount, m.UnchangedFileCount)
		}
	})

	t.Run("FAILED row exposes failedAt", func(t *testing.T) {
		var failedRow *api.RepoIndexManifestRow
		for i, m := range body.IndexManifests {
			if m.Status == "FAILED" {
				failedRow = &body.IndexManifests[i]
				break
			}
		}
		if failedRow == nil {
			t.Fatalf("no FAILED row in response")
		}
		if failedRow.FailedAt == nil {
			t.Errorf("FailedAt: got nil")
		}
	})
}

// Empty case — repo with no manifests yields empty arrays.
func TestParity_StatusResponse_IndexManifestsBlock_Empty(t *testing.T) {
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

	suffix := uuid.NewString()[:8]
	orgName := "p3dme-" + suffix
	var orgID int32
	_ = pool.QueryRow(ctx, `INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id`,
		orgName, orgName+".test").Scan(&orgID)
	userID := uuid.NewString()
	_, _ = pool.Exec(ctx, `INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())`,
		userID, orgName+"-o@test.local", "owner")
	_, _ = pool.Exec(ctx, `INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')`, userID, orgID)
	const encryptionKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encryptionKey, apiSecret)
	_, _ = pool.Exec(ctx, `INSERT INTO "ApiKey" (hash, name, "orgId", "createdById") VALUES ($1, $2, $3, $4)`,
		apiKeyHash, "key-"+suffix, orgID, userID)
	bearer := "Bearer " + auth.ApiKeyPrefix + apiSecret

	emptyMD, _ := json.Marshal(map[string]any{})
	var repoID int32
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`, "p3dme-"+suffix, "https://example/p3dme.git",
		"ext-"+suffix, "github", "https://github.com",
		string(emptyMD), orgID).Scan(&repoID)
	var connID int32
	cfg, _ := json.Marshal(map[string]any{"type": "github"})
	_ = pool.QueryRow(ctx, `INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt") VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id`,
		"p3dme-conn-"+suffix, cfg, "github", orgID).Scan(&connID)
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)

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
	req.Header.Set("Authorization", bearer)
	resp, _ := httpSrv.Client().Do(req)
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d body=%s", resp.StatusCode, bodyBytes)
	}
	var body api.RepoStatusResponse
	_ = json.Unmarshal(bodyBytes, &body)
	if body.IndexManifests == nil || len(body.IndexManifests) != 0 {
		t.Errorf("indexManifests: got %+v want []", body.IndexManifests)
	}
	if body.CurrentIndexManifests == nil || len(body.CurrentIndexManifests) != 0 {
		t.Errorf("currentIndexManifests: got %+v want []", body.CurrentIndexManifests)
	}
	// And critically — the JSON keys must be present (as []),
	// not absent. Helps clients that destructure the response.
	raw := string(bodyBytes)
	if !contains(raw, `"indexManifests":[]`) {
		t.Errorf("body missing indexManifests:[]: %s", raw)
	}
	if !contains(raw, `"currentIndexManifests":[]`) {
		t.Errorf("body missing currentIndexManifests:[]: %s", raw)
	}
}
