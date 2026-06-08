-- Adds the org-scoped lookup index on the Connection table.
-- Repo already has Repo_orgId_idx (0001:89); Connection was missing
-- the equivalent, forcing a seq scan for every org-scoped count or
-- list query (status rollup, /api/connections, /api/secrets ref
-- checks). The new index closes that gap.
CREATE INDEX IF NOT EXISTS "Connection_orgId_idx" ON "Connection" ("orgId");
