-- Slice S.7 of the codeintel ↔ legacy schema-parity recovery.
-- Adds Chat + ChatAccess byte-equal vs legacy.
--
--   Chat       — top-level chat session (private/public).
--   ChatAccess — per-user grant for non-owner viewers/editors
--                of a Chat. Composite unique on (chatId, userId).
--
-- Chat uses the `ChatVisibility` enum from S.1. Chat.createdById
-- is nullable because system chats / agent-created chats may
-- have no owning user; anonymousCreatorId carries an opaque id
-- for anonymously-created chats.

-- Chat -------------------------------------------------------

CREATE TABLE IF NOT EXISTS "Chat" (
    "id"                  TEXT                            NOT NULL,
    "name"                TEXT                            NULL,
    "createdById"         TEXT                            NULL,
    "createdAt"           TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"           TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "orgId"               INTEGER                         NOT NULL,
    "visibility"          "ChatVisibility"                NOT NULL DEFAULT 'PRIVATE',
    "messages"            JSONB                           NOT NULL,
    "anonymousCreatorId"  TEXT                            NULL,
    CONSTRAINT "Chat_pkey" PRIMARY KEY ("id")
);

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'Chat_createdById_fkey') THEN
    ALTER TABLE "Chat"
      ADD CONSTRAINT "Chat_createdById_fkey"
      FOREIGN KEY ("createdById") REFERENCES "User"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'Chat_orgId_fkey') THEN
    ALTER TABLE "Chat"
      ADD CONSTRAINT "Chat_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- ChatAccess -------------------------------------------------

CREATE TABLE IF NOT EXISTS "ChatAccess" (
    "id"        TEXT                            NOT NULL,
    "chatId"    TEXT                            NOT NULL,
    "userId"    TEXT                            NOT NULL,
    "createdAt" TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT "ChatAccess_pkey" PRIMARY KEY ("id")
);

CREATE UNIQUE INDEX IF NOT EXISTS "ChatAccess_chatId_userId_key"
    ON "ChatAccess" ("chatId", "userId");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ChatAccess_chatId_fkey') THEN
    ALTER TABLE "ChatAccess"
      ADD CONSTRAINT "ChatAccess_chatId_fkey"
      FOREIGN KEY ("chatId") REFERENCES "Chat"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ChatAccess_userId_fkey') THEN
    ALTER TABLE "ChatAccess"
      ADD CONSTRAINT "ChatAccess_userId_fkey"
      FOREIGN KEY ("userId") REFERENCES "User"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;
