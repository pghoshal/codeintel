//go:build integration

// Phase P.3a parity guards: GET /api/repos/{id}/status surfaces
// Repo.metadata + RepoIndexingJob.metadata in the response,
// matching the legacy projection's select clauses
// (packages/web/src/app/api/(server)/repos/[id]/status/route.ts:49,63).
//
// Scope of P.3a is the direct-column subset; the helper-derived
// blocks (indexStatus, branchStatuses, codeIntel.scip,
// codeIntel.codeGraph, indexManifests) land in P.3b/P.3c.
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

func TestParity_StatusResponse_RepoAndJobMetadata(t *testing.T) {
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

	// Bootstrap Org + User + OWNER ApiKey.
	suffix := uuid.NewString()[:8]
	orgName := "p3a-" + suffix
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

	// Repo with a non-trivial metadata JSON blob. The legacy
	// projection round-trips this JSONB column directly into the
	// response — operators rely on values like .codeHostStars
	// being visible on /status.
	repoMeta := map[string]any{
		"codeHostStars":  42,
		"customLabel":    "platform-team",
		"nestedSettings": map[string]any{"a": 1, "b": []int{2, 3}},
	}
	repoMetaJSON, _ := json.Marshal(repoMeta)
	var repoID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`, "p3a-"+suffix, "https://example/p3a.git",
		"ext-"+suffix, "github", "https://github.com",
		string(repoMetaJSON), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}

	// Attach to a Connection (P.7 parity).
	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "p3a-conn-"+suffix, cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)

	// Seed one RepoIndexingJob with its own metadata blob.
	jobID := uuid.NewString()
	jobMeta := map[string]any{
		"durationMs":    1234,
		"shardsWritten": 3,
		"branch":        "main",
	}
	jobMetaJSON, _ := json.Marshal(jobMeta)
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, metadata, "updatedAt")
		VALUES ($1, $2, 'INDEX'::"RepoIndexingJobType", 'COMPLETED'::"RepoIndexingJobStatus", $3::jsonb, NOW())
	`, jobID, repoID, string(jobMetaJSON)); err != nil {
		t.Fatalf("insert RepoIndexingJob: %v", err)
	}

	// Hit GET /status.
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
		t.Fatalf("decode: %v body=%s", err, bodyBytes)
	}

	t.Run("Repo.metadata round-tripped", func(t *testing.T) {
		if len(body.Metadata) == 0 {
			t.Fatalf("body.metadata is empty; want non-trivial JSON")
		}
		var got map[string]any
		if err := json.Unmarshal(body.Metadata, &got); err != nil {
			t.Fatalf("decode metadata: %v raw=%s", err, body.Metadata)
		}
		// Spot-check the values survived the JSONB round-trip.
		if v, _ := got["codeHostStars"].(float64); v != 42 {
			t.Errorf("metadata.codeHostStars: got %v want 42", got["codeHostStars"])
		}
		if v, _ := got["customLabel"].(string); v != "platform-team" {
			t.Errorf("metadata.customLabel: got %v want platform-team", got["customLabel"])
		}
		if nested, _ := got["nestedSettings"].(map[string]any); nested == nil {
			t.Errorf("metadata.nestedSettings missing or wrong type")
		} else if v, _ := nested["a"].(float64); v != 1 {
			t.Errorf("metadata.nestedSettings.a: got %v want 1", nested["a"])
		}
	})

	t.Run("Job.metadata round-tripped", func(t *testing.T) {
		if len(body.Jobs) != 1 {
			t.Fatalf("jobs len: got %d want 1", len(body.Jobs))
		}
		if len(body.Jobs[0].Metadata) == 0 {
			t.Fatalf("jobs[0].metadata empty")
		}
		var got map[string]any
		if err := json.Unmarshal(body.Jobs[0].Metadata, &got); err != nil {
			t.Fatalf("decode job metadata: %v raw=%s", err, body.Jobs[0].Metadata)
		}
		if v, _ := got["durationMs"].(float64); v != 1234 {
			t.Errorf("metadata.durationMs: got %v want 1234", got["durationMs"])
		}
		if v, _ := got["branch"].(string); v != "main" {
			t.Errorf("metadata.branch: got %v want main", got["branch"])
		}
	})

	t.Run("response includes metadata key on the wire", func(t *testing.T) {
		// String-level check — the JSON KEY must be present.
		// Easier than comparing the whole shape to a fixture.
		raw := string(bodyBytes)
		for _, key := range []string{`"metadata":`} {
			if !contains(raw, key) {
				t.Errorf("missing %s in body=%s", key, raw)
			}
		}
	})
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && stringIndex(haystack, needle) >= 0
}

func stringIndex(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
