//go:build integration

// Phase P.3d-iii parity guards: GET /api/repos/{id}/status
// surfaces codeIntel.codeGraph[] (sorted) + codeIntel.currentCodeGraph
// (the SelectCurrentCodeGraphIndex pick). Mirrors
// codeIntelStatus.ts:143-211 + route.ts:222-223.
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

func TestParity_StatusResponse_CodeGraphBlock(t *testing.T) {
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
	orgName := "p3d-cg-" + suffix
	var orgID int32
	if err := pool.QueryRow(ctx, `INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id`,
		orgName, orgName+".test").Scan(&orgID); err != nil {
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
	bearer := "Bearer " + auth.ApiKeyPrefix + apiSecret

	emptyMD, _ := json.Marshal(map[string]any{})
	var repoID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`, "p3d-cg-"+suffix, "https://example/cg.git",
		"ext-"+suffix, "github", "https://github.com",
		string(emptyMD), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}
	var connID int32
	cfg, _ := json.Marshal(map[string]any{"type": "github"})
	_ = pool.QueryRow(ctx, `INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt") VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id`,
		"p3d-cg-conn-"+suffix, cfg, "github", orgID).Scan(&connID)
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)

	// Seed two CodeGraphIndex rows: one READY with a revision
	// (the "current" pick), one FAILED without revisions.
	readyID := uuid.NewString()
	failedID := uuid.NewString()

	// READY index — wins SelectCurrentCodeGraphIndex via
	// isCurrentGraphSnapshot=true (READY + has revisions).
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeGraphIndex" (
		    id, "repoId", "orgId", provider, status,
		    "commitHash", "workspaceId", "schemaVersion", "builderVersion",
		    "vertexCount", "edgeCount", "anchorCount", "linkedEdgeCount",
		    "indexedAt", "updatedAt"
		) VALUES (
		    $1, $2, $3,
		    'NEBULA'::"CodeGraphProvider",
		    'READY'::"CodeGraphIndexStatus",
		    'abc123', 'ws', 1, 'v1',
		    1000, 2000, 500, 750,
		    NOW(), NOW()
		)
	`, readyID, repoID, orgID); err != nil {
		t.Fatalf("seed READY CodeGraphIndex: %v", err)
	}
	// Revision attached to the READY index so it counts as a
	// "current snapshot".
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeGraphRevision" (
		    id, revision, "workspaceId", "commitHash",
		    "provider", "schemaVersion", "builderVersion",
		    "orgId", "repoId", "codeGraphIndexId", "updatedAt"
		) VALUES (
		    $1, 'refs/heads/main', 'ws', 'abc123',
		    'NEBULA'::"CodeGraphProvider", 1, 'v1',
		    $2, $3, $4, NOW()
		)
	`, uuid.NewString(), orgID, repoID, readyID); err != nil {
		t.Fatalf("seed CodeGraphRevision: %v", err)
	}

	// FAILED index, no revisions.
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeGraphIndex" (
		    id, "repoId", "orgId", provider, status,
		    "commitHash", "workspaceId", "schemaVersion", "builderVersion",
		    "vertexCount", "edgeCount", "anchorCount", "linkedEdgeCount",
		    "indexedAt", "errorMessage", "updatedAt"
		) VALUES (
		    $1, $2, $3,
		    'NEBULA'::"CodeGraphProvider",
		    'FAILED'::"CodeGraphIndexStatus",
		    'def456', 'ws', 1, 'v1',
		    0, 0, 0, 0,
		    NOW(), 'failure reason here', NOW()
		)
	`, failedID, repoID, orgID); err != nil {
		t.Fatalf("seed FAILED CodeGraphIndex: %v", err)
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
		t.Fatalf("decode: %v body=%s", err, bodyBytes)
	}

	t.Run("codeGraph[] has both rows, sorted READY-with-revisions first", func(t *testing.T) {
		if len(body.CodeIntel.CodeGraph) != 2 {
			t.Fatalf("codeGraph: got %d want 2", len(body.CodeIntel.CodeGraph))
		}
		if body.CodeIntel.CodeGraph[0].ID != readyID {
			t.Errorf("codeGraph[0].id: got %q want %q (READY w/ revisions first)",
				body.CodeIntel.CodeGraph[0].ID, readyID)
		}
		if body.CodeIntel.CodeGraph[1].ID != failedID {
			t.Errorf("codeGraph[1].id: got %q want %q (FAILED last)",
				body.CodeIntel.CodeGraph[1].ID, failedID)
		}
	})

	t.Run("currentCodeGraph points at the READY+revisions row", func(t *testing.T) {
		if body.CodeIntel.CurrentCodeGraph == nil {
			t.Fatalf("currentCodeGraph: got nil")
		}
		if body.CodeIntel.CurrentCodeGraph.ID != readyID {
			t.Errorf("currentCodeGraph.id: got %q want %q",
				body.CodeIntel.CurrentCodeGraph.ID, readyID)
		}
		if !body.CodeIntel.CurrentCodeGraph.IsActive {
			t.Errorf("currentCodeGraph.isActive: got false want true")
		}
		if body.CodeIntel.CurrentCodeGraph.ActiveRevisionCount != 1 {
			t.Errorf("currentCodeGraph.activeRevisionCount: got %d want 1",
				body.CodeIntel.CurrentCodeGraph.ActiveRevisionCount)
		}
		revs := body.CodeIntel.CurrentCodeGraph.ActiveRevisions
		if len(revs) != 1 || revs[0].Revision != "refs/heads/main" {
			t.Errorf("currentCodeGraph.activeRevisions: %+v", revs)
		}
	})

	t.Run("FAILED row has isActive=false + errorMessage exposed", func(t *testing.T) {
		failed := body.CodeIntel.CodeGraph[1]
		if failed.IsActive {
			t.Errorf("FAILED row IsActive=true")
		}
		if failed.ErrorMessage == nil || *failed.ErrorMessage != "failure reason here" {
			t.Errorf("errorMessage: %v", failed.ErrorMessage)
		}
		if failed.Status != "FAILED" {
			t.Errorf("status: got %q want FAILED", failed.Status)
		}
	})

	t.Run("READY row repo-level scalar fields surface correctly", func(t *testing.T) {
		ready := body.CodeIntel.CodeGraph[0]
		if ready.VertexCount != 1000 || ready.EdgeCount != 2000 || ready.AnchorCount != 500 || ready.LinkedEdgeCount != 750 {
			t.Errorf("counts: V=%d E=%d A=%d L=%d", ready.VertexCount, ready.EdgeCount, ready.AnchorCount, ready.LinkedEdgeCount)
		}
		if ready.WorkspaceID != "ws" {
			t.Errorf("workspaceId: %q", ready.WorkspaceID)
		}
		if ready.SchemaVersion != 1 {
			t.Errorf("schemaVersion: %d", ready.SchemaVersion)
		}
		if ready.BuilderVersion != "v1" {
			t.Errorf("builderVersion: %q", ready.BuilderVersion)
		}
		// No semantic_* rows seeded, so the count subqueries
		// return 0 — which falls through to the
		// "explicit nil + agg 0" branch -> 0.
		if ready.SemanticFactCount != 0 || ready.SemanticEdgeCount != 0 || ready.SemanticHyperedgeCount != 0 {
			t.Errorf("semantic*: facts=%d edges=%d hyper=%d (expected zero — no semantic rows seeded)",
				ready.SemanticFactCount, ready.SemanticEdgeCount, ready.SemanticHyperedgeCount)
		}
	})
}

// No-CodeGraphIndex rows -> empty array + null currentCodeGraph.
func TestParity_StatusResponse_CodeGraphBlock_Empty(t *testing.T) {
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
	orgName := "p3d-cge-" + suffix
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
	`, "p3d-cge-"+suffix, "https://example/cge.git",
		"ext-"+suffix, "github", "https://github.com",
		string(emptyMD), orgID).Scan(&repoID)
	var connID int32
	cfg, _ := json.Marshal(map[string]any{"type": "github"})
	_ = pool.QueryRow(ctx, `INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt") VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW()) RETURNING id`,
		"p3d-cge-conn-"+suffix, cfg, "github", orgID).Scan(&connID)
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

	if body.CodeIntel.CodeGraph == nil || len(body.CodeIntel.CodeGraph) != 0 {
		t.Errorf("codeGraph: got %+v want [] (empty slice, not nil)", body.CodeIntel.CodeGraph)
	}
	if body.CodeIntel.CurrentCodeGraph != nil {
		t.Errorf("currentCodeGraph: got %+v want nil", body.CodeIntel.CurrentCodeGraph)
	}
}
