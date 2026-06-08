-- CodeGraphIndex captures one snapshot of the graph builder's
-- output for a (repoId, workspaceId, commitHash, provider,
-- schemaVersion, builderVersion) tuple. Each row records the
-- count of materialised vertices / edges / anchors / linked
-- edges in the backing graph store (NebulaGraph today; provider
-- column lets future graph engines plug in without a column
-- rename).
--
-- The per-Repo LATERAL on /api/repos picks the most-recently-
-- updated row; the disambiguation logic that selects the "current"
-- index across overlapping revisions (when more than one workspaceId
-- is active) lands in a follow-up slice alongside the
-- CodeGraphRevision sub-table.
CREATE TABLE IF NOT EXISTS "CodeGraphIndex" (
    id                TEXT PRIMARY KEY,
    provider          TEXT NOT NULL DEFAULT 'NEBULA',
    status            TEXT NOT NULL DEFAULT 'PENDING',
    "sourceRevision"  TEXT,
    "commitHash"      TEXT NOT NULL,
    "graphSpace"      TEXT,
    "workspaceId"     TEXT NOT NULL,
    "schemaVersion"   INTEGER NOT NULL DEFAULT 1,
    "builderVersion"  TEXT NOT NULL,
    "indexRunId"      TEXT,
    "vertexCount"     INTEGER NOT NULL DEFAULT 0,
    "edgeCount"       INTEGER NOT NULL DEFAULT 0,
    "anchorCount"     INTEGER NOT NULL DEFAULT 0,
    "linkedEdgeCount" INTEGER NOT NULL DEFAULT 0,
    "errorMessage"    TEXT,
    "indexedAt"       TIMESTAMPTZ,
    "supersededAt"    TIMESTAMPTZ,
    "deleteAfter"     TIMESTAMPTZ,
    "createdAt"       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "orgId"           INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    "repoId"          INTEGER NOT NULL REFERENCES "Repo"(id) ON DELETE CASCADE
);

-- One row per (repo, workspace, commit, provider, schemaVersion,
-- builderVersion) tuple — re-running the builder collapses onto
-- the existing row rather than appending a history per build.
CREATE UNIQUE INDEX IF NOT EXISTS "CodeGraphIndex_repoId_workspaceId_commitHash_provider_schema_builder_key"
    ON "CodeGraphIndex" ("repoId", "workspaceId", "commitHash", provider, "schemaVersion", "builderVersion");

-- Per-repo most-recent lookup: the LATERAL on /api/repos does
-- WHERE "repoId" = r.id ORDER BY "updatedAt" DESC LIMIT 1.
CREATE INDEX IF NOT EXISTS "CodeGraphIndex_repoId_updatedAt_idx"
    ON "CodeGraphIndex" ("repoId", "updatedAt" DESC);

-- Org-wide rollup queries (status-grouped failure counts).
CREATE INDEX IF NOT EXISTS "CodeGraphIndex_orgId_status_idx"
    ON "CodeGraphIndex" ("orgId", status);
