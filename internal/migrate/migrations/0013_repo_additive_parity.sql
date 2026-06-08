-- Slice S.3a of the codeintel ↔ legacy schema-parity recovery
-- (see docs/codeintel-schema-parity-audit.md). Additive-only
-- changes to the Repo table to close the column / index / FK
-- drift vs the legacy reference schema. Destructive changes
-- (text → enum type conversion, timestamptz → timestamp(3)
-- drop, NOT NULL tightening, the AuditEvent → Audit rename
-- with producer-API change) defer to S.3b / S.3c.
--
-- What this migration adds (all idempotent):
--
--   1. Three missing columns: zoektShardGroupId, indexedCommitHash,
--      permissionSyncedAt. All nullable for now; tightened when
--      the worker/permission-sync slices fill them.
--   2. Four missing indexes: id+orgId UNIQUE, name+orgId UNIQUE,
--      indexedAt, orgId+zoektShardGroupId.
--   3. FK Repo.zoektShardGroupId → ZoektShardGroup.id with
--      ON UPDATE CASCADE ON DELETE SET NULL (matches legacy
--      Repo_zoektShardGroupId_fkey).
--   4. Index rename: 0010's
--      "Repo_external_id_codeHostUrl_orgId_key" → legacy's
--      "Repo_external_id_external_codeHostUrl_orgId_key". The
--      0010 name was a shortened form; legacy uses the full
--      `<col1>_<col2>_<col3>_key` Prisma convention with the
--      full column name preserved.

-- 1. Missing columns ---------------------------------------

ALTER TABLE "Repo"
    ADD COLUMN IF NOT EXISTS "zoektShardGroupId" INTEGER NULL,
    ADD COLUMN IF NOT EXISTS "indexedCommitHash" TEXT    NULL,
    ADD COLUMN IF NOT EXISTS "permissionSyncedAt" TIMESTAMP(3) WITHOUT TIME ZONE NULL;

-- 2. Missing indexes ---------------------------------------

CREATE UNIQUE INDEX IF NOT EXISTS "Repo_id_orgId_key"
    ON "Repo" ("id", "orgId");

CREATE UNIQUE INDEX IF NOT EXISTS "Repo_name_orgId_key"
    ON "Repo" ("name", "orgId");

CREATE INDEX IF NOT EXISTS "Repo_indexedAt_idx"
    ON "Repo" ("indexedAt");

CREATE INDEX IF NOT EXISTS "Repo_orgId_zoektShardGroupId_idx"
    ON "Repo" ("orgId", "zoektShardGroupId");

-- 3. FK to ZoektShardGroup ---------------------------------

-- pg's ALTER TABLE ... ADD CONSTRAINT IF NOT EXISTS does NOT
-- exist; emulate via pg_constraint lookup.
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'Repo_zoektShardGroupId_fkey'
  ) THEN
    ALTER TABLE "Repo"
      ADD CONSTRAINT "Repo_zoektShardGroupId_fkey"
      FOREIGN KEY ("zoektShardGroupId")
      REFERENCES "ZoektShardGroup"("id")
      ON UPDATE CASCADE ON DELETE SET NULL;
  END IF;
END $$;

-- 4. Index rename ------------------------------------------
-- DROP the partial-name index 0010 created, then CREATE the
-- legacy-shaped name. Skip if the index is already correctly
-- named (re-run safety).

DO $$ BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_class WHERE relname = 'Repo_external_id_codeHostUrl_orgId_key'
  ) AND NOT EXISTS (
    SELECT 1 FROM pg_class WHERE relname = 'Repo_external_id_external_codeHostUrl_orgId_key'
  ) THEN
    ALTER INDEX "Repo_external_id_codeHostUrl_orgId_key"
      RENAME TO "Repo_external_id_external_codeHostUrl_orgId_key";
  END IF;
END $$;

-- Make sure the index exists under the canonical name even on
-- fresh DBs (where 0010 created it with the legacy-shaped name
-- after a re-run that includes both migrations). The
-- CREATE UNIQUE INDEX IF NOT EXISTS below is the safety net:
-- it produces the index iff neither the old nor new name exists
-- (because the unique-on-cols constraint would already block a
-- second index covering the same tuple).
CREATE UNIQUE INDEX IF NOT EXISTS "Repo_external_id_external_codeHostUrl_orgId_key"
    ON "Repo" ("external_id", "external_codeHostUrl", "orgId");
