-- Adds an index on (repoId, createdAt DESC) to keep the LATERAL
-- subquery on /api/repos (ORDER BY "createdAt" DESC LIMIT 1 per
-- repo) at O(log n) regardless of how many jobs a single repo has
-- accumulated. Without this index Postgres can pick the existing
-- (repoId, type, status) index, which forces an in-memory sort by
-- createdAt — fine at <=5 jobs per repo, but at thousands of
-- historical job rows per repo it blows the per-repo p99 budget
-- and amplifies the total page latency at perPage=100.
--
-- Partial index keeps the footprint small: only INDEX-type jobs
-- show up in the listing's latestJob projection today.
CREATE INDEX IF NOT EXISTS "RepoIndexingJob_repoId_createdAt_idx"
    ON "RepoIndexingJob" ("repoId", "createdAt" DESC);
