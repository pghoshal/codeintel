-- ConnectionSyncJob captures the lifecycle of a sync attempt for a
-- given Connection. The /api/connections/{id}/status handler reads
-- the most recent 20 rows per connection to render a rollup. The
-- ConnectionSyncer extension point persists rows here when a sync
-- is scheduled and updates them as the work progresses.

CREATE TABLE IF NOT EXISTS "ConnectionSyncJob" (
    id              TEXT PRIMARY KEY,
    "connectionId"  INTEGER NOT NULL REFERENCES "Connection"(id) ON DELETE CASCADE,
    status          TEXT NOT NULL DEFAULT 'PENDING',
    "createdAt"     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "completedAt"   TIMESTAMPTZ,
    "errorMessage"  TEXT,
    "warningMessages" TEXT[] NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS "ConnectionSyncJob_connectionId_createdAt_idx"
    ON "ConnectionSyncJob" ("connectionId", "createdAt" DESC);
