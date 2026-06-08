package indexplanwriter

import (
	"context"
	"errors"
	"testing"

	"codeintel/internal/backend/indexsubjobs"
	codeintelv1 "codeintel/proto/codeintel/v1"

	"github.com/pashagolub/pgxmock/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestValidateRequestAndPlannerConversion(t *testing.T) {
	req := validRequest()
	if err := validateRequest(req); err != nil {
		t.Fatalf("validateRequest: %v", err)
	}
	in := toPlannerInput(req)
	if in.MaxAttempts != 3 || len(in.Revisions) != 1 || len(in.Revisions[0].SCIPProjects) != 1 {
		t.Fatalf("unexpected planner input: %#v", in)
	}
	if got := in.Revisions[0].SCIPProjects[0].SCIPWorkerClass; got != "go" {
		t.Fatalf("SCIPWorkerClass = %q want go", got)
	}
}

func TestValidateRejectsUnsafeProjectRoot(t *testing.T) {
	for _, root := range []string{"../outside", "services/../../outside", "/abs", `..\outside`} {
		req := validRequest()
		req.Revisions[0].ScipProjects[0].ProjectRoot = root
		if err := validateRequest(req); err == nil {
			t.Fatalf("expected %q to be rejected", root)
		}
	}
}

func TestValidateRejectsMismatchedRevisionAndActivateOnly(t *testing.T) {
	req := validRequest()
	req.Revisions[0].Revision = "refs/heads/other"
	if err := validateRequest(req); err == nil {
		t.Fatal("expected mismatched revision to be rejected")
	}

	req = validRequest()
	req.Revisions[0].RunAstTreeSitter = false
	req.Revisions[0].RunGraphMerge = false
	req.Revisions[0].ScipProjects = nil
	if err := validateRequest(req); err == nil {
		t.Fatal("expected activate-only revision to be rejected")
	}
}

func TestWritePlanRejectsWorkspaceMismatch(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	req := validRequest()
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs("job-1", int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("org-workspace"))

	store := &recordingStore{}
	_, err = NewServer(mock, store, nil).WritePlan(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v want InvalidArgument", err)
	}
	if len(store.rows) != 0 {
		t.Fatalf("store writes: got %d want 0", len(store.rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestWritePlanRequiresActiveIndexJob(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	req := validRequest()
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs("job-1", int32(42), int32(7)).
		WillReturnError(errors.New("scope lookup failed"))

	_, err = NewServer(mock, &recordingStore{}, nil).WritePlan(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got %v want Internal", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestWritePlanRequiresPreparedManifestScope(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	req := validRequest()
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs("job-1", int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("ws-1"))
	mock.ExpectQuery(`SELECT 1`).
		WithArgs("job-1", int32(7), int32(42), "ws-1", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").
		WillReturnError(errors.New("manifest lookup failed"))

	_, err = NewServer(mock, &recordingStore{}, nil).WritePlan(context.Background(), req)
	if status.Code(err) != codes.Internal {
		t.Fatalf("got %v want Internal", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestWritePlanPersistsSubjobs(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	req := validRequest()
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs("job-1", int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("ws-1"))
	mock.ExpectQuery(`SELECT 1`).
		WithArgs("job-1", int32(7), int32(42), "ws-1", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(1))

	store := &recordingStore{}
	resp, err := NewServer(mock, store, nil).WritePlan(context.Background(), req)
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	if resp.GetRevisionCount() != 1 || resp.GetSubjobCount() != 4 {
		t.Fatalf("response = %#v want 1 revision / 4 subjobs", resp)
	}
	if len(store.rows) != 4 {
		t.Fatalf("store rows = %d want 4: %#v", len(store.rows), store.rows)
	}
	if store.rows[0].Layer != indexsubjobs.LayerASTTreeSitter {
		t.Fatalf("first layer = %s want AST_TREE_SITTER", store.rows[0].Layer)
	}
	var sawSCIP bool
	for _, row := range store.rows {
		if row.Layer == indexsubjobs.LayerSCIP {
			sawSCIP = true
			if row.WorkerClass != "scip-go" || row.QueueName != "codeintel-index-scip-go" {
				t.Fatalf("SCIP row class/queue mismatch: %#v", row)
			}
		}
		if row.Layer == indexsubjobs.LayerClone || row.Layer == indexsubjobs.LayerZoekt {
			t.Fatalf("repo-wide layer should not be revision-planned yet: %#v", row)
		}
	}
	if !sawSCIP {
		t.Fatalf("missing SCIP row")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

type recordingStore struct {
	rows []indexsubjobs.CreateInput
	err  error
}

func (s *recordingStore) UpsertQueued(_ context.Context, in indexsubjobs.CreateInput) error {
	if s.err != nil {
		return s.err
	}
	s.rows = append(s.rows, in)
	return nil
}

func validRequest() *codeintelv1.WriteIndexPlanRequest {
	return &codeintelv1.WriteIndexPlanRequest{
		IndexJobId:  "job-1",
		OrgId:       7,
		RepoId:      42,
		MaxAttempts: 3,
		Revisions: []*codeintelv1.IndexPlanRevision{{
			WorkspaceId:      "ws-1",
			Branch:           "refs/heads/main",
			Revision:         "refs/heads/main",
			CommitHash:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RunAstTreeSitter: true,
			RunGraphMerge:    true,
			RunActivate:      true,
			ScipProjects: []*codeintelv1.SCIPProjectPlan{{
				Language:        "go",
				ProjectRoot:     "",
				Indexer:         "scip-go",
				ScipWorkerClass: "go",
			}},
		}},
	}
}

func TestWritePlanMapsInvalidPlanToInvalidArgument(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	req := validRequest()
	req.Revisions[0].ScipProjects[0].ScipWorkerClass = "missing"
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs("job-1", int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("ws-1"))
	mock.ExpectQuery(`SELECT 1`).
		WithArgs("job-1", int32(7), int32(42), "ws-1", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa").
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(1))

	_, err = NewServer(mock, &recordingStore{}, nil).WritePlan(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("got %v want InvalidArgument", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}
