-- Slice S.6 of the codeintel ↔ legacy schema-parity recovery.
-- Adds the four OAuth tables byte-equal vs legacy:
--
--   OAuthClient            — registered OAuth client app.
--   OAuthToken             — issued access token (hash-keyed).
--   OAuthRefreshToken      — issued refresh token (hash-keyed).
--   OAuthAuthorizationCode — pending authorization code with PKCE.
--
-- All hash-keyed tables use the hash as the PK (not a separate
-- id) — matches legacy. All token / code tables FK to
-- OAuthClient + User, both cascade-cascade.

-- OAuthClient ------------------------------------------------

CREATE TABLE IF NOT EXISTS "OAuthClient" (
    "id"           TEXT                            NOT NULL,
    "name"         TEXT                            NOT NULL,
    "redirectUris" TEXT[]                          NULL,
    "createdAt"    TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "logoUri"      TEXT                            NULL,
    CONSTRAINT "OAuthClient_pkey" PRIMARY KEY ("id")
);

-- OAuthToken -------------------------------------------------

CREATE TABLE IF NOT EXISTS "OAuthToken" (
    "hash"       TEXT                            NOT NULL,
    "clientId"   TEXT                            NOT NULL,
    "userId"     TEXT                            NOT NULL,
    "scope"      TEXT                            NOT NULL DEFAULT '',
    "expiresAt"  TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "createdAt"  TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "lastUsedAt" TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "resource"   TEXT                            NULL,
    CONSTRAINT "OAuthToken_pkey" PRIMARY KEY ("hash")
);

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'OAuthToken_clientId_fkey') THEN
    ALTER TABLE "OAuthToken"
      ADD CONSTRAINT "OAuthToken_clientId_fkey"
      FOREIGN KEY ("clientId") REFERENCES "OAuthClient"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'OAuthToken_userId_fkey') THEN
    ALTER TABLE "OAuthToken"
      ADD CONSTRAINT "OAuthToken_userId_fkey"
      FOREIGN KEY ("userId") REFERENCES "User"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- OAuthRefreshToken ------------------------------------------

CREATE TABLE IF NOT EXISTS "OAuthRefreshToken" (
    "hash"      TEXT                            NOT NULL,
    "clientId"  TEXT                            NOT NULL,
    "userId"    TEXT                            NOT NULL,
    "scope"     TEXT                            NOT NULL DEFAULT '',
    "resource"  TEXT                            NULL,
    "expiresAt" TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "createdAt" TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT "OAuthRefreshToken_pkey" PRIMARY KEY ("hash")
);

CREATE INDEX IF NOT EXISTS "OAuthRefreshToken_clientId_userId_idx"
    ON "OAuthRefreshToken" ("clientId", "userId");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'OAuthRefreshToken_clientId_fkey') THEN
    ALTER TABLE "OAuthRefreshToken"
      ADD CONSTRAINT "OAuthRefreshToken_clientId_fkey"
      FOREIGN KEY ("clientId") REFERENCES "OAuthClient"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'OAuthRefreshToken_userId_fkey') THEN
    ALTER TABLE "OAuthRefreshToken"
      ADD CONSTRAINT "OAuthRefreshToken_userId_fkey"
      FOREIGN KEY ("userId") REFERENCES "User"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- OAuthAuthorizationCode -------------------------------------

CREATE TABLE IF NOT EXISTS "OAuthAuthorizationCode" (
    "codeHash"      TEXT                            NOT NULL,
    "clientId"      TEXT                            NOT NULL,
    "userId"        TEXT                            NOT NULL,
    "redirectUri"   TEXT                            NOT NULL,
    "codeChallenge" TEXT                            NOT NULL,
    "expiresAt"     TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "createdAt"     TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "resource"      TEXT                            NULL,
    CONSTRAINT "OAuthAuthorizationCode_pkey" PRIMARY KEY ("codeHash")
);

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'OAuthAuthorizationCode_clientId_fkey') THEN
    ALTER TABLE "OAuthAuthorizationCode"
      ADD CONSTRAINT "OAuthAuthorizationCode_clientId_fkey"
      FOREIGN KEY ("clientId") REFERENCES "OAuthClient"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'OAuthAuthorizationCode_userId_fkey') THEN
    ALTER TABLE "OAuthAuthorizationCode"
      ADD CONSTRAINT "OAuthAuthorizationCode_userId_fkey"
      FOREIGN KEY ("userId") REFERENCES "User"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;
