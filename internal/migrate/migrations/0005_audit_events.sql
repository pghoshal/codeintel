-- AuditEvent persists every event codeintel-app forwards through
-- AuditService.Emit. Backend's audit-service handler writes one
-- row per RPC. The "metadata" JSONB column captures event-specific
-- context as a key→value map; the typed columns above it are
-- the indexed projection callers query by.
CREATE TABLE IF NOT EXISTS "AuditEvent" (
    id              BIGSERIAL PRIMARY KEY,
    action          TEXT NOT NULL,
    "actorId"       TEXT NOT NULL,
    "actorType"     TEXT NOT NULL,
    "targetId"      TEXT NOT NULL,
    "targetType"    TEXT NOT NULL,
    "orgId"         INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    "requestId"     TEXT NOT NULL DEFAULT '',
    "eventTime"     TIMESTAMPTZ NOT NULL,
    "insertedAt"    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metadata        JSONB
);

-- Tenant-scoped queries (the dashboard pulls "recent audit events
-- for org X") hit this index first.
CREATE INDEX IF NOT EXISTS "AuditEvent_orgId_eventTime_idx"
    ON "AuditEvent" ("orgId", "eventTime" DESC);

-- Action-scoped queries (compliance reports filter by action) hit
-- this partial / composite index. Keep total index footprint
-- small by leading with the high-selectivity orgId column.
CREATE INDEX IF NOT EXISTS "AuditEvent_orgId_action_eventTime_idx"
    ON "AuditEvent" ("orgId", action, "eventTime" DESC);
