package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrChatNotFound          = errors.New("db: chat not found")
	ErrSearchContextNotFound = errors.New("db: search context not found")
)

type ChatRow struct {
	ID                 string
	Name               *string
	CreatedByID        *string
	AnonymousCreatorID *string
	OrgID              int32
	Visibility         string
	Messages           json.RawMessage
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type CreateChatParams struct {
	ID                 string
	Name               *string
	CreatedByID        *string
	AnonymousCreatorID *string
	OrgID              int32
	Visibility         string
	Messages           json.RawMessage
}

const (
	createChatQuery                 = `INSERT INTO "Chat" (id, name, "createdById", "anonymousCreatorId", "orgId", visibility, messages, "createdAt", "updatedAt") VALUES ($1, $2, $3, $4, $5, $6::"ChatVisibility", $7, NOW(), NOW()) RETURNING id, name, "createdById", "anonymousCreatorId", "orgId", visibility::text, messages, "createdAt", "updatedAt"`
	getChatQuery                    = `SELECT id, name, "createdById", "anonymousCreatorId", "orgId", visibility::text, messages, "createdAt", "updatedAt" FROM "Chat" WHERE "orgId" = $1 AND id = $2`
	updateChatMessagesQuery         = `UPDATE "Chat" SET messages = $3, "updatedAt" = NOW() WHERE "orgId" = $1 AND id = $2`
	updateChatNameQuery             = `UPDATE "Chat" SET name = $3, "updatedAt" = NOW() WHERE "orgId" = $1 AND id = $2`
	listSearchContextRepoNamesQuery = `SELECT r.name FROM "SearchContext" sc JOIN "_RepoToSearchContext" rtc ON rtc."B" = sc.id JOIN "Repo" r ON r.id = rtc."A" WHERE sc."orgId" = $1 AND sc.name = $2 AND EXISTS (SELECT 1 FROM "RepoToConnection" rc JOIN "Connection" c ON c.id = rc."connectionId" WHERE rc."repoId" = r.id AND c."orgId" = r."orgId") ORDER BY r.name ASC`
)

func (q *Queries) CreateChat(ctx context.Context, p CreateChatParams) (ChatRow, error) {
	if p.OrgID <= 0 {
		return ChatRow{}, ErrInvalidOrgID
	}
	if p.ID == "" {
		return ChatRow{}, fmt.Errorf("db: CreateChat: id is required")
	}
	if p.Visibility == "" {
		p.Visibility = "PRIVATE"
	}
	if len(p.Messages) == 0 || !json.Valid(p.Messages) {
		return ChatRow{}, fmt.Errorf("db: CreateChat: messages must be valid JSON")
	}
	var row ChatRow
	err := q.db.QueryRow(ctx, createChatQuery,
		p.ID,
		p.Name,
		p.CreatedByID,
		p.AnonymousCreatorID,
		p.OrgID,
		p.Visibility,
		p.Messages,
	).Scan(
		&row.ID,
		&row.Name,
		&row.CreatedByID,
		&row.AnonymousCreatorID,
		&row.OrgID,
		&row.Visibility,
		&row.Messages,
		&row.CreatedAt,
		&row.UpdatedAt,
	)
	if err != nil {
		return ChatRow{}, fmt.Errorf("db: CreateChat: %w", err)
	}
	return row, nil
}

func (q *Queries) GetChatForOrg(ctx context.Context, orgID int32, chatID string) (ChatRow, error) {
	if orgID <= 0 {
		return ChatRow{}, ErrInvalidOrgID
	}
	if chatID == "" {
		return ChatRow{}, ErrChatNotFound
	}
	var row ChatRow
	err := q.db.QueryRow(ctx, getChatQuery, orgID, chatID).Scan(
		&row.ID,
		&row.Name,
		&row.CreatedByID,
		&row.AnonymousCreatorID,
		&row.OrgID,
		&row.Visibility,
		&row.Messages,
		&row.CreatedAt,
		&row.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ChatRow{}, ErrChatNotFound
	}
	if err != nil {
		return ChatRow{}, fmt.Errorf("db: GetChatForOrg: %w", err)
	}
	return row, nil
}

func (q *Queries) UpdateChatMessages(ctx context.Context, orgID int32, chatID string, messages json.RawMessage) error {
	if orgID <= 0 {
		return ErrInvalidOrgID
	}
	if chatID == "" {
		return ErrChatNotFound
	}
	if len(messages) == 0 || !json.Valid(messages) {
		return fmt.Errorf("db: UpdateChatMessages: messages must be valid JSON")
	}
	tag, err := q.db.Exec(ctx, updateChatMessagesQuery, orgID, chatID, messages)
	if err != nil {
		return fmt.Errorf("db: UpdateChatMessages: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrChatNotFound
	}
	return nil
}

func (q *Queries) UpdateChatName(ctx context.Context, orgID int32, chatID string, name string) error {
	if orgID <= 0 {
		return ErrInvalidOrgID
	}
	if chatID == "" {
		return ErrChatNotFound
	}
	tag, err := q.db.Exec(ctx, updateChatNameQuery, orgID, chatID, name)
	if err != nil {
		return fmt.Errorf("db: UpdateChatName: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrChatNotFound
	}
	return nil
}

func (q *Queries) ListSearchContextRepoNames(ctx context.Context, orgID int32, name string) ([]string, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	if name == "" {
		return nil, ErrSearchContextNotFound
	}
	rows, err := q.db.Query(ctx, listSearchContextRepoNamesQuery, orgID, name)
	if err != nil {
		return nil, fmt.Errorf("db: ListSearchContextRepoNames: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, fmt.Errorf("db: ListSearchContextRepoNames: scan: %w", err)
		}
		out = append(out, repo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListSearchContextRepoNames: rows: %w", err)
	}
	if len(out) == 0 {
		return nil, ErrSearchContextNotFound
	}
	return out, nil
}
