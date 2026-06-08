//go:build integration

// Phase C.1 live integration test for the RepoIndexingJob
// lifecycle helpers. Exercises Insert -> MarkInProgress ->
// MarkCompleted + the latest-status denormalisation against
// the real codeintel-postgres + the RepoIndexingJobType /
// RepoIndexingJobStatus enums landed in S.1 + S.3b.
package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"codeintel/internal/backend/repoindexmanager"
	"codeintel/internal/db"
	"codeintel/internal/migrate"

	"github.com/google/uuid"
)

func TestRepoIndexLifecycle_LiveLifecycleHappyPath(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	// Insert minimal Org + Repo fixtures (Repo has many NOT NULL
	// columns after the S.3c tightening, so we have to supply
	// every required value).
	orgName := "c1-lifecycle-" + uuid.NewString()[:8]
	var orgID int32
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID); err != nil {
		t.Fatalf("insert Org: %v", err)
	}
	var repoID int32
	emptyMD, _ := json.Marshal(map[string]any{})
	if err := pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`,
		"repo-"+uuid.NewString()[:6],
		"https://example/test.git",
		"ext-"+uuid.NewString()[:6],
		"github",
		"https://github.com",
		string(emptyMD),
		orgID,
	).Scan(&repoID); err != nil {
		t.Fatalf("insert Repo: %v", err)
	}

	store := repoindexmanager.NewStore(pool)
	jobID := uuid.NewString()

	t.Run("InsertPending lands a PENDING row", func(t *testing.T) {
		if err := store.InsertPending(ctx, jobID, repoID, "INDEX"); err != nil {
			t.Fatalf("InsertPending: %v", err)
		}
		var status, jobType string
		if err := pool.QueryRow(ctx, `
			SELECT status::text, type::text FROM "RepoIndexingJob" WHERE id = $1
		`, jobID).Scan(&status, &jobType); err != nil {
			t.Fatalf("query row: %v", err)
		}
		if status != "PENDING" || jobType != "INDEX" {
			t.Errorf("got status=%q type=%q want PENDING INDEX", status, jobType)
		}
	})

	t.Run("MarkInProgress flips PENDING -> IN_PROGRESS", func(t *testing.T) {
		if err := store.MarkInProgress(ctx, jobID); err != nil {
			t.Fatalf("MarkInProgress: %v", err)
		}
		var status string
		_ = pool.QueryRow(ctx, `SELECT status::text FROM "RepoIndexingJob" WHERE id = $1`, jobID).Scan(&status)
		if status != "IN_PROGRESS" {
			t.Errorf("status: got %q want IN_PROGRESS", status)
		}
	})

	t.Run("MarkInProgress is idempotent on IN_PROGRESS row", func(t *testing.T) {
		// Re-running on an IN_PROGRESS row updates updatedAt but
		// doesn't fail (legacy guard accepts both PENDING and
		// IN_PROGRESS).
		if err := store.MarkInProgress(ctx, jobID); err != nil {
			t.Errorf("re-MarkInProgress: %v", err)
		}
	})

	t.Run("MarkCompleted finalises the row + stamps completedAt", func(t *testing.T) {
		if err := store.MarkCompleted(ctx, jobID); err != nil {
			t.Fatalf("MarkCompleted: %v", err)
		}
		var status string
		var completedAt *time.Time
		_ = pool.QueryRow(ctx, `
			SELECT status::text, "completedAt" FROM "RepoIndexingJob" WHERE id = $1
		`, jobID).Scan(&status, &completedAt)
		if status != "COMPLETED" {
			t.Errorf("status: got %q want COMPLETED", status)
		}
		if completedAt == nil {
			t.Errorf("completedAt nil after MarkCompleted")
		}
	})

	t.Run("MarkInProgress on COMPLETED returns ErrJobInTerminalState", func(t *testing.T) {
		err := store.MarkInProgress(ctx, jobID)
		if err == nil {
			t.Errorf("expected ErrJobInTerminalState on COMPLETED row")
		}
	})

	t.Run("RefreshRepoLatestIndexingJobStatus denormalises COMPLETED", func(t *testing.T) {
		if err := store.RefreshRepoLatestIndexingJobStatus(ctx, repoID); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		var status *string
		_ = pool.QueryRow(ctx, `SELECT "latestIndexingJobStatus"::text FROM "Repo" WHERE id = $1`, repoID).Scan(&status)
		if status == nil || *status != "COMPLETED" {
			t.Errorf("Repo.latestIndexingJobStatus: %v want COMPLETED", status)
		}
	})
}

func TestRepoIndexLifecycle_MarkFailedRecordsErrorMessage(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{DSN: dsn, AllowInsecureRemoteDSN: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()
	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	orgName := "c1-failed-" + uuid.NewString()[:8]
	var orgID int32
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Org" (name, domain, "updatedAt") VALUES ($1, $2, NOW()) RETURNING id
	`, orgName, orgName+".test").Scan(&orgID)
	var repoID int32
	emptyMD, _ := json.Marshal(map[string]any{})
	_ = pool.QueryRow(ctx, `
		INSERT INTO "Repo" (
		    name, "isFork", "isArchived",
		    "cloneUrl", "external_id", "external_codeHostType", "external_codeHostUrl",
		    metadata, "orgId", "updatedAt"
		) VALUES ($1, FALSE, FALSE, $2, $3, $4::"CodeHostType", $5, $6::jsonb, $7, NOW())
		RETURNING id
	`,
		"failrepo-"+uuid.NewString()[:6],
		"https://example/fail.git",
		"ext-"+uuid.NewString()[:6],
		"github",
		"https://github.com",
		string(emptyMD),
		orgID,
	).Scan(&repoID)

	store := repoindexmanager.NewStore(pool)
	jobID := uuid.NewString()
	if err := store.InsertPending(ctx, jobID, repoID, "INDEX"); err != nil {
		t.Fatalf("InsertPending: %v", err)
	}
	if err := store.MarkFailed(ctx, jobID, "clone failed: connection refused"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	var status, errMsg string
	_ = pool.QueryRow(ctx, `
		SELECT status::text, COALESCE("errorMessage", '') FROM "RepoIndexingJob" WHERE id = $1
	`, jobID).Scan(&status, &errMsg)
	if status != "FAILED" {
		t.Errorf("status: got %q want FAILED", status)
	}
	if errMsg != "clone failed: connection refused" {
		t.Errorf("errorMessage: got %q", errMsg)
	}
}
