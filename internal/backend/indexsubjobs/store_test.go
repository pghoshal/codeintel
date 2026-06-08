package indexsubjobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestUpsertQueued(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-ws-1"
	language := "go"
	projectRoot := "services/api"
	indexer := "scip-go"
	mock.ExpectExec(`INSERT INTO "CodeIntelIndexSubjob"`).
		WithArgs(
			"subjob-1", "job-1", (*string)(nil), int32(7), &workspaceID,
			int32(42), "refs/heads/main", "refs/heads/main", strings.Repeat("a", 40), "SCIP", &language,
			&projectRoot, &indexer, "scip-go", "codeintel-index-scip-go", "QUEUED", int32(3),
			(*string)(nil), (*string)(nil), (*string)(nil), "", "", nil,
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	s := NewStore(mock)
	err = s.UpsertQueued(context.Background(), CreateInput{
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
	})
	if err != nil {
		t.Fatalf("UpsertQueued: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestUpsertQueuedReturnsScopeErrorOnZeroRows(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-ws-1"
	mock.ExpectExec(`INSERT INTO "CodeIntelIndexSubjob"`).
		WithArgs(anyArgs(23)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	err = NewStore(mock).UpsertQueued(context.Background(), CreateInput{
		ID:                "subjob-1",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             LayerASTTreeSitter,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
	})
	if !errors.Is(err, ErrScopeNotReady) {
		t.Fatalf("err = %v want ErrScopeNotReady", err)
	}
}

func TestUpsertSkipped(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-ws-1"
	language := "go"
	projectRoot := ""
	indexer := "scip-go"
	mock.ExpectExec(`INSERT INTO "CodeIntelIndexSubjob"`).
		WithArgs(anyArgs(23)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err = NewStore(mock).UpsertSkipped(context.Background(), CreateInput{
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
	}, "WORKER_CLASS_UNAVAILABLE", "SCIP worker class is not deployed")
	if err != nil {
		t.Fatalf("UpsertSkipped: %v", err)
	}
}

func TestUpsertQueuedRejectsInvalidInput(t *testing.T) {
	err := NewStore(nil).UpsertQueued(context.Background(), CreateInput{})
	if !errors.Is(err, ErrInvalidCreateInput) {
		t.Fatalf("error = %v want ErrInvalidCreateInput", err)
	}
}

func TestUpsertQueuedRejectsQueueClassMismatch(t *testing.T) {
	workspaceID := "atom-ws-1"
	language := "go"
	err := NewStore(nil).UpsertQueued(context.Background(), CreateInput{
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
		WorkerClass:       "scip-go",
		QueueName:         "codeintel-index-scip-jvm",
	})
	if !errors.Is(err, ErrInvalidCreateInput) {
		t.Fatalf("error = %v want ErrInvalidCreateInput", err)
	}
}

func TestUpsertQueuedRejectsMalformedLayerFields(t *testing.T) {
	workspaceID := "atom-ws-1"
	language := "go"
	badRoot := "../outside"
	indexer := "scip-go"
	for _, tc := range []CreateInput{
		{
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
			ProjectRoot:       &badRoot,
			Indexer:           &indexer,
			WorkerClass:       "scip-go",
			QueueName:         "codeintel-index-scip-go",
		},
		{
			ID:                "subjob-2",
			RepoIndexingJobID: "job-1",
			OrgID:             7,
			WorkspaceID:       &workspaceID,
			RepoID:            42,
			Branch:            "refs/heads/main",
			Revision:          "refs/heads/main",
			CommitHash:        strings.Repeat("a", 40),
			Layer:             LayerGraphMerge,
			Language:          &language,
			WorkerClass:       "core",
			QueueName:         "codeintel-index-core",
		},
	} {
		err := NewStore(nil).UpsertQueued(context.Background(), tc)
		if !errors.Is(err, ErrInvalidCreateInput) {
			t.Fatalf("input %+v error = %v want ErrInvalidCreateInput", tc, err)
		}
	}
}

func TestListDispatchable(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-ws-1"
	language := "go"
	projectRoot := "services/api"
	indexer := "scip-go"
	rows := pgxmock.NewRows([]string{
		"id", "repoIndexingJobId", "orgId", "workspaceId",
		"repoId", "branch", "revision", "commitHash", "layer",
		"language", "projectRoot", "indexer", "workerClass",
		"queueName", "attempt",
	}).AddRow(
		"subjob-1", "job-1", int32(7), &workspaceID,
		int32(42), "refs/heads/main", "refs/heads/main", strings.Repeat("a", 40), "SCIP",
		&language, &projectRoot, &indexer, "scip-go",
		"codeintel-index-scip-go", int32(1),
	)
	mock.ExpectQuery(`SELECT s.id, s."repoIndexingJobId"`).
		WithArgs(int32(20)).
		WillReturnRows(rows)

	got, err := NewStore(mock).ListDispatchable(context.Background(), 20)
	if err != nil {
		t.Fatalf("ListDispatchable: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d want 1", len(got))
	}
	if got[0].ID != "subjob-1" || got[0].WorkerClass != "scip-go" || got[0].Layer != LayerSCIP {
		t.Fatalf("unexpected row: %+v", got[0])
	}
	if got[0].WorkspaceID == nil || *got[0].WorkspaceID != workspaceID {
		t.Fatalf("workspaceID = %+v", got[0].WorkspaceID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestClaimHeartbeatAndArtifactTransitions(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-ws-1"
	language := "go"
	projectRoot := "services/api"
	indexer := "scip-go"
	scope := ClaimScope{
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
	}
	leaseUntil := time.Unix(1000, 0).UTC()
	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs(
			"subjob-1", "job-1", int32(7), &workspaceID,
			int32(42), "refs/heads/main", "refs/heads/main", strings.Repeat("a", 40),
			"SCIP", &language, &projectRoot, &indexer,
			"scip-go", "codeintel-index-scip-go", "worker-a",
			"attempt-1", leaseUntil, int32(0),
		).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs("subjob-1", "worker-a", leaseUntil.Add(time.Minute), "attempt-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs("subjob-1", "worker-a", "/efs/tmp/a", "/efs/artifacts/a.scip", "sha256:a", "attempt-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	ok, err := s.ClaimScoped(context.Background(), scope, "worker-a", "attempt-1", leaseUntil)
	if err != nil || !ok {
		t.Fatalf("ClaimScoped ok=%v err=%v", ok, err)
	}
	ok, err = s.Heartbeat(context.Background(), "subjob-1", "worker-a", "attempt-1", leaseUntil.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Heartbeat ok=%v err=%v", ok, err)
	}
	ok, err = s.MarkArtifactWritten(context.Background(), "subjob-1", "worker-a", "attempt-1", "/efs/tmp/a", "/efs/artifacts/a.scip", "sha256:a")
	if err != nil || !ok {
		t.Fatalf("MarkArtifactWritten ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestClaimReturnsFalseWhenLeaseNotAcquired(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-ws-1"
	scope := ClaimScope{
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
		Attempt:           1,
	}
	leaseUntil := time.Unix(1000, 0).UTC()
	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs(
			"subjob-1", "job-1", int32(7), &workspaceID,
			int32(42), "refs/heads/main", "refs/heads/main", strings.Repeat("a", 40),
			"ZOEKT", (*string)(nil), (*string)(nil), (*string)(nil),
			"core", "codeintel-index-core", "worker-a",
			"attempt-1", leaseUntil, int32(0),
		).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	ok, err := NewStore(mock).ClaimScoped(context.Background(), scope, "worker-a", "attempt-1", leaseUntil)
	if err != nil {
		t.Fatalf("ClaimScoped: %v", err)
	}
	if ok {
		t.Fatal("ClaimScoped returned true for 0 rows")
	}
}

func TestTerminalTransitions(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs("subjob-1", "worker-a", "attempt-1").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs("subjob-2", "worker-b", "attempt-2", "OOM", "pod exceeded memory limit").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`WITH failed_subjob AS`).
		WithArgs("subjob-2", "OOM", "pod exceeded memory limit").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs("subjob-3", "worker-c", "attempt-3", "EXECUTOR_UNAVAILABLE", "executor service is scaling").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`WITH failed_subjob AS`).
		WithArgs("subjob-3", "EXECUTOR_UNAVAILABLE", "executor service is scaling").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	if ok, err := s.MarkSucceeded(context.Background(), "subjob-1", "worker-a", "attempt-1"); err != nil || !ok {
		t.Fatalf("MarkSucceeded ok=%v err=%v", ok, err)
	}
	if ok, err := s.MarkFailed(context.Background(), "subjob-2", "worker-b", "attempt-2", "OOM", "pod exceeded memory limit"); err != nil || !ok {
		t.Fatalf("MarkFailed ok=%v err=%v", ok, err)
	}
	if ok, err := s.MarkRetryableInfrastructureFailure(context.Background(), "subjob-3", "worker-c", "attempt-3", "EXECUTOR_UNAVAILABLE", "executor service is scaling"); err != nil || !ok {
		t.Fatalf("MarkRetryableInfrastructureFailure ok=%v err=%v", ok, err)
	}
}

func TestTerminalSCIPExhaustionBecomesSkipped(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`WHEN layer IN \('SCIP', 'AST_TREE_SITTER'\) THEN 'SKIPPED'`).
		WithArgs("subjob-scip", "worker-a", "attempt-3", "OOM", "pod exceeded memory limit").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`WITH failed_subjob AS`).
		WithArgs("subjob-scip", "OOM", "pod exceeded memory limit").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`WHEN layer IN \('SCIP', 'AST_TREE_SITTER'\) THEN 'SKIPPED'`).
		WithArgs("subjob-scip-infra", "worker-b", "attempt-3", "EXECUTOR_UNAVAILABLE", "executor restarted").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`WITH failed_subjob AS`).
		WithArgs("subjob-scip-infra", "EXECUTOR_UNAVAILABLE", "executor restarted").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	s := NewStore(mock)
	if ok, err := s.MarkFailed(context.Background(), "subjob-scip", "worker-a", "attempt-3", "OOM", "pod exceeded memory limit"); err != nil || !ok {
		t.Fatalf("MarkFailed SCIP ok=%v err=%v", ok, err)
	}
	if ok, err := s.MarkRetryableInfrastructureFailure(context.Background(), "subjob-scip-infra", "worker-b", "attempt-3", "EXECUTOR_UNAVAILABLE", "executor restarted"); err != nil || !ok {
		t.Fatalf("MarkRetryableInfrastructureFailure SCIP ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestTerminalASTTreeSitterExhaustionBecomesSkipped(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`WHEN layer IN \('SCIP', 'AST_TREE_SITTER'\) THEN 'SKIPPED'`).
		WithArgs("subjob-ast", "worker-a", "attempt-3", "ARTIFACT_INGEST_FAILED", "payload validation: statement count 30000 exceeds 20000").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`WITH failed_subjob AS`).
		WithArgs("subjob-ast", "ARTIFACT_INGEST_FAILED", "payload validation: statement count 30000 exceeds 20000").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	if ok, err := NewStore(mock).MarkFailed(context.Background(), "subjob-ast", "worker-a", "attempt-3", "ARTIFACT_INGEST_FAILED", "payload validation: statement count 30000 exceeds 20000"); err != nil || !ok {
		t.Fatalf("MarkFailed AST ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMarkFailedNormalizesBlankDiagnostic(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs("subjob-blank", "worker-a", "attempt-3", "SUBJOB_FAILED", "subjob failed without diagnostic").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`WITH failed_subjob AS`).
		WithArgs("subjob-blank", "SUBJOB_FAILED", "subjob failed without diagnostic").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	if ok, err := NewStore(mock).MarkFailed(context.Background(), "subjob-blank", "worker-a", "attempt-3", "", "   "); err != nil || !ok {
		t.Fatalf("MarkFailed blank ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestSCIPTimeoutSkipsWithoutRetrying(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`WHEN layer = 'SCIP' AND \$5 ILIKE '%timeout after%' THEN 'SKIPPED'`).
		WithArgs("subjob-scip-timeout", "worker-a", "attempt-1", "EXECUTION_FAILED", "timeout after 300s").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`WITH failed_subjob AS`).
		WithArgs("subjob-scip-timeout", "EXECUTION_FAILED", "timeout after 300s").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	if ok, err := NewStore(mock).MarkFailed(context.Background(), "subjob-scip-timeout", "worker-a", "attempt-1", "EXECUTION_FAILED", "timeout after 300s"); err != nil || !ok {
		t.Fatalf("MarkFailed SCIP timeout ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestSCIPHollowArtifactSkipsWithoutRetrying(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	message := "indexer produced semantically empty .scip file at /tmp/index.scip: 0 symbols, 0 occurrences, 0 relationships"
	mock.ExpectExec(`semantically empty \.scip`).
		WithArgs("subjob-scip-hollow", "worker-a", "attempt-1", "EXECUTION_FAILED", message).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`WITH failed_subjob AS`).
		WithArgs("subjob-scip-hollow", "EXECUTION_FAILED", message).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	if ok, err := NewStore(mock).MarkFailed(context.Background(), "subjob-scip-hollow", "worker-a", "attempt-1", "EXECUTION_FAILED", message); err != nil || !ok {
		t.Fatalf("MarkFailed SCIP hollow ok=%v err=%v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestReconcileTerminalFailures(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`WITH semantic_empty AS`).
		WithArgs(int32(50)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	got, err := NewStore(mock).ReconcileTerminalFailures(context.Background(), 50)
	if err != nil {
		t.Fatalf("ReconcileTerminalFailures: %v", err)
	}
	if got != 1 {
		t.Fatalf("rows = %d want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestReconcileTerminalFailuresSkipsSemanticEmptyRetryRows(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`WITH semantic_empty AS`).
		WithArgs(int32(50)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	if _, err := NewStore(mock).ReconcileTerminalFailures(context.Background(), 50); err != nil {
		t.Fatalf("ReconcileTerminalFailures: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

func TestDispatchLeaseLock(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	leaseUntil := time.Unix(3000, 0).UTC()
	mock.ExpectQuery(`INSERT INTO "CodeIntelIndexSubjobDispatchLock"`).
		WithArgs(dispatchLockID, "owner-a", leaseUntil).
		WillReturnRows(pgxmock.NewRows([]string{"locked"}).AddRow(1))
	mock.ExpectExec(`DELETE FROM "CodeIntelIndexSubjobDispatchLock"`).
		WithArgs(dispatchLockID, "owner-a").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	store := NewStore(mock)
	ok, err := store.TryAcquireDispatchLock(context.Background(), "owner-a", leaseUntil)
	if err != nil || !ok {
		t.Fatalf("TryAcquireDispatchLock ok=%v err=%v", ok, err)
	}
	if err := store.ReleaseDispatchLock(context.Background(), "owner-a"); err != nil {
		t.Fatalf("ReleaseDispatchLock: %v", err)
	}
}

func TestRequeueExpiredLeases(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Unix(2000, 0).UTC()
	mock.ExpectExec(`WITH expired AS`).
		WithArgs(now, int32(50)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 3))

	updated, err := NewStore(mock).RequeueExpiredLeases(context.Background(), now, 50)
	if err != nil {
		t.Fatalf("RequeueExpiredLeases: %v", err)
	}
	if updated != 3 {
		t.Fatalf("updated = %d want 3", updated)
	}
}

func TestRequeueLeasesForOwnerPrefixes(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	now := time.Unix(2100, 0).UTC()
	prefixes := []string{"index-executor-pod-a-", "index-core-pod-a-"}
	mock.ExpectExec(`WITH rescued AS`).
		WithArgs(prefixes, int32(50), now).
		WillReturnResult(pgxmock.NewResult("UPDATE", 4))

	updated, err := NewStore(mock).RequeueLeasesForOwnerPrefixes(context.Background(), prefixes, now, 50)
	if err != nil {
		t.Fatalf("RequeueLeasesForOwnerPrefixes: %v", err)
	}
	if updated != 4 {
		t.Fatalf("updated = %d want 4", updated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestRequeueLeasesForOwnerPrefixesRejectsEmptyScope(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	if _, err := NewStore(mock).RequeueLeasesForOwnerPrefixes(context.Background(), []string{" "}, time.Unix(1, 0).UTC(), 50); err == nil {
		t.Fatal("expected empty prefix list to be rejected")
	}
}
