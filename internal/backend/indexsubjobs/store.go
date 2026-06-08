// Package indexsubjobs owns the durable state machine for
// worker-class indexing subjobs.
package indexsubjobs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"codeintel/internal/backend/workerclasses"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Status string

const (
	StatusQueued          Status = "QUEUED"
	StatusClaimed         Status = "CLAIMED"
	StatusRunning         Status = "RUNNING"
	StatusArtifactWritten Status = "ARTIFACT_WRITTEN"
	StatusValidating      Status = "VALIDATING"
	StatusSucceeded       Status = "SUCCEEDED"
	StatusFailed          Status = "FAILED"
	StatusRetrying        Status = "RETRYING"
	StatusCanceled        Status = "CANCELED"
	StatusSkipped         Status = "SKIPPED"
)

type Layer string

const (
	LayerClone         Layer = "CLONE"
	LayerZoekt         Layer = "ZOEKT"
	LayerASTTreeSitter Layer = "AST_TREE_SITTER"
	LayerSCIP          Layer = "SCIP"
	LayerGraphMerge    Layer = "GRAPH_MERGE"
	LayerActivate      Layer = "ACTIVATE"
	LayerRemove        Layer = "REMOVE"
)

type CreateInput struct {
	ID                string
	RepoIndexingJobID string
	CodeIntelIndexID  *string
	OrgID             int32
	WorkspaceID       *string
	RepoID            int32
	Branch            string
	Revision          string
	CommitHash        string
	Layer             Layer
	Language          *string
	ProjectRoot       *string
	Indexer           *string
	WorkerClass       string
	QueueName         string
	MaxAttempts       int32
	InputDigest       *string
	ToolchainDigest   *string
	ImageDigest       *string
}

type DispatchableSubjob struct {
	ID                string
	RepoIndexingJobID string
	OrgID             int32
	WorkspaceID       *string
	RepoID            int32
	Branch            string
	Revision          string
	CommitHash        string
	Layer             Layer
	Language          *string
	ProjectRoot       *string
	Indexer           *string
	WorkerClass       string
	QueueName         string
	Attempt           int32
}

// ClaimScope is the immutable scope copied from a queued task
// payload. Consumers must claim by this scope, not by id alone,
// so a stale or malicious queue message cannot claim another
// tenant/branch's row just because it guessed a subjob id.
type ClaimScope struct {
	ID                string
	RepoIndexingJobID string
	OrgID             int32
	WorkspaceID       *string
	RepoID            int32
	Branch            string
	Revision          string
	CommitHash        string
	Layer             Layer
	Language          *string
	ProjectRoot       *string
	Indexer           *string
	WorkerClass       string
	QueueName         string
	Attempt           int32
}

type Store struct {
	db pgxQuerier
}

const dispatchLockID = "index-subjob-dispatch"

var commitHashPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func NewStore(db pgxQuerier) *Store {
	return &Store{db: db}
}

func (s *Store) TryAcquireDispatchLock(ctx context.Context, owner string, leaseUntil time.Time) (bool, error) {
	if owner == "" {
		return false, errors.New("indexsubjobs: dispatch lock owner is required")
	}
	var locked int
	err := s.db.QueryRow(ctx, `
		INSERT INTO "CodeIntelIndexSubjobDispatchLock" (
		    id, "leaseOwner", "leaseExpiresAt", "updatedAt"
		)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (id) DO UPDATE
		SET "leaseOwner" = EXCLUDED."leaseOwner",
		    "leaseExpiresAt" = EXCLUDED."leaseExpiresAt",
		    "updatedAt" = NOW()
		WHERE "CodeIntelIndexSubjobDispatchLock"."leaseExpiresAt" < NOW()
		   OR "CodeIntelIndexSubjobDispatchLock"."leaseOwner" = EXCLUDED."leaseOwner"
		RETURNING 1
	`, dispatchLockID, owner, leaseUntil).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("TryAcquireDispatchLock: %w", err)
	}
	return locked == 1, nil
}

func (s *Store) ReleaseDispatchLock(ctx context.Context, owner string) error {
	if owner == "" {
		return errors.New("indexsubjobs: dispatch lock owner is required")
	}
	_, err := s.db.Exec(ctx, `
		DELETE FROM "CodeIntelIndexSubjobDispatchLock"
		WHERE id = $1
		  AND "leaseOwner" = $2
	`, dispatchLockID, owner)
	if err != nil {
		return fmt.Errorf("ReleaseDispatchLock: %w", err)
	}
	return nil
}

var ErrInvalidCreateInput = errors.New("indexsubjobs: invalid create input")
var ErrScopeNotReady = errors.New("indexsubjobs: parent job or manifest scope is not ready")

// UpsertQueued inserts one durable subjob. The unique scope index
// makes this idempotent when a planner is retried after a crash.
func (s *Store) UpsertQueued(ctx context.Context, in CreateInput) error {
	return s.upsertWithStatus(ctx, in, StatusQueued, "", "")
}

func (s *Store) UpsertSkipped(ctx context.Context, in CreateInput, errorCode, errorMessage string) error {
	if strings.TrimSpace(errorCode) == "" || strings.TrimSpace(errorMessage) == "" {
		return ErrInvalidCreateInput
	}
	return s.upsertWithStatus(ctx, in, StatusSkipped, errorCode, errorMessage)
}

func (s *Store) upsertWithStatus(ctx context.Context, in CreateInput, status Status, errorCode, errorMessage string) error {
	if in.ID == "" || in.RepoIndexingJobID == "" || in.OrgID <= 0 || in.RepoID <= 0 ||
		in.WorkspaceID == nil || *in.WorkspaceID == "" ||
		in.Branch == "" || in.Revision == "" || in.CommitHash == "" || in.Layer == "" ||
		in.WorkerClass == "" || in.QueueName == "" {
		return ErrInvalidCreateInput
	}
	if !commitHashPattern.MatchString(in.CommitHash) {
		return ErrInvalidCreateInput
	}
	if !validLayer(in.Layer) {
		return ErrInvalidCreateInput
	}
	class, ok := workerclasses.ByName(in.WorkerClass)
	if !ok || class.QueueName != in.QueueName {
		return ErrInvalidCreateInput
	}
	if in.Layer == LayerSCIP {
		if in.Language == nil || *in.Language == "" || in.ProjectRoot == nil ||
			in.Indexer == nil || *in.Indexer == "" || !safeProjectRoot(*in.ProjectRoot) {
			return ErrInvalidCreateInput
		}
	} else if in.Language != nil || in.ProjectRoot != nil || in.Indexer != nil {
		return ErrInvalidCreateInput
	}
	maxAttempts := in.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	var completedAt any
	if status == StatusSkipped {
		completedAt = time.Now().UTC()
	} else if status != StatusQueued {
		return ErrInvalidCreateInput
	}
	tag, err := s.db.Exec(ctx, `
		INSERT INTO "CodeIntelIndexSubjob" (
		    id, "repoIndexingJobId", "codeIntelIndexId", "orgId", "workspaceId",
		    "repoId", branch, revision, "commitHash", layer, language,
		    "projectRoot", indexer, "workerClass", "queueName", status,
		    "maxAttempts", "inputDigest", "toolchainDigest", "imageDigest",
		    "errorCode", "errorMessage", "completedAt", "updatedAt"
		)
		SELECT
		    $1, $2, $3, $4, $5,
		    $6, $7, $8, $9, $10, $11,
		    $12, $13, $14, $15, $16,
		    $17, $18, $19, $20,
		    $21, $22, $23, NOW()
		WHERE EXISTS (
		    SELECT 1
		    FROM "RepoIndexingJob" j
		    JOIN "Repo" r
		      ON r.id = j."repoId"
		     AND r."orgId" = $4
		    WHERE j.id = $2
		      AND j."repoId" = $6
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		)
		AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexManifest" m
		    WHERE m."indexJobId" = $2
		      AND m."orgId" = $4
		      AND m."repoId" = $6
		      AND m."workspaceId" = $5
		      AND m.branch = $7
		      AND m."commitHash" = $9
		      AND m.status = 'PENDING'::"RepoIndexManifestStatus"
		)
		ON CONFLICT (id) DO UPDATE
		SET "updatedAt" = "CodeIntelIndexSubjob"."updatedAt"
		WHERE "CodeIntelIndexSubjob"."repoIndexingJobId" = EXCLUDED."repoIndexingJobId"
		  AND "CodeIntelIndexSubjob"."orgId" = EXCLUDED."orgId"
		  AND "CodeIntelIndexSubjob"."workspaceId" = EXCLUDED."workspaceId"
		  AND "CodeIntelIndexSubjob"."repoId" = EXCLUDED."repoId"
		  AND "CodeIntelIndexSubjob".branch = EXCLUDED.branch
		  AND "CodeIntelIndexSubjob".revision = EXCLUDED.revision
		  AND "CodeIntelIndexSubjob"."commitHash" = EXCLUDED."commitHash"
		  AND "CodeIntelIndexSubjob".layer = EXCLUDED.layer
		  AND COALESCE("CodeIntelIndexSubjob".language, '') = COALESCE(EXCLUDED.language, '')
		  AND COALESCE("CodeIntelIndexSubjob"."projectRoot", '') = COALESCE(EXCLUDED."projectRoot", '')
		  AND COALESCE("CodeIntelIndexSubjob".indexer, '') = COALESCE(EXCLUDED.indexer, '')
		  AND "CodeIntelIndexSubjob"."workerClass" = EXCLUDED."workerClass"
		  AND "CodeIntelIndexSubjob"."queueName" = EXCLUDED."queueName"
	`, in.ID, in.RepoIndexingJobID, in.CodeIntelIndexID, in.OrgID, in.WorkspaceID,
		in.RepoID, in.Branch, in.Revision, in.CommitHash, string(in.Layer), in.Language,
		in.ProjectRoot, in.Indexer, in.WorkerClass, in.QueueName, string(status), maxAttempts,
		in.InputDigest, in.ToolchainDigest, in.ImageDigest, errorCode, errorMessage, completedAt)
	if err != nil {
		return fmt.Errorf("Upsert%s: %w", status, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrScopeNotReady
	}
	return nil
}

func validLayer(layer Layer) bool {
	switch layer {
	case LayerClone, LayerZoekt, LayerASTTreeSitter, LayerSCIP, LayerGraphMerge, LayerActivate, LayerRemove:
		return true
	default:
		return false
	}
}

func safeProjectRoot(root string) bool {
	root = strings.TrimSpace(root)
	if root == "" || root == "." {
		return true
	}
	if strings.HasPrefix(root, "/") || strings.HasPrefix(root, `\`) || strings.Contains(root, `\`) {
		return false
	}
	clean := path.Clean(root)
	if clean == "." || clean == "" {
		return true
	}
	return clean == root && clean != ".." && !strings.HasPrefix(clean, "../") && !strings.Contains(clean, "/../")
}

// ListDispatchable returns subjobs whose durable state says they
// should have a matching queue task. It is intentionally safe to
// call from many backend pods: dispatch uses deterministic asynq
// TaskID values, so duplicate enqueue attempts collapse at the
// queue layer.
func (s *Store) ListDispatchable(ctx context.Context, limit int32) ([]DispatchableSubjob, error) {
	if limit <= 0 {
		return nil, errors.New("indexsubjobs: ListDispatchable requires positive limit")
	}
	rows, err := s.db.Query(ctx, `
		SELECT s.id, s."repoIndexingJobId", s."orgId", s."workspaceId",
		       s."repoId", s.branch, s.revision, s."commitHash", s.layer,
		       s.language, s."projectRoot", s.indexer, s."workerClass",
		       s."queueName", s.attempt
		FROM "CodeIntelIndexSubjob" s
		JOIN "RepoIndexingJob" j
		  ON j.id = s."repoIndexingJobId"
		 AND j."repoId" = s."repoId"
		JOIN "Repo" r
		  ON r.id = s."repoId"
		 AND r."orgId" = s."orgId"
		WHERE s.status IN ('QUEUED', 'RETRYING')
		  AND j.type = 'INDEX'::"RepoIndexingJobType"
		  AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  AND s.attempt < s."maxAttempts"
		  AND (
		    s.layer IN ('CLONE', 'REMOVE')
		    OR (
		      s.layer IN ('ZOEKT', 'AST_TREE_SITTER', 'SCIP')
		      AND NOT EXISTS (
		        SELECT 1 FROM "CodeIntelIndexSubjob" dep
		        WHERE dep."repoIndexingJobId" = s."repoIndexingJobId"
		          AND dep."workspaceId" = s."workspaceId"
		          AND dep."repoId" = s."repoId"
		          AND dep.branch = s.branch
		          AND dep.revision = s.revision
		          AND dep."commitHash" = s."commitHash"
		          AND dep.layer = 'CLONE'
		          AND dep.status NOT IN ('SUCCEEDED', 'SKIPPED')
		      )
		    )
		    OR (
		      s.layer = 'GRAPH_MERGE'
		      AND NOT EXISTS (
		        SELECT 1 FROM "CodeIntelIndexSubjob" dep
		        WHERE dep."repoIndexingJobId" = s."repoIndexingJobId"
		          AND dep."workspaceId" = s."workspaceId"
		          AND dep."repoId" = s."repoId"
		          AND dep.branch = s.branch
		          AND dep.revision = s.revision
		          AND dep."commitHash" = s."commitHash"
		          AND dep.layer IN ('AST_TREE_SITTER', 'SCIP')
		          AND dep.status NOT IN ('SUCCEEDED', 'SKIPPED')
		          AND NOT (
		            dep.layer = 'SCIP'
		            AND dep.status IN ('FAILED', 'CANCELED')
		          )
		      )
		    )
		    OR (
		      s.layer = 'ACTIVATE'
		      AND NOT EXISTS (
		        SELECT 1 FROM "CodeIntelIndexSubjob" dep
		        WHERE dep."repoIndexingJobId" = s."repoIndexingJobId"
		          AND dep."workspaceId" = s."workspaceId"
		          AND dep."repoId" = s."repoId"
		          AND dep.branch = s.branch
		          AND dep.revision = s.revision
		          AND dep."commitHash" = s."commitHash"
		          AND dep.id <> s.id
		          AND dep.layer <> 'ACTIVATE'
		          AND dep.status NOT IN ('SUCCEEDED', 'SKIPPED')
		          AND NOT (
		            dep.layer = 'SCIP'
		            AND dep.status IN ('FAILED', 'CANCELED')
		          )
		      )
		      AND NOT EXISTS (
		        SELECT 1 FROM "CodeIntelIndexSubjob" act
		        WHERE act."repoIndexingJobId" = s."repoIndexingJobId"
		          AND act."workspaceId" = s."workspaceId"
		          AND act."repoId" = s."repoId"
		          AND act.layer = 'ACTIVATE'
		          AND act.id <> s.id
		          AND act.status IN ('QUEUED', 'CLAIMED', 'RUNNING', 'VALIDATING', 'RETRYING')
		          AND (act."createdAt", act.id) < (s."createdAt", s.id)
		      )
		    )
		  )
		ORDER BY s."createdAt" ASC, s.id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("ListDispatchable: %w", err)
	}
	defer rows.Close()

	var out []DispatchableSubjob
	for rows.Next() {
		var item DispatchableSubjob
		var layer string
		if err := rows.Scan(
			&item.ID,
			&item.RepoIndexingJobID,
			&item.OrgID,
			&item.WorkspaceID,
			&item.RepoID,
			&item.Branch,
			&item.Revision,
			&item.CommitHash,
			&layer,
			&item.Language,
			&item.ProjectRoot,
			&item.Indexer,
			&item.WorkerClass,
			&item.QueueName,
			&item.Attempt,
		); err != nil {
			return nil, fmt.Errorf("ListDispatchable scan: %w", err)
		}
		item.Layer = Layer(layer)
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListDispatchable rows: %w", err)
	}
	return out, nil
}

// ClaimScoped is the consumer-facing claim operation. It verifies
// the immutable tenant/repo/branch/layer/class scope from the queue
// payload against the DB row before acquiring the lease.
func (s *Store) ClaimScoped(ctx context.Context, scope ClaimScope, leaseOwner, attemptID string, leaseUntil time.Time) (bool, error) {
	if scope.ID == "" || scope.RepoIndexingJobID == "" || scope.OrgID <= 0 || scope.RepoID <= 0 ||
		scope.WorkspaceID == nil || *scope.WorkspaceID == "" ||
		scope.Branch == "" || scope.Revision == "" || scope.CommitHash == "" || scope.Layer == "" ||
		scope.WorkerClass == "" || scope.QueueName == "" || scope.Attempt <= 0 {
		return false, errors.New("indexsubjobs: ClaimScoped requires immutable scope")
	}
	class, ok := workerclasses.ByName(scope.WorkerClass)
	if !ok || class.QueueName != scope.QueueName {
		return false, errors.New("indexsubjobs: ClaimScoped workerClass/queue mismatch")
	}
	if leaseOwner == "" || attemptID == "" {
		return false, errors.New("indexsubjobs: ClaimScoped requires leaseOwner and attemptID")
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob"
		SET status = 'CLAIMED',
		    attempt = attempt + 1,
		    "attemptId" = $16,
		    "leaseOwner" = $15,
		    "leaseExpiresAt" = $17,
		    "heartbeatAt" = NOW(),
		    "startedAt" = COALESCE("startedAt", NOW()),
		    "updatedAt" = NOW()
		WHERE id = $1
		  AND "repoIndexingJobId" = $2
		  AND "orgId" = $3
		  AND "workspaceId" = $4
		  AND "repoId" = $5
		  AND branch = $6
		  AND revision = $7
		  AND "commitHash" = $8
		  AND layer = $9
		  AND COALESCE(language, '') = COALESCE($10::text, '')
		  AND COALESCE("projectRoot", '') = COALESCE($11::text, '')
		  AND COALESCE(indexer, '') = COALESCE($12::text, '')
		  AND "workerClass" = $13
		  AND "queueName" = $14
		  AND status IN ('QUEUED', 'RETRYING', 'CLAIMED')
		  AND attempt = $18
		  AND attempt < "maxAttempts"
		  AND ("leaseExpiresAt" IS NULL OR "leaseExpiresAt" < NOW() OR status IN ('QUEUED', 'RETRYING'))
		  AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexingJob" j
		    JOIN "Repo" r
		      ON r.id = j."repoId"
		     AND r."orgId" = $3
		    WHERE j.id = $2
		      AND j."repoId" = $5
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  )
	`,
		scope.ID,
		scope.RepoIndexingJobID,
		scope.OrgID,
		scope.WorkspaceID,
		scope.RepoID,
		scope.Branch,
		scope.Revision,
		scope.CommitHash,
		string(scope.Layer),
		scope.Language,
		scope.ProjectRoot,
		scope.Indexer,
		scope.WorkerClass,
		scope.QueueName,
		leaseOwner,
		attemptID,
		leaseUntil,
		scope.Attempt-1,
	)
	if err != nil {
		return false, fmt.Errorf("ClaimScoped: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Heartbeat renews a held lease and moves CLAIMED work into RUNNING.
func (s *Store) Heartbeat(ctx context.Context, subjobID, leaseOwner, attemptID string, leaseUntil time.Time) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob"
		SET status = CASE
		        WHEN status IN ('CLAIMED', 'RUNNING') THEN 'RUNNING'
		        ELSE status
		    END,
		    "leaseExpiresAt" = $3,
		    "heartbeatAt" = NOW(),
		    "updatedAt" = NOW()
		WHERE id = $1
		  AND "leaseOwner" = $2
		  AND "attemptId" = $4
		  AND status IN ('CLAIMED', 'RUNNING', 'ARTIFACT_WRITTEN', 'VALIDATING')
		  AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexingJob" j
		    JOIN "Repo" r
		      ON r.id = j."repoId"
		     AND r."orgId" = "CodeIntelIndexSubjob"."orgId"
		    WHERE j.id = "CodeIntelIndexSubjob"."repoIndexingJobId"
		      AND j."repoId" = "CodeIntelIndexSubjob"."repoId"
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  )
	`, subjobID, leaseOwner, leaseUntil, attemptID)
	if err != nil {
		return false, fmt.Errorf("Heartbeat: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkArtifactWritten records the final artifact after the worker
// has atomically moved it into the commit-scoped artifact path.
func (s *Store) MarkArtifactWritten(ctx context.Context, subjobID, leaseOwner, attemptID, tempPath, artifactPath, artifactSHA256 string) (bool, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob"
		SET status = 'ARTIFACT_WRITTEN',
		    "artifactTempPath" = $3,
		    "artifactPath" = $4,
		    "artifactSha256" = $5,
		    "heartbeatAt" = NOW(),
		    "updatedAt" = NOW()
		WHERE id = $1
		  AND "leaseOwner" = $2
		  AND "attemptId" = $6
		  AND status IN ('CLAIMED', 'RUNNING', 'ARTIFACT_WRITTEN')
		  AND "leaseExpiresAt" IS NOT NULL
		  AND "leaseExpiresAt" > NOW()
		  AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexingJob" j
		    JOIN "Repo" r
		      ON r.id = j."repoId"
		     AND r."orgId" = "CodeIntelIndexSubjob"."orgId"
		    WHERE j.id = "CodeIntelIndexSubjob"."repoIndexingJobId"
		      AND j."repoId" = "CodeIntelIndexSubjob"."repoId"
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  )
	`, subjobID, leaseOwner, tempPath, artifactPath, artifactSHA256, attemptID)
	if err != nil {
		return false, fmt.Errorf("MarkArtifactWritten: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) MarkSucceeded(ctx context.Context, subjobID, leaseOwner, attemptID string) (bool, error) {
	if subjobID == "" || leaseOwner == "" || attemptID == "" {
		return false, errors.New("indexsubjobs: MarkSucceeded requires subjobID, leaseOwner, and attemptID")
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob"
		SET status = 'SUCCEEDED',
		    "leaseOwner" = NULL,
		    "leaseExpiresAt" = NULL,
		    "errorCode" = NULL,
		    "errorMessage" = NULL,
		    "completedAt" = NOW(),
		    "updatedAt" = NOW()
		WHERE id = $1
		  AND "leaseOwner" = $2
		  AND "attemptId" = $3
		  AND status IN ('CLAIMED', 'RUNNING', 'ARTIFACT_WRITTEN', 'VALIDATING')
		  AND "leaseExpiresAt" IS NOT NULL
		  AND "leaseExpiresAt" > NOW()
		  AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexingJob" j
		    JOIN "Repo" r
		      ON r.id = j."repoId"
		     AND r."orgId" = "CodeIntelIndexSubjob"."orgId"
		    WHERE j.id = "CodeIntelIndexSubjob"."repoIndexingJobId"
		      AND j."repoId" = "CodeIntelIndexSubjob"."repoId"
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  )
	`, subjobID, leaseOwner, attemptID)
	if err != nil {
		return false, fmt.Errorf("MarkSucceeded: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) MarkFailed(ctx context.Context, subjobID, leaseOwner, attemptID, errorCode, errorMessage string) (bool, error) {
	if subjobID == "" || leaseOwner == "" || attemptID == "" {
		return false, errors.New("indexsubjobs: MarkFailed requires subjobID, leaseOwner, and attemptID")
	}
	errorCode, errorMessage = normalizeFailure(errorCode, errorMessage)
	tag, err := s.db.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob"
		SET status = CASE
		        WHEN layer = 'SCIP'
		         AND (
		              $5 ILIKE '%semantically empty .scip%'
		           OR $5 ILIKE '%has no semantic rows:%'
		         ) THEN 'SKIPPED'
		        WHEN layer = 'SCIP' AND $5 ILIKE '%timeout after%' THEN 'SKIPPED'
		        WHEN attempt < "maxAttempts" THEN 'RETRYING'
		        WHEN layer IN ('SCIP', 'AST_TREE_SITTER') THEN 'SKIPPED'
		        ELSE 'FAILED'
		    END,
		    "leaseOwner" = NULL,
		    "leaseExpiresAt" = NULL,
		    "attemptId" = NULL,
		    "heartbeatAt" = NULL,
		    "errorCode" = $4,
		    "errorMessage" = $5,
		    "completedAt" = CASE
		        WHEN layer = 'SCIP'
		         AND (
		              $5 ILIKE '%semantically empty .scip%'
		           OR $5 ILIKE '%has no semantic rows:%'
		         ) THEN NOW()
		        WHEN layer = 'SCIP' AND $5 ILIKE '%timeout after%' THEN NOW()
		        WHEN attempt < "maxAttempts" THEN NULL
		        ELSE NOW()
		    END,
		    "updatedAt" = NOW()
		WHERE id = $1
		  AND "leaseOwner" = $2
		  AND "attemptId" = $3
		  AND status IN ('CLAIMED', 'RUNNING', 'ARTIFACT_WRITTEN', 'VALIDATING')
		  AND "leaseExpiresAt" IS NOT NULL
		  AND "leaseExpiresAt" > NOW()
		  AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexingJob" j
		    JOIN "Repo" r
		      ON r.id = j."repoId"
		     AND r."orgId" = "CodeIntelIndexSubjob"."orgId"
		    WHERE j.id = "CodeIntelIndexSubjob"."repoIndexingJobId"
		      AND j."repoId" = "CodeIntelIndexSubjob"."repoId"
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  )
	`, subjobID, leaseOwner, attemptID, errorCode, errorMessage)
	if err != nil {
		return false, fmt.Errorf("MarkFailed: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if err := s.failParentIfTerminalFailure(ctx, subjobID, errorCode, errorMessage); err != nil {
			return false, err
		}
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) MarkRetryableInfrastructureFailure(ctx context.Context, subjobID, leaseOwner, attemptID, errorCode, errorMessage string) (bool, error) {
	if subjobID == "" || leaseOwner == "" || attemptID == "" {
		return false, errors.New("indexsubjobs: MarkRetryableInfrastructureFailure requires subjobID, leaseOwner, and attemptID")
	}
	errorCode, errorMessage = normalizeFailure(errorCode, errorMessage)
	tag, err := s.db.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob"
		SET status = CASE
		        WHEN attempt < "maxAttempts" THEN 'RETRYING'
		        WHEN layer IN ('SCIP', 'AST_TREE_SITTER') THEN 'SKIPPED'
		        ELSE 'FAILED'
		    END,
		    "leaseOwner" = NULL,
		    "leaseExpiresAt" = NULL,
		    "attemptId" = NULL,
		    "heartbeatAt" = NULL,
		    "errorCode" = $4,
		    "errorMessage" = $5,
		    "completedAt" = CASE
		        WHEN attempt < "maxAttempts" THEN NULL
		        ELSE NOW()
		    END,
		    "updatedAt" = NOW()
		WHERE id = $1
		  AND "leaseOwner" = $2
		  AND "attemptId" = $3
		  AND status IN ('CLAIMED', 'RUNNING')
		  AND "leaseExpiresAt" IS NOT NULL
		  AND "leaseExpiresAt" > NOW()
		  AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexingJob" j
		    JOIN "Repo" r
		      ON r.id = j."repoId"
		     AND r."orgId" = "CodeIntelIndexSubjob"."orgId"
		    WHERE j.id = "CodeIntelIndexSubjob"."repoIndexingJobId"
		      AND j."repoId" = "CodeIntelIndexSubjob"."repoId"
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  )
	`, subjobID, leaseOwner, attemptID, errorCode, errorMessage)
	if err != nil {
		return false, fmt.Errorf("MarkRetryableInfrastructureFailure: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if err := s.failParentIfTerminalFailure(ctx, subjobID, errorCode, errorMessage); err != nil {
			return false, err
		}
	}
	return tag.RowsAffected() == 1, nil
}

func normalizeFailure(errorCode, errorMessage string) (string, string) {
	errorCode = strings.TrimSpace(errorCode)
	errorMessage = strings.TrimSpace(errorMessage)
	if errorCode == "" {
		errorCode = "SUBJOB_FAILED"
	}
	if errorMessage == "" {
		errorMessage = "subjob failed without diagnostic"
	}
	return errorCode, errorMessage
}

func (s *Store) failParentIfTerminalFailure(ctx context.Context, subjobID, errorCode, errorMessage string) error {
	_, err := s.db.Exec(ctx, `
		WITH failed_subjob AS (
		    SELECT "repoIndexingJobId", "repoId"
		    FROM "CodeIntelIndexSubjob"
		    WHERE id = $1
		      AND status = 'FAILED'
		),
		canceled_dependents AS (
		    UPDATE "CodeIntelIndexSubjob" s
		    SET status = 'CANCELED',
		        "leaseOwner" = NULL,
		        "leaseExpiresAt" = NULL,
		        "attemptId" = NULL,
		        "heartbeatAt" = NULL,
		        "errorCode" = 'PARENT_SUBJOB_FAILED',
		        "errorMessage" = CONCAT($2::text, ': ', $3::text),
		        "completedAt" = NOW(),
		        "updatedAt" = NOW()
		    FROM failed_subjob f
		    WHERE s."repoIndexingJobId" = f."repoIndexingJobId"
		      AND s.id <> $1
		      AND s.status IN ('QUEUED', 'RETRYING')
		    RETURNING s.id
		),
		updated_job AS (
		    UPDATE "RepoIndexingJob" j
		    SET status = 'FAILED',
		        "errorMessage" = CONCAT($2::text, ': ', $3::text),
		        "completedAt" = NOW(),
		        "updatedAt" = NOW()
		    FROM failed_subjob f
		    WHERE j.id = f."repoIndexingJobId"
		      AND j."repoId" = f."repoId"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		    RETURNING j."repoId"
		)
		UPDATE "Repo" r
		SET "latestIndexingJobStatus" = 'FAILED'::"RepoIndexingJobStatus",
		    "updatedAt" = NOW()
		FROM updated_job u
		WHERE r.id = u."repoId"
	`, subjobID, errorCode, errorMessage)
	if err != nil {
		return fmt.Errorf("fail parent index job after terminal subjob failure: %w", err)
	}
	return nil
}

// ReconcileTerminalFailures repairs parent jobs that already have a
// terminal failed subjob. It is deliberately safe to call from every
// backend pod under the dispatcher lock: repeated executions update
// the same terminal rows and do not create new work.
func (s *Store) ReconcileTerminalFailures(ctx context.Context, limit int32) (int64, error) {
	if limit <= 0 {
		return 0, errors.New("indexsubjobs: ReconcileTerminalFailures requires positive limit")
	}
	tag, err := s.db.Exec(ctx, `
		WITH semantic_empty AS (
		    SELECT s.id
		    FROM "CodeIntelIndexSubjob" s
		    JOIN "RepoIndexingJob" j
		      ON j.id = s."repoIndexingJobId"
		     AND j."repoId" = s."repoId"
		    JOIN "Repo" r
		      ON r.id = s."repoId"
		     AND r."orgId" = s."orgId"
		    WHERE s.layer = 'SCIP'
		      AND s.status = 'RETRYING'
		      AND (
		           s."errorMessage" ILIKE '%semantically empty .scip%'
		        OR s."errorMessage" ILIKE '%has no semantic rows:%'
		      )
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		    ORDER BY s."updatedAt" ASC, s.id ASC
		    LIMIT $1
		    FOR UPDATE OF s SKIP LOCKED
		),
		skipped_semantic_empty AS (
		    UPDATE "CodeIntelIndexSubjob" s
		    SET status = 'SKIPPED',
		        "leaseOwner" = NULL,
		        "leaseExpiresAt" = NULL,
		        "attemptId" = NULL,
		        "heartbeatAt" = NULL,
		        "completedAt" = NOW(),
		        "updatedAt" = NOW()
		    FROM semantic_empty e
		    WHERE s.id = e.id
		    RETURNING s.id
		),
		failed_jobs AS (
		    SELECT DISTINCT ON (s."repoIndexingJobId")
		        s."repoIndexingJobId",
		        s."repoId",
		        COALESCE(NULLIF(s."errorCode", ''), 'SUBJOB_FAILED') AS "errorCode",
		        COALESCE(NULLIF(s."errorMessage", ''), 'terminal subjob failed') AS "errorMessage"
		    FROM "CodeIntelIndexSubjob" s
		    JOIN "RepoIndexingJob" j
		      ON j.id = s."repoIndexingJobId"
		     AND j."repoId" = s."repoId"
		    JOIN "Repo" r
		      ON r.id = s."repoId"
		     AND r."orgId" = s."orgId"
		    WHERE s.status = 'FAILED'
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		    ORDER BY s."repoIndexingJobId", s."updatedAt" DESC, s.id DESC
		    LIMIT $1
		),
		canceled_dependents AS (
		    UPDATE "CodeIntelIndexSubjob" s
		    SET status = 'CANCELED',
		        "leaseOwner" = NULL,
		        "leaseExpiresAt" = NULL,
		        "attemptId" = NULL,
		        "heartbeatAt" = NULL,
		        "errorCode" = 'PARENT_SUBJOB_FAILED',
		        "errorMessage" = CONCAT(f."errorCode", ': ', f."errorMessage"),
		        "completedAt" = NOW(),
		        "updatedAt" = NOW()
		    FROM failed_jobs f
		    WHERE s."repoIndexingJobId" = f."repoIndexingJobId"
		      AND s.status IN ('QUEUED', 'RETRYING')
		    RETURNING s.id
		),
		updated_job AS (
		    UPDATE "RepoIndexingJob" j
		    SET status = 'FAILED',
		        "errorMessage" = CONCAT(f."errorCode", ': ', f."errorMessage"),
		        "completedAt" = NOW(),
		        "updatedAt" = NOW()
		    FROM failed_jobs f
		    WHERE j.id = f."repoIndexingJobId"
		      AND j."repoId" = f."repoId"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		    RETURNING j."repoId"
		)
		UPDATE "Repo" r
		SET "latestIndexingJobStatus" = 'FAILED'::"RepoIndexingJobStatus",
		    "updatedAt" = NOW()
		FROM updated_job u
		WHERE r.id = u."repoId"
	`, limit)
	if err != nil {
		return 0, fmt.Errorf("ReconcileTerminalFailures: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RequeueExpiredLeases returns expired in-flight subjobs to
// RETRYING. The queue producer can then enqueue the matching
// payloads again. This is safe to run from many backend pods.
func (s *Store) RequeueExpiredLeases(ctx context.Context, now time.Time, limit int32) (int64, error) {
	if limit <= 0 {
		return 0, errors.New("indexsubjobs: RequeueExpiredLeases requires positive limit")
	}
	tag, err := s.db.Exec(ctx, `
		WITH expired AS (
		    SELECT s.id
		    FROM "CodeIntelIndexSubjob" s
		    JOIN "RepoIndexingJob" j
		      ON j.id = s."repoIndexingJobId"
		     AND j."repoId" = s."repoId"
		    WHERE s.status IN ('CLAIMED', 'RUNNING', 'ARTIFACT_WRITTEN', 'VALIDATING')
		      AND s."leaseExpiresAt" IS NOT NULL
		      AND s."leaseExpiresAt" < $1
		    ORDER BY s."leaseExpiresAt" ASC
		    LIMIT $2
		    FOR UPDATE OF s SKIP LOCKED
		)
		UPDATE "CodeIntelIndexSubjob" s
		SET status = CASE
		        WHEN j.type = 'INDEX'::"RepoIndexingJobType"
		         AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		         AND s.attempt < s."maxAttempts" THEN 'RETRYING'
		        WHEN j.type = 'INDEX'::"RepoIndexingJobType"
		         AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		         AND s.layer IN ('SCIP', 'AST_TREE_SITTER') THEN 'SKIPPED'
		        WHEN j.type = 'INDEX'::"RepoIndexingJobType"
		         AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus" THEN 'FAILED'
		        ELSE 'CANCELED'
		    END,
		    "leaseOwner" = NULL,
		    "attemptId" = NULL,
		    "leaseExpiresAt" = NULL,
		    "heartbeatAt" = NULL,
		    "completedAt" = CASE
		        WHEN j.type = 'INDEX'::"RepoIndexingJobType"
		         AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		         AND s.attempt < s."maxAttempts" THEN NULL
		        ELSE NOW()
		    END,
		    "updatedAt" = NOW()
		FROM expired, "RepoIndexingJob" j
		WHERE s.id = expired.id
		  AND j.id = s."repoIndexingJobId"
		  AND j."repoId" = s."repoId"
	`, now, limit)
	if err != nil {
		return 0, fmt.Errorf("RequeueExpiredLeases: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RequeueLeasesForOwnerPrefixes rescues in-flight subjobs owned by a
// previous process in the same Kubernetes pod. It is intentionally scoped
// to explicit lease-owner prefixes so a restarted backend does not steal
// work from another HPA replica.
func (s *Store) RequeueLeasesForOwnerPrefixes(ctx context.Context, ownerPrefixes []string, now time.Time, limit int32) (int64, error) {
	if limit <= 0 {
		return 0, errors.New("indexsubjobs: RequeueLeasesForOwnerPrefixes requires positive limit")
	}
	cleanPrefixes := make([]string, 0, len(ownerPrefixes))
	for _, prefix := range ownerPrefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix != "" {
			cleanPrefixes = append(cleanPrefixes, prefix)
		}
	}
	if len(cleanPrefixes) == 0 {
		return 0, errors.New("indexsubjobs: RequeueLeasesForOwnerPrefixes requires at least one owner prefix")
	}
	tag, err := s.db.Exec(ctx, `
		WITH rescued AS (
		    SELECT s.id
		    FROM "CodeIntelIndexSubjob" s
		    JOIN "RepoIndexingJob" j
		      ON j.id = s."repoIndexingJobId"
		     AND j."repoId" = s."repoId"
		    WHERE s.status IN ('CLAIMED', 'RUNNING', 'ARTIFACT_WRITTEN', 'VALIDATING')
		      AND s."leaseOwner" IS NOT NULL
		      AND EXISTS (
		        SELECT 1
		        FROM unnest($1::text[]) AS prefix
		        WHERE s."leaseOwner" LIKE prefix || '%'
		    )
		    ORDER BY s."leaseExpiresAt" ASC NULLS FIRST, s."updatedAt" ASC
		    LIMIT $2
		    FOR UPDATE OF s SKIP LOCKED
		)
		UPDATE "CodeIntelIndexSubjob" s
		SET status = CASE
		        WHEN j.type = 'INDEX'::"RepoIndexingJobType"
		         AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		         AND s.attempt < s."maxAttempts" THEN 'RETRYING'
		        WHEN j.type = 'INDEX'::"RepoIndexingJobType"
		         AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		         AND s.layer = 'SCIP' THEN 'SKIPPED'
		        WHEN j.type = 'INDEX'::"RepoIndexingJobType"
		         AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus" THEN 'FAILED'
		        ELSE 'CANCELED'
		    END,
		    "leaseOwner" = NULL,
		    "attemptId" = NULL,
		    "leaseExpiresAt" = NULL,
		    "heartbeatAt" = NULL,
		    "completedAt" = CASE
		        WHEN j.type = 'INDEX'::"RepoIndexingJobType"
		         AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		         AND s.attempt < s."maxAttempts" THEN NULL
		        ELSE $3::timestamp
		    END,
		    "updatedAt" = $3::timestamp
		FROM rescued, "RepoIndexingJob" j
		WHERE s.id = rescued.id
		  AND j.id = s."repoIndexingJobId"
		  AND j."repoId" = s."repoId"
	`, cleanPrefixes, limit, now)
	if err != nil {
		return 0, fmt.Errorf("RequeueLeasesForOwnerPrefixes: %w", err)
	}
	return tag.RowsAffected(), nil
}
