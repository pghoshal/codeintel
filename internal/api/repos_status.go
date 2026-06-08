// GET /api/repos/{id}/status — per-repo indexing-state summary.
// Minimum-useful subset port of legacy GET /api/repos/[id]/status
// (packages/web/src/app/api/(server)/repos/[id]/status/route.ts).
//
// Scope of this slice (Phase C.6): Repo basic fields + the last
// 20 RepoIndexingJob rows. The legacy projection also bundles
// SCIP / CodeGraph index summaries + branch index status; those
// land in follow-up slices when the CodeIntelLanguageIndex +
// CodeGraphRevision sub-tables are wired up.
//
// Tenant scoping: the SQL filters on "Repo"."orgId" so a caller
// in OrgA cannot probe for OrgB's repos by id (returns 404 just
// like a missing row).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"codeintel/internal/auth"
	"codeintel/pkg/repoindexstatus"

	"github.com/jackc/pgx/v5"
)

// RepoStatusJob is the per-job projection inside the status
// response. Mirrors the legacy `jobs[*]` shape from
// packages/web/src/app/api/(server)/repos/[id]/status/route.ts:55-64:
// id, type, status, createdAt, updatedAt, completedAt, errorMessage,
// metadata.
type RepoStatusJob struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	CreatedAt    time.Time       `json:"createdAt"`
	UpdatedAt    time.Time       `json:"updatedAt"`
	CompletedAt  *time.Time      `json:"completedAt,omitempty"`
	ErrorMessage *string         `json:"errorMessage,omitempty"`
	Metadata     json.RawMessage `json:"metadata,omitempty"`
}

// RepoStatusResponse is the wire body for GET /api/repos/{id}/status.
// Nullable Repo columns surface as omitempty pointer fields so a
// repo that's never been indexed produces a clean JSON shape.
//
// Scope as of P.3b: the helper-derived summary fields
// (indexStatus, indexStatusColor, indexed, activeIndexStatus,
// activeIndexStatusColor, activeIndexUsable, indexedRevisions,
// latestIndexRun, latestJob) are now populated from
// pkg/repoindexstatus.BuildRepoIndexSummary. The remaining
// legacy projection bits (branchStatus, branchStatuses,
// codeIntel.scip block, codeIntel.codeGraph block,
// indexManifests) land in P.3c / P.3d.
type RepoStatusResponse struct {
	ID                      int32           `json:"id"`
	Name                    string          `json:"name"`
	DisplayName             *string         `json:"displayName,omitempty"`
	DefaultBranch           *string         `json:"defaultBranch,omitempty"`
	IndexedAt               *time.Time      `json:"indexedAt,omitempty"`
	IndexedCommitHash       *string         `json:"indexedCommitHash,omitempty"`
	LatestIndexingJobStatus *string         `json:"latestIndexingJobStatus,omitempty"`
	UpdatedAt               time.Time       `json:"updatedAt"`
	Metadata                json.RawMessage `json:"metadata,omitempty"`

	// Helper-derived fields (P.3b). Direct ports of the legacy
	// `buildRepoIndexSummary` return shape — same JSON keys.
	IndexStatus            string             `json:"indexStatus"`
	IndexStatusColor       string             `json:"indexStatusColor"`
	Indexed                bool               `json:"indexed"`
	IndexedRevisions       []string           `json:"indexedRevisions"`
	ActiveIndexStatus      string             `json:"activeIndexStatus"`
	ActiveIndexStatusColor string             `json:"activeIndexStatusColor"`
	ActiveIndexUsable      bool               `json:"activeIndexUsable"`
	LatestIndexRun         RepoLatestIndexRun `json:"latestIndexRun"`
	LatestJob              *RepoStatusJob     `json:"latestJob,omitempty"`

	// Branch-status fields (P.3c). BranchStatuses is the union
	// of known branches; BranchStatus is the per-branch query
	// driven by `?branch=X` and only populated when the route
	// got that query param.
	BranchStatuses []repoindexstatus.BranchIndexStatus `json:"branchStatuses"`
	BranchStatus   *repoindexstatus.BranchIndexStatus  `json:"branchStatus,omitempty"`

	// CodeIntel mirrors the nested legacy object
	// route.ts:220-224. P.3d-ii populates the .scip[] array;
	// .codeGraph[] and .currentCodeGraph land in P.3d-iii.
	CodeIntel RepoStatusCodeIntel `json:"codeIntel"`

	// IndexManifests + CurrentIndexManifests mirror
	// route.ts:225-226. CurrentIndexManifests is the subset
	// where status=READY AND supersededAt IS NULL — the
	// "active" manifests UI surfaces.
	IndexManifests        []RepoIndexManifestRow `json:"indexManifests"`
	CurrentIndexManifests []RepoIndexManifestRow `json:"currentIndexManifests"`
	IndexQualityIssues    []string               `json:"indexQualityIssues"`

	Jobs []RepoStatusJob `json:"jobs"`
}

// RepoIndexManifestRow mirrors the legacy
// `indexManifests[*]` projection (route.ts:154-180). JSON keys
// match the legacy property names verbatim.
type RepoIndexManifestRow struct {
	ID                    string     `json:"id"`
	Status                string     `json:"status"`
	WorkspaceID           string     `json:"workspaceId"`
	ProviderConnectionID  *string    `json:"providerConnectionId"`
	Branch                string     `json:"branch"`
	CommitHash            string     `json:"commitHash"`
	FileCount             int32      `json:"fileCount"`
	FileRowCount          int32      `json:"fileRowCount"`
	AddedFileCount        int32      `json:"addedFileCount"`
	ChangedFileCount      int32      `json:"changedFileCount"`
	DeletedFileCount      int32      `json:"deletedFileCount"`
	UnchangedFileCount    int32      `json:"unchangedFileCount"`
	ZoektStrategy         *string    `json:"zoektStrategy"`
	ScipStrategy          *string    `json:"scipStrategy"`
	GraphStrategy         *string    `json:"graphStrategy"`
	SemanticStrategy      *string    `json:"semanticStrategy"`
	SemanticPromptVersion *string    `json:"semanticPromptVersion"`
	SemanticModelID       *string    `json:"semanticModelId"`
	SemanticSchemaVersion *int32     `json:"semanticSchemaVersion"`
	ActivatedAt           *time.Time `json:"activatedAt"`
	SupersededAt          *time.Time `json:"supersededAt"`
	FailedAt              *time.Time `json:"failedAt"`
	ErrorMessage          *string    `json:"errorMessage"`
	CreatedAt             time.Time  `json:"createdAt"`
	UpdatedAt             time.Time  `json:"updatedAt"`
	QualityIssues         []string   `json:"qualityIssues,omitempty"`
}

// RepoStatusCodeIntel is the legacy `codeIntel` block. Fields
// added incrementally as their port slice lands.
//
//   - Scip[]                 — populated by P.3d-ii.
//   - CodeGraph[]            — populated by P.3d-iii. Sorted by
//     SortCodeGraphIndexesForStatus.
//   - CurrentCodeGraph       — populated by P.3d-iii. The single
//     row SelectCurrentCodeGraphIndex
//     returns, or null when there are
//     no indexes for the repo.
type RepoStatusCodeIntel struct {
	Scip             []repoindexstatus.ScipCodeIntelIndexOutput `json:"scip"`
	CodeGraph        []repoindexstatus.CodeGraphIndexOutput     `json:"codeGraph"`
	CurrentCodeGraph *repoindexstatus.CodeGraphIndexOutput      `json:"currentCodeGraph"`
}

// RepoLatestIndexRun mirrors the legacy `latestIndexRun` nested
// object (route.ts:212-216). Status + statusColor + the
// blocks-active-index boolean.
type RepoLatestIndexRun struct {
	Status            string `json:"status"`
	StatusColor       string `json:"statusColor"`
	BlocksActiveIndex bool   `json:"blocksActiveIndex"`
}

// ErrRepoStatusNotFound is the typed sentinel returned by the
// fetcher when the repo doesn't exist in the caller's org.
// Surfaces as 404 NOT_FOUND at the HTTP boundary.
var ErrRepoStatusNotFound = errors.New("api: repo not found in org")

// RepoStatusFetcher is the extension point so tests can drop in a
// fake without standing up Postgres. Same pattern as RepoIndexer.
//
// The optional `requestedBranch` is the value of the
// `?branch=X` query parameter from GET /api/repos/{id}/status.
// When non-empty it populates the `branchStatus` field on the
// response. Empty leaves it omitted.
type RepoStatusFetcher interface {
	Fetch(ctx context.Context, orgID, repoID int32, requestedBranch string) (RepoStatusResponse, error)
}

// pgxStatusQuerier is the narrow pgx surface PgxRepoStatusFetcher
// uses. *pgxpool.Pool satisfies it directly.
type pgxStatusQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// PgxRepoStatusFetcher loads the repo + last 20 jobs from the
// codeintel pgx pool. Two round-trips (one for the Repo row, one
// for the jobs) — keeps the SQL simple and lets the handler
// short-circuit on a 404 before the second query fires.
type PgxRepoStatusFetcher struct {
	db pgxStatusQuerier
}

// NewPgxRepoStatusFetcher wires the fetcher to a live pool.
func NewPgxRepoStatusFetcher(db pgxStatusQuerier) *PgxRepoStatusFetcher {
	return &PgxRepoStatusFetcher{db: db}
}

// Fetch returns the status projection for (orgID, repoID), or
// ErrRepoStatusNotFound when the repo isn't in the org OR has
// no RepoToConnection rows. The connections.some({}) filter
// mirrors legacy
// packages/web/src/app/api/(server)/repos/[id]/status/route.ts
// (closes P.7 in docs/codeintel-parity-gaps.md).
//
// requestedBranch (P.3c) drives the per-branch `branchStatus`
// field — empty leaves the field omitted, matching the legacy
// `requestedBranch ? buildBranchIndexStatus(...) : undefined`.
func (f *PgxRepoStatusFetcher) Fetch(ctx context.Context, orgID, repoID int32, requestedBranch string) (RepoStatusResponse, error) {
	var resp RepoStatusResponse
	var latestStatus *string
	var metadataBytes []byte
	err := f.db.QueryRow(ctx, `
		SELECT r.id, r.name, r."displayName", r."defaultBranch",
		       r."indexedAt", r."indexedCommitHash",
		       r."latestIndexingJobStatus"::text,
		       r."updatedAt",
		       r.metadata
		FROM   "Repo" r
		WHERE  r.id = $1 AND r."orgId" = $2
		  AND  EXISTS (
		      SELECT 1 FROM "RepoToConnection" rc
		      WHERE  rc."repoId" = r.id
		  )
	`, repoID, orgID).Scan(
		&resp.ID, &resp.Name, &resp.DisplayName, &resp.DefaultBranch,
		&resp.IndexedAt, &resp.IndexedCommitHash,
		&latestStatus,
		&resp.UpdatedAt,
		&metadataBytes,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoStatusResponse{}, ErrRepoStatusNotFound
	}
	if err != nil {
		return RepoStatusResponse{}, fmt.Errorf("PgxRepoStatusFetcher: load Repo: %w", err)
	}
	resp.LatestIndexingJobStatus = latestStatus
	if len(metadataBytes) > 0 {
		resp.Metadata = json.RawMessage(metadataBytes)
	}

	rows, err := f.db.Query(ctx, `
		SELECT id, type::text, status::text,
		       "createdAt", "updatedAt", "completedAt", "errorMessage",
		       metadata
		FROM   "RepoIndexingJob"
		WHERE  "repoId" = $1
		ORDER  BY "createdAt" DESC NULLS LAST, id DESC
		LIMIT  20
	`, repoID)
	if err != nil {
		return RepoStatusResponse{}, fmt.Errorf("PgxRepoStatusFetcher: load jobs: %w", err)
	}
	defer rows.Close()
	resp.Jobs = make([]RepoStatusJob, 0, 20)
	for rows.Next() {
		var j RepoStatusJob
		var jobMeta []byte
		if err := rows.Scan(&j.ID, &j.Type, &j.Status, &j.CreatedAt, &j.UpdatedAt, &j.CompletedAt, &j.ErrorMessage, &jobMeta); err != nil {
			return RepoStatusResponse{}, fmt.Errorf("PgxRepoStatusFetcher: scan job: %w", err)
		}
		if len(jobMeta) > 0 {
			j.Metadata = json.RawMessage(jobMeta)
		}
		resp.Jobs = append(resp.Jobs, j)
	}
	if err := rows.Err(); err != nil {
		return RepoStatusResponse{}, fmt.Errorf("PgxRepoStatusFetcher: iterate jobs: %w", err)
	}

	// P.3b: derive the helper fields from the just-loaded Repo +
	// the most-recent job. Mirror of legacy route.ts:192-216.
	var latestJob *RepoStatusJob
	if len(resp.Jobs) > 0 {
		latestJob = &resp.Jobs[0]
	}
	applyIndexSummary(&resp, latestJob)
	applyBranchStatuses(&resp, latestJob, requestedBranch)
	if err := f.loadCodeIntelScip(ctx, repoID, &resp); err != nil {
		return RepoStatusResponse{}, err
	}
	if err := f.loadCodeGraph(ctx, repoID, &resp); err != nil {
		return RepoStatusResponse{}, err
	}
	if err := f.loadIndexManifests(ctx, repoID, &resp); err != nil {
		return RepoStatusResponse{}, err
	}
	return resp, nil
}

// loadIndexManifests populates resp.IndexManifests +
// resp.CurrentIndexManifests. Last 20 RepoIndexManifest rows
// for the repo, sorted by (activatedAt DESC, createdAt DESC) —
// matches route.ts:148-152.
func (f *PgxRepoStatusFetcher) loadIndexManifests(ctx context.Context, repoID int32, resp *RepoStatusResponse) error {
	rows, err := f.db.Query(ctx, `
		SELECT id, status::text, "workspaceId", "providerConnectionId",
		       branch, "commitHash",
		       "fileCount",
		       (SELECT COUNT(*) FROM "RepoIndexManifestFile" mf WHERE mf."manifestId" = m.id) AS "fileRowCount",
		       "addedFileCount", "changedFileCount",
		       "deletedFileCount", "unchangedFileCount",
		       "zoektStrategy", "scipStrategy", "graphStrategy", "semanticStrategy",
		       "semanticPromptVersion", "semanticModelId", "semanticSchemaVersion",
		       "activatedAt", "supersededAt", "failedAt", "errorMessage",
		       "createdAt", "updatedAt"
		FROM   "RepoIndexManifest" m
		WHERE  "repoId" = $1
		ORDER  BY "activatedAt" DESC NULLS LAST, "createdAt" DESC
		LIMIT  20
	`, repoID)
	if err != nil {
		return fmt.Errorf("loadIndexManifests: %w", err)
	}
	defer rows.Close()

	resp.IndexManifests = make([]RepoIndexManifestRow, 0, 20)
	for rows.Next() {
		var m RepoIndexManifestRow
		if err := rows.Scan(
			&m.ID, &m.Status, &m.WorkspaceID, &m.ProviderConnectionID,
			&m.Branch, &m.CommitHash,
			&m.FileCount, &m.FileRowCount, &m.AddedFileCount, &m.ChangedFileCount,
			&m.DeletedFileCount, &m.UnchangedFileCount,
			&m.ZoektStrategy, &m.ScipStrategy, &m.GraphStrategy, &m.SemanticStrategy,
			&m.SemanticPromptVersion, &m.SemanticModelID, &m.SemanticSchemaVersion,
			&m.ActivatedAt, &m.SupersededAt, &m.FailedAt, &m.ErrorMessage,
			&m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return fmt.Errorf("loadIndexManifests: scan: %w", err)
		}
		m.QualityIssues = manifestQualityIssues(m)
		resp.IndexManifests = append(resp.IndexManifests, m)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("loadIndexManifests: rows.Err: %w", err)
	}

	// currentIndexManifests = manifests where status=READY AND
	// supersededAt IS NULL. Mirrors route.ts:226 filter.
	resp.CurrentIndexManifests = make([]RepoIndexManifestRow, 0, len(resp.IndexManifests))
	for _, m := range resp.IndexManifests {
		if m.Status == "READY" && m.SupersededAt == nil {
			resp.CurrentIndexManifests = append(resp.CurrentIndexManifests, m)
		}
	}
	resp.IndexQualityIssues = append(resp.IndexQualityIssues, repoManifestQualityIssues(resp.IndexManifests)...)
	return nil
}

func manifestQualityIssues(m RepoIndexManifestRow) []string {
	var issues []string
	if m.Status == "READY" && m.FileCount > 0 && m.FileRowCount == 0 {
		issues = append(issues, "ready manifest has no linked file rows")
	}
	if m.Status == "READY" && m.FileRowCount > 0 && m.FileRowCount != m.FileCount {
		issues = append(issues, "ready manifest file row count does not match fileCount")
	}
	if m.Status == "READY" && m.ZoektStrategy != nil && *m.ZoektStrategy == "FULL_REPO" && (m.ChangedFileCount > 0 || m.DeletedFileCount > 0) {
		issues = append(issues, "Zoekt reindex used full-repo strategy for changed files")
	}
	return issues
}

func repoManifestQualityIssues(manifests []RepoIndexManifestRow) []string {
	var issues []string
	for _, m := range manifests {
		if m.Status != "READY" || m.SupersededAt != nil {
			continue
		}
		for _, issue := range m.QualityIssues {
			issues = append(issues, "manifest "+m.ID+": "+issue)
		}
	}
	hasCurrentReadyByScope := map[string]bool{}
	for _, m := range manifests {
		if m.Status == "READY" && m.SupersededAt == nil {
			hasCurrentReadyByScope[m.WorkspaceID+"\x00"+m.Branch] = true
		}
	}
	for _, m := range manifests {
		if m.Status == "PENDING" && hasCurrentReadyByScope[m.WorkspaceID+"\x00"+m.Branch] {
			issues = append(issues, "manifest "+m.ID+": stale pending manifest exists beside active READY manifest")
		}
	}
	return issues
}

// loadCodeGraph populates resp.CodeIntel.CodeGraph[] +
// resp.CodeIntel.CurrentCodeGraph by loading the last 10
// CodeGraphIndex rows plus their CodeGraphRevision children +
// semantic-table counts. Mirrors legacy route.ts:106-147 +
// route.ts:194-195,222-223.
func (f *PgxRepoStatusFetcher) loadCodeGraph(ctx context.Context, repoID int32, resp *RepoStatusResponse) error {
	rows, err := f.db.Query(ctx, `
		SELECT g.id,
		       g.provider::text,
		       g.status::text,
		       g."sourceRevision",
		       g."commitHash",
		       g."graphSpace",
		       g."workspaceId",
		       g."schemaVersion",
		       g."builderVersion",
		       g."vertexCount",
		       g."edgeCount",
		       g."anchorCount",
		       g."linkedEdgeCount",
		       g."indexedAt",
		       g."supersededAt",
		       g."deleteAfter",
		       g."errorMessage",
		       (SELECT COUNT(*) FROM "CodeGraphSemanticFact"      WHERE "graphIndexId" = g.id),
		       (SELECT COUNT(*) FROM "CodeGraphSemanticEdge"      WHERE "graphIndexId" = g.id),
		       (SELECT COUNT(*) FROM "CodeGraphSemanticHyperedge" WHERE "graphIndexId" = g.id)
		FROM   "CodeGraphIndex" g
		WHERE  g."repoId" = $1
		ORDER  BY g."updatedAt" DESC
		LIMIT  10
	`, repoID)
	if err != nil {
		return fmt.Errorf("loadCodeGraph: %w", err)
	}
	defer rows.Close()

	var indexes []repoindexstatus.CodeGraphIndexInput
	var indexIDs []string
	for rows.Next() {
		var idx repoindexstatus.CodeGraphIndexInput
		var facts, edges, hyper int64
		if err := rows.Scan(
			&idx.ID, &idx.Provider, &idx.Status,
			&idx.SourceRevision, &idx.CommitHash, &idx.GraphSpace,
			&idx.WorkspaceID, &idx.SchemaVersion, &idx.BuilderVersion,
			&idx.VertexCount, &idx.EdgeCount, &idx.AnchorCount, &idx.LinkedEdgeCount,
			&idx.IndexedAt, &idx.SupersededAt, &idx.DeleteAfter, &idx.ErrorMessage,
			&facts, &edges, &hyper,
		); err != nil {
			return fmt.Errorf("loadCodeGraph: scan: %w", err)
		}
		fc := int32(facts)
		ec := int32(edges)
		hc := int32(hyper)
		idx.Counts = &repoindexstatus.CodeGraphCounts{
			SemanticFacts:      &fc,
			SemanticEdges:      &ec,
			SemanticHyperedges: &hc,
		}
		indexes = append(indexes, idx)
		indexIDs = append(indexIDs, idx.ID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("loadCodeGraph: rows.Err: %w", err)
	}

	// Bulk-load the revision children for all the indexes.
	revsByParent := make(map[string][]repoindexstatus.CodeGraphRevisionInput, len(indexes))
	if len(indexIDs) > 0 {
		rrows, err := f.db.Query(ctx, `
			SELECT "codeGraphIndexId", revision, "commitHash", "activatedAt"
			FROM   "CodeGraphRevision"
			WHERE  "codeGraphIndexId" = ANY ($1::text[])
			ORDER  BY "codeGraphIndexId", revision
		`, indexIDs)
		if err != nil {
			return fmt.Errorf("loadCodeGraph: revisions query: %w", err)
		}
		defer rrows.Close()
		for rrows.Next() {
			var parentID string
			var rev repoindexstatus.CodeGraphRevisionInput
			if err := rrows.Scan(&parentID, &rev.Revision, &rev.CommitHash, &rev.ActivatedAt); err != nil {
				return fmt.Errorf("loadCodeGraph: revision scan: %w", err)
			}
			revsByParent[parentID] = append(revsByParent[parentID], rev)
		}
		if err := rrows.Err(); err != nil {
			return fmt.Errorf("loadCodeGraph: revisions rows.Err: %w", err)
		}
	}
	for i := range indexes {
		indexes[i].Revisions = revsByParent[indexes[i].ID]
	}

	// Sort + format. Legacy uses SortCodeGraphIndexesForStatus
	// for the array shape + SelectCurrentCodeGraphIndex for
	// the singleton "currentCodeGraph" pointer.
	sorted := repoindexstatus.SortCodeGraphIndexesForStatus(indexes)
	resp.CodeIntel.CodeGraph = make([]repoindexstatus.CodeGraphIndexOutput, 0, len(sorted))
	for i := range sorted {
		out := repoindexstatus.FormatCodeGraphIndex(&sorted[i])
		if out != nil {
			resp.CodeIntel.CodeGraph = append(resp.CodeIntel.CodeGraph, *out)
			resp.IndexQualityIssues = append(resp.IndexQualityIssues, codeGraphQualityIssues(*out)...)
		}
	}
	if current := repoindexstatus.SelectCurrentCodeGraphIndex(indexes); current != nil {
		resp.CodeIntel.CurrentCodeGraph = repoindexstatus.FormatCodeGraphIndex(current)
	}
	return nil
}

func codeGraphQualityIssues(g repoindexstatus.CodeGraphIndexOutput) []string {
	if g.Status != "READY" {
		return nil
	}
	var issues []string
	if g.EdgeCount > 0 && g.AnchorCount == 0 {
		issues = append(issues, "graph "+g.ID+": READY graph has edges but zero anchors")
	}
	if g.EdgeCount > 0 && g.LinkedEdgeCount == 0 {
		issues = append(issues, "graph "+g.ID+": READY graph has edges but zero linked edges")
	}
	if g.SemanticEdgeCount == 0 && g.EdgeCount > 0 {
		issues = append(issues, "graph "+g.ID+": READY graph has no Postgres semantic edge rows")
	}
	return issues
}

// loadCodeIntelScip loads the last 10 CodeIntelIndex rows (with
// their language indexes) and populates resp.CodeIntel.Scip.
// Mirrors the legacy route.ts:66-105 + route.ts:221 selection.
func (f *PgxRepoStatusFetcher) loadCodeIntelScip(ctx context.Context, repoID int32, resp *RepoStatusResponse) error {
	rows, err := f.db.Query(ctx, `
		SELECT id, kind::text, status::text, revision, "commitHash",
		       "languageCount", "symbolCount", "occurrenceCount", "relationshipCount",
		       "indexedAt", "errorMessage"
		FROM   "CodeIntelIndex"
		WHERE  "repoId" = $1
		ORDER  BY "updatedAt" DESC
		LIMIT  10
	`, repoID)
	if err != nil {
		return fmt.Errorf("loadCodeIntelScip: %w", err)
	}
	defer rows.Close()

	var indexes []repoindexstatus.ScipCodeIntelIndexInput
	var indexIDs []string
	for rows.Next() {
		var idx repoindexstatus.ScipCodeIntelIndexInput
		if err := rows.Scan(
			&idx.ID, &idx.Kind, &idx.Status, &idx.Revision, &idx.CommitHash,
			&idx.LanguageCount, &idx.SymbolCount, &idx.OccurrenceCount, &idx.RelationshipCount,
			&idx.IndexedAt, &idx.ErrorMessage,
		); err != nil {
			return fmt.Errorf("loadCodeIntelScip: scan: %w", err)
		}
		indexes = append(indexes, idx)
		indexIDs = append(indexIDs, idx.ID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("loadCodeIntelScip: rows.Err: %w", err)
	}

	// Bulk-load language indexes for all parent rows in a single
	// query. Order matches the format helper's sort (projectRoot,
	// language, indexer) so the per-parent slices come back
	// pre-sorted.
	langByParent := make(map[string][]repoindexstatus.CodeIntelLanguageIndexInput, len(indexes))
	if len(indexIDs) > 0 {
		lrows, err := f.db.Query(ctx, `
			SELECT "codeIntelIndexId",
			       language, "projectRoot", indexer,
			       "workerClass", status::text, "artifactPath",
			       "toolchainFingerprint", "toolchainVersion", "toolchainPath", "toolchainSha256",
			       "durationMs", "errorMessage"
			FROM   "CodeIntelLanguageIndex"
			WHERE  "codeIntelIndexId" = ANY ($1::text[])
			ORDER  BY "codeIntelIndexId", "projectRoot", language, indexer
		`, indexIDs)
		if err != nil {
			return fmt.Errorf("loadCodeIntelScip: language query: %w", err)
		}
		defer lrows.Close()
		for lrows.Next() {
			var parentID string
			var li repoindexstatus.CodeIntelLanguageIndexInput
			var durationMs *int32
			if err := lrows.Scan(
				&parentID,
				&li.Language, &li.ProjectRoot, &li.Indexer,
				&li.WorkerClass, &li.Status, &li.ArtifactPath,
				&li.ToolchainFingerprint, &li.ToolchainVersion, &li.ToolchainPath, &li.ToolchainSha256,
				&durationMs, &li.ErrorMessage,
			); err != nil {
				return fmt.Errorf("loadCodeIntelScip: language scan: %w", err)
			}
			if durationMs != nil {
				v := int64(*durationMs)
				li.DurationMs = &v
			}
			langByParent[parentID] = append(langByParent[parentID], li)
		}
		if err := lrows.Err(); err != nil {
			return fmt.Errorf("loadCodeIntelScip: language rows.Err: %w", err)
		}
	}

	// Run the format helper on each parent + collect into the
	// resp slice. Legacy passes includeArtifactPaths=true on
	// /status (route.ts:221).
	resp.CodeIntel.Scip = make([]repoindexstatus.ScipCodeIntelIndexOutput, 0, len(indexes))
	for i := range indexes {
		indexes[i].LanguageIndexes = langByParent[indexes[i].ID]
		out := repoindexstatus.FormatScipCodeIntelIndex(&indexes[i],
			repoindexstatus.FormatScipCodeIntelOptions{IncludeArtifactPaths: true})
		if out != nil {
			resp.CodeIntel.Scip = append(resp.CodeIntel.Scip, *out)
			resp.IndexQualityIssues = append(resp.IndexQualityIssues, scipQualityIssues(*out)...)
		}
	}
	return nil
}

func scipQualityIssues(s repoindexstatus.ScipCodeIntelIndexOutput) []string {
	if s.Status == "READY" && s.OccurrenceCount == 0 {
		return []string{"SCIP " + s.ID + ": READY index has zero occurrences"}
	}
	if s.Status == "PARTIAL" || s.Status == "INDEXING" || s.Status == "FAILED" {
		return []string{"SCIP " + s.ID + ": index is " + s.Status}
	}
	return nil
}

// applyBranchStatuses populates BranchStatuses (always) and
// BranchStatus (when requestedBranch is non-empty). Mirrors
// route.ts:218-219.
func applyBranchStatuses(resp *RepoStatusResponse, latestJob *RepoStatusJob, requestedBranch string) {
	repoInput := repoindexstatus.RepoInput{
		Metadata:                resp.Metadata,
		DefaultBranch:           resp.DefaultBranch,
		LatestIndexingJobStatus: resp.LatestIndexingJobStatus,
	}
	if resp.IndexedAt != nil {
		s := resp.IndexedAt.Format(time.RFC3339Nano)
		repoInput.IndexedAt = &s
	}
	var jobInput *repoindexstatus.LatestJob
	if latestJob != nil {
		jobInput = &repoindexstatus.LatestJob{
			ID:     latestJob.ID,
			Type:   repoindexstatus.JobType(latestJob.Type),
			Status: repoindexstatus.JobStatus(latestJob.Status),
		}
	}
	resp.BranchStatuses = repoindexstatus.BuildKnownBranchIndexStatuses(repoInput, jobInput)
	if requestedBranch != "" {
		b := repoindexstatus.BuildBranchIndexStatus(repoInput, requestedBranch, jobInput)
		resp.BranchStatus = &b
	}
}

// applyIndexSummary populates the helper-derived fields on
// resp by invoking the parity-port BuildRepoIndexSummary. Kept
// as a free function so it's straightforward to wire-test (and
// mirror against legacy fixtures) without touching the SQL
// path.
func applyIndexSummary(resp *RepoStatusResponse, latestJob *RepoStatusJob) {
	repoInput := repoindexstatus.RepoInput{
		Metadata:                resp.Metadata,
		DefaultBranch:           resp.DefaultBranch,
		LatestIndexingJobStatus: resp.LatestIndexingJobStatus,
	}
	if resp.IndexedAt != nil {
		// BuildRepoIndexSummary only checks IndexedAt for
		// non-nil-ness; encoding as the formatted string keeps
		// the type-cross-package wiring trivial.
		s := resp.IndexedAt.Format(time.RFC3339Nano)
		repoInput.IndexedAt = &s
	}

	var jobInput *repoindexstatus.LatestJob
	if latestJob != nil {
		jobInput = &repoindexstatus.LatestJob{
			ID:     latestJob.ID,
			Type:   repoindexstatus.JobType(latestJob.Type),
			Status: repoindexstatus.JobStatus(latestJob.Status),
		}
	}

	summary := repoindexstatus.BuildRepoIndexSummary(repoInput, jobInput)
	resp.IndexStatus = string(summary.Status)
	resp.IndexStatusColor = string(summary.Color)
	resp.Indexed = summary.Indexed
	resp.IndexedRevisions = summary.IndexedRevisions
	resp.ActiveIndexStatus = string(summary.ActiveIndexStatus)
	resp.ActiveIndexStatusColor = string(summary.ActiveIndexStatusColor)
	resp.ActiveIndexUsable = summary.ActiveIndexUsable
	resp.LatestIndexRun = RepoLatestIndexRun{
		Status:            string(summary.LatestRunStatus),
		StatusColor:       string(summary.LatestRunStatusColor),
		BlocksActiveIndex: summary.LatestRunBlocksActiveIndex,
	}
	resp.LatestJob = latestJob
}

// NoopRepoStatusFetcher is the default impl when the server isn't
// wired with a real one. Always returns ErrRepoStatusNotFound so
// the route surfaces a clean 404 in dev mode instead of pretending
// every repo has empty status.
type NoopRepoStatusFetcher struct{}

func (NoopRepoStatusFetcher) Fetch(_ context.Context, _, _ int32, _ string) (RepoStatusResponse, error) {
	return RepoStatusResponse{}, ErrRepoStatusNotFound
}

func (s *Server) repoStatusFetcher() RepoStatusFetcher {
	if s.cfg.RepoStatusFetcher != nil {
		return s.cfg.RepoStatusFetcher
	}
	return NoopRepoStatusFetcher{}
}

func (s *Server) handleGetOrgRepoStatus(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.reposLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	idStr := r.PathValue("id")
	idParsed, parseErr := strconv.ParseInt(idStr, 10, 32)
	if parseErr != nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidQueryParams,
			Message:    "Repo id must be an integer.",
		}, s.reposLogger)
		return
	}
	repoID := int32(idParsed)

	fetcher := s.repoStatusFetcher()
	// P.3c: `?branch=X` drives the per-branch BranchStatus on
	// the response. Empty leaves the field omitted, matching
	// legacy `requestedBranch ? buildBranchIndexStatus(...) :
	// undefined` (route.ts:218).
	requestedBranch := r.URL.Query().Get("branch")
	resp, err := fetcher.Fetch(r.Context(), authCtx.Org.ID, repoID, requestedBranch)
	if err != nil {
		if errors.Is(err, ErrRepoStatusNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Repo not found.",
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("repo status fetch failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	encoded, err := json.Marshal(resp)
	if err != nil {
		s.reposLogger.Error("encode repo status response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
