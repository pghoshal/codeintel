//go:build integration

// Phase P.3d-ii parity guards: GET /api/repos/{id}/status
// surfaces codeIntel.scip[] with the legacy field set + ordering
// (codeIntelStatus.ts:83-141 + route.ts:66-105,221).
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

func TestParity_StatusResponse_ScipBlock(t *testing.T) {
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
	orgName := "p3d-scip-" + suffix
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
	`, "p3d-scip-"+suffix, "https://example/scip.git",
		"ext-"+suffix, "github", "https://github.com",
		string(emptyMD), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}
	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "p3d-conn-"+suffix, cfgJSON, "github", orgID).Scan(&connID)
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)

	// One CodeIntelIndex with three language children: 2 READY, 1
	// FAILED, 1 SKIPPED. Tests the bucketed languages aggregation.
	indexID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "CodeIntelIndex" (
		    id, "repoId", "orgId", kind, status, revision, "commitHash",
		    "languageCount", "symbolCount", "occurrenceCount", "relationshipCount",
		    "indexedAt", "updatedAt"
		) VALUES (
		    $1, $2, $3,
		    'SCIP'::"CodeIntelIndexKind",
		    'READY'::"CodeIntelIndexStatus",
		    'main', 'deadbeef',
		    4, 1000, 5000, 2000,
		    NOW(), NOW()
		)
	`, indexID, repoID, orgID); err != nil {
		t.Fatalf("seed CodeIntelIndex: %v", err)
	}
	// Insert language indexes with mixed projectRoot / language /
	// indexer so the ORDER BY (projectRoot, language, indexer) is
	// observable.
	type lang struct {
		root, language, indexer, status, toolchainPath, artifactPath string
		duration                                                     int32
	}
	langRows := []lang{
		{root: "/svc/api", language: "go", indexer: "scip-go", status: "READY", toolchainPath: "/opt/scip-go", artifactPath: "s3://bucket/api.scip", duration: 5000},
		{root: "/svc/api", language: "ts", indexer: "scip-typescript", status: "READY", toolchainPath: "/opt/scip-ts", artifactPath: "s3://bucket/api.scip.ts", duration: 7000},
		{root: "/", language: "py", indexer: "scip-python", status: "FAILED", duration: 0},
		{root: "/", language: "rb", indexer: "scip-ruby", status: "SKIPPED", duration: 0},
	}
	for _, l := range langRows {
		liID := uuid.NewString()
		toolPath := l.toolchainPath
		artPath := l.artifactPath
		var toolPathArg, artPathArg any
		if toolPath != "" {
			toolPathArg = toolPath
		}
		if artPath != "" {
			artPathArg = artPath
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO "CodeIntelLanguageIndex" (
			    id, "codeIntelIndexId",
			    language, "projectRoot", indexer,
			    status, "artifactPath", "toolchainPath", "durationMs", "updatedAt"
			) VALUES ($1, $2, $3, $4, $5, $6::"CodeIntelIndexStatus", $7, $8, $9, NOW())
		`, liID, indexID, l.language, l.root, l.indexer, l.status,
			artPathArg, toolPathArg, l.duration); err != nil {
			t.Fatalf("seed CodeIntelLanguageIndex %s: %v", l.language, err)
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

	t.Run("scip block populated with one index", func(t *testing.T) {
		if len(body.CodeIntel.Scip) != 1 {
			t.Fatalf("CodeIntel.Scip: got %d rows want 1", len(body.CodeIntel.Scip))
		}
	})
	scip := body.CodeIntel.Scip[0]

	t.Run("repository-level scip fields", func(t *testing.T) {
		if scip.Status != "READY" {
			t.Errorf("status: got %q want READY", scip.Status)
		}
		if scip.LanguageCount != 4 {
			t.Errorf("languageCount: got %d want 4", scip.LanguageCount)
		}
		if scip.SymbolCount != 1000 || scip.OccurrenceCount != 5000 || scip.RelationshipCount != 2000 {
			t.Errorf("counts: symbols=%d occurrences=%d relationships=%d", scip.SymbolCount, scip.OccurrenceCount, scip.RelationshipCount)
		}
		if scip.ProjectCount != 4 {
			t.Errorf("projectCount: got %d want 4", scip.ProjectCount)
		}
	})

	t.Run("detected/ready/skipped/failed language buckets", func(t *testing.T) {
		wantDetected := []string{"go", "py", "rb", "ts"}
		if !stringSliceEqualSorted(scip.DetectedLanguages, wantDetected) {
			t.Errorf("DetectedLanguages: got %v want %v", scip.DetectedLanguages, wantDetected)
		}
		wantReady := []string{"go", "ts"}
		if !stringSliceEqualSorted(scip.ReadyLanguages, wantReady) {
			t.Errorf("ReadyLanguages: got %v want %v", scip.ReadyLanguages, wantReady)
		}
		if !stringSliceEqualSorted(scip.SkippedLanguages, []string{"rb"}) {
			t.Errorf("SkippedLanguages: got %v", scip.SkippedLanguages)
		}
		if !stringSliceEqualSorted(scip.FailedLanguages, []string{"py"}) {
			t.Errorf("FailedLanguages: got %v", scip.FailedLanguages)
		}
	})

	t.Run("languageIndexes ordered by (projectRoot, language, indexer)", func(t *testing.T) {
		// Expected order:
		//   /        py    scip-python
		//   /        rb    scip-ruby
		//   /svc/api go    scip-go
		//   /svc/api ts    scip-typescript
		want := []struct{ root, lang string }{
			{"/", "py"}, {"/", "rb"},
			{"/svc/api", "go"}, {"/svc/api", "ts"},
		}
		if len(scip.LanguageIndexes) != len(want) {
			t.Fatalf("LanguageIndexes len: %d want %d", len(scip.LanguageIndexes), len(want))
		}
		for i, w := range want {
			got := scip.LanguageIndexes[i]
			if got.ProjectRoot != w.root || got.Language != w.lang {
				t.Errorf("[%d]: got (%s,%s) want (%s,%s)", i,
					got.ProjectRoot, got.Language, w.root, w.lang)
			}
		}
	})

	t.Run("toolchainPath + artifactPath emitted on the wire (includeArtifactPaths=true)", func(t *testing.T) {
		// The /svc/api go row had non-NULL toolchainPath +
		// artifactPath. The /, py + rb rows had neither.
		// includeArtifactPaths=true on this route, so the
		// JSON keys should be present (even if value is null
		// for the empty rows).
		raw := string(bodyBytes)
		if !contains(raw, `"toolchainPath":`) {
			t.Errorf("body missing toolchainPath key: %s", raw)
		}
		if !contains(raw, `"artifactPath":`) {
			t.Errorf("body missing artifactPath key: %s", raw)
		}
		// And specifically that the go row's values made it
		// through.
		if !contains(raw, `"/opt/scip-go"`) {
			t.Errorf("toolchainPath value /opt/scip-go missing: %s", raw)
		}
		if !contains(raw, `"s3://bucket/api.scip"`) {
			t.Errorf("artifactPath value missing: %s", raw)
		}
	})
}

func stringSliceEqualSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
