package repoindexmanager

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeintel/pkg/graphschema"
	"codeintel/pkg/repoindex"
	"codeintel/pkg/repopaths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

const jobID = "11112222-3333-4444-5555-666677778888"

func TestGitCloneDepthDefaultsToShallow(t *testing.T) {
	t.Setenv("CODEINTEL_GIT_CLONE_DEPTH", "")
	if got := gitCloneDepth(); got != 1 {
		t.Fatalf("gitCloneDepth default = %d, want 1", got)
	}
	t.Setenv("CODEINTEL_GIT_CLONE_DEPTH", "0")
	if got := gitCloneDepth(); got != 0 {
		t.Fatalf("gitCloneDepth explicit full clone = %d, want 0", got)
	}
}

func TestGitCloneTimeoutBounds(t *testing.T) {
	t.Setenv("CODEINTEL_GIT_CLONE_TIMEOUT_SECONDS", "")
	if got := gitCloneTimeout(); got != 10*time.Minute {
		t.Fatalf("gitCloneTimeout default = %s, want 10m", got)
	}
	t.Setenv("CODEINTEL_GIT_CLONE_TIMEOUT_SECONDS", "5")
	if got := gitCloneTimeout(); got != 30*time.Second {
		t.Fatalf("gitCloneTimeout floor = %s, want 30s", got)
	}
}

// TestMarkInProgress_HappyPath: PENDING row gets updated and
// the helper returns nil.
func TestMarkInProgress_HappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "RepoIndexingJob"`).
		WithArgs(jobID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.MarkInProgress(ctx, jobID); err != nil {
		t.Fatalf("MarkInProgress: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

// TestMarkInProgress_TerminalState: 0 rows matched -> typed
// sentinel surfaced.
func TestMarkInProgress_TerminalState(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`UPDATE "RepoIndexingJob"`).
		WithArgs(jobID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	s := NewStore(mock)
	err = s.MarkInProgress(context.Background(), jobID)
	if !errors.Is(err, ErrJobInTerminalState) {
		t.Errorf("got %v, want ErrJobInTerminalState", err)
	}
}

func TestFetchSemanticIndexHealthFlagsHollowReadyIndexes(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`WITH scip AS`).
		WithArgs(int32(7), int32(42), "atom-ws", "refs/heads/main", strings.Repeat("a", 40)).
		WillReturnRows(pgxmock.NewRows([]string{
			"scip_found",
			"symbols",
			"occurrences",
			"relationships",
			"graph_found",
			"anchors",
			"linked_edges",
		}).AddRow(true, int32(0), int32(0), int32(0), true, int32(0), int32(0)))

	s := NewStore(mock)
	health, err := s.FetchSemanticIndexHealth(context.Background(), 7, 42, "atom-ws", "refs/heads/main", strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("FetchSemanticIndexHealth: %v", err)
	}
	if !health.NeedsRepair() {
		t.Fatalf("hollow health should need repair: %#v", health)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestFetchJobScope(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(`SELECT r\."orgId", j\."repoId", j\.type::text`).
		WithArgs(jobID).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "repoId", "type"}).
			AddRow(int32(7), int32(42), "REMOVE_INDEX"))

	s := NewStore(mock)
	scope, err := s.FetchJobScope(context.Background(), jobID)
	if err != nil {
		t.Fatalf("FetchJobScope: %v", err)
	}
	if scope.OrgID != 7 || scope.RepoID != 42 || scope.Type != "REMOVE_INDEX" {
		t.Fatalf("scope mismatch: %+v", scope)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMarkInProgressScopedUsesOrgRepoAndTypeGuard(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`(?s)UPDATE "RepoIndexingJob" j\s+SET status = 'IN_PROGRESS'.*j\."repoId" = \$2.*j\.type = \$3::"RepoIndexingJobType".*r\."orgId" = \$4`).
		WithArgs(jobID, int32(42), "INDEX", int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	if err := s.MarkInProgressScoped(context.Background(), jobID, 7, 42, "INDEX"); err != nil {
		t.Fatalf("MarkInProgressScoped: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMarkCompletedScopedUsesOrgRepoAndTypeGuard(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`(?s)UPDATE "RepoIndexingJob" j\s+SET status = 'COMPLETED'.*j\."repoId" = \$2.*j\.type = \$3::"RepoIndexingJobType".*r\."orgId" = \$4`).
		WithArgs(jobID, int32(42), "REMOVE_INDEX", int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	if err := s.MarkCompletedScoped(context.Background(), jobID, 7, 42, "REMOVE_INDEX"); err != nil {
		t.Fatalf("MarkCompletedScoped: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMarkFailedScopedUsesOrgRepoAndTypeGuard(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`(?s)UPDATE "RepoIndexingJob" j\s+SET status\s+= 'FAILED'.*j\."repoId" = \$2.*j\.type = \$3::"RepoIndexingJobType".*r\."orgId" = \$4`).
		WithArgs(jobID, int32(42), "CLEANUP", int32(7), "boom").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	if err := s.MarkFailedScoped(context.Background(), jobID, 7, 42, "CLEANUP", "boom"); err != nil {
		t.Fatalf("MarkFailedScoped: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestScopedTransitionsReturnTerminalStateOnZeroRows(t *testing.T) {
	cases := []struct {
		name string
		run  func(*Store, context.Context) error
		sql  string
		args []any
	}{
		{
			name: "mark in progress",
			run:  func(s *Store, ctx context.Context) error { return s.MarkInProgressScoped(ctx, jobID, 7, 42, "INDEX") },
			sql:  `UPDATE "RepoIndexingJob" j\s+SET status = 'IN_PROGRESS'`,
			args: []any{jobID, int32(42), "INDEX", int32(7)},
		},
		{
			name: "mark completed",
			run:  func(s *Store, ctx context.Context) error { return s.MarkCompletedScoped(ctx, jobID, 7, 42, "INDEX") },
			sql:  `UPDATE "RepoIndexingJob" j\s+SET status = 'COMPLETED'`,
			args: []any{jobID, int32(42), "INDEX", int32(7)},
		},
		{
			name: "mark failed",
			run: func(s *Store, ctx context.Context) error {
				return s.MarkFailedScoped(ctx, jobID, 7, 42, "INDEX", "boom")
			},
			sql:  `UPDATE "RepoIndexingJob" j\s+SET status\s+= 'FAILED'`,
			args: []any{jobID, int32(42), "INDEX", int32(7), "boom"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock: %v", err)
			}
			defer mock.Close()
			mock.ExpectExec(`(?s)` + tc.sql).
				WithArgs(tc.args...).
				WillReturnResult(pgxmock.NewResult("UPDATE", 0))

			err = tc.run(NewStore(mock), context.Background())
			if !errors.Is(err, ErrJobInTerminalState) {
				t.Fatalf("err=%v want ErrJobInTerminalState", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet: %v", err)
			}
		})
	}
}

func TestHandleMarksJobFailedOnPayloadScopeMismatch(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(`SELECT r\."orgId", j\."repoId", j\.type::text`).
		WithArgs(jobID).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "repoId", "type"}).
			AddRow(int32(7), int32(42), "INDEX"))
	mock.ExpectExec(`(?s)UPDATE "RepoIndexingJob" j\s+SET status\s+= 'FAILED'.*j\."repoId" = \$2.*j\.type = \$3::"RepoIndexingJobType".*r\."orgId" = \$4`).
		WithArgs(jobID, int32(42), "INDEX", int32(7), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "Repo" r`).
		WithArgs(int32(42)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	raw, err := repoindex.Marshal(repoindex.TaskPayload{
		Type:   repoindex.JobTypeIndex,
		JobID:  jobID,
		RepoID: 42,
		OrgID:  8,
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	handler := NewHandler(NewStore(mock), repopaths.Config{DataCacheDir: t.TempDir()}, nil)
	err = handler.Handle(context.Background(), asynq.NewTask("repo-index-queue", raw))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("Handle err=%v want SkipRetry", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMarkCompleted(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`UPDATE "RepoIndexingJob"`).
		WithArgs(jobID).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	s := NewStore(mock)
	if err := s.MarkCompleted(context.Background(), jobID); err != nil {
		t.Fatalf("MarkCompleted: %v", err)
	}
}

func TestRecordUsableIndexedRevisionRefreshesRepoMetadataFromReadyManifest(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`(?s)UPDATE "Repo" r\s+SET metadata = jsonb_set.*FROM "RepoIndexManifest" m.*m\."workspaceId" = \$3.*m\.branch = \$4.*m\."commitHash" = \$5.*m\."supersededAt" IS NULL`).
		WithArgs(int32(7), int32(42), "atom-workspace", "refs/heads/release-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	if err := s.RecordUsableIndexedRevision(context.Background(), 7, 42, "atom-workspace", "refs/heads/release-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatalf("RecordUsableIndexedRevision: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMarkFailed(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`UPDATE "RepoIndexingJob"`).
		WithArgs(jobID, "boom").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	s := NewStore(mock)
	if err := s.MarkFailed(context.Background(), jobID, "boom"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
}

func TestRefreshRepoLatestIndexingJobStatus(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`UPDATE "Repo" r`).
		WithArgs(int32(42)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	s := NewStore(mock)
	if err := s.RefreshRepoLatestIndexingJobStatus(context.Background(), 42); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
}

func TestCleanupRepoDBStateClearsIndexedRevisionMetadata(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`DELETE FROM "CodeGraphIndex"`).
		WithArgs(int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(`DELETE FROM "CodeIntelIndex"`).
		WithArgs(int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(`DELETE FROM "RepoIndexManifest"`).
		WithArgs(int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(`SET\s+"indexedAt"\s+=\s+NULL,\s+"indexedCommitHash"\s+=\s+NULL,\s+"latestIndexingJobStatus"\s+=\s+NULL,\s+metadata\s+=\s+COALESCE\(metadata, '\{\}'::jsonb\) - 'indexedRevisions'`).
		WithArgs(int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	if err := s.CleanupRepoDBState(context.Background(), 7, 42); err != nil {
		t.Fatalf("CleanupRepoDBState: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestCleanupRepoDBStateForRefDeletesOnlySelectedRevision(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`WITH removed AS \(\s+DELETE FROM "CodeGraphRevision"`).
		WithArgs(int32(42), int32(7), []string{"release-a", "refs/heads/release-a"}).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(`DELETE FROM "CodeIntelIndex"`).
		WithArgs(int32(42), int32(7), []string{"release-a", "refs/heads/release-a"}).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(`DELETE FROM "RepoIndexManifest"`).
		WithArgs(int32(42), int32(7), []string{"release-a", "refs/heads/release-a"}).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(`jsonb_set`).
		WithArgs(int32(42), int32(7), []string{"release-a", "refs/heads/release-a"}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewStore(mock)
	if err := s.CleanupRepoDBStateForRef(context.Background(), 7, 42, "release-a"); err != nil {
		t.Fatalf("CleanupRepoDBStateForRef: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestListGraphSnapshotsForCleanupRefSkipsSharedRevisionSnapshot(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(`CodeGraphRevision" sibling`).
		WithArgs(int32(42), int32(7), []string{"release-a", "refs/heads/release-a"}).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "repoId", "workspaceId", "commitHash", "schemaVersion", "builderVersion"}))

	s := NewStore(mock)
	got, err := s.ListGraphSnapshotsForCleanupRef(context.Background(), 7, 42, "release-a")
	if err != nil {
		t.Fatalf("ListGraphSnapshotsForCleanupRef: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("snapshots = %d, want 0 for sibling-protected commit", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestListSnapshotCommitsForCleanupRefSkipsCommitReferencedBySibling(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(`sibling_manifest`).
		WithArgs(int32(42), int32(7), []string{"release-a", "refs/heads/release-a"}).
		WillReturnRows(pgxmock.NewRows([]string{"commitHash"}))

	s := NewStore(mock)
	got, err := s.ListSnapshotCommitsForCleanupRef(context.Background(), 7, 42, "release-a")
	if err != nil {
		t.Fatalf("ListSnapshotCommitsForCleanupRef: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("commits = %d, want 0 for sibling-protected commit", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestInsertPending(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`INSERT INTO "RepoIndexingJob"`).
		WithArgs(jobID, int32(7), "INDEX").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	s := NewStore(mock)
	if err := s.InsertPending(context.Background(), jobID, 7, "INDEX"); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
}

func TestInsertPending_RequiresJobID(t *testing.T) {
	s := NewStore(nil)
	err := s.InsertPending(context.Background(), "", 1, "INDEX")
	if err == nil || !errors.Is(err, err) {
		t.Errorf("expected error for empty jobID")
	}
}

func TestListGraphSnapshotsForCleanup(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(`SELECT DISTINCT "orgId", "repoId", "workspaceId", "commitHash"`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "repoId", "workspaceId", "commitHash", "schemaVersion", "builderVersion"}).
			AddRow(int64(7), int64(42), "ws-1", "0123456789abcdef0123456789abcdef01234567", int64(1), "codeintel-v5"))

	s := NewStore(mock)
	got, err := s.ListGraphSnapshotsForCleanup(context.Background(), 7, 42)
	if err != nil {
		t.Fatalf("ListGraphSnapshotsForCleanup: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(got))
	}
	if got[0].OrgID != 7 || got[0].RepoID != 42 || got[0].WorkspaceID != "ws-1" || got[0].BuilderVersion != "codeintel-v5" {
		t.Fatalf("snapshot mismatch: %+v", got[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestDispatchCleanupStopsBeforeDBDeleteWhenGraphRetireFails(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(`SELECT "orgId", id, COALESCE\("cloneUrl", ''\), COALESCE\("external_codeHostType"::text, ''\)`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "id", "cloneUrl", "external_codeHostType"}).
			AddRow(int64(7), int32(42), "https://example.com/acme/orders.git", "github"))
	mock.ExpectQuery(`SELECT DISTINCT "orgId", "repoId", "workspaceId", "commitHash"`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "repoId", "workspaceId", "commitHash", "schemaVersion", "builderVersion"}).
			AddRow(int64(7), int64(42), "ws-1", "0123456789abcdef0123456789abcdef01234567", int64(1), "codeintel-v5"))

	retireErr := errors.New("nebula unavailable")
	retirer := &fakeGraphRetirer{err: retireErr}
	handler := NewHandlerWithGraphRetirer(NewStore(mock), repopaths.Config{DataCacheDir: t.TempDir()}, retirer, nil)
	err = handler.dispatchCleanup(context.Background(), 7, 42)
	if !errors.Is(err, retireErr) {
		t.Fatalf("dispatchCleanup error = %v, want graph retire error", err)
	}
	if len(retirer.calls) != 1 {
		t.Fatalf("graph retire calls = %d, want 1", len(retirer.calls))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestDispatchCleanupFailsClosedWhenGraphRetirerMissingAndSnapshotsExist(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(`SELECT "orgId", id, COALESCE\("cloneUrl", ''\), COALESCE\("external_codeHostType"::text, ''\)`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "id", "cloneUrl", "external_codeHostType"}).
			AddRow(int64(7), int32(42), "https://example.com/acme/orders.git", "github"))
	mock.ExpectQuery(`SELECT DISTINCT "orgId", "repoId", "workspaceId", "commitHash"`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "repoId", "workspaceId", "commitHash", "schemaVersion", "builderVersion"}).
			AddRow(int64(7), int64(42), "ws-1", "0123456789abcdef0123456789abcdef01234567", int64(1), "codeintel-v5"))

	handler := NewHandlerWithGraphRetirer(NewStore(mock), repopaths.Config{DataCacheDir: t.TempDir()}, nil, nil)
	err = handler.dispatchCleanup(context.Background(), 7, 42)
	if err == nil {
		t.Fatalf("expected missing graph retirer error, got %v", err)
	}
	if !strings.Contains(err.Error(), "graph snapshot retirer is not configured") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestCleanupRepoFilesystemUsesDelimiterSafeShardPrefix(t *testing.T) {
	root := t.TempDir()
	cfg := repopaths.Config{DataCacheDir: root}
	indexDir := filepath.Join(root, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("mkdir index: %v", err)
	}
	target := filepath.Join(indexDir, "7_2_main_0.zoekt")
	neighbor := filepath.Join(indexDir, "7_23_main_0.zoekt")
	prefixOnly := filepath.Join(indexDir, "7_2")
	for _, path := range []string{target, neighbor, prefixOnly} {
		if err := os.WriteFile(path, []byte("shard"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := CleanupRepoFilesystem(context.Background(), cfg, repopaths.Repo{
		OrgID:        7,
		RepoID:       2,
		CloneURL:     "https://example.com/o/r.git",
		CodeHostType: "github",
	}, logger)
	if err != nil {
		t.Fatalf("CleanupRepoFilesystem: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target shard still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(neighbor); err != nil {
		t.Fatalf("neighbor repo shard should remain: %v", err)
	}
	if _, err := os.Stat(prefixOnly); err != nil {
		t.Fatalf("prefix-only non-shard should remain: %v", err)
	}
}

func TestCleanupRepoFilesystemForRefKeepsSiblingBranchShard(t *testing.T) {
	root := t.TempDir()
	cfg := repopaths.Config{DataCacheDir: root}
	indexDir := filepath.Join(root, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("mkdir index: %v", err)
	}
	target := filepath.Join(indexDir, "7_2_release-a_0.zoekt")
	siblingBranch := filepath.Join(indexDir, "7_2_main_0.zoekt")
	siblingRepo := filepath.Join(indexDir, "7_23_release-a_0.zoekt")
	for _, path := range []string{target, siblingBranch, siblingRepo} {
		if err := os.WriteFile(path, []byte("shard"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := CleanupRepoFilesystemForRef(context.Background(), cfg, repopaths.Repo{
		OrgID:        7,
		RepoID:       2,
		CloneURL:     "https://example.com/o/r.git",
		CodeHostType: "github",
	}, "refs/heads/release-a", logger)
	if err != nil {
		t.Fatalf("CleanupRepoFilesystemForRef: %v", err)
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target shard still exists or stat failed unexpectedly: %v", err)
	}
	for _, path := range []string{siblingBranch, siblingRepo} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sibling shard should remain %s: %v", path, err)
		}
	}
}

func TestCleanupRepoSnapshotsRemovesRepoScopedSnapshotRoot(t *testing.T) {
	root := t.TempDir()
	cfg := repopaths.Config{DataCacheDir: root}
	target := cfg.RevisionSnapshotPath(7, 2, strings.Repeat("a", 40))
	neighbor := cfg.RevisionSnapshotPath(7, 23, strings.Repeat("b", 40))
	for _, dir := range []string{target, neighbor} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "source.go"), []byte("package source\n"), 0o644); err != nil {
			t.Fatalf("write source %s: %v", dir, err)
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := CleanupRepoSnapshots(context.Background(), cfg, repopaths.Repo{OrgID: 7, RepoID: 2}, logger); err != nil {
		t.Fatalf("CleanupRepoSnapshots: %v", err)
	}
	if _, err := os.Stat(cfg.RevisionSnapshotRepoRoot(7, 2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target snapshot root still exists or stat failed unexpectedly: %v", err)
	}
	if _, err := os.Stat(neighbor); err != nil {
		t.Fatalf("neighbor snapshot should remain: %v", err)
	}
}

func TestPlanSplitIndexPersistsDetectedSCIPSubjob(t *testing.T) {
	t.Setenv("CODEINTEL_INDEX_PLAN_SCIP_WORKER_CLASSES", "go")
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "go.mod"), []byte("module example.com/orders\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, "src"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "src", "orders.ts"), []byte("export function createOrder() {}\n"), 0o644); err != nil {
		t.Fatalf("write orders.ts: %v", err)
	}
	repo, err := git.PlainInit(worktree, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("git worktree: %v", err)
	}
	if _, err := tree.Add("go.mod"); err != nil {
		t.Fatalf("git add go.mod: %v", err)
	}
	if _, err := tree.Add("src/orders.ts"); err != nil {
		t.Fatalf("git add src/orders.ts: %v", err)
	}
	hash, err := tree.Commit("seed", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.local", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("git commit: %v", err)
	}

	commit := hash.String()
	const workspaceID = "atom-ws-scip"
	mock.ExpectQuery(`SELECT r\."orgId"`).
		WithArgs(int32(42)).
		WillReturnRows(pgxmock.NewRows([]string{"orgId", "workspaceId", "defaultBranch", "metadata"}).
			AddRow(int32(7), workspaceID, "main", "{}"))
	mock.ExpectQuery(`SELECT id\s+FROM "RepoIndexManifest"`).
		WithArgs(int32(7), int32(42), workspaceID, "refs/heads/main").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectExec(`INSERT INTO "RepoIndexManifest"`).
		WithArgs(anyArgs(17)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "RepoIndexManifestFile"`).
		WithArgs(anyArgs(20)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	for i := 0; i < 6; i++ {
		mock.ExpectExec(`INSERT INTO "CodeIntelIndexSubjob"`).
			WithArgs(anyArgs(23)...).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}

	handler := NewHandler(NewStore(mock), repopaths.Config{DataCacheDir: t.TempDir()}, nil)
	err = handler.planSplitIndex(context.Background(), repoindex.TaskPayload{
		JobID:  jobID,
		RepoID: 42,
		OrgID:  7,
		Type:   repoindex.JobTypeIndex,
	}, repopaths.Repo{OrgID: 7, RepoID: 42}, worktree, commit, "main")
	if !errors.Is(err, errIndexContinuesAsSubjobs) {
		t.Fatalf("planSplitIndex err = %v want errIndexContinuesAsSubjobs", err)
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

type fakeGraphRetirer struct {
	calls []graphschema.CodeGraphDeleteInput
	err   error
}

func (f *fakeGraphRetirer) MarkSnapshotForDeletion(_ context.Context, input graphschema.CodeGraphDeleteInput) error {
	f.calls = append(f.calls, input)
	return f.err
}
