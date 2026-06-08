-- Final schema-parity slice. Replaces the codeintel-divergent
-- "AuditEvent" table with the legacy-shaped "Audit" table.
--
-- Shape divergences being reconciled:
--
--   AuditEvent.id          BIGSERIAL          → Audit.id              TEXT (producer-supplied cuid/uuid)
--   AuditEvent.eventTime   TIMESTAMPTZ NOT NULL → Audit.timestamp     TIMESTAMP(3) WITHOUT TIME ZONE
--                                                                     NOT NULL DEFAULT CURRENT_TIMESTAMP
--   AuditEvent.insertedAt  TIMESTAMPTZ        → (removed, not in legacy)
--   AuditEvent.requestId   TEXT NOT NULL ''   → (removed; producer merges into metadata)
--   (none)                                    → Audit.codeintelVersion TEXT NOT NULL
--                                              (the legacy column under
--                                               this slot carried a
--                                               brand-prefixed name; the
--                                               port renames it to the
--                                               codeintel-prefixed form)
--
-- The last in-DB brand reference is closed by this rename. The
-- brand-sweep test in codeintel/tests/lint/ will now hold
-- against the entire schema.
--
-- Producer-API update is in pkg/audit (server.go): the
-- INSERT statement, the id-generation path, and the metadata
-- merge of RequestId all land in the same slice as this
-- migration so the binary is never in a half-renamed state.

-- Drop the divergent AuditEvent + its supporting sequence.
DROP TABLE IF EXISTS "AuditEvent" CASCADE;

-- Create legacy-shaped Audit.
CREATE TABLE IF NOT EXISTS "Audit" (
    "id"               TEXT                            NOT NULL,
    "timestamp"        TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "action"           TEXT                            NOT NULL,
    "actorId"          TEXT                            NOT NULL,
    "actorType"        TEXT                            NOT NULL,
    "targetId"         TEXT                            NOT NULL,
    "targetType"       TEXT                            NOT NULL,
    "codeintelVersion" TEXT                            NOT NULL,
    "metadata"         JSONB                           NULL,
    "orgId"            INTEGER                         NOT NULL,
    CONSTRAINT "Audit_pkey" PRIMARY KEY ("id")
);

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'Audit_orgId_fkey') THEN
    ALTER TABLE "Audit"
      ADD CONSTRAINT "Audit_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;

-- Three legacy indexes:
CREATE INDEX IF NOT EXISTS "Audit_actorId_actorType_targetId_targetType_orgId_idx"
    ON "Audit" ("actorId", "actorType", "targetId", "targetType", "orgId");

CREATE INDEX IF NOT EXISTS "idx_audit_actor_time_full"
    ON "Audit" ("actorId", "timestamp");

CREATE INDEX IF NOT EXISTS "idx_audit_core_actions_full"
    ON "Audit" ("orgId", "timestamp", "action", "actorId");
