package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Boundary-guard sentinels for the OrgSecret write path. Surfacing
// each empty input separately makes a misconfigured caller trivial
// to diagnose at the boundary.
var (
	ErrEmptySecretKey    = errors.New("db: org-secret key argument is required")
	ErrEmptySecretValue  = errors.New("db: org-secret encrypted-value argument is required")
	ErrEmptySecretIV     = errors.New("db: org-secret iv argument is required")
	ErrOrgSecretNotFound = errors.New("db: org-secret not found")
)

// OrgSecret is the projection of OrgSecret rows the
// GET /api/secrets handler exposes. The full schema row carries
// encryptedValue + iv columns too; those are never sent over the
// wire and are not selected here.
type OrgSecret struct {
	Key       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// OrgSecretCiphertext is the private projection used only by
// server-side secret resolvers. It intentionally excludes timestamps
// and includes the encrypted value + iv columns needed by auth.Decrypt.
type OrgSecretCiphertext struct {
	Key            string
	EncryptedValue string
	IV             string
}

// OrgLanguageModelRow projects the columns the GET /api/models
// handler needs: the raw JSON config (the handler decodes
// provider/model/displayName) plus the org-scoped sort order.
type OrgLanguageModelRow struct {
	Config []byte
}

// UpsertOrgSecretParams is the typed argument bag for the secret
// upsert write.
type UpsertOrgSecretParams struct {
	OrgID          int32
	Key            string
	EncryptedValue string
	IV             string
}

// OrgLanguageModelInsert is one row to INSERT during a
// ReplaceOrgLanguageModels transaction.
type OrgLanguageModelInsert struct {
	Name   string
	Config any
	Order  int32
}

const (
	listOrgSecretsQuery               = `SELECT key, "createdAt", "updatedAt" FROM "OrgSecret" WHERE "orgId" = $1 ORDER BY key ASC`
	getOrgSecretCiphertextQuery       = `SELECT key, "encryptedValue", iv FROM "OrgSecret" WHERE "orgId" = $1 AND key = $2`
	selectMissingOrgSecretKeysQuery   = `SELECT k FROM unnest($2::text[]) AS k WHERE k NOT IN (SELECT key FROM "OrgSecret" WHERE "orgId" = $1)`
	listEnabledOrgLanguageModelsQuery = `SELECT config FROM "OrgLanguageModel" WHERE "orgId" = $1 AND enabled = true ORDER BY "order" ASC, id ASC`
	upsertOrgSecretQuery              = `INSERT INTO "OrgSecret" ("orgId", key, "encryptedValue", iv, "createdAt", "updatedAt") VALUES ($1, $2, $3, $4, NOW(), NOW()) ON CONFLICT ("orgId", key) DO UPDATE SET "encryptedValue" = EXCLUDED."encryptedValue", iv = EXCLUDED.iv, "updatedAt" = NOW() RETURNING key, "createdAt", "updatedAt"`
	deleteAllOrgLanguageModelsQuery   = `DELETE FROM "OrgLanguageModel" WHERE "orgId" = $1`
	insertOrgLanguageModelQuery       = `INSERT INTO "OrgLanguageModel" ("orgId", name, config, "order", enabled, "createdAt", "updatedAt") VALUES ($1, $2, $3, $4, true, NOW(), NOW())`
	updateApiKeyLastUsedAtQuery       = `UPDATE "ApiKey" SET "lastUsedAt" = NOW() WHERE hash = $1`
)

// ListOrgSecrets returns every secret row for the org sorted by
// key ascending. Returns a non-nil empty slice when the org has
// no secrets so callers that JSON-encode the result emit `[]` and
// not `null`.
func (q *Queries) ListOrgSecrets(ctx context.Context, orgID int32) ([]OrgSecret, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	rows, err := q.db.Query(ctx, listOrgSecretsQuery, orgID)
	if err != nil {
		return nil, fmt.Errorf("db: ListOrgSecrets: %w", err)
	}
	defer rows.Close()

	out := make([]OrgSecret, 0)
	for rows.Next() {
		var s OrgSecret
		if err := rows.Scan(&s.Key, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("db: ListOrgSecrets: scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListOrgSecrets: rows: %w", err)
	}
	return out, nil
}

// GetOrgSecretCiphertext returns the encrypted payload for one
// org-scoped secret. Callers decrypt it with auth.Decrypt using the
// process encryption key. The plaintext never crosses the db package.
func (q *Queries) GetOrgSecretCiphertext(ctx context.Context, orgID int32, key string) (OrgSecretCiphertext, error) {
	if orgID <= 0 {
		return OrgSecretCiphertext{}, ErrInvalidOrgID
	}
	if key == "" {
		return OrgSecretCiphertext{}, ErrEmptySecretKey
	}
	var s OrgSecretCiphertext
	err := q.db.QueryRow(ctx, getOrgSecretCiphertextQuery, orgID, key).Scan(&s.Key, &s.EncryptedValue, &s.IV)
	if errors.Is(err, pgx.ErrNoRows) {
		return OrgSecretCiphertext{}, ErrOrgSecretNotFound
	}
	if err != nil {
		return OrgSecretCiphertext{}, fmt.Errorf("db: GetOrgSecretCiphertext: %w", err)
	}
	return s, nil
}

// SelectMissingOrgSecretKeys returns which of the supplied
// candidate keys do NOT exist as OrgSecret rows under the org.
// One round-trip via unnest($2::text[]) NOT IN (subquery).
func (q *Queries) SelectMissingOrgSecretKeys(ctx context.Context, orgID int32, candidateKeys []string) ([]string, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	if len(candidateKeys) == 0 {
		return make([]string, 0), nil
	}
	rows, err := q.db.Query(ctx, selectMissingOrgSecretKeysQuery, orgID, candidateKeys)
	if err != nil {
		return nil, fmt.Errorf("db: SelectMissingOrgSecretKeys: %w", err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("db: SelectMissingOrgSecretKeys: scan: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: SelectMissingOrgSecretKeys: rows: %w", err)
	}
	return out, nil
}

// ListEnabledOrgLanguageModels returns the enabled language-model
// configs for the org, sorted by (order ASC, id ASC). Returns a
// non-nil empty slice on zero rows for `[]` JSON parity.
func (q *Queries) ListEnabledOrgLanguageModels(ctx context.Context, orgID int32) ([]OrgLanguageModelRow, error) {
	if orgID <= 0 {
		return nil, ErrInvalidOrgID
	}
	rows, err := q.db.Query(ctx, listEnabledOrgLanguageModelsQuery, orgID)
	if err != nil {
		return nil, fmt.Errorf("db: ListEnabledOrgLanguageModels: %w", err)
	}
	defer rows.Close()
	out := make([]OrgLanguageModelRow, 0)
	for rows.Next() {
		var r OrgLanguageModelRow
		if err := rows.Scan(&r.Config); err != nil {
			return nil, fmt.Errorf("db: ListEnabledOrgLanguageModels: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: ListEnabledOrgLanguageModels: rows: %w", err)
	}
	return out, nil
}

// UpsertOrgSecret inserts a new (orgId, key) secret or updates the
// existing row's encryptedValue + iv. createdAt is preserved across
// updates by the ON CONFLICT branch. NOW() is used server-side for
// both timestamps so app replicas can't disagree on clock.
func (q *Queries) UpsertOrgSecret(ctx context.Context, p UpsertOrgSecretParams) (OrgSecret, error) {
	if p.OrgID <= 0 {
		return OrgSecret{}, ErrInvalidOrgID
	}
	if p.Key == "" {
		return OrgSecret{}, ErrEmptySecretKey
	}
	if p.EncryptedValue == "" {
		return OrgSecret{}, ErrEmptySecretValue
	}
	if p.IV == "" {
		return OrgSecret{}, ErrEmptySecretIV
	}
	var s OrgSecret
	if err := q.db.QueryRow(ctx, upsertOrgSecretQuery, p.OrgID, p.Key, p.EncryptedValue, p.IV).
		Scan(&s.Key, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return OrgSecret{}, fmt.Errorf("db: UpsertOrgSecret: %w", err)
	}
	return s, nil
}

// ReplaceOrgLanguageModels does the full replace-all transaction:
//
//	BEGIN;
//	DELETE FROM "OrgLanguageModel" WHERE "orgId" = $1;
//	(for each model) INSERT INTO "OrgLanguageModel" (...);
//	COMMIT;
//
// The tx ensures partial-failure atomicity: a mid-insert error
// rolls back the delete so the org's existing model set is
// preserved.
func (q *Queries) ReplaceOrgLanguageModels(ctx context.Context, orgID int32, models []OrgLanguageModelInsert) error {
	if orgID <= 0 {
		return ErrInvalidOrgID
	}
	tx, err := q.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: ReplaceOrgLanguageModels: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, deleteAllOrgLanguageModelsQuery, orgID); err != nil {
		return fmt.Errorf("db: ReplaceOrgLanguageModels: delete: %w", err)
	}
	for i, m := range models {
		cfgBytes, err := json.Marshal(m.Config)
		if err != nil {
			return fmt.Errorf("db: ReplaceOrgLanguageModels: marshal config[%d]: %w", i, err)
		}
		if _, err := tx.Exec(ctx, insertOrgLanguageModelQuery, orgID, m.Name, cfgBytes, m.Order); err != nil {
			return fmt.Errorf("db: ReplaceOrgLanguageModels: insert[%d]: %w", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: ReplaceOrgLanguageModels: commit: %w", err)
	}
	return nil
}

// UpdateApiKeyLastUsedAt bumps the lastUsedAt column for the
// matched ApiKey row. Uses NOW() server-side rather than a Go-side
// time.Time so the timestamp comes from the DB clock — avoiding
// clock-skew between app pods.
//
// Returns silently on zero affected rows: if the hash resolved to
// a real key at GetApiKeyAuth time but the row was deleted between
// auth resolution and this UPDATE, the request should still succeed
// (the user was authenticated at the moment of check). Surfacing
// zero-rows as an error would create a race-loss 500.
func (q *Queries) UpdateApiKeyLastUsedAt(ctx context.Context, hash string) error {
	if hash == "" {
		return ErrEmptyHash
	}
	if _, err := q.db.Exec(ctx, updateApiKeyLastUsedAtQuery, hash); err != nil {
		return fmt.Errorf("db: UpdateApiKeyLastUsedAt: %w", err)
	}
	return nil
}
