package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrRepoNotFound is returned by tenant-scoped repo lookups when
// the repo does not exist in the org or is no longer attached to
// any active connection.
var ErrRepoNotFound = errors.New("db: repo not found")

// SearchRepoRow is the tenant-scoped repo metadata projection used
// by the headless search response. It deliberately carries only
// display/link fields; authorization and index selection happen
// before the search engine is called.
type SearchRepoRow struct {
	ID              int32
	Name            string
	DisplayName     *string
	CodeHostType    string
	WebURL          *string
	DefaultBranch   *string
	IndexedAt       *time.Time
	Metadata        []byte
	LatestJobType   *string
	LatestJobStatus *string
}

// RepoReadRow is the tenant-scoped repo projection used by MCP
// file-read tools. CloneURL + CodeHostType are only used to
// resolve the local checkout path; credentials and remote auth
// never leave the indexing path.
type RepoReadRow struct {
	ID              int32
	OrgID           int32
	Name            string
	DisplayName     *string
	CloneURL        string
	CodeHostType    string
	WebURL          *string
	DefaultBranch   *string
	IndexedAt       *time.Time
	Metadata        []byte
	LatestJobType   *string
	LatestJobStatus *string
}

const searchRepoProjection = `SELECT r.id, r.name, r."displayName", r."external_codeHostType"::text, r."webUrl", r."defaultBranch", r."indexedAt", r.metadata, j.type::text, j.status::text `
const searchRepoFrom = `FROM "Repo" r LEFT JOIN LATERAL (SELECT type, status FROM "RepoIndexingJob" WHERE "repoId" = r.id ORDER BY "createdAt" DESC NULLS LAST, id DESC LIMIT 1) j ON TRUE `
const searchRepoActiveWhere = `AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")`
const listOrgSearchReposQuery = searchRepoProjection + searchRepoFrom + `WHERE r."orgId" = $1 AND (r.id = ANY($2::int[]) OR r.name = ANY($3::text[])) ` + searchRepoActiveWhere
const listOrgSearchPolicyReposQuery = searchRepoProjection + searchRepoFrom + `WHERE r."orgId" = $1 AND (cardinality($2::text[]) = 0 OR r.name = ANY($2::text[])) ` + searchRepoActiveWhere
const getOrgRepoForReadQuery = `SELECT r.id, r."orgId", r.name, r."displayName", COALESCE(r."cloneUrl", ''), COALESCE(r."external_codeHostType"::text, ''), r."webUrl", r."defaultBranch", r."indexedAt", r.metadata, j.type::text, j.status::text FROM "Repo" r LEFT JOIN LATERAL (SELECT type, status FROM "RepoIndexingJob" WHERE "repoId" = r.id ORDER BY "createdAt" DESC NULLS LAST, id DESC LIMIT 1) j ON TRUE WHERE r."orgId" = $1 AND r.name = $2 AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")`

// ListOrgSearchRepos resolves search-result repo ids/names back to
// metadata inside a single org. The org predicate is the critical
// tenant boundary: same repo names can exist in different orgs, and
// this lookup must never resolve globally by name.
func (q *Queries) ListOrgSearchRepos(ctx context.Context, orgID int32, repoIDs []int32, repoNames []string) ([]SearchRepoRow, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	if repoIDs == nil {
		repoIDs = []int32{}
	}
	if repoNames == nil {
		repoNames = []string{}
	}
	if len(repoIDs) == 0 && len(repoNames) == 0 {
		return []SearchRepoRow{}, nil
	}
	rows, err := q.db.Query(ctx, listOrgSearchReposQuery, orgID, repoIDs, repoNames)
	if err != nil {
		return nil, fmt.Errorf("db: ListOrgSearchRepos: %w", err)
	}
	defer rows.Close()

	out := make([]SearchRepoRow, 0, len(repoIDs)+len(repoNames))
	for rows.Next() {
		var r SearchRepoRow
		if err := rows.Scan(&r.ID, &r.Name, &r.DisplayName, &r.CodeHostType, &r.WebURL, &r.DefaultBranch, &r.IndexedAt, &r.Metadata, &r.LatestJobType, &r.LatestJobStatus); err != nil {
			return nil, fmt.Errorf("db: ListOrgSearchRepos: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListOrgSearchRepos: rows: %w", err)
	}
	return out, nil
}

// ListOrgSearchPolicyRepos resolves active repos whose branch
// policy can constrain a raw search before Zoekt dispatch. Passing
// an empty repoNames slice returns every active repo in the org;
// passing names applies the same tenant-scoped active-repo gate.
func (q *Queries) ListOrgSearchPolicyRepos(ctx context.Context, orgID int32, repoNames []string) ([]SearchRepoRow, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	if repoNames == nil {
		repoNames = []string{}
	}
	rows, err := q.db.Query(ctx, listOrgSearchPolicyReposQuery, orgID, repoNames)
	if err != nil {
		return nil, fmt.Errorf("db: ListOrgSearchPolicyRepos: %w", err)
	}
	defer rows.Close()

	out := make([]SearchRepoRow, 0, len(repoNames))
	for rows.Next() {
		var r SearchRepoRow
		if err := rows.Scan(&r.ID, &r.Name, &r.DisplayName, &r.CodeHostType, &r.WebURL, &r.DefaultBranch, &r.IndexedAt, &r.Metadata, &r.LatestJobType, &r.LatestJobStatus); err != nil {
			return nil, fmt.Errorf("db: ListOrgSearchPolicyRepos: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListOrgSearchPolicyRepos: rows: %w", err)
	}
	return out, nil
}

// GetOrgRepoForRead resolves one active repo by name within a
// single org. The org predicate plus active RepoToConnection check
// matches the repo-list/search surfaces and protects same-name
// repos across tenants.
func (q *Queries) GetOrgRepoForRead(ctx context.Context, orgID int32, repoName string) (RepoReadRow, error) {
	if orgID <= 0 {
		return RepoReadRow{}, ErrInvalidOrgID
	}
	if repoName == "" {
		return RepoReadRow{}, fmt.Errorf("db: GetOrgRepoForRead: repo name is required")
	}
	var r RepoReadRow
	err := q.db.QueryRow(ctx, getOrgRepoForReadQuery, orgID, repoName).Scan(
		&r.ID,
		&r.OrgID,
		&r.Name,
		&r.DisplayName,
		&r.CloneURL,
		&r.CodeHostType,
		&r.WebURL,
		&r.DefaultBranch,
		&r.IndexedAt,
		&r.Metadata,
		&r.LatestJobType,
		&r.LatestJobStatus,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RepoReadRow{}, ErrRepoNotFound
	}
	if err != nil {
		return RepoReadRow{}, fmt.Errorf("db: GetOrgRepoForRead: %w", err)
	}
	return r, nil
}
