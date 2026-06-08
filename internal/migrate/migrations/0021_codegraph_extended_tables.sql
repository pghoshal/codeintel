-- Slice S.8 of the codeintel ↔ legacy schema-parity recovery.
-- Adds the 5 CodeGraph extended tables byte-equal vs legacy:
--
--   CodeGraphAnchor             — anchor record linking a graph
--                                  node to a source location.
--   CodeGraphRevision           — per-revision index pointer.
--   CodeGraphSemanticEdge       — semantic edge between symbols.
--   CodeGraphSemanticFact       — extracted semantic fact.
--   CodeGraphSemanticHyperedge  — multi-node semantic relation.
--
-- All five tables FK to CodeGraphIndex via an 8-column
-- composite key (id, orgId, repoId, commitHash, provider,
-- schemaVersion, builderVersion, workspaceId) which the S.3b
-- migration created the matching unique index for.
--
-- Uses 3 enums from S.1: CodeGraphAnchorDirection,
-- CodeGraphFactConfidenceTier, CodeGraphProvider.

-- 1. CodeGraphAnchor -----------------------------------------

CREATE TABLE IF NOT EXISTS "CodeGraphAnchor" (
    "id"               TEXT                            NOT NULL,
    "kind"             TEXT                            NOT NULL,
    "direction"        "CodeGraphAnchorDirection"      NOT NULL,
    "key"              TEXT                            NOT NULL,
    "normalizedKey"    TEXT                            NOT NULL,
    "nodeVid"          TEXT                            NOT NULL,
    "workspaceId"      TEXT                            NOT NULL,
    "commitHash"       TEXT                            NOT NULL,
    "provider"         "CodeGraphProvider"             NOT NULL DEFAULT 'NEBULA',
    "schemaVersion"    INTEGER                         NOT NULL DEFAULT 1,
    "builderVersion"   TEXT                            NOT NULL,
    "evidenceFilePath" TEXT                            NULL,
    "startLine"        INTEGER                         NULL,
    "endLine"          INTEGER                         NULL,
    "confidence"       DOUBLE PRECISION                NOT NULL DEFAULT 1.0,
    "source"           TEXT                            NOT NULL,
    "createdAt"        TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"        TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"            INTEGER                         NOT NULL,
    "repoId"           INTEGER                         NOT NULL,
    "graphIndexId"     TEXT                            NOT NULL,
    CONSTRAINT "CodeGraphAnchor_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "CodeGraphAnchor_graphIndexId_idx"
    ON "CodeGraphAnchor" ("graphIndexId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphAnchor_graphIndexId_kind_direction_normalizedKey_nodeV"
    ON "CodeGraphAnchor" ("graphIndexId", "kind", "direction", "normalizedKey", "nodeVid");

CREATE INDEX IF NOT EXISTS "CodeGraphAnchor_orgId_repoId_kind_normalizedKey_idx"
    ON "CodeGraphAnchor" ("orgId", "repoId", "kind", "normalizedKey");

CREATE INDEX IF NOT EXISTS "CodeGraphAnchor_orgId_workspaceId_kind_normalizedKey_direction_"
    ON "CodeGraphAnchor" ("orgId", "workspaceId", "kind", "normalizedKey", "direction");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphAnchor_orgId_fkey') THEN
    ALTER TABLE "CodeGraphAnchor"
      ADD CONSTRAINT "CodeGraphAnchor_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphAnchor_repoId_fkey') THEN
    ALTER TABLE "CodeGraphAnchor"
      ADD CONSTRAINT "CodeGraphAnchor_repoId_fkey"
      FOREIGN KEY ("repoId", "orgId") REFERENCES "Repo"("id", "orgId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  -- 8-column composite FK to CodeGraphIndex (id, orgId, repoId,
  -- commitHash, provider, schemaVersion, builderVersion, workspaceId).
  -- Long name truncated by PostgreSQL to 63 chars matching legacy.
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphAnchor_graphIndexId_orgId_repoId_commitHash_provider_s') THEN
    ALTER TABLE "CodeGraphAnchor"
      ADD CONSTRAINT "CodeGraphAnchor_graphIndexId_orgId_repoId_commitHash_provider_s"
      FOREIGN KEY ("graphIndexId", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      REFERENCES "CodeGraphIndex"("id", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- 2. CodeGraphRevision ---------------------------------------

CREATE TABLE IF NOT EXISTS "CodeGraphRevision" (
    "id"               TEXT                            NOT NULL,
    "revision"         TEXT                            NOT NULL,
    "workspaceId"      TEXT                            NOT NULL,
    "commitHash"       TEXT                            NOT NULL,
    "provider"         "CodeGraphProvider"             NOT NULL DEFAULT 'NEBULA',
    "schemaVersion"    INTEGER                         NOT NULL DEFAULT 1,
    "builderVersion"   TEXT                            NOT NULL,
    "activatedAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "createdAt"        TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"        TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"            INTEGER                         NOT NULL,
    "repoId"           INTEGER                         NOT NULL,
    "codeGraphIndexId" TEXT                            NOT NULL,
    CONSTRAINT "CodeGraphRevision_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "CodeGraphRevision_orgId_repoId_codeGraphIndexId_idx"
    ON "CodeGraphRevision" ("orgId", "repoId", "codeGraphIndexId");

CREATE INDEX IF NOT EXISTS "CodeGraphRevision_orgId_workspaceId_revision_idx"
    ON "CodeGraphRevision" ("orgId", "workspaceId", "revision");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphRevision_repoId_workspaceId_revision_provider_schemaVe"
    ON "CodeGraphRevision" ("repoId", "workspaceId", "revision", "provider", "schemaVersion", "builderVersion");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphRevision_orgId_fkey') THEN
    ALTER TABLE "CodeGraphRevision"
      ADD CONSTRAINT "CodeGraphRevision_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphRevision_repoId_fkey') THEN
    ALTER TABLE "CodeGraphRevision"
      ADD CONSTRAINT "CodeGraphRevision_repoId_fkey"
      FOREIGN KEY ("repoId", "orgId") REFERENCES "Repo"("id", "orgId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphRevision_codeGraphIndexId_orgId_repoId_commitHash_prov') THEN
    ALTER TABLE "CodeGraphRevision"
      ADD CONSTRAINT "CodeGraphRevision_codeGraphIndexId_orgId_repoId_commitHash_prov"
      FOREIGN KEY ("codeGraphIndexId", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      REFERENCES "CodeGraphIndex"("id", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- 3. CodeGraphSemanticEdge -----------------------------------

CREATE TABLE IF NOT EXISTS "CodeGraphSemanticEdge" (
    "id"                 TEXT                            NOT NULL,
    "sourceExternalId"   TEXT                            NOT NULL,
    "targetExternalId"   TEXT                            NOT NULL,
    "dedupeKey"          TEXT                            NOT NULL,
    "relation"           TEXT                            NOT NULL,
    "workspaceId"        TEXT                            NOT NULL,
    "commitHash"         TEXT                            NOT NULL,
    "provider"           "CodeGraphProvider"             NOT NULL DEFAULT 'NEBULA',
    "schemaVersion"      INTEGER                         NOT NULL DEFAULT 1,
    "builderVersion"     TEXT                            NOT NULL,
    "sourceFile"         TEXT                            NOT NULL,
    "startLine"          INTEGER                         NULL,
    "endLine"            INTEGER                         NULL,
    "evidence"           TEXT                            NULL,
    "evidenceHash"       TEXT                            NULL,
    "rationale"          TEXT                            NULL,
    "confidenceTier"     "CodeGraphFactConfidenceTier"   NOT NULL,
    "confidence"         DOUBLE PRECISION                NOT NULL DEFAULT 1.0,
    "source"             TEXT                            NOT NULL,
    "extractionMethod"   TEXT                            NOT NULL,
    "episodeId"          TEXT                            NULL,
    "validFrom"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "validTo"            TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "invalidatedAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "invalidationReason" TEXT                            NULL,
    "createdAt"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"              INTEGER                         NOT NULL,
    "repoId"             INTEGER                         NOT NULL,
    "graphIndexId"       TEXT                            NOT NULL,
    CONSTRAINT "CodeGraphSemanticEdge_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticEdge_episodeId_idx"
    ON "CodeGraphSemanticEdge" ("episodeId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphSemanticEdge_graphIndexId_dedupeKey_key"
    ON "CodeGraphSemanticEdge" ("graphIndexId", "dedupeKey");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticEdge_graphIndexId_idx"
    ON "CodeGraphSemanticEdge" ("graphIndexId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphSemanticEdge_graphIndexId_sourceExternalId_targetExter"
    ON "CodeGraphSemanticEdge" ("graphIndexId", "sourceExternalId", "targetExternalId", "relation", "sourceFile", "startLine", "endLine");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticEdge_orgId_repoId_sourceFile_idx"
    ON "CodeGraphSemanticEdge" ("orgId", "repoId", "sourceFile");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticEdge_orgId_workspaceId_relation_idx"
    ON "CodeGraphSemanticEdge" ("orgId", "workspaceId", "relation");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticEdge_orgId_fkey') THEN
    ALTER TABLE "CodeGraphSemanticEdge"
      ADD CONSTRAINT "CodeGraphSemanticEdge_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticEdge_repoId_orgId_fkey') THEN
    ALTER TABLE "CodeGraphSemanticEdge"
      ADD CONSTRAINT "CodeGraphSemanticEdge_repoId_orgId_fkey"
      FOREIGN KEY ("repoId", "orgId") REFERENCES "Repo"("id", "orgId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticEdge_graphIndexId_orgId_repoId_commitHash_prov') THEN
    ALTER TABLE "CodeGraphSemanticEdge"
      ADD CONSTRAINT "CodeGraphSemanticEdge_graphIndexId_orgId_repoId_commitHash_prov"
      FOREIGN KEY ("graphIndexId", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      REFERENCES "CodeGraphIndex"("id", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- 4. CodeGraphSemanticFact -----------------------------------

CREATE TABLE IF NOT EXISTS "CodeGraphSemanticFact" (
    "id"                 TEXT                            NOT NULL,
    "externalId"         TEXT                            NOT NULL,
    "dedupeKey"          TEXT                            NOT NULL,
    "kind"               TEXT                            NOT NULL,
    "label"              TEXT                            NOT NULL,
    "workspaceId"        TEXT                            NOT NULL,
    "commitHash"         TEXT                            NOT NULL,
    "provider"           "CodeGraphProvider"             NOT NULL DEFAULT 'NEBULA',
    "schemaVersion"      INTEGER                         NOT NULL DEFAULT 1,
    "builderVersion"     TEXT                            NOT NULL,
    "sourceFile"         TEXT                            NOT NULL,
    "startLine"          INTEGER                         NULL,
    "endLine"            INTEGER                         NULL,
    "evidence"           TEXT                            NULL,
    "evidenceHash"       TEXT                            NULL,
    "confidenceTier"     "CodeGraphFactConfidenceTier"   NOT NULL,
    "confidence"         DOUBLE PRECISION                NOT NULL DEFAULT 1.0,
    "source"             TEXT                            NOT NULL,
    "extractionMethod"   TEXT                            NOT NULL,
    "episodeId"          TEXT                            NULL,
    "validFrom"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "validTo"            TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "invalidatedAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "invalidationReason" TEXT                            NULL,
    "createdAt"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"              INTEGER                         NOT NULL,
    "repoId"             INTEGER                         NOT NULL,
    "graphIndexId"       TEXT                            NOT NULL,
    CONSTRAINT "CodeGraphSemanticFact_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticFact_episodeId_idx"
    ON "CodeGraphSemanticFact" ("episodeId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphSemanticFact_graphIndexId_dedupeKey_key"
    ON "CodeGraphSemanticFact" ("graphIndexId", "dedupeKey");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphSemanticFact_graphIndexId_externalId_key"
    ON "CodeGraphSemanticFact" ("graphIndexId", "externalId");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticFact_graphIndexId_idx"
    ON "CodeGraphSemanticFact" ("graphIndexId");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticFact_orgId_repoId_sourceFile_idx"
    ON "CodeGraphSemanticFact" ("orgId", "repoId", "sourceFile");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticFact_orgId_workspaceId_kind_label_idx"
    ON "CodeGraphSemanticFact" ("orgId", "workspaceId", "kind", "label");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticFact_orgId_fkey') THEN
    ALTER TABLE "CodeGraphSemanticFact"
      ADD CONSTRAINT "CodeGraphSemanticFact_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticFact_repoId_orgId_fkey') THEN
    ALTER TABLE "CodeGraphSemanticFact"
      ADD CONSTRAINT "CodeGraphSemanticFact_repoId_orgId_fkey"
      FOREIGN KEY ("repoId", "orgId") REFERENCES "Repo"("id", "orgId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticFact_graphIndexId_orgId_repoId_commitHash_prov') THEN
    ALTER TABLE "CodeGraphSemanticFact"
      ADD CONSTRAINT "CodeGraphSemanticFact_graphIndexId_orgId_repoId_commitHash_prov"
      FOREIGN KEY ("graphIndexId", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      REFERENCES "CodeGraphIndex"("id", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- 5. CodeGraphSemanticHyperedge ------------------------------

CREATE TABLE IF NOT EXISTS "CodeGraphSemanticHyperedge" (
    "id"                 TEXT                            NOT NULL,
    "externalId"         TEXT                            NOT NULL,
    "dedupeKey"          TEXT                            NOT NULL,
    "label"              TEXT                            NOT NULL,
    "relation"           TEXT                            NOT NULL,
    "nodeExternalIds"    TEXT[]                          NULL,
    "workspaceId"        TEXT                            NOT NULL,
    "commitHash"         TEXT                            NOT NULL,
    "provider"           "CodeGraphProvider"             NOT NULL DEFAULT 'NEBULA',
    "schemaVersion"      INTEGER                         NOT NULL DEFAULT 1,
    "builderVersion"     TEXT                            NOT NULL,
    "sourceFile"         TEXT                            NOT NULL,
    "startLine"          INTEGER                         NULL,
    "endLine"            INTEGER                         NULL,
    "evidence"           TEXT                            NULL,
    "evidenceHash"       TEXT                            NULL,
    "confidenceTier"     "CodeGraphFactConfidenceTier"   NOT NULL,
    "confidence"         DOUBLE PRECISION                NOT NULL DEFAULT 1.0,
    "source"             TEXT                            NOT NULL,
    "extractionMethod"   TEXT                            NOT NULL,
    "episodeId"          TEXT                            NULL,
    "validFrom"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "validTo"            TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "invalidatedAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "invalidationReason" TEXT                            NULL,
    "createdAt"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"          TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"              INTEGER                         NOT NULL,
    "repoId"             INTEGER                         NOT NULL,
    "graphIndexId"       TEXT                            NOT NULL,
    CONSTRAINT "CodeGraphSemanticHyperedge_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticHyperedge_episodeId_idx"
    ON "CodeGraphSemanticHyperedge" ("episodeId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphSemanticHyperedge_graphIndexId_dedupeKey_key"
    ON "CodeGraphSemanticHyperedge" ("graphIndexId", "dedupeKey");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphSemanticHyperedge_graphIndexId_externalId_key"
    ON "CodeGraphSemanticHyperedge" ("graphIndexId", "externalId");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticHyperedge_graphIndexId_idx"
    ON "CodeGraphSemanticHyperedge" ("graphIndexId");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticHyperedge_orgId_repoId_sourceFile_idx"
    ON "CodeGraphSemanticHyperedge" ("orgId", "repoId", "sourceFile");

CREATE INDEX IF NOT EXISTS "CodeGraphSemanticHyperedge_orgId_workspaceId_relation_idx"
    ON "CodeGraphSemanticHyperedge" ("orgId", "workspaceId", "relation");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticHyperedge_orgId_fkey') THEN
    ALTER TABLE "CodeGraphSemanticHyperedge"
      ADD CONSTRAINT "CodeGraphSemanticHyperedge_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticHyperedge_repoId_orgId_fkey') THEN
    ALTER TABLE "CodeGraphSemanticHyperedge"
      ADD CONSTRAINT "CodeGraphSemanticHyperedge_repoId_orgId_fkey"
      FOREIGN KEY ("repoId", "orgId") REFERENCES "Repo"("id", "orgId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeGraphSemanticHyperedge_graphIndexId_orgId_repoId_commitHash') THEN
    ALTER TABLE "CodeGraphSemanticHyperedge"
      ADD CONSTRAINT "CodeGraphSemanticHyperedge_graphIndexId_orgId_repoId_commitHash"
      FOREIGN KEY ("graphIndexId", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      REFERENCES "CodeGraphIndex"("id", "orgId", "repoId", "commitHash", "provider", "schemaVersion", "builderVersion", "workspaceId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;
