package db

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

func TestGetOrgRepoForReadScopesByOrgAndActiveConnection(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	indexedAt := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	defaultBranch := "main"
	jobType := "REMOVE_INDEX"
	jobStatus := "IN_PROGRESS"
	mock.ExpectQuery(regexp.QuoteMeta(getOrgRepoForReadQuery)).
		WithArgs(int32(7), "github.com/acme/api").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "orgId", "name", "displayName", "cloneUrl", "external_codeHostType",
			"webUrl", "defaultBranch", "indexedAt", "metadata", "type", "status",
		}).AddRow(
			int32(42), int32(7), "github.com/acme/api", ptrString("API"),
			"https://github.com/acme/api.git", "github", ptrString("https://github.com/acme/api"),
			&defaultBranch, &indexedAt, []byte(`{"indexedRevisions":["refs/heads/main"]}`), &jobType, &jobStatus,
		))

	q := &Queries{db: mock}
	row, err := q.GetOrgRepoForRead(context.Background(), 7, "github.com/acme/api")
	if err != nil {
		t.Fatalf("GetOrgRepoForRead: %v", err)
	}
	if row.OrgID != 7 || row.ID != 42 || row.Name != "github.com/acme/api" {
		t.Fatalf("wrong row: %+v", row)
	}
	if row.IndexedAt == nil || !row.IndexedAt.Equal(indexedAt) {
		t.Fatalf("indexedAt wrong: %v", row.IndexedAt)
	}
	if string(row.Metadata) != `{"indexedRevisions":["refs/heads/main"]}` {
		t.Fatalf("metadata wrong: %s", row.Metadata)
	}
	if row.LatestJobType == nil || *row.LatestJobType != "REMOVE_INDEX" || row.LatestJobStatus == nil || *row.LatestJobStatus != "IN_PROGRESS" {
		t.Fatalf("latest job fields wrong: type=%v status=%v", row.LatestJobType, row.LatestJobStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestGetOrgRepoForReadOtherOrgSameNameReturnsNotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(regexp.QuoteMeta(getOrgRepoForReadQuery)).
		WithArgs(int32(7), "github.com/acme/api").
		WillReturnRows(pgxmock.NewRows([]string{
			"id", "orgId", "name", "displayName", "cloneUrl", "external_codeHostType",
			"webUrl", "defaultBranch", "indexedAt", "metadata", "type", "status",
		}))

	q := &Queries{db: mock}
	_, err = q.GetOrgRepoForRead(context.Background(), 7, "github.com/acme/api")
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("got %v, want ErrRepoNotFound", err)
	}
}

func TestListOrgSearchReposRequiresActiveConnection(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	indexedAt := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	defaultBranch := "main"
	jobType := "REMOVE_INDEX"
	jobStatus := "PENDING"
	mock.ExpectQuery(regexp.QuoteMeta(listOrgSearchReposQuery)).
		WithArgs(int32(7), []int32{42}, []string{"github.com/acme/api"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "displayName", "external_codeHostType", "webUrl", "defaultBranch", "indexedAt", "metadata", "type", "status"}).
			AddRow(int32(42), "github.com/acme/api", ptrString("API"), "github", ptrString("https://github.com/acme/api"), &defaultBranch, &indexedAt, []byte(`{"indexedRevisions":["refs/heads/main"]}`), &jobType, &jobStatus))

	q := &Queries{db: mock}
	rows, err := q.ListOrgSearchRepos(context.Background(), 7, []int32{42}, []string{"github.com/acme/api"})
	if err != nil {
		t.Fatalf("ListOrgSearchRepos: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 42 || rows[0].Name != "github.com/acme/api" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	if rows[0].DefaultBranch == nil || *rows[0].DefaultBranch != "main" || rows[0].IndexedAt == nil {
		t.Fatalf("expected search repo policy fields, got %+v", rows[0])
	}
	if string(rows[0].Metadata) != `{"indexedRevisions":["refs/heads/main"]}` {
		t.Fatalf("metadata: got %s", rows[0].Metadata)
	}
	if rows[0].LatestJobType == nil || *rows[0].LatestJobType != "REMOVE_INDEX" || rows[0].LatestJobStatus == nil || *rows[0].LatestJobStatus != "PENDING" {
		t.Fatalf("latest job fields: got type=%v status=%v", rows[0].LatestJobType, rows[0].LatestJobStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestListOrgSearchPolicyReposCanReturnAllActiveRepos(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	indexedAt := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	defaultBranch := "release-b"
	jobType := "REMOVE_INDEX"
	jobStatus := "IN_PROGRESS"
	mock.ExpectQuery(regexp.QuoteMeta(listOrgSearchPolicyReposQuery)).
		WithArgs(int32(7), []string{}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "displayName", "external_codeHostType", "webUrl", "defaultBranch", "indexedAt", "metadata", "type", "status"}).
			AddRow(int32(42), "github.com/acme/api", ptrString("API"), "github", ptrString("https://github.com/acme/api"), &defaultBranch, &indexedAt, []byte(`{"branches":["release-b"],"indexedRevisions":["refs/heads/release-b"]}`), &jobType, &jobStatus))

	q := &Queries{db: mock}
	rows, err := q.ListOrgSearchPolicyRepos(context.Background(), 7, []string{})
	if err != nil {
		t.Fatalf("ListOrgSearchPolicyRepos: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 42 || rows[0].DefaultBranch == nil || *rows[0].DefaultBranch != "release-b" {
		t.Fatalf("unexpected policy rows: %+v", rows)
	}
	if rows[0].LatestJobType == nil || *rows[0].LatestJobType != "REMOVE_INDEX" || rows[0].LatestJobStatus == nil || *rows[0].LatestJobStatus != "IN_PROGRESS" {
		t.Fatalf("latest policy job fields: type=%v status=%v", rows[0].LatestJobType, rows[0].LatestJobStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestListOrgSearchPolicyReposNormalizesNilRepoNamesToAllRepos(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	indexedAt := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	defaultBranch := "release-a"
	mock.ExpectQuery(regexp.QuoteMeta(listOrgSearchPolicyReposQuery)).
		WithArgs(int32(7), []string{}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "displayName", "external_codeHostType", "webUrl", "defaultBranch", "indexedAt", "metadata", "type", "status"}).
			AddRow(int32(43), "github.com/acme/release", ptrString("Release"), "github", ptrString("https://github.com/acme/release"), &defaultBranch, &indexedAt, []byte(`{"branches":["release-a"],"indexedRevisions":["refs/heads/release-a"]}`), nil, nil))

	q := &Queries{db: mock}
	rows, err := q.ListOrgSearchPolicyRepos(context.Background(), 7, nil)
	if err != nil {
		t.Fatalf("ListOrgSearchPolicyRepos: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != 43 {
		t.Fatalf("unexpected policy rows: %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
