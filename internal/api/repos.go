// GET /api/repos
//
// Steps:
//
//  1. optional-auth middleware — anonymous-permitted; the audit emit
//     only fires when a non-anonymous identity is resolved (matching
//     the wire contract: `getAuditService().createAudit({...})` is
//     guarded by `if (user)`).
//  2. Parse query params (page, perPage, sort, direction, query)
//     with Zod-equivalent defaults/bounds; 400 on validation error.
//  3. Parallel ListOrgRepos + CountOrgRepos via errgroup so the
//     endpoint's p99 is bounded by the slower query.
//  4. Map db rows to the public {repoId, repoName, repoDisplayName?,
//     indexedAt?} projection. Optional fields use omitempty so an
//     absent value emits no JSON key.
//  5. Emit `user.listed_repos` audit event (skipped for anonymous).
//  6. Respond with the JSON array body + X-Total-Count header.
//
// Wire-divergence in this slice: the response body is the strict
// subset of the eventual full RepositoryQuery projection that the
// current Repo table can support. Follow-up slices add the
// codeIntelIndexes / codeGraphIndexes / jobs blocks plus the
// extended columns (codeHostType, webUrl, pushedAt, ...) as those
// tables land.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/pkg/audit"
	"codeintel/pkg/repoindexstatus"
	"golang.org/x/sync/errgroup"
)

// clientSourceHeader is the request header the audit metadata reads
// to record which client surface triggered the listing. Renamed
// from the legacy brand-prefixed value per the
// docs/codeintel-porting-plan.md §G.7 wire-rename decision.
const clientSourceHeader = "X-Codeintel-Client-Source"

// repoListItem is the per-row response shape. omitempty mirrors the
// wire schema's optional fields (`repoDisplayName`, `externalWebUrl`,
// `imageUrl`, `indexedAt`, `pushedAt`, `defaultBranch`); these are
// emitted only when the underlying DB column is non-NULL.
//
// Required fields per the wire schema are always present:
//   - `codeHostType` is z.nativeEnum-typed in the wire schema and
//     the prior schema marked the column NOT NULL. Existing rows
//     in this codeintel deployment migrated in without a value, so
//     a row with NULL surfaces as an empty string rather than
//     dropping the key — keeps the wire field required by the
//     schema present in every response.
//   - `webUrl` is always emitted. In this codeintel deployment the
//     value is the empty string (the headless mode contract: when
//     no UI host is wired the field's wire shape is `""`).
//   - `isFork` / `isArchived` are required booleans backed by
//     NOT NULL DEFAULT FALSE columns, so plain `bool` is correct.
//
// The pointer-on-string for optional fields lets an empty-string
// column value still produce `"<field>": ""` on the wire (matching
// the JS `value ?? undefined` semantics, which only drops null /
// undefined and keeps empty strings).
type repoListItem struct {
	CodeHostType           string             `json:"codeHostType"`
	RepoID                 int32              `json:"repoId"`
	RepoName               string             `json:"repoName"`
	WebUrl                 string             `json:"webUrl"`
	RepoDisplayName        *string            `json:"repoDisplayName,omitempty"`
	ExternalWebUrl         *string            `json:"externalWebUrl,omitempty"`
	ImageUrl               *string            `json:"imageUrl,omitempty"`
	IndexedAt              *iso8601MilliTime  `json:"indexedAt,omitempty"`
	PushedAt               *iso8601MilliTime  `json:"pushedAt,omitempty"`
	DefaultBranch          *string            `json:"defaultBranch,omitempty"`
	IndexStatus            string             `json:"indexStatus"`
	IndexStatusColor       string             `json:"indexStatusColor"`
	Indexed                bool               `json:"indexed"`
	IndexedRevisions       []string           `json:"indexedRevisions"`
	ActiveIndexStatus      string             `json:"activeIndexStatus"`
	ActiveIndexStatusColor string             `json:"activeIndexStatusColor"`
	ActiveIndexUsable      bool               `json:"activeIndexUsable"`
	LatestIndexRun         RepoLatestIndexRun `json:"latestIndexRun"`
	CodeIntel              repoCodeIntel      `json:"codeIntel"`
	LatestJob              *repoLatestJob     `json:"latestJob"`
	IsFork                 bool               `json:"isFork"`
	IsArchived             bool               `json:"isArchived"`
}

// repoCodeIntel is the nested `codeIntel` parent on the wire. Both
// children are nullable: `scip` is populated from the latest
// CodeIntelIndex row (or null when no row exists), `codeGraph`
// stays null in this slice and will land once the CodeGraphIndex
// table + projection ship.
type repoCodeIntel struct {
	Scip      *repoCodeIntelScip      `json:"scip"`
	CodeGraph *repoCodeIntelCodeGraph `json:"codeGraph"`
}

// repoCodeIntelScip is the per-row latest-CodeIntelIndex projection.
// Field order mirrors the wire schema's top-level scalar order
// (id / kind / status / revision / commitHash / languageCount /
// symbolCount / occurrenceCount / relationshipCount / indexedAt /
// errorMessage). The derived `projectCount` / `detectedLanguages` /
// `readyLanguages` / `skippedLanguages` / `failedLanguages` and the
// nested `languageIndexes` array land once CodeIntelLanguageIndex
// is ported.
type repoCodeIntelScip struct {
	ID                string            `json:"id"`
	Kind              string            `json:"kind"`
	Status            string            `json:"status"`
	Revision          string            `json:"revision"`
	CommitHash        string            `json:"commitHash"`
	LanguageCount     int32             `json:"languageCount"`
	SymbolCount       int32             `json:"symbolCount"`
	OccurrenceCount   int32             `json:"occurrenceCount"`
	RelationshipCount int32             `json:"relationshipCount"`
	IndexedAt         *iso8601MilliTime `json:"indexedAt"`
	ErrorMessage      *string           `json:"errorMessage"`
}

// repoCodeIntelCodeGraph is the per-row latest-CodeGraphIndex
// projection. Field order mirrors the wire schema sequence (id,
// provider, isActive, status, sourceRevision, commitHash,
// graphSpace, workspaceId, schemaVersion, builderVersion,
// vertexCount, edgeCount, anchorCount, linkedEdgeCount,
// activeRevisionCount, activeRevisions, indexedAt, supersededAt,
// deleteAfter, errorMessage).
//
// `isActive` / `activeRevisionCount` / `activeRevisions` are derived
// from the latest selected READY graph snapshot. `/api/repos/{id}/status`
// exposes the full CodeGraphRevision-backed history; the list route keeps
// a compact single-revision projection so indexed repos are not presented
// as graph-inactive in the Atom control-plane UI.
//
// Nullable wire fields (`sourceRevision`, `graphSpace`,
// `indexedAt`, `supersededAt`, `deleteAfter`, `errorMessage`) use
// pointer-with-explicit-null (no omitempty) so a NULL DB column
// emits JSON null rather than dropping the key. ActiveRevisions
// is a slice; an empty slice marshals to `[]` (not `null`).
type repoCodeIntelCodeGraph struct {
	ID                  string                        `json:"id"`
	Provider            string                        `json:"provider"`
	IsActive            bool                          `json:"isActive"`
	Status              string                        `json:"status"`
	SourceRevision      *string                       `json:"sourceRevision"`
	CommitHash          string                        `json:"commitHash"`
	GraphSpace          *string                       `json:"graphSpace"`
	WorkspaceID         string                        `json:"workspaceId"`
	SchemaVersion       int32                         `json:"schemaVersion"`
	BuilderVersion      string                        `json:"builderVersion"`
	VertexCount         int32                         `json:"vertexCount"`
	EdgeCount           int32                         `json:"edgeCount"`
	AnchorCount         int32                         `json:"anchorCount"`
	LinkedEdgeCount     int32                         `json:"linkedEdgeCount"`
	ActiveRevisionCount int32                         `json:"activeRevisionCount"`
	ActiveRevisions     []repoCodeGraphActiveRevision `json:"activeRevisions"`
	IndexedAt           *iso8601MilliTime             `json:"indexedAt"`
	SupersededAt        *iso8601MilliTime             `json:"supersededAt"`
	DeleteAfter         *iso8601MilliTime             `json:"deleteAfter"`
	ErrorMessage        *string                       `json:"errorMessage"`
}

// repoCodeGraphActiveRevision is the per-element shape of the
// `activeRevisions` array. The array itself is always present;
// each element carries the revision name, its commit hash, and
// the timestamp it was activated. Populated by the CodeGraphRevision
// table port in a follow-up slice.
type repoCodeGraphActiveRevision struct {
	Revision    string            `json:"revision"`
	CommitHash  string            `json:"commitHash"`
	ActivatedAt *iso8601MilliTime `json:"activatedAt"`
}

// repoLatestJob is the nested per-row most-recent RepoIndexingJob
// projection. The wire shape lists `latestJob` as nullable rather
// than optional: when no jobs exist the field is emitted as
// `"latestJob": null` (not absent). CompletedAt + ErrorMessage are
// nullable; the other four are non-null on the source row.
type repoLatestJob struct {
	ID           string            `json:"id"`
	Type         string            `json:"type"`
	Status       string            `json:"status"`
	CreatedAt    iso8601MilliTime  `json:"createdAt"`
	CompletedAt  *iso8601MilliTime `json:"completedAt"`
	ErrorMessage *string           `json:"errorMessage"`
}

// listReposParams is the parsed-and-validated query string. The
// defaults match the wire schema's `.default()` annotations exactly.
type listReposParams struct {
	page      int32
	perPage   int32
	sort      db.ReposSortField
	direction db.ReposSortDirection
	query     string
}

// defaultPerPage and maxPerPage mirror the wire schema's
// `.positive().max(100).default(30)` bounds on perPage. maxRepoListPage
// caps page so `(page-1)*perPage` cannot overflow int32 — without it
// a hostile `page=2^31` request would compute a negative skip and
// burn a 500 envelope.
const (
	defaultRepoListPage    int32 = 1
	defaultRepoListPerPage int32 = 30
	maxRepoListPerPage     int32 = 100
	maxRepoListPage        int32 = 1_000_000
)

// maxQueryLen caps the filter substring. The Postgres planner can
// handle long ILIKE inputs but the cost scales linearly; bounding
// the input keeps a single hostile request inside the per-request
// CPU budget. 256 chars is well past every real Repo.name length.
const maxQueryLen = 256

// maxClientSourceLen bounds the X-Codeintel-Client-Source header
// value that lands on every audit event's metadata. Without a cap a
// hostile caller can bloat each event up to net/http's
// MaxHeaderBytes (~1 MiB), which would balloon audit-store cost.
const maxClientSourceLen = 128

// dbCallTimeout bounds the parallel List + Count call. The /api/repos
// endpoint's p99 SLO is 50 ms — a stuck backend must not let one
// request burn the budget for everyone. The value is generous (5 s)
// because real Postgres queries against a well-indexed Repo table
// take low-ms; the timeout exists to catch backend hangs, not slow
// legitimate queries.
const dbCallTimeout = 5 * time.Second

// parseListReposParams parses + validates the query string. Returns
// nil ServiceError on success; on failure the returned envelope is
// the 400 response body the handler echoes back.
func parseListReposParams(q map[string][]string) (listReposParams, *ServiceError) {
	out := listReposParams{
		page:      defaultRepoListPage,
		perPage:   defaultRepoListPerPage,
		sort:      db.ReposSortName,
		direction: db.ReposSortAsc,
	}

	if raw, ok := firstQuery(q, "page"); ok {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n <= 0 || n > int64(maxRepoListPage) {
			return out, &ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidQueryParams,
				Message:    "page must be a positive integer.",
			}
		}
		out.page = int32(n)
	}
	if raw, ok := firstQuery(q, "perPage"); ok {
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || n <= 0 || n > int64(maxRepoListPerPage) {
			return out, &ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidQueryParams,
				Message:    "perPage must be a positive integer no greater than 100.",
			}
		}
		out.perPage = int32(n)
	}
	if raw, ok := firstQuery(q, "sort"); ok {
		switch raw {
		case "name":
			out.sort = db.ReposSortName
		case "pushed":
			out.sort = db.ReposSortPushedAt
		default:
			return out, &ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidQueryParams,
				Message:    "sort must be one of: name, pushed.",
			}
		}
	}
	if raw, ok := firstQuery(q, "direction"); ok {
		switch raw {
		case "asc":
			out.direction = db.ReposSortAsc
		case "desc":
			out.direction = db.ReposSortDesc
		default:
			return out, &ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidQueryParams,
				Message:    "direction must be one of: asc, desc.",
			}
		}
	}
	if raw, ok := firstQuery(q, "query"); ok {
		if len(raw) > maxQueryLen {
			return out, &ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidQueryParams,
				Message:    "query must be 256 characters or fewer.",
			}
		}
		out.query = raw
	}
	return out, nil
}

// firstQuery returns the first value for the named key together
// with whether the key was present (and non-empty). The map type
// matches net/url.Values without importing it at every call site.
func firstQuery(q map[string][]string, key string) (string, bool) {
	if vs := q[key]; len(vs) > 0 && vs[0] != "" {
		return vs[0], true
	}
	return "", false
}

// timestampFromRow wraps a nullable time.Time into a
// *iso8601MilliTime so json.Marshal emits no key for missing
// values via omitempty (the pointer is nil → key omitted) and the
// canonical 3-digit millisecond ISO-8601 UTC format when present.
func timestampFromRow(t *time.Time) *iso8601MilliTime {
	if t == nil {
		return nil
	}
	p := iso8601MilliTime(*t)
	return &p
}

// derefOrEmpty returns the dereferenced string or empty when nil.
// Used to project a nullable DB column onto a wire-required string
// field (codeHostType in the current schema — see the repoListItem
// docstring for the rationale).
func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// latestJobFromRow projects the optional RepoListJobRow sub-row
// onto the wire's `latestJob` field. A nil sub-row produces a nil
// pointer (json.Marshal then emits `"latestJob": null` — without
// omitempty the key is present even when null, matching the wire
// contract).
func latestJobFromRow(j *db.RepoListJobRow) *repoLatestJob {
	if j == nil {
		return nil
	}
	return &repoLatestJob{
		ID:           j.ID,
		Type:         j.Type,
		Status:       j.Status,
		CreatedAt:    iso8601MilliTime(j.CreatedAt),
		CompletedAt:  timestampFromRow(j.CompletedAt),
		ErrorMessage: j.ErrorMessage,
	}
}

func latestSummaryJobFromRow(j *db.RepoListJobRow) *repoindexstatus.LatestJob {
	if j == nil {
		return nil
	}
	return &repoindexstatus.LatestJob{
		ID:     j.ID,
		Type:   repoindexstatus.JobType(j.Type),
		Status: repoindexstatus.JobStatus(j.Status),
	}
}

func repoIndexSummaryFromRow(row *db.RepoListRow) repoindexstatus.RepoIndexSummary {
	repoInput := repoindexstatus.RepoInput{
		Metadata:                row.Metadata,
		DefaultBranch:           row.DefaultBranch,
		LatestIndexingJobStatus: row.LatestIndexingJobStatus,
	}
	if row.IndexedAt != nil {
		s := row.IndexedAt.Format(time.RFC3339Nano)
		repoInput.IndexedAt = &s
	}
	return repoindexstatus.BuildRepoIndexSummary(repoInput, latestSummaryJobFromRow(row.LatestJob))
}

// scipFromRow projects the optional RepoListScipRow sub-row onto
// the wire's `codeIntel.scip` field. A nil sub-row produces a nil
// pointer — json.Marshal then emits `"scip": null` (the parent
// `codeIntel` object always carries both `scip` and `codeGraph`
// keys; neither is omitempty).
func scipFromRow(s *db.RepoListScipRow) *repoCodeIntelScip {
	if s == nil {
		return nil
	}
	return &repoCodeIntelScip{
		ID:                s.ID,
		Kind:              s.Kind,
		Status:            s.Status,
		Revision:          s.Revision,
		CommitHash:        s.CommitHash,
		LanguageCount:     s.LanguageCount,
		SymbolCount:       s.SymbolCount,
		OccurrenceCount:   s.OccurrenceCount,
		RelationshipCount: s.RelationshipCount,
		IndexedAt:         timestampFromRow(s.IndexedAt),
		ErrorMessage:      s.ErrorMessage,
	}
}

// codeGraphFromRow projects the optional RepoListCodeGraphRow sub-
// row onto the wire's `codeIntel.codeGraph` field. The repo-list
// query only selects one latest CodeGraphIndex row, so the active
// revision projection is intentionally compact: a READY,
// non-superseded, non-deleting graph with a source revision emits
// that revision as active. The richer `/api/repos/{id}/status`
// endpoint expands CodeGraphRevision children.
func codeGraphFromRow(g *db.RepoListCodeGraphRow) *repoCodeIntelCodeGraph {
	if g == nil {
		return nil
	}
	activeRevisions := []repoCodeGraphActiveRevision{}
	if g.Status == "READY" && g.SupersededAt == nil && g.DeleteAfter == nil &&
		g.SourceRevision != nil && *g.SourceRevision != "" && g.CommitHash != "" {
		activeRevisions = append(activeRevisions, repoCodeGraphActiveRevision{
			Revision:    *g.SourceRevision,
			CommitHash:  g.CommitHash,
			ActivatedAt: timestampFromRow(g.IndexedAt),
		})
	}
	return &repoCodeIntelCodeGraph{
		ID:                  g.ID,
		Provider:            g.Provider,
		IsActive:            len(activeRevisions) > 0,
		Status:              g.Status,
		SourceRevision:      g.SourceRevision,
		CommitHash:          g.CommitHash,
		GraphSpace:          g.GraphSpace,
		WorkspaceID:         g.WorkspaceID,
		SchemaVersion:       g.SchemaVersion,
		BuilderVersion:      g.BuilderVersion,
		VertexCount:         g.VertexCount,
		EdgeCount:           g.EdgeCount,
		AnchorCount:         g.AnchorCount,
		LinkedEdgeCount:     g.LinkedEdgeCount,
		ActiveRevisionCount: int32(len(activeRevisions)),
		ActiveRevisions:     activeRevisions,
		IndexedAt:           timestampFromRow(g.IndexedAt),
		SupersededAt:        timestampFromRow(g.SupersededAt),
		DeleteAfter:         timestampFromRow(g.DeleteAfter),
		ErrorMessage:        g.ErrorMessage,
	}
}

// handleListOrgRepos is the request entry point. Layout mirrors
// connections/secrets handlers: resolve → validate → DB → respond.
func (s *Server) handleListOrgRepos(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveOptionalFromHeaders(r.Context(), r.Header, auth.OptionalAuthConfig{
		SingleTenantOrgID: s.cfg.SingleTenantOrgID,
		EncryptionKey:     s.cfg.EncryptionKey,
		Queries:           s.cfg.Queries,
	})
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.reposLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	params, perr := parseListReposParams(r.URL.Query())
	if perr != nil {
		writeServiceError(w, *perr, s.reposLogger)
		return
	}

	skip := (params.page - 1) * params.perPage

	dbCtx, cancel := context.WithTimeout(r.Context(), dbCallTimeout)
	defer cancel()

	var (
		rows  []db.RepoListRow
		total int32
	)
	g, gctx := errgroup.WithContext(dbCtx)
	g.Go(func() error {
		out, err := s.cfg.Queries.ListOrgRepos(gctx, db.ListOrgReposParams{
			OrgID:     authCtx.Org.ID,
			Query:     params.query,
			Skip:      skip,
			Take:      params.perPage,
			Sort:      params.sort,
			Direction: params.direction,
		})
		if err != nil {
			return err
		}
		rows = out
		return nil
	})
	g.Go(func() error {
		n, err := s.cfg.Queries.CountOrgRepos(gctx, db.CountOrgReposParams{
			OrgID: authCtx.Org.ID,
			Query: params.query,
		})
		if err != nil {
			return err
		}
		total = n
		return nil
	})
	if err := g.Wait(); err != nil {
		s.reposLogger.Error("ListOrgRepos/CountOrgRepos failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	items := make([]repoListItem, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		summary := repoIndexSummaryFromRow(row)
		items = append(items, repoListItem{
			CodeHostType:           derefOrEmpty(row.CodeHostType),
			RepoID:                 row.RepoID,
			RepoName:               row.RepoName,
			WebUrl:                 "",
			RepoDisplayName:        row.RepoDisplayName,
			ExternalWebUrl:         row.WebUrl,
			ImageUrl:               row.ImageUrl,
			IndexedAt:              timestampFromRow(row.IndexedAt),
			PushedAt:               timestampFromRow(row.PushedAt),
			DefaultBranch:          row.DefaultBranch,
			IndexStatus:            string(summary.Status),
			IndexStatusColor:       string(summary.Color),
			Indexed:                summary.Indexed,
			IndexedRevisions:       summary.IndexedRevisions,
			ActiveIndexStatus:      string(summary.ActiveIndexStatus),
			ActiveIndexStatusColor: string(summary.ActiveIndexStatusColor),
			ActiveIndexUsable:      summary.ActiveIndexUsable,
			LatestIndexRun: RepoLatestIndexRun{
				Status:            string(summary.LatestRunStatus),
				StatusColor:       string(summary.LatestRunStatusColor),
				BlocksActiveIndex: summary.LatestRunBlocksActiveIndex,
			},
			CodeIntel:  repoCodeIntel{Scip: scipFromRow(row.LatestScip), CodeGraph: codeGraphFromRow(row.LatestCodeGraph)},
			LatestJob:  latestJobFromRow(row.LatestJob),
			IsFork:     row.IsFork,
			IsArchived: row.IsArchived,
		})
	}

	encoded, err := json.Marshal(items)
	if err != nil {
		s.reposLogger.Error("encode repos response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	// Audit emit fires only for authenticated callers; anonymous
	// (single-tenant guest) listings are not recorded — guard
	// mirrors the wire contract's `if (user) { ... }` predicate.
	if authCtx.AuthSource != "anonymous" {
		source := r.Header.Get(clientSourceHeader)
		if len(source) > maxClientSourceLen {
			source = source[:maxClientSourceLen]
		}
		ev := audit.Event{
			Action:     "user.listed_repos",
			TargetID:   targetIDFromInt(authCtx.Org.ID),
			TargetType: audit.TargetOrg,
			OrgID:      authCtx.Org.ID,
		}
		ev.ActorID, ev.ActorType = auditActor(authCtx)
		if source != "" {
			ev.Metadata = map[string]any{"source": source}
		}
		s.emitAudit(r.Context(), ev)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Total-Count", strconv.FormatInt(int64(total), 10))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// Compile-time check: ReposQuerier is satisfied by AuthQuerier so
// callers passing api.Config.Queries don't need a per-route adapter.
var _ ReposQuerier = (AuthQuerier)(nil)
