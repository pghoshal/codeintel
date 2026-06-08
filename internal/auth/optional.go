package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"codeintel/internal/db"
)

// OrgMetadata is the parsed shape of the Org.metadata JSON column.
// Unknown fields are silently ignored by encoding/json so adding
// more knobs later is forward-compatible.
type OrgMetadata struct {
	AnonymousAccessEnabled bool `json:"anonymousAccessEnabled,omitempty"`
}

// ParseOrgMetadata decodes the raw metadata JSON column. Returns a
// zero-value OrgMetadata (not an error) when the input is nil or
// empty — a missing column is treated as "no metadata", never a
// parse failure.
//
// A genuinely malformed JSON value returns an error so callers can
// log and surface it.
func ParseOrgMetadata(raw []byte) (OrgMetadata, error) {
	if len(raw) == 0 {
		return OrgMetadata{}, nil
	}
	var m OrgMetadata
	if err := json.Unmarshal(raw, &m); err != nil {
		return OrgMetadata{}, err
	}
	return m, nil
}

// OptionalAuthConfig groups the configuration the optional-auth
// resolver needs but the required-auth resolver does not.
type OptionalAuthConfig struct {
	// SingleTenantOrgID is the org id used to resolve anonymous
	// requests.
	SingleTenantOrgID int32
	// EncryptionKey is the HMAC key for API-key hashing — used only
	// when credentials are present (the API-key strategy runs).
	EncryptionKey string
	// Queries is the db surface. *db.Queries satisfies it.
	Queries OptionalQuerier
}

// ResolveOptionalFromHeaders is the optional-auth resolver. If
// credentials are present, the API-key strategy runs and any
// failure surfaces verbatim — there is no fall-through to anonymous
// after a credential-present failure (that would let a bad key
// silently downgrade to anonymous, a security hole).
//
// If no credentials are present, the single-tenant org's metadata
// gates whether to admit the request as anonymous.
//
// On anonymous admission, the returned AuthContext has:
//   - UserID = ""
//   - Role = OrgRoleGuest
//   - AuthSource = "anonymous"
//   - Org populated from the single-tenant lookup
//   - ApiKeyHash = ""
func ResolveOptionalFromHeaders(ctx context.Context, h http.Header, cfg OptionalAuthConfig) (AuthContext, error) {
	if extractApiKey(h) != "" {
		return ResolveFromHeaders(ctx, h, cfg.EncryptionKey, cfg.Queries)
	}

	if cfg.SingleTenantOrgID <= 0 {
		return AuthContext{}, ErrNoCredentials
	}

	org, err := cfg.Queries.GetOrgWithMetadata(ctx, cfg.SingleTenantOrgID)
	if err != nil {
		if errors.Is(err, db.ErrOrgNotFound) {
			return AuthContext{}, ErrNoCredentials
		}
		return AuthContext{}, err
	}

	meta, err := ParseOrgMetadata(org.Metadata)
	if err != nil {
		// Malformed metadata fails closed — anonymous stays
		// disabled.
		return AuthContext{}, ErrNoCredentials
	}
	if !meta.AnonymousAccessEnabled {
		return AuthContext{}, ErrNoCredentials
	}

	return AuthContext{
		UserID:     "",
		Org:        org,
		Role:       OrgRoleGuest,
		AuthSource: "anonymous",
	}, nil
}
