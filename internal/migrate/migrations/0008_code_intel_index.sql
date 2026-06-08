-- CodeIntelIndex captures one SCIP / future-LSIF indexing run for a
-- (repoId, revision, commitHash, kind) tuple. The denormalised
-- counts (languageCount, symbolCount, occurrenceCount,
-- relationshipCount) let the /api/repos listing emit a
-- per-repo summary without walking the per-language /
-- per-symbol child tables on every read.
--
-- kind / status are TEXT-typed enums; the wire contract uses the
-- enum-name strings verbatim ("SCIP", "READY", "FAILED", etc.) so
-- TEXT keeps the migration extensible without per-enum DDL when
-- new indexer kinds land.
CREATE TABLE IF NOT EXISTS "CodeIntelIndex" (
    id                  TEXT PRIMARY KEY,
    kind                TEXT NOT NULL DEFAULT 'SCIP',
    status              TEXT NOT NULL DEFAULT 'PENDING',
    revision            TEXT NOT NULL,
    "commitHash"        TEXT NOT NULL,
    "artifactRoot"      TEXT,
    "languageCount"     INTEGER NOT NULL DEFAULT 0,
    "symbolCount"       INTEGER NOT NULL DEFAULT 0,
    "occurrenceCount"   INTEGER NOT NULL DEFAULT 0,
    "relationshipCount" INTEGER NOT NULL DEFAULT 0,
    "errorMessage"      TEXT,
    "indexedAt"         TIMESTAMPTZ,
    "createdAt"         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "orgId"             INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    "repoId"            INTEGER NOT NULL REFERENCES "Repo"(id) ON DELETE CASCADE
);

-- One index row per (repo, revision, commitHash, kind) tuple.
-- The writer collapses a re-index of the same commit onto the
-- existing row rather than appending a history per commit; the
-- per-repo LATERAL on /api/repos picks the most-recently-updated
-- row anyway, so this is the natural natural key.
CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelIndex_repoId_revision_commitHash_kind_key"
    ON "CodeIntelIndex" ("repoId", revision, "commitHash", kind);

-- Per-repo most-recent lookup: the LATERAL on /api/repos does
-- WHERE "repoId" = r.id ORDER BY "updatedAt" DESC LIMIT 1. The
-- index keeps that at O(log n) regardless of how many index
-- generations have accumulated for a single repo.
CREATE INDEX IF NOT EXISTS "CodeIntelIndex_repoId_updatedAt_idx"
    ON "CodeIntelIndex" ("repoId", "updatedAt" DESC);

-- Org-wide rollup queries (e.g. "how many CodeIntelIndex rows
-- failed for org X") use this index.
CREATE INDEX IF NOT EXISTS "CodeIntelIndex_orgId_status_idx"
    ON "CodeIntelIndex" ("orgId", status);
