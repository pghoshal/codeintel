// Package db carries the typed database surface every higher-level
// package consumes. Each query helper is a method on *Queries and
// uses pgx with positional placeholders; the pgxQuerier interface
// lets unit tests substitute a mock without booting Postgres.
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgxTxBeginner is the minimal surface needed to start a pgx
// transaction. *pgxpool.Pool satisfies it directly. Defined here so
// transactional helpers don't have to bring pgxpool into their
// signatures and tests can supply fakes.
type pgxTxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// ErrOrgNotFound is returned when an org lookup yields zero rows.
// Callers use errors.Is to branch cleanly into a 404 / 401 response.
var ErrOrgNotFound = errors.New("db: org not found")

// ErrApiKeyNotFound is returned when an api-key hash lookup yields
// zero rows. The auth layer converts this into a 401 response.
var ErrApiKeyNotFound = errors.New("db: api key not found")

// ErrEmptyDomain rejects a GetOrgByDomain call with an empty
// argument. An empty domain at the boundary catches caller-side
// zero-value bugs before producing a wasted database round-trip.
var ErrEmptyDomain = errors.New("db: domain argument is required")

// ErrEmptyHash rejects a GetOrgByApiKeyHash call with an empty
// argument. An empty hash would never be a real key, but a forgotten
// hash computation would silently match no rows — fail loud.
var ErrEmptyHash = errors.New("db: api key hash argument is required")

// ErrInvalidOrgID rejects a GetOrgByID call with a non-positive id.
// Autoincrement primary keys start at 1, so 0 / negative are never
// real rows; failing at the boundary surfaces a caller-side zero-
// value bug instead of wasting a Postgres round-trip.
var ErrInvalidOrgID = errors.New("db: org id must be positive")

// Org projects the columns of the Org table the higher layers
// consume. Metadata is the raw JSON bytes of the org's metadata
// column — callers parse it through auth.ParseOrgMetadata (or
// future kind-specific helpers) so the db layer stays free of
// schema knowledge. A nil Metadata means the column was NULL.
type Org struct {
	ID              int32
	Name            string
	Domain          string
	AtomWorkspaceID *string
	Metadata        []byte
}

// pgxQuerier is the minimal surface of *pgxpool.Pool that the typed
// query helpers depend on. Defined as an interface so a fake can
// substitute it in unit tests without booting Postgres.
type pgxQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Queries groups the typed query helpers. Each query is a method on
// *Queries so callers compose them through one handle:
// `q := db.NewQueries(pool)` in service code.
type Queries struct {
	db pgxQuerier
}

// NewQueries binds a Queries handle to the supplied pool. nil-safe
// to support compile-time type-chain checks in tests.
// Replaces the former `(*Pool).Queries()` method now that *Pool is
// a type alias for *dbpool.Pool (Go forbids methods on non-local
// types).
func NewQueries(p *Pool) *Queries {
	if p == nil {
		return &Queries{}
	}
	return &Queries{db: p.Pool}
}

// GetOrgByID returns the org row for the supplied numeric id, or
// ErrOrgNotFound when no row exists. Non-positive id values are
// rejected with ErrInvalidOrgID before any DB round-trip.
func (q *Queries) GetOrgByID(ctx context.Context, id int32) (Org, error) {
	if id <= 0 {
		return Org{}, ErrInvalidOrgID
	}
	const query = `SELECT id, name, domain, "atomWorkspaceId" FROM "Org" WHERE id = $1`
	var org Org
	err := q.db.QueryRow(ctx, query, id).Scan(&org.ID, &org.Name, &org.Domain, &org.AtomWorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, ErrOrgNotFound
	}
	if err != nil {
		return Org{}, fmt.Errorf("db: GetOrgByID: %w", err)
	}
	return org, nil
}

// GetOrgWithMetadata returns the org row plus its raw metadata JSON.
// The auth layer's optional resolver consumes this to gate
// anonymous access via `metadata.anonymousAccessEnabled`. Kept
// separate from GetOrgByID so the cheap-projection callers don't
// pay the extra column read.
func (q *Queries) GetOrgWithMetadata(ctx context.Context, id int32) (Org, error) {
	if id <= 0 {
		return Org{}, ErrInvalidOrgID
	}
	const query = `SELECT id, name, domain, "atomWorkspaceId", metadata FROM "Org" WHERE id = $1`
	var org Org
	err := q.db.QueryRow(ctx, query, id).Scan(&org.ID, &org.Name, &org.Domain, &org.AtomWorkspaceID, &org.Metadata)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, ErrOrgNotFound
	}
	if err != nil {
		return Org{}, fmt.Errorf("db: GetOrgWithMetadata: %w", err)
	}
	return org, nil
}

// GetOrgByDomain returns the org with the supplied domain slug.
// Empty input is rejected at the boundary so a forgotten domain
// header cannot accidentally trigger a full-table scan.
func (q *Queries) GetOrgByDomain(ctx context.Context, domain string) (Org, error) {
	if domain == "" {
		return Org{}, ErrEmptyDomain
	}
	const query = `SELECT id, name, domain, "atomWorkspaceId" FROM "Org" WHERE domain = $1`
	var org Org
	err := q.db.QueryRow(ctx, query, domain).Scan(&org.ID, &org.Name, &org.Domain, &org.AtomWorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, ErrOrgNotFound
	}
	if err != nil {
		return Org{}, fmt.Errorf("db: GetOrgByDomain: %w", err)
	}
	return org, nil
}

// GetOrgByApiKeyHash returns the org that owns the supplied API-key
// hash, or ErrApiKeyNotFound when no row exists.
//
// The auth layer's HashSecret produces the lowercase hex digest
// passed here; the ApiKey.hash column stores the byte-identical
// digest, so the SQL match is a direct equality. If a caller
// accidentally passes an un-hashed value the SELECT yields zero
// rows and this function returns ErrApiKeyNotFound — never a parse
// error, never a DB error, never a log of the raw input.
//
// Callers should prefer GetApiKeyAuth (apikeys.go) which
// resolves user + org + role in a single JOIN. This helper survives
// for code paths that only need the org row.
func (q *Queries) GetOrgByApiKeyHash(ctx context.Context, hash string) (Org, error) {
	if hash == "" {
		return Org{}, ErrEmptyHash
	}
	const query = `SELECT o.id, o.name, o.domain, o."atomWorkspaceId" FROM "ApiKey" ak JOIN "Org" o ON o.id = ak."orgId" WHERE ak.hash = $1`
	var org Org
	err := q.db.QueryRow(ctx, query, hash).Scan(&org.ID, &org.Name, &org.Domain, &org.AtomWorkspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Org{}, ErrApiKeyNotFound
	}
	if err != nil {
		return Org{}, fmt.Errorf("db: GetOrgByApiKeyHash: %w", err)
	}
	return org, nil
}
