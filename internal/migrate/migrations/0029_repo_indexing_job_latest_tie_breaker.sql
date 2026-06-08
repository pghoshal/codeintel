CREATE INDEX IF NOT EXISTS "RepoIndexingJob_repoId_createdAt_id_latest_idx"
ON "RepoIndexingJob" ("repoId", "createdAt" DESC, id DESC);

CREATE INDEX IF NOT EXISTS "RepoIndexingJob_repoId_active_latest_idx"
ON "RepoIndexingJob" ("repoId", "createdAt" DESC, id DESC)
WHERE status IN ('PENDING'::"RepoIndexingJobStatus", 'IN_PROGRESS'::"RepoIndexingJobStatus");
