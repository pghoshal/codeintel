package llmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"codeintel/pkg/llmproxy"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	db *pgxpool.Pool
}

type RequestStore interface {
	ClaimStarted(ctx context.Context, req llmproxy.ChatRequest, staleAfter time.Duration) (RequestRow, bool, error)
	MarkPartial(ctx context.Context, orgID int32, id string, resp llmproxy.ChatResponse) error
	MarkSucceeded(ctx context.Context, orgID int32, id string, resp llmproxy.ChatResponse) error
	MarkRetryableError(ctx context.Context, orgID int32, id string, message string) error
	MarkFailed(ctx context.Context, orgID int32, id string, message string) error
	Get(ctx context.Context, orgID int32, id string) (RequestRow, error)
}

type RequestRow struct {
	ID          string
	OrgID       int32
	Status      string
	Provider    string
	Model       string
	Request     json.RawMessage
	Response    json.RawMessage
	Error       *string
	Attempts    int32
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

func (s *Store) ClaimStarted(ctx context.Context, req llmproxy.ChatRequest, staleAfter time.Duration) (RequestRow, bool, error) {
	if s == nil || s.db == nil {
		return RequestRow{}, false, errors.New("llm gateway store is not configured")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return RequestRow{}, false, err
	}
	staleSeconds := int(staleAfter.Seconds())
	if staleSeconds <= 0 {
		staleSeconds = 360
	}
	row, err := scanRequestRow(s.db.QueryRow(ctx, `
INSERT INTO "LLMRequest" (id, "orgId", status, provider, model, request, attempts, "createdAt", "updatedAt")
VALUES ($1, $2, 'IN_PROGRESS', $3, $4, $5, 0, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
SET status = 'IN_PROGRESS',
    provider = EXCLUDED.provider,
    model = EXCLUDED.model,
    request = EXCLUDED.request,
    response = NULL,
    error = NULL,
    "updatedAt" = NOW(),
    "completedAt" = NULL
WHERE "LLMRequest"."orgId" = EXCLUDED."orgId"
  AND (
    "LLMRequest".status = 'FAILED'
    OR "LLMRequest".request IS DISTINCT FROM EXCLUDED.request
    OR (
      "LLMRequest".status = 'IN_PROGRESS'
      AND "LLMRequest"."updatedAt" < NOW() - ($6::int * INTERVAL '1 second')
    )
  )
RETURNING id, "orgId", status, provider, model, request, COALESCE(response, '{}'::jsonb), error, attempts, "createdAt", "updatedAt", "completedAt"
`, req.RequestID, req.OrgID, req.Provider, req.Model.Model, payload, staleSeconds))
	if err == nil {
		return row, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return RequestRow{}, false, fmt.Errorf("claim llm request: %w", err)
	}
	existing, err := s.Get(ctx, req.OrgID, req.RequestID)
	if err != nil {
		return RequestRow{}, false, err
	}
	return existing, false, nil
}

func (s *Store) MarkStarted(ctx context.Context, req llmproxy.ChatRequest) error {
	if s == nil || s.db == nil {
		return errors.New("llm gateway store is not configured")
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
INSERT INTO "LLMRequest" (id, "orgId", status, provider, model, request, attempts, "createdAt", "updatedAt")
VALUES ($1, $2, 'IN_PROGRESS', $3, $4, $5, 0, NOW(), NOW())
ON CONFLICT (id) DO UPDATE
SET status = CASE WHEN "LLMRequest".status = 'SUCCEEDED' THEN "LLMRequest".status ELSE 'IN_PROGRESS' END,
    request = CASE WHEN "LLMRequest".status = 'SUCCEEDED' THEN "LLMRequest".request ELSE EXCLUDED.request END,
    error = CASE WHEN "LLMRequest".status = 'SUCCEEDED' THEN "LLMRequest".error ELSE NULL END,
    "updatedAt" = NOW()
WHERE "LLMRequest"."orgId" = EXCLUDED."orgId"`, req.RequestID, req.OrgID, req.Provider, req.Model.Model, payload)
	if err != nil {
		return fmt.Errorf("mark llm request started: %w", err)
	}
	return nil
}

func (s *Store) MarkSucceeded(ctx context.Context, orgID int32, id string, resp llmproxy.ChatResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `
UPDATE "LLMRequest"
SET status = 'SUCCEEDED',
    response = $3,
    error = NULL,
    attempts = attempts + 1,
    "updatedAt" = NOW(),
    "completedAt" = NOW()
WHERE "orgId" = $1 AND id = $2`, orgID, id, payload)
	if err != nil {
		return fmt.Errorf("mark llm request succeeded: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) MarkPartial(ctx context.Context, orgID int32, id string, resp llmproxy.ChatResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `
UPDATE "LLMRequest"
SET response = $3,
    error = NULL,
    "updatedAt" = NOW()
WHERE "orgId" = $1 AND id = $2 AND status = 'IN_PROGRESS'`, orgID, id, payload)
	if err != nil {
		return fmt.Errorf("mark llm request partial response: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) MarkRetryableError(ctx context.Context, orgID int32, id string, message string) error {
	tag, err := s.db.Exec(ctx, `
UPDATE "LLMRequest"
SET error = $3,
    attempts = attempts + 1,
    "updatedAt" = NOW()
WHERE "orgId" = $1 AND id = $2 AND status = 'IN_PROGRESS'`, orgID, id, message)
	if err != nil {
		return fmt.Errorf("mark llm request retryable error: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, orgID int32, id string, message string) error {
	tag, err := s.db.Exec(ctx, `
UPDATE "LLMRequest"
SET status = 'FAILED',
    error = $3,
    attempts = attempts + 1,
    "updatedAt" = NOW(),
    "completedAt" = NOW()
WHERE "orgId" = $1 AND id = $2`, orgID, id, message)
	if err != nil {
		return fmt.Errorf("mark llm request failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (s *Store) Get(ctx context.Context, orgID int32, id string) (RequestRow, error) {
	return scanRequestRow(s.db.QueryRow(ctx, `
SELECT id, "orgId", status, provider, model, request, COALESCE(response, '{}'::jsonb), error, attempts, "createdAt", "updatedAt", "completedAt"
FROM "LLMRequest"
WHERE "orgId" = $1 AND id = $2`, orgID, id))
}

type requestRowScanner interface {
	Scan(dest ...any) error
}

func scanRequestRow(scanner requestRowScanner) (RequestRow, error) {
	var row RequestRow
	err := scanner.Scan(
		&row.ID,
		&row.OrgID,
		&row.Status,
		&row.Provider,
		&row.Model,
		&row.Request,
		&row.Response,
		&row.Error,
		&row.Attempts,
		&row.CreatedAt,
		&row.UpdatedAt,
		&row.CompletedAt,
	)
	if err != nil {
		return RequestRow{}, err
	}
	return row, nil
}
