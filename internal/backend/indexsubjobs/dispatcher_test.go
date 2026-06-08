package indexsubjobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"codeintel/internal/backend/indexsubjobtask"

	"github.com/hibiken/asynq"
)

func TestDispatchReadyEnqueuesClassQueuePayloads(t *testing.T) {
	workspaceID := "atom-ws-1"
	language := "go"
	projectRoot := ""
	indexer := "scip-go"
	store := &fakeDispatchStore{
		rows: []DispatchableSubjob{{
			ID:                "subjob-1",
			RepoIndexingJobID: "job-1",
			OrgID:             7,
			WorkspaceID:       &workspaceID,
			RepoID:            42,
			Branch:            "refs/heads/main",
			Revision:          "refs/heads/main",
			CommitHash:        strings.Repeat("a", 40),
			Layer:             LayerSCIP,
			Language:          &language,
			ProjectRoot:       &projectRoot,
			Indexer:           &indexer,
			WorkerClass:       "scip-go",
			QueueName:         "codeintel-index-scip-go",
			Attempt:           1,
		}},
	}
	enq := &fakeEnqueuer{}

	stats, err := NewDispatcher(store, enq).DispatchReady(context.Background(), 25)
	if err != nil {
		t.Fatalf("DispatchReady: %v", err)
	}
	if stats.Scanned != 1 || stats.Enqueued != 1 || stats.Duplicates != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if store.limit != 25 {
		t.Fatalf("limit = %d want 25", store.limit)
	}
	if len(enq.tasks) != 1 {
		t.Fatalf("tasks = %d want 1", len(enq.tasks))
	}
	if enq.tasks[0].Type() != "codeintel-index-scip-go" {
		t.Fatalf("task type = %q", enq.tasks[0].Type())
	}
	payload, err := indexsubjobtask.Unmarshal(enq.tasks[0].Payload())
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.SubjobID != "subjob-1" || payload.WorkerClass != "scip-go" || payload.Attempt != 2 {
		t.Fatalf("payload = %+v", payload)
	}
	assertOption(t, enq.opts[0], asynq.QueueOpt, "codeintel-index-scip-go")
	assertOption(t, enq.opts[0], asynq.MaxRetryOpt, 0)
	assertOption(t, enq.opts[0], asynq.TaskIDOpt, dispatchTaskID("subjob-1", 2))
}

func TestDispatchReadyTreatsDuplicateTaskAsSuccess(t *testing.T) {
	workspaceID := "atom-ws-1"
	store := &fakeDispatchStore{rows: []DispatchableSubjob{{
		ID:                "subjob-1",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             LayerZoekt,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
	}}}
	enq := &fakeEnqueuer{err: asynq.ErrTaskIDConflict}
	stats, err := NewDispatcher(store, enq).DispatchReady(context.Background(), 10)
	if err != nil {
		t.Fatalf("DispatchReady: %v", err)
	}
	if stats.Enqueued != 0 || stats.Duplicates != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestDispatchReadyRescuesDuplicateTaskIDConflict(t *testing.T) {
	workspaceID := "atom-ws-1"
	store := &fakeDispatchStore{rows: []DispatchableSubjob{{
		ID:                "subjob-1",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             LayerZoekt,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
	}}}
	enq := &fakeEnqueuer{errs: []error{asynq.ErrTaskIDConflict, nil}}
	stats, err := NewDispatcher(store, enq).DispatchReady(context.Background(), 10)
	if err != nil {
		t.Fatalf("DispatchReady: %v", err)
	}
	if stats.Enqueued != 0 || stats.Duplicates != 1 || stats.RescuedDuplicates != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if len(enq.tasks) != 1 {
		t.Fatalf("rescue tasks = %d want 1", len(enq.tasks))
	}
	assertOption(t, enq.opts[0], asynq.UniqueOpt, rescueTaskUniquenessTTL)
	assertOption(t, enq.opts[0], asynq.RetentionOpt, time.Duration(0))
	payload, err := indexsubjobtask.Unmarshal(enq.tasks[0].Payload())
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.SubjobID != "subjob-1" || payload.Attempt != 1 {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestRequeueExpiredAndDispatch(t *testing.T) {
	now := time.Unix(2000, 0).UTC()
	workspaceID := "atom-ws-1"
	store := &fakeDispatchStore{
		expired:    2,
		reconciled: 1,
		lockOK:     true,
		rows: []DispatchableSubjob{{
			ID:                "subjob-1",
			RepoIndexingJobID: "job-1",
			OrgID:             7,
			WorkspaceID:       &workspaceID,
			RepoID:            42,
			Branch:            "refs/heads/main",
			Revision:          "refs/heads/main",
			CommitHash:        strings.Repeat("a", 40),
			Layer:             LayerZoekt,
			WorkerClass:       "core",
			QueueName:         "codeintel-index-core",
		}},
	}
	stats, err := NewDispatcher(store, &fakeEnqueuer{}).RequeueExpiredAndDispatch(context.Background(), now, 50, 10)
	if err != nil {
		t.Fatalf("RequeueExpiredAndDispatch: %v", err)
	}
	if store.now != now || store.leaseLimit != 50 {
		t.Fatalf("requeue args now=%v limit=%d", store.now, store.leaseLimit)
	}
	if store.lockOwner == "" || store.releaseOwner != store.lockOwner {
		t.Fatalf("lock owner mismatch lock=%q release=%q", store.lockOwner, store.releaseOwner)
	}
	if !store.locked || !store.released {
		t.Fatalf("dispatcher did not acquire/release lock: locked=%v released=%v", store.locked, store.released)
	}
	if stats.ExpiredRequeued != 2 || stats.TerminalFailuresReconciled != 1 || stats.Enqueued != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestRequeueExpiredAndDispatchNoopsWithoutLock(t *testing.T) {
	store := &fakeDispatchStore{lockOK: false}
	stats, err := NewDispatcher(store, &fakeEnqueuer{}).RequeueExpiredAndDispatch(context.Background(), time.Unix(1, 0), 50, 10)
	if err != nil {
		t.Fatalf("RequeueExpiredAndDispatch: %v", err)
	}
	if stats != (DispatchStats{}) {
		t.Fatalf("stats = %+v want zero", stats)
	}
	if store.expiredCalled {
		t.Fatal("RequeueExpiredLeases called without lock")
	}
}

func TestDispatchReadyRejectsQueueMismatch(t *testing.T) {
	workspaceID := "atom-ws-1"
	store := &fakeDispatchStore{rows: []DispatchableSubjob{{
		ID:                "subjob-1",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             LayerZoekt,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-scip-go",
	}}}
	_, err := NewDispatcher(store, &fakeEnqueuer{}).DispatchReady(context.Background(), 10)
	if err == nil {
		t.Fatal("DispatchReady accepted workerClass/queue mismatch")
	}
}

func TestDispatcherRequiresConfig(t *testing.T) {
	if _, err := NewDispatcher(nil, nil).DispatchReady(context.Background(), 1); err == nil {
		t.Fatal("DispatchReady nil config returned nil error")
	}
}

type fakeDispatchStore struct {
	rows          []DispatchableSubjob
	limit         int32
	expired       int64
	reconciled    int64
	now           time.Time
	leaseLimit    int32
	lockOK        bool
	locked        bool
	released      bool
	lockOwner     string
	releaseOwner  string
	expiredCalled bool
}

func (s *fakeDispatchStore) ListDispatchable(_ context.Context, limit int32) ([]DispatchableSubjob, error) {
	s.limit = limit
	return s.rows, nil
}

func (s *fakeDispatchStore) RequeueExpiredLeases(_ context.Context, now time.Time, limit int32) (int64, error) {
	s.expiredCalled = true
	s.now = now
	s.leaseLimit = limit
	return s.expired, nil
}

func (s *fakeDispatchStore) ReconcileTerminalFailures(_ context.Context, limit int32) (int64, error) {
	s.leaseLimit = limit
	return s.reconciled, nil
}

func (s *fakeDispatchStore) TryAcquireDispatchLock(_ context.Context, owner string, _ time.Time) (bool, error) {
	s.locked = true
	s.lockOwner = owner
	if !s.lockOK {
		return false, nil
	}
	return true, nil
}

func (s *fakeDispatchStore) ReleaseDispatchLock(_ context.Context, owner string) error {
	s.released = true
	s.releaseOwner = owner
	return nil
}

type fakeEnqueuer struct {
	tasks []*asynq.Task
	opts  [][]asynq.Option
	err   error
	errs  []error
}

func (e *fakeEnqueuer) EnqueueContext(_ context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	if len(e.errs) > 0 {
		err := e.errs[0]
		e.errs = e.errs[1:]
		if err != nil {
			return nil, err
		}
	}
	if e.err != nil {
		return nil, e.err
	}
	if task == nil {
		return nil, errors.New("nil task")
	}
	e.tasks = append(e.tasks, task)
	e.opts = append(e.opts, opts)
	return &asynq.TaskInfo{ID: "task-1", Queue: task.Type(), Type: task.Type(), Payload: task.Payload()}, nil
}

func assertOption(t *testing.T, opts []asynq.Option, typ asynq.OptionType, want any) {
	t.Helper()
	for _, opt := range opts {
		if opt.Type() == typ {
			if opt.Value() != want {
				t.Fatalf("option %v value = %#v want %#v", typ, opt.Value(), want)
			}
			return
		}
	}
	t.Fatalf("missing option type %v", typ)
}
