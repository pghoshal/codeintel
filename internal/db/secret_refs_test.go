package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

const expectedListConnectionsForRefcheckQuery = `SELECT name, config FROM "Connection" WHERE "orgId" = \$1`
const expectedListLanguageModelsForRefcheckQuery = `SELECT name, config FROM "OrgLanguageModel" WHERE "orgId" = \$1`
const expectedDeleteOrgSecretQuery = `DELETE FROM "OrgSecret" WHERE "orgId" = \$1 AND key = \$2`

// TestListOrgConnectionsForRefcheck_ReturnsNameAndConfig locks the
// projection the DELETE handler needs: name (for the
// "connection:<name>" diagnostic) + config.
func TestListOrgConnectionsForRefcheck_ReturnsNameAndConfig(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedListConnectionsForRefcheckQuery).
		WithArgs(int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"name", "config"}).
			AddRow("gh-prod", []byte(`{"auth":{"token":{"secretRef":"GH_TOKEN"}}}`)).
			AddRow("gh-stage", []byte(`{"branches":["main"]}`)))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgConnectionsForRefcheck(ctx, 7)
	if err != nil {
		t.Fatalf("ListOrgConnectionsForRefcheck: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].Name != "gh-prod" {
		t.Errorf("Name: got %q", rows[0].Name)
	}
	if got, _ := rows[0].Config.(map[string]any); got == nil {
		t.Errorf("Config should decode to map[string]any, got %T (%v)", rows[0].Config, rows[0].Config)
	}
}

// TestListOrgConnectionsForRefcheck_EmptyOrg confirms a zero-row
// result returns an empty slice (never nil) so the handler doesn't
// have to nil-guard.
func TestListOrgConnectionsForRefcheck_EmptyOrg(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedListConnectionsForRefcheckQuery).
		WithArgs(int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"name", "config"}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.ListOrgConnectionsForRefcheck(ctx, 7)
	if err != nil {
		t.Fatalf("ListOrgConnectionsForRefcheck: %v", err)
	}
	if got == nil {
		t.Fatalf("expected empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(got))
	}
}

// TestListOrgLanguageModelsForRefcheck mirrors the connections test —
// same shape, different table.
func TestListOrgLanguageModelsForRefcheck(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedListLanguageModelsForRefcheckQuery).
		WithArgs(int32(3)).
		WillReturnRows(pgxmock.NewRows([]string{"name", "config"}).
			AddRow("z-ai:opus", []byte(`{"apiKey":{"secretRef":"LLM_SECRET"}}`)))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.ListOrgLanguageModelsForRefcheck(ctx, 3)
	if err != nil {
		t.Fatalf("ListOrgLanguageModelsForRefcheck: %v", err)
	}
	if len(got) != 1 || got[0].Name != "z-ai:opus" {
		t.Fatalf("got %+v", got)
	}
}

// TestListOrgConnectionsForRefcheck_InvalidConfigSurfacesError
// confirms malformed JSON in the config column surfaces an error
// rather than silently dropping the row (which could let a refcheck
// MISS a still-referenced secret and allow accidental deletion).
func TestListOrgConnectionsForRefcheck_InvalidConfigSurfacesError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedListConnectionsForRefcheckQuery).
		WithArgs(int32(5)).
		WillReturnRows(pgxmock.NewRows([]string{"name", "config"}).
			AddRow("bad", []byte(`{this is not json`)))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	if _, err := q.ListOrgConnectionsForRefcheck(ctx, 5); err == nil {
		t.Fatalf("expected an error on malformed config JSON, got nil")
	}
}

// TestDeleteOrgSecret_HappyPath locks the canonical DELETE shape.
func TestDeleteOrgSecret_HappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(expectedDeleteOrgSecretQuery).
		WithArgs(int32(7), "GH_TOKEN").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	if err := q.DeleteOrgSecret(ctx, 7, "GH_TOKEN"); err != nil {
		t.Fatalf("DeleteOrgSecret: %v", err)
	}
}

// TestDeleteOrgSecret_ZeroAffectedIsSuccess locks the idempotent-
// delete contract: zero rows affected is not an error.
func TestDeleteOrgSecret_ZeroAffectedIsSuccess(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(expectedDeleteOrgSecretQuery).
		WithArgs(int32(7), "MISSING").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	if err := q.DeleteOrgSecret(ctx, 7, "MISSING"); err != nil {
		t.Fatalf("idempotent delete must not error on zero rows, got %v", err)
	}
}

// TestDeleteOrgSecret_BoundaryGuards rejects empty key / non-positive
// orgID at the boundary.
func TestDeleteOrgSecret_BoundaryGuards(t *testing.T) {
	q := &Queries{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := q.DeleteOrgSecret(ctx, 0, "K"); !errors.Is(err, ErrInvalidOrgID) {
		t.Errorf("zero orgID: got %v, want ErrInvalidOrgID", err)
	}
	if err := q.DeleteOrgSecret(ctx, 7, ""); !errors.Is(err, ErrEmptySecretKey) {
		t.Errorf("empty key: got %v, want ErrEmptySecretKey", err)
	}
}
