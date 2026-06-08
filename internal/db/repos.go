// Repo-list queries that back GET /api/repos.
//
// The "active" repo set is gated through RepoToConnection: a Repo
// row only counts as active when at least one Connection still
// references it. This matches the wire contract — connectionless
// repos are stale records the listing must hide.
//
// The Repo table in this codebase currently carries the core
// identity columns (id, orgId, name, displayName, indexedAt) — the
// extended projection columns (codeHostType, webUrl, externalWebUrl,
// imageUrl, pushedAt, defaultBranch, isFork, isArchived) and the
// nested codeIntelIndexes / codeGraphIndexes / jobs blocks belong
// to follow-up slices. ListOrgRepos / CountOrgRepos return only the
// columns the table actually has so the response shape stays a
// correct strict subset of the eventual full projection.
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// RepoListRow is the per-row projection the listing handler echoes.
// Nullable columns (DisplayName / IndexedAt / WebUrl / ImageUrl /
// PushedAt / DefaultBranch / CodeHostType) decode as *T so the
// handler can distinguish "absent" from "zero value" and route
// optional wire fields through omitempty. IsFork / IsArchived are
// non-null (DEFAULT FALSE in the migration) so plain bool is safe.
//
// LatestJob is nil when no RepoIndexingJob rows exist for the
// repo (the LEFT JOIN LATERAL emits NULLs for every job column).
// The scan path detects "no job" by checking the job id column:
// since the cuid primary key is non-null on a real row, a nil
// pointer scan target surfaces only on the LATERAL no-match
// branch and is the unambiguous sentinel for "no job exists".
type RepoListRow struct {
	RepoID                  int32
	RepoName                string
	RepoDisplayName         *string
	IndexedAt               *time.Time
	Metadata                json.RawMessage
	LatestIndexingJobStatus *string
	CodeHostType            *string
	WebUrl                  *string
	ImageUrl                *string
	PushedAt                *time.Time
	DefaultBranch           *string
	IsFork                  bool
	IsArchived              bool
	LatestJob               *RepoListJobRow
	LatestScip              *RepoListScipRow
	LatestCodeGraph         *RepoListCodeGraphRow
}

// RepoListJobRow is the per-repo latest-job sub-projection. Carries
// the six fields the wire `latestJob` object emits: id, type,
// status, createdAt, completedAt, errorMessage. completedAt and
// errorMessage are nullable on the underlying RepoIndexingJob
// table; the other four are non-null.
type RepoListJobRow struct {
	ID           string
	Type         string
	Status       string
	CreatedAt    time.Time
	CompletedAt  *time.Time
	ErrorMessage *string
}

// RepoListScipRow is the per-repo latest-CodeIntelIndex sub-
// projection. Carries the eleven scalar fields the wire `codeIntel.scip`
// object emits in this slice — the languageIndexes array + the
// derived projectCount / detectedLanguages / readyLanguages /
// skippedLanguages / failedLanguages fields land once the
// CodeIntelLanguageIndex table ships.
//
// IndexedAt and ErrorMessage are nullable on the underlying
// CodeIntelIndex row; LanguageCount / SymbolCount / OccurrenceCount /
// RelationshipCount are NOT NULL DEFAULT 0 so plain int32 is correct.
type RepoListScipRow struct {
	ID                string
	Kind              string
	Status            string
	Revision          string
	CommitHash        string
	LanguageCount     int32
	SymbolCount       int32
	OccurrenceCount   int32
	RelationshipCount int32
	IndexedAt         *time.Time
	ErrorMessage      *string
}

// RepoListCodeGraphRow is the per-repo latest-CodeGraphIndex sub-
// projection. Carries the 17 scalar columns the wire's
// `codeIntel.codeGraph` object emits. The API layer derives a
// compact active-revision projection from the latest READY graph
// snapshot selected here; `/api/repos/{id}/status` expands the full
// CodeGraphRevision history for detailed health views.
//
// SourceRevision / GraphSpace / IndexedAt / SupersededAt /
// DeleteAfter / ErrorMessage are nullable on the underlying
// CodeGraphIndex row. The four counts and SchemaVersion are
// NOT NULL DEFAULT 0/1 so plain int32 is correct.
type RepoListCodeGraphRow struct {
	ID              string
	Provider        string
	Status          string
	SourceRevision  *string
	CommitHash      string
	GraphSpace      *string
	WorkspaceID     string
	SchemaVersion   int32
	BuilderVersion  string
	VertexCount     int32
	EdgeCount       int32
	AnchorCount     int32
	LinkedEdgeCount int32
	IndexedAt       *time.Time
	SupersededAt    *time.Time
	DeleteAfter     *time.Time
	ErrorMessage    *string
}

// ReposSortField discriminates the legal ORDER BY columns. Anything
// outside this set is rejected at the handler boundary so the SQL
// is never built from raw user input.
type ReposSortField string

const (
	// ReposSortName orders alphabetically by Repo.name.
	ReposSortName ReposSortField = "name"
	// ReposSortPushedAt orders by Repo.pushedAt.
	ReposSortPushedAt ReposSortField = "pushedAt"
	// ReposSortIndexedAt orders by Repo.indexedAt — the closest
	// stand-in for "last activity" available on the current schema.
	// NULL ordering follows Postgres defaults (asc → NULLS LAST,
	// desc → NULLS LAST as well, via the explicit NULLS LAST below).
	ReposSortIndexedAt ReposSortField = "indexedAt"
)

// ReposSortDirection is "asc" / "desc". Other values reject at the
// handler boundary; the SQL only embeds one of these two literals.
type ReposSortDirection string

const (
	ReposSortAsc  ReposSortDirection = "asc"
	ReposSortDesc ReposSortDirection = "desc"
)

// ListOrgReposParams is the argument bag for ListOrgRepos. Skip /
// Take map straight to SQL OFFSET / LIMIT; Query is a case-
// insensitive substring filter on Repo.name (ILIKE '%q%'); Sort and
// Direction must be one of the constants above (the boundary guard
// rejects everything else).
type ListOrgReposParams struct {
	OrgID     int32
	Query     string
	Skip      int32
	Take      int32
	Sort      ReposSortField
	Direction ReposSortDirection
}

// CountOrgReposParams is the argument bag for CountOrgRepos. The
// same orgID + query the listing uses so the totalCount matches the
// listing window exactly.
type CountOrgReposParams struct {
	OrgID int32
	Query string
}

// listOrgReposBaseQuery is the shared FROM + WHERE prefix used by
// both the count and the listing — a Repo gated by an EXISTS
// against RepoToConnection so the row set is exactly the active
// repos for the org. The optional ILIKE filter slots in via the
// parameterised $2 placeholder; an empty query is sentinelled to
// `”` so the placeholder always binds without dynamic SQL
// building.
//
// The listing splices a LEFT JOIN LATERAL between FROM and WHERE
// (see listOrgReposBaseFrom / listOrgReposLateralJoin); the count
// reuses the original FROM-then-WHERE shape so it stays a simple
// COUNT predicate.
const listOrgReposBaseFrom = `FROM "Repo" r`
const listOrgReposWhere = ` WHERE r."orgId" = $1 AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId") AND ($2 = '' OR r.name ILIKE '%' || $2 || '%')`
const listOrgReposBaseQuery = listOrgReposBaseFrom + listOrgReposWhere

// countOrgReposQuery picks DISTINCT r.id to dedupe across multiple
// Connection links per Repo. The result is the exact totalCount the
// pagination header echoes.
const countOrgReposQuery = `SELECT COUNT(DISTINCT r.id) ` + listOrgReposBaseQuery

// listOrgReposProjection is the shared SELECT column list. The
// per-direction queries below splice it onto the base FROM/WHERE +
// the appropriate ORDER BY suffix. Centralising the projection
// keeps the four pre-built statements in sync — adding a column
// is a one-line change here rather than four near-duplicates.
//
// The trailing `j.*` columns come from a LEFT JOIN LATERAL on
// RepoIndexingJob ordered by createdAt DESC, id DESC LIMIT 1, so
// each Repo row carries its most-recent job alongside. Repos with no job
// rows scan as all-NULL on the j.* columns, which the row builder
// detects by an empty j.id (cuid primary keys cannot be empty in a
// real row).
const listOrgReposProjection = `SELECT r.id, r.name, r."displayName", r."indexedAt", r.metadata, r."latestIndexingJobStatus"::text, r."external_codeHostType", r."webUrl", r."imageUrl", r."pushedAt", r."defaultBranch", r."isFork", r."isArchived", j.id, j.type, j.status, j."createdAt", j."completedAt", j."errorMessage", s.id, s.kind, s.status, s.revision, s."commitHash", s."languageCount", s."symbolCount", s."occurrenceCount", s."relationshipCount", s."indexedAt", s."errorMessage", g.id, g.provider, g.status, g."sourceRevision", g."commitHash", g."graphSpace", g."workspaceId", g."schemaVersion", g."builderVersion", g."vertexCount", g."edgeCount", g."anchorCount", g."linkedEdgeCount", g."indexedAt", g."supersededAt", g."deleteAfter", g."errorMessage" `

// listOrgReposLateralJoin is the LATERAL subquery that picks the
// most recent RepoIndexingJob for each Repo row. ORDER BY
// "createdAt" DESC, id DESC + LIMIT 1 yields exactly one job (or zero
// rows when no job has ever run against the Repo) — keeping the outer
// query at one row per Repo.
const listOrgReposLateralJoin = ` LEFT JOIN LATERAL (SELECT id, type, status, "createdAt", "completedAt", "errorMessage" FROM "RepoIndexingJob" WHERE "repoId" = r.id ORDER BY "createdAt" DESC NULLS LAST, id DESC LIMIT 1) j ON TRUE`

// listOrgReposLateralScip is the LATERAL subquery that picks the
// most-recently-updated CodeIntelIndex row for each Repo, regardless
// of kind (the wire schema's `codeIntel.scip` projects whichever
// kind is most recent). ORDER BY "updatedAt" DESC matches the way
// the indexer pipeline supersedes prior generations: the newest
// updatedAt is the active row.
// The "orgId" = r."orgId" predicate is defense-in-depth: it's
// redundant against a correctly-paired writer (the FK on repoId
// already constrains tenant) but a buggy or compromised indexer
// that wrote a row with mismatched (orgId, repoId) cannot leak
// through this listing.
const listOrgReposLateralScip = ` LEFT JOIN LATERAL (SELECT id, kind, status, revision, "commitHash", "languageCount", "symbolCount", "occurrenceCount", "relationshipCount", "indexedAt", "errorMessage" FROM "CodeIntelIndex" WHERE "repoId" = r.id AND "orgId" = r."orgId" ORDER BY "updatedAt" DESC LIMIT 1) s ON TRUE`

// listOrgReposLateralCodeGraph picks the current usable CodeGraphIndex
// row for each Repo. During reindex a newer BUILDING row can exist
// beside the previous READY snapshot; Atom needs the READY snapshot as
// the usable query surface while latestJob communicates the in-progress
// reindex. Same orgId defense-in-depth predicate as the SCIP LATERAL.
const listOrgReposLateralCodeGraph = ` LEFT JOIN LATERAL (SELECT id, provider, status, "sourceRevision", "commitHash", "graphSpace", "workspaceId", "schemaVersion", "builderVersion", "vertexCount", "edgeCount", "anchorCount", "linkedEdgeCount", "indexedAt", "supersededAt", "deleteAfter", "errorMessage" FROM "CodeGraphIndex" WHERE "repoId" = r.id AND "orgId" = r."orgId" ORDER BY CASE WHEN status = 'READY'::"CodeGraphIndexStatus" AND "sourceRevision" IS NOT NULL AND "supersededAt" IS NULL AND "deleteAfter" IS NULL THEN 0 WHEN status = 'READY'::"CodeGraphIndexStatus" THEN 1 ELSE 2 END, "updatedAt" DESC LIMIT 1) g ON TRUE`

// listOrgReposQueryNameAsc / listOrgReposQueryNameDesc /
// listOrgReposQueryIndexedAtAsc / listOrgReposQueryIndexedAtDesc are
// the four ORDER BY shapes the listing supports. SQL keeps the
// ORDER BY column + direction static so the planner sees the same
// query text on every call — avoiding query-cache thrash that a
// dynamically-built ORDER BY would induce.
//
// The listing inserts the LEFT JOIN LATERAL between the FROM
// clause and the WHERE predicate so the latest-job sub-select runs
// once per surviving Repo row.
const listOrgReposListingFrom = listOrgReposBaseFrom + listOrgReposLateralJoin + listOrgReposLateralScip + listOrgReposLateralCodeGraph + listOrgReposWhere
const listOrgReposQueryNameAsc = listOrgReposProjection + listOrgReposListingFrom + ` ORDER BY r.name ASC LIMIT $3 OFFSET $4`
const listOrgReposQueryNameDesc = listOrgReposProjection + listOrgReposListingFrom + ` ORDER BY r.name DESC LIMIT $3 OFFSET $4`
const listOrgReposQueryPushedAtAsc = listOrgReposProjection + listOrgReposListingFrom + ` ORDER BY r."pushedAt" ASC NULLS LAST, r.name ASC LIMIT $3 OFFSET $4`
const listOrgReposQueryPushedAtDesc = listOrgReposProjection + listOrgReposListingFrom + ` ORDER BY r."pushedAt" DESC NULLS LAST, r.name ASC LIMIT $3 OFFSET $4`
const listOrgReposQueryIndexedAtAsc = listOrgReposProjection + listOrgReposListingFrom + ` ORDER BY r."indexedAt" ASC NULLS LAST, r.name ASC LIMIT $3 OFFSET $4`
const listOrgReposQueryIndexedAtDesc = listOrgReposProjection + listOrgReposListingFrom + ` ORDER BY r."indexedAt" DESC NULLS LAST, r.name ASC LIMIT $3 OFFSET $4`

// pickListReposQuery maps the typed (sort, direction) pair to the
// matching pre-built query string. A bad pair returns the empty
// string; the caller treats that as a programmer error.
func pickListReposQuery(sort ReposSortField, dir ReposSortDirection) string {
	switch sort {
	case ReposSortName:
		if dir == ReposSortDesc {
			return listOrgReposQueryNameDesc
		}
		return listOrgReposQueryNameAsc
	case ReposSortPushedAt:
		if dir == ReposSortDesc {
			return listOrgReposQueryPushedAtDesc
		}
		return listOrgReposQueryPushedAtAsc
	case ReposSortIndexedAt:
		if dir == ReposSortDesc {
			return listOrgReposQueryIndexedAtDesc
		}
		return listOrgReposQueryIndexedAtAsc
	}
	return ""
}

// ListOrgRepos returns the paginated, optionally-filtered active-
// repo listing for the org. Pre-allocated slice capacity matches
// the requested page size so the happy path needs no resize.
func (q *Queries) ListOrgRepos(ctx context.Context, p ListOrgReposParams) ([]RepoListRow, error) {
	if p.OrgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	if p.Take <= 0 {
		return nil, fmt.Errorf("db: ListOrgRepos: take must be positive")
	}
	if p.Skip < 0 {
		return nil, fmt.Errorf("db: ListOrgRepos: skip must be non-negative")
	}
	query := pickListReposQuery(p.Sort, p.Direction)
	if query == "" {
		return nil, fmt.Errorf("db: ListOrgRepos: unsupported sort/direction (%q, %q)", p.Sort, p.Direction)
	}
	rows, err := q.db.Query(ctx, query, p.OrgID, p.Query, p.Take, p.Skip)
	if err != nil {
		return nil, fmt.Errorf("db: ListOrgRepos: %w", err)
	}
	defer rows.Close()
	out := make([]RepoListRow, 0, p.Take)
	for rows.Next() {
		var (
			r       RepoListRow
			jobID   *string
			jobType *string
			jobStat *string
			jobAt   *time.Time
			jobDone *time.Time
			jobErr  *string

			scipID                *string
			scipKind              *string
			scipStatus            *string
			scipRevision          *string
			scipCommitHash        *string
			scipLanguageCount     *int32
			scipSymbolCount       *int32
			scipOccurrenceCount   *int32
			scipRelationshipCount *int32
			scipIndexedAt         *time.Time
			scipErr               *string

			cgID              *string
			cgProvider        *string
			cgStatus          *string
			cgSourceRevision  *string
			cgCommitHash      *string
			cgGraphSpace      *string
			cgWorkspaceID     *string
			cgSchemaVersion   *int32
			cgBuilderVersion  *string
			cgVertexCount     *int32
			cgEdgeCount       *int32
			cgAnchorCount     *int32
			cgLinkedEdgeCount *int32
			cgIndexedAt       *time.Time
			cgSupersededAt    *time.Time
			cgDeleteAfter     *time.Time
			cgErrorMessage    *string
		)
		if err := rows.Scan(
			&r.RepoID, &r.RepoName, &r.RepoDisplayName, &r.IndexedAt, &r.Metadata, &r.LatestIndexingJobStatus,
			&r.CodeHostType, &r.WebUrl, &r.ImageUrl, &r.PushedAt, &r.DefaultBranch,
			&r.IsFork, &r.IsArchived,
			&jobID, &jobType, &jobStat, &jobAt, &jobDone, &jobErr,
			&scipID, &scipKind, &scipStatus, &scipRevision, &scipCommitHash,
			&scipLanguageCount, &scipSymbolCount, &scipOccurrenceCount, &scipRelationshipCount,
			&scipIndexedAt, &scipErr,
			&cgID, &cgProvider, &cgStatus, &cgSourceRevision, &cgCommitHash,
			&cgGraphSpace, &cgWorkspaceID, &cgSchemaVersion, &cgBuilderVersion,
			&cgVertexCount, &cgEdgeCount, &cgAnchorCount, &cgLinkedEdgeCount,
			&cgIndexedAt, &cgSupersededAt, &cgDeleteAfter, &cgErrorMessage,
		); err != nil {
			return nil, fmt.Errorf("db: ListOrgRepos: scan: %w", err)
		}
		// A LEFT JOIN LATERAL with no matching job emits NULL for
		// every job column; jobID is the cuid primary key which
		// cannot be empty in a real row, so a nil pointer is the
		// "no job" signal.
		if jobID != nil {
			job := RepoListJobRow{
				ID:           *jobID,
				CompletedAt:  jobDone,
				ErrorMessage: jobErr,
			}
			if jobType != nil {
				job.Type = *jobType
			}
			if jobStat != nil {
				job.Status = *jobStat
			}
			if jobAt != nil {
				job.CreatedAt = *jobAt
			}
			r.LatestJob = &job
		}
		// Same sentinel pattern for the CodeIntelIndex LATERAL: the
		// cuid primary key is the "row exists" signal.
		if scipID != nil {
			scip := RepoListScipRow{
				ID:           *scipID,
				IndexedAt:    scipIndexedAt,
				ErrorMessage: scipErr,
			}
			if scipKind != nil {
				scip.Kind = *scipKind
			}
			if scipStatus != nil {
				scip.Status = *scipStatus
			}
			if scipRevision != nil {
				scip.Revision = *scipRevision
			}
			if scipCommitHash != nil {
				scip.CommitHash = *scipCommitHash
			}
			if scipLanguageCount != nil {
				scip.LanguageCount = *scipLanguageCount
			}
			if scipSymbolCount != nil {
				scip.SymbolCount = *scipSymbolCount
			}
			if scipOccurrenceCount != nil {
				scip.OccurrenceCount = *scipOccurrenceCount
			}
			if scipRelationshipCount != nil {
				scip.RelationshipCount = *scipRelationshipCount
			}
			r.LatestScip = &scip
		}
		// Same sentinel pattern for the CodeGraphIndex LATERAL.
		if cgID != nil {
			cg := RepoListCodeGraphRow{
				ID:             *cgID,
				SourceRevision: cgSourceRevision,
				GraphSpace:     cgGraphSpace,
				IndexedAt:      cgIndexedAt,
				SupersededAt:   cgSupersededAt,
				DeleteAfter:    cgDeleteAfter,
				ErrorMessage:   cgErrorMessage,
			}
			if cgProvider != nil {
				cg.Provider = *cgProvider
			}
			if cgStatus != nil {
				cg.Status = *cgStatus
			}
			if cgCommitHash != nil {
				cg.CommitHash = *cgCommitHash
			}
			if cgWorkspaceID != nil {
				cg.WorkspaceID = *cgWorkspaceID
			}
			if cgSchemaVersion != nil {
				cg.SchemaVersion = *cgSchemaVersion
			}
			if cgBuilderVersion != nil {
				cg.BuilderVersion = *cgBuilderVersion
			}
			if cgVertexCount != nil {
				cg.VertexCount = *cgVertexCount
			}
			if cgEdgeCount != nil {
				cg.EdgeCount = *cgEdgeCount
			}
			if cgAnchorCount != nil {
				cg.AnchorCount = *cgAnchorCount
			}
			if cgLinkedEdgeCount != nil {
				cg.LinkedEdgeCount = *cgLinkedEdgeCount
			}
			r.LatestCodeGraph = &cg
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListOrgRepos: rows: %w", err)
	}
	return out, nil
}

// CountOrgRepos returns the total active-repo count for the org
// under the same query filter ListOrgRepos uses, so the totalCount
// the handler emits as X-Total-Count is consistent with the
// paginated row set.
func (q *Queries) CountOrgRepos(ctx context.Context, p CountOrgReposParams) (int32, error) {
	if p.OrgID <= 0 {
		return 0, ErrInvalidOrgID
	}
	var n int32
	if err := q.db.QueryRow(ctx, countOrgReposQuery, p.OrgID, p.Query).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: CountOrgRepos: %w", err)
	}
	return n, nil
}
