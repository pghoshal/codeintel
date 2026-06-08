package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const getOrgRepoPrimaryConnectionMetaQuery = `SELECT c.id, c.name, c."connectionType", c.config, c."syncedAt", c."updatedAt" FROM "Repo" r JOIN "RepoToConnection" rc ON rc."repoId" = r.id JOIN "Connection" c ON c.id = rc."connectionId" AND c."orgId" = r."orgId" WHERE r.id = $1 AND r."orgId" = $2 ORDER BY c."updatedAt" DESC, c.id ASC LIMIT 1`

// GetOrgRepoPrimaryConnectionMeta returns the active connection that
// controls branch policy for a repo row. The org predicate is on the
// Repo row and the joined Connection row so an org A caller cannot
// discover an org B repo or connection by id.
func (q *Queries) GetOrgRepoPrimaryConnectionMeta(ctx context.Context, orgID, repoID int32) (ConnectionMetaRow, error) {
	if orgID <= 0 {
		return ConnectionMetaRow{}, ErrInvalidOrgID
	}
	if repoID <= 0 {
		return ConnectionMetaRow{}, ErrConnectionNotFound
	}
	var (
		row     ConnectionMetaRow
		cfgJSON []byte
	)
	if err := q.db.QueryRow(ctx, getOrgRepoPrimaryConnectionMetaQuery, repoID, orgID).Scan(
		&row.ID, &row.Name, &row.ConnectionType, &cfgJSON, &row.SyncedAt, &row.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConnectionMetaRow{}, ErrConnectionNotFound
		}
		return ConnectionMetaRow{}, fmt.Errorf("db: GetOrgRepoPrimaryConnectionMeta: %w", err)
	}
	if len(cfgJSON) > 0 {
		if err := json.Unmarshal(cfgJSON, &row.Config); err != nil {
			return ConnectionMetaRow{}, fmt.Errorf("db: GetOrgRepoPrimaryConnectionMeta: decode config: %w", err)
		}
	}
	return row, nil
}

const updateOrgRepoBranchPolicyMetadataQuery = `
UPDATE "Repo"
SET metadata = CASE
	WHEN $3::jsonb = '[]'::jsonb THEN COALESCE(metadata, '{}'::jsonb) - 'branches'
	ELSE jsonb_set(COALESCE(metadata, '{}'::jsonb), '{branches}', $3::jsonb, true)
END,
"updatedAt" = NOW()
WHERE id = $1 AND "orgId" = $2`

// UpdateOrgRepoBranchPolicyMetadata mirrors an Atom branch-policy edit into
// Repo.metadata so status, search, and MCP branch validation stop exposing
// stale branch indexes immediately, even when the caller chooses sync:false.
func (q *Queries) UpdateOrgRepoBranchPolicyMetadata(ctx context.Context, orgID, repoID int32, branches []string) error {
	if orgID <= 0 {
		return ErrInvalidOrgID
	}
	if repoID <= 0 {
		return ErrRepoNotFound
	}
	if branches == nil {
		branches = []string{}
	}
	encoded, err := json.Marshal(branches)
	if err != nil {
		return fmt.Errorf("db: UpdateOrgRepoBranchPolicyMetadata: marshal branches: %w", err)
	}
	tag, err := q.db.Exec(ctx, updateOrgRepoBranchPolicyMetadataQuery, repoID, orgID, encoded)
	if err != nil {
		return fmt.Errorf("db: UpdateOrgRepoBranchPolicyMetadata: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRepoNotFound
	}
	return nil
}

const getOrgRepoPrimaryConnectionForUpdateQuery = `SELECT c.id, c.name, c."connectionType", c.config, c."isDeclarative", c."syncedAt", c."enforcePermissions", c."enforcePermissionsForPublicRepos" FROM "Repo" r JOIN "RepoToConnection" rc ON rc."repoId" = r.id JOIN "Connection" c ON c.id = rc."connectionId" AND c."orgId" = r."orgId" WHERE r.id = $1 AND r."orgId" = $2 ORDER BY c."updatedAt" DESC, c.id ASC LIMIT 1`

// GetOrgRepoPrimaryConnectionForUpdate returns the same merge-base
// shape as GetOrgConnectionForUpdate, but addresses it through a
// repo id. Used by Atom's repo-level branch dropdown API.
func (q *Queries) GetOrgRepoPrimaryConnectionForUpdate(ctx context.Context, orgID, repoID int32) (ConnectionListRow, error) {
	if orgID <= 0 {
		return ConnectionListRow{}, ErrInvalidOrgID
	}
	if repoID <= 0 {
		return ConnectionListRow{}, ErrConnectionNotFound
	}
	var (
		row     ConnectionListRow
		cfgJSON []byte
	)
	if err := q.db.QueryRow(ctx, getOrgRepoPrimaryConnectionForUpdateQuery, repoID, orgID).Scan(
		&row.ID, &row.Name, &row.ConnectionType, &cfgJSON,
		&row.IsDeclarative, &row.SyncedAt,
		&row.EnforcePermissions, &row.EnforcePermissionsForPublicRepos,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConnectionListRow{}, ErrConnectionNotFound
		}
		return ConnectionListRow{}, fmt.Errorf("db: GetOrgRepoPrimaryConnectionForUpdate: %w", err)
	}
	if len(cfgJSON) > 0 {
		if err := json.Unmarshal(cfgJSON, &row.Config); err != nil {
			return ConnectionListRow{}, fmt.Errorf("db: GetOrgRepoPrimaryConnectionForUpdate: decode config: %w", err)
		}
	}
	return row, nil
}
