// Refcheck queries that back the DELETE /api/secrets/{key} handler.
// Each row's config column is a Postgres JSONB blob: pgx scans it as
// []byte and we json.Unmarshal into a generic any so
// secretrefs.Collect can walk it without a typed schema.
//
// the deleteMany at line 87-91.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

// ConfigOwner is one row of a Connection / OrgLanguageModel select.
// Both tables share the same {name, config-json} shape that the
// DELETE handler needs for the refcheck diagnostic.
type ConfigOwner struct {
	Name   string
	Config any // decoded JSON: map[string]any | []any | scalar
}

const listOrgConnectionsForRefcheckQuery = `SELECT name, config FROM "Connection" WHERE "orgId" = $1`
const listOrgLanguageModelsForRefcheckQuery = `SELECT name, config FROM "OrgLanguageModel" WHERE "orgId" = $1`
const deleteOrgSecretQuery = `DELETE FROM "OrgSecret" WHERE "orgId" = $1 AND key = $2`

// ListOrgConnectionsForRefcheck returns the name + decoded config
// JSON for every Connection in the org. The DELETE handler walks
// each config for nested secretRef references and refuses the
// delete if any reference matches the target key.
func (q *Queries) ListOrgConnectionsForRefcheck(ctx context.Context, orgID int32) ([]ConfigOwner, error) {
	return q.listConfigOwners(ctx, orgID, listOrgConnectionsForRefcheckQuery, "ListOrgConnectionsForRefcheck")
}

// ListOrgLanguageModelsForRefcheck is the same shape over the
// OrgLanguageModel table.
func (q *Queries) ListOrgLanguageModelsForRefcheck(ctx context.Context, orgID int32) ([]ConfigOwner, error) {
	return q.listConfigOwners(ctx, orgID, listOrgLanguageModelsForRefcheckQuery, "ListOrgLanguageModelsForRefcheck")
}

// listConfigOwners is the shared row-iteration body. Both refcheck
// queries return identical {name, jsonb} projections so the helper
// avoids two near-duplicates.
func (q *Queries) listConfigOwners(ctx context.Context, orgID int32, query, opName string) ([]ConfigOwner, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	rows, err := q.db.Query(ctx, query, orgID)
	if err != nil {
		return nil, fmt.Errorf("db: %s: %w", opName, err)
	}
	defer rows.Close()

	out := make([]ConfigOwner, 0)
	for rows.Next() {
		var (
			name    string
			cfgJSON []byte
		)
		if err := rows.Scan(&name, &cfgJSON); err != nil {
			return nil, fmt.Errorf("db: %s: scan: %w", opName, err)
		}
		var decoded any
		// Empty / NULL configs decode as nil — JSONB columns
		// holding SQL NULL stay nil rather than becoming `{}`.
		if len(cfgJSON) > 0 {
			if err := json.Unmarshal(cfgJSON, &decoded); err != nil {
				return nil, fmt.Errorf("db: %s: decode row %q config: %w", opName, name, err)
			}
		}
		out = append(out, ConfigOwner{Name: name, Config: decoded})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: %s: rows: %w", opName, err)
	}
	return out, nil
}

// ConnectionSyncJobRow is the projection the status handler
// exposes for each sync-job row.
type ConnectionSyncJobRow struct {
	ID              string
	Status          string
	CreatedAt       time.Time
	UpdatedAt       time.Time
	CompletedAt     *time.Time
	ErrorMessage    *string
	WarningMessages []string
}

// listConnectionSyncJobsQuery joins through "Connection" so the
// org-scope predicate is enforced inside the SQL itself. This is
// defence-in-depth: even if a caller forgets to do a meta check
// first, the query cannot return rows belonging to another tenant.
const listConnectionSyncJobsQuery = `SELECT j.id, j.status, j."createdAt", j."updatedAt", j."completedAt", j."errorMessage", j."warningMessages" FROM "ConnectionSyncJob" j JOIN "Connection" c ON c.id = j."connectionId" WHERE c.id = $1 AND c."orgId" = $2 ORDER BY j."createdAt" DESC LIMIT $3`

// ListConnectionSyncJobs returns up to limit most-recent jobs for
// the connection scoped to the given org, newest first. Returns a
// non-nil empty slice on zero rows so callers JSON-encode as []
// not null.
func (q *Queries) ListConnectionSyncJobs(ctx context.Context, orgID, connectionID, limit int32) ([]ConnectionSyncJobRow, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	if connectionID <= 0 {
		return nil, ErrConnectionNotFound
	}
	if limit <= 0 {
		return nil, fmt.Errorf("db: ListConnectionSyncJobs: limit must be positive")
	}
	rows, err := q.db.Query(ctx, listConnectionSyncJobsQuery, connectionID, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: ListConnectionSyncJobs: %w", err)
	}
	defer rows.Close()
	out := make([]ConnectionSyncJobRow, 0)
	for rows.Next() {
		var r ConnectionSyncJobRow
		if err := rows.Scan(&r.ID, &r.Status, &r.CreatedAt, &r.UpdatedAt, &r.CompletedAt, &r.ErrorMessage, &r.WarningMessages); err != nil {
			return nil, fmt.Errorf("db: ListConnectionSyncJobs: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListConnectionSyncJobs: rows: %w", err)
	}
	return out, nil
}

// countConnectionReposQuery joins through "Connection" to enforce
// the org-scope predicate at the SQL layer (defence-in-depth).
const countConnectionReposQuery = `SELECT COUNT(*) FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE c.id = $1 AND c."orgId" = $2`

// CountConnectionRepos returns how many repos are linked to the
// connection within the given org. Used by the status handler's
// repo-count rollup.
func (q *Queries) CountConnectionRepos(ctx context.Context, orgID, connectionID int32) (int32, error) {
	if orgID <= 0 {
		return 0, ErrInvalidOrgID
	}
	if connectionID <= 0 {
		return 0, ErrConnectionNotFound
	}
	var n int32
	if err := q.db.QueryRow(ctx, countConnectionReposQuery, connectionID, orgID).Scan(&n); err != nil {
		return 0, fmt.Errorf("db: CountConnectionRepos: %w", err)
	}
	return n, nil
}

// ConnectionMetaRow is the projection the branches handler needs:
// id + name + connectionType + config + the two timestamps the
// response echoes. Separate from ConnectionListRow because the
// branches endpoint does not need the enforcement / isDeclarative
// flags and DOES need updatedAt (which the list projection skips).
type ConnectionMetaRow struct {
	ID             int32
	Name           string
	ConnectionType string
	Config         any
	SyncedAt       *time.Time
	UpdatedAt      time.Time
}

const getOrgConnectionMetaQuery = `SELECT id, name, "connectionType", config, "syncedAt", "updatedAt" FROM "Connection" WHERE id = $1 AND "orgId" = $2`

// GetOrgConnectionMeta fetches a connection's identity + config +
// timestamps scoped to the org. Returns ErrConnectionNotFound on
// miss so the handler can map to 404.
func (q *Queries) GetOrgConnectionMeta(ctx context.Context, orgID, connectionID int32) (ConnectionMetaRow, error) {
	if orgID <= 0 {
		return ConnectionMetaRow{}, ErrInvalidOrgID
	}
	if connectionID <= 0 {
		return ConnectionMetaRow{}, ErrConnectionNotFound
	}
	var (
		row     ConnectionMetaRow
		cfgJSON []byte
	)
	if err := q.db.QueryRow(ctx, getOrgConnectionMetaQuery, connectionID, orgID).Scan(
		&row.ID, &row.Name, &row.ConnectionType, &cfgJSON, &row.SyncedAt, &row.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConnectionMetaRow{}, ErrConnectionNotFound
		}
		return ConnectionMetaRow{}, fmt.Errorf("db: GetOrgConnectionMeta: %w", err)
	}
	if len(cfgJSON) > 0 {
		if err := json.Unmarshal(cfgJSON, &row.Config); err != nil {
			return ConnectionMetaRow{}, fmt.Errorf("db: GetOrgConnectionMeta: decode config: %w", err)
		}
	}
	return row, nil
}

// ConnectionListRow is the projection the GET /api/connections
// handler needs. Separate from ConnectionMetaRow because the list
// response shape carries enforcement + isDeclarative flags and
// does NOT echo the updatedAt timestamp.
type ConnectionListRow struct {
	UpdatedAt                        time.Time
	ID                               int32
	Name                             string
	ConnectionType                   string
	Config                           any
	IsDeclarative                    bool
	SyncedAt                         *time.Time
	EnforcePermissions               bool
	EnforcePermissionsForPublicRepos bool
}

const listOrgConnectionsForReadQuery = `SELECT id, name, "connectionType", config, "isDeclarative", "syncedAt", "enforcePermissions", "enforcePermissionsForPublicRepos" FROM "Connection" WHERE "orgId" = $1 ORDER BY "updatedAt" DESC`

// ListOrgConnectionsForRead returns the connection-list projection
// the GET /api/connections handler echoes back (with config redacted
// by the api layer before responding). ORDER BY updatedAt DESC
func (q *Queries) ListOrgConnectionsForRead(ctx context.Context, orgID int32) ([]ConnectionListRow, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	rows, err := q.db.Query(ctx, listOrgConnectionsForReadQuery, orgID)
	if err != nil {
		return nil, fmt.Errorf("db: ListOrgConnectionsForRead: %w", err)
	}
	defer rows.Close()
	out := make([]ConnectionListRow, 0)
	for rows.Next() {
		var (
			r       ConnectionListRow
			cfgJSON []byte
		)
		if err := rows.Scan(&r.ID, &r.Name, &r.ConnectionType, &cfgJSON, &r.IsDeclarative, &r.SyncedAt, &r.EnforcePermissions, &r.EnforcePermissionsForPublicRepos); err != nil {
			return nil, fmt.Errorf("db: ListOrgConnectionsForRead: scan: %w", err)
		}
		if len(cfgJSON) > 0 {
			if err := json.Unmarshal(cfgJSON, &r.Config); err != nil {
				return nil, fmt.Errorf("db: ListOrgConnectionsForRead: decode row %q config: %w", r.Name, err)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListOrgConnectionsForRead: rows: %w", err)
	}
	return out, nil
}

// UpsertOrgConnectionParams is the argument bag for an
// INSERT-or-UPDATE on the Connection table. The handler decodes
// the incoming JSON, runs BindToOrg over the config, then
// populates this struct.
type UpsertOrgConnectionParams struct {
	OrgID                            int32
	Name                             string
	ConnectionType                   string
	Config                           any
	EnforcePermissions               bool
	EnforcePermissionsForPublicRepos bool
	ResetSync                        bool
}

// ErrEmptyConnectionName / ErrEmptyConnectionType are the
// boundary-guard sentinels for the upsert write path. The handler
// distinguishes these from generic DB errors when building the
// 400 response.
var (
	ErrEmptyConnectionName = errors.New("db: connection name argument is required")
	ErrEmptyConnectionType = errors.New("db: connection type argument is required")
)

// upsertOrgConnectionQuery is a single statement INSERT...ON
// CONFLICT...DO UPDATE keyed by (orgId, name). On conflict we
// refresh config / connectionType / enforcement flags and reset
// isDeclarative to false. syncedAt is conditionally nulled when
// ResetSync is true so a sync can be scheduled against fresh
// state.
const upsertOrgConnectionQuery = `INSERT INTO "Connection" ("orgId", name, "connectionType", config, "isDeclarative", "enforcePermissions", "enforcePermissionsForPublicRepos", "createdAt", "updatedAt") VALUES ($1, $2, $3, $4, false, $5, $6, NOW(), NOW()) ON CONFLICT (name, "orgId") DO UPDATE SET "connectionType" = EXCLUDED."connectionType", config = EXCLUDED.config, "isDeclarative" = false, "enforcePermissions" = EXCLUDED."enforcePermissions", "enforcePermissionsForPublicRepos" = EXCLUDED."enforcePermissionsForPublicRepos", "syncedAt" = CASE WHEN $7 THEN NULL ELSE "Connection"."syncedAt" END, "updatedAt" = NOW() RETURNING id, name, "connectionType", config, "isDeclarative", "syncedAt", "enforcePermissions", "enforcePermissionsForPublicRepos", "updatedAt"`

// UpsertOrgConnection inserts a new (orgId, name) Connection row
// or updates the existing one's mutable columns. Returns the row
// in the same projection the listing handler echoes back so the
// caller can build the response without a follow-up SELECT.
func (q *Queries) UpsertOrgConnection(ctx context.Context, p UpsertOrgConnectionParams) (ConnectionListRow, error) {
	if p.OrgID <= 0 {
		return ConnectionListRow{}, ErrInvalidOrgID
	}
	if p.Name == "" {
		return ConnectionListRow{}, ErrEmptyConnectionName
	}
	if p.ConnectionType == "" {
		return ConnectionListRow{}, ErrEmptyConnectionType
	}
	cfgBytes, err := json.Marshal(p.Config)
	if err != nil {
		return ConnectionListRow{}, fmt.Errorf("db: UpsertOrgConnection: marshal config: %w", err)
	}
	var (
		row     ConnectionListRow
		cfgJSON []byte
	)
	if err := q.db.QueryRow(ctx, upsertOrgConnectionQuery,
		p.OrgID, p.Name, p.ConnectionType, cfgBytes,
		p.EnforcePermissions, p.EnforcePermissionsForPublicRepos,
		p.ResetSync,
	).Scan(
		&row.ID, &row.Name, &row.ConnectionType, &cfgJSON,
		&row.IsDeclarative, &row.SyncedAt,
		&row.EnforcePermissions, &row.EnforcePermissionsForPublicRepos,
		&row.UpdatedAt,
	); err != nil {
		return ConnectionListRow{}, fmt.Errorf("db: UpsertOrgConnection: %w", err)
	}
	if len(cfgJSON) > 0 {
		if err := json.Unmarshal(cfgJSON, &row.Config); err != nil {
			return ConnectionListRow{}, fmt.Errorf("db: UpsertOrgConnection: decode returning config: %w", err)
		}
	}
	return row, nil
}

// ConnectionExistsInOrg returns true when (orgID, connectionID)
// matches an existing Connection row. Used by handlers that need
// to enforce cross-tenant scoping but do not need any column
// values — a narrow `SELECT 1` covers it without paying the cost
// of decoding the full config JSONB.
const connectionExistsInOrgQuery = `SELECT 1 FROM "Connection" WHERE id = $1 AND "orgId" = $2`

func (q *Queries) ConnectionExistsInOrg(ctx context.Context, orgID, connectionID int32) (bool, error) {
	if orgID <= 0 {
		return false, ErrInvalidOrgID
	}
	if connectionID <= 0 {
		return false, nil
	}
	var one int
	err := q.db.QueryRow(ctx, connectionExistsInOrgQuery, connectionID, orgID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("db: ConnectionExistsInOrg: %w", err)
	}
	return true, nil
}

// GetOrgConnectionForUpdate returns the full Connection row for
// the supplied (orgID, connectionID) pair so the PATCH handler can
// read the current state, merge in the partial patch, and write
// the result through UpsertOrgConnection. Returns
// ErrConnectionNotFound when no row matches.
const getOrgConnectionForUpdateQuery = `SELECT id, name, "connectionType", config, "isDeclarative", "syncedAt", "enforcePermissions", "enforcePermissionsForPublicRepos" FROM "Connection" WHERE id = $1 AND "orgId" = $2`

// GetOrgConnectionForUpdate fetches the row used as the merge-base
// for a PATCH. Strict org-scoping in the WHERE clause: a request
// authenticated to org A cannot read a connection owned by org B.
func (q *Queries) GetOrgConnectionForUpdate(ctx context.Context, orgID, connectionID int32) (ConnectionListRow, error) {
	if orgID <= 0 {
		return ConnectionListRow{}, ErrInvalidOrgID
	}
	if connectionID <= 0 {
		return ConnectionListRow{}, ErrConnectionNotFound
	}
	var (
		row     ConnectionListRow
		cfgJSON []byte
	)
	if err := q.db.QueryRow(ctx, getOrgConnectionForUpdateQuery, connectionID, orgID).Scan(
		&row.ID, &row.Name, &row.ConnectionType, &cfgJSON,
		&row.IsDeclarative, &row.SyncedAt,
		&row.EnforcePermissions, &row.EnforcePermissionsForPublicRepos,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConnectionListRow{}, ErrConnectionNotFound
		}
		return ConnectionListRow{}, fmt.Errorf("db: GetOrgConnectionForUpdate: %w", err)
	}
	if len(cfgJSON) > 0 {
		if err := json.Unmarshal(cfgJSON, &row.Config); err != nil {
			return ConnectionListRow{}, fmt.Errorf("db: GetOrgConnectionForUpdate: decode config: %w", err)
		}
	}
	return row, nil
}

// CheckOrgConnectionNameAvailable confirms no row in the org
// already uses the requested name (excluding the row being
// updated). Returns ErrConnectionNameConflict on collision.
const checkOrgConnectionNameQuery = `SELECT id FROM "Connection" WHERE "orgId" = $1 AND name = $2 AND id <> $3`

// ErrConnectionNameConflict surfaces a PATCH name change that
// collides with an existing connection in the same org.
var ErrConnectionNameConflict = errors.New("db: connection name already in use")

// CheckOrgConnectionNameAvailable returns nil when the name is
// free in the org (ignoring the supplied excludeID) and
// ErrConnectionNameConflict when another row already owns it.
func (q *Queries) CheckOrgConnectionNameAvailable(ctx context.Context, orgID int32, name string, excludeID int32) error {
	if orgID <= 0 {
		return ErrInvalidOrgID
	}
	if name == "" {
		return ErrEmptyConnectionName
	}
	var existingID int32
	err := q.db.QueryRow(ctx, checkOrgConnectionNameQuery, orgID, name, excludeID).Scan(&existingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("db: CheckOrgConnectionNameAvailable: %w", err)
	}
	return ErrConnectionNameConflict
}

// DeleteOrgConnection removes a Connection row scoped to the org.
// Returns ErrConnectionNotFound when no row matches the (orgId, id)
// pair — the handler converts to 404 via the RowsAffected check.
//
// SQL:
//
//	DELETE FROM "Connection" WHERE id = $1 AND "orgId" = $2
const deleteOrgConnectionQuery = `DELETE FROM "Connection" WHERE id = $1 AND "orgId" = $2`

// ErrConnectionNotFound is returned by DeleteOrgConnection when no
// row matched the scoped delete.
var ErrConnectionNotFound = errors.New("db: connection not found in org")

// DeleteOrgConnection deletes the connection row identified by
// (orgID, connectionID). Returns ErrConnectionNotFound when the
// row does not exist.
func (q *Queries) DeleteOrgConnection(ctx context.Context, orgID int32, connectionID int32) error {
	if orgID <= 0 {
		return ErrInvalidOrgID
	}
	if connectionID <= 0 {
		return ErrConnectionNotFound
	}
	tag, err := q.db.Exec(ctx, deleteOrgConnectionQuery, connectionID, orgID)
	if err != nil {
		return fmt.Errorf("db: DeleteOrgConnection: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConnectionNotFound
	}
	return nil
}

// OrgStatusRollup is the scalar projection the status handler
// emits. A repo is "active" iff at least one Connection references
// it via the RepoToConnection link table; a connection is "synced"
// iff syncedAt is non-null; sync-job and index-job counts are
// scoped by status. Repo-indexing counts are gated on
// Repo.latestIndexingJobStatus matching the per-status filter so
// each active repo contributes at most one row per status bucket.
type OrgStatusRollup struct {
	RepoCount               int32
	IndexedRepoCount        int32
	ConnectionCount         int32
	SyncedConnectionCount   int32
	PendingSyncJobs         int32
	InProgressSyncJobs      int32
	FailedSyncJobs          int32
	PendingRepoIndexJobs    int32
	InProgressRepoIndexJobs int32
	FailedRepoIndexJobs     int32
}

// RecentFailedConnectionSyncJobRow is the per-row projection for
// the most-recent FAILED sync jobs across the org. The connection
// columns are included via a JOIN so the status handler can emit
// the nested {connection: {id, name, connectionType}} shape
// without a second round-trip.
type RecentFailedConnectionSyncJobRow struct {
	ID             string
	ErrorMessage   *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ConnectionID   int32
	ConnectionName string
	ConnectionType string
}

const listRecentFailedConnectionSyncJobsQuery = `SELECT j.id, j."errorMessage", j."createdAt", j."updatedAt", c.id, c.name, c."connectionType" FROM "ConnectionSyncJob" j JOIN "Connection" c ON c.id = j."connectionId" WHERE c."orgId" = $1 AND j.status = 'FAILED' ORDER BY j."updatedAt" DESC LIMIT $2`

// ListRecentFailedConnectionSyncJobs returns up to limit
// most-recently-updated FAILED sync jobs across the org's
// connections. Returns a non-nil empty slice on zero rows so
// callers JSON-encode as [] not null.
func (q *Queries) ListRecentFailedConnectionSyncJobs(ctx context.Context, orgID, limit int32) ([]RecentFailedConnectionSyncJobRow, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	if limit <= 0 {
		return nil, fmt.Errorf("db: ListRecentFailedConnectionSyncJobs: limit must be positive")
	}
	rows, err := q.db.Query(ctx, listRecentFailedConnectionSyncJobsQuery, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: ListRecentFailedConnectionSyncJobs: %w", err)
	}
	defer rows.Close()
	out := make([]RecentFailedConnectionSyncJobRow, 0, limit)
	for rows.Next() {
		var r RecentFailedConnectionSyncJobRow
		if err := rows.Scan(&r.ID, &r.ErrorMessage, &r.CreatedAt, &r.UpdatedAt, &r.ConnectionID, &r.ConnectionName, &r.ConnectionType); err != nil {
			return nil, fmt.Errorf("db: ListRecentFailedConnectionSyncJobs: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListRecentFailedConnectionSyncJobs: rows: %w", err)
	}
	return out, nil
}

// RecentFailedRepoIndexingJobRow is the per-row projection for the
// most-recent FAILED repo-indexing jobs across the org. The repo
// columns ride along via a JOIN so the status handler can emit the
// nested {repo: {id, name}} shape without a second round-trip.
type RecentFailedRepoIndexingJobRow struct {
	ID           string
	ErrorMessage *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	RepoID       int32
	RepoName     string
}

const listRecentFailedRepoIndexingJobsQuery = `SELECT j.id, j."errorMessage", j."createdAt", j."updatedAt", r.id, r.name FROM "RepoIndexingJob" j JOIN "Repo" r ON r.id = j."repoId" WHERE r."orgId" = $1 AND j.type = 'INDEX' AND j.status = 'FAILED' AND r."latestIndexingJobStatus" = 'FAILED' AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId") ORDER BY j."updatedAt" DESC LIMIT $2`

// ListRecentFailedRepoIndexingJobs returns up to limit
// most-recently-updated FAILED INDEX-type jobs for active repos
// in the org. The latestIndexingJobStatus = 'FAILED' filter
// guarantees each repo contributes at most its most recent failed
// run. Returns a non-nil empty slice on zero rows so callers
// JSON-encode as [] not null.
func (q *Queries) ListRecentFailedRepoIndexingJobs(ctx context.Context, orgID, limit int32) ([]RecentFailedRepoIndexingJobRow, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	if limit <= 0 {
		return nil, fmt.Errorf("db: ListRecentFailedRepoIndexingJobs: limit must be positive")
	}
	rows, err := q.db.Query(ctx, listRecentFailedRepoIndexingJobsQuery, orgID, limit)
	if err != nil {
		return nil, fmt.Errorf("db: ListRecentFailedRepoIndexingJobs: %w", err)
	}
	defer rows.Close()
	out := make([]RecentFailedRepoIndexingJobRow, 0, limit)
	for rows.Next() {
		var r RecentFailedRepoIndexingJobRow
		if err := rows.Scan(&r.ID, &r.ErrorMessage, &r.CreatedAt, &r.UpdatedAt, &r.RepoID, &r.RepoName); err != nil {
			return nil, fmt.Errorf("db: ListRecentFailedRepoIndexingJobs: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListRecentFailedRepoIndexingJobs: rows: %w", err)
	}
	return out, nil
}

// countOrgRepoStatusQuery returns both the active-repo count and
// the active-indexed-repo count in a single round-trip using
// FILTER aggregates.
const countOrgRepoStatusQuery = `SELECT COUNT(DISTINCT r.id), COUNT(DISTINCT r.id) FILTER (WHERE r."indexedAt" IS NOT NULL) FROM "Repo" r JOIN "RepoToConnection" rc ON rc."repoId" = r.id JOIN "Connection" c ON c.id = rc."connectionId" AND c."orgId" = r."orgId" WHERE r."orgId" = $1`

// countOrgConnectionStatusQuery returns total + synced connection
// counts for the org in a single round-trip.
const countOrgConnectionStatusQuery = `SELECT COUNT(*), COUNT(*) FILTER (WHERE "syncedAt" IS NOT NULL) FROM "Connection" WHERE "orgId" = $1`

// countOrgConnectionSyncJobsByStatusQuery returns counts for the
// three terminal statuses in one round-trip. Rows arrive as
// (status, count) pairs the caller maps into the rollup struct.
const countOrgConnectionSyncJobsByStatusQuery = `SELECT j.status, COUNT(*) FROM "ConnectionSyncJob" j JOIN "Connection" c ON c.id = j."connectionId" WHERE c."orgId" = $1 AND j.status IN ('PENDING', 'IN_PROGRESS', 'FAILED') GROUP BY j.status`

// countOrgRepoIndexJobsByStatusQuery counts INDEX-type repo
// indexing jobs grouped by status, gated to active repos
// (those with at least one connection) and to the latest job for
// each repo (via Repo.latestIndexingJobStatus). Returns
// (status, count) rows for PENDING/IN_PROGRESS/FAILED.
const countOrgRepoIndexJobsByStatusQuery = `SELECT j.status, COUNT(*) FROM "RepoIndexingJob" j JOIN "Repo" r ON r.id = j."repoId" WHERE r."orgId" = $1 AND j.type = 'INDEX' AND j.status IN ('PENDING', 'IN_PROGRESS', 'FAILED') AND r."latestIndexingJobStatus" = j.status AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId") GROUP BY j.status`

// GetOrgStatusRollup runs the three scalar count queries for the
// org and assembles them into the rollup struct. The three sub-
// queries have no data dependency and are dispatched in parallel
// via errgroup so the endpoint's p99 is bounded by the slowest
// query (not the sum). Any single failure cancels the siblings.
func (q *Queries) GetOrgStatusRollup(ctx context.Context, orgID int32) (OrgStatusRollup, error) {
	if orgID <= 0 {
		return OrgStatusRollup{}, ErrInvalidOrgID
	}
	var rollup OrgStatusRollup
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := q.db.QueryRow(gctx, countOrgRepoStatusQuery, orgID).Scan(&rollup.RepoCount, &rollup.IndexedRepoCount); err != nil {
			return fmt.Errorf("db: GetOrgStatusRollup: repo status: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := q.db.QueryRow(gctx, countOrgConnectionStatusQuery, orgID).Scan(&rollup.ConnectionCount, &rollup.SyncedConnectionCount); err != nil {
			return fmt.Errorf("db: GetOrgStatusRollup: connection status: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		rows, err := q.db.Query(gctx, countOrgConnectionSyncJobsByStatusQuery, orgID)
		if err != nil {
			return fmt.Errorf("db: GetOrgStatusRollup: sync-job counts: %w", err)
		}
		defer rows.Close()
		var pending, inProgress, failed int32
		for rows.Next() {
			var status string
			var n int32
			if err := rows.Scan(&status, &n); err != nil {
				return fmt.Errorf("db: GetOrgStatusRollup: scan sync-job count: %w", err)
			}
			switch status {
			case "PENDING":
				pending = n
			case "IN_PROGRESS":
				inProgress = n
			case "FAILED":
				failed = n
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("db: GetOrgStatusRollup: sync-job rows: %w", err)
		}
		rollup.PendingSyncJobs = pending
		rollup.InProgressSyncJobs = inProgress
		rollup.FailedSyncJobs = failed
		return nil
	})
	g.Go(func() error {
		rows, err := q.db.Query(gctx, countOrgRepoIndexJobsByStatusQuery, orgID)
		if err != nil {
			return fmt.Errorf("db: GetOrgStatusRollup: index-job counts: %w", err)
		}
		defer rows.Close()
		var pending, inProgress, failed int32
		for rows.Next() {
			var status string
			var n int32
			if err := rows.Scan(&status, &n); err != nil {
				return fmt.Errorf("db: GetOrgStatusRollup: scan index-job count: %w", err)
			}
			switch status {
			case "PENDING":
				pending = n
			case "IN_PROGRESS":
				inProgress = n
			case "FAILED":
				failed = n
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("db: GetOrgStatusRollup: index-job rows: %w", err)
		}
		rollup.PendingRepoIndexJobs = pending
		rollup.InProgressRepoIndexJobs = inProgress
		rollup.FailedRepoIndexJobs = failed
		return nil
	})
	if err := g.Wait(); err != nil {
		return OrgStatusRollup{}, err
	}
	return rollup, nil
}

// DeleteOrgSecret removes the (orgId, key) row from OrgSecret.
// key}})` — zero rows affected is success (idempotent delete).
func (q *Queries) DeleteOrgSecret(ctx context.Context, orgID int32, key string) error {
	if orgID <= 0 {
		return ErrInvalidOrgID
	}
	if key == "" {
		return ErrEmptySecretKey
	}
	if _, err := q.db.Exec(ctx, deleteOrgSecretQuery, orgID, key); err != nil {
		return fmt.Errorf("db: DeleteOrgSecret: %w", err)
	}
	return nil
}
