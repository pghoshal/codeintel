-- Durable hybrid executor subjobs.
--
-- One RepoIndexingJob remains the user-visible index/reindex/remove-index
-- lifecycle. This table breaks an INDEX run into retry-safe,
-- worker-class-specific units such as clone, zoekt, ast, scip-go,
-- scip-jvm, graph-merge, and activation. Pods are disposable; this
-- table is the durable source of truth.

CREATE TABLE IF NOT EXISTS "CodeIntelIndexSubjob" (
    "id"                  TEXT                            NOT NULL,
    "repoIndexingJobId"   TEXT                            NOT NULL,
    "codeIntelIndexId"    TEXT                            NULL,
    "orgId"               INTEGER                         NOT NULL,
    "workspaceId"         TEXT                            NOT NULL,
    "repoId"              INTEGER                         NOT NULL,
    "branch"              TEXT                            NOT NULL,
    "revision"            TEXT                            NOT NULL,
    "commitHash"          TEXT                            NOT NULL,
    "layer"               TEXT                            NOT NULL,
    "language"            TEXT                            NULL,
    "projectRoot"         TEXT                            NULL,
    "indexer"             TEXT                            NULL,
    "workerClass"         TEXT                            NOT NULL,
    "queueName"           TEXT                            NOT NULL,
    "status"              TEXT                            NOT NULL DEFAULT 'QUEUED',
    "attempt"             INTEGER                         NOT NULL DEFAULT 0,
    "maxAttempts"         INTEGER                         NOT NULL DEFAULT 3,
    "attemptId"           TEXT                            NULL,
    "leaseOwner"          TEXT                            NULL,
    "leaseExpiresAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "heartbeatAt"         TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "startedAt"           TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "completedAt"         TIMESTAMP(3) WITHOUT TIME ZONE  NULL,
    "artifactTempPath"    TEXT                            NULL,
    "artifactPath"        TEXT                            NULL,
    "artifactSha256"      TEXT                            NULL,
    "inputDigest"         TEXT                            NULL,
    "toolchainDigest"     TEXT                            NULL,
    "imageDigest"         TEXT                            NULL,
    "errorCode"           TEXT                            NULL,
    "errorMessage"        TEXT                            NULL,
    "payload"             JSONB                           NOT NULL DEFAULT '{}'::jsonb,
    "createdAt"           TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"           TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT "CodeIntelIndexSubjob_pkey" PRIMARY KEY ("id"),
    CONSTRAINT "CodeIntelIndexSubjob_status_check" CHECK (
      "status" IN (
        'QUEUED',
        'CLAIMED',
        'RUNNING',
        'ARTIFACT_WRITTEN',
        'VALIDATING',
        'SUCCEEDED',
        'FAILED',
        'RETRYING',
        'CANCELED',
        'SKIPPED'
      )
    ),
    CONSTRAINT "CodeIntelIndexSubjob_layer_check" CHECK (
      "layer" IN (
        'CLONE',
        'ZOEKT',
        'AST_TREE_SITTER',
        'SCIP',
        'GRAPH_MERGE',
        'ACTIVATE',
        'REMOVE'
      )
    )
);

CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelIndexSubjob_scope_key"
    ON "CodeIntelIndexSubjob" (
      "repoIndexingJobId",
      COALESCE("workspaceId", ''),
      "branch",
      "revision",
      "commitHash",
      "layer",
      "workerClass",
      COALESCE("language", ''),
      COALESCE("projectRoot", ''),
      COALESCE("indexer", '')
    );

CREATE INDEX IF NOT EXISTS "CodeIntelIndexSubjob_org_repo_status_idx"
    ON "CodeIntelIndexSubjob" ("orgId", "repoId", "status", "updatedAt" DESC);

CREATE INDEX IF NOT EXISTS "CodeIntelIndexSubjob_worker_status_lease_idx"
    ON "CodeIntelIndexSubjob" ("workerClass", "status", "leaseExpiresAt");

CREATE INDEX IF NOT EXISTS "CodeIntelIndexSubjob_queue_status_idx"
    ON "CodeIntelIndexSubjob" ("queueName", "status", "createdAt");

CREATE INDEX IF NOT EXISTS "CodeIntelIndexSubjob_status_createdAt_idx"
    ON "CodeIntelIndexSubjob" ("status", "createdAt", "id");

CREATE INDEX IF NOT EXISTS "CodeIntelIndexSubjob_status_lease_idx"
    ON "CodeIntelIndexSubjob" ("status", "leaseExpiresAt")
    WHERE "leaseExpiresAt" IS NOT NULL;

CREATE INDEX IF NOT EXISTS "CodeIntelIndexSubjob_repoIndexingJobId_idx"
    ON "CodeIntelIndexSubjob" ("repoIndexingJobId");

CREATE UNIQUE INDEX IF NOT EXISTS "RepoIndexingJob_id_repoId_key"
    ON "RepoIndexingJob" ("id", "repoId");

CREATE UNIQUE INDEX IF NOT EXISTS "CodeIntelIndex_id_orgId_repoId_revision_commitHash_key"
    ON "CodeIntelIndex" ("id", "orgId", "repoId", "revision", "commitHash");

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelIndexSubjob_repoIndexingJobId_fkey') THEN
    ALTER TABLE "CodeIntelIndexSubjob"
      ADD CONSTRAINT "CodeIntelIndexSubjob_repoIndexingJobId_fkey"
      FOREIGN KEY ("repoIndexingJobId") REFERENCES "RepoIndexingJob"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelIndexSubjob_job_repo_fkey') THEN
    ALTER TABLE "CodeIntelIndexSubjob"
      ADD CONSTRAINT "CodeIntelIndexSubjob_job_repo_fkey"
      FOREIGN KEY ("repoIndexingJobId", "repoId") REFERENCES "RepoIndexingJob"("id", "repoId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelIndexSubjob_codeIntelIndexId_fkey') THEN
    ALTER TABLE "CodeIntelIndexSubjob"
      ADD CONSTRAINT "CodeIntelIndexSubjob_codeIntelIndexId_fkey"
      FOREIGN KEY ("codeIntelIndexId") REFERENCES "CodeIntelIndex"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelIndexSubjob_codeIntelIndex_scope_fkey') THEN
    ALTER TABLE "CodeIntelIndexSubjob"
      ADD CONSTRAINT "CodeIntelIndexSubjob_codeIntelIndex_scope_fkey"
      FOREIGN KEY ("codeIntelIndexId", "orgId", "repoId", "revision", "commitHash")
      REFERENCES "CodeIntelIndex"("id", "orgId", "repoId", "revision", "commitHash")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelIndexSubjob_orgId_fkey') THEN
    ALTER TABLE "CodeIntelIndexSubjob"
      ADD CONSTRAINT "CodeIntelIndexSubjob_orgId_fkey"
      FOREIGN KEY ("orgId") REFERENCES "Org"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelIndexSubjob_repoId_fkey') THEN
    ALTER TABLE "CodeIntelIndexSubjob"
      ADD CONSTRAINT "CodeIntelIndexSubjob_repoId_fkey"
      FOREIGN KEY ("repoId") REFERENCES "Repo"("id")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'CodeIntelIndexSubjob_repo_org_fkey') THEN
    ALTER TABLE "CodeIntelIndexSubjob"
      ADD CONSTRAINT "CodeIntelIndexSubjob_repo_org_fkey"
      FOREIGN KEY ("repoId", "orgId") REFERENCES "Repo"("id", "orgId")
      ON UPDATE CASCADE ON DELETE CASCADE;
  END IF;
END $$;
