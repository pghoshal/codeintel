-- Slice S.10 of the codeintel ↔ legacy schema-parity recovery
-- (final additive slice). Adds 6 remaining tables byte-equal vs
-- legacy:
--
--   SearchContext             — per-org named search context
--                                referencing a set of Repos.
--   _RepoToSearchContext      — Prisma implicit M2M Repo↔
--                                SearchContext.
--   OrgCodeIntelConfig        — per-org code-intel config (query
--                                mode + graph toggle).
--   RepoIndexManifest         — per-(repo, branch, commit) index
--                                manifest with strategy fields.
--   RepoIndexManifestFile     — per-file entry within a manifest.
--   RepoSemanticChunkManifest — per-(file, range, prompt, model,
--                                schema) chunk manifest.
--
-- After S.10 lands, the only outstanding schema work is:
--   - S.3b/S.3c destructive existing-table reconciliation
--   - S.8 CodeGraph extended (5 tables, blocked by S.3b's
--     CodeGraphIndex enum cast)
--   - AuditEvent→Audit rename (producer-API change)
--
-- Uses two enums from S.1: CodeIntelQueryMode (OrgCodeIntelConfig),
-- RepoIndexManifestStatus (RepoIndexManifest).

-- SearchContext ----------------------------------------------

CREATE TABLE IF NOT EXISTS "SearchContext" (
    "id"            SERIAL  PRIMARY KEY,
    "name"          TEXT    NOT NULL,
    "description"   TEXT    NULL,
    "orgId"         INTEGER NOT NULL,
    "config"        JSONB   NULL,
    "isDeclarative" BOOLEAN NOT NULL DEFAULT FALSE
);

CREATE UNIQUE INDEX IF NOT EXISTS "SearchContext_name_orgId_key"
    ON "SearchContext" ("name", "orgId");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'SearchContext_orgId_fkey') THEN
    ALTER TABLE "SearchContext"
      ADD CONSTRAINT "SearchContext_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- _RepoToSearchContext (Prisma implicit M2M) ----------------

CREATE TABLE IF NOT EXISTS "_RepoToSearchContext" (
    "A" INTEGER NOT NULL,
    "B" INTEGER NOT NULL,
    CONSTRAINT "_RepoToSearchContext_AB_pkey" PRIMARY KEY ("A", "B")
);

CREATE INDEX IF NOT EXISTS "_RepoToSearchContext_B_index"
    ON "_RepoToSearchContext" ("B");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = '_RepoToSearchContext_A_fkey') THEN
    ALTER TABLE "_RepoToSearchContext"
      ADD CONSTRAINT "_RepoToSearchContext_A_fkey"
      FOREIGN KEY ("A") REFERENCES "Repo"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = '_RepoToSearchContext_B_fkey') THEN
    ALTER TABLE "_RepoToSearchContext"
      ADD CONSTRAINT "_RepoToSearchContext_B_fkey"
      FOREIGN KEY ("B") REFERENCES "SearchContext"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- OrgCodeIntelConfig -----------------------------------------

CREATE TABLE IF NOT EXISTS "OrgCodeIntelConfig" (
    "id"                    SERIAL                          PRIMARY KEY,
    "queryMode"             "CodeIntelQueryMode"            NOT NULL DEFAULT 'HYBRID',
    "codeGraphQueryEnabled" BOOLEAN                         NOT NULL DEFAULT TRUE,
    "createdAt"             TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"             TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"                 INTEGER                         NOT NULL
);

CREATE INDEX IF NOT EXISTS "OrgCodeIntelConfig_orgId_idx"
    ON "OrgCodeIntelConfig" ("orgId");

CREATE UNIQUE INDEX IF NOT EXISTS "OrgCodeIntelConfig_orgId_key"
    ON "OrgCodeIntelConfig" ("orgId");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'OrgCodeIntelConfig_orgId_fkey') THEN
    ALTER TABLE "OrgCodeIntelConfig"
      ADD CONSTRAINT "OrgCodeIntelConfig_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- RepoIndexManifest ------------------------------------------

CREATE TABLE IF NOT EXISTS "RepoIndexManifest" (
    "id"                    TEXT                            NOT NULL,
    "status"                "RepoIndexManifestStatus"       NOT NULL DEFAULT 'PENDING',
    "workspaceId"           TEXT                            NOT NULL,
    "providerConnectionId"  TEXT                            NULL,
    "branch"                TEXT                            NOT NULL,
    "commitHash"            TEXT                            NOT NULL,
    "plan"                  JSONB                           NULL,
    "fileCount"             INTEGER                         NOT NULL DEFAULT 0,
    "addedFileCount"        INTEGER                         NOT NULL DEFAULT 0,
    "changedFileCount"      INTEGER                         NOT NULL DEFAULT 0,
    "deletedFileCount"      INTEGER                         NOT NULL DEFAULT 0,
    "unchangedFileCount"    INTEGER                         NOT NULL DEFAULT 0,
    "zoektStrategy"         TEXT                            NULL,
    "scipStrategy"          TEXT                            NULL,
    "graphStrategy"         TEXT                            NULL,
    "semanticStrategy"      TEXT                            NULL,
    "semanticPromptVersion" TEXT                            NULL,
    "semanticModelId"       TEXT                            NULL,
    "semanticSchemaVersion" INTEGER                         NULL,
    "activatedAt"           TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "supersededAt"          TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "failedAt"              TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "errorMessage"          TEXT                            NULL,
    "createdAt"             TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"             TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"                 INTEGER                         NOT NULL,
    "repoId"                INTEGER                         NOT NULL,
    "indexJobId"            TEXT                            NULL,
    CONSTRAINT "RepoIndexManifest_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "RepoIndexManifest_indexJobId_idx"
    ON "RepoIndexManifest" ("indexJobId");

CREATE INDEX IF NOT EXISTS "RepoIndexManifest_orgId_workspaceId_repoId_branch_status_idx"
    ON "RepoIndexManifest" ("orgId", "workspaceId", "repoId", "branch", "status");

CREATE INDEX IF NOT EXISTS "RepoIndexManifest_repoId_workspaceId_branch_commitHash_idx"
    ON "RepoIndexManifest" ("repoId", "workspaceId", "branch", "commitHash");

CREATE INDEX IF NOT EXISTS "RepoIndexManifest_status_createdAt_idx"
    ON "RepoIndexManifest" ("status", "createdAt");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'RepoIndexManifest_indexJobId_fkey') THEN
    ALTER TABLE "RepoIndexManifest"
      ADD CONSTRAINT "RepoIndexManifest_indexJobId_fkey"
      FOREIGN KEY ("indexJobId") REFERENCES "RepoIndexingJob"("id")
      ON UPDATE CASCADE ON DELETE SET NULL;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'RepoIndexManifest_orgId_fkey') THEN
    ALTER TABLE "RepoIndexManifest"
      ADD CONSTRAINT "RepoIndexManifest_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  -- Composite FK to Repo(id, orgId) — depends on
  -- Repo_id_orgId_key unique idx landed in S.3a (0013).
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'RepoIndexManifest_repoId_orgId_fkey') THEN
    ALTER TABLE "RepoIndexManifest"
      ADD CONSTRAINT "RepoIndexManifest_repoId_orgId_fkey"
      FOREIGN KEY ("repoId", "orgId") REFERENCES "Repo"("id", "orgId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- RepoIndexManifestFile --------------------------------------

CREATE TABLE IF NOT EXISTS "RepoIndexManifestFile" (
    "id"          TEXT                            NOT NULL,
    "path"        TEXT                            NOT NULL,
    "contentHash" TEXT                            NOT NULL,
    "language"    TEXT                            NULL,
    "projectRoot" TEXT                            NULL,
    "generated"   BOOLEAN                         NOT NULL DEFAULT FALSE,
    "vendor"      BOOLEAN                         NOT NULL DEFAULT FALSE,
    "test"        BOOLEAN                         NOT NULL DEFAULT FALSE,
    "artifacts"   JSONB                           NULL,
    "createdAt"   TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "manifestId"  TEXT                            NOT NULL,
    CONSTRAINT "RepoIndexManifestFile_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "RepoIndexManifestFile_manifestId_contentHash_idx"
    ON "RepoIndexManifestFile" ("manifestId", "contentHash");

CREATE UNIQUE INDEX IF NOT EXISTS "RepoIndexManifestFile_manifestId_path_key"
    ON "RepoIndexManifestFile" ("manifestId", "path");

CREATE INDEX IF NOT EXISTS "RepoIndexManifestFile_path_idx"
    ON "RepoIndexManifestFile" ("path");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'RepoIndexManifestFile_manifestId_fkey') THEN
    ALTER TABLE "RepoIndexManifestFile"
      ADD CONSTRAINT "RepoIndexManifestFile_manifestId_fkey"
      FOREIGN KEY ("manifestId") REFERENCES "RepoIndexManifest"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- RepoSemanticChunkManifest ----------------------------------

CREATE TABLE IF NOT EXISTS "RepoSemanticChunkManifest" (
    "id"                TEXT                            NOT NULL,
    "filePath"          TEXT                            NOT NULL,
    "startLine"         INTEGER                         NOT NULL,
    "endLine"           INTEGER                         NOT NULL,
    "contentHash"       TEXT                            NOT NULL,
    "promptVersion"     TEXT                            NOT NULL,
    "modelId"           TEXT                            NOT NULL,
    "schemaVersion"     INTEGER                         NOT NULL,
    "acceptedFactIds"   TEXT[]                          NULL,
    "rejectedFactCount" INTEGER                         NOT NULL DEFAULT 0,
    "createdAt"         TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"         TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "manifestId"        TEXT                            NOT NULL,
    CONSTRAINT "RepoSemanticChunkManifest_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "RepoSemanticChunkManifest_contentHash_promptVersion_modelId_sch"
    ON "RepoSemanticChunkManifest" ("contentHash", "promptVersion", "modelId", "schemaVersion");

CREATE INDEX IF NOT EXISTS "RepoSemanticChunkManifest_manifestId_filePath_idx"
    ON "RepoSemanticChunkManifest" ("manifestId", "filePath");

CREATE UNIQUE INDEX IF NOT EXISTS "RepoSemanticChunkManifest_manifestId_filePath_startLine_endLine"
    ON "RepoSemanticChunkManifest" ("manifestId", "filePath", "startLine", "endLine", "promptVersion", "modelId", "schemaVersion");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'RepoSemanticChunkManifest_manifestId_fkey') THEN
    ALTER TABLE "RepoSemanticChunkManifest"
      ADD CONSTRAINT "RepoSemanticChunkManifest_manifestId_fkey"
      FOREIGN KEY ("manifestId") REFERENCES "RepoIndexManifest"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;
