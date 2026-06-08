-- Extended scalar columns for the Repo table backing the
-- /api/repos listing's top-level RepositoryQuery projection. The
-- nullable columns map to the wire's optional fields
-- (externalWebUrl / imageUrl / pushedAt / defaultBranch); the
-- non-null boolean columns map to the wire's required boolean
-- fields (isFork / isArchived). external_codeHostType records the
-- repo's hosting service (github / gitlab / gitea / etc.); it
-- backs the wire's `codeHostType` field.
--
-- isFork / isArchived default to FALSE so existing rows backfill
-- without a separate UPDATE pass. New writes are expected to set
-- the correct value at insert time.
ALTER TABLE "Repo"
    ADD COLUMN IF NOT EXISTS "external_codeHostType" TEXT,
    ADD COLUMN IF NOT EXISTS "webUrl"                TEXT,
    ADD COLUMN IF NOT EXISTS "imageUrl"              TEXT,
    ADD COLUMN IF NOT EXISTS "pushedAt"              TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS "defaultBranch"         TEXT,
    ADD COLUMN IF NOT EXISTS "isFork"                BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS "isArchived"            BOOLEAN NOT NULL DEFAULT FALSE;
