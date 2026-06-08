-- Initial schema covering every table the codeintel service reads
-- or writes today. Column names use the quoted-camelCase convention
-- consistently across the schema.

CREATE TABLE IF NOT EXISTS "Org" (
    id                SERIAL PRIMARY KEY,
    name              TEXT NOT NULL,
    domain            TEXT NOT NULL UNIQUE,
    "atomWorkspaceId" TEXT UNIQUE,
    "atomWorkspaceName" TEXT,
    metadata          JSONB,
    "createdAt"       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS "User" (
    id              TEXT PRIMARY KEY,
    email           TEXT UNIQUE,
    name            TEXT,
    "createdAt"     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS "UserToOrg" (
    "orgId"   INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    "userId"  TEXT NOT NULL REFERENCES "User"(id) ON DELETE CASCADE,
    role      TEXT NOT NULL DEFAULT 'MEMBER',
    "joinedAt" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ("orgId", "userId")
);

CREATE TABLE IF NOT EXISTS "ApiKey" (
    hash          TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    "orgId"       INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    "createdById" TEXT NOT NULL REFERENCES "User"(id) ON DELETE CASCADE,
    "createdAt"   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "lastUsedAt"  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS "ApiKey_orgId_idx" ON "ApiKey" ("orgId");

CREATE TABLE IF NOT EXISTS "OrgSecret" (
    id               SERIAL PRIMARY KEY,
    "orgId"          INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    key              TEXT NOT NULL,
    "encryptedValue" TEXT NOT NULL,
    iv               TEXT NOT NULL,
    "createdAt"      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE ("orgId", key)
);

CREATE TABLE IF NOT EXISTS "OrgLanguageModel" (
    id          SERIAL PRIMARY KEY,
    "orgId"     INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    config      JSONB NOT NULL,
    "order"     INTEGER NOT NULL DEFAULT 0,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    "createdAt" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE ("orgId", name)
);

CREATE TABLE IF NOT EXISTS "Connection" (
    id                                 SERIAL PRIMARY KEY,
    "orgId"                            INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    name                               TEXT NOT NULL,
    config                             JSONB NOT NULL,
    "connectionType"                   TEXT NOT NULL,
    "isDeclarative"                    BOOLEAN NOT NULL DEFAULT FALSE,
    "enforcePermissions"               BOOLEAN NOT NULL DEFAULT TRUE,
    "enforcePermissionsForPublicRepos" BOOLEAN NOT NULL DEFAULT FALSE,
    "syncedAt"                         TIMESTAMPTZ,
    "createdAt"                        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"                        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (name, "orgId")
);

CREATE TABLE IF NOT EXISTS "Repo" (
    id              SERIAL PRIMARY KEY,
    "orgId"         INTEGER NOT NULL REFERENCES "Org"(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    "displayName"   TEXT,
    "indexedAt"     TIMESTAMPTZ,
    "createdAt"     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "updatedAt"     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS "Repo_orgId_idx" ON "Repo" ("orgId");

CREATE TABLE IF NOT EXISTS "RepoToConnection" (
    "repoId"       INTEGER NOT NULL REFERENCES "Repo"(id) ON DELETE CASCADE,
    "connectionId" INTEGER NOT NULL REFERENCES "Connection"(id) ON DELETE CASCADE,
    "addedAt"      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY ("repoId", "connectionId")
);
CREATE INDEX IF NOT EXISTS "RepoToConnection_connectionId_idx" ON "RepoToConnection" ("connectionId");
