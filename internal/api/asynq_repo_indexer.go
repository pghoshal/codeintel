//go:build legacy_api_asynq

package api

import (
	"context"
	"errors"
	"fmt"

	"codeintel/pkg/asynqueues"
	"codeintel/pkg/repoindex"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
)

// AsynqRepoIndexer is the production RepoIndexer implementation.
// DELETE /api/repos/{id}/index (REMOVE_INDEX) and the
// forthcoming POST /api/repos/{id}/index (INDEX) routes
// delegate here via api.Config.RepoIndexer.
//
// Same row-then-enqueue ordering as [AsynqConnectionSyncer]: the
// RepoIndexingJob row lands in PENDING before the asynq task so
// the worker's MarkInProgress can find it. Tenant-capacity
// gating is deferred (same caveat as the connection-sync syncer).
type AsynqRepoIndexer struct {
	db    asyncqQuerier
	asynq *asynq.Client
}

// NewAsynqRepoIndexer wires the indexer to a live pool + asynq
// client. The caller (codeintel-app main.go) owns the
// asynq.Client's lifecycle.
func NewAsynqRepoIndexer(db asyncqQuerier, ac *asynq.Client) *AsynqRepoIndexer {
	return &AsynqRepoIndexer{db: db, asynq: ac}
}

// Schedule inserts one RepoIndexingJob row, enqueues an asynq
// task pointing at it, and returns the row id.
//
// Failure paths:
//   - Repo lookup miss               → ErrRepoNotFound.
//   - Repo lookup error              → wrapped error.
//   - Active job already exists      → JobAlreadyActiveError.
//   - Job insert error               → wrapped error.
//   - asynq enqueue error after row → row is marked FAILED and
//     the enqueue error is returned so a retry can schedule a
//     fresh job.
func (s *AsynqRepoIndexer) Schedule(ctx context.Context, req RepoIndexRequest) (RepoIndexResult, error) {
	if !req.Kind.Valid() {
		return RepoIndexResult{}, fmt.Errorf("AsynqRepoIndexer: invalid Kind %q", req.Kind)
	}

	// Verify the repo belongs to the org AND is attached to at
	// least one connection. Mirrors the legacy
	// `connections: { some: {} }` filter on the Prisma findFirst
	// (packages/web/src/app/api/(server)/repos/[id]/index/route.ts:111-118).
	// Closes P.1 in docs/codeintel-parity-gaps.md.
	//
	// An orphaned Repo (zero RepoToConnection rows) is treated
	// as not-found, matching legacy. The Repo row also supplies
	// the human-readable name we drop into the asynq payload.
	var repoName string
	err := s.db.QueryRow(ctx, `
		SELECT name FROM "Repo" r
		WHERE  r.id = $1
		  AND  r."orgId" = $2
		  AND  EXISTS (
		      SELECT 1
		      FROM "RepoToConnection" rc
		      JOIN "Connection" c ON c.id = rc."connectionId"
		      WHERE rc."repoId" = r.id
		        AND c."orgId" = r."orgId"
		  )
	`, req.RepoID, req.OrgID).Scan(&repoName)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoIndexResult{}, ErrRepoNotFound
	}
	if err != nil {
		return RepoIndexResult{}, fmt.Errorf("AsynqRepoIndexer: load Repo: %w", err)
	}

	jobID := uuid.NewString()
	inserted, active, err := s.insertPendingRepoIndexJob(ctx, req.RepoID, jobID, string(req.Kind))
	if err != nil {
		return RepoIndexResult{}, err
	}
	if !inserted {
		return RepoIndexResult{}, &JobAlreadyActiveError{
			JobID:  active.JobID,
			Type:   active.Type,
			Status: active.Status,
		}
	}

	payload, err := repoindex.Marshal(repoindex.TaskPayload{
		Type:     repoindex.JobType(req.Kind),
		JobID:    jobID,
		RepoID:   req.RepoID,
		OrgID:    req.OrgID,
		RepoName: repoName,
		Ref:      req.Ref,
	})
	if err != nil {
		_ = s.markRepoIndexJobFailedAfterEnqueueError(ctx, jobID, err)
		return RepoIndexResult{}, fmt.Errorf("AsynqRepoIndexer: marshal payload: %w", err)
	}

	// Split-executor path: INDEX, CLEANUP, and REMOVE_INDEX all
	// enter the Go-owned repo-index queue. INDEX now performs
	// clone/snapshot/manifest planning in codeintel-backend, then
	// child AST/SCIP/graph/activate work fans out through the
	// durable subjob dispatcher. The old repo-index-rust queue is
	// retained only for legacy compatibility tests.
	queueName := asynqueues.QueueRepoIndex
	taskType := asynqueues.QueueRepoIndex
	task := asynq.NewTask(taskType, payload)
	if _, err := s.asynq.EnqueueContext(ctx, task,
		asynq.Queue(queueName),
	); err != nil {
		_ = s.markRepoIndexJobFailedAfterEnqueueError(ctx, jobID, err)
		return RepoIndexResult{}, fmt.Errorf("AsynqRepoIndexer: enqueue: %w", err)
	}

	return RepoIndexResult{JobID: jobID}, nil
}

type activeRepoIndexJob struct {
	JobID  string
	Type   string
	Status string
}

func (s *AsynqRepoIndexer) insertPendingRepoIndexJob(ctx context.Context, repoID int32, jobID, jobType string) (bool, activeRepoIndexJob, error) {
	var (
		row      activeRepoIndexJob
		inserted bool
	)
	err := s.db.QueryRow(ctx, `
		WITH lock AS (
		    SELECT pg_advisory_xact_lock(31001, $1::int)
		),
		existing AS (
		    SELECT j.id, j.type::text, j.status::text
		    FROM "RepoIndexingJob" j, lock
		    WHERE j."repoId" = $1
		      AND j.status IN ('PENDING'::"RepoIndexingJobStatus", 'IN_PROGRESS'::"RepoIndexingJobStatus")
		    ORDER BY j."createdAt" DESC NULLS LAST, j.id DESC
		    LIMIT 1
		),
		inserted AS (
		    INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, "updatedAt")
		    SELECT $2, $1, $3::"RepoIndexingJobType", 'PENDING', NOW()
		    FROM lock
		    WHERE NOT EXISTS (SELECT 1 FROM existing)
		    RETURNING id, type::text, status::text
		)
		SELECT id, type, status, TRUE AS inserted FROM inserted
		UNION ALL
		SELECT id, type, status, FALSE AS inserted FROM existing
		LIMIT 1
	`, repoID, jobID, jobType).Scan(&row.JobID, &row.Type, &row.Status, &inserted)
	if err != nil {
		return false, activeRepoIndexJob{}, fmt.Errorf("AsynqRepoIndexer: insert pending job: %w", err)
	}
	return inserted, row, nil
}

func (s *AsynqRepoIndexer) markRepoIndexJobFailedAfterEnqueueError(ctx context.Context, jobID string, cause error) error {
	if cause == nil {
		cause = errors.New("enqueue failed")
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE "RepoIndexingJob"
		SET status = 'FAILED',
		    "errorMessage" = $2,
		    "completedAt" = NOW(),
		    "updatedAt" = NOW()
		WHERE id = $1
		  AND status = 'PENDING'::"RepoIndexingJobStatus"
	`, jobID, "enqueue failed before worker delivery: "+cause.Error()); err != nil {
		return fmt.Errorf("AsynqRepoIndexer: mark enqueue failure: %w", err)
	}
	return nil
}
