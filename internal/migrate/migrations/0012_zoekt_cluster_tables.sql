-- Slice S.2 of the codeintel ↔ legacy schema-parity recovery
-- (see docs/codeintel-schema-parity-audit.md). Creates the
-- three Zoekt-cluster tables the legacy schema defines:
--
--   ZoektShardGroup   — sharding partition the Repo column
--                       Repo.zoektShardGroupId points at.
--   ZoektOrgIndex     — one per org; tracks the org's Zoekt
--                       indexing state.
--   ZoektOrgReplica   — many per ZoektOrgIndex; tracks each
--                       Zoekt replica endpoint serving the org.
--
-- All columns / nullability / defaults / indexes / FK shapes
-- are byte-equal with the legacy reference postgres
-- (database legacy_schema, captured 2026-05-24).
--
-- Required as the FK target for the Repo.zoektShardGroupId
-- column landing in S.3 (existing-table parity).
--
-- Idempotent: every CREATE is guarded with IF NOT EXISTS so
-- re-running after a partial apply is a no-op.

-- 1. ZoektShardGroup ----------------------------------------

CREATE TABLE IF NOT EXISTS "ZoektShardGroup" (
    "id"           SERIAL                          PRIMARY KEY,
    "name"         TEXT                            NOT NULL,
    "endpointUrls" TEXT[]                          NULL,
    "description"  TEXT                            NULL,
    "createdAt"    TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"    TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS "ZoektShardGroup_name_key"
    ON "ZoektShardGroup" ("name");

-- 2. ZoektOrgIndex ------------------------------------------

CREATE TABLE IF NOT EXISTS "ZoektOrgIndex" (
    "id"                SERIAL                          PRIMARY KEY,
    "storageRoot"       TEXT                            NOT NULL,
    "indexPath"         TEXT                            NOT NULL,
    "repoCachePath"     TEXT                            NOT NULL,
    "status"            "ZoektOrgIndexStatus"           NOT NULL DEFAULT 'PENDING',
    "lastIndexedAt"     TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "lastHealthCheckAt" TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "errorMessage"      TEXT                            NULL,
    "createdAt"         TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"         TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"             INTEGER                         NOT NULL,
    CONSTRAINT "ZoektOrgIndex_orgId_fkey"
        FOREIGN KEY ("orgId") REFERENCES "Org"("id")
        ON UPDATE CASCADE ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS "ZoektOrgIndex_orgId_key"
    ON "ZoektOrgIndex" ("orgId");

CREATE INDEX IF NOT EXISTS "ZoektOrgIndex_status_idx"
    ON "ZoektOrgIndex" ("status");

-- 3. ZoektOrgReplica ----------------------------------------

CREATE TABLE IF NOT EXISTS "ZoektOrgReplica" (
    "id"                SERIAL                          PRIMARY KEY,
    "endpointUrl"       TEXT                            NOT NULL,
    "nodeName"          TEXT                            NULL,
    "isWriter"          BOOLEAN                         NOT NULL DEFAULT FALSE,
    "priority"          INTEGER                         NOT NULL DEFAULT 0,
    "status"            "ZoektOrgReplicaStatus"         NOT NULL DEFAULT 'UNKNOWN',
    "lastHealthCheckAt" TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "errorMessage"      TEXT                            NULL,
    "createdAt"         TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"         TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgIndexId"        INTEGER                         NOT NULL,
    CONSTRAINT "ZoektOrgReplica_orgIndexId_fkey"
        FOREIGN KEY ("orgIndexId") REFERENCES "ZoektOrgIndex"("id")
        ON UPDATE CASCADE ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS "ZoektOrgReplica_orgIndexId_endpointUrl_key"
    ON "ZoektOrgReplica" ("orgIndexId", "endpointUrl");

CREATE INDEX IF NOT EXISTS "ZoektOrgReplica_endpointUrl_status_idx"
    ON "ZoektOrgReplica" ("endpointUrl", "status");
