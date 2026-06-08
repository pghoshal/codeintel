-- Tie split-executor SCIP child rows back to the language/project
-- artifact that produced them. Existing read paths still use
-- CodeIntelIndex scope; this nullable link lets retries replace one
-- language/project artifact without deleting sibling language rows.

ALTER TABLE "CodeIntelSymbol"
  ADD COLUMN IF NOT EXISTS "codeIntelLanguageIndexId" TEXT NULL;

ALTER TABLE "CodeIntelOccurrence"
  ADD COLUMN IF NOT EXISTS "codeIntelLanguageIndexId" TEXT NULL;

ALTER TABLE "CodeIntelRelationship"
  ADD COLUMN IF NOT EXISTS "codeIntelLanguageIndexId" TEXT NULL;

ALTER TABLE "CodeIntelIndex"
  ADD COLUMN IF NOT EXISTS "workspaceId" TEXT NULL;

ALTER TABLE "CodeIntelIndex"
  ADD COLUMN IF NOT EXISTS branch TEXT NULL;

UPDATE "CodeIntelIndex" ci
SET "workspaceId" = COALESCE(ci."workspaceId", o."atomWorkspaceId", 'legacy')
FROM "Org" o
WHERE o.id = ci."orgId"
  AND ci."workspaceId" IS NULL;

UPDATE "CodeIntelIndex"
SET branch = revision
WHERE branch IS NULL;

ALTER TABLE "CodeIntelIndex"
  ALTER COLUMN "workspaceId" SET NOT NULL;

ALTER TABLE "CodeIntelIndex"
  ALTER COLUMN branch SET NOT NULL;

CREATE INDEX IF NOT EXISTS "CodeIntelSymbol_languageIndexId_idx"
  ON "CodeIntelSymbol" ("codeIntelLanguageIndexId");

CREATE INDEX IF NOT EXISTS "CodeIntelOccurrence_languageIndexId_idx"
  ON "CodeIntelOccurrence" ("codeIntelLanguageIndexId");

CREATE INDEX IF NOT EXISTS "CodeIntelRelationship_languageIndexId_idx"
  ON "CodeIntelRelationship" ("codeIntelLanguageIndexId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelIndex_id_orgId_repoId_key"
  ON "CodeIntelIndex" (id, "orgId", "repoId");

DROP INDEX IF EXISTS "CodeIntelIndex_repoId_revision_commitHash_kind_key";

CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelIndex_scope_revision_commit_kind_key"
  ON "CodeIntelIndex" ("orgId", "repoId", "workspaceId", branch, revision, "commitHash", kind);

CREATE INDEX IF NOT EXISTS "CodeIntelIndex_org_repo_workspace_branch_status_idx"
  ON "CodeIntelIndex" ("orgId", "repoId", "workspaceId", branch, status);

CREATE INDEX IF NOT EXISTS "CodeIntelIndex_repoId_updatedAt_idx"
  ON "CodeIntelIndex" ("repoId", "updatedAt" DESC);

WITH fallback_language AS (
  INSERT INTO "CodeIntelLanguageIndex" (
    id, language, "projectRoot", indexer, "workerClass", status,
    "artifactPath", command, "durationMs", "errorMessage",
    "createdAt", "updatedAt", "codeIntelIndexId"
  )
  SELECT
    'legacy-scip-' || ci.id,
    'unknown',
    '',
    'legacy-pre-split',
    NULL,
    CASE
      WHEN ci.status IN ('READY'::"CodeIntelIndexStatus", 'PARTIAL'::"CodeIntelIndexStatus") THEN 'READY'::"CodeIntelIndexStatus"
      WHEN ci.status = 'FAILED'::"CodeIntelIndexStatus" THEN 'FAILED'::"CodeIntelIndexStatus"
      ELSE 'INDEXING'::"CodeIntelIndexStatus"
    END,
    ci."artifactRoot",
    'legacy pre-split SCIP rows',
    NULL,
    NULL,
    NOW(),
    NOW(),
    ci.id
  FROM "CodeIntelIndex" ci
  WHERE ci.kind = 'SCIP'::"CodeIntelIndexKind"
    AND EXISTS (
      SELECT 1
      FROM "CodeIntelSymbol" s
      WHERE s."codeIntelIndexId" = ci.id
        AND s."codeIntelLanguageIndexId" IS NULL
    )
  ON CONFLICT ("codeIntelIndexId", language, "projectRoot", indexer)
  DO UPDATE SET "updatedAt" = NOW()
  RETURNING id, "codeIntelIndexId"
)
UPDATE "CodeIntelSymbol" s
SET "codeIntelLanguageIndexId" = COALESCE(fallback_language.id, li.id)
FROM "CodeIntelIndex" ci
LEFT JOIN fallback_language
  ON fallback_language."codeIntelIndexId" = ci.id
LEFT JOIN "CodeIntelLanguageIndex" li
  ON li."codeIntelIndexId" = ci.id
 AND li.language = 'unknown'
 AND li."projectRoot" = ''
 AND li.indexer = 'legacy-pre-split'
WHERE s."codeIntelIndexId" = ci.id
  AND s."codeIntelLanguageIndexId" IS NULL;

UPDATE "CodeIntelOccurrence" o
SET "codeIntelLanguageIndexId" = li.id
FROM "CodeIntelLanguageIndex" li
WHERE o."codeIntelIndexId" = li."codeIntelIndexId"
  AND li.language = 'unknown'
  AND li."projectRoot" = ''
  AND li.indexer = 'legacy-pre-split'
  AND o."codeIntelLanguageIndexId" IS NULL;

UPDATE "CodeIntelRelationship" r
SET "codeIntelLanguageIndexId" = li.id
FROM "CodeIntelLanguageIndex" li
WHERE r."codeIntelIndexId" = li."codeIntelIndexId"
  AND li.language = 'unknown'
  AND li."projectRoot" = ''
  AND li.indexer = 'legacy-pre-split'
  AND r."codeIntelLanguageIndexId" IS NULL;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelSymbol_languageIndexId_fkey') THEN
    ALTER TABLE "CodeIntelSymbol"
      ADD CONSTRAINT "CodeIntelSymbol_languageIndexId_fkey"
      FOREIGN KEY ("codeIntelLanguageIndexId") REFERENCES "CodeIntelLanguageIndex"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelOccurrence_languageIndexId_fkey') THEN
    ALTER TABLE "CodeIntelOccurrence"
      ADD CONSTRAINT "CodeIntelOccurrence_languageIndexId_fkey"
      FOREIGN KEY ("codeIntelLanguageIndexId") REFERENCES "CodeIntelLanguageIndex"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelRelationship_languageIndexId_fkey') THEN
    ALTER TABLE "CodeIntelRelationship"
      ADD CONSTRAINT "CodeIntelRelationship_languageIndexId_fkey"
      FOREIGN KEY ("codeIntelLanguageIndexId") REFERENCES "CodeIntelLanguageIndex"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelSymbol_codeIntelIndex_scope_fkey') THEN
    ALTER TABLE "CodeIntelSymbol"
      ADD CONSTRAINT "CodeIntelSymbol_codeIntelIndex_scope_fkey"
      FOREIGN KEY ("codeIntelIndexId", "orgId", "repoId") REFERENCES "CodeIntelIndex"(id, "orgId", "repoId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelOccurrence_codeIntelIndex_scope_fkey') THEN
    ALTER TABLE "CodeIntelOccurrence"
      ADD CONSTRAINT "CodeIntelOccurrence_codeIntelIndex_scope_fkey"
      FOREIGN KEY ("codeIntelIndexId", "orgId", "repoId") REFERENCES "CodeIntelIndex"(id, "orgId", "repoId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelRelationship_codeIntelIndex_scope_fkey') THEN
    ALTER TABLE "CodeIntelRelationship"
      ADD CONSTRAINT "CodeIntelRelationship_codeIntelIndex_scope_fkey"
      FOREIGN KEY ("codeIntelIndexId", "orgId", "repoId") REFERENCES "CodeIntelIndex"(id, "orgId", "repoId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;
