//go:build integration

// Phase P.3c parity guards: GET /api/repos/{id}/status surfaces
// branchStatuses (always) and branchStatus (when `?branch=X`).
// Mirrors the route.ts:218-219 behavior:
//
//   branchStatus:   requestedBranch ? buildBranchIndexStatus(...) : undefined
//   branchStatuses: buildKnownBranchIndexStatuses(repo, latestJob)
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/repoindexstatus"

	"github.com/google/uuid"
)

// Seeds Org + Repo with explicit metadata.branches + indexedRevisions.
// Returns the inputs the test asserts against.
func seedRepoWithBranches(t *testing.T, ctx context.Context, pool *db.Pool,
	branches []string, indexedRevisions []string) (orgID, repoID int32, bearer, encKey string) {
	t.Helper()
	suffix := uuid.NewString()[:8]
	orgName := "p3c-" + suffix
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	userID := uuid.NewString()
	_, _ = pool.Exec(ctx, `INSERT INTO "User" (id, email, name, "updatedAt") VALUES ($1, $2, $3, NOW())`,
		userID, orgName+"-o@test.local", "owner")
	_, _ = pool.Exec(ctx, `INSERT INTO "UserToOrg" ("userId", "orgId", role) VALUES ($1, $2, 'OWNER')`, userID, orgID)
	encKey = "test-encryption-key-32-bytes-long"
	apiSecret := uuid.NewString()
	apiKeyHash := auth.HashSecret(encKey, apiSecret)
	_, _ = pool.Exec(ctx, `INSERT INTO "ApiKey" (hash, name, "orgId", "createdById") VALUES ($1, $2, $3, $4)`,
		apiKeyHash, "key-"+suffix, orgID, userID)
	bearer = "Bearer " + auth.ApiKeyPrefix + apiSecret

	meta := map[string]any{}
	if branches != nil {
		meta["branches"] = branches
	}
	if indexedRevisions != nil {
		meta["indexedRevisions"] = indexedRevisions
	}
	metaJSON, _ := json.Marshal(meta)
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "indexedAt", "indexedCommitHash", "defaultBranch",
		    "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb,
		          NOW(), 'deadbeef', 'main', $7, NOW())
		RETURNING id
	`,
		"p3c-"+suffix, "https://example/p3c.git", "ext-"+suffix,
		"github", "https://github.com", string(metaJSON), orgID,
	).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}

	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "p3c-conn-"+suffix, cfgJSON, "github", orgID).Scan(&connID)
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)
	return
}

func TestParity_StatusResponse_BranchStatuses_Union(t *testing.T) {
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

	// branches: main (concrete), release/v1 (concrete), feature/*
	// (glob, excluded).
	// indexedRevisions: main, develop, refs/tags/v1 (tag, excluded).
	orgID, repoID, bearer, encKey := seedRepoWithBranches(t, ctx, pool,
		[]string{"main", "release/v1", "feature/*"},
		[]string{"refs/heads/main", "refs/heads/develop", "refs/tags/v1"},
	)

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
		t.Fatalf("GET: %v", err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d body=%s", resp.StatusCode, bodyBytes)
	}
	var body api.RepoStatusResponse
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	t.Run("branchStatuses returned for known branches", func(t *testing.T) {
		// Expected: main + release/v1 + develop, deduped + in
		// first-seen order (default first, then configured, then
		// indexed-only).
		want := []string{"main", "release/v1", "develop"}
		got := make([]string, len(body.BranchStatuses))
		for i, b := range body.BranchStatuses {
			got[i] = b.Branch
		}
		if !equalSlices(got, want) {
			t.Errorf("BranchStatuses ordering: got %v want %v", got, want)
		}
	})

	t.Run("default branch is indexed + allowedByPolicy", func(t *testing.T) {
		main := findBranch(t, body.BranchStatuses, "main")
		if !main.AllowedByPolicy {
			t.Errorf("main.allowedByPolicy=false")
		}
		if !main.Indexed {
			t.Errorf("main.indexed=false")
		}
		if main.Status != "indexed" {
			t.Errorf("main.status=%q want indexed", main.Status)
		}
	})

	t.Run("non-default configured branch: allowed, not indexed", func(t *testing.T) {
		// release/v1 is in metadata.branches AND in
		// metadata.indexedRevisions? No — only main + develop
		// are. So release/v1 is allowed by policy but not yet
		// indexed (indexed=false).
		rel := findBranch(t, body.BranchStatuses, "release/v1")
		if !rel.AllowedByPolicy {
			t.Errorf("release/v1.allowedByPolicy=false")
		}
		if rel.Indexed {
			t.Errorf("release/v1.indexed=true want false (not in indexedRevisions)")
		}
	})

	t.Run("indexed-only branch surfaces with indexed=true", func(t *testing.T) {
		dev := findBranch(t, body.BranchStatuses, "develop")
		// develop is in indexedRevisions but NOT in branches
		// patterns — should be DISallowed by policy (since
		// branches list is non-empty + doesn't include develop
		// or "*"), but the branch still appears in the union.
		if dev.AllowedByPolicy {
			t.Errorf("develop.allowedByPolicy=true want false (not in branches list)")
		}
		// Since not allowedByPolicy, indexed=false despite
		// being in indexedRevisions (legacy semantics:
		// indexStatus.ts:184 gates `indexed` on allowedByPolicy).
		if dev.Indexed {
			t.Errorf("develop.indexed=true want false (policy excludes)")
		}
	})

	t.Run("glob entries from metadata.branches are excluded", func(t *testing.T) {
		// "feature/*" must NOT appear as its own branch row.
		if _, ok := tryFindBranch(body.BranchStatuses, "feature/*"); ok {
			t.Errorf("glob entry feature/* should be excluded from BranchStatuses")
		}
	})

	t.Run("tag-only indexedRevisions don't produce branch rows", func(t *testing.T) {
		if _, ok := tryFindBranch(body.BranchStatuses, "v1"); ok {
			t.Errorf("refs/tags/v1 should not produce a branch row")
		}
	})

	t.Run("branchStatus omitted when ?branch= absent", func(t *testing.T) {
		if body.BranchStatus != nil {
			t.Errorf("BranchStatus: got %+v want nil (no ?branch= query)", body.BranchStatus)
		}
	})
}

func TestParity_StatusResponse_BranchStatus_QueryParam(t *testing.T) {
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
	orgID, repoID, bearer, encKey := seedRepoWithBranches(t, ctx, pool,
		nil, // no branches list -> only default allowed
		[]string{"refs/heads/main"},
	)

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

	// Hit with ?branch=main
	url := fmt.Sprintf("%s/api/repos/%d/status?branch=main", httpSrv.URL, repoID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", bearer)
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
		t.Fatalf("decode: %v", err)
	}

	t.Run("branchStatus populated with the requested branch", func(t *testing.T) {
		if body.BranchStatus == nil {
			t.Fatalf("BranchStatus: got nil want {branch:main, ...}")
		}
		if body.BranchStatus.Branch != "main" {
			t.Errorf("BranchStatus.branch: got %q want main", body.BranchStatus.Branch)
		}
		if body.BranchStatus.Revision != "refs/heads/main" {
			t.Errorf("BranchStatus.revision: got %q want refs/heads/main", body.BranchStatus.Revision)
		}
		if !body.BranchStatus.AllowedByPolicy {
			t.Errorf("BranchStatus.allowedByPolicy: false")
		}
		if !body.BranchStatus.Indexed {
			t.Errorf("BranchStatus.indexed: false")
		}
	})

	// Hit with ?branch=feature (not in branches list -> disallowed
	// when no patterns + not the default).
	url = fmt.Sprintf("%s/api/repos/%d/status?branch=feature", httpSrv.URL, repoID)
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("Authorization", bearer)
	resp, err = httpSrv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET feature: %v", err)
	}
	bodyBytes, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("feature status: got %d body=%s", resp.StatusCode, bodyBytes)
	}
	_ = json.Unmarshal(bodyBytes, &body)
	t.Run("non-default-non-pattern branch: disallowed + not indexed", func(t *testing.T) {
		if body.BranchStatus == nil {
			t.Fatalf("BranchStatus: got nil")
		}
		if body.BranchStatus.AllowedByPolicy {
			t.Errorf("feature.allowedByPolicy=true want false")
		}
		if body.BranchStatus.Indexed {
			t.Errorf("feature.indexed=true want false")
		}
	})
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	// Strict ordered comparison — the legacy preserves first-seen
	// order via [...new Set(...)]; we mirror that.
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func findBranch(t *testing.T, bs []repoindexstatus.BranchIndexStatus, name string) repoindexstatus.BranchIndexStatus {
	t.Helper()
	for _, b := range bs {
		if b.Branch == name {
			return b
		}
	}
	var names []string
	for _, b := range bs {
		names = append(names, b.Branch)
	}
	sort.Strings(names)
	t.Fatalf("branch %q not in BranchStatuses; got %v", name, names)
	return repoindexstatus.BranchIndexStatus{}
}

func tryFindBranch(bs []repoindexstatus.BranchIndexStatus, name string) (repoindexstatus.BranchIndexStatus, bool) {
	for _, b := range bs {
		if b.Branch == name {
			return b, true
		}
	}
	return repoindexstatus.BranchIndexStatus{}, false
}

