//go:build integration

// Phase P.3b parity guard: GET /api/repos/{id}/status surfaces
// the buildRepoIndexSummary-derived fields (indexStatus,
// indexStatusColor, indexed, activeIndexStatus,
// activeIndexStatusColor, activeIndexUsable, indexedRevisions,
// latestIndexRun, latestJob) with values byte-equivalent to the
// legacy port. Mirrors three of the four legacy unit cases
// from packages/web/src/features/repos/indexStatus.test.ts:
// the "indexed-with-failed-reindex", "not-indexed-with-failed",
// and "indexing-in-progress" branches.
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

// seedRepoForSummary creates an Org + OWNER ApiKey + Repo +
// Connection + RepoToConnection. mods can tweak Repo columns +
// metadata before insert. Returns (orgID, repoID, bearer,
// encryptionKey).
type seedOpts struct {
	indexed           bool
	indexedCommitHash string
	indexedRevisions  []string
	defaultBranch     string
}

func seedRepoForSummary(t *testing.T, ctx context.Context, pool *db.Pool, opts seedOpts) (int32, int32, string, string) {
	t.Helper()
	if opts.defaultBranch == "" {
		opts.defaultBranch = "main"
	}
	if opts.indexedRevisions == nil {
		opts.indexedRevisions = []string{"refs/heads/" + opts.defaultBranch}
	}

	suffix := uuid.NewString()[:8]
	orgName := "p3b-" + suffix
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

	metaJSON, _ := json.Marshal(map[string]any{
		"indexedRevisions": opts.indexedRevisions,
	})

	indexedAtClause := "NULL"
	indexedCommitClause := "NULL"
	if opts.indexed {
		indexedAtClause = "NOW()"
		if opts.indexedCommitHash == "" {
			opts.indexedCommitHash = "deadbeef"
		}
		indexedCommitClause = "$8"
	}
	args := []any{
		"p3b-" + suffix, // 1 name
		"https://example/p3b.git", // 2 cloneUrl
		"ext-" + suffix, // 3 external_id
		"github",        // 4 codeHostType
		"https://github.com", // 5 codeHostUrl
		string(metaJSON), // 6 metadata
		orgID, // 7 orgId
	}
	if opts.indexed {
		args = append(args, opts.indexedCommitHash) // 8
	}
	repoSQL := fmt.Sprintf(`
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "indexedAt", "indexedCommitHash", "defaultBranch",
		    "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5,
		          $6::jsonb, %s, %s, '%s', $7, NOW())
		RETURNING id
	`, indexedAtClause, indexedCommitClause, opts.defaultBranch)
	var repoID int32
	if err := pool.QueryRow(ctx, repoSQL, args...).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}

	var connID int32
	cfgJSON, _ := json.Marshal(map[string]any{"type": "github"})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Connection" (name, config, "connectionType", "orgId", "updatedAt")
		VALUES ($1, $2::jsonb, $3::"ConnectionType", $4, NOW())
		RETURNING id
	`, "p3b-conn-"+suffix, cfgJSON, "github", orgID).Scan(&connID); err != nil {
		t.Fatalf("insert Connection: %v", err)
	}
	_, _ = pool.Exec(ctx, `INSERT INTO "RepoToConnection" ("connectionId", "repoId", "addedAt") VALUES ($1, $2, NOW())`, connID, repoID)

	return orgID, repoID, "Bearer " + auth.ApiKeyPrefix + apiSecret, encryptionKey
}

func seedJob(t *testing.T, ctx context.Context, pool *db.Pool, repoID int32, jobType, status string) string {
	t.Helper()
	jobID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, "updatedAt")
		VALUES ($1, $2, $3::"RepoIndexingJobType", $4::"RepoIndexingJobStatus", NOW())
	`, jobID, repoID, jobType, status); err != nil {
		t.Fatalf("insert RepoIndexingJob: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE "Repo" SET "latestIndexingJobStatus" = $1::"RepoIndexingJobStatus" WHERE id = $2`, status, repoID); err != nil {
		t.Fatalf("update Repo.latestIndexingJobStatus: %v", err)
	}
	return jobID
}

func getStatus(t *testing.T, ctx context.Context, httpSrv *httptest.Server, bearer string, repoID int32) api.RepoStatusResponse {
	t.Helper()
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
		t.Fatalf("decode: %v body=%s", err, bodyBytes)
	}
	return body
}

func startServer(t *testing.T, pool *db.Pool, orgID int32, encKey string) *httptest.Server {
	t.Helper()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	apiSrv := api.NewServer(api.Config{
		Logger:            silent,
		Queries:           db.NewQueries(pool),
		EncryptionKey:     encKey,
		DBPinger:          pool,
		SingleTenantOrgID: orgID,
		RepoStatusFetcher: api.NewPgxRepoStatusFetcher(pool.Pool),
	})
	return httptest.NewServer(apiSrv.Router())
}

// Case 1 (mirror of indexStatus.test.ts:17-31):
// indexed=true, latest job FAILED — visible status stays
// "indexed"/green, latestRun is "failed"/red, blocks=false.
func TestParity_Summary_OlderIndexUsableWhenReindexFailed(t *testing.T) {
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

	orgID, repoID, bearer, encKey := seedRepoForSummary(t, ctx, pool, seedOpts{indexed: true})
	jobID := seedJob(t, ctx, pool, repoID, "INDEX", "FAILED")

	httpSrv := startServer(t, pool, orgID, encKey)
	defer httpSrv.Close()
	body := getStatus(t, ctx, httpSrv, bearer, repoID)

	if body.IndexStatus != "indexed" {
		t.Errorf("indexStatus: got %q want indexed", body.IndexStatus)
	}
	if body.IndexStatusColor != "green" {
		t.Errorf("indexStatusColor: got %q want green", body.IndexStatusColor)
	}
	if !body.Indexed {
		t.Errorf("indexed: got false want true")
	}
	if body.ActiveIndexStatus != "indexed" {
		t.Errorf("activeIndexStatus: got %q want indexed", body.ActiveIndexStatus)
	}
	if !body.ActiveIndexUsable {
		t.Errorf("activeIndexUsable: got false want true")
	}
	if body.LatestIndexRun.Status != "failed" {
		t.Errorf("latestIndexRun.status: got %q want failed", body.LatestIndexRun.Status)
	}
	if body.LatestIndexRun.StatusColor != "red" {
		t.Errorf("latestIndexRun.statusColor: got %q want red", body.LatestIndexRun.StatusColor)
	}
	if body.LatestIndexRun.BlocksActiveIndex {
		t.Errorf("latestIndexRun.blocksActiveIndex: got true want false (older index still usable)")
	}
	if body.LatestJob == nil || body.LatestJob.ID != jobID {
		t.Errorf("latestJob: got %+v want %s", body.LatestJob, jobID)
	}
	if len(body.IndexedRevisions) != 1 || body.IndexedRevisions[0] != "refs/heads/main" {
		t.Errorf("indexedRevisions: got %v", body.IndexedRevisions)
	}
}

// Case 2 (mirror of indexStatus.test.ts:33-48):
// indexedAt=null, empty indexedRevisions, latest job FAILED ->
// status="failed"/red, indexed=false, blocks=true.
func TestParity_Summary_FailedWhenNoActiveIndex(t *testing.T) {
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

	orgID, repoID, bearer, encKey := seedRepoForSummary(t, ctx, pool, seedOpts{
		indexed:          false,
		indexedRevisions: []string{}, // empty
	})
	seedJob(t, ctx, pool, repoID, "INDEX", "FAILED")

	httpSrv := startServer(t, pool, orgID, encKey)
	defer httpSrv.Close()
	body := getStatus(t, ctx, httpSrv, bearer, repoID)

	if body.IndexStatus != "failed" {
		t.Errorf("indexStatus: got %q want failed", body.IndexStatus)
	}
	if body.IndexStatusColor != "red" {
		t.Errorf("indexStatusColor: got %q want red", body.IndexStatusColor)
	}
	if body.Indexed {
		t.Errorf("indexed: got true want false")
	}
	if body.ActiveIndexStatus != "not_indexed" {
		t.Errorf("activeIndexStatus: got %q want not_indexed", body.ActiveIndexStatus)
	}
	if !body.LatestIndexRun.BlocksActiveIndex {
		t.Errorf("latestIndexRun.blocksActiveIndex: got false want true")
	}
}

// Case 3 (mirror of indexStatus.test.ts:50-63):
// indexed=true, latest job IN_PROGRESS -> visible=indexing/yellow,
// activeIndex still usable, blocks=true.
func TestParity_Summary_IndexingPreservesActiveUsability(t *testing.T) {
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

	orgID, repoID, bearer, encKey := seedRepoForSummary(t, ctx, pool, seedOpts{indexed: true})
	seedJob(t, ctx, pool, repoID, "INDEX", "IN_PROGRESS")

	httpSrv := startServer(t, pool, orgID, encKey)
	defer httpSrv.Close()
	body := getStatus(t, ctx, httpSrv, bearer, repoID)

	if body.IndexStatus != "indexing" {
		t.Errorf("indexStatus: got %q want indexing", body.IndexStatus)
	}
	if body.IndexStatusColor != "yellow" {
		t.Errorf("indexStatusColor: got %q want yellow", body.IndexStatusColor)
	}
	if !body.Indexed {
		t.Errorf("indexed: got false want true (older index still usable)")
	}
	if body.ActiveIndexStatus != "indexed" {
		t.Errorf("activeIndexStatus: got %q want indexed", body.ActiveIndexStatus)
	}
	if body.LatestIndexRun.Status != "indexing" {
		t.Errorf("latestIndexRun.status: got %q want indexing", body.LatestIndexRun.Status)
	}
	if !body.LatestIndexRun.BlocksActiveIndex {
		t.Errorf("latestIndexRun.blocksActiveIndex: got false want true")
	}
}

// Never-indexed + no jobs at all: status=not_indexed/gray,
// latestRun=none/gray.
func TestParity_Summary_NeverIndexed_NoJobs(t *testing.T) {
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

	orgID, repoID, bearer, encKey := seedRepoForSummary(t, ctx, pool, seedOpts{
		indexed:          false,
		indexedRevisions: []string{},
	})
	// No job seeded.

	httpSrv := startServer(t, pool, orgID, encKey)
	defer httpSrv.Close()
	body := getStatus(t, ctx, httpSrv, bearer, repoID)

	if body.IndexStatus != "not_indexed" {
		t.Errorf("indexStatus: got %q want not_indexed", body.IndexStatus)
	}
	if body.IndexStatusColor != "gray" {
		t.Errorf("indexStatusColor: got %q want gray", body.IndexStatusColor)
	}
	if body.LatestIndexRun.Status != "none" {
		t.Errorf("latestIndexRun.status: got %q want none", body.LatestIndexRun.Status)
	}
	if body.LatestIndexRun.StatusColor != "gray" {
		t.Errorf("latestIndexRun.statusColor: got %q want gray", body.LatestIndexRun.StatusColor)
	}
	if body.LatestJob != nil {
		t.Errorf("latestJob: got %+v want nil", body.LatestJob)
	}
}
