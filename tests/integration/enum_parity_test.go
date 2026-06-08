//go:build integration

// Live-Postgres parity test for the 23 enum types the legacy
// Prisma schema defines (see
// docs/codeintel-schema-parity-audit.md slice S.1). Asserts
// every enum's name + ordered value list byte-matches the
// captured legacy ground truth.
//
// Ground truth was captured 2026-05-24 by running this query
// against the legacy reference postgres (database
// `legacy_schema`):
//
//	SELECT t.typname, array_agg(e.enumlabel ORDER BY e.enumsortorder)
//	FROM pg_type t JOIN pg_enum e ON e.enumtypid = t.oid
//	WHERE t.typtype = 'e' GROUP BY t.typname ORDER BY t.typname;
//
// The expected slice below is the verbatim transcription. A
// drift here means either: a legacy Prisma migration was missed
// in 0011_enum_types_parity.sql, or a legacy enum-value-add
// migration has landed since 2026-05-24 (in which case
// regenerate the ground truth + the migration).
package integration

import (
	"context"
	"reflect"
	"testing"
	"time"

	"codeintel/internal/db"
	"codeintel/internal/migrate"
)

type expectedEnum struct {
	name   string
	values []string
}

// legacyEnumGroundTruth captures every enum type the legacy
// Prisma schema defines. Order matters — enum ordinal positions
// are observable via PostgreSQL's enum-comparison operators, so
// a reorder is a semantic change.
var legacyEnumGroundTruth = []expectedEnum{
	{"AccountPermissionSyncJobStatus", []string{"PENDING", "IN_PROGRESS", "COMPLETED", "FAILED"}},
	{"ChatVisibility", []string{"PRIVATE", "PUBLIC"}},
	{"CodeGraphAnchorDirection", []string{"PROVIDES", "CONSUMES", "REFERENCES"}},
	{"CodeGraphFactConfidenceTier", []string{"EXTRACTED", "INFERRED", "AMBIGUOUS"}},
	{"CodeGraphIndexStatus", []string{"PENDING", "BUILDING", "READY", "PARTIAL", "SKIPPED", "FAILED", "DELETING"}},
	{"CodeGraphProvider", []string{"NEBULA"}},
	{"CodeHostType", []string{"github", "gitlab", "gitea", "gerrit", "bitbucket-server", "bitbucket-cloud", "generic-git-host", "azuredevops"}},
	{"CodeIntelIndexKind", []string{"SCIP"}},
	{"CodeIntelIndexStatus", []string{"PENDING", "INDEXING", "READY", "PARTIAL", "SKIPPED", "FAILED"}},
	{"CodeIntelOccurrenceRole", []string{"DEFINITION", "REFERENCE", "IMPORT", "READ", "WRITE", "GENERATED", "TEST", "FORWARD_DEFINITION"}},
	{"CodeIntelQueryMode", []string{"HYBRID", "ZOEKT_ONLY", "SCIP_ONLY"}},
	{"ConnectionSyncJobStatus", []string{"PENDING", "IN_PROGRESS", "COMPLETED", "FAILED"}},
	{"ConnectionSyncStatus", []string{"SYNC_NEEDED", "IN_SYNC_QUEUE", "SYNCING", "SYNCED", "FAILED", "SYNCED_WITH_WARNINGS"}},
	{"ConnectionType", []string{"github", "gitlab", "gitea", "gerrit", "bitbucket", "azuredevops", "git"}},
	{"OrgRole", []string{"OWNER", "MEMBER", "GUEST"}},
	{"PermissionSyncSource", []string{"ACCOUNT_DRIVEN", "REPO_DRIVEN"}},
	{"RepoIndexManifestStatus", []string{"PENDING", "READY", "FAILED", "SUPERSEDED"}},
	{"RepoIndexingJobStatus", []string{"PENDING", "IN_PROGRESS", "COMPLETED", "FAILED"}},
	{"RepoIndexingJobType", []string{"INDEX", "CLEANUP", "REMOVE_INDEX"}},
	{"RepoPermissionSyncJobStatus", []string{"PENDING", "IN_PROGRESS", "COMPLETED", "FAILED"}},
	{"StripeSubscriptionStatus", []string{"ACTIVE", "INACTIVE"}},
	{"ZoektOrgIndexStatus", []string{"PENDING", "INDEXING", "READY", "DEGRADED", "FAILED", "DISABLED"}},
	{"ZoektOrgReplicaStatus", []string{"UNKNOWN", "READY", "UNREACHABLE", "DISABLED"}},
}

// TestMigrate_EnumTypesParity asserts every legacy enum type
// exists in codeintel with the exact same ordered value list.
// Slice S.1 of the schema-parity recovery.
func TestMigrate_EnumTypesParity(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                    dsn,
		AllowInsecureRemoteDSN: true,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	for _, want := range legacyEnumGroundTruth {
		t.Run(want.name, func(t *testing.T) {
			rows, err := pool.Query(ctx, `
				SELECT e.enumlabel
				FROM pg_type t
				JOIN pg_enum e ON e.enumtypid = t.oid
				WHERE t.typname = $1
				ORDER BY e.enumsortorder
			`, want.name)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var label string
				if err := rows.Scan(&label); err != nil {
					t.Fatalf("scan: %v", err)
				}
				got = append(got, label)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("rows.Err: %v", err)
			}
			if len(got) == 0 {
				t.Fatalf("enum %q has zero values — was the type created at all?", want.name)
			}
			if !reflect.DeepEqual(got, want.values) {
				t.Errorf("enum %q values:\n got:  %v\nwant: %v", want.name, got, want.values)
			}
		})
	}
}

// TestMigrate_EnumCount locks the count: exactly 23 enum types
// exist after applying migrations through 0011. A drift here
// catches either an accidental extra enum (e.g., one created
// for a codeintel-only feature) or a missing enum (e.g., a
// legacy migration added one that 0011 didn't capture).
func TestMigrate_EnumCount(t *testing.T) {
	dsn := requireDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.NewPool(ctx, db.Config{
		DSN:                    dsn,
		AllowInsecureRemoteDSN: true,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	if _, err := migrate.Apply(ctx, pool); err != nil {
		t.Fatalf("migrate.Apply: %v", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM pg_type
		WHERE typtype = 'e' AND typnamespace = 'public'::regnamespace
	`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	want := len(legacyEnumGroundTruth)
	if count != want {
		t.Errorf("enum-type count: got %d, want %d", count, want)
	}
}
