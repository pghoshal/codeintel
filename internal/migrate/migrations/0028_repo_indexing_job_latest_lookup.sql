CREATE INDEX IF NOT EXISTS "RepoIndexingJob_repoId_createdAt_latest_idx"
ON "RepoIndexingJob" ("repoId", "createdAt" DESC);
