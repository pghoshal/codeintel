package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

const expectedListOrgSecretsQuery = `SELECT key, "createdAt", "updatedAt" FROM "OrgSecret" WHERE "orgId" = \$1 ORDER BY key ASC`
const expectedGetOrgSecretCiphertextQuery = `SELECT key, "encryptedValue", iv FROM "OrgSecret" WHERE "orgId" = \$1 AND key = \$2`

// TestListOrgSecrets_EmptyOrg locks the parity contract for the most
// common case: an org with no secrets must return an empty slice
// (zero rows) — never nil, never an error. The HTTP handler
// encodes the result as JSON; nil would become `null`, the empty
// slice becomes `[]`. The wire response for an empty org is `[]`.
func TestListOrgSecrets_EmptyOrg(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedListOrgSecretsQuery).
		WithArgs(int32(1)).
		WillReturnRows(pgxmock.NewRows([]string{"key", "createdAt", "updatedAt"}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.ListOrgSecrets(ctx, 1)
	if err != nil {
		t.Fatalf("ListOrgSecrets: %v", err)
	}
	if got == nil {
		t.Fatalf("expected empty slice, got nil — nil would JSON-encode as `null`, not `[]`")
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 rows, got %d", len(got))
	}
}

// TestListOrgSecrets_PreservesOrderAndShape locks the row shape and
// the `ORDER BY key ASC` contract — the JSON array must reflect
// that order byte-for-byte.
func TestListOrgSecrets_PreservesOrderAndShape(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	now := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	later := now.Add(2 * time.Hour)
	mock.ExpectQuery(expectedListOrgSecretsQuery).
		WithArgs(int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"key", "createdAt", "updatedAt"}).
			AddRow("ALPHA", now, now).
			AddRow("BETA", later, later))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.ListOrgSecrets(ctx, 7)
	if err != nil {
		t.Fatalf("ListOrgSecrets: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if got[0].Key != "ALPHA" || got[1].Key != "BETA" {
		t.Fatalf("order/keys: got [%q, %q], want [ALPHA, BETA]", got[0].Key, got[1].Key)
	}
	if !got[0].CreatedAt.Equal(now) {
		t.Errorf("row 0 CreatedAt: got %v, want %v", got[0].CreatedAt, now)
	}
	if !got[1].UpdatedAt.Equal(later) {
		t.Errorf("row 1 UpdatedAt: got %v, want %v", got[1].UpdatedAt, later)
	}
}

// TestListOrgSecrets_ThreeRowsOrderStable extends the two-row order
// test with a third element so the ORDER BY contract is locked at
// scale and the append-loop doesn't accidentally drop or duplicate
// rows at non-trivial slice growth.
func TestListOrgSecrets_ThreeRowsOrderStable(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	now := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	mock.ExpectQuery(expectedListOrgSecretsQuery).
		WithArgs(int32(9)).
		WillReturnRows(pgxmock.NewRows([]string{"key", "createdAt", "updatedAt"}).
			AddRow("ALPHA", now, now).
			AddRow("BETA", now, now).
			AddRow("CHARLIE", now, now))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.ListOrgSecrets(ctx, 9)
	if err != nil {
		t.Fatalf("ListOrgSecrets: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 rows, got %d", len(got))
	}
	if got[0].Key != "ALPHA" || got[1].Key != "BETA" || got[2].Key != "CHARLIE" {
		t.Fatalf("order: [%q, %q, %q], want [ALPHA, BETA, CHARLIE]", got[0].Key, got[1].Key, got[2].Key)
	}
}

// TestUpdateApiKeyLastUsedAt_ZeroRowsIsSuccess locks the race-window
// contract documented in db/secrets.go: if the ApiKey row was deleted
// between auth resolution and this UPDATE, the function returns nil
// (not an error). The auth check already succeeded; turning a delete
// race into a 500 would be operationally hostile.
func TestUpdateApiKeyLastUsedAt_ZeroRowsIsSuccess(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(expectedUpdateLastUsedQuery).
		WithArgs("vanished-hash").
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	if err := q.UpdateApiKeyLastUsedAt(ctx, "vanished-hash"); err != nil {
		t.Fatalf("zero affected rows must be success, got %v", err)
	}
}

// TestListOrgSecrets_InvalidOrgID guards against the caller passing a
// non-positive org id (matching the GetOrgByID guard added in Slice
// 2.5).
func TestListOrgSecrets_InvalidOrgID(t *testing.T) {
	q := &Queries{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	for _, id := range []int32{0, -1} {
		if _, err := q.ListOrgSecrets(ctx, id); !errors.Is(err, ErrInvalidOrgID) {
			t.Errorf("ListOrgSecrets(%d): got %v, want ErrInvalidOrgID", id, err)
		}
	}
}

func TestGetOrgSecretCiphertext_HappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(expectedGetOrgSecretCiphertextQuery).
		WithArgs(int32(7), "GLM_KEY").
		WillReturnRows(pgxmock.NewRows([]string{"key", "encryptedValue", "iv"}).
			AddRow("GLM_KEY", "cipherhex", "ivhex"))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.GetOrgSecretCiphertext(ctx, 7, "GLM_KEY")
	if err != nil {
		t.Fatalf("GetOrgSecretCiphertext: %v", err)
	}
	if got.Key != "GLM_KEY" || got.EncryptedValue != "cipherhex" || got.IV != "ivhex" {
		t.Fatalf("ciphertext row mismatch: %+v", got)
	}
}

func TestGetOrgSecretCiphertext_NotFoundAndBoundaryGuards(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	q := &Queries{}
	if _, err := q.GetOrgSecretCiphertext(ctx, 0, "K"); !errors.Is(err, ErrInvalidOrgID) {
		t.Fatalf("zero org: got %v want ErrInvalidOrgID", err)
	}
	if _, err := q.GetOrgSecretCiphertext(ctx, 7, ""); !errors.Is(err, ErrEmptySecretKey) {
		t.Fatalf("empty key: got %v want ErrEmptySecretKey", err)
	}

	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()
	mock.ExpectQuery(expectedGetOrgSecretCiphertextQuery).
		WithArgs(int32(7), "MISSING").
		WillReturnRows(pgxmock.NewRows([]string{"key", "encryptedValue", "iv"}))

	q = &Queries{db: mock}
	if _, err := q.GetOrgSecretCiphertext(ctx, 7, "MISSING"); !errors.Is(err, ErrOrgSecretNotFound) {
		t.Fatalf("missing secret: got %v want ErrOrgSecretNotFound", err)
	}
}

const expectedUpsertOrgSecretQuery = `INSERT INTO "OrgSecret" \("orgId", key, "encryptedValue", iv, "createdAt", "updatedAt"\) VALUES \(\$1, \$2, \$3, \$4, NOW\(\), NOW\(\)\) ON CONFLICT \("orgId", key\) DO UPDATE SET "encryptedValue" = EXCLUDED\."encryptedValue", iv = EXCLUDED\.iv, "updatedAt" = NOW\(\) RETURNING key, "createdAt", "updatedAt"`

// TestUpsertOrgSecret_NewRow locks the INSERT branch: a fresh
// (orgId, key) pair must create a row and return its identity.
func TestUpsertOrgSecret_NewRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery(expectedUpsertOrgSecretQuery).
		WithArgs(int32(5), "GH_TOKEN", "ciphertexthex", "ivhex").
		WillReturnRows(pgxmock.NewRows([]string{"key", "createdAt", "updatedAt"}).
			AddRow("GH_TOKEN", now, now))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	got, err := q.UpsertOrgSecret(ctx, UpsertOrgSecretParams{
		OrgID:          5,
		Key:            "GH_TOKEN",
		EncryptedValue: "ciphertexthex",
		IV:             "ivhex",
	})
	if err != nil {
		t.Fatalf("UpsertOrgSecret: %v", err)
	}
	if got.Key != "GH_TOKEN" {
		t.Errorf("Key: got %q", got.Key)
	}
	if !got.CreatedAt.Equal(now) || !got.UpdatedAt.Equal(now) {
		t.Errorf("timestamps: got created=%v updated=%v, want %v", got.CreatedAt, got.UpdatedAt, now)
	}
}

// TestUpsertOrgSecret_RejectsBoundaryInputs locks the four
// boundary guards: empty key, empty encryptedValue, empty iv, and
// non-positive orgID. Each must surface a typed error BEFORE any DB
// round-trip so a typo can't trigger a partial INSERT.
func TestUpsertOrgSecret_RejectsBoundaryInputs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{}
	cases := []struct {
		name string
		p    UpsertOrgSecretParams
		want error
	}{
		{"empty_key", UpsertOrgSecretParams{OrgID: 1, EncryptedValue: "c", IV: "i"}, ErrEmptySecretKey},
		{"empty_value", UpsertOrgSecretParams{OrgID: 1, Key: "K", IV: "i"}, ErrEmptySecretValue},
		{"empty_iv", UpsertOrgSecretParams{OrgID: 1, Key: "K", EncryptedValue: "c"}, ErrEmptySecretIV},
		{"zero_orgid", UpsertOrgSecretParams{OrgID: 0, Key: "K", EncryptedValue: "c", IV: "i"}, ErrInvalidOrgID},
		{"negative_orgid", UpsertOrgSecretParams{OrgID: -1, Key: "K", EncryptedValue: "c", IV: "i"}, ErrInvalidOrgID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := q.UpsertOrgSecret(ctx, c.p); !errors.Is(err, c.want) {
				t.Errorf("got %v, want %v", err, c.want)
			}
		})
	}
}

const expectedUpdateLastUsedQuery = `UPDATE "ApiKey" SET "lastUsedAt" = NOW\(\) WHERE hash = \$1`

// TestUpdateApiKeyLastUsedAt_HappyPath locks the contract that the
// helper issues a single `UPDATE "ApiKey" SET "lastUsedAt" = NOW()
// WHERE hash = $1`. Using NOW() (not a Go-side time.Time) avoids
// clock-skew differences between the app pod and the Postgres
// server — and the only observable surface (DB row content) is
// byte-identical.
func TestUpdateApiKeyLastUsedAt_HappyPath(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(expectedUpdateLastUsedQuery).
		WithArgs("hash-xyz").
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	q := &Queries{db: mock}
	if err := q.UpdateApiKeyLastUsedAt(ctx, "hash-xyz"); err != nil {
		t.Fatalf("UpdateApiKeyLastUsedAt: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestUpdateApiKeyLastUsedAt_EmptyHash rejects an empty hash at the
// boundary so a typo in the middleware cannot accidentally update
// every row (or zero rows silently).
func TestUpdateApiKeyLastUsedAt_EmptyHash(t *testing.T) {
	q := &Queries{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if err := q.UpdateApiKeyLastUsedAt(ctx, ""); !errors.Is(err, ErrEmptyHash) {
		t.Fatalf("got %v, want ErrEmptyHash", err)
	}
}
