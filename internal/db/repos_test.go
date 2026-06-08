package db

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// Regex versions of the constants for pgxmock's regexp matcher.
// pgxmock treats the expected query as a regex by default; we escape
// the literal LIKE wildcards and the parameterised $ marker.
const reposProjectionRegex = `SELECT r\.id, r\.name, r\."displayName", r\."indexedAt", r\.metadata, r\."latestIndexingJobStatus"::text, r\."external_codeHostType", r\."webUrl", r\."imageUrl", r\."pushedAt", r\."defaultBranch", r\."isFork", r\."isArchived", j\.id, j\.type, j\.status, j\."createdAt", j\."completedAt", j\."errorMessage", s\.id, s\.kind, s\.status, s\.revision, s\."commitHash", s\."languageCount", s\."symbolCount", s\."occurrenceCount", s\."relationshipCount", s\."indexedAt", s\."errorMessage", g\.id, g\.provider, g\.status, g\."sourceRevision", g\."commitHash", g\."graphSpace", g\."workspaceId", g\."schemaVersion", g\."builderVersion", g\."vertexCount", g\."edgeCount", g\."anchorCount", g\."linkedEdgeCount", g\."indexedAt", g\."supersededAt", g\."deleteAfter", g\."errorMessage" `
const reposLateralJoinRegex = ` LEFT JOIN LATERAL \(SELECT id, type, status, "createdAt", "completedAt", "errorMessage" FROM "RepoIndexingJob" WHERE "repoId" = r\.id ORDER BY "createdAt" DESC NULLS LAST, id DESC LIMIT 1\) j ON TRUE`
const reposLateralScipRegex = ` LEFT JOIN LATERAL \(SELECT id, kind, status, revision, "commitHash", "languageCount", "symbolCount", "occurrenceCount", "relationshipCount", "indexedAt", "errorMessage" FROM "CodeIntelIndex" WHERE "repoId" = r\.id AND "orgId" = r\."orgId" ORDER BY "updatedAt" DESC LIMIT 1\) s ON TRUE`
const reposLateralCodeGraphRegex = ` LEFT JOIN LATERAL \(SELECT id, provider, status, "sourceRevision", "commitHash", "graphSpace", "workspaceId", "schemaVersion", "builderVersion", "vertexCount", "edgeCount", "anchorCount", "linkedEdgeCount", "indexedAt", "supersededAt", "deleteAfter", "errorMessage" FROM "CodeGraphIndex" WHERE "repoId" = r\.id AND "orgId" = r\."orgId" ORDER BY CASE WHEN status = 'READY'::"CodeGraphIndexStatus" AND "sourceRevision" IS NOT NULL AND "supersededAt" IS NULL AND "deleteAfter" IS NULL THEN 0 WHEN status = 'READY'::"CodeGraphIndexStatus" THEN 1 ELSE 2 END, "updatedAt" DESC LIMIT 1\) g ON TRUE`
const activeRepoExistsRegex = `EXISTS \(SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c\.id = rc\."connectionId" WHERE rc\."repoId" = r\.id AND c\."orgId" = r\."orgId"\)`
const reposWhereRegex = ` WHERE r\."orgId" = \$1 AND ` + activeRepoExistsRegex + ` AND \(\$2 = '' OR r\.name ILIKE '%' \|\| \$2 \|\| '%'\)`
const reposFromWhereRegex = `FROM "Repo" r` + reposLateralJoinRegex + reposLateralScipRegex + reposLateralCodeGraphRegex + reposWhereRegex
const expectedListReposNameAscQuery = reposProjectionRegex + reposFromWhereRegex + ` ORDER BY r\.name ASC LIMIT \$3 OFFSET \$4`
const expectedListReposNameDescQuery = reposProjectionRegex + reposFromWhereRegex + ` ORDER BY r\.name DESC LIMIT \$3 OFFSET \$4`
const expectedListReposIndexedAtDescQuery = reposProjectionRegex + reposFromWhereRegex + ` ORDER BY r\."indexedAt" DESC NULLS LAST, r\.name ASC LIMIT \$3 OFFSET \$4`
const expectedListReposIndexedAtAscQuery = reposProjectionRegex + reposFromWhereRegex + ` ORDER BY r\."indexedAt" ASC NULLS LAST, r\.name ASC LIMIT \$3 OFFSET \$4`
const expectedCountOrgReposQuery = `SELECT COUNT\(DISTINCT r\.id\) FROM "Repo" r WHERE r\."orgId" = \$1 AND ` + activeRepoExistsRegex + ` AND \(\$2 = '' OR r\.name ILIKE '%' \|\| \$2 \|\| '%'\)`

func ptrString(s string) *string     { return &s }
func ptrTime(t time.Time) *time.Time { return &t }

func TestListOrgRepos_CodeGraphLateralPrefersUsableReadyDuringReindex(t *testing.T) {
	if !strings.Contains(listOrgReposLateralCodeGraph, `THEN 0 WHEN status = 'READY'`) {
		t.Fatalf("code graph lateral must prefer the current READY graph before newer building rows: %s", listOrgReposLateralCodeGraph)
	}
	if !strings.Contains(listOrgReposLateralCodeGraph, `ELSE 2 END, "updatedAt" DESC`) {
		t.Fatalf("code graph lateral must still return the newest non-ready graph when no READY graph exists: %s", listOrgReposLateralCodeGraph)
	}
}

// reposMockColumns is the column list every list-rows mock returns.
// Keeping it in one place stops a column-addition slice from having
// to edit a dozen test mocks individually.
var reposMockColumns = []string{
	"id", "name", "displayName", "indexedAt",
	"metadata", "latestIndexingJobStatus",
	"external_codeHostType", "webUrl", "imageUrl", "pushedAt", "defaultBranch",
	"isFork", "isArchived",
	"job_id", "job_type", "job_status", "job_createdAt", "job_completedAt", "job_errorMessage",
	"scip_id", "scip_kind", "scip_status", "scip_revision", "scip_commitHash",
	"scip_languageCount", "scip_symbolCount", "scip_occurrenceCount", "scip_relationshipCount",
	"scip_indexedAt", "scip_errorMessage",
	"cg_id", "cg_provider", "cg_status", "cg_sourceRevision", "cg_commitHash",
	"cg_graphSpace", "cg_workspaceId", "cg_schemaVersion", "cg_builderVersion",
	"cg_vertexCount", "cg_edgeCount", "cg_anchorCount", "cg_linkedEdgeCount",
	"cg_indexedAt", "cg_supersededAt", "cg_deleteAfter", "cg_errorMessage",
}

// addBasicRepoRow appends a row with sensible defaults for the
// extended columns (all nullable scalars NULL, bool flags FALSE),
// no latest-job (every LATERAL job column NULL — the no-match
// branch) and no SCIP index (every LATERAL scip column NULL — also
// the no-match branch). Tests that exercise specific columns set
// them explicitly via AddRow with the full positional argument list.
func addBasicRepoRow(rows *pgxmock.Rows, id int32, name string, displayName *string, indexedAt *time.Time) *pgxmock.Rows {
	return rows.AddRow(
		id, name, displayName, indexedAt,
		[]byte(`{}`), nil,
		nil, nil, nil, nil, nil, false, false,
		nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
}

// TestListOrgRepos_NameAscHappyPath locks the default page: org 7,
// no filter, sort=name asc, page size 30, offset 0. The projection
// matches the row scan order: id, name, displayName, indexedAt.
func TestListOrgRepos_NameAscHappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	indexedAt := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	mockRows := pgxmock.NewRows(reposMockColumns).
		AddRow(int32(101), "alpha", ptrString("Alpha Display"), ptrTime(indexedAt),
			[]byte(`{}`), nil,
			ptrString("github"), ptrString("https://github.com/x/alpha"), nil,
			ptrTime(time.Date(2025, 6, 1, 9, 0, 0, 0, time.UTC)),
			ptrString("main"), false, false,
			nil, nil, nil, nil, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
		AddRow(int32(102), "beta", nil, nil,
			[]byte(`{}`), nil,
			nil, nil, nil, nil, nil, true, true,
			nil, nil, nil, nil, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
			nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	mock.ExpectQuery(expectedListReposNameAscQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(mockRows)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID:     7,
		Query:     "",
		Skip:      0,
		Take:      30,
		Sort:      ReposSortName,
		Direction: ReposSortAsc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].RepoID != 101 || rows[0].RepoName != "alpha" {
		t.Errorf("row[0]: got %+v", rows[0])
	}
	if rows[0].RepoDisplayName == nil || *rows[0].RepoDisplayName != "Alpha Display" {
		t.Errorf("row[0].RepoDisplayName: got %v", rows[0].RepoDisplayName)
	}
	if rows[0].IndexedAt == nil || !rows[0].IndexedAt.Equal(indexedAt) {
		t.Errorf("row[0].IndexedAt: got %v", rows[0].IndexedAt)
	}
	if rows[0].CodeHostType == nil || *rows[0].CodeHostType != "github" {
		t.Errorf("row[0].CodeHostType: got %v", rows[0].CodeHostType)
	}
	if rows[0].WebUrl == nil || *rows[0].WebUrl != "https://github.com/x/alpha" {
		t.Errorf("row[0].WebUrl: got %v", rows[0].WebUrl)
	}
	if rows[0].DefaultBranch == nil || *rows[0].DefaultBranch != "main" {
		t.Errorf("row[0].DefaultBranch: got %v", rows[0].DefaultBranch)
	}
	if rows[0].IsFork || rows[0].IsArchived {
		t.Errorf("row[0] flags: got fork=%v archived=%v, want both false", rows[0].IsFork, rows[0].IsArchived)
	}
	if rows[1].RepoDisplayName != nil {
		t.Errorf("row[1].RepoDisplayName: expected nil for NULL column, got %v", rows[1].RepoDisplayName)
	}
	if rows[1].IndexedAt != nil {
		t.Errorf("row[1].IndexedAt: expected nil for NULL column, got %v", rows[1].IndexedAt)
	}
	if rows[1].CodeHostType != nil || rows[1].WebUrl != nil || rows[1].ImageUrl != nil {
		t.Errorf("row[1] nullables: got codeHostType=%v webUrl=%v imageUrl=%v, want all nil", rows[1].CodeHostType, rows[1].WebUrl, rows[1].ImageUrl)
	}
	if !rows[1].IsFork || !rows[1].IsArchived {
		t.Errorf("row[1] flags: got fork=%v archived=%v, want both true", rows[1].IsFork, rows[1].IsArchived)
	}
}

// TestListOrgRepos_EmptyOrgReturnsEmptySlice confirms a zero-row
// result is a non-nil empty slice. JSON encoding then emits `[]`
// rather than `null` — the handler relies on this invariant.
func TestListOrgRepos_EmptyOrgReturnsEmptySlice(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedListReposNameAscQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(pgxmock.NewRows(reposMockColumns))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Take: 30, Sort: ReposSortName, Direction: ReposSortAsc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if got == nil {
		t.Fatalf("expected empty slice, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(got))
	}
}

// TestListOrgRepos_FilterPassesThrough confirms the query string
// is bound as $2 (the ILIKE filter clause). The handler relies on
// this — without it, the `q=foo` request param would silently
// return the unfiltered set.
func TestListOrgRepos_FilterPassesThrough(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedListReposNameAscQuery).
		WithArgs(int32(7), "foo", int32(10), int32(20)).
		WillReturnRows(addBasicRepoRow(pgxmock.NewRows(reposMockColumns), 201, "foobar", nil, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Query: "foo", Skip: 20, Take: 10, Sort: ReposSortName, Direction: ReposSortAsc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if len(rows) != 1 || rows[0].RepoName != "foobar" {
		t.Errorf("got %+v", rows)
	}
}

// TestListOrgRepos_NameDescSelectsCorrectQuery confirms the
// direction switch routes to the desc-ordered statement (not asc).
func TestListOrgRepos_NameDescSelectsCorrectQuery(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mockRows := pgxmock.NewRows(reposMockColumns)
	addBasicRepoRow(mockRows, 102, "beta", nil, nil)
	addBasicRepoRow(mockRows, 101, "alpha", nil, nil)
	mock.ExpectQuery(expectedListReposNameDescQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(mockRows)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Take: 30, Sort: ReposSortName, Direction: ReposSortDesc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if rows[0].RepoName != "beta" {
		t.Errorf("desc order broken: first row %q", rows[0].RepoName)
	}
}

// TestListOrgRepos_IndexedAtDescSelectsCorrectQuery exercises the
// indexedAt branch end-to-end so the four-statement switch can't
// regress without a test catching it.
func TestListOrgRepos_IndexedAtDescSelectsCorrectQuery(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	t1 := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2024, 12, 31, 9, 0, 0, 0, time.UTC)
	mockRows := pgxmock.NewRows(reposMockColumns)
	addBasicRepoRow(mockRows, 301, "recent", nil, ptrTime(t1))
	addBasicRepoRow(mockRows, 302, "older", nil, ptrTime(t2))
	mock.ExpectQuery(expectedListReposIndexedAtDescQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(mockRows)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Take: 30, Sort: ReposSortIndexedAt, Direction: ReposSortDesc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if rows[0].RepoName != "recent" || rows[1].RepoName != "older" {
		t.Errorf("indexedAt desc order broken: %+v", rows)
	}
}

// TestListOrgRepos_IndexedAtAscSelectsCorrectQuery confirms the
// fourth ORDER BY branch (asc + indexedAt) actually routes to its
// own pre-built statement. Without this, an inverted asc/desc
// switch in pickListReposQuery would silently route asc requests
// to the desc statement.
func TestListOrgRepos_IndexedAtAscSelectsCorrectQuery(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	mockRows := pgxmock.NewRows(reposMockColumns)
	addBasicRepoRow(mockRows, 401, "old", nil, ptrTime(t1))
	addBasicRepoRow(mockRows, 402, "new", nil, ptrTime(t2))
	mock.ExpectQuery(expectedListReposIndexedAtAscQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(mockRows)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Take: 30, Sort: ReposSortIndexedAt, Direction: ReposSortAsc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if rows[0].RepoName != "old" || rows[1].RepoName != "new" {
		t.Errorf("indexedAt asc order broken: %+v", rows)
	}
}

// TestListOrgRepos_BoundaryGuards locks every rejection at the
// pgx-call boundary: invalid orgID, non-positive take, negative
// skip, and an unsupported (sort, direction) pair. Each must
// return a typed error WITHOUT round-tripping to the database
// (mock has no ExpectQuery set, so a stray call would fail the
// "all expectations were met" check).
func TestListOrgRepos_BoundaryGuards(t *testing.T) {
	tests := []struct {
		name   string
		params ListOrgReposParams
		wantIs error
	}{
		{"orgID=0", ListOrgReposParams{OrgID: 0, Take: 30, Sort: ReposSortName, Direction: ReposSortAsc}, ErrInvalidOrgID},
		{"orgID negative", ListOrgReposParams{OrgID: -5, Take: 30, Sort: ReposSortName, Direction: ReposSortAsc}, ErrInvalidOrgID},
		{"take=0", ListOrgReposParams{OrgID: 7, Take: 0, Sort: ReposSortName, Direction: ReposSortAsc}, nil},
		{"take negative", ListOrgReposParams{OrgID: 7, Take: -1, Sort: ReposSortName, Direction: ReposSortAsc}, nil},
		{"skip negative", ListOrgReposParams{OrgID: 7, Skip: -1, Take: 30, Sort: ReposSortName, Direction: ReposSortAsc}, nil},
		{"unsupported sort", ListOrgReposParams{OrgID: 7, Take: 30, Sort: "random", Direction: ReposSortAsc}, nil},
		{"unsupported direction", ListOrgReposParams{OrgID: 7, Take: 30, Sort: ReposSortName, Direction: "random"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock pool: %v", err)
			}
			defer mock.Close()
			q := &Queries{db: mock}
			_, err = q.ListOrgRepos(context.Background(), tt.params)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("want sentinel %v, got %v", tt.wantIs, err)
			}
		})
	}
}

// TestListOrgRepos_ExtendedColumnsRoundtrip locks the extended-
// scalar projection: a row with every nullable column populated AND
// every boolean flag set decodes into the matching RepoListRow
// fields. This is the column-by-column scan-order check — a
// reordered Scan() call regresses this test before any handler
// test gets a chance.
func TestListOrgRepos_ExtendedColumnsRoundtrip(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	indexedAt := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	pushedAt := time.Date(2025, 4, 15, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(expectedListReposNameAscQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(pgxmock.NewRows(reposMockColumns).
			AddRow(int32(501), "full",
				ptrString("Full Display"), ptrTime(indexedAt),
				[]byte(`{"indexedRevisions":["refs/heads/trunk"]}`), nil,
				ptrString("gitlab"), ptrString("https://gitlab.com/x/full"),
				ptrString("https://cdn/img.png"), ptrTime(pushedAt),
				ptrString("trunk"), true, true,
				nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Take: 30, Sort: ReposSortName, Direction: ReposSortAsc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.RepoID != 501 || r.RepoName != "full" {
		t.Errorf("identity: got %+v", r)
	}
	if r.CodeHostType == nil || *r.CodeHostType != "gitlab" {
		t.Errorf("CodeHostType: got %v", r.CodeHostType)
	}
	if r.WebUrl == nil || *r.WebUrl != "https://gitlab.com/x/full" {
		t.Errorf("WebUrl: got %v", r.WebUrl)
	}
	if r.ImageUrl == nil || *r.ImageUrl != "https://cdn/img.png" {
		t.Errorf("ImageUrl: got %v", r.ImageUrl)
	}
	if r.PushedAt == nil || !r.PushedAt.Equal(pushedAt) {
		t.Errorf("PushedAt: got %v", r.PushedAt)
	}
	if r.DefaultBranch == nil || *r.DefaultBranch != "trunk" {
		t.Errorf("DefaultBranch: got %v", r.DefaultBranch)
	}
	if !r.IsFork || !r.IsArchived {
		t.Errorf("flags: got fork=%v archived=%v, want both true", r.IsFork, r.IsArchived)
	}
}

// TestListOrgRepos_LatestJob_PresentAndAbsent locks the LATERAL
// JOIN scan: a Repo row with an emitted job populates LatestJob;
// a Repo row with all-NULL job columns leaves LatestJob nil. The
// scan path detects "no job" by the cuid primary-key column being
// nil, so a regression that flipped the sentinel to `Type` or
// `Status` would fail this test.
func TestListOrgRepos_LatestJob_PresentAndAbsent(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	createdAt := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	completedAt := time.Date(2025, 5, 1, 12, 5, 0, 0, time.UTC)
	mock.ExpectQuery(expectedListReposNameAscQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(pgxmock.NewRows(reposMockColumns).
			// Row 1: has a job row, no scip, no codeGraph.
			AddRow(int32(901), "with-job", nil, nil,
				[]byte(`{}`), nil,
				nil, nil, nil, nil, nil, false, false,
				ptrString("ckjob1"), ptrString("INDEX"), ptrString("COMPLETED"),
				ptrTime(createdAt), ptrTime(completedAt), ptrString("ok"),
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
			// Row 2: no job rows — LATERAL emits NULL on every job
			// column; same for scip and codeGraph.
			AddRow(int32(902), "no-job", nil, nil,
				[]byte(`{}`), nil,
				nil, nil, nil, nil, nil, false, false,
				nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Take: 30, Sort: ReposSortName, Direction: ReposSortAsc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].LatestJob == nil {
		t.Fatalf("row[0].LatestJob: expected populated, got nil")
	}
	job := rows[0].LatestJob
	if job.ID != "ckjob1" || job.Type != "INDEX" || job.Status != "COMPLETED" {
		t.Errorf("job identity: got %+v", job)
	}
	if !job.CreatedAt.Equal(createdAt) {
		t.Errorf("job.CreatedAt: got %v, want %v", job.CreatedAt, createdAt)
	}
	if job.CompletedAt == nil || !job.CompletedAt.Equal(completedAt) {
		t.Errorf("job.CompletedAt: got %v, want %v", job.CompletedAt, completedAt)
	}
	if job.ErrorMessage == nil || *job.ErrorMessage != "ok" {
		t.Errorf("job.ErrorMessage: got %v", job.ErrorMessage)
	}
	if rows[1].LatestJob != nil {
		t.Errorf("row[1].LatestJob: expected nil for no-match LATERAL, got %+v", rows[1].LatestJob)
	}
}

// TestListOrgRepos_LatestScip_PresentAndAbsent locks the second
// LATERAL JOIN scan path: a Repo with a populated CodeIntelIndex
// surfaces a non-nil LatestScip with the eleven scalars; a Repo
// with no CodeIntelIndex rows surfaces nil. The cuid primary-key
// nil sentinel mirrors the latestJob path.
func TestListOrgRepos_LatestScip_PresentAndAbsent(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	scipIndexedAt := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(expectedListReposNameAscQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(pgxmock.NewRows(reposMockColumns).
			// Row 1: has a CodeIntelIndex, no codeGraph.
			AddRow(int32(1001), "with-scip", nil, nil,
				[]byte(`{}`), nil,
				nil, nil, nil, nil, nil, false, false,
				nil, nil, nil, nil, nil, nil,
				ptrString("ckscip1"), ptrString("SCIP"), ptrString("READY"),
				ptrString("abc123"), ptrString("deadbeef"),
				ptrInt32(3), ptrInt32(1500), ptrInt32(8000), ptrInt32(400),
				ptrTime(scipIndexedAt), nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil).
			// Row 2: no CodeIntelIndex — LATERAL emits NULL on every
			// scip column; codeGraph also empty.
			AddRow(int32(1002), "no-scip", nil, nil,
				[]byte(`{}`), nil,
				nil, nil, nil, nil, nil, false, false,
				nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Take: 30, Sort: ReposSortName, Direction: ReposSortAsc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].LatestScip == nil {
		t.Fatalf("row[0].LatestScip: expected populated, got nil")
	}
	scip := rows[0].LatestScip
	if scip.ID != "ckscip1" || scip.Kind != "SCIP" || scip.Status != "READY" {
		t.Errorf("scip identity: got %+v", scip)
	}
	if scip.Revision != "abc123" || scip.CommitHash != "deadbeef" {
		t.Errorf("scip rev/hash: got %+v", scip)
	}
	if scip.LanguageCount != 3 || scip.SymbolCount != 1500 ||
		scip.OccurrenceCount != 8000 || scip.RelationshipCount != 400 {
		t.Errorf("scip counts: got %+v", scip)
	}
	if scip.IndexedAt == nil || !scip.IndexedAt.Equal(scipIndexedAt) {
		t.Errorf("scip.IndexedAt: got %v", scip.IndexedAt)
	}
	if scip.ErrorMessage != nil {
		t.Errorf("scip.ErrorMessage: want nil (LATERAL emitted NULL), got %v", scip.ErrorMessage)
	}
	if rows[1].LatestScip != nil {
		t.Errorf("row[1].LatestScip: expected nil for no-match LATERAL, got %+v", rows[1].LatestScip)
	}
}

// ptrInt32 wraps a literal int32 into a *int32 pointer for the
// pgxmock AddRow column values.
func ptrInt32(n int32) *int32 { return &n }

// TestListOrgRepos_LatestCodeGraph_PresentAndAbsent locks the
// third LATERAL JOIN scan path. A populated CodeGraphIndex row
// surfaces all 17 scalars on the wire; an absent row leaves
// LatestCodeGraph nil. Mirrors the latestJob / latestScip sentinel
// pattern.
func TestListOrgRepos_LatestCodeGraph_PresentAndAbsent(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	cgIndexedAt := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	cgSupersededAt := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	cgDeleteAfter := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(expectedListReposNameAscQuery).
		WithArgs(int32(7), "", int32(30), int32(0)).
		WillReturnRows(pgxmock.NewRows(reposMockColumns).
			// Row 1: has a CodeGraphIndex with every scalar populated.
			AddRow(int32(2001), "with-cg", nil, nil,
				[]byte(`{}`), nil,
				nil, nil, nil, nil, nil, false, false,
				nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				ptrString("ckcg1"), ptrString("NEBULA"), ptrString("READY"),
				ptrString("HEAD"), ptrString("deadbeef"),
				ptrString("codeintel"), ptrString("ws-1"),
				ptrInt32(2), ptrString("builder-v3"),
				ptrInt32(10000), ptrInt32(50000), ptrInt32(800), ptrInt32(3000),
				ptrTime(cgIndexedAt), ptrTime(cgSupersededAt), ptrTime(cgDeleteAfter), nil).
			// Row 2: no CodeGraphIndex — LATERAL emits NULL on every
			// codeGraph column.
			AddRow(int32(2002), "no-cg", nil, nil,
				[]byte(`{}`), nil,
				nil, nil, nil, nil, nil, false, false,
				nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	rows, err := q.ListOrgRepos(ctx, ListOrgReposParams{
		OrgID: 7, Take: 30, Sort: ReposSortName, Direction: ReposSortAsc,
	})
	if err != nil {
		t.Fatalf("ListOrgRepos: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].LatestCodeGraph == nil {
		t.Fatalf("row[0].LatestCodeGraph: expected populated, got nil")
	}
	cg := rows[0].LatestCodeGraph
	if cg.ID != "ckcg1" || cg.Provider != "NEBULA" || cg.Status != "READY" {
		t.Errorf("cg identity: got %+v", cg)
	}
	if cg.WorkspaceID != "ws-1" || cg.CommitHash != "deadbeef" {
		t.Errorf("cg workspace/hash: got %+v", cg)
	}
	// Lock the (SourceRevision, GraphSpace) *string pair so a Scan-
	// order swap between these two adjacent nullable strings can't
	// regress unnoticed at the db boundary.
	if cg.SourceRevision == nil || *cg.SourceRevision != "HEAD" {
		t.Errorf("cg.SourceRevision: got %v, want HEAD", cg.SourceRevision)
	}
	if cg.GraphSpace == nil || *cg.GraphSpace != "codeintel" {
		t.Errorf("cg.GraphSpace: got %v, want codeintel", cg.GraphSpace)
	}
	if cg.SchemaVersion != 2 || cg.BuilderVersion != "builder-v3" {
		t.Errorf("cg version: got %+v", cg)
	}
	if cg.VertexCount != 10000 || cg.EdgeCount != 50000 ||
		cg.AnchorCount != 800 || cg.LinkedEdgeCount != 3000 {
		t.Errorf("cg counts: got %+v", cg)
	}
	if cg.IndexedAt == nil || !cg.IndexedAt.Equal(cgIndexedAt) {
		t.Errorf("cg.IndexedAt: got %v", cg.IndexedAt)
	}
	if cg.SupersededAt == nil || !cg.SupersededAt.Equal(cgSupersededAt) {
		t.Errorf("cg.SupersededAt: got %v", cg.SupersededAt)
	}
	if cg.DeleteAfter == nil || !cg.DeleteAfter.Equal(cgDeleteAfter) {
		t.Errorf("cg.DeleteAfter: got %v", cg.DeleteAfter)
	}
	if rows[1].LatestCodeGraph != nil {
		t.Errorf("row[1].LatestCodeGraph: expected nil for no-match LATERAL, got %+v", rows[1].LatestCodeGraph)
	}
}

// TestCountOrgRepos_Happy locks the COUNT projection and parameter
// binding. The handler emits this number as the X-Total-Count
// response header so the assertion guards a wire-visible field.
func TestCountOrgRepos_Happy(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedCountOrgReposQuery).
		WithArgs(int32(7), "foo").
		WillReturnRows(pgxmock.NewRows([]string{"count"}).AddRow(int32(42)))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	n, err := q.CountOrgRepos(ctx, CountOrgReposParams{OrgID: 7, Query: "foo"})
	if err != nil {
		t.Fatalf("CountOrgRepos: %v", err)
	}
	if n != 42 {
		t.Errorf("got %d, want 42", n)
	}
}

// TestCountOrgRepos_InvalidOrgID rejects without a round-trip.
func TestCountOrgRepos_InvalidOrgID(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	q := &Queries{db: mock}
	_, err = q.CountOrgRepos(context.Background(), CountOrgReposParams{OrgID: 0, Query: ""})
	if !errors.Is(err, ErrInvalidOrgID) {
		t.Errorf("got %v, want ErrInvalidOrgID", err)
	}
}
