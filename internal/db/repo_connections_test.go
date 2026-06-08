package db

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v4"
)

func TestUpdateOrgRepoBranchPolicyMetadataScopesRepoAndWritesBranches(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(regexp.QuoteMeta(updateOrgRepoBranchPolicyMetadataQuery)).
		WithArgs(int32(42), int32(7), []byte(`["release-a"]`)).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 1"))

	q := &Queries{db: mock}
	if err := q.UpdateOrgRepoBranchPolicyMetadata(context.Background(), 7, 42, []string{"release-a"}); err != nil {
		t.Fatalf("UpdateOrgRepoBranchPolicyMetadata: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUpdateOrgRepoBranchPolicyMetadataMissingRepo(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(regexp.QuoteMeta(updateOrgRepoBranchPolicyMetadataQuery)).
		WithArgs(int32(42), int32(7), []byte(`[]`)).
		WillReturnResult(pgconn.NewCommandTag("UPDATE 0"))

	q := &Queries{db: mock}
	err = q.UpdateOrgRepoBranchPolicyMetadata(context.Background(), 7, 42, nil)
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("got %v, want ErrRepoNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
