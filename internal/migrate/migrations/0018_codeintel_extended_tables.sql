-- Slice S.9 of the codeintel ↔ legacy schema-parity recovery.
-- Adds the 5 CodeIntel extended tables byte-equal vs legacy:
--
--   CodeIntelToolchain      — toolchain registry (worker
--                              + language + indexer + platform).
--   CodeIntelLanguageIndex  — per-(index, language, projectRoot)
--                              indexing record.
--   CodeIntelSymbol         — symbol-level record from SCIP.
--   CodeIntelOccurrence     — occurrence-level record with role.
--   CodeIntelRelationship   — symbol-to-symbol relationship.
--
-- Toolchain is created first because LanguageIndex FKs to it.
-- All other tables FK to CodeIntelIndex(id) via single-column
-- which is supported regardless of codeintel's current enum
-- drift on CodeIntelIndex other columns (kind, status remain
-- TEXT until S.3b destructive reconciliation).
--
-- Uses two enums from S.1: CodeIntelIndexStatus,
-- CodeIntelOccurrenceRole.

-- CodeIntelToolchain -----------------------------------------

CREATE TABLE IF NOT EXISTS "CodeIntelToolchain" (
    "id"             TEXT                            NOT NULL,
    "fingerprint"    TEXT                            NOT NULL,
    "workerClass"    TEXT                            NOT NULL,
    "language"       TEXT                            NOT NULL,
    "indexer"        TEXT                            NOT NULL,
    "commandPath"    TEXT                            NULL,
    "commandVersion" TEXT                            NULL,
    "commandSha256"  TEXT                            NULL,
    "platform"       TEXT                            NOT NULL,
    "architecture"   TEXT                            NOT NULL,
    "sourceUrl"      TEXT                            NULL,
    "createdAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "lastSeenAt"     TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    CONSTRAINT "CodeIntelToolchain_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelToolchain_fingerprint_key"
    ON "CodeIntelToolchain" ("fingerprint");

CREATE INDEX IF NOT EXISTS "CodeIntelToolchain_fingerprint_idx"
    ON "CodeIntelToolchain" ("fingerprint");

CREATE INDEX IF NOT EXISTS "CodeIntelToolchain_workerClass_language_indexer_idx"
    ON "CodeIntelToolchain" ("workerClass", "language", "indexer");

-- CodeIntelLanguageIndex -------------------------------------

CREATE TABLE IF NOT EXISTS "CodeIntelLanguageIndex" (
    "id"                   TEXT                            NOT NULL,
    "language"             TEXT                            NOT NULL,
    "projectRoot"          TEXT                            NOT NULL,
    "indexer"              TEXT                            NOT NULL,
    "status"               "CodeIntelIndexStatus"          NOT NULL DEFAULT 'PENDING',
    "artifactPath"         TEXT                            NULL,
    "command"              TEXT                            NULL,
    "durationMs"           INTEGER                         NULL,
    "errorMessage"         TEXT                            NULL,
    "createdAt"            TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"            TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "codeIntelIndexId"     TEXT                            NOT NULL,
    "workerClass"          TEXT                            NULL,
    "toolchainFingerprint" TEXT                            NULL,
    "toolchainVersion"     TEXT                            NULL,
    "toolchainPath"        TEXT                            NULL,
    "toolchainSha256"      TEXT                            NULL,
    "toolchainId"          TEXT                            NULL,
    CONSTRAINT "CodeIntelLanguageIndex_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelLanguageIndex_codeIntelIndexId_language_projectRoot_in"
    ON "CodeIntelLanguageIndex" ("codeIntelIndexId", "language", "projectRoot", "indexer");

CREATE INDEX IF NOT EXISTS "CodeIntelLanguageIndex_status_idx"
    ON "CodeIntelLanguageIndex" ("status");

CREATE INDEX IF NOT EXISTS "CodeIntelLanguageIndex_toolchainId_idx"
    ON "CodeIntelLanguageIndex" ("toolchainId");

CREATE INDEX IF NOT EXISTS "CodeIntelLanguageIndex_workerClass_status_idx"
    ON "CodeIntelLanguageIndex" ("workerClass", "status");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelLanguageIndex_codeIntelIndexId_fkey') THEN
    ALTER TABLE "CodeIntelLanguageIndex"
      ADD CONSTRAINT "CodeIntelLanguageIndex_codeIntelIndexId_fkey"
      FOREIGN KEY ("codeIntelIndexId") REFERENCES "CodeIntelIndex"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelLanguageIndex_toolchainId_fkey') THEN
    ALTER TABLE "CodeIntelLanguageIndex"
      ADD CONSTRAINT "CodeIntelLanguageIndex_toolchainId_fkey"
      FOREIGN KEY ("toolchainId") REFERENCES "CodeIntelToolchain"("id")
      ON UPDATE CASCADE ON DELETE SET NULL;
  END IF;
END $$;

-- CodeIntelSymbol --------------------------------------------

CREATE TABLE IF NOT EXISTS "CodeIntelSymbol" (
    "id"               TEXT                            NOT NULL,
    "symbol"           TEXT                            NOT NULL,
    "displayName"      TEXT                            NOT NULL,
    "kind"             TEXT                            NULL,
    "language"         TEXT                            NULL,
    "documentation"    TEXT[]                          NULL,
    "signature"        TEXT                            NULL,
    "filePath"         TEXT                            NULL,
    "startLine"        INTEGER                         NULL,
    "startCharacter"   INTEGER                         NULL,
    "endLine"          INTEGER                         NULL,
    "endCharacter"     INTEGER                         NULL,
    "enclosingSymbol"  TEXT                            NULL,
    "createdAt"        TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "orgId"            INTEGER                         NOT NULL,
    "repoId"           INTEGER                         NOT NULL,
    "codeIntelIndexId" TEXT                            NOT NULL,
    CONSTRAINT "CodeIntelSymbol_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelSymbol_codeIntelIndexId_symbol_key"
    ON "CodeIntelSymbol" ("codeIntelIndexId", "symbol");

CREATE INDEX IF NOT EXISTS "CodeIntelSymbol_orgId_repoId_displayName_idx"
    ON "CodeIntelSymbol" ("orgId", "repoId", "displayName");

CREATE INDEX IF NOT EXISTS "CodeIntelSymbol_orgId_repoId_symbol_idx"
    ON "CodeIntelSymbol" ("orgId", "repoId", "symbol");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelSymbol_codeIntelIndexId_fkey') THEN
    ALTER TABLE "CodeIntelSymbol"
      ADD CONSTRAINT "CodeIntelSymbol_codeIntelIndexId_fkey"
      FOREIGN KEY ("codeIntelIndexId") REFERENCES "CodeIntelIndex"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelSymbol_orgId_fkey') THEN
    ALTER TABLE "CodeIntelSymbol"
      ADD CONSTRAINT "CodeIntelSymbol_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelSymbol_repoId_fkey') THEN
    ALTER TABLE "CodeIntelSymbol"
      ADD CONSTRAINT "CodeIntelSymbol_repoId_fkey"
      FOREIGN KEY ("repoId") REFERENCES "Repo"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- CodeIntelOccurrence ----------------------------------------

CREATE TABLE IF NOT EXISTS "CodeIntelOccurrence" (
    "id"               TEXT                            NOT NULL,
    "symbol"           TEXT                            NOT NULL,
    "filePath"         TEXT                            NOT NULL,
    "startLine"        INTEGER                         NOT NULL,
    "startCharacter"   INTEGER                         NOT NULL,
    "endLine"          INTEGER                         NOT NULL,
    "endCharacter"     INTEGER                         NOT NULL,
    "role"             "CodeIntelOccurrenceRole"       NOT NULL,
    "language"         TEXT                            NULL,
    "syntaxKind"       TEXT                            NULL,
    "lineContent"      TEXT                            NULL,
    "enclosingSymbol"  TEXT                            NULL,
    "createdAt"        TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "orgId"            INTEGER                         NOT NULL,
    "repoId"           INTEGER                         NOT NULL,
    "codeIntelIndexId" TEXT                            NOT NULL,
    CONSTRAINT "CodeIntelOccurrence_pkey" PRIMARY KEY ("id")
);

CREATE INDEX IF NOT EXISTS "CodeIntelOccurrence_codeIntelIndexId_symbol_role_idx"
    ON "CodeIntelOccurrence" ("codeIntelIndexId", "symbol", "role");

CREATE INDEX IF NOT EXISTS "CodeIntelOccurrence_orgId_repoId_filePath_idx"
    ON "CodeIntelOccurrence" ("orgId", "repoId", "filePath");

CREATE INDEX IF NOT EXISTS "CodeIntelOccurrence_orgId_repoId_symbol_role_idx"
    ON "CodeIntelOccurrence" ("orgId", "repoId", "symbol", "role");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelOccurrence_codeIntelIndexId_fkey') THEN
    ALTER TABLE "CodeIntelOccurrence"
      ADD CONSTRAINT "CodeIntelOccurrence_codeIntelIndexId_fkey"
      FOREIGN KEY ("codeIntelIndexId") REFERENCES "CodeIntelIndex"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelOccurrence_orgId_fkey') THEN
    ALTER TABLE "CodeIntelOccurrence"
      ADD CONSTRAINT "CodeIntelOccurrence_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelOccurrence_repoId_fkey') THEN
    ALTER TABLE "CodeIntelOccurrence"
      ADD CONSTRAINT "CodeIntelOccurrence_repoId_fkey"
      FOREIGN KEY ("repoId") REFERENCES "Repo"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- CodeIntelRelationship --------------------------------------

CREATE TABLE IF NOT EXISTS "CodeIntelRelationship" (
    "id"               TEXT                            NOT NULL,
    "sourceSymbol"     TEXT                            NOT NULL,
    "targetSymbol"     TEXT                            NOT NULL,
    "isReference"      BOOLEAN                         NOT NULL DEFAULT FALSE,
    "isImplementation" BOOLEAN                         NOT NULL DEFAULT FALSE,
    "isTypeDefinition" BOOLEAN                         NOT NULL DEFAULT FALSE,
    "isDefinition"     BOOLEAN                         NOT NULL DEFAULT FALSE,
    "createdAt"        TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "orgId"            INTEGER                         NOT NULL,
    "repoId"           INTEGER                         NOT NULL,
    "codeIntelIndexId" TEXT                            NOT NULL,
    CONSTRAINT "CodeIntelRelationship_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelRelationship_codeIntelIndexId_sourceSymbol_targetSymbo"
    ON "CodeIntelRelationship" ("codeIntelIndexId", "sourceSymbol", "targetSymbol", "isReference", "isImplementation", "isTypeDefinition", "isDefinition");

CREATE INDEX IF NOT EXISTS "CodeIntelRelationship_orgId_repoId_sourceSymbol_idx"
    ON "CodeIntelRelationship" ("orgId", "repoId", "sourceSymbol");

CREATE INDEX IF NOT EXISTS "CodeIntelRelationship_orgId_repoId_targetSymbol_idx"
    ON "CodeIntelRelationship" ("orgId", "repoId", "targetSymbol");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelRelationship_codeIntelIndexId_fkey') THEN
    ALTER TABLE "CodeIntelRelationship"
      ADD CONSTRAINT "CodeIntelRelationship_codeIntelIndexId_fkey"
      FOREIGN KEY ("codeIntelIndexId") REFERENCES "CodeIntelIndex"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelRelationship_orgId_fkey') THEN
    ALTER TABLE "CodeIntelRelationship"
      ADD CONSTRAINT "CodeIntelRelationship_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelRelationship_repoId_fkey') THEN
    ALTER TABLE "CodeIntelRelationship"
      ADD CONSTRAINT "CodeIntelRelationship_repoId_fkey"
      FOREIGN KEY ("repoId") REFERENCES "Repo"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;
