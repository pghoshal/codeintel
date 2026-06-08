package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v4"
)

// TestGetOrgByID_ReturnsRow locks the contract for the primary
// org-by-id lookup. The query must select exactly the four fields
// downstream consumers  need.
func TestGetOrgByID_ReturnsRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	expectedQuery := `SELECT id, name, domain, "atomWorkspaceId" FROM "Org" WHERE id = \$1`
	mock.ExpectQuery(expectedQuery).
		WithArgs(int32(42)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "domain", "atomWorkspaceId"}).
			AddRow(int32(42), "Atom Org A", "orga", strPtr("atom-org-a-kind")))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	q := &Queries{db: mock}
	got, err := q.GetOrgByID(ctx, 42)
	if err != nil {
		t.Fatalf("GetOrgByID: %v", err)
	}
	if got.ID != 42 {
		t.Errorf("ID: got %d, want 42", got.ID)
	}
	if got.Name != "Atom Org A" {
		t.Errorf("Name: got %q", got.Name)
	}
	if got.Domain != "orga" {
		t.Errorf("Domain: got %q", got.Domain)
	}
	if got.AtomWorkspaceID == nil || *got.AtomWorkspaceID != "atom-org-a-kind" {
		t.Errorf("AtomWorkspaceID: got %v", got.AtomWorkspaceID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetOrgByID_NotFound asserts the not-found sentinel so callers
// (auth) can branch on it explicitly without inspecting pgx error
// strings.
func TestGetOrgByID_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, name, domain, "atomWorkspaceId" FROM "Org" WHERE id = \$1`).
		WithArgs(int32(404)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "domain", "atomWorkspaceId"}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	q := &Queries{db: mock}
	got, err := q.GetOrgByID(ctx, 404)
	if !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("expected ErrOrgNotFound, got err=%v org=%v", err, got)
	}
}

// TestGetOrgByDomain_ReturnsRow exercises domain-based lookup used by
// Atom-control-plane routes that route by ${domain}/api/... pattern.
func TestGetOrgByDomain_ReturnsRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, name, domain, "atomWorkspaceId" FROM "Org" WHERE domain = \$1`).
		WithArgs("orga").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "domain", "atomWorkspaceId"}).
			AddRow(int32(2), "Atom Org A", "orga", strPtr("atom-org-a-kind")))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	q := &Queries{db: mock}
	got, err := q.GetOrgByDomain(ctx, "orga")
	if err != nil {
		t.Fatalf("GetOrgByDomain: %v", err)
	}
	if got.ID != 2 || got.Domain != "orga" {
		t.Errorf("unexpected row: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetOrgByDomain_Empty asserts callers cannot accidentally fan out
// a SELECT * with an empty domain.
func TestGetOrgByDomain_Empty(t *testing.T) {
	q := &Queries{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if _, err := q.GetOrgByDomain(ctx, ""); !errors.Is(err, ErrEmptyDomain) {
		t.Fatalf("expected ErrEmptyDomain, got %v", err)
	}
}

// TestGetOrgByDomain_NotFound covers the empty-result-set branch that
// the empty-input test does not reach — a syntactically valid domain
// that doesn't resolve to any row must still return ErrOrgNotFound.
func TestGetOrgByDomain_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, name, domain, "atomWorkspaceId" FROM "Org" WHERE domain = \$1`).
		WithArgs("ghost").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "domain", "atomWorkspaceId"}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	q := &Queries{db: mock}
	if _, err := q.GetOrgByDomain(ctx, "ghost"); !errors.Is(err, ErrOrgNotFound) {
		t.Fatalf("expected ErrOrgNotFound, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetOrgByID_InvalidID locks the boundary guard that catches the
// zero-value-bug-in-caller case before any DB round-trip. Org.id is
// a SERIAL starting at 1, so id <= 0 is never a real row.
func TestGetOrgByID_InvalidID(t *testing.T) {
	q := &Queries{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	for _, id := range []int32{0, -1, -100} {
		if _, err := q.GetOrgByID(ctx, id); !errors.Is(err, ErrInvalidOrgID) {
			t.Errorf("GetOrgByID(%d): got %v, want ErrInvalidOrgID", id, err)
		}
	}
}

// TestGetOrgByApiKeyHash_ReturnsOrg locks the JOIN query that auth
// uses to map an API-key SHA-256 hash to its owning org. The auth
// middleware never accepts raw key values — only hashes — so the SQL
// must accept a hash parameter, never plaintext.
func TestGetOrgByApiKeyHash_ReturnsOrg(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	hash := "sha256:abcdef0123"
	mock.ExpectQuery(`SELECT o.id, o.name, o.domain, o."atomWorkspaceId" FROM "ApiKey" ak JOIN "Org" o ON o.id = ak."orgId" WHERE ak.hash = \$1`).
		WithArgs(hash).
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "domain", "atomWorkspaceId"}).
			AddRow(int32(7), "Atom Org A", "orga", strPtr("atom-org-a-kind")))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	q := &Queries{db: mock}
	got, err := q.GetOrgByApiKeyHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetOrgByApiKeyHash: %v", err)
	}
	if got.ID != 7 {
		t.Errorf("ID: got %d, want 7", got.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestGetOrgByApiKeyHash_NotFound asserts the not-found sentinel so
// auth can return 401 cleanly when a key isn't recognised.
func TestGetOrgByApiKeyHash_NotFound(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock pool: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT o.id, o.name, o.domain, o."atomWorkspaceId" FROM "ApiKey" ak JOIN "Org" o ON o.id = ak."orgId" WHERE ak.hash = \$1`).
		WithArgs("unknown").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "domain", "atomWorkspaceId"}))

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	q := &Queries{db: mock}
	if _, err := q.GetOrgByApiKeyHash(ctx, "unknown"); !errors.Is(err, ErrApiKeyNotFound) {
		t.Fatalf("expected ErrApiKeyNotFound, got %v", err)
	}
}

// TestGetOrgByApiKeyHash_EmptyHash defends against an auth bug where
// a forgotten hash value would result in a free org lookup.
func TestGetOrgByApiKeyHash_EmptyHash(t *testing.T) {
	q := &Queries{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if _, err := q.GetOrgByApiKeyHash(ctx, ""); !errors.Is(err, ErrEmptyHash) {
		t.Fatalf("expected ErrEmptyHash, got %v", err)
	}
}

// TestQueries_FromPool exercises Pool.Queries()'s nil-safe guard:
// calling it on a nil *Pool must return a non-nil *Queries (with an
// internal db == nil) rather than panicking. Auth  depends
// on this for compile-time type checks where the pool may not yet be
// initialised. The previous version of this test used `_ = p.Queries`
// (a method-value reference) which compiled but never invoked the
// method — the nil-safe branch was uncovered.
func TestQueries_FromPool(t *testing.T) {
	var p *Pool
	q := NewQueries(p)
	if q == nil {
		t.Fatalf("Pool.Queries() returned nil for nil receiver; want a usable *Queries")
	}
}

// strPtr is a test helper for nullable string columns. The
// atomWorkspaceId column in the Org table is NULLable in the DB
// schema; reflect that in Go via *string.
func strPtr(s string) *string { return &s }
