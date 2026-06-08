// Package repoindexmanager hosts the codeintel-backend worker
// for the repo-index-queue. This file lands the status-
// transition helpers (PENDING → IN_PROGRESS → COMPLETED/FAILED)
// on the RepoIndexingJob row. The actual worker handler that
// performs the per-type work (INDEX clones + indexes the repo,
// CLEANUP drops orphaned data, REMOVE_INDEX tombstones the
// index) lands in Phase C.2+.
//
// Direct port of the status-transition surface of
// repoIndexManager.ts. Keeps the same status-guard semantics so
// a job that's already in a terminal state can't be
// double-processed.
package repoindexmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgxQuerier is the narrow pgx surface the store uses. Same
// shape as connectionmanager.pgxQuerier — *pgxpool.Pool
// satisfies it directly; pgxmock satisfies it for unit tests.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store wraps the pgx surface with typed RepoIndexingJob
// lifecycle operations. New worker call sites use the scoped
// transition helpers below so a stale or forged queue payload
// cannot move a job row across org/repo/type boundaries.
type Store struct {
	db pgxQuerier
}

type JobScope struct {
	OrgID  int32
	RepoID int32
	Type   string
}

type RepoIndexScope struct {
	OrgID         int32
	WorkspaceID   string
	DefaultBranch string
	Metadata      []byte
}

type pendingManifestInput struct {
	ID          string
	JobID       string
	OrgID       int32
	RepoID      int32
	WorkspaceID string
	Branch      string
	CommitHash  string
	FileCount   int32
	Plan        *deltaReindexPlan
	Files       []manifestFileRow
}

type semanticIndexHealth struct {
	SCIPFound             bool
	SCIPSymbolCount       int32
	SCIPOccurrenceCount   int32
	SCIPRelationshipCount int32
	GraphFound            bool
	GraphAnchorCount      int32
	GraphLinkedEdgeCount  int32
}

func (h semanticIndexHealth) NeedsRepair() bool {
	if !h.SCIPFound || h.SCIPSymbolCount+h.SCIPOccurrenceCount+h.SCIPRelationshipCount == 0 {
		return true
	}
	if !h.GraphFound || h.GraphAnchorCount == 0 || h.GraphLinkedEdgeCount == 0 {
		return true
	}
	return false
}

// NewStore constructs a Store. The caller (worker handler / API
// route) is responsible for the pool's lifecycle.
func NewStore(db pgxQuerier) *Store {
	return &Store{db: db}
}

// ErrJobInTerminalState is returned when MarkInProgress is
// invoked on a job whose status is already COMPLETED or FAILED
// (or the row doesn't exist). The status guard mirrors the
// legacy "skip if not PENDING/IN_PROGRESS" branch.
var ErrJobInTerminalState = errors.New("repoindexmanager: job already in terminal state or missing")

func (s *Store) FetchJobScope(ctx context.Context, jobID string) (JobScope, error) {
	var scope JobScope
	err := s.db.QueryRow(ctx, `
		SELECT r."orgId", j."repoId", j.type::text
		FROM "RepoIndexingJob" j
		JOIN "Repo" r ON r.id = j."repoId"
		WHERE j.id = $1
	`, jobID).Scan(&scope.OrgID, &scope.RepoID, &scope.Type)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobScope{}, ErrJobInTerminalState
	}
	if err != nil {
		return JobScope{}, fmt.Errorf("FetchJobScope: %w", err)
	}
	return scope, nil
}

// MarkInProgress transitions PENDING / IN_PROGRESS jobs to
// IN_PROGRESS and stamps updatedAt. Returns ErrJobInTerminalState
// when zero rows match — the legacy treats this as "already
// done" and silently succeeds; the Go port surfaces a typed
// sentinel so the caller can choose between skip vs. fail.
func (s *Store) MarkInProgress(ctx context.Context, jobID string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE "RepoIndexingJob"
		SET status = 'IN_PROGRESS', "updatedAt" = NOW()
		WHERE id = $1 AND status IN ('PENDING', 'IN_PROGRESS')
	`, jobID)
	if err != nil {
		return fmt.Errorf("MarkInProgress: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobInTerminalState
	}
	return nil
}

func (s *Store) MarkInProgressScoped(ctx context.Context, jobID string, orgID, repoID int32, jobType string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE "RepoIndexingJob" j
		SET status = 'IN_PROGRESS', "updatedAt" = NOW()
		FROM "Repo" r
		WHERE j.id = $1
		  AND j."repoId" = $2
		  AND j.type = $3::"RepoIndexingJobType"
		  AND j.status IN ('PENDING'::"RepoIndexingJobStatus", 'IN_PROGRESS'::"RepoIndexingJobStatus")
		  AND r.id = j."repoId"
		  AND r."orgId" = $4
	`, jobID, repoID, jobType, orgID)
	if err != nil {
		return fmt.Errorf("MarkInProgressScoped: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobInTerminalState
	}
	return nil
}

// MarkCompleted finalises the row at COMPLETED with the
// supplied completion metadata. The IndexedAt / IndexedCommitHash
// projection on the Repo row is updated separately by
// callers when relevant (an INDEX job stamps it; CLEANUP /
// REMOVE_INDEX don't).
//
// Mirrors the legacy onJobCompleted (repoIndexManager.ts:889):
// status=COMPLETED, completedAt=NOW(), updatedAt=NOW().
func (s *Store) MarkCompleted(ctx context.Context, jobID string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE "RepoIndexingJob"
		SET status = 'COMPLETED',
		    "completedAt" = NOW(),
		    "updatedAt"   = NOW()
		WHERE id = $1
	`, jobID)
	if err != nil {
		return fmt.Errorf("MarkCompleted: %w", err)
	}
	return nil
}

func (s *Store) MarkCompletedScoped(ctx context.Context, jobID string, orgID, repoID int32, jobType string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE "RepoIndexingJob" j
		SET status = 'COMPLETED',
		    "completedAt" = NOW(),
		    "updatedAt"   = NOW()
		FROM "Repo" r
		WHERE j.id = $1
		  AND j."repoId" = $2
		  AND j.type = $3::"RepoIndexingJobType"
		  AND j.status IN ('PENDING'::"RepoIndexingJobStatus", 'IN_PROGRESS'::"RepoIndexingJobStatus")
		  AND r.id = j."repoId"
		  AND r."orgId" = $4
	`, jobID, repoID, jobType, orgID)
	if err != nil {
		return fmt.Errorf("MarkCompletedScoped: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobInTerminalState
	}
	return nil
}

// MarkFailed records the failure with errMsg. The asynq retry
// policy on the consumer side decides whether to re-queue;
// the row itself goes to FAILED so an operator query can find
// it regardless of the queue state.
//
// Mirrors the legacy onJobMaybeFailed (repoIndexManager.ts:988):
// status=FAILED, errorMessage=…, completedAt=NOW(), updatedAt=NOW().
func (s *Store) MarkFailed(ctx context.Context, jobID, errMsg string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE "RepoIndexingJob"
		SET status         = 'FAILED',
		    "errorMessage" = $2,
		    "completedAt"  = NOW(),
		    "updatedAt"    = NOW()
		WHERE id = $1
	`, jobID, errMsg)
	if err != nil {
		return fmt.Errorf("MarkFailed: %w", err)
	}
	return nil
}

func (s *Store) MarkFailedScoped(ctx context.Context, jobID string, orgID, repoID int32, jobType, errMsg string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE "RepoIndexingJob" j
		SET status         = 'FAILED',
		    "errorMessage" = $5,
		    "completedAt"  = NOW(),
		    "updatedAt"    = NOW()
		FROM "Repo" r
		WHERE j.id = $1
		  AND j."repoId" = $2
		  AND j.type = $3::"RepoIndexingJobType"
		  AND j.status IN ('PENDING'::"RepoIndexingJobStatus", 'IN_PROGRESS'::"RepoIndexingJobStatus")
		  AND r.id = j."repoId"
		  AND r."orgId" = $4
	`, jobID, repoID, jobType, orgID, errMsg)
	if err != nil {
		return fmt.Errorf("MarkFailedScoped: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobInTerminalState
	}
	return nil
}

// RefreshRepoLatestIndexingJobStatus mirrors the legacy
// refreshRepoLatestIndexingJobStatus (repoIndexManager.ts:224)
// helper — denormalises the Repo.latestIndexingJobStatus column
// from the most recent job row so per-repo dashboards don't
// need a sub-query. Picks the status from the most-recent
// RepoIndexingJob for the repo.
func (s *Store) RefreshRepoLatestIndexingJobStatus(ctx context.Context, repoID int32) error {
	_, err := s.db.Exec(ctx, `
		UPDATE "Repo" r
		SET "latestIndexingJobStatus" = sub.status
		FROM (
		    SELECT j.status
		    FROM   "RepoIndexingJob" j
		    WHERE  j."repoId" = $1
		    ORDER  BY j."createdAt" DESC NULLS LAST, j.id DESC
		    LIMIT  1
		) sub
		WHERE r.id = $1
	`, repoID)
	if err != nil {
		return fmt.Errorf("RefreshRepoLatestIndexingJobStatus: %w", err)
	}
	return nil
}

// RecordSuccessfulIndex persists the post-clone state on the Repo
// row: indexedAt=NOW(), indexedCommitHash=<observed HEAD>. Called
// by the INDEX dispatch path after a successful clone produces a
// real commit hash on disk. This is the column the GET
// /api/repos/{id}/status surface reads + the value the legacy UI
// uses to render "last indexed at".
//
// Direct port of the relevant fragment of legacy
// repoIndexManager.ts (the indexedAt+indexedCommitHash update
// inside onJobCompleted, repoIndexManager.ts:889-920).
func (s *Store) RecordSuccessfulIndex(ctx context.Context, repoID int32, commitHash string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE "Repo"
		SET "indexedAt"         = NOW(),
		    "indexedCommitHash" = $2,
		    "updatedAt"         = NOW()
		WHERE id = $1
	`, repoID, commitHash)
	if err != nil {
		return fmt.Errorf("RecordSuccessfulIndex: %w", err)
	}
	return nil
}

// RecordUsableIndexedRevision refreshes the repo-level indexed projection when
// an INDEX request reuses an already READY manifest instead of producing new
// split-index subjobs. This is required after branch-scoped remove-index clears
// metadata.indexedRevisions but leaves reusable READY manifests/semantic
// indexes for an unchanged revision.
func (s *Store) RecordUsableIndexedRevision(ctx context.Context, orgID, repoID int32, workspaceID, branch, commitHash string) error {
	if orgID <= 0 || repoID <= 0 || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(branch) == "" || strings.TrimSpace(commitHash) == "" {
		return errors.New("repoindexmanager: invalid indexed revision refresh scope")
	}
	_, err := s.db.Exec(ctx, `
		UPDATE "Repo" r
		SET metadata = jsonb_set(
		        COALESCE(r.metadata, '{}'::jsonb),
		        '{indexedRevisions}',
		        COALESCE((
		            SELECT jsonb_agg(branch ORDER BY branch)
		            FROM (
		                SELECT DISTINCT branch
		                FROM (
		                    SELECT jsonb_array_elements_text(COALESCE(r.metadata->'indexedRevisions', '[]'::jsonb)) AS branch
		                    UNION
		                    SELECT m.branch
		                    FROM "RepoIndexManifest" m
		                    WHERE m."orgId" = $1
		                      AND m."repoId" = $2
		                      AND m."workspaceId" = $3
		                      AND m.branch = $4
		                      AND m."commitHash" = $5
		                      AND m.status = 'READY'::"RepoIndexManifestStatus"
		                      AND m."supersededAt" IS NULL
		                ) merged
		                WHERE branch <> ''
		            ) ready
		        ), '[]'::jsonb),
		        true
		    ),
		    "indexedAt" = CURRENT_TIMESTAMP,
		    "indexedCommitHash" = $5,
		    "updatedAt" = CURRENT_TIMESTAMP
		WHERE r.id = $2 AND r."orgId" = $1
	`, orgID, repoID, workspaceID, branch, commitHash)
	if err != nil {
		return fmt.Errorf("RecordUsableIndexedRevision: %w", err)
	}
	return nil
}

func (s *Store) FetchRepoIndexScope(ctx context.Context, repoID int32) (RepoIndexScope, error) {
	var scope RepoIndexScope
	err := s.db.QueryRow(ctx, `
		SELECT r."orgId",
		       COALESCE(o."atomWorkspaceId", o.domain, 'org-' || o.id::text) AS "workspaceId",
		       COALESCE(NULLIF(r."defaultBranch", ''), 'main') AS "defaultBranch",
		       COALESCE(r.metadata, '{}'::jsonb)::text AS metadata
		FROM "Repo" r
		JOIN "Org" o ON o.id = r."orgId"
		WHERE r.id = $1
	`, repoID).Scan(&scope.OrgID, &scope.WorkspaceID, &scope.DefaultBranch, &scope.Metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoIndexScope{}, ErrRepoNotFound
	}
	if err != nil {
		return RepoIndexScope{}, fmt.Errorf("FetchRepoIndexScope: %w", err)
	}
	return scope, nil
}

func (s *Store) FetchPreviousReadyManifestFiles(ctx context.Context, orgID, repoID int32, workspaceID, branch string) ([]manifestFileRow, error) {
	if orgID <= 0 || repoID <= 0 || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(branch) == "" {
		return nil, errors.New("repoindexmanager: invalid previous manifest scope")
	}
	var manifestID string
	err := s.db.QueryRow(ctx, `
		SELECT id
		FROM "RepoIndexManifest"
		WHERE "orgId" = $1
		  AND "repoId" = $2
		  AND "workspaceId" = $3
		  AND branch = $4
		  AND status = 'READY'::"RepoIndexManifestStatus"
		ORDER BY "activatedAt" DESC NULLS LAST, "updatedAt" DESC, id DESC
		LIMIT 1
	`, orgID, repoID, workspaceID, branch).Scan(&manifestID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("FetchPreviousReadyManifestFiles: %w", err)
	}
	rows, err := s.db.Query(ctx, `
		SELECT path, "contentHash", language, "projectRoot", generated, vendor, test
		FROM "RepoIndexManifestFile"
		WHERE "manifestId" = $1
		ORDER BY path ASC
	`, manifestID)
	if err != nil {
		return nil, fmt.Errorf("FetchPreviousReadyManifestFiles files: %w", err)
	}
	defer rows.Close()
	var out []manifestFileRow
	for rows.Next() {
		var file manifestFileRow
		var language, projectRoot *string
		if err := rows.Scan(&file.Path, &file.ContentHash, &language, &projectRoot, &file.Generated, &file.Vendor, &file.Test); err != nil {
			return nil, fmt.Errorf("FetchPreviousReadyManifestFiles scan: %w", err)
		}
		file.Language = language
		file.ProjectRoot = projectRoot
		out = append(out, file)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("FetchPreviousReadyManifestFiles rows: %w", err)
	}
	return out, nil
}

func (s *Store) FetchSemanticIndexHealth(ctx context.Context, orgID, repoID int32, workspaceID, branch, commitHash string) (semanticIndexHealth, error) {
	if orgID <= 0 || repoID <= 0 || strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(branch) == "" || strings.TrimSpace(commitHash) == "" {
		return semanticIndexHealth{}, errors.New("repoindexmanager: invalid semantic health scope")
	}
	var health semanticIndexHealth
	err := s.db.QueryRow(ctx, `
		WITH scip AS (
		  SELECT TRUE AS found,
		         COALESCE("symbolCount", 0)::int AS symbols,
		         COALESCE("occurrenceCount", 0)::int AS occurrences,
		         COALESCE("relationshipCount", 0)::int AS relationships
		  FROM "CodeIntelIndex"
		  WHERE "orgId" = $1
		    AND "repoId" = $2
		    AND revision = $4
		    AND "commitHash" = $5
		    AND kind = 'SCIP'::"CodeIntelIndexKind"
		    AND status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus")
		  ORDER BY "indexedAt" DESC NULLS LAST, "updatedAt" DESC, id DESC
		  LIMIT 1
		),
		graph AS (
		  SELECT TRUE AS found,
		         COALESCE("anchorCount", 0)::int AS anchors,
		         COALESCE("linkedEdgeCount", 0)::int AS linked_edges
		  FROM "CodeGraphIndex"
		  WHERE "orgId" = $1
		    AND "repoId" = $2
		    AND "workspaceId" = $3
		    AND "sourceRevision" = $4
		    AND "commitHash" = $5
		    AND status = 'READY'::"CodeGraphIndexStatus"
		    AND "supersededAt" IS NULL
		    AND "deleteAfter" IS NULL
		  ORDER BY "indexedAt" DESC NULLS LAST, "updatedAt" DESC, id DESC
		  LIMIT 1
		)
		SELECT COALESCE((SELECT found FROM scip), FALSE),
		       COALESCE((SELECT symbols FROM scip), 0),
		       COALESCE((SELECT occurrences FROM scip), 0),
		       COALESCE((SELECT relationships FROM scip), 0),
		       COALESCE((SELECT found FROM graph), FALSE),
		       COALESCE((SELECT anchors FROM graph), 0),
		       COALESCE((SELECT linked_edges FROM graph), 0)
	`, orgID, repoID, workspaceID, branch, commitHash).Scan(
		&health.SCIPFound,
		&health.SCIPSymbolCount,
		&health.SCIPOccurrenceCount,
		&health.SCIPRelationshipCount,
		&health.GraphFound,
		&health.GraphAnchorCount,
		&health.GraphLinkedEdgeCount,
	)
	if err != nil {
		return semanticIndexHealth{}, fmt.Errorf("FetchSemanticIndexHealth: %w", err)
	}
	return health, nil
}

func (s *Store) InsertPendingManifest(ctx context.Context, in pendingManifestInput) error {
	if in.ID == "" || in.JobID == "" || in.OrgID <= 0 || in.RepoID <= 0 ||
		in.WorkspaceID == "" || in.Branch == "" || in.CommitHash == "" {
		return errors.New("repoindexmanager: invalid pending manifest input")
	}
	planJSON, added, changed, deleted, unchanged := manifestPlanValues(in.Plan)
	zoektStrategy, scipStrategy, graphStrategy, semanticStrategy := "FULL_REPO", "FULL_REPO", "FULL_REPO", "SKIPPED"
	if in.Plan != nil {
		zoektStrategy = in.Plan.Zoekt.Strategy
		scipStrategy = in.Plan.SCIP.Strategy
		graphStrategy = in.Plan.Graph.Strategy
		semanticStrategy = in.Plan.Semantic.Strategy
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO "RepoIndexManifest" (
			id, status, "workspaceId", branch, "commitHash", plan, "fileCount",
			"addedFileCount", "changedFileCount", "deletedFileCount", "unchangedFileCount",
			"updatedAt", "orgId", "repoId", "indexJobId",
			"zoektStrategy", "scipStrategy", "graphStrategy", "semanticStrategy"
		)
		VALUES (
			$1, 'PENDING'::"RepoIndexManifestStatus", $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			NOW(), $11, $12, $13,
			$14, $15, $16, $17
		)
	`, in.ID, in.WorkspaceID, in.Branch, in.CommitHash, planJSON, in.FileCount,
		added, changed, deleted, unchanged, in.OrgID, in.RepoID, in.JobID,
		zoektStrategy, scipStrategy, graphStrategy, semanticStrategy)
	if err != nil {
		return fmt.Errorf("InsertPendingManifest: %w", err)
	}
	if len(in.Files) > 0 {
		if err := s.InsertManifestFiles(ctx, in.ID, in.Files); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) InsertManifestFiles(ctx context.Context, manifestID string, files []manifestFileRow) error {
	if strings.TrimSpace(manifestID) == "" {
		return errors.New("repoindexmanager: manifest id is required")
	}
	const rowsPerChunk = 1000
	for start := 0; start < len(files); start += rowsPerChunk {
		end := start + rowsPerChunk
		if end > len(files) {
			end = len(files)
		}
		chunk := files[start:end]
		var sql strings.Builder
		sql.WriteString(`INSERT INTO "RepoIndexManifestFile" (id, path, "contentHash", language, "projectRoot", generated, vendor, test, artifacts, "manifestId") VALUES `)
		args := make([]any, 0, len(chunk)*10)
		for i, file := range chunk {
			if i > 0 {
				sql.WriteString(", ")
			}
			base := i * 10
			sql.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10))
			args = append(args,
				stableManifestFileID(manifestID, file.Path),
				file.Path,
				file.ContentHash,
				nullableStringPtr(file.Language),
				nullableStringPtr(file.ProjectRoot),
				file.Generated,
				file.Vendor,
				file.Test,
				nil,
				manifestID,
			)
		}
		sql.WriteString(` ON CONFLICT ("manifestId", "path") DO UPDATE SET
			"contentHash" = EXCLUDED."contentHash",
			language = EXCLUDED.language,
			"projectRoot" = EXCLUDED."projectRoot",
			generated = EXCLUDED.generated,
			vendor = EXCLUDED.vendor,
			test = EXCLUDED.test,
			artifacts = EXCLUDED.artifacts`)
		if _, err := s.db.Exec(ctx, sql.String(), args...); err != nil {
			return fmt.Errorf("InsertManifestFiles: %w", err)
		}
	}
	return nil
}

func stableManifestFileID(manifestID, filePath string) string {
	sum := sha256.Sum256([]byte(manifestID + "\x00" + filePath))
	return "rmf_" + hex.EncodeToString(sum[:])[:32]
}

func nullableStringPtr(value *string) any {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return *value
}

// InsertPending inserts a new RepoIndexingJob row in PENDING
// state. Returns the resulting row id (caller-supplied so the
// producer can synthesise a uuid + bind it into the asynq
// task payload before enqueueing).
//
// Mirrors the per-job INSERT inside legacy createJobsForLockedOrg
// (repoIndexManager.ts:392).
func (s *Store) InsertPending(ctx context.Context, jobID string, repoID int32, jobType string) error {
	if jobID == "" {
		return errors.New("repoindexmanager: InsertPending requires jobID")
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, "updatedAt")
		VALUES ($1, $2, $3::"RepoIndexingJobType", 'PENDING', NOW())
	`, jobID, repoID, jobType)
	if err != nil {
		return fmt.Errorf("InsertPending: %w", err)
	}
	return nil
}
