package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const (
	AtomServiceUserID    = "codeintel-atom-control-plane"
	AtomServiceUserEmail = "atom-control-plane@codeintel.local"
)

var (
	ErrOrgDomainExists         = errors.New("db: org domain already exists")
	ErrAtomServiceUserConflict = errors.New("db: atom service principal email is used by another user")
)

type AtomWorkspaceTenantParams struct {
	WorkspaceID   string
	WorkspaceName string
	Domain        string
	APIKeyName    string
	APIKeyHash    string
}

type AtomWorkspaceTenant struct {
	ID                int32
	Name              string
	Domain            string
	AtomWorkspaceID   string
	AtomWorkspaceName string
}

func (q *Queries) UpsertAtomWorkspaceTenant(ctx context.Context, p AtomWorkspaceTenantParams) (AtomWorkspaceTenant, error) {
	if p.WorkspaceID == "" {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: workspace id is required")
	}
	if p.WorkspaceName == "" {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: workspace name is required")
	}
	if p.Domain == "" {
		return AtomWorkspaceTenant{}, ErrEmptyDomain
	}
	if p.APIKeyName == "" && p.APIKeyHash != "" {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: api key name is required")
	}

	tx, err := q.db.Begin(ctx)
	if err != nil {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var collidingUserID string
	err = tx.QueryRow(ctx, `SELECT id FROM "User" WHERE email = $1`, AtomServiceUserEmail).Scan(&collidingUserID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: service user email check: %w", err)
	}
	if err == nil && collidingUserID != AtomServiceUserID {
		return AtomWorkspaceTenant{}, ErrAtomServiceUserConflict
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO "User" (id, email, name, "createdAt", "updatedAt")
		VALUES ($1, $2, $3, NOW(), NOW())
		ON CONFLICT (id) DO UPDATE SET
			email = EXCLUDED.email,
			name = EXCLUDED.name,
			"updatedAt" = NOW()
	`, AtomServiceUserID, AtomServiceUserEmail, "Atom Control Plane"); err != nil {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: upsert service user: %w", err)
	}

	var existingWorkspaceOrgID int32
	err = tx.QueryRow(ctx, `SELECT id FROM "Org" WHERE "atomWorkspaceId" = $1`, p.WorkspaceID).Scan(&existingWorkspaceOrgID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: workspace lookup: %w", err)
	}
	hasExistingWorkspaceOrg := err == nil

	var existingDomainOrgID int32
	err = tx.QueryRow(ctx, `SELECT id FROM "Org" WHERE domain = $1`, p.Domain).Scan(&existingDomainOrgID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: domain lookup: %w", err)
	}
	if err == nil && (!hasExistingWorkspaceOrg || existingDomainOrgID != existingWorkspaceOrgID) {
		return AtomWorkspaceTenant{}, ErrOrgDomainExists
	}

	metadata, err := json.Marshal(map[string]any{
		"atom": map[string]any{
			"workspaceId":   p.WorkspaceID,
			"workspaceName": p.WorkspaceName,
		},
	})
	if err != nil {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: metadata marshal: %w", err)
	}

	var tenant AtomWorkspaceTenant
	if hasExistingWorkspaceOrg {
		err = tx.QueryRow(ctx, `
			UPDATE "Org"
			SET name = $2,
				domain = $3,
				"atomWorkspaceName" = $4,
				metadata = $5::jsonb,
				"updatedAt" = NOW()
			WHERE id = $1
			RETURNING id, name, domain, "atomWorkspaceId", "atomWorkspaceName"
		`, existingWorkspaceOrgID, p.WorkspaceName, p.Domain, p.WorkspaceName, metadata).
			Scan(&tenant.ID, &tenant.Name, &tenant.Domain, &tenant.AtomWorkspaceID, &tenant.AtomWorkspaceName)
	} else {
		err = tx.QueryRow(ctx, `
			INSERT INTO "Org" (name, domain, "atomWorkspaceId", "atomWorkspaceName", metadata, "createdAt", "updatedAt")
			VALUES ($1, $2, $3, $4, $5::jsonb, NOW(), NOW())
			RETURNING id, name, domain, "atomWorkspaceId", "atomWorkspaceName"
		`, p.WorkspaceName, p.Domain, p.WorkspaceID, p.WorkspaceName, metadata).
			Scan(&tenant.ID, &tenant.Name, &tenant.Domain, &tenant.AtomWorkspaceID, &tenant.AtomWorkspaceName)
	}
	if err != nil {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: upsert org: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO "UserToOrg" ("orgId", "userId", role, "joinedAt")
		VALUES ($1, $2, 'OWNER', NOW())
		ON CONFLICT ("orgId", "userId") DO UPDATE SET role = 'OWNER'
	`, tenant.ID, AtomServiceUserID); err != nil {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: upsert membership: %w", err)
	}

	if p.APIKeyHash != "" {
		if _, err := tx.Exec(ctx, `
			DELETE FROM "ApiKey"
			WHERE "orgId" = $1 AND name = $2 AND "createdById" = $3
		`, tenant.ID, p.APIKeyName, AtomServiceUserID); err != nil {
			return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: delete prior api key: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO "ApiKey" (hash, name, "orgId", "createdById", "createdAt")
			VALUES ($1, $2, $3, $4, NOW())
		`, p.APIKeyHash, p.APIKeyName, tenant.ID, AtomServiceUserID); err != nil {
			return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: insert api key: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return AtomWorkspaceTenant{}, fmt.Errorf("db: UpsertAtomWorkspaceTenant: commit: %w", err)
	}
	return tenant, nil
}
