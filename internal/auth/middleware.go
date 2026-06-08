package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"codeintel/internal/db"
)

// OrgRole is the role a user holds within an org. The enum values
// are string-typed so comparisons stay readable and direct equality
// against the DB column works without conversion.
type OrgRole string

const (
	OrgRoleOwner  OrgRole = "OWNER"
	OrgRoleMember OrgRole = "MEMBER"
	OrgRoleGuest  OrgRole = "GUEST"
)

// ApiKeyHeader is the request header carrying the raw API key.
// Kept as an exported constant so handler glue and tests share the
// same name without duplicating the string.
const ApiKeyHeader = "X-Api-Key"

// AuthContext is the resolved per-request authentication state that
// downstream handlers consume. AuthSource lets handlers branch on
// how the identity was established (api-key today; session and
// oauth strategies plug in here later).
type AuthContext struct {
	UserID     string
	UserEmail  *string
	UserName   *string
	Org        db.Org
	Role       OrgRole
	AuthSource string
	ApiKeyHash string
}

// Querier is the minimal db surface the API-key strategy uses.
// Declared here so substitution with a fake in tests doesn't need
// pgxmock. The production *db.Queries satisfies it directly.
type Querier interface {
	GetApiKeyAuth(ctx context.Context, hash string) (db.AuthLookup, error)
	UpdateApiKeyLastUsedAt(ctx context.Context, hash string) error
}

// OptionalQuerier extends Querier with the org-metadata lookup the
// anonymous-access path uses. Kept as a separate interface so
// handlers that need only the required-auth surface don't have to
// wire the extra method.
type OptionalQuerier interface {
	Querier
	GetOrgWithMetadata(ctx context.Context, id int32) (db.Org, error)
}

// Strategy is the per-request auth-resolution contract. Multiple
// strategies (api-key, session, oauth) compose into a chain via
// ChainedResolver; each Resolve call returns either a populated
// AuthContext, ErrNotApplicable (skip this strategy, try the next),
// or a hard failure that propagates.
type Strategy interface {
	Resolve(ctx context.Context, r *http.Request) (AuthContext, error)
}

// ErrNotApplicable is returned by a Strategy when the request does
// not carry the credentials it understands. The chain advances to
// the next strategy.
var ErrNotApplicable = errors.New("auth: strategy not applicable to this request")

// Sentinel errors describing the various auth-failure modes the
// HTTP layer must translate into 401 responses. Each one is wire-
// surface relevant and used by handlers via errors.Is.
var (
	ErrNoCredentials = errors.New("auth: no api-key credentials in request")
	ErrMalformedKey  = errors.New("auth: api-key value is malformed")
	ErrUnknownKey    = errors.New("auth: api-key hash did not resolve")
	ErrGuestRole     = errors.New("auth: resolved user has GUEST role in target org")
)

// ApiKeyStrategy implements Strategy for header- and bearer-supplied
// API keys.
type ApiKeyStrategy struct {
	EncryptionKey string
	Queries       Querier
}

// Resolve extracts an API key from the request headers, hashes it,
// looks up the user/org/role tuple, bumps lastUsedAt, and returns
// the populated AuthContext. A request that carries no recognised
// API-key credential returns ErrNotApplicable.
func (s *ApiKeyStrategy) Resolve(ctx context.Context, r *http.Request) (AuthContext, error) {
	raw := extractApiKey(r.Header)
	if raw == "" {
		return AuthContext{}, ErrNotApplicable
	}
	secret, err := ParseApiKey(raw)
	if err != nil {
		return AuthContext{}, ErrMalformedKey
	}
	hash := HashSecret(s.EncryptionKey, secret)

	lookup, err := s.Queries.GetApiKeyAuth(ctx, hash)
	if err != nil {
		if errors.Is(err, db.ErrApiKeyNotFound) {
			return AuthContext{}, ErrUnknownKey
		}
		return AuthContext{}, err
	}

	if OrgRole(lookup.Role) == OrgRoleGuest {
		return AuthContext{}, ErrGuestRole
	}

	authCtx := AuthContext{
		UserID:     lookup.UserID,
		UserEmail:  lookup.UserEmail,
		UserName:   lookup.UserName,
		Org:        lookup.Org,
		Role:       OrgRole(lookup.Role),
		AuthSource: "api_key",
		ApiKeyHash: lookup.ApiKeyHash,
	}

	// The lastUsedAt bump is synchronous so a row-state divergence
	// across replicas is bounded. A transient UPDATE failure (for
	// example a hot-standby read-only blip) propagates here so the
	// caller can surface 500 — every authentication event must
	// produce a consistent audit row.
	if err := s.Queries.UpdateApiKeyLastUsedAt(ctx, hash); err != nil {
		return AuthContext{}, err
	}
	return authCtx, nil
}

// hasApiKeyPrefix reports whether s begins with the recognised
// wire-format prefix. Used to gate the Authorization: Bearer
// fallback so OAuth-shaped tokens are not hashed as API keys.
func hasApiKeyPrefix(s string) bool {
	return strings.HasPrefix(s, ApiKeyPrefix)
}

// extractApiKey returns the raw key string from the request headers,
// or "" if none is present.
//
// Precedence:
//  1. The dedicated API-key header (most specific).
//  2. Authorization: Bearer <key> when the bearer carries a
//     recognised API-key prefix. Bearer tokens that don't match an
//     API-key prefix are left for other strategies (OAuth, session).
func extractApiKey(h http.Header) string {
	if v := h.Get(ApiKeyHeader); v != "" {
		return v
	}
	authz := h.Get("Authorization")
	if bearer, ok := strings.CutPrefix(authz, "Bearer "); ok {
		if hasApiKeyPrefix(bearer) {
			return bearer
		}
	}
	return ""
}

// ResolveFromHeaders is the convenience wrapper handlers call when
// they need an authenticated request and don't care which strategy
// produced the context. Internally it constructs the api-key
// strategy and invokes it directly — keeping a single function
// signature stable while the strategy surface evolves.
//
// A request that carries no recognised credential returns
// ErrNoCredentials so the HTTP layer can emit a uniform 401 body.
func ResolveFromHeaders(ctx context.Context, h http.Header, encryptionKey string, q Querier) (AuthContext, error) {
	strategy := &ApiKeyStrategy{EncryptionKey: encryptionKey, Queries: q}
	r := &http.Request{Header: h}
	authCtx, err := strategy.Resolve(ctx, r)
	if errors.Is(err, ErrNotApplicable) {
		return AuthContext{}, ErrNoCredentials
	}
	return authCtx, err
}
