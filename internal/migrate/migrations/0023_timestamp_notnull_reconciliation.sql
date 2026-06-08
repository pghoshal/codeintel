-- Slice S.3c of the codeintel ↔ legacy schema-parity recovery.
-- Final destructive slice. Two concerns:
--
--   A. Convert every `timestamp with time zone` column on the
--      14 existing codeintel tables to `timestamp(3) without
--      time zone` to match legacy. The conversion drops the
--      timezone offset (cast is safe on empty tables; codeintel
--      dev DB has zero rows in all affected tables 2026-05-24).
--      Defaults change from `now()` to `CURRENT_TIMESTAMP`
--      where applicable.
--   B. Tighten 4 Repo columns to NOT NULL (cloneUrl, external_id,
--      external_codeHostUrl, metadata) matching legacy. The S.3a
--      migration deferred these as NULLABLE to be safe on a
--      DB with existing rows; the worker hasn't landed yet
--      (Phase B.1d) so no data carries through.
--   C. Drop Repo.isFork / Repo.isArchived DEFAULT FALSE — legacy
--      has them NOT NULL with no default (caller MUST supply).
--      The codeintel migrations added a default for convenience;
--      removing it for parity. Future INSERTs that omitted the
--      column would now fail — by design, callers must be
--      explicit.
--
-- Safety: every ALTER guard checks information_schema for the
-- current state, so the migration is idempotent on re-run.
-- Skip schema_migrations.applied_at — that's internal
-- bookkeeping, not part of the legacy schema.

-- ============================================================
-- A. timestamptz → timestamp(3) without time zone
-- ============================================================

-- Helper note: each ALTER pattern is
--   ALTER COLUMN col DROP DEFAULT;
--   ALTER COLUMN col TYPE TIMESTAMP(3) WITHOUT TIME ZONE
--     USING col::timestamp(3) without time zone;
--   ALTER COLUMN col SET DEFAULT CURRENT_TIMESTAMP;  -- if had now()
-- Wrapped in DO blocks gated on the current type.

-- ApiKey
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'ApiKey' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "ApiKey" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "ApiKey" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "ApiKey" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "ApiKey" ALTER COLUMN "lastUsedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "lastUsedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- CodeGraphIndex (5 timestamp cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'CodeGraphIndex' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
    -- legacy updatedAt has NO default (caller must supply on update).
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "indexedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "indexedAt"::timestamp(3) without time zone;
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "supersededAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "supersededAt"::timestamp(3) without time zone;
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "deleteAfter" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "deleteAfter"::timestamp(3) without time zone;
  END IF;
END $$;

-- CodeIntelIndex (3 timestamp cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'CodeIntelIndex' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "indexedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "indexedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- Connection (3 timestamp cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'Connection' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "Connection" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "Connection" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "Connection" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "Connection" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "Connection" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
    ALTER TABLE "Connection" ALTER COLUMN "syncedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "syncedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- ConnectionSyncJob (3 cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'ConnectionSyncJob' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "ConnectionSyncJob" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "ConnectionSyncJob" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "ConnectionSyncJob" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "ConnectionSyncJob" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "ConnectionSyncJob" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
    ALTER TABLE "ConnectionSyncJob" ALTER COLUMN "completedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "completedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- Org (2 cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'Org' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "Org" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "Org" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "Org" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "Org" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "Org" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- OrgLanguageModel (2 cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'OrgLanguageModel' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "OrgLanguageModel" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "OrgLanguageModel" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "OrgLanguageModel" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "OrgLanguageModel" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "OrgLanguageModel" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- OrgSecret (2 cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'OrgSecret' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "OrgSecret" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "OrgSecret" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "OrgSecret" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "OrgSecret" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "OrgSecret" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- Repo (4 cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "Repo" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "Repo" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "Repo" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "Repo" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "Repo" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
    ALTER TABLE "Repo" ALTER COLUMN "indexedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "indexedAt"::timestamp(3) without time zone;
    ALTER TABLE "Repo" ALTER COLUMN "pushedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "pushedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- RepoIndexingJob (3 cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'RepoIndexingJob' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "RepoIndexingJob" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "RepoIndexingJob" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "RepoIndexingJob" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "RepoIndexingJob" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "RepoIndexingJob" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
    ALTER TABLE "RepoIndexingJob" ALTER COLUMN "completedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "completedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- RepoToConnection
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'RepoToConnection' AND column_name = 'addedAt') = 'timestamp with time zone' THEN
    ALTER TABLE "RepoToConnection" ALTER COLUMN "addedAt" DROP DEFAULT;
    ALTER TABLE "RepoToConnection" ALTER COLUMN "addedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "addedAt"::timestamp(3) without time zone;
    ALTER TABLE "RepoToConnection" ALTER COLUMN "addedAt" SET DEFAULT CURRENT_TIMESTAMP;
  END IF;
END $$;

-- User (2 cols)
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'User' AND column_name = 'createdAt') = 'timestamp with time zone' THEN
    ALTER TABLE "User" ALTER COLUMN "createdAt" DROP DEFAULT;
    ALTER TABLE "User" ALTER COLUMN "createdAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "createdAt"::timestamp(3) without time zone;
    ALTER TABLE "User" ALTER COLUMN "createdAt" SET DEFAULT CURRENT_TIMESTAMP;
    ALTER TABLE "User" ALTER COLUMN "updatedAt" DROP DEFAULT;
    ALTER TABLE "User" ALTER COLUMN "updatedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "updatedAt"::timestamp(3) without time zone;
  END IF;
END $$;

-- UserToOrg
DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'UserToOrg' AND column_name = 'joinedAt') = 'timestamp with time zone' THEN
    ALTER TABLE "UserToOrg" ALTER COLUMN "joinedAt" DROP DEFAULT;
    ALTER TABLE "UserToOrg" ALTER COLUMN "joinedAt" TYPE TIMESTAMP(3) WITHOUT TIME ZONE
      USING "joinedAt"::timestamp(3) without time zone;
    ALTER TABLE "UserToOrg" ALTER COLUMN "joinedAt" SET DEFAULT CURRENT_TIMESTAMP;
  END IF;
END $$;

-- ============================================================
-- B. Repo NOT NULL tightening
-- ============================================================

-- The S.3a migration deferred these as NULLABLE so the migration
-- was safe on a DB with existing Repo rows. The connection-sync
-- worker hasn't landed yet so no rows carry through; tighten
-- now for parity with legacy.
DO $$ BEGIN
  IF (SELECT is_nullable FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'cloneUrl') = 'YES' THEN
    ALTER TABLE "Repo" ALTER COLUMN "cloneUrl" SET NOT NULL;
  END IF;
  IF (SELECT is_nullable FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'external_id') = 'YES' THEN
    ALTER TABLE "Repo" ALTER COLUMN "external_id" SET NOT NULL;
  END IF;
  IF (SELECT is_nullable FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'external_codeHostUrl') = 'YES' THEN
    ALTER TABLE "Repo" ALTER COLUMN "external_codeHostUrl" SET NOT NULL;
  END IF;
  IF (SELECT is_nullable FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'metadata') = 'YES' THEN
    ALTER TABLE "Repo" ALTER COLUMN "metadata" SET NOT NULL;
  END IF;
END $$;

-- ============================================================
-- C. Drop Repo.isFork / Repo.isArchived defaults
-- ============================================================

-- Legacy has them NOT NULL no default. Codeintel added DEFAULT FALSE
-- for convenience; remove for parity. Future INSERTs must supply
-- a value explicitly.
DO $$ BEGIN
  IF (SELECT column_default FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'isFork') = 'false' THEN
    ALTER TABLE "Repo" ALTER COLUMN "isFork" DROP DEFAULT;
  END IF;
  IF (SELECT column_default FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'isArchived') = 'false' THEN
    ALTER TABLE "Repo" ALTER COLUMN "isArchived" DROP DEFAULT;
  END IF;
END $$;

-- ============================================================
-- D. Also tighten Repo.external_codeHostType to NOT NULL
-- ============================================================

-- After S.3b's enum cast, Repo.external_codeHostType is still
-- nullable (codeintel-side default). Legacy has it NOT NULL.
DO $$ BEGIN
  IF (SELECT is_nullable FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'external_codeHostType') = 'YES' THEN
    ALTER TABLE "Repo" ALTER COLUMN "external_codeHostType" SET NOT NULL;
  END IF;
END $$;
