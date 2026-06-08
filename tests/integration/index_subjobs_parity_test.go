//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeintel/internal/backend/astartifact"
	"codeintel/internal/backend/graphstore"
	"codeintel/internal/backend/indexcore"
	"codeintel/internal/backend/indexexecutor"
	"codeintel/internal/backend/indexplanwriter"
	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/internal/backend/scipartifact"
	"codeintel/internal/db"
	"codeintel/internal/migrate"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"
	"codeintel/pkg/graphschema"
	"codeintel/pkg/repopaths"
	codeintelv1 "codeintel/proto/codeintel/v1"
	scippb "codeintel/proto/scip/v1"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

func TestIndexSubjobsBranchWorkspaceUniqueScope(t *testing.T) {
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

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	var orgID int32
	workspaceID := "atom-ws-" + suffix
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "atomWorkspaceId", "updatedAt")
		VALUES ($1, $2, $3, NOW())
		RETURNING id
	`, "index-subjob-org-"+suffix, "index-subjob-"+suffix, workspaceID).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	var repoID int32
	repoMetadata, _ := json.Marshal(map[string]any{
		"branches": []string{"main", "release"},
	})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
			name, "isFork", "isArchived",
			"cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
			metadata, "orgId", "updatedAt", "defaultBranch"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW(), 'main')
		RETURNING id
	`, "repo-"+suffix, "https://example.com/repo-"+suffix+".git",
		"ext-"+suffix, "github", "https://github.com", string(repoMetadata), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	jobID := "job-" + suffix
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, "updatedAt")
		VALUES ($1, $2, 'INDEX'::"RepoIndexingJobType", 'IN_PROGRESS'::"RepoIndexingJobStatus", NOW())
	`, jobID, repoID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/release", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	store := indexsubjobs.NewStore(pool.Pool)
	language := "go"
	projectRoot := ""
	indexer := "scip-go"
	base := indexsubjobs.CreateInput{
		ID:                "subjob-main-" + suffix,
		RepoIndexingJobID: jobID,
		OrgID:             orgID,
		WorkspaceID:       &workspaceID,
		RepoID:            repoID,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Layer:             indexsubjobs.LayerSCIP,
		Language:          &language,
		ProjectRoot:       &projectRoot,
		Indexer:           &indexer,
		WorkerClass:       "scip-go",
		QueueName:         "codeintel-index-scip-go",
	}
	if err := store.UpsertQueued(ctx, base); err != nil {
		t.Fatalf("upsert main: %v", err)
	}
	release := base
	release.ID = "subjob-release-" + suffix
	release.Branch = "refs/heads/release"
	release.Revision = "refs/heads/release"
	release.CommitHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := store.UpsertQueued(ctx, release); err != nil {
		t.Fatalf("upsert release: %v", err)
	}
	duplicateMain := base
	duplicateMain.ID = "subjob-main-duplicate-" + suffix
	if err := store.UpsertQueued(ctx, duplicateMain); err != nil {
		t.Fatalf("upsert duplicate main: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "CodeIntelIndexSubjob"
		WHERE "repoIndexingJobId" = $1
	`, jobID).Scan(&count); err != nil {
		t.Fatalf("count subjobs: %v", err)
	}
	if count != 2 {
		t.Fatalf("subjob count = %d want 2", count)
	}
}

func TestIndexPlanWriterPersistsBranchScopedSubjobs(t *testing.T) {
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

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID, workspaceID, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)

	mainCommit := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	releaseCommit := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/main", mainCommit)
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/release", releaseCommit)

	req := &codeintelv1.WriteIndexPlanRequest{
		IndexJobId:  jobID,
		OrgId:       orgID,
		RepoId:      repoID,
		MaxAttempts: 3,
		Revisions: []*codeintelv1.IndexPlanRevision{
			planRevision(workspaceID, "refs/heads/main", mainCommit),
			planRevision(workspaceID, "refs/heads/release", releaseCommit),
		},
	}
	srv := indexplanwriter.NewServer(pool.Pool, indexsubjobs.NewStore(pool.Pool), nil)
	if _, err := srv.WritePlan(ctx, req); err != nil {
		t.Fatalf("WritePlan first: %v", err)
	}
	if _, err := srv.WritePlan(ctx, req); err != nil {
		t.Fatalf("WritePlan replay: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "CodeIntelIndexSubjob"
		WHERE "repoIndexingJobId" = $1
	`, jobID).Scan(&count); err != nil {
		t.Fatalf("count subjobs: %v", err)
	}
	if count != 8 {
		t.Fatalf("subjob count = %d want 8", count)
	}
	var distinctBranches int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT branch)
		FROM "CodeIntelIndexSubjob"
		WHERE "repoIndexingJobId" = $1
		  AND "workspaceId" = $2
		  AND "orgId" = $3
		  AND "repoId" = $4
	`, jobID, workspaceID, orgID, repoID).Scan(&distinctBranches); err != nil {
		t.Fatalf("count branches: %v", err)
	}
	if distinctBranches != 2 {
		t.Fatalf("distinct branches = %d want 2", distinctBranches)
	}
	var scipGoRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "CodeIntelIndexSubjob"
		WHERE "repoIndexingJobId" = $1
		  AND layer = 'SCIP'
		  AND "workerClass" = 'scip-go'
		  AND "queueName" = 'codeintel-index-scip-go'
	`, jobID).Scan(&scipGoRows); err != nil {
		t.Fatalf("count scip rows: %v", err)
	}
	if scipGoRows != 2 {
		t.Fatalf("scip-go rows = %d want 2", scipGoRows)
	}
}

func TestSCIPArtifactIngestPersistsRowsIdempotently(t *testing.T) {
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

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID, workspaceID, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)
	commit := "cccccccccccccccccccccccccccccccccccccccc"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/main", commit)
	dataRoot := t.TempDir()
	paths := repopaths.Config{DataCacheDir: dataRoot}
	artifactRoot := t.TempDir()
	worktree := paths.RevisionSnapshotPath(orgID, repoID, commit)
	mustWriteIntegration(t, filepath.Join(worktree, "src/orders/createOrder.ts"),
		"export async function createOrder(command) {\n  return command.id;\n}\n")
	mustWriteIntegration(t, filepath.Join(worktree, "src/routes/internalOrders.ts"),
		"export async function handler() {\n  return createOrder({ id: '1' });\n}\n")
	createSymbol := "scip-typescript npm app 1.0.0 src/orders/createOrder.ts/createOrder()."
	handlerSymbol := "scip-typescript npm app 1.0.0 src/routes/internalOrders.ts/handler()."
	raw := mustSCIPArtifact(t, createSymbol, handlerSymbol)
	artifactPath := filepath.Join(artifactRoot, fmt.Sprint(orgID), fmt.Sprint(repoID), artifactScopeSegmentForTest(workspaceID), artifactScopeSegmentForTest("refs/heads/main"), commit, "scip", "artifact.scip")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.WriteFile(artifactPath, raw, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256(raw)
	language := "typescript"
	projectRoot := ""
	indexer := "scip-typescript"
	payload := indexsubjobtask.Payload{
		SubjobID:          "subjob-" + suffix,
		RepoIndexingJobID: "job-" + suffix,
		OrgID:             orgID,
		WorkspaceID:       &workspaceID,
		RepoID:            repoID,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        commit,
		Layer:             indexsubjobtask.LayerSCIP,
		Language:          &language,
		ProjectRoot:       &projectRoot,
		Indexer:           &indexer,
		WorkerClass:       "scip-ts-python",
		QueueName:         "codeintel-index-scip-ts-python",
		Attempt:           1,
	}
	result := indexexecutor.Result{
		ArtifactPath:   artifactPath,
		ArtifactSHA256: fmt.Sprintf("sha256:%x", sum[:]),
		Metadata:       map[string]string{"durationMs": "17"},
	}
	subjobStore := indexsubjobs.NewStore(pool.Pool)
	if err := subjobStore.UpsertQueued(ctx, indexsubjobs.CreateInput{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.LayerSCIP,
		Language:          payload.Language,
		ProjectRoot:       payload.ProjectRoot,
		Indexer:           payload.Indexer,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
	}); err != nil {
		t.Fatalf("UpsertQueued SCIP subjob: %v", err)
	}
	leaseOwner := "integration-worker"
	attemptID := "attempt-" + suffix
	claimed, err := subjobStore.ClaimScoped(ctx, indexsubjobs.ClaimScope{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.LayerSCIP,
		Language:          payload.Language,
		ProjectRoot:       payload.ProjectRoot,
		Indexer:           payload.Indexer,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
		Attempt:           payload.Attempt,
	}, leaseOwner, attemptID, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ClaimScoped SCIP subjob: %v", err)
	}
	if !claimed {
		t.Fatal("ClaimScoped SCIP subjob returned false")
	}
	written, err := subjobStore.MarkArtifactWritten(ctx, payload.SubjobID, leaseOwner, attemptID, artifactPath+".tmp", artifactPath, result.ArtifactSHA256)
	if err != nil {
		t.Fatalf("MarkArtifactWritten: %v", err)
	}
	if !written {
		t.Fatal("MarkArtifactWritten returned false")
	}
	store, err := scipartifact.NewStore(pool.Pool, paths, artifactRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Ingest(ctx, payload, result, leaseOwner, attemptID); err != nil {
		t.Fatalf("Ingest first: %v", err)
	}
	if err := store.Ingest(ctx, payload, result, leaseOwner, attemptID); err != nil {
		t.Fatalf("Ingest replay: %v", err)
	}

	var symbolCount, occurrenceCount, relationshipCount int
	var indexStatus string
	var indexedAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT status::text, "indexedAt", "symbolCount", "occurrenceCount", "relationshipCount"
		FROM "CodeIntelIndex"
		WHERE "orgId" = $1 AND "repoId" = $2 AND revision = 'refs/heads/main' AND "commitHash" = $3
	`, orgID, repoID, commit).Scan(&indexStatus, &indexedAt, &symbolCount, &occurrenceCount, &relationshipCount); err != nil {
		t.Fatalf("query CodeIntelIndex counts: %v", err)
	}
	if indexStatus != "READY" || indexedAt.IsZero() {
		t.Fatalf("CodeIntelIndex readiness status=%s indexedAt=%v", indexStatus, indexedAt)
	}
	if symbolCount != 2 || occurrenceCount != 4 || relationshipCount != 1 {
		t.Fatalf("counts symbol=%d occurrence=%d relationship=%d", symbolCount, occurrenceCount, relationshipCount)
	}
	var lineContent string
	if err := pool.QueryRow(ctx, `
		SELECT "lineContent"
		FROM "CodeIntelOccurrence"
		WHERE "orgId" = $1 AND "repoId" = $2 AND symbol = $3 AND role = 'REFERENCE'::"CodeIntelOccurrenceRole"
	`, orgID, repoID, createSymbol).Scan(&lineContent); err != nil {
		t.Fatalf("query reference line: %v", err)
	}
	if lineContent != "  return createOrder({ id: '1' });" {
		t.Fatalf("lineContent=%q", lineContent)
	}
	var duplicates int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "CodeIntelOccurrence"
		WHERE "orgId" = $1 AND "repoId" = $2
	`, orgID, repoID).Scan(&duplicates); err != nil {
		t.Fatalf("count occurrences: %v", err)
	}
	if duplicates != 4 {
		t.Fatalf("occurrence rows after replay = %d want 4", duplicates)
	}
}

func TestSCIPArtifactIngestRejectsExpiredLeaseBeforeWritingRows(t *testing.T) {
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

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID, workspaceID, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)
	commit := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/main", commit)
	dataRoot := t.TempDir()
	paths := repopaths.Config{DataCacheDir: dataRoot}
	artifactRoot := t.TempDir()
	worktree := paths.RevisionSnapshotPath(orgID, repoID, commit)
	mustWriteIntegration(t, filepath.Join(worktree, "src/orders/createOrder.ts"),
		"export async function createOrder(command) {\n  return command.id;\n}\n")
	createSymbol := "scip-typescript npm app 1.0.0 src/orders/createOrder.ts/createOrder()."
	handlerSymbol := "scip-typescript npm app 1.0.0 src/routes/internalOrders.ts/handler()."
	raw := mustSCIPArtifact(t, createSymbol, handlerSymbol)
	artifactPath := filepath.Join(artifactRoot, fmt.Sprint(orgID), fmt.Sprint(repoID), artifactScopeSegmentForTest(workspaceID), artifactScopeSegmentForTest("refs/heads/main"), commit, "scip", "artifact.scip")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.WriteFile(artifactPath, raw, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	sum := sha256.Sum256(raw)
	language := "typescript"
	projectRoot := ""
	indexer := "scip-typescript"
	payload := indexsubjobtask.Payload{
		SubjobID:          "subjob-" + suffix,
		RepoIndexingJobID: jobID,
		OrgID:             orgID,
		WorkspaceID:       &workspaceID,
		RepoID:            repoID,
		Branch:            "refs/heads/main",
		Revision:          "refs/remotes/origin/main",
		CommitHash:        commit,
		Layer:             indexsubjobtask.LayerSCIP,
		Language:          &language,
		ProjectRoot:       &projectRoot,
		Indexer:           &indexer,
		WorkerClass:       "scip-ts-python",
		QueueName:         "codeintel-index-scip-ts-python",
		Attempt:           1,
	}
	subjobStore := indexsubjobs.NewStore(pool.Pool)
	if err := subjobStore.UpsertQueued(ctx, indexsubjobs.CreateInput{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.LayerSCIP,
		Language:          payload.Language,
		ProjectRoot:       payload.ProjectRoot,
		Indexer:           payload.Indexer,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
	}); err != nil {
		t.Fatalf("UpsertQueued SCIP subjob: %v", err)
	}
	leaseOwner := "integration-worker"
	attemptID := "attempt-" + suffix
	claimed, err := subjobStore.ClaimScoped(ctx, indexsubjobs.ClaimScope{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.LayerSCIP,
		Language:          payload.Language,
		ProjectRoot:       payload.ProjectRoot,
		Indexer:           payload.Indexer,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
		Attempt:           payload.Attempt,
	}, leaseOwner, attemptID, time.Now().Add(5*time.Minute))
	if err != nil {
		t.Fatalf("ClaimScoped SCIP subjob: %v", err)
	}
	if !claimed {
		t.Fatal("ClaimScoped SCIP subjob returned false")
	}
	result := indexexecutor.Result{
		ArtifactPath:   artifactPath,
		ArtifactSHA256: fmt.Sprintf("sha256:%x", sum[:]),
		Metadata:       map[string]string{"durationMs": "17"},
	}
	written, err := subjobStore.MarkArtifactWritten(ctx, payload.SubjobID, leaseOwner, attemptID, artifactPath+".tmp", artifactPath, result.ArtifactSHA256)
	if err != nil {
		t.Fatalf("MarkArtifactWritten: %v", err)
	}
	if !written {
		t.Fatal("MarkArtifactWritten returned false")
	}
	if _, err := pool.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob"
		SET "leaseExpiresAt" = NOW() - INTERVAL '1 second'
		WHERE id = $1
	`, payload.SubjobID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	store, err := scipartifact.NewStore(pool.Pool, paths, artifactRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Ingest(ctx, payload, result, leaseOwner, attemptID); err == nil {
		t.Fatal("Ingest succeeded with expired lease")
	}
	var indexRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "CodeIntelIndex"
		WHERE "orgId" = $1 AND "repoId" = $2 AND "commitHash" = $3
	`, orgID, repoID, commit).Scan(&indexRows); err != nil {
		t.Fatalf("count CodeIntelIndex rows: %v", err)
	}
	if indexRows != 0 {
		t.Fatalf("expired lease wrote %d CodeIntelIndex rows", indexRows)
	}
}

func TestSCIPArtifactIngestScopesParentIndexByWorkspace(t *testing.T) {
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

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID, workspaceA, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)
	workspaceB := "atom-ws-b-" + suffix
	commit := "ffffffffffffffffffffffffffffffffffffffff"
	branch := "refs/heads/main"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceA, repoID, branch, commit)
	seedManifest(t, ctx, pool, jobID, orgID, workspaceB, repoID, branch, commit)

	dataRoot := t.TempDir()
	paths := repopaths.Config{DataCacheDir: dataRoot}
	artifactRoot := t.TempDir()
	worktree := paths.RevisionSnapshotPath(orgID, repoID, commit)
	mustWriteIntegration(t, filepath.Join(worktree, "src/orders/createOrder.ts"),
		"export async function createOrder(command) {\n  return command.id;\n}\n")
	mustWriteIntegration(t, filepath.Join(worktree, "src/routes/internalOrders.ts"),
		"export async function handler() {\n  return createOrder({ id: '1' });\n}\n")
	createSymbol := "scip-typescript npm app 1.0.0 src/orders/createOrder.ts/createOrder()."
	handlerSymbol := "scip-typescript npm app 1.0.0 src/routes/internalOrders.ts/handler()."
	raw := mustSCIPArtifact(t, createSymbol, handlerSymbol)
	store, err := scipartifact.NewStore(pool.Pool, paths, artifactRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	subjobStore := indexsubjobs.NewStore(pool.Pool)

	ingestWorkspace := func(workspaceID, subjobID string) {
		t.Helper()
		language := "typescript"
		projectRoot := ""
		indexer := "scip-typescript"
		payload := indexsubjobtask.Payload{
			SubjobID:          subjobID,
			RepoIndexingJobID: jobID,
			OrgID:             orgID,
			WorkspaceID:       &workspaceID,
			RepoID:            repoID,
			Branch:            branch,
			Revision:          branch,
			CommitHash:        commit,
			Layer:             indexsubjobtask.LayerSCIP,
			Language:          &language,
			ProjectRoot:       &projectRoot,
			Indexer:           &indexer,
			WorkerClass:       "scip-ts-python",
			QueueName:         "codeintel-index-scip-ts-python",
			Attempt:           1,
		}
		artifactPath := filepath.Join(artifactRoot, fmt.Sprint(orgID), fmt.Sprint(repoID), artifactScopeSegmentForTest(workspaceID), artifactScopeSegmentForTest(branch), commit, "scip", subjobID+".scip")
		if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
			t.Fatalf("mkdir artifact dir: %v", err)
		}
		if err := os.WriteFile(artifactPath, raw, 0o644); err != nil {
			t.Fatalf("write artifact: %v", err)
		}
		sum := sha256.Sum256(raw)
		if err := subjobStore.UpsertQueued(ctx, indexsubjobs.CreateInput{
			ID:                payload.SubjobID,
			RepoIndexingJobID: payload.RepoIndexingJobID,
			OrgID:             payload.OrgID,
			WorkspaceID:       payload.WorkspaceID,
			RepoID:            payload.RepoID,
			Branch:            payload.Branch,
			Revision:          payload.Revision,
			CommitHash:        payload.CommitHash,
			Layer:             indexsubjobs.LayerSCIP,
			Language:          payload.Language,
			ProjectRoot:       payload.ProjectRoot,
			Indexer:           payload.Indexer,
			WorkerClass:       payload.WorkerClass,
			QueueName:         payload.QueueName,
		}); err != nil {
			t.Fatalf("UpsertQueued SCIP subjob: %v", err)
		}
		leaseOwner := "integration-worker"
		attemptID := "attempt-" + subjobID
		claimed, err := subjobStore.ClaimScoped(ctx, indexsubjobs.ClaimScope{
			ID:                payload.SubjobID,
			RepoIndexingJobID: payload.RepoIndexingJobID,
			OrgID:             payload.OrgID,
			WorkspaceID:       payload.WorkspaceID,
			RepoID:            payload.RepoID,
			Branch:            payload.Branch,
			Revision:          payload.Revision,
			CommitHash:        payload.CommitHash,
			Layer:             indexsubjobs.LayerSCIP,
			Language:          payload.Language,
			ProjectRoot:       payload.ProjectRoot,
			Indexer:           payload.Indexer,
			WorkerClass:       payload.WorkerClass,
			QueueName:         payload.QueueName,
			Attempt:           payload.Attempt,
		}, leaseOwner, attemptID, time.Now().Add(5*time.Minute))
		if err != nil {
			t.Fatalf("ClaimScoped SCIP subjob: %v", err)
		}
		if !claimed {
			t.Fatal("ClaimScoped SCIP subjob returned false")
		}
		result := indexexecutor.Result{
			ArtifactPath:   artifactPath,
			ArtifactSHA256: fmt.Sprintf("sha256:%x", sum[:]),
		}
		written, err := subjobStore.MarkArtifactWritten(ctx, payload.SubjobID, leaseOwner, attemptID, artifactPath+".tmp", artifactPath, result.ArtifactSHA256)
		if err != nil {
			t.Fatalf("MarkArtifactWritten: %v", err)
		}
		if !written {
			t.Fatal("MarkArtifactWritten returned false")
		}
		if err := store.Ingest(ctx, payload, result, leaseOwner, attemptID); err != nil {
			t.Fatalf("Ingest %s: %v", workspaceID, err)
		}
	}

	ingestWorkspace(workspaceA, "subjob-a-"+suffix)
	ingestWorkspace(workspaceB, "subjob-b-"+suffix)

	var parentCount, distinctWorkspaceCount, symbolCount int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(DISTINCT "workspaceId")
		FROM "CodeIntelIndex"
		WHERE "orgId" = $1 AND "repoId" = $2 AND branch = $3 AND revision = $3 AND "commitHash" = $4
	`, orgID, repoID, branch, commit).Scan(&parentCount, &distinctWorkspaceCount); err != nil {
		t.Fatalf("query scoped CodeIntelIndex rows: %v", err)
	}
	if parentCount != 2 || distinctWorkspaceCount != 2 {
		t.Fatalf("parentCount=%d distinctWorkspaceCount=%d want 2/2", parentCount, distinctWorkspaceCount)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "CodeIntelSymbol"
		WHERE "orgId" = $1 AND "repoId" = $2 AND symbol = $3
	`, orgID, repoID, createSymbol).Scan(&symbolCount); err != nil {
		t.Fatalf("query symbol rows: %v", err)
	}
	if symbolCount != 2 {
		t.Fatalf("symbolCount=%d want 2, one per workspace-scoped parent index", symbolCount)
	}
}

func TestIndexExecutorHandlerPublishesAndPersistsSCIPArtifactAgainstPostgres(t *testing.T) {
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

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID, workspaceID, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)
	commit := "dddddddddddddddddddddddddddddddddddddddd"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/main", commit)

	dataRoot := t.TempDir()
	paths := repopaths.Config{DataCacheDir: dataRoot}
	worktree := paths.RevisionSnapshotPath(orgID, repoID, commit)
	mustWriteIntegration(t, filepath.Join(worktree, "src/orders/createOrder.ts"),
		"export async function createOrder(command) {\n  return command.id;\n}\n")
	mustWriteIntegration(t, filepath.Join(worktree, "src/routes/internalOrders.ts"),
		"export async function handler() {\n  return createOrder({ id: '1' });\n}\n")
	createSymbol := "scip-typescript npm app 1.0.0 src/orders/createOrder.ts/createOrder()."
	handlerSymbol := "scip-typescript npm app 1.0.0 src/routes/internalOrders.ts/handler()."
	raw := mustSCIPArtifact(t, createSymbol, handlerSymbol)
	artifactRoot := t.TempDir()
	tempPath, finalPath, sha := writeScopedArtifact(t, artifactRoot, orgID, repoID, workspaceID, "refs/heads/main", commit, "subjob-"+suffix, raw)

	language := "typescript"
	projectRoot := ""
	indexer := "scip-typescript"
	payload := indexsubjobtask.Payload{
		SubjobID:          "subjob-" + suffix,
		RepoIndexingJobID: jobID,
		OrgID:             orgID,
		WorkspaceID:       &workspaceID,
		RepoID:            repoID,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        commit,
		Layer:             indexsubjobtask.LayerSCIP,
		Language:          &language,
		ProjectRoot:       &projectRoot,
		Indexer:           &indexer,
		WorkerClass:       "scip-ts-python",
		QueueName:         "codeintel-index-scip-ts-python",
		Attempt:           1,
	}
	subjobStore := indexsubjobs.NewStore(pool.Pool)
	if err := subjobStore.UpsertQueued(ctx, indexsubjobs.CreateInput{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.LayerSCIP,
		Language:          payload.Language,
		ProjectRoot:       payload.ProjectRoot,
		Indexer:           payload.Indexer,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
	}); err != nil {
		t.Fatalf("UpsertQueued SCIP subjob: %v", err)
	}
	validator, err := indexexecutor.NewFilesystemArtifactValidator(artifactRoot)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactValidator: %v", err)
	}
	scipStore, err := scipartifact.NewStore(pool.Pool, paths, artifactRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	handler, err := indexexecutor.NewHandler(
		subjobStore,
		&integrationRunner{result: indexexecutor.Result{
			ArtifactTempPath: tempPath,
			ArtifactPath:     finalPath,
			ArtifactSHA256:   sha,
			Metadata:         map[string]string{"durationMs": "19"},
		}},
		nil,
		indexexecutor.Config{
			LeaseDuration:     time.Minute,
			HeartbeatInterval: 10 * time.Second,
			LeaseOwner:        "integration-worker",
			ArtifactValidator: validator,
			ArtifactIngestor:  scipStore,
		},
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	taskPayload, err := indexsubjobtask.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	if err := handler.Handle(ctx, asynq.NewTask(payload.QueueName, taskPayload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var subjobStatus, indexStatus string
	var symbolCount int
	if err := pool.QueryRow(ctx, `
		SELECT status
		FROM "CodeIntelIndexSubjob"
		WHERE id = $1
	`, payload.SubjobID).Scan(&subjobStatus); err != nil {
		t.Fatalf("query subjob status: %v", err)
	}
	if subjobStatus != "SUCCEEDED" {
		t.Fatalf("subjob status=%s want SUCCEEDED", subjobStatus)
	}
	if err := pool.QueryRow(ctx, `
		SELECT status::text, "symbolCount"
		FROM "CodeIntelIndex"
		WHERE "orgId" = $1 AND "repoId" = $2 AND revision = 'refs/heads/main' AND "commitHash" = $3
	`, orgID, repoID, commit).Scan(&indexStatus, &symbolCount); err != nil {
		t.Fatalf("query CodeIntelIndex: %v", err)
	}
	if indexStatus != "READY" || symbolCount != 2 {
		t.Fatalf("index status=%s symbolCount=%d", indexStatus, symbolCount)
	}
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("published artifact missing: %v", err)
	}
}

func TestIndexExecutorHandlerPublishesAndPersistsASTGraphArtifactAgainstPostgres(t *testing.T) {
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

	suffix := fmt.Sprintf("ast-%d", time.Now().UnixNano())
	orgID, workspaceID, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)
	commit := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/main", commit)

	artifactRoot := t.TempDir()
	payload := indexsubjobtask.Payload{
		SubjobID:          "ast-subjob-" + suffix,
		RepoIndexingJobID: jobID,
		OrgID:             orgID,
		WorkspaceID:       &workspaceID,
		RepoID:            repoID,
		Branch:            "refs/heads/main",
		Revision:          "refs/remotes/origin/main",
		CommitHash:        commit,
		Layer:             indexsubjobtask.LayerASTTreeSitter,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
		Attempt:           1,
	}
	graphPayload := astGraphPayloadForTest(t, payload)
	tempPath, finalPath, sha := writeScopedArtifactWithLayer(t, artifactRoot, payload, "ast_tree_sitter", "ast.json", graphPayload)
	subjobStore := indexsubjobs.NewStore(pool.Pool)
	if err := subjobStore.UpsertQueued(ctx, indexsubjobs.CreateInput{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.LayerASTTreeSitter,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
	}); err != nil {
		t.Fatalf("UpsertQueued AST subjob: %v", err)
	}
	validator, err := indexexecutor.NewFilesystemArtifactValidator(artifactRoot)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactValidator: %v", err)
	}
	graph := &fakeRenderedGraphStore{}
	astStore, err := astartifact.NewStore(pool.Pool, graph, nil, artifactRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	handler, err := indexexecutor.NewHandler(
		subjobStore,
		&integrationRunner{result: indexexecutor.Result{
			ArtifactTempPath: tempPath,
			ArtifactPath:     finalPath,
			ArtifactSHA256:   sha,
			Metadata:         map[string]string{"factCount": "2", "vertexCount": "2", "edgeCount": "1"},
		}},
		nil,
		indexexecutor.Config{
			LeaseDuration:     time.Minute,
			HeartbeatInterval: 10 * time.Second,
			LeaseOwner:        "integration-worker",
			ArtifactValidator: validator,
			ArtifactIngestor:  astStore,
		},
	)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	taskPayload, err := indexsubjobtask.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	if err := handler.Handle(ctx, asynq.NewTask(payload.QueueName, taskPayload)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var subjobStatus, graphStatus, graphError, manifestStatus string
	var vertexCount, edgeCount, factRows, edgeRows, revisionRows int
	if err := pool.QueryRow(ctx, `SELECT status FROM "CodeIntelIndexSubjob" WHERE id = $1`, payload.SubjobID).Scan(&subjobStatus); err != nil {
		t.Fatalf("query subjob: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT status::text, COALESCE("errorMessage", ''), "vertexCount", "edgeCount"
		FROM "CodeGraphIndex"
		WHERE "orgId" = $1 AND "repoId" = $2 AND "workspaceId" = $3 AND "commitHash" = $4
	`, orgID, repoID, workspaceID, commit).Scan(&graphStatus, &graphError, &vertexCount, &edgeCount); err != nil {
		t.Fatalf("query CodeGraphIndex: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphSemanticFact" WHERE "orgId" = $1 AND "repoId" = $2`, orgID, repoID).Scan(&factRows); err != nil {
		t.Fatalf("query CodeGraphSemanticFact: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphSemanticEdge" WHERE "orgId" = $1 AND "repoId" = $2`, orgID, repoID).Scan(&edgeRows); err != nil {
		t.Fatalf("query CodeGraphSemanticEdge: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexManifest" WHERE "indexJobId" = $1`, jobID).Scan(&manifestStatus); err != nil {
		t.Fatalf("query manifest: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphRevision" WHERE "orgId" = $1 AND "repoId" = $2`, orgID, repoID).Scan(&revisionRows); err != nil {
		t.Fatalf("query CodeGraphRevision: %v", err)
	}
	if subjobStatus != "SUCCEEDED" || graphStatus != "PARTIAL" || vertexCount != 2 || edgeCount != 1 || factRows != 2 || edgeRows != 1 {
		t.Fatalf("unexpected AST graph persistence: subjob=%s graph=%s graphError=%q vertices=%d edges=%d facts=%d semanticEdges=%d",
			subjobStatus, graphStatus, graphError, vertexCount, edgeCount, factRows, edgeRows)
	}
	if manifestStatus != "PENDING" || revisionRows != 0 {
		t.Fatalf("split AST graph write must not activate manifest/revision: manifest=%s revisionRows=%d", manifestStatus, revisionRows)
	}
	if graph.calls != 1 {
		t.Fatalf("graph WriteRenderedStatements calls=%d want 1", graph.calls)
	}
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("published AST artifact missing: %v", err)
	}
}

func TestASTGraphArtifactIngestRejectsExpiredLeaseBeforeWritingRows(t *testing.T) {
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

	suffix := fmt.Sprintf("ast-expired-%d", time.Now().UnixNano())
	orgID, workspaceID, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)
	commit := "ffffffffffffffffffffffffffffffffffffffff"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, "refs/heads/main", commit)
	payload := indexsubjobtask.Payload{
		SubjobID:          "ast-expired-subjob-" + suffix,
		RepoIndexingJobID: jobID,
		OrgID:             orgID,
		WorkspaceID:       &workspaceID,
		RepoID:            repoID,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        commit,
		Layer:             indexsubjobtask.LayerASTTreeSitter,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
		Attempt:           1,
	}
	subjobStore := indexsubjobs.NewStore(pool.Pool)
	if err := subjobStore.UpsertQueued(ctx, indexsubjobs.CreateInput{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.LayerASTTreeSitter,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
	}); err != nil {
		t.Fatalf("UpsertQueued AST subjob: %v", err)
	}
	if ok, err := subjobStore.ClaimScoped(ctx, indexsubjobs.ClaimScope{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.LayerASTTreeSitter,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
		Attempt:           payload.Attempt,
	}, "expired-worker", "attempt-expired", time.Now().Add(-time.Minute)); err != nil || !ok {
		t.Fatalf("ClaimScoped expired: ok=%v err=%v", ok, err)
	}

	artifactRoot := t.TempDir()
	graphPayload := astGraphPayloadForTest(t, payload)
	_, finalPath, sha := writeScopedArtifactWithLayer(t, artifactRoot, payload, "ast_tree_sitter", "ast.json", graphPayload)
	// Store.Ingest is called after validator publish in production;
	// write the final artifact directly for this stale-lease guard.
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatalf("mkdir final artifact: %v", err)
	}
	if err := os.WriteFile(finalPath, graphPayload, 0o644); err != nil {
		t.Fatalf("write final artifact: %v", err)
	}
	astStore, err := astartifact.NewStore(pool.Pool, &fakeRenderedGraphStore{}, nil, artifactRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	err = astStore.Ingest(ctx, payload, indexexecutor.Result{
		ArtifactPath:   finalPath,
		ArtifactSHA256: sha,
	}, "expired-worker", "attempt-expired")
	if err == nil {
		t.Fatal("expected expired lease ingest to fail")
	}
	var graphRows, factRows, edgeRows int
	for label, query := range map[string]string{
		"CodeGraphIndex":        `SELECT COUNT(*) FROM "CodeGraphIndex" WHERE "orgId" = $1 AND "repoId" = $2`,
		"CodeGraphSemanticFact": `SELECT COUNT(*) FROM "CodeGraphSemanticFact" WHERE "orgId" = $1 AND "repoId" = $2`,
		"CodeGraphSemanticEdge": `SELECT COUNT(*) FROM "CodeGraphSemanticEdge" WHERE "orgId" = $1 AND "repoId" = $2`,
	} {
		var n int
		if err := pool.QueryRow(ctx, query, orgID, repoID).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", label, err)
		}
		switch label {
		case "CodeGraphIndex":
			graphRows = n
		case "CodeGraphSemanticFact":
			factRows = n
		case "CodeGraphSemanticEdge":
			edgeRows = n
		}
	}
	if graphRows != 0 || factRows != 0 || edgeRows != 0 {
		t.Fatalf("expired AST ingest wrote graph rows: graph=%d facts=%d edges=%d", graphRows, factRows, edgeRows)
	}
}

func TestIndexCoreGraphMergeAndActivateAgainstPostgres(t *testing.T) {
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

	suffix := fmt.Sprintf("core-activate-%d", time.Now().UnixNano())
	orgID, workspaceID, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)
	commit := "1111111111111111111111111111111111111111"
	branch := "refs/heads/main"
	revision := "refs/remotes/origin/main"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, branch, commit)

	artifactRoot := t.TempDir()
	astPayload := indexsubjobtask.Payload{
		SubjobID:          "ast-core-subjob-" + suffix,
		RepoIndexingJobID: jobID,
		OrgID:             orgID,
		WorkspaceID:       &workspaceID,
		RepoID:            repoID,
		Branch:            branch,
		Revision:          revision,
		CommitHash:        commit,
		Layer:             indexsubjobtask.LayerASTTreeSitter,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
		Attempt:           1,
	}
	subjobStore := indexsubjobs.NewStore(pool.Pool)
	if err := subjobStore.UpsertQueued(ctx, createSubjobFromPayload(astPayload)); err != nil {
		t.Fatalf("UpsertQueued AST: %v", err)
	}
	graphPayload := astGraphPayloadForTest(t, astPayload)
	tempPath, finalPath, sha := writeScopedArtifactWithLayer(t, artifactRoot, astPayload, "ast_tree_sitter", "ast.json", graphPayload)
	validator, err := indexexecutor.NewFilesystemArtifactValidator(artifactRoot)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactValidator: %v", err)
	}
	astStore, err := astartifact.NewStore(pool.Pool, &fakeRenderedGraphStore{}, nil, artifactRoot)
	if err != nil {
		t.Fatalf("New AST store: %v", err)
	}
	astHandler, err := indexexecutor.NewHandler(
		subjobStore,
		&integrationRunner{result: indexexecutor.Result{
			ArtifactTempPath: tempPath,
			ArtifactPath:     finalPath,
			ArtifactSHA256:   sha,
			Metadata:         map[string]string{"factCount": "2", "vertexCount": "2", "edgeCount": "1"},
		}},
		nil,
		indexexecutor.Config{
			LeaseDuration:     time.Minute,
			HeartbeatInterval: 10 * time.Second,
			LeaseOwner:        "integration-worker",
			ArtifactValidator: validator,
			ArtifactIngestor:  astStore,
		},
	)
	if err != nil {
		t.Fatalf("New AST handler: %v", err)
	}
	astTaskPayload, err := indexsubjobtask.Marshal(astPayload)
	if err != nil {
		t.Fatalf("Marshal AST payload: %v", err)
	}
	if err := astHandler.Handle(ctx, asynq.NewTask(astPayload.QueueName, astTaskPayload)); err != nil {
		t.Fatalf("AST Handle: %v", err)
	}

	graphMergePayload := coreLayerPayload(astPayload, "graph-merge-subjob-"+suffix, indexsubjobtask.LayerGraphMerge)
	activatePayload := coreLayerPayload(astPayload, "activate-subjob-"+suffix, indexsubjobtask.LayerActivate)
	if err := subjobStore.UpsertQueued(ctx, createSubjobFromPayload(graphMergePayload)); err != nil {
		t.Fatalf("UpsertQueued GRAPH_MERGE: %v", err)
	}
	if err := subjobStore.UpsertQueued(ctx, createSubjobFromPayload(activatePayload)); err != nil {
		t.Fatalf("UpsertQueued ACTIVATE: %v", err)
	}

	coreHandler, err := indexcore.NewHandler(pool.Pool, subjobStore, nil, indexcore.Config{
		LeaseDuration: time.Minute,
		LeaseOwner:    "integration-core-worker",
	})
	if err != nil {
		t.Fatalf("New core handler: %v", err)
	}
	for _, payload := range []indexsubjobtask.Payload{graphMergePayload, activatePayload} {
		raw, err := indexsubjobtask.Marshal(payload)
		if err != nil {
			t.Fatalf("Marshal %s: %v", payload.Layer, err)
		}
		if err := coreHandler.Handle(ctx, asynq.NewTask(payload.QueueName, raw)); err != nil {
			t.Fatalf("core Handle %s: %v", payload.Layer, err)
		}
	}

	var graphStatus, manifestStatus, jobStatus string
	var revisionRows int
	if err := pool.QueryRow(ctx, `
		SELECT status::text
		FROM "CodeGraphIndex"
		WHERE "orgId" = $1 AND "repoId" = $2 AND "workspaceId" = $3 AND "sourceRevision" = $4 AND "commitHash" = $5
	`, orgID, repoID, workspaceID, revision, commit).Scan(&graphStatus); err != nil {
		t.Fatalf("query CodeGraphIndex status: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexManifest" WHERE "indexJobId" = $1`, jobID).Scan(&manifestStatus); err != nil {
		t.Fatalf("query RepoIndexManifest status: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM "CodeGraphRevision"
		WHERE "orgId" = $1 AND "repoId" = $2 AND "workspaceId" = $3 AND revision = $4 AND "commitHash" = $5
	`, orgID, repoID, workspaceID, revision, commit).Scan(&revisionRows); err != nil {
		t.Fatalf("query CodeGraphRevision rows: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexingJob" WHERE id = $1`, jobID).Scan(&jobStatus); err != nil {
		t.Fatalf("query RepoIndexingJob status: %v", err)
	}
	if graphStatus != "READY" || manifestStatus != "READY" || revisionRows != 1 || jobStatus != "COMPLETED" {
		t.Fatalf("activation state graph=%s manifest=%s revisions=%d job=%s, want READY/READY/1/COMPLETED",
			graphStatus, manifestStatus, revisionRows, jobStatus)
	}
}

func TestIndexExecutorAsynqRustASTGraphActivationAgainstDocker(t *testing.T) {
	dsn := requireDSN(t)
	redisURL := redisURLForExecutorE2E()
	requireLoopbackRedisOrFatal(t, redisURL)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}
	cancelStaleIntegrationDispatchRows(t, ctx, pool)

	suffix := fmt.Sprintf("asynq-rust-ast-%d", time.Now().UnixNano())
	orgID, workspaceID, repoID, jobID := seedOrgRepoJob(t, ctx, pool, suffix)
	commit := "2222222222222222222222222222222222222222"
	branch := "refs/heads/main"
	revision := "refs/remotes/origin/main"
	seedManifest(t, ctx, pool, jobID, orgID, workspaceID, repoID, branch, commit)

	dataRoot := t.TempDir()
	artifactRoot := t.TempDir()
	worktree := repopaths.Config{DataCacheDir: dataRoot}.RevisionSnapshotPath(orgID, repoID, commit)
	mustWriteIntegration(t, filepath.Join(worktree, "src/routes/orders.ts"), `
export function createOrder(command: { id: string }) {
  return { id: command.id, status: "created" };
}

export function handleOrderRoute(input: { id: string }) {
  return createOrder(input);
}
`)
	mustWriteIntegration(t, filepath.Join(worktree, "services/audit.py"), `
def emit_order_created(order_id):
    return {"event": "order.created", "id": order_id}
`)

	listenAddr := freeTCPAddr(t)
	stopExecutor := startRustExecutorForTest(t, ctx, listenAddr, dataRoot, artifactRoot)
	defer stopExecutor()

	redisOpt, err := asynqbridge.RedisOptFromURL(redisURL)
	if err != nil {
		t.Fatalf("RedisOptFromURL: %v", err)
	}
	flushRedisDBForTest(t, redisURL)
	cleanAsynqQueue(t, redisOpt, asynqueues.QueueIndexCore)
	client := asynq.NewClient(redisOpt)
	defer client.Close()

	subjobStore := indexsubjobs.NewStore(pool.Pool)
	astPayload := indexsubjobtask.Payload{
		SubjobID:          "ast-queue-subjob-" + suffix,
		RepoIndexingJobID: jobID,
		OrgID:             orgID,
		WorkspaceID:       &workspaceID,
		RepoID:            repoID,
		Branch:            branch,
		Revision:          revision,
		CommitHash:        commit,
		Layer:             indexsubjobtask.LayerASTTreeSitter,
		WorkerClass:       "core",
		QueueName:         asynqueues.QueueIndexCore,
		Attempt:           1,
	}
	for _, payload := range []indexsubjobtask.Payload{
		astPayload,
		coreLayerPayload(astPayload, "graph-merge-queue-subjob-"+suffix, indexsubjobtask.LayerGraphMerge),
		coreLayerPayload(astPayload, "activate-queue-subjob-"+suffix, indexsubjobtask.LayerActivate),
	} {
		if err := subjobStore.UpsertQueued(ctx, createSubjobFromPayload(payload)); err != nil {
			t.Fatalf("UpsertQueued %s: %v", payload.Layer, err)
		}
	}

	validator, err := indexexecutor.NewFilesystemArtifactValidator(artifactRoot)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactValidator: %v", err)
	}
	astStore, err := astartifact.NewStore(pool.Pool, &fakeRenderedGraphStore{}, nil, artifactRoot)
	if err != nil {
		t.Fatalf("New AST store: %v", err)
	}
	runner, err := indexexecutor.NewGRPCRunner(listenAddr, 30*time.Second)
	if err != nil {
		t.Fatalf("NewGRPCRunner: %v", err)
	}
	defer runner.Close()
	executorHandler, err := indexexecutor.NewHandler(
		subjobStore,
		runner,
		nil,
		indexexecutor.Config{
			LeaseDuration:     time.Minute,
			HeartbeatInterval: 5 * time.Second,
			LeaseOwner:        "integration-asynq-worker",
			ArtifactValidator: validator,
			ArtifactIngestor:  astStore,
		},
	)
	if err != nil {
		t.Fatalf("New executor handler: %v", err)
	}
	coreHandler, err := indexcore.NewHandler(pool.Pool, subjobStore, nil, indexcore.Config{
		LeaseDuration: time.Minute,
		LeaseOwner:    "integration-core-worker",
	})
	if err != nil {
		t.Fatalf("New core handler: %v", err)
	}

	server := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency:     1,
		Queues:          map[string]int{asynqueues.QueueIndexCore: 1},
		ShutdownTimeout: 2 * time.Second,
		Logger:          &asynqbridge.SlogLogger{Base: slog.New(slog.NewTextHandler(io.Discard, nil))},
	})
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueIndexCore, func(ctx context.Context, task *asynq.Task) error {
		payload, err := indexsubjobtask.Unmarshal(task.Payload())
		if err != nil || payload.Layer == indexsubjobtask.LayerASTTreeSitter {
			return executorHandler.Handle(ctx, task)
		}
		return coreHandler.Handle(ctx, task)
	})
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.Run(mux)
	}()
	select {
	case err := <-serverDone:
		t.Fatalf("asynq server exited before processing tasks: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	defer func() {
		server.Stop()
		server.Shutdown()
		select {
		case err := <-serverDone:
			if err != nil && !strings.Contains(err.Error(), "Server closed") {
				t.Logf("asynq server stopped: %v", err)
			}
		default:
		}
	}()

	dispatcher := indexsubjobs.NewDispatcher(subjobStore, client)
	waitForIndexJobCompleted(t, ctx, pool, dispatcher, jobID, orgID, repoID)

	var graphStatus, manifestStatus string
	var subjobSucceeded, revisionRows, factRows, edgeRows int
	if err := pool.QueryRow(ctx, `
		SELECT status::text
		FROM "CodeGraphIndex"
		WHERE "orgId" = $1 AND "repoId" = $2 AND "workspaceId" = $3 AND "sourceRevision" = $4 AND "commitHash" = $5
	`, orgID, repoID, workspaceID, revision, commit).Scan(&graphStatus); err != nil {
		t.Fatalf("query CodeGraphIndex: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexManifest" WHERE "indexJobId" = $1`, jobID).Scan(&manifestStatus); err != nil {
		t.Fatalf("query manifest: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM "CodeIntelIndexSubjob"
		WHERE "repoIndexingJobId" = $1 AND status = 'SUCCEEDED'
	`, jobID).Scan(&subjobSucceeded); err != nil {
		t.Fatalf("query succeeded subjobs: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM "CodeGraphRevision"
		WHERE "orgId" = $1 AND "repoId" = $2 AND "workspaceId" = $3 AND revision = $4
	`, orgID, repoID, workspaceID, revision).Scan(&revisionRows); err != nil {
		t.Fatalf("query CodeGraphRevision: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphSemanticFact" WHERE "orgId" = $1 AND "repoId" = $2`, orgID, repoID).Scan(&factRows); err != nil {
		t.Fatalf("query graph facts: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "CodeGraphSemanticEdge" WHERE "orgId" = $1 AND "repoId" = $2`, orgID, repoID).Scan(&edgeRows); err != nil {
		t.Fatalf("query graph edges: %v", err)
	}
	if graphStatus != "READY" || manifestStatus != "READY" || subjobSucceeded != 3 || revisionRows != 1 || factRows == 0 || edgeRows == 0 {
		t.Fatalf("queued Rust AST activation state graph=%s manifest=%s succeeded=%d revisions=%d facts=%d edges=%d",
			graphStatus, manifestStatus, subjobSucceeded, revisionRows, factRows, edgeRows)
	}
}

func seedOrgRepoJob(t *testing.T, ctx context.Context, pool *db.Pool, suffix string) (int32, string, int32, string) {
	t.Helper()
	var orgID int32
	workspaceID := "atom-ws-" + suffix
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "atomWorkspaceId", "updatedAt")
		VALUES ($1, $2, $3, NOW())
		RETURNING id
	`, "index-subjob-org-"+suffix, "index-subjob-"+suffix, workspaceID).Scan(&orgID); err != nil {
		t.Fatalf("insert org: %v", err)
	}
	repoMetadata, _ := json.Marshal(map[string]any{
		"branches": []string{"main", "release"},
	})
	var repoID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
			name, "isFork", "isArchived",
			"cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
			metadata, "orgId", "updatedAt", "defaultBranch"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW(), 'main')
		RETURNING id
	`, "repo-"+suffix, "https://example.com/repo-"+suffix+".git",
		"ext-"+suffix, "github", "https://github.com", string(repoMetadata), orgID).Scan(&repoID); err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	jobID := "job-" + suffix
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, "updatedAt")
		VALUES ($1, $2, 'INDEX'::"RepoIndexingJobType", 'IN_PROGRESS'::"RepoIndexingJobStatus", NOW())
	`, jobID, repoID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	return orgID, workspaceID, repoID, jobID
}

func seedManifest(t *testing.T, ctx context.Context, pool *db.Pool, jobID string, orgID int32, workspaceID string, repoID int32, branch string, commit string) {
	t.Helper()
	manifestID := fmt.Sprintf("manifest-%s-%s-%s-%s", jobID, workspaceID, branch, commit[:12])
	if _, err := pool.Exec(ctx, `
		INSERT INTO "RepoIndexManifest" (
			id, status, "workspaceId", branch, "commitHash", "fileCount",
			"updatedAt", "orgId", "repoId", "indexJobId"
		)
		VALUES (
			$1, 'PENDING'::"RepoIndexManifestStatus", $2, $3, $4, 1,
			NOW(), $5, $6, $7
		)
	`, manifestID, workspaceID, branch, commit, orgID, repoID, jobID); err != nil {
		t.Fatalf("seed RepoIndexManifest %s: %v", branch, err)
	}
}

func planRevision(workspaceID, branch, commit string) *codeintelv1.IndexPlanRevision {
	return &codeintelv1.IndexPlanRevision{
		WorkspaceId:      workspaceID,
		Branch:           branch,
		Revision:         branch,
		CommitHash:       commit,
		RunAstTreeSitter: true,
		RunGraphMerge:    true,
		RunActivate:      true,
		ScipProjects: []*codeintelv1.SCIPProjectPlan{{
			Language:        "go",
			ProjectRoot:     "",
			Indexer:         "scip-go",
			ScipWorkerClass: "go",
		}},
	}
}

func createSubjobFromPayload(payload indexsubjobtask.Payload) indexsubjobs.CreateInput {
	return indexsubjobs.CreateInput{
		ID:                payload.SubjobID,
		RepoIndexingJobID: payload.RepoIndexingJobID,
		OrgID:             payload.OrgID,
		WorkspaceID:       payload.WorkspaceID,
		RepoID:            payload.RepoID,
		Branch:            payload.Branch,
		Revision:          payload.Revision,
		CommitHash:        payload.CommitHash,
		Layer:             indexsubjobs.Layer(payload.Layer),
		Language:          payload.Language,
		ProjectRoot:       payload.ProjectRoot,
		Indexer:           payload.Indexer,
		WorkerClass:       payload.WorkerClass,
		QueueName:         payload.QueueName,
	}
}

func coreLayerPayload(base indexsubjobtask.Payload, subjobID string, layer indexsubjobtask.Layer) indexsubjobtask.Payload {
	base.SubjobID = subjobID
	base.Layer = layer
	base.Language = nil
	base.ProjectRoot = nil
	base.Indexer = nil
	base.WorkerClass = "core"
	base.QueueName = "codeintel-index-core"
	base.Attempt = 1
	return base
}

func redisURLForExecutorE2E() string {
	if raw := strings.TrimSpace(os.Getenv("CODEINTEL_TEST_REDIS_URL")); raw != "" {
		return raw
	}
	return "redis://127.0.0.1:6380/14"
}

func requireLoopbackRedisOrFatal(t *testing.T, raw string) {
	t.Helper()
	if !strings.Contains(raw, "127.0.0.1") && !strings.Contains(raw, "localhost") && !strings.Contains(raw, "[::1]") {
		t.Fatalf("refusing destructive queue cleanup against non-loopback Redis URL %q", raw)
	}
}

func cleanAsynqQueue(t *testing.T, opt asynq.RedisConnOpt, queue string) {
	t.Helper()
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	for label, fn := range map[string]func(string) (int, error){
		"pending":   inspector.DeleteAllPendingTasks,
		"scheduled": inspector.DeleteAllScheduledTasks,
		"retry":     inspector.DeleteAllRetryTasks,
		"archived":  inspector.DeleteAllArchivedTasks,
		"completed": inspector.DeleteAllCompletedTasks,
	} {
		if _, err := fn(queue); err != nil {
			if strings.Contains(err.Error(), "queue "+strconv.Quote(queue)+" does not exist") {
				continue
			}
			t.Fatalf("clean asynq %s tasks for %s: %v", label, queue, err)
		}
	}
}

func flushRedisDBForTest(t *testing.T, rawURL string) {
	t.Helper()
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("parse Redis URL for flush: %v", err)
	}
	client := redis.NewClient(opts)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flush isolated Redis DB: %v", err)
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen free TCP addr: %v", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		t.Fatalf("close free TCP addr listener: %v", err)
	}
	return addr
}

func startRustExecutorForTest(t *testing.T, ctx context.Context, listenAddr, dataRoot, artifactRoot string) func() {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	codeintelRoot := filepath.Clean(filepath.Join(wd, "..", ".."))
	args := []string{
		"executor",
		"--listen-addr", listenAddr,
		"--data-cache-dir", dataRoot,
		"--artifact-root", artifactRoot,
		"--scip-timeout-seconds", "5",
	}
	if zoektBinary := locateOrBuildZoektBinary(t); zoektBinary != "" {
		args = append(args, "--zoekt-binary", zoektBinary)
	}
	var cmd *exec.Cmd
	if bin := strings.TrimSpace(os.Getenv("CODEINTEL_RUST_EXECUTOR_BIN")); bin != "" {
		cmd = exec.CommandContext(ctx, bin, args...)
	} else {
		cargoArgs := append([]string{"run", "--quiet", "--manifest-path", filepath.Join(codeintelRoot, "indexer-rs", "Cargo.toml"), "--"}, args...)
		cmd = exec.CommandContext(ctx, "cargo", cargoArgs...)
	}
	cmd.Dir = codeintelRoot
	cmd.Env = append(os.Environ(),
		"CODEINTEL_ZOEKT_STORAGE_LAYOUT=",
		"CODEINTEL_ZOEKT_EFS_ROOT=",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start Rust executor: %v\n%s", err, output.String())
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	waitForTCPReady(t, listenAddr, done, &output)
	return func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Logf("Rust executor did not exit after kill; output:\n%s", output.String())
		}
	}
}

func waitForTCPReady(t *testing.T, addr string, done <-chan error, output *bytes.Buffer) {
	t.Helper()
	deadline := time.After(60 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			t.Fatalf("Rust executor exited before listening on %s: %v\n%s", addr, err, output.String())
		case <-deadline:
			t.Fatalf("Rust executor did not listen on %s before timeout\n%s", addr, output.String())
		case <-tick.C:
			conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return
			}
		}
	}
}

func waitForIndexJobCompleted(t *testing.T, ctx context.Context, pool *db.Pool, dispatcher *indexsubjobs.Dispatcher, jobID string, orgID int32, repoID int32) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := dispatcher.RequeueExpiredAndDispatch(ctx, time.Now().UTC(), 20, 20); err != nil {
			t.Fatalf("dispatch subjobs: %v", err)
		}
		var status string
		if err := pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexingJob" WHERE id = $1 AND "repoId" = $2`, jobID, repoID).Scan(&status); err != nil {
			t.Fatalf("query RepoIndexingJob: %v", err)
		}
		if status == "COMPLETED" {
			return
		}
		var failedCount int
		var lastFailure string
		if err := pool.QueryRow(ctx, `
			SELECT COUNT(*), COALESCE(MAX("errorMessage"), '')
			FROM "CodeIntelIndexSubjob"
			WHERE "repoIndexingJobId" = $1
			  AND "orgId" = $2
			  AND "repoId" = $3
			  AND status = 'FAILED'
		`, jobID, orgID, repoID).Scan(&failedCount, &lastFailure); err != nil {
			t.Fatalf("query failed subjobs: %v", err)
		}
		if failedCount > 0 {
			t.Fatalf("index job %s has failed subjobs: %s", jobID, lastFailure)
		}
		time.Sleep(200 * time.Millisecond)
	}
	var rows []struct {
		ID     string
		Layer  string
		Status string
		Error  string
	}
	pgRows, err := pool.Query(ctx, `
		SELECT id, layer::text, status::text, COALESCE("errorMessage", '')
		FROM "CodeIntelIndexSubjob"
		WHERE "repoIndexingJobId" = $1
		ORDER BY "createdAt", id
	`, jobID)
	if err != nil {
		t.Fatalf("query subjobs after timeout: %v", err)
	}
	defer pgRows.Close()
	for pgRows.Next() {
		var row struct {
			ID     string
			Layer  string
			Status string
			Error  string
		}
		if err := pgRows.Scan(&row.ID, &row.Layer, &row.Status, &row.Error); err != nil {
			t.Fatalf("scan subjob after timeout: %v", err)
		}
		rows = append(rows, row)
	}
	t.Fatalf("index job %s did not complete before timeout; subjobs=%+v", jobID, rows)
}

func cancelStaleIntegrationDispatchRows(t *testing.T, ctx context.Context, pool *db.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob"
		SET status = 'CANCELED',
		    "leaseOwner" = NULL,
		    "attemptId" = NULL,
		    "leaseExpiresAt" = NULL,
		    "updatedAt" = NOW()
		WHERE "repoIndexingJobId" LIKE 'job-%'
		  AND status IN ('QUEUED', 'RETRYING', 'CLAIMED', 'RUNNING', 'ARTIFACT_WRITTEN', 'VALIDATING')
	`); err != nil {
		t.Fatalf("cancel stale integration subjobs: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE "RepoIndexingJob"
		SET status = 'FAILED'::"RepoIndexingJobStatus",
		    "completedAt" = COALESCE("completedAt", NOW()),
		    "updatedAt" = NOW()
		WHERE id LIKE 'job-%'
		  AND status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
	`); err != nil {
		t.Fatalf("cancel stale integration jobs: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM "CodeIntelIndexSubjobDispatchLock"`); err != nil {
		t.Fatalf("clear stale dispatch lock: %v", err)
	}
}

func mustWriteIntegration(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustSCIPArtifact(t *testing.T, createSymbol, handlerSymbol string) []byte {
	t.Helper()
	index := &scippb.Index{
		Documents: []*scippb.Document{
			{
				Language:     "typescript",
				RelativePath: "src/orders/createOrder.ts",
				Occurrences: []*scippb.Occurrence{{
					Range:          []int32{0, 22, 33},
					Symbol:         createSymbol,
					SymbolRoles:    int32(scippb.SymbolRole_DEFINITION),
					SyntaxKind:     scippb.SyntaxKind_IDENTIFIER_FUNCTION_DEFINITION,
					EnclosingRange: []int32{0, 0, 2, 1},
				}},
				Symbols: []*scippb.SymbolInformation{{
					Symbol:      createSymbol,
					Kind:        scippb.SymbolInformation_FUNCTION,
					DisplayName: "createOrder",
				}},
			},
			{
				Language:     "typescript",
				RelativePath: "src/routes/internalOrders.ts",
				Occurrences: []*scippb.Occurrence{
					{
						Range:          []int32{0, 22, 29},
						Symbol:         handlerSymbol,
						SymbolRoles:    int32(scippb.SymbolRole_DEFINITION),
						SyntaxKind:     scippb.SyntaxKind_IDENTIFIER_FUNCTION_DEFINITION,
						EnclosingRange: []int32{0, 0, 2, 1},
					},
					{
						Range:       []int32{1, 9, 20},
						Symbol:      createSymbol,
						SymbolRoles: int32(scippb.SymbolRole_READ_ACCESS),
						SyntaxKind:  scippb.SyntaxKind_IDENTIFIER_FUNCTION,
					},
				},
				Symbols: []*scippb.SymbolInformation{{
					Symbol: handlerSymbol,
					Kind:   scippb.SymbolInformation_FUNCTION,
					Relationships: []*scippb.Relationship{{
						Symbol:      createSymbol,
						IsReference: true,
					}},
					DisplayName: "handler",
				}},
			},
		},
	}
	raw, err := proto.Marshal(index)
	if err != nil {
		t.Fatalf("marshal SCIP: %v", err)
	}
	return raw
}

type integrationRunner struct {
	result indexexecutor.Result
}

func (r *integrationRunner) Execute(context.Context, indexexecutor.Job) (indexexecutor.Result, error) {
	return r.result, nil
}

func writeScopedArtifact(t *testing.T, root string, orgID int32, repoID int32, workspaceID, branch, commit, subjobID string, content []byte) (tempPath, finalPath, sha string) {
	t.Helper()
	base := filepath.Join(root, fmt.Sprint(orgID), fmt.Sprint(repoID), artifactScopeSegmentForTest(workspaceID), artifactScopeSegmentForTest(branch), commit)
	tempPath = filepath.Join(base, "tmp", subjobID+".scip.tmp")
	finalPath = filepath.Join(base, "scip", subjobID+".scip")
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		t.Fatalf("mkdir temp artifact: %v", err)
	}
	if err := os.WriteFile(tempPath, content, 0o644); err != nil {
		t.Fatalf("write temp artifact: %v", err)
	}
	sum := sha256.Sum256(content)
	return tempPath, finalPath, "sha256:" + fmt.Sprintf("%x", sum[:])
}

func writeScopedArtifactWithLayer(t *testing.T, root string, payload indexsubjobtask.Payload, layerDir, ext string, content []byte) (tempPath, finalPath, sha string) {
	t.Helper()
	if payload.WorkspaceID == nil {
		t.Fatal("payload workspace required")
	}
	base := filepath.Join(root, fmt.Sprint(payload.OrgID), fmt.Sprint(payload.RepoID), artifactScopeSegmentForTest(*payload.WorkspaceID), artifactScopeSegmentForTest(payload.Branch), payload.CommitHash)
	tempPath = filepath.Join(base, "tmp", payload.SubjobID+"."+ext+".tmp")
	finalPath = filepath.Join(base, layerDir, payload.SubjobID+"."+ext)
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		t.Fatalf("mkdir temp artifact: %v", err)
	}
	if err := os.WriteFile(tempPath, content, 0o644); err != nil {
		t.Fatalf("write temp artifact: %v", err)
	}
	sum := sha256.Sum256(content)
	return tempPath, finalPath, "sha256:" + fmt.Sprintf("%x", sum[:])
}

func astGraphPayloadForTest(t *testing.T, payload indexsubjobtask.Payload) []byte {
	t.Helper()
	if payload.WorkspaceID == nil {
		t.Fatal("payload workspace required")
	}
	schemaVersion := int64(1)
	builderVersion := "codeintel-code-graph-v7"
	repoVID := codeGraphVIDForTest(payload, schemaVersion, builderVersion, "repo", fmt.Sprintf("repo:%d", payload.RepoID))
	handlerVID := codeGraphVIDForTest(payload, schemaVersion, builderVersion, "function", "src/routes/orders.ts#handler")
	source := "tree-sitter-typescript"
	statements := graphschema.RenderSnapshotStatements(graphschema.CodeGraphSnapshot{
		OrgID:          int64(payload.OrgID),
		WorkspaceID:    *payload.WorkspaceID,
		RepoID:         int64(payload.RepoID),
		Revision:       payload.Revision,
		CommitHash:     payload.CommitHash,
		SchemaVersion:  schemaVersion,
		BuilderVersion: builderVersion,
		Vertices: []graphschema.CodeGraphVertex{
			{
				VID:  repoVID,
				Kind: "repo",
				Properties: map[string]graphschema.CodeGraphPrimitive{
					"kind":           "repo",
					"orgId":          int64(payload.OrgID),
					"workspaceId":    *payload.WorkspaceID,
					"repoId":         int64(payload.RepoID),
					"revision":       payload.Revision,
					"commitHash":     payload.CommitHash,
					"schemaVersion":  schemaVersion,
					"builderVersion": builderVersion,
					"key":            fmt.Sprintf("repo:%d", payload.RepoID),
					"label":          "repo",
					"confidence":     1.0,
					"confidenceTier": "EXTRACTED",
					"source":         source,
					"provenance":     "tree-sitter",
				},
			},
			{
				VID:  handlerVID,
				Kind: "function",
				Properties: map[string]graphschema.CodeGraphPrimitive{
					"kind":             "function",
					"orgId":            int64(payload.OrgID),
					"workspaceId":      *payload.WorkspaceID,
					"repoId":           int64(payload.RepoID),
					"revision":         payload.Revision,
					"commitHash":       payload.CommitHash,
					"schemaVersion":    schemaVersion,
					"builderVersion":   builderVersion,
					"key":              "src/routes/orders.ts#handler",
					"label":            "handler",
					"path":             "src/routes/orders.ts",
					"language":         "typescript",
					"confidence":       0.95,
					"confidenceTier":   "EXTRACTED",
					"evidenceFilePath": "src/routes/orders.ts",
					"startLine":        int64(3),
					"endLine":          int64(7),
					"source":           source,
					"provenance":       "tree-sitter",
				},
			},
		},
		Edges: []graphschema.CodeGraphEdge{{
			FromVID: repoVID,
			ToVID:   handlerVID,
			Kind:    "DEFINES",
			Rank:    graphschema.EdgeRank(repoVID + "->" + handlerVID + ":DEFINES:" + source),
			Properties: map[string]graphschema.CodeGraphPrimitive{
				"kind":             "DEFINES",
				"orgId":            int64(payload.OrgID),
				"workspaceId":      *payload.WorkspaceID,
				"repoId":           int64(payload.RepoID),
				"revision":         payload.Revision,
				"commitHash":       payload.CommitHash,
				"schemaVersion":    schemaVersion,
				"builderVersion":   builderVersion,
				"confidence":       0.95,
				"confidenceTier":   "EXTRACTED",
				"evidenceFilePath": "src/routes/orders.ts",
				"startLine":        int64(3),
				"endLine":          int64(7),
				"source":           source,
				"provenance":       "tree-sitter",
				"context":          "definition",
			},
		}},
	})
	raw, err := json.Marshal(map[string]any{
		"orgId":          payload.OrgID,
		"workspaceId":    *payload.WorkspaceID,
		"repoId":         payload.RepoID,
		"branch":         payload.Branch,
		"revision":       payload.Revision,
		"commitHash":     payload.CommitHash,
		"schemaVersion":  schemaVersion,
		"builderVersion": builderVersion,
		"indexJobId":     payload.RepoIndexingJobID,
		"manifestId":     "",
		"source":         "syntactic-ast",
		"statements":     statements,
	})
	if err != nil {
		t.Fatalf("marshal AST graph payload: %v", err)
	}
	return raw
}

func codeGraphVIDForTest(payload indexsubjobtask.Payload, schemaVersion int64, builderVersion, kind, key string) string {
	workspaceID := ""
	if payload.WorkspaceID != nil {
		workspaceID = *payload.WorkspaceID
	}
	builderHash := hashPartsForTest([]string{builderVersion}, 8)
	keyHash := hashPartsForTest([]string{
		fmt.Sprint(payload.OrgID),
		workspaceID,
		fmt.Sprint(payload.RepoID),
		payload.CommitHash,
		fmt.Sprint(schemaVersion),
		builderVersion,
		kind,
		key,
	}, 32)
	return fmt.Sprintf("cg:o%d:w%s:r%d:c%s:s%d:b%s:%s:%s",
		payload.OrgID,
		hashPartsForTest([]string{workspaceID}, 8),
		payload.RepoID,
		payload.CommitHash[:12],
		schemaVersion,
		builderHash,
		kind,
		keyHash,
	)
}

func hashPartsForTest(parts []string, n int) string {
	h := sha256.New()
	for i, part := range parts {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(part))
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:n]
}

type fakeRenderedGraphStore struct {
	calls int
}

func (s *fakeRenderedGraphStore) WriteRenderedStatements(_ context.Context, input graphstore.RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error) {
	s.calls++
	vertices, edges, err := graphstore.ValidateRenderedStatementWrite(input)
	if err != nil {
		return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
	}
	return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusReady, VertexCount: vertices, EdgeCount: edges}, nil
}

func (s *fakeRenderedGraphStore) WriteSnapshot(context.Context, graphschema.CodeGraphSnapshot) (graphschema.CodeGraphWriteResult, error) {
	return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusReady}, nil
}

func (s *fakeRenderedGraphStore) MarkSnapshotForDeletion(context.Context, graphschema.CodeGraphDeleteInput) error {
	return nil
}

func artifactScopeSegmentForTest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "s-" + fmt.Sprintf("%x", sum[:])[:16]
}
