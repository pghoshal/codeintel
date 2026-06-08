-- Slice S.3b of the codeintel ↔ legacy schema-parity recovery.
-- Destructive existing-table reconciliation: convert text
-- columns to their proper enum types on the four existing
-- tables that had this drift, plus add missing CodeGraphIndex
-- indexes (including the 8-column unique idx that S.8's
-- CodeGraph extended tables FK to).
--
-- Safety: this migration is DESTRUCTIVE if existing rows hold
-- values not in the enum label set. It is safe to run on a
-- codeintel dev DB (zero existing rows in any affected table
-- as of 2026-05-24). Production-grade migration paths need a
-- separate backfill+validate phase.
--
-- Idempotency: each ALTER COLUMN is wrapped in a DO block that
-- checks information_schema.columns for the current type before
-- attempting the cast. Re-runs after a successful apply are
-- no-ops.
--
-- Deferred to S.3c (later destructive slice):
--   - timestamptz → timestamp(3) without time zone drops.
--   - cloneUrl / external_id / external_codeHostUrl / metadata
--     NOT NULL tightenings on Repo.
--   - Composite FK Repo_orgId_fkey ON UPDATE CASCADE.
--   - CodeGraphIndex.Repo FK: single-col → composite (repoId, orgId).
--   - Drop codeintel-specific indexes not in legacy
--     (e.g., CodeGraphIndex_repoId_updatedAt_idx).

-- 0. Drop codeintel-specific indexes not in legacy ---------
-- These exist in the codeintel migrations but were never in
-- legacy's Prisma schema. Drop them for parity. The partial
-- index on RepoIndexingJob.status (with WHERE clause comparing
-- against `'FAILED'::text`) is the load-bearing one because
-- its predicate blocks the column-type ALTER below.

DROP INDEX IF EXISTS "RepoIndexingJob_failed_updatedAt_idx";
DROP INDEX IF EXISTS "RepoIndexingJob_repoId_createdAt_idx";
DROP INDEX IF EXISTS "CodeGraphIndex_repoId_updatedAt_idx";
DROP INDEX IF EXISTS "CodeIntelIndex_repoId_updatedAt_idx";
DROP INDEX IF EXISTS "Connection_orgId_idx";
DROP INDEX IF EXISTS "ApiKey_orgId_idx";
DROP INDEX IF EXISTS "ConnectionSyncJob_connectionId_createdAt_idx";
DROP INDEX IF EXISTS "RepoToConnection_connectionId_idx";

-- 1. CodeGraphIndex enum casts -------------------------------

DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'CodeGraphIndex' AND column_name = 'provider') = 'text' THEN
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "provider" DROP DEFAULT;
    ALTER TABLE "CodeGraphIndex"
      ALTER COLUMN "provider" TYPE "CodeGraphProvider" USING "provider"::"CodeGraphProvider";
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "provider"
      SET DEFAULT 'NEBULA'::"CodeGraphProvider";
  END IF;
END $$;

DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'CodeGraphIndex' AND column_name = 'status') = 'text' THEN
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "status" DROP DEFAULT;
    ALTER TABLE "CodeGraphIndex"
      ALTER COLUMN "status" TYPE "CodeGraphIndexStatus" USING "status"::"CodeGraphIndexStatus";
    ALTER TABLE "CodeGraphIndex" ALTER COLUMN "status"
      SET DEFAULT 'PENDING'::"CodeGraphIndexStatus";
  END IF;
END $$;

-- 2. CodeGraphIndex missing indexes (incl the 8-column unique
-- that S.8 FKs to) ------------------------------------------

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphIndex_id_orgId_repoId_commitHash_key"
    ON "CodeGraphIndex" ("id", "orgId", "repoId", "commitHash");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphIndex_id_orgId_repoId_commitHash_provider_schemaVersio"
    ON "CodeGraphIndex" ("id", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphIndex_id_orgId_repoId_key"
    ON "CodeGraphIndex" ("id", "orgId", "repoId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphIndex_id_orgId_repoId_provider_schemaVersion_builderVe"
    ON "CodeGraphIndex" ("id", "orgId", "repoId", "provider", "schemaVersion", "builderVersion");

CREATE INDEX IF NOT EXISTS "CodeGraphIndex_deleteAfter_status_idx"
    ON "CodeGraphIndex" ("deleteAfter", "status");

CREATE INDEX IF NOT EXISTS "CodeGraphIndex_orgId_repoId_commitHash_idx"
    ON "CodeGraphIndex" ("orgId", "repoId", "commitHash");

CREATE INDEX IF NOT EXISTS "CodeGraphIndex_orgId_repoId_status_idx"
    ON "CodeGraphIndex" ("orgId", "repoId", "status");

-- 3. CodeIntelIndex enum casts -------------------------------

DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'CodeIntelIndex' AND column_name = 'kind') = 'text' THEN
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "kind" DROP DEFAULT;
    ALTER TABLE "CodeIntelIndex"
      ALTER COLUMN "kind" TYPE "CodeIntelIndexKind" USING "kind"::"CodeIntelIndexKind";
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "kind"
      SET DEFAULT 'SCIP'::"CodeIntelIndexKind";
  END IF;
END $$;

DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'CodeIntelIndex' AND column_name = 'status') = 'text' THEN
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "status" DROP DEFAULT;
    ALTER TABLE "CodeIntelIndex"
      ALTER COLUMN "status" TYPE "CodeIntelIndexStatus" USING "status"::"CodeIntelIndexStatus";
    ALTER TABLE "CodeIntelIndex" ALTER COLUMN "status"
      SET DEFAULT 'PENDING'::"CodeIntelIndexStatus";
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS "CodeIntelIndex_orgId_repoId_revision_idx"
    ON "CodeIntelIndex" ("orgId", "repoId", "revision");

-- 4. Repo enum casts -----------------------------------------

DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'external_codeHostType') = 'text' THEN
    ALTER TABLE "Repo"
      ALTER COLUMN "external_codeHostType" TYPE "CodeHostType"
      USING "external_codeHostType"::"CodeHostType";
  END IF;
END $$;

DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'Repo' AND column_name = 'latestIndexingJobStatus') = 'text' THEN
    ALTER TABLE "Repo"
      ALTER COLUMN "latestIndexingJobStatus" TYPE "RepoIndexingJobStatus"
      USING "latestIndexingJobStatus"::"RepoIndexingJobStatus";
  END IF;
END $$;

-- 5. RepoIndexingJob enum casts ------------------------------

DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'RepoIndexingJob' AND column_name = 'type') = 'text' THEN
    ALTER TABLE "RepoIndexingJob"
      ALTER COLUMN "type" TYPE "RepoIndexingJobType"
      USING "type"::"RepoIndexingJobType";
  END IF;
END $$;

DO $$ BEGIN
  IF (SELECT data_type FROM information_schema.columns
      WHERE table_name = 'RepoIndexingJob' AND column_name = 'status') = 'text' THEN
    ALTER TABLE "RepoIndexingJob" ALTER COLUMN "status" DROP DEFAULT;
    ALTER TABLE "RepoIndexingJob"
      ALTER COLUMN "status" TYPE "RepoIndexingJobStatus"
      USING "status"::"RepoIndexingJobStatus";
    ALTER TABLE "RepoIndexingJob" ALTER COLUMN "status"
      SET DEFAULT 'PENDING'::"RepoIndexingJobStatus";
  END IF;
END $$;
