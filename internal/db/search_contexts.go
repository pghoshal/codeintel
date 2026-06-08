package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

var ErrSearchContextRepoNotFound = errors.New("db: search context repo not found")

type SearchContextRow struct {
	ID            int32
	Name          string
	Description   *string
	Config        json.RawMessage
	IsDeclarative bool
	RepoNames     []string
}

type SearchContextInput struct {
	Name        string
	Description *string
	Config      any
	RepoNames   []string
}

const listOrgSearchContextsQuery = `SELECT sc.id, sc.name, sc.description, COALESCE(sc.config, '{}'::jsonb), sc."isDeclarative", COALESCE(array_agg(r.name ORDER BY r.name) FILTER (WHERE r.id IS NOT NULL), ARRAY[]::text[]) FROM "SearchContext" sc LEFT JOIN "_RepoToSearchContext" rtc ON rtc."B" = sc.id LEFT JOIN "Repo" r ON r.id = rtc."A" AND r."orgId" = sc."orgId" AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId") WHERE sc."orgId" = $1 GROUP BY sc.id, sc.name, sc.description, sc.config, sc."isDeclarative" ORDER BY sc.name ASC`

func (q *Queries) ListOrgSearchContexts(ctx context.Context, orgID int32) ([]SearchContextRow, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	rows, err := q.db.Query(ctx, listOrgSearchContextsQuery, orgID)
	if err != nil {
		return nil, fmt.Errorf("db: ListOrgSearchContexts: %w", err)
	}
	defer rows.Close()
	out := make([]SearchContextRow, 0)
	for rows.Next() {
		var r SearchContextRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &r.Config, &r.IsDeclarative, &r.RepoNames); err != nil {
			return nil, fmt.Errorf("db: ListOrgSearchContexts: scan: %w", err)
		}
		if r.RepoNames == nil {
			r.RepoNames = []string{}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListOrgSearchContexts: rows: %w", err)
	}
	return out, nil
}

const findOrgActiveReposByNameQuery = `SELECT r.id, r.name FROM "Repo" r WHERE r."orgId" = $1 AND r.name = ANY($2::text[]) AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId")`
const upsertSearchContextQuery = `INSERT INTO "SearchContext" (name, description, config, "isDeclarative", "orgId") VALUES ($1, $2, $3, false, $4) ON CONFLICT (name, "orgId") DO UPDATE SET description = EXCLUDED.description, config = EXCLUDED.config, "isDeclarative" = false RETURNING id`
const deleteSearchContextReposQuery = `DELETE FROM "_RepoToSearchContext" WHERE "B" = $1`
const insertSearchContextRepoQuery = `INSERT INTO "_RepoToSearchContext" ("A", "B") VALUES ($1, $2) ON CONFLICT DO NOTHING`
const deleteObsoleteSearchContextsQuery = `DELETE FROM "SearchContext" WHERE "orgId" = $1 AND "isDeclarative" = false AND NOT (name = ANY($2::text[]))`

func (q *Queries) ReplaceOrgSearchContexts(ctx context.Context, orgID int32, contexts []SearchContextInput) error {
	if orgID <= 0 {
		return ErrInvalidOrgID
	}
	tx, err := q.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: ReplaceOrgSearchContexts: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	names := make([]string, 0, len(contexts))
	for i, c := range contexts {
		if c.Name == "" {
			return fmt.Errorf("db: ReplaceOrgSearchContexts: context[%d] name is required", i)
		}
		names = append(names, c.Name)
		repoIDs, err := findActiveRepoIDsByName(ctx, tx, orgID, c.RepoNames)
		if err != nil {
			return fmt.Errorf("db: ReplaceOrgSearchContexts: context[%d]: %w", i, err)
		}
		cfg, err := json.Marshal(c.Config)
		if err != nil {
			return fmt.Errorf("db: ReplaceOrgSearchContexts: marshal config[%d]: %w", i, err)
		}
		var contextID int32
		if err := tx.QueryRow(ctx, upsertSearchContextQuery, c.Name, c.Description, cfg, orgID).Scan(&contextID); err != nil {
			return fmt.Errorf("db: ReplaceOrgSearchContexts: upsert[%d]: %w", i, err)
		}
		if _, err := tx.Exec(ctx, deleteSearchContextReposQuery, contextID); err != nil {
			return fmt.Errorf("db: ReplaceOrgSearchContexts: clear repos[%d]: %w", i, err)
		}
		for _, repoID := range repoIDs {
			if _, err := tx.Exec(ctx, insertSearchContextRepoQuery, repoID, contextID); err != nil {
				return fmt.Errorf("db: ReplaceOrgSearchContexts: link repo[%d]: %w", i, err)
			}
		}
	}
	if _, err := tx.Exec(ctx, deleteObsoleteSearchContextsQuery, orgID, names); err != nil {
		return fmt.Errorf("db: ReplaceOrgSearchContexts: delete obsolete: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: ReplaceOrgSearchContexts: commit: %w", err)
	}
	return nil
}

func findActiveRepoIDsByName(ctx context.Context, tx pgx.Tx, orgID int32, names []string) ([]int32, error) {
	if len(names) == 0 {
		return []int32{}, nil
	}
	seen := make(map[string]struct{}, len(names))
	unique := make([]string, 0, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		unique = append(unique, name)
	}
	rows, err := tx.Query(ctx, findOrgActiveReposByNameQuery, orgID, unique)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int32, 0, len(unique))
	found := make(map[string]struct{}, len(unique))
	for rows.Next() {
		var id int32
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		ids = append(ids, id)
		found[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, name := range unique {
		if _, ok := found[name]; !ok {
			return nil, ErrSearchContextRepoNotFound
		}
	}
	return ids, nil
}
