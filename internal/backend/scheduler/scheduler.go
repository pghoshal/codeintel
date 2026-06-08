package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"codeintel/pkg/asynqueues"
	"codeintel/pkg/connectionsync"
	"codeintel/pkg/repoindex"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

type Service struct {
	db    pgxQuerier
	asynq *asynq.Client
}

func NewService(db pgxQuerier, ac *asynq.Client) *Service {
	return &Service{db: db, asynq: ac}
}

var (
	ErrRepoNotFound       = errors.New("backend scheduler: repo not found in org")
	ErrConnectionNotFound = errors.New("backend scheduler: connection not found in org")
	ErrUnavailable        = errors.New("backend scheduler: queue producer is not configured")
)

type RepoScheduleRequest struct {
	OrgID  int32
	RepoID int32
	Kind   repoindex.JobType
	Ref    string
}

type ScheduleResult struct {
	JobID             string
	AlreadyAtCapacity bool
}

type JobAlreadyActiveError struct {
	JobID  string
	Type   string
	Status string
}

func (e *JobAlreadyActiveError) Error() string {
	return fmt.Sprintf("Repo already has active %s job %s.", e.Type, e.JobID)
}

func (s *Service) ScheduleRepoIndex(ctx context.Context, req RepoScheduleRequest) (ScheduleResult, error) {
	if s == nil || s.db == nil || s.asynq == nil {
		return ScheduleResult{}, ErrUnavailable
	}
	if !req.Kind.Valid() {
		return ScheduleResult{}, fmt.Errorf("backend scheduler: invalid repo index kind %q", req.Kind)
	}
	if req.OrgID <= 0 || req.RepoID <= 0 {
		return ScheduleResult{}, fmt.Errorf("backend scheduler: orgId and repoId are required")
	}

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
		return ScheduleResult{}, ErrRepoNotFound
	}
	if err != nil {
		return ScheduleResult{}, fmt.Errorf("backend scheduler: load repo: %w", err)
	}

	jobID := uuid.NewString()
	inserted, active, err := s.insertPendingRepoIndexJob(ctx, req.RepoID, jobID, string(req.Kind), strings.TrimSpace(req.Ref))
	if err != nil {
		return ScheduleResult{}, err
	}
	if !inserted {
		return ScheduleResult{}, &JobAlreadyActiveError{JobID: active.JobID, Type: active.Type, Status: active.Status}
	}

	payload, err := repoindex.Marshal(repoindex.TaskPayload{
		Type:     req.Kind,
		JobID:    jobID,
		RepoID:   req.RepoID,
		OrgID:    req.OrgID,
		RepoName: repoName,
		Ref:      strings.TrimSpace(req.Ref),
	})
	if err != nil {
		_ = s.markRepoIndexJobFailedAfterEnqueueError(ctx, jobID, err)
		return ScheduleResult{}, fmt.Errorf("backend scheduler: marshal repo index payload: %w", err)
	}

	task := asynq.NewTask(asynqueues.QueueRepoIndex, payload)
	if _, err := s.asynq.EnqueueContext(ctx, task, asynq.Queue(asynqueues.QueueRepoIndex)); err != nil {
		_ = s.markRepoIndexJobFailedAfterEnqueueError(ctx, jobID, err)
		return ScheduleResult{}, fmt.Errorf("backend scheduler: enqueue repo index: %w", err)
	}
	return ScheduleResult{JobID: jobID}, nil
}

type activeRepoIndexJob struct {
	JobID  string
	Type   string
	Status string
}

func (s *Service) insertPendingRepoIndexJob(ctx context.Context, repoID int32, jobID, jobType, ref string) (bool, activeRepoIndexJob, error) {
	var (
		row      activeRepoIndexJob
		inserted bool
	)
	refCandidates := schedulerRefCandidates(ref)
	err := s.db.QueryRow(ctx, `
			WITH lock AS (
			    SELECT pg_advisory_xact_lock(31001, $1::int)
			),
			existing AS (
			    SELECT j.id, j.type::text, j.status::text
			    FROM "RepoIndexingJob" j, lock
			    WHERE j."repoId" = $1
			      AND j.status IN ('PENDING'::"RepoIndexingJobStatus", 'IN_PROGRESS'::"RepoIndexingJobStatus")
			      AND (
			        $4::text = ''
			        OR COALESCE(j.metadata->>'ref', '') = ''
			        OR j.metadata->>'ref' = ANY($5::text[])
			      )
			    ORDER BY j."createdAt" DESC NULLS LAST, j.id DESC
			    LIMIT 1
			),
		inserted AS (
		    INSERT INTO "RepoIndexingJob" (id, "repoId", type, status, "updatedAt", metadata)
		    SELECT $2, $1, $3::"RepoIndexingJobType", 'PENDING', NOW(),
		           CASE WHEN $4::text = '' THEN '{}'::jsonb ELSE jsonb_build_object('ref', $4::text) END
		    FROM lock
		    WHERE NOT EXISTS (SELECT 1 FROM existing)
		    RETURNING id, type::text, status::text
		)
		SELECT id, type, status, TRUE AS inserted FROM inserted
			UNION ALL
			SELECT id, type, status, FALSE AS inserted FROM existing
			LIMIT 1
		`, repoID, jobID, jobType, ref, refCandidates).Scan(&row.JobID, &row.Type, &row.Status, &inserted)
	if err != nil {
		return false, activeRepoIndexJob{}, fmt.Errorf("backend scheduler: insert pending repo index job: %w", err)
	}
	return inserted, row, nil
}

func schedulerRefCandidates(ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return []string{}
	}
	out := []string{ref}
	if stripped := strings.TrimPrefix(ref, "refs/heads/"); stripped != ref && strings.TrimSpace(stripped) != "" {
		out = append(out, stripped)
	} else {
		out = append(out, "refs/heads/"+ref)
	}
	seen := map[string]bool{}
	deduped := out[:0]
	for _, candidate := range out {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		deduped = append(deduped, candidate)
	}
	return deduped
}

func (s *Service) markRepoIndexJobFailedAfterEnqueueError(ctx context.Context, jobID string, cause error) error {
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
		return fmt.Errorf("backend scheduler: mark enqueue failure: %w", err)
	}
	return nil
}

type ConnectionSyncRequest struct {
	OrgID        int32
	ConnectionID int32
}

func (s *Service) ScheduleConnectionSync(ctx context.Context, req ConnectionSyncRequest) (ScheduleResult, error) {
	if s == nil || s.db == nil || s.asynq == nil {
		return ScheduleResult{}, ErrUnavailable
	}
	if req.OrgID <= 0 || req.ConnectionID <= 0 {
		return ScheduleResult{}, fmt.Errorf("backend scheduler: orgId and connectionId are required")
	}
	var connectionName string
	if err := s.db.QueryRow(ctx, `
		SELECT name FROM "Connection" WHERE id = $1 AND "orgId" = $2
	`, req.ConnectionID, req.OrgID).Scan(&connectionName); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ScheduleResult{}, ErrConnectionNotFound
		}
		return ScheduleResult{}, fmt.Errorf("backend scheduler: load connection: %w", err)
	}

	jobID := uuid.NewString()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO "ConnectionSyncJob" (id, "connectionId", status, "updatedAt", "warningMessages")
		VALUES ($1, $2, 'PENDING', NOW(), ARRAY[]::text[])
	`, jobID, req.ConnectionID); err != nil {
		return ScheduleResult{}, fmt.Errorf("backend scheduler: insert connection sync job: %w", err)
	}

	payload, err := connectionsync.Marshal(connectionsync.TaskPayload{
		JobID:          jobID,
		ConnectionID:   req.ConnectionID,
		ConnectionName: connectionName,
		OrgID:          req.OrgID,
	})
	if err != nil {
		_ = s.markConnectionSyncJobFailedAfterEnqueueError(ctx, jobID, req.ConnectionID, err)
		return ScheduleResult{}, fmt.Errorf("backend scheduler: marshal connection sync payload: %w", err)
	}
	task := asynq.NewTask(asynqueues.QueueConnectionSync, payload)
	if _, err := s.asynq.EnqueueContext(ctx, task, asynq.Queue(asynqueues.QueueConnectionSync)); err != nil {
		_ = s.markConnectionSyncJobFailedAfterEnqueueError(ctx, jobID, req.ConnectionID, err)
		return ScheduleResult{}, fmt.Errorf("backend scheduler: enqueue connection sync: %w", err)
	}
	return ScheduleResult{JobID: jobID}, nil
}

func (s *Service) markConnectionSyncJobFailedAfterEnqueueError(ctx context.Context, jobID string, connectionID int32, cause error) error {
	if cause == nil {
		cause = errors.New("enqueue failed")
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE "ConnectionSyncJob"
		SET status = 'FAILED',
		    "errorMessage" = $3,
		    "completedAt" = NOW(),
		    "updatedAt" = NOW()
		WHERE id = $1
		  AND "connectionId" = $2
		  AND status = 'PENDING'
	`, jobID, connectionID, "enqueue failed before worker delivery: "+cause.Error()); err != nil {
		return fmt.Errorf("backend scheduler: mark connection sync enqueue failure: %w", err)
	}
	return nil
}
