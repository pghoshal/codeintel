package indexsubjobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/internal/backend/workerclasses"

	"github.com/hibiken/asynq"
)

type dispatchStore interface {
	ListDispatchable(context.Context, int32) ([]DispatchableSubjob, error)
	RequeueExpiredLeases(context.Context, time.Time, int32) (int64, error)
	ReconcileTerminalFailures(context.Context, int32) (int64, error)
	TryAcquireDispatchLock(context.Context, string, time.Time) (bool, error)
	ReleaseDispatchLock(context.Context, string) error
}

type taskEnqueuer interface {
	EnqueueContext(context.Context, *asynq.Task, ...asynq.Option) (*asynq.TaskInfo, error)
}

type Dispatcher struct {
	store    dispatchStore
	enqueuer taskEnqueuer
}

const rescueTaskUniquenessTTL = 10 * time.Minute

type DispatchStats struct {
	ExpiredRequeued            int64
	TerminalFailuresReconciled int64
	Scanned                    int
	Enqueued                   int
	Duplicates                 int
	RescuedDuplicates          int
}

func NewDispatcher(store dispatchStore, enqueuer taskEnqueuer) *Dispatcher {
	return &Dispatcher{store: store, enqueuer: enqueuer}
}

func (d *Dispatcher) RequeueExpiredAndDispatch(ctx context.Context, now time.Time, leaseLimit, dispatchLimit int32) (DispatchStats, error) {
	if d == nil || d.store == nil || d.enqueuer == nil {
		return DispatchStats{}, errors.New("indexsubjobs: dispatcher not configured")
	}
	owner := dispatchOwner(now)
	locked, err := d.store.TryAcquireDispatchLock(ctx, owner, now.Add(30*time.Second))
	if err != nil {
		return DispatchStats{}, err
	}
	if !locked {
		return DispatchStats{}, nil
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = d.store.ReleaseDispatchLock(releaseCtx, owner)
	}()
	expired, err := d.store.RequeueExpiredLeases(ctx, now, leaseLimit)
	if err != nil {
		return DispatchStats{}, err
	}
	reconciled, err := d.store.ReconcileTerminalFailures(ctx, leaseLimit)
	if err != nil {
		return DispatchStats{}, err
	}
	stats, err := d.DispatchReady(ctx, dispatchLimit)
	if err != nil {
		return stats, err
	}
	stats.ExpiredRequeued = expired
	stats.TerminalFailuresReconciled = reconciled
	return stats, nil
}

func (d *Dispatcher) DispatchReady(ctx context.Context, limit int32) (DispatchStats, error) {
	if d == nil || d.store == nil || d.enqueuer == nil {
		return DispatchStats{}, errors.New("indexsubjobs: dispatcher not configured")
	}
	rows, err := d.store.ListDispatchable(ctx, limit)
	if err != nil {
		return DispatchStats{}, err
	}
	stats := DispatchStats{Scanned: len(rows)}
	for _, row := range rows {
		if err := d.enqueue(ctx, row); err != nil {
			if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
				stats.Duplicates++
				if rescueErr := d.enqueueRescue(ctx, row); rescueErr != nil {
					if errors.Is(rescueErr, asynq.ErrTaskIDConflict) || errors.Is(rescueErr, asynq.ErrDuplicateTask) {
						continue
					}
					return stats, rescueErr
				}
				stats.RescuedDuplicates++
				continue
			}
			return stats, err
		}
		stats.Enqueued++
	}
	return stats, nil
}

func (d *Dispatcher) enqueue(ctx context.Context, row DispatchableSubjob) error {
	nextAttempt := row.Attempt + 1
	class, ok := workerclasses.ByName(row.WorkerClass)
	if !ok {
		return fmt.Errorf("subjob %s has unknown worker class %q", row.ID, row.WorkerClass)
	}
	if row.QueueName != "" && row.QueueName != class.QueueName {
		return fmt.Errorf("subjob %s queue %q does not match worker class %q queue %q", row.ID, row.QueueName, row.WorkerClass, class.QueueName)
	}
	payload, err := indexsubjobtask.Marshal(indexsubjobtask.Payload{
		SubjobID:          row.ID,
		RepoIndexingJobID: row.RepoIndexingJobID,
		OrgID:             row.OrgID,
		WorkspaceID:       row.WorkspaceID,
		RepoID:            row.RepoID,
		Branch:            row.Branch,
		Revision:          row.Revision,
		CommitHash:        row.CommitHash,
		Layer:             indexsubjobtask.Layer(row.Layer),
		Language:          row.Language,
		ProjectRoot:       row.ProjectRoot,
		Indexer:           row.Indexer,
		WorkerClass:       row.WorkerClass,
		QueueName:         class.QueueName,
		Attempt:           nextAttempt,
	})
	if err != nil {
		return fmt.Errorf("marshal subjob payload %s: %w", row.ID, err)
	}
	task := asynq.NewTask(class.QueueName, payload)
	_, err = d.enqueuer.EnqueueContext(ctx, task,
		asynq.Queue(class.QueueName),
		asynq.MaxRetry(0),
		asynq.Retention(0),
		asynq.TaskID(dispatchTaskID(row.ID, nextAttempt)),
	)
	if err != nil {
		return fmt.Errorf("enqueue subjob %s: %w", row.ID, err)
	}
	return nil
}

func (d *Dispatcher) enqueueRescue(ctx context.Context, row DispatchableSubjob) error {
	nextAttempt := row.Attempt + 1
	class, ok := workerclasses.ByName(row.WorkerClass)
	if !ok {
		return fmt.Errorf("subjob %s has unknown worker class %q", row.ID, row.WorkerClass)
	}
	if row.QueueName != "" && row.QueueName != class.QueueName {
		return fmt.Errorf("subjob %s queue %q does not match worker class %q queue %q", row.ID, row.QueueName, row.WorkerClass, class.QueueName)
	}
	payload, err := indexsubjobtask.Marshal(indexsubjobtask.Payload{
		SubjobID:          row.ID,
		RepoIndexingJobID: row.RepoIndexingJobID,
		OrgID:             row.OrgID,
		WorkspaceID:       row.WorkspaceID,
		RepoID:            row.RepoID,
		Branch:            row.Branch,
		Revision:          row.Revision,
		CommitHash:        row.CommitHash,
		Layer:             indexsubjobtask.Layer(row.Layer),
		Language:          row.Language,
		ProjectRoot:       row.ProjectRoot,
		Indexer:           row.Indexer,
		WorkerClass:       row.WorkerClass,
		QueueName:         class.QueueName,
		Attempt:           nextAttempt,
	})
	if err != nil {
		return fmt.Errorf("marshal rescue subjob payload %s: %w", row.ID, err)
	}
	task := asynq.NewTask(class.QueueName, payload)
	_, err = d.enqueuer.EnqueueContext(ctx, task,
		asynq.Queue(class.QueueName),
		asynq.MaxRetry(0),
		asynq.Retention(0),
		asynq.Unique(rescueTaskUniquenessTTL),
	)
	if err != nil {
		return fmt.Errorf("enqueue rescue subjob %s: %w", row.ID, err)
	}
	return nil
}

func dispatchTaskID(subjobID string, attempt int32) string {
	return fmt.Sprintf("codeintel-index-subjob:%s:%d", subjobID, attempt)
}

func dispatchOwner(now time.Time) string {
	hostname, _ := os.Hostname()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Sprintf("dispatcher-%s-%d", hostname, now.UnixNano())
	}
	return fmt.Sprintf("dispatcher-%s-%d-%s", hostname, now.UnixNano(), hex.EncodeToString(nonce[:]))
}
