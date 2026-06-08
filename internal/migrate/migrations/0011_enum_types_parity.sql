-- Slice S.1 of the codeintel ↔ legacy schema-parity recovery
-- (see docs/codeintel-schema-parity-audit.md). Creates all 23
-- enum types the legacy Prisma schema defines so subsequent
-- migrations can declare columns of these types instead of
-- using `TEXT` placeholders.
--
-- Each `CREATE TYPE` is wrapped in a DO block with a
-- `pg_type` existence guard so the migration is idempotent
-- (re-runs after a partial apply produce no error). Enum
-- value lists are byte-for-byte copies of the legacy Prisma
-- enums as captured from `pg_enum` on the legacy reference
-- postgres (database `legacy_schema`).
--
-- Source-of-truth enum value lists are captured 2026-05-24
-- against legacy Prisma migrations 20250122225856_postgres_init
-- through 20260522020000_add_code_graph_semantic_facts.

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'AccountPermissionSyncJobStatus') THEN
    CREATE TYPE "AccountPermissionSyncJobStatus" AS ENUM ('PENDING', 'IN_PROGRESS', 'COMPLETED', 'FAILED');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'ChatVisibility') THEN
    CREATE TYPE "ChatVisibility" AS ENUM ('PRIVATE', 'PUBLIC');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeGraphAnchorDirection') THEN
    CREATE TYPE "CodeGraphAnchorDirection" AS ENUM ('PROVIDES', 'CONSUMES', 'REFERENCES');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeGraphFactConfidenceTier') THEN
    CREATE TYPE "CodeGraphFactConfidenceTier" AS ENUM ('EXTRACTED', 'INFERRED', 'AMBIGUOUS');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeGraphIndexStatus') THEN
    CREATE TYPE "CodeGraphIndexStatus" AS ENUM ('PENDING', 'BUILDING', 'READY', 'PARTIAL', 'SKIPPED', 'FAILED', 'DELETING');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeGraphProvider') THEN
    CREATE TYPE "CodeGraphProvider" AS ENUM ('NEBULA');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeHostType') THEN
    CREATE TYPE "CodeHostType" AS ENUM ('github', 'gitlab', 'gitea', 'gerrit', 'bitbucket-server', 'bitbucket-cloud', 'generic-git-host', 'azuredevops');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeIntelIndexKind') THEN
    CREATE TYPE "CodeIntelIndexKind" AS ENUM ('SCIP');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeIntelIndexStatus') THEN
    CREATE TYPE "CodeIntelIndexStatus" AS ENUM ('PENDING', 'INDEXING', 'READY', 'PARTIAL', 'SKIPPED', 'FAILED');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeIntelOccurrenceRole') THEN
    CREATE TYPE "CodeIntelOccurrenceRole" AS ENUM ('DEFINITION', 'REFERENCE', 'IMPORT', 'READ', 'WRITE', 'GENERATED', 'TEST', 'FORWARD_DEFINITION');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'CodeIntelQueryMode') THEN
    CREATE TYPE "CodeIntelQueryMode" AS ENUM ('HYBRID', 'ZOEKT_ONLY', 'SCIP_ONLY');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'ConnectionSyncJobStatus') THEN
    CREATE TYPE "ConnectionSyncJobStatus" AS ENUM ('PENDING', 'IN_PROGRESS', 'COMPLETED', 'FAILED');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'ConnectionSyncStatus') THEN
    CREATE TYPE "ConnectionSyncStatus" AS ENUM ('SYNC_NEEDED', 'IN_SYNC_QUEUE', 'SYNCING', 'SYNCED', 'FAILED', 'SYNCED_WITH_WARNINGS');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'ConnectionType') THEN
    CREATE TYPE "ConnectionType" AS ENUM ('github', 'gitlab', 'gitea', 'gerrit', 'bitbucket', 'azuredevops', 'git');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'OrgRole') THEN
    CREATE TYPE "OrgRole" AS ENUM ('OWNER', 'MEMBER', 'GUEST');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'PermissionSyncSource') THEN
    CREATE TYPE "PermissionSyncSource" AS ENUM ('ACCOUNT_DRIVEN', 'REPO_DRIVEN');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'RepoIndexManifestStatus') THEN
    CREATE TYPE "RepoIndexManifestStatus" AS ENUM ('PENDING', 'READY', 'FAILED', 'SUPERSEDED');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'RepoIndexingJobStatus') THEN
    CREATE TYPE "RepoIndexingJobStatus" AS ENUM ('PENDING', 'IN_PROGRESS', 'COMPLETED', 'FAILED');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'RepoIndexingJobType') THEN
    CREATE TYPE "RepoIndexingJobType" AS ENUM ('INDEX', 'CLEANUP', 'REMOVE_INDEX');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'RepoPermissionSyncJobStatus') THEN
    CREATE TYPE "RepoPermissionSyncJobStatus" AS ENUM ('PENDING', 'IN_PROGRESS', 'COMPLETED', 'FAILED');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'StripeSubscriptionStatus') THEN
    CREATE TYPE "StripeSubscriptionStatus" AS ENUM ('ACTIVE', 'INACTIVE');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'ZoektOrgIndexStatus') THEN
    CREATE TYPE "ZoektOrgIndexStatus" AS ENUM ('PENDING', 'INDEXING', 'READY', 'DEGRADED', 'FAILED', 'DISABLED');
  END IF;
END $$;

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_type WHERE typname = 'ZoektOrgReplicaStatus') THEN
    CREATE TYPE "ZoektOrgReplicaStatus" AS ENUM ('UNKNOWN', 'READY', 'UNREACHABLE', 'DISABLED');
  END IF;
END $$;
