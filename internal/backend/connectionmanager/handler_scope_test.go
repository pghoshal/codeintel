package connectionmanager

import (
	"context"
	"strings"
	"testing"

	"github.com/pashagolub/pgxmock/v4"
)

const connectionSyncJobID = "22223333-4444-5555-6666-777788889999"

func TestHandlerMarkInProgressScopesJobConnectionAndOrg(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "ConnectionSyncJob" j`).
		WithArgs(connectionSyncJobID, int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	h := NewHandler(mock, nil)
	if err := h.markInProgress(context.Background(), connectionSyncJobID, 42, 7); err != nil {
		t.Fatalf("markInProgress: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerMarkInProgressRejectsMismatchedScope(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "ConnectionSyncJob" j`).
		WithArgs(connectionSyncJobID, int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	h := NewHandler(mock, nil)
	err = h.markInProgress(context.Background(), connectionSyncJobID, 42, 7)
	if err == nil || !strings.Contains(err.Error(), "terminal state or missing") {
		t.Fatalf("markInProgress error: got %v, want terminal/missing scope error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerLoadConnectionScopesByOrg(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, "orgId", name, "connectionType"::text, config::text`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "orgId", "name", "connectionType", "config"}).
			AddRow(int32(42), int32(7), "gh-prod", "github", `{"type":"github"}`))

	h := NewHandler(mock, nil)
	got, err := h.loadConnection(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("loadConnection: %v", err)
	}
	if got.ID != 42 || got.OrgID != 7 || got.Name != "gh-prod" || got.ConnectionType != "github" || string(got.Config) != `{"type":"github"}` {
		t.Fatalf("loadConnection got unexpected row: %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerMarkFailedScopesJobConnectionAndOrg(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "ConnectionSyncJob" j`).
		WithArgs(connectionSyncJobID, "boom", int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	h := NewHandler(mock, nil)
	if err := h.markFailed(context.Background(), connectionSyncJobID, 42, 7, "boom"); err != nil {
		t.Fatalf("markFailed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerMarkCompletedScopesJobConnectionAndOrg(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "ConnectionSyncJob"`).
		WithArgs(connectionSyncJobID, []string{"rate limited once"}, int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "Connection" SET`).
		WithArgs(int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	h := NewHandler(mock, nil)
	if err := h.markCompleted(context.Background(), connectionSyncJobID, 42, 7, []string{"rate limited once"}); err != nil {
		t.Fatalf("markCompleted: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerMarkCompletedRejectsMismatchedJobConnection(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "ConnectionSyncJob"`).
		WithArgs(connectionSyncJobID, []string{}, int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	h := NewHandler(mock, nil)
	err = h.markCompleted(context.Background(), connectionSyncJobID, 42, 7, nil)
	if err == nil || !strings.Contains(err.Error(), "mismatched connection") {
		t.Fatalf("markCompleted error: got %v, want mismatched connection error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerMarkCompletedRejectsMismatchedOrgConnection(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`UPDATE "ConnectionSyncJob"`).
		WithArgs(connectionSyncJobID, []string{}, int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "Connection" SET`).
		WithArgs(int32(42), int32(7)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	h := NewHandler(mock, nil)
	err = h.markCompleted(context.Background(), connectionSyncJobID, 42, 7, nil)
	if err == nil || !strings.Contains(err.Error(), "missing in org") {
		t.Fatalf("markCompleted error: got %v, want missing org connection error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}
