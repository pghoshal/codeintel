-- Extended Repo columns the connection-sync worker writes. The
-- legacy connectionManager.ts upserts each repo by a composite
-- (external_id, external_codeHostUrl, orgId) key after a code-host
-- adapter (github/gitlab/etc.) emits a normalized RepoData record,
-- then attaches metadata + cloneUrl + isPublic + isAutoCleanupDisabled
-- on the Repo row itself.
--
-- This migration adds those columns + the matching unique
-- constraint. Phase B.1d (the worker port) fills them on every
-- write; the connection-sync wire output (the `Repo` row written
-- on a successful sync) is byte-identical with the legacy
-- packages/db/prisma/schema.prisma `model Repo` for these fields:
--
--   isPublic              Boolean  @default(false)
--   isAutoCleanupDisabled Boolean  @default(false)
--   metadata              Json
--   cloneUrl              String
--   external_id           String
--   external_codeHostUrl  String
--   @@unique([external_id, external_codeHostUrl, orgId])
--
-- Nullability divergence — DELIBERATE, FOLLOW-UP TIGHTEN PLANNED:
--   cloneUrl / external_id / external_codeHostUrl / metadata are
--   added as NULL-able here so the migration is safe on a DB that
--   already has Repo rows from migrations 0001 / 0006. Once the
--   Phase B.1d worker has been observed setting these on every
--   new write (and a backfill pass has filled legacy rows), a
--   later migration tightens them to NOT NULL to match Prisma.
--
-- The unique index covers Postgres's standard NULL-distinct
-- semantics — multiple rows with NULL external_id values do NOT
-- collide; only rows with concrete values are uniqueness-checked.
-- The legacy Prisma constraint behaves the same way (Prisma treats
-- @@unique on a nullable column-tuple as a SQL-standard unique
-- index, which permits multiple NULLs).
--
-- Skipped intentionally (later phases):
--   permissionSyncedAt          → Phase H (permission sync).
--   indexedCommitHash           → Phase C (repo indexing).
--   latestIndexingJobStatus     → Phase C (repo indexing).
--   zoektShardGroupId + table   → Phase C (Zoekt sharding).
ALTER TABLE "Repo"
    ADD COLUMN IF NOT EXISTS "cloneUrl"              TEXT,
    ADD COLUMN IF NOT EXISTS "external_id"           TEXT,
    ADD COLUMN IF NOT EXISTS "external_codeHostUrl"  TEXT,
    ADD COLUMN IF NOT EXISTS "metadata"              JSONB,
    ADD COLUMN IF NOT EXISTS "isPublic"              BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS "isAutoCleanupDisabled" BOOLEAN NOT NULL DEFAULT FALSE;

-- Unique constraint mirrors the legacy
-- @@unique([external_id, external_codeHostUrl, orgId]) on
-- packages/db/prisma/schema.prisma:97. The worker's per-repo
-- upsert path keys on this tuple.
CREATE UNIQUE INDEX IF NOT EXISTS "Repo_external_id_codeHostUrl_orgId_key"
    ON "Repo" ("external_id", "external_codeHostUrl", "orgId");
