-- Slice S.5 of the codeintel ↔ legacy schema-parity recovery.
-- Adds the three permission-sync tables byte-equal vs legacy:
--
--   AccountPermissionSyncJob — async job that re-syncs a single
--                              Account's repo permission set.
--   AccountToRepoPermission  — many-to-many edge: which Account
--                              has access to which Repo, with a
--                              `source` enum recording whether
--                              the grant came from the account
--                              side or the repo side.
--   RepoPermissionSyncJob    — async job that re-syncs a single
--                              Repo's permission set.
--
-- All FKs cascade-cascade. Uses 3 enum types from S.1 (0011):
-- AccountPermissionSyncJobStatus, RepoPermissionSyncJobStatus,
-- PermissionSyncSource.
--
-- Prereqs satisfied:
--   - Account table (created in S.4 / 0014).
--   - Repo table (codeintel 0001 + S.3a / 0013 extensions).
--   - All three enum types (S.1 / 0011).

-- AccountPermissionSyncJob -----------------------------------

CREATE TABLE IF NOT EXISTS "AccountPermissionSyncJob" (
    "id"           TEXT                              NOT NULL,
    "status"       "AccountPermissionSyncJobStatus"  NOT NULL DEFAULT 'PENDING',
    "createdAt"    TIMESTAMP(3) WITHOUT TIME ZONE    NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"    TIMESTAMP(3) WITHOUT TIME ZONE    NOT NULL,
    "completedAt"  TIMESTAMP(3) WITHOUT TIME ZONE    NULL,
    "errorMessage" TEXT                              NULL,
    "accountId"    TEXT                              NOT NULL,
    CONSTRAINT "AccountPermissionSyncJob_pkey" PRIMARY KEY ("id")
);

DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'AccountPermissionSyncJob_accountId_fkey'
  ) THEN
    ALTER TABLE "AccountPermissionSyncJob"
      ADD CONSTRAINT "AccountPermissionSyncJob_accountId_fkey"
      FOREIGN KEY ("accountId") REFERENCES "Account"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- AccountToRepoPermission ------------------------------------
-- Composite PK on (repoId, accountId) is the legacy shape.

CREATE TABLE IF NOT EXISTS "AccountToRepoPermission" (
    "createdAt" TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "repoId"    INTEGER                         NOT NULL,
    "accountId" TEXT                            NOT NULL,
    "source"    "PermissionSyncSource"          NOT NULL DEFAULT 'ACCOUNT_DRIVEN',
    CONSTRAINT "AccountToRepoPermission_pkey" PRIMARY KEY ("repoId", "accountId")
);

DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'AccountToRepoPermission_accountId_fkey'
  ) THEN
    ALTER TABLE "AccountToRepoPermission"
      ADD CONSTRAINT "AccountToRepoPermission_accountId_fkey"
      FOREIGN KEY ("accountId") REFERENCES "Account"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'AccountToRepoPermission_repoId_fkey'
  ) THEN
    ALTER TABLE "AccountToRepoPermission"
      ADD CONSTRAINT "AccountToRepoPermission_repoId_fkey"
      FOREIGN KEY ("repoId") REFERENCES "Repo"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- RepoPermissionSyncJob --------------------------------------

CREATE TABLE IF NOT EXISTS "RepoPermissionSyncJob" (
    "id"           TEXT                            NOT NULL,
    "status"       "RepoPermissionSyncJobStatus"   NOT NULL DEFAULT 'PENDING',
    "createdAt"    TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"    TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "completedAt"  TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "errorMessage" TEXT                            NULL,
    "repoId"       INTEGER                         NOT NULL,
    CONSTRAINT "RepoPermissionSyncJob_pkey" PRIMARY KEY ("id")
);

DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'RepoPermissionSyncJob_repoId_fkey'
  ) THEN
    ALTER TABLE "RepoPermissionSyncJob"
      ADD CONSTRAINT "RepoPermissionSyncJob_repoId_fkey"
      FOREIGN KEY ("repoId") REFERENCES "Repo"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;
