//go:build legacy_api_asynq

package api

import (
	"context"
	"fmt"

	"codeintel/pkg/asynqueues"
	"codeintel/pkg/connectionsync"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// asyncqQuerier is the narrow pgx surface AsynqConnectionSyncer
// uses. *pgxpool.Pool satisfies it directly; pgxmock satisfies
// it for unit tests.
type asyncqQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// AsynqConnectionSyncer is the production ConnectionSyncer
// implementation. POST /api/connections/{id}/sync delegates here
// (via the api.Config.ConnectionSyncer field); each invocation
// inserts a ConnectionSyncJob row in PENDING status and enqueues
// an asynq.Task on the connection-sync-queue. The
// codeintel-backend worker (connectionmanager.Handler) picks it
// up.
//
// Direct port of the relevant slice of legacy
// ConnectionManager.createJobs (connectionManager.ts:60-128): we
// keep the row-then-enqueue ordering (so the row exists by the
// time the worker tries to update its status), but skip the
// per-org tenant-capacity check (legacy gated on a brand-
// prefixed `TENANT_MAX_ACTIVE_CONNECTION_SYNCS` env var) — that
// capacity feature is deferred to a later slice; it requires a
// separate config knob + a SELECT COUNT over active jobs.
type AsynqConnectionSyncer struct {
	db    asyncqQuerier
	asynq *asynq.Client
}

// NewAsynqConnectionSyncer wires the syncer to a live pool +
// asynq client. The caller (codeintel-app main.go) owns the
// asynq.Client's lifecycle.
func NewAsynqConnectionSyncer(db asyncqQuerier, ac *asynq.Client) *AsynqConnectionSyncer {
	return &AsynqConnectionSyncer{db: db, asynq: ac}
}

// Schedule satisfies the ConnectionSyncer interface. Inserts a
// fresh ConnectionSyncJob row with a uuid id + status=PENDING,
// then enqueues an asynq task pointing at that row. Returns the
// jobID on success.
//
// Failure paths:
//   - Connection lookup error                       → wrapped error.
//   - ConnectionSyncJob insert error                → wrapped error.
//   - asynq.Enqueue error after the row landed      → wrapped error.
//     The row exists but the worker won't run it; future operator
//     cleanup OR a re-POST is the recovery path. We do NOT roll
//     back the row to keep behaviour predictable + auditable.
//
// AlreadyAtCapacity is always false in this slice — the
// per-tenant cap (legacy) lands in a follow-up slice.
func (s *AsynqConnectionSyncer) Schedule(ctx context.Context, req SyncRequest) (SyncResult, error) {
	// Read Connection.name so the task payload + future logs
	// carry the human-readable identifier (matches legacy
	// JobPayload.connectionName field).
	var connectionName string
	if err := s.db.QueryRow(ctx, `
		SELECT name FROM "Connection" WHERE id = $1 AND "orgId" = $2
	`, req.ConnectionID, req.OrgID).Scan(&connectionName); err != nil {
		return SyncResult{}, fmt.Errorf("AsynqConnectionSyncer: load Connection: %w", err)
	}

	jobID := uuid.NewString()
	if _, err := s.db.Exec(ctx, `
		INSERT INTO "ConnectionSyncJob" (id, "connectionId", status, "updatedAt", "warningMessages")
		VALUES ($1, $2, 'PENDING', NOW(), ARRAY[]::text[])
	`, jobID, req.ConnectionID); err != nil {
		return SyncResult{}, fmt.Errorf("AsynqConnectionSyncer: insert job row: %w", err)
	}

	payload, err := connectionsync.Marshal(connectionsync.TaskPayload{
		JobID:          jobID,
		ConnectionID:   req.ConnectionID,
		ConnectionName: connectionName,
		OrgID:          req.OrgID,
	})
	if err != nil {
		return SyncResult{}, fmt.Errorf("AsynqConnectionSyncer: marshal payload: %w", err)
	}

	task := asynq.NewTask(asynqueues.QueueConnectionSync, payload)
	if _, err := s.asynq.EnqueueContext(ctx, task,
		asynq.Queue(asynqueues.QueueConnectionSync),
	); err != nil {
		return SyncResult{}, fmt.Errorf("AsynqConnectionSyncer: enqueue: %w", err)
	}

	return SyncResult{JobID: jobID}, nil
}
