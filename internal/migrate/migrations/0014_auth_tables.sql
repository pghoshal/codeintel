-- Slice S.4 of the codeintel ↔ legacy schema-parity recovery.
-- Adds the four auth-related tables byte-equal vs the legacy
-- reference schema:
--
--   Account            — NextAuth-style external-provider link
--                        (refresh/access tokens, scope, issuer).
--   AccountRequest     — pending account-onboarding requests
--                        per Org.
--   Invite             — pending org invitations (email + host).
--   VerificationToken  — NextAuth-style verification tokens
--                        (e.g., magic-link sign-in).
--
-- All idempotent. All FKs to User / Org with ON UPDATE CASCADE
-- ON DELETE CASCADE (matches legacy).
--
-- These tables are not yet load-bearing for any shipped feature
-- slice — they exist as part of the foundational schema parity
-- so future ports (Phase G auth + EE, Phase H permission sync)
-- can land FKs without an intermediate migration.

-- Account ----------------------------------------------------

CREATE TABLE IF NOT EXISTS "Account" (
    "id"                       TEXT                            NOT NULL,
    "userId"                   TEXT                            NOT NULL,
    "type"                     TEXT                            NOT NULL,
    "provider"                 TEXT                            NOT NULL,
    "providerAccountId"        TEXT                            NOT NULL,
    "refresh_token"            TEXT                            NULL,
    "access_token"             TEXT                            NULL,
    "expires_at"               INTEGER                         NULL,
    "token_type"               TEXT                            NULL,
    "scope"                    TEXT                            NULL,
    "id_token"                 TEXT                            NULL,
    "session_state"            TEXT                            NULL,
    "createdAt"                TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"                TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "permissionSyncedAt"       TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "issuerUrl"                TEXT                            NULL,
    "tokenRefreshErrorMessage" TEXT                            NULL,
    CONSTRAINT "Account_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX IF NOT EXISTS "Account_provider_providerAccountId_key"
    ON "Account" ("provider", "providerAccountId");

DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'Account_userId_fkey'
  ) THEN
    ALTER TABLE "Account"
      ADD CONSTRAINT "Account_userId_fkey"
      FOREIGN KEY ("userId") REFERENCES "User"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- AccountRequest ---------------------------------------------

CREATE TABLE IF NOT EXISTS "AccountRequest" (
    "id"            TEXT                            NOT NULL,
    "createdAt"     TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "requestedById" TEXT                            NOT NULL,
    "orgId"         INTEGER                         NOT NULL,
    CONSTRAINT "AccountRequest_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX IF NOT EXISTS "AccountRequest_requestedById_key"
    ON "AccountRequest" ("requestedById");

CREATE UNIQUE INDEX IF NOT EXISTS "AccountRequest_requestedById_orgId_key"
    ON "AccountRequest" ("requestedById", "orgId");

DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'AccountRequest_orgId_fkey'
  ) THEN
    ALTER TABLE "AccountRequest"
      ADD CONSTRAINT "AccountRequest_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'AccountRequest_requestedById_fkey'
  ) THEN
    ALTER TABLE "AccountRequest"
      ADD CONSTRAINT "AccountRequest_requestedById_fkey"
      FOREIGN KEY ("requestedById") REFERENCES "User"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- Invite -----------------------------------------------------

CREATE TABLE IF NOT EXISTS "Invite" (
    "id"             TEXT                            NOT NULL,
    "createdAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "recipientEmail" TEXT                            NOT NULL,
    "hostUserId"     TEXT                            NOT NULL,
    "orgId"          INTEGER                         NOT NULL,
    CONSTRAINT "Invite_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX IF NOT EXISTS "Invite_recipientEmail_orgId_key"
    ON "Invite" ("recipientEmail", "orgId");

DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'Invite_hostUserId_fkey'
  ) THEN
    ALTER TABLE "Invite"
      ADD CONSTRAINT "Invite_hostUserId_fkey"
      FOREIGN KEY ("hostUserId") REFERENCES "User"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'Invite_orgId_fkey'
  ) THEN
    ALTER TABLE "Invite"
      ADD CONSTRAINT "Invite_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- VerificationToken ------------------------------------------
-- Note: legacy has no primary key on this table — only the
-- unique (identifier, token) index. Mirror that exactly.

CREATE TABLE IF NOT EXISTS "VerificationToken" (
    "identifier" TEXT                            NOT NULL,
    "token"      TEXT                            NOT NULL,
    "expires"    TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS "VerificationToken_identifier_token_key"
    ON "VerificationToken" ("identifier", "token");
