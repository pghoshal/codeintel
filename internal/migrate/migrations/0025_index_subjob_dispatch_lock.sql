-- HPA-safe dispatcher lease for CodeIntelIndexSubjob sweeps.
--
-- The first implementation used session advisory locks. That is unsafe
-- behind pgxpool because acquire/release can land on different physical
-- connections. This lease row is ordinary table state, so any backend pod
-- can acquire it after expiry and release only the owner it holds.

CREATE TABLE IF NOT EXISTS "CodeIntelIndexSubjobDispatchLock" (
    "id"             TEXT                            NOT NULL,
    "leaseOwner"     TEXT                            NOT NULL,
    "leaseExpiresAt" TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL,
    "createdAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    "updatedAt"      TIMESTAMP(3) WITHOUT TIME ZONE  NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT "CodeIntelIndexSubjobDispatchLock_pkey" PRIMARY KEY ("id")
);
