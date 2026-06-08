// Auth lookup: a single JOIN produces the (user, org, role) tuple
// the auth middleware needs in one round-trip — equivalent to the
// `membership?.role ?? OrgRole.GUEST` fallback rule. If the
// API-key creator is no longer a member of the owning org, the
// LEFT JOIN yields NULL and codeintel coerces to GUEST — never a
// null-deref, never a thrown error.
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AuthLookup is the projection of (User, Org, UserToOrg.role,
// ApiKey.hash) that the auth middleware needs to build an AuthContext.
// Defined in the db package because every field is a direct column
// from the JOIN — keeping the type here means no marshalling glue.
//
// Role is a raw string so the db package stays free of the OrgRole
// enum (which lives in the auth package alongside the comparison
// rules). The middleware converts the string to its typed enum.
type AuthLookup struct {
	UserID     string
	UserEmail  *string
	UserName   *string
	Org        Org
	Role       string // "OWNER" | "MEMBER" | "GUEST"
	ApiKeyHash string
}

// authQuery is the single JOIN that replaces three separate
// round-trips. The LEFT JOIN on UserToOrg is critical: a missing
// membership row must yield a NULL role (coerced to GUEST), not
// drop the API-key row from the result set.
//
// SQL is held in a package-level const so a future refactor that
// changes the join shape — and therefore the parity contract — fails
// the apikeys_test.go expectedAuthQuery check immediately.
const authQuery = `SELECT u.id, u.email, u.name, o.id, o.name, o.domain, o."atomWorkspaceId", uto.role, ak.hash FROM "ApiKey" ak JOIN "Org" o ON o.id = ak."orgId" JOIN "User" u ON u.id = ak."createdById" LEFT JOIN "UserToOrg" uto ON uto."orgId" = o.id AND uto."userId" = u.id WHERE ak.hash = $1`

// GetApiKeyAuth resolves an API-key hash to the (user, org, role)
// tuple the middleware needs to admit a request. Returns
// ErrApiKeyNotFound when no row matches and ErrEmptyHash when the
// caller passed an empty argument (a programming error to surface
// instead of a free DB scan).
//
// The role string is "OWNER" | "MEMBER" | "GUEST" — the canonical
// enum values from the UserToOrg.role column, coerced to "GUEST"
// when the LEFT JOIN produced NULL.
func (q *Queries) GetApiKeyAuth(ctx context.Context, hash string) (AuthLookup, error) {
	if hash == "" {
		return AuthLookup{}, ErrEmptyHash
	}

	var (
		lookup  AuthLookup
		rolePtr *string
	)
	err := q.db.QueryRow(ctx, authQuery, hash).Scan(
		&lookup.UserID,
		&lookup.UserEmail,
		&lookup.UserName,
		&lookup.Org.ID,
		&lookup.Org.Name,
		&lookup.Org.Domain,
		&lookup.Org.AtomWorkspaceID,
		&rolePtr,
		&lookup.ApiKeyHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthLookup{}, ErrApiKeyNotFound
	}
	if err != nil {
		return AuthLookup{}, fmt.Errorf("db: GetApiKeyAuth: %w", err)
	}
	if rolePtr == nil {
		lookup.Role = "GUEST"
	} else {
		lookup.Role = *rolePtr
	}
	return lookup, nil
}
