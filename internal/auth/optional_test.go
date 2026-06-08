package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"codeintel/internal/db"
)

// fakeOptionalQuerier extends fakeQuerier with GetOrgWithMetadata so
// optional-auth tests can dictate the metadata bytes the anonymous
// path sees.
type fakeOptionalQuerier struct {
	fakeQuerier
	metaOrg db.Org
	metaErr error
}

func (f *fakeOptionalQuerier) GetOrgWithMetadata(ctx context.Context, id int32) (db.Org, error) {
	return f.metaOrg, f.metaErr
}

// TestResolveOptional_CredentialsPresent_FallsThroughToRequired
// confirms the precedence rule: if the request carries an API key,
// optional-auth delegates to required-auth and any failure surfaces
// verbatim (no anonymous fallback after a bad key).
func TestResolveOptional_CredentialsPresent_FallsThroughToRequired(t *testing.T) {
	const encKey = "k"
	secret := "ok"
	hash := HashSecret(encKey, secret)

	fq := &fakeOptionalQuerier{
		fakeQuerier: fakeQuerier{lookupResult: validLookup(hash)},
	}
	h := http.Header{}
	h.Set("X-Api-Key", "cik_"+secret)

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	got, err := ResolveOptionalFromHeaders(ctx, h, OptionalAuthConfig{
		SingleTenantOrgID: 1,
		EncryptionKey:     encKey,
		Queries:           fq,
	})
	if err != nil {
		t.Fatalf("ResolveOptionalFromHeaders: %v", err)
	}
	if got.AuthSource != "api_key" {
		t.Errorf("AuthSource: got %q, want api_key", got.AuthSource)
	}
	if got.Role != OrgRoleOwner {
		t.Errorf("Role: got %q, want OWNER", got.Role)
	}
}

// TestResolveOptional_CredentialsPresentButUnknown_NoFallback
// locks the safety rule that a malformed/unknown key does NOT cause
// anonymous fall-through. The 401 the required path returns must
// propagate.
func TestResolveOptional_CredentialsPresentButUnknown_NoFallback(t *testing.T) {
	fq := &fakeOptionalQuerier{
		fakeQuerier: fakeQuerier{lookupErr: db.ErrApiKeyNotFound},
		// Anonymous IS allowed for this org — but we should NEVER
		// fall through into it after a credential-present failure.
		metaOrg: db.Org{ID: 1, Metadata: []byte(`{"anonymousAccessEnabled":true}`)},
	}
	h := http.Header{}
	h.Set("X-Api-Key", "cik_badkey")

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveOptionalFromHeaders(ctx, h, OptionalAuthConfig{
		SingleTenantOrgID: 1,
		EncryptionKey:     "k",
		Queries:           fq,
	})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("got %v, want ErrUnknownKey (no anonymous fallback)", err)
	}
}

// TestResolveOptional_AnonymousAllowed locks the happy anonymous
// path: no credentials + org has metadata.anonymousAccessEnabled =
// true → AuthContext{Role:GUEST, AuthSource:"anonymous", Org: ...}.
func TestResolveOptional_AnonymousAllowed(t *testing.T) {
	org := db.Org{
		ID:       1,
		Name:     "Single Tenant",
		Domain:   "single",
		Metadata: []byte(`{"anonymousAccessEnabled":true}`),
	}
	fq := &fakeOptionalQuerier{metaOrg: org}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	got, err := ResolveOptionalFromHeaders(ctx, http.Header{}, OptionalAuthConfig{
		SingleTenantOrgID: 1,
		Queries:           fq,
	})
	if err != nil {
		t.Fatalf("ResolveOptionalFromHeaders: %v", err)
	}
	if got.AuthSource != "anonymous" {
		t.Errorf("AuthSource: got %q, want anonymous", got.AuthSource)
	}
	if got.Role != OrgRoleGuest {
		t.Errorf("Role: got %q, want GUEST", got.Role)
	}
	if got.Org.ID != 1 {
		t.Errorf("Org.ID: got %d, want 1", got.Org.ID)
	}
	if got.UserID != "" {
		t.Errorf("UserID: got %q, want empty", got.UserID)
	}
}

// TestResolveOptional_AnonymousDisabled covers the explicit-off path:
// metadata.anonymousAccessEnabled = false (or absent) blocks the
// anonymous fallback and the request gets 401-equivalent.
func TestResolveOptional_AnonymousDisabled(t *testing.T) {
	cases := []struct {
		name string
		meta []byte
	}{
		{"explicit_false", []byte(`{"anonymousAccessEnabled":false}`)},
		{"omitted_key", []byte(`{}`)},
		{"null_metadata", nil},
		{"empty_metadata", []byte{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fq := &fakeOptionalQuerier{metaOrg: db.Org{ID: 1, Metadata: c.meta}}
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_, err := ResolveOptionalFromHeaders(ctx, http.Header{}, OptionalAuthConfig{
				SingleTenantOrgID: 1,
				Queries:           fq,
			})
			if !errors.Is(err, ErrNoCredentials) {
				t.Errorf("metadata=%s: got %v, want ErrNoCredentials", string(c.meta), err)
			}
		})
	}
}

// TestResolveOptional_MalformedMetadataFailsClosed asserts the
// security-critical fail-closed behaviour: if the metadata column
// is non-JSON garbage, we do NOT default to "anonymous allowed".
func TestResolveOptional_MalformedMetadataFailsClosed(t *testing.T) {
	fq := &fakeOptionalQuerier{metaOrg: db.Org{ID: 1, Metadata: []byte(`{not json`)}}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveOptionalFromHeaders(ctx, http.Header{}, OptionalAuthConfig{
		SingleTenantOrgID: 1,
		Queries:           fq,
	})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials (fail-closed on malformed metadata)", err)
	}
}

// TestResolveOptional_OrgNotFound covers a misconfigured deployment
// where the single-tenant org id points at a row that doesn't exist.
func TestResolveOptional_OrgNotFound(t *testing.T) {
	fq := &fakeOptionalQuerier{metaErr: db.ErrOrgNotFound}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveOptionalFromHeaders(ctx, http.Header{}, OptionalAuthConfig{
		SingleTenantOrgID: 999,
		Queries:           fq,
	})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials", err)
	}
}

// TestResolveOptional_InvalidOrgIDRejected covers the boundary
// check on a misconfigured CODEINTEL_SINGLE_TENANT_ORG_ID (zero /
// negative).
func TestResolveOptional_InvalidOrgIDRejected(t *testing.T) {
	fq := &fakeOptionalQuerier{}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveOptionalFromHeaders(ctx, http.Header{}, OptionalAuthConfig{
		SingleTenantOrgID: 0,
		Queries:           fq,
	})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials", err)
	}
}

// TestParseOrgMetadata_EmptyAndNil locks the contract that nil/empty
// inputs return zero-value (not error) — JSONB NULL columns are
// common.
func TestParseOrgMetadata_EmptyAndNil(t *testing.T) {
	for _, raw := range [][]byte{nil, {}} {
		m, err := ParseOrgMetadata(raw)
		if err != nil {
			t.Errorf("ParseOrgMetadata(%v): unexpected error %v", raw, err)
		}
		if m.AnonymousAccessEnabled {
			t.Errorf("expected zero-value (anonymous disabled), got %+v", m)
		}
	}
}
