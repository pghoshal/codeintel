package scheduler

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

func TestSchedulerRefCandidatesNormalizesHeadsRef(t *testing.T) {
	got := schedulerRefCandidates("refs/heads/release-a")
	want := []string{"refs/heads/release-a", "release-a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
}

func TestSchedulerRefCandidatesAddsHeadsVariant(t *testing.T) {
	got := schedulerRefCandidates("main")
	want := []string{"main", "refs/heads/main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
}

func TestInsertPendingRepoIndexJobUsesBranchScopedActiveGuard(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(`metadata->>'ref' = ANY\(\$5::text\[\]\)`).
		WithArgs(int32(42), "job-1", "INDEX", "refs/heads/release-a", []string{"refs/heads/release-a", "release-a"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "type", "status", "inserted"}).
			AddRow("job-1", "INDEX", "PENDING", true))

	s := NewService(mock, nil)
	inserted, active, err := s.insertPendingRepoIndexJob(context.Background(), 42, "job-1", "INDEX", "refs/heads/release-a")
	if err != nil {
		t.Fatalf("insertPendingRepoIndexJob: %v", err)
	}
	if !inserted || active.JobID != "job-1" {
		t.Fatalf("inserted=%v active=%+v", inserted, active)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestMarkConnectionSyncJobFailedAfterEnqueueError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`UPDATE "ConnectionSyncJob"`).
		WithArgs("sync-job-1", int32(42), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	s := NewService(mock, nil)
	err = s.markConnectionSyncJobFailedAfterEnqueueError(context.Background(), "sync-job-1", 42, errors.New("redis down"))
	if err != nil {
		t.Fatalf("markConnectionSyncJobFailedAfterEnqueueError: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}
