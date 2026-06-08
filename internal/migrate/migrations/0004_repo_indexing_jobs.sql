-- RepoIndexingJob captures one execution of an index/cleanup/
-- remove-index pipeline for a Repo. The denormalised
-- "latestIndexingJobStatus" column on Repo mirrors the most-
-- recent job's status so dashboards can rollup without a JOIN
-- per repo.
ALTER TABLE "Repo"
    ADD COLUMN IF NOT EXISTS "latestIndexingJobStatus" TEXT;

CREATE TABLE IF NOT EXISTS "RepoIndexingJob" (
    id              TEXT PRIMARY KEY,
    type            TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'PENDING',
    "createdAt"     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "completedAt"   TIMESTAMPTZ,
    metadata        JSONB,
    "errorMessage"  TEXT,
    "repoId"        INTEGER NOT NULL REFERENCES "Repo"(id) ON DELETE CASCADE
);

-- Per-repo lookups by type + status, e.g. "find the in-flight
-- INDEX job for repo X".
CREATE INDEX IF NOT EXISTS "RepoIndexingJob_repoId_type_status_idx"
    ON "RepoIndexingJob" ("repoId", type, status);

-- Recent-failures listing: ORDER BY updatedAt DESC under
-- status='FAILED'. Partial index keeps the footprint small since
-- failures are a fraction of total jobs.
CREATE INDEX IF NOT EXISTS "RepoIndexingJob_failed_updatedAt_idx"
    ON "RepoIndexingJob" ("updatedAt" DESC)
    WHERE status = 'FAILED';
