package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"codeintel/internal/db"
)

// fakeQuerier is a hand-rolled Querier substitute used by the auth
// tests. Avoiding pgxmock here keeps the middleware tests focused on
// header parsing and control flow rather than SQL — the SQL itself
// is exercised by the db package tests.
type fakeQuerier struct {
	lookupHash    string
	lookupResult  db.AuthLookup
	lookupErr     error
	lastUsedCalls []string
	lastUsedErr   error
}

func (f *fakeQuerier) GetApiKeyAuth(ctx context.Context, hash string) (db.AuthLookup, error) {
	f.lookupHash = hash
	return f.lookupResult, f.lookupErr
}

func (f *fakeQuerier) UpdateApiKeyLastUsedAt(ctx context.Context, hash string) error {
	f.lastUsedCalls = append(f.lastUsedCalls, hash)
	return f.lastUsedErr
}

// validLookup returns a canonical OWNER-role AuthLookup used by the
// happy-path tests.
func validLookup(hash string) db.AuthLookup {
	return db.AuthLookup{
		UserID:    "u-1",
		UserEmail: strPtr("owner@example.com"),
		UserName:  strPtr("Owner"),
		Org: db.Org{
			ID:              7,
			Name:            "Org A",
			Domain:          "orga",
			AtomWorkspaceID: strPtr("ws-orga"),
		},
		Role:       "OWNER",
		ApiKeyHash: hash,
	}
}

func strPtr(s string) *string { return &s }

// TestResolveFromHeaders_HappyPathDedicatedHeader locks the canonical
// path: a client sends the dedicated API-key header with a well-
// formed key whose hash resolves to an OWNER membership.
// AuthContext must carry through every field of AuthLookup, source
// must be "api_key", and lastUsedAt must be bumped exactly once.
func TestResolveFromHeaders_HappyPathDedicatedHeader(t *testing.T) {
	const encKey = "test-encryption-key-32-bytes-long"
	secret := "deadbeef"
	expectedHash := HashSecret(encKey, secret)

	h := http.Header{}
	h.Set(ApiKeyHeader, ApiKeyPrefix+secret)

	fq := &fakeQuerier{lookupResult: validLookup(expectedHash)}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	authCtx, err := ResolveFromHeaders(ctx, h, encKey, fq)
	if err != nil {
		t.Fatalf("ResolveFromHeaders: %v", err)
	}
	if fq.lookupHash != expectedHash {
		t.Errorf("lookup hash: got %q, want %q", fq.lookupHash, expectedHash)
	}
	if authCtx.UserID != "u-1" {
		t.Errorf("UserID: got %q", authCtx.UserID)
	}
	if authCtx.Org.ID != 7 {
		t.Errorf("Org.ID: got %d", authCtx.Org.ID)
	}
	if authCtx.Role != OrgRoleOwner {
		t.Errorf("Role: got %q, want OWNER", authCtx.Role)
	}
	if authCtx.AuthSource != "api_key" {
		t.Errorf("AuthSource: got %q, want api_key", authCtx.AuthSource)
	}
	if len(fq.lastUsedCalls) != 1 || fq.lastUsedCalls[0] != expectedHash {
		t.Errorf("UpdateApiKeyLastUsedAt: got calls %v, want exactly one with hash %q", fq.lastUsedCalls, expectedHash)
	}
}

// TestResolveFromHeaders_HappyPathBearerAuthorization confirms the
// resolver falls back to the Authorization: Bearer header when the
// dedicated header is absent. Bearer-shaped tokens that don't carry
// an API-key prefix are NOT consumed here (those are reserved for
// other strategies).
func TestResolveFromHeaders_HappyPathBearerAuthorization(t *testing.T) {
	const encKey = "k"
	secret := "cafebabe"
	expectedHash := HashSecret(encKey, secret)

	h := http.Header{}
	h.Set("Authorization", "Bearer "+ApiKeyPrefix+secret)

	fq := &fakeQuerier{lookupResult: validLookup(expectedHash)}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	authCtx, err := ResolveFromHeaders(ctx, h, encKey, fq)
	if err != nil {
		t.Fatalf("ResolveFromHeaders: %v", err)
	}
	if authCtx.AuthSource != "api_key" {
		t.Errorf("AuthSource: got %q, want api_key", authCtx.AuthSource)
	}
}

// TestResolveFromHeaders_PrefersDedicatedHeaderOverBearer encodes
// the precedence rule: the dedicated header is checked before the
// Authorization: Bearer fallback. If a request carries both, the
// dedicated header wins and Bearer is never parsed.
func TestResolveFromHeaders_PrefersDedicatedHeaderOverBearer(t *testing.T) {
	const encKey = "k"
	headerSecret := "headersec"
	bearerSecret := "bearersec"
	expectedHash := HashSecret(encKey, headerSecret)

	h := http.Header{}
	h.Set(ApiKeyHeader, ApiKeyPrefix+headerSecret)
	h.Set("Authorization", "Bearer "+ApiKeyPrefix+bearerSecret)

	fq := &fakeQuerier{lookupResult: validLookup(expectedHash)}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	if _, err := ResolveFromHeaders(ctx, h, encKey, fq); err != nil {
		t.Fatalf("ResolveFromHeaders: %v", err)
	}
	if fq.lookupHash != expectedHash {
		t.Errorf("expected lookup with header-hash %q, got %q (bearer was wrongly used)", expectedHash, fq.lookupHash)
	}
}

// TestResolveFromHeaders_NoCredentials confirms that a request with
// no recognised auth header returns ErrNoCredentials so the HTTP
// layer can translate to a 401.
func TestResolveFromHeaders_NoCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveFromHeaders(ctx, http.Header{}, "k", &fakeQuerier{})
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials", err)
	}
}

// TestResolveFromHeaders_MalformedKey covers ParseApiKey rejection —
// a header is present but missing or carrying an unrecognised
// prefix. Must NOT hit the database.
func TestResolveFromHeaders_MalformedKey(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"dedicated_header_unprefixed", ApiKeyHeader, "raw-no-prefix"},
		{"dedicated_header_empty_secret", ApiKeyHeader, ApiKeyPrefix},
		{"bearer_with_apikey_prefix_then_empty", "Authorization", "Bearer " + ApiKeyPrefix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set(tc.header, tc.value)

			fq := &fakeQuerier{}
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_, err := ResolveFromHeaders(ctx, h, "k", fq)
			if !errors.Is(err, ErrMalformedKey) {
				t.Fatalf("got %v, want ErrMalformedKey", err)
			}
			if fq.lookupHash != "" {
				t.Fatalf("DB should NOT have been queried, got lookup hash %q", fq.lookupHash)
			}
		})
	}
}

// TestResolveFromHeaders_UnknownKey covers the case where a
// well-formed key's hash does not match any ApiKey row.
func TestResolveFromHeaders_UnknownKey(t *testing.T) {
	h := http.Header{}
	h.Set(ApiKeyHeader, ApiKeyPrefix+"unknown")
	fq := &fakeQuerier{lookupErr: db.ErrApiKeyNotFound}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveFromHeaders(ctx, h, "k", fq)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("got %v, want ErrUnknownKey", err)
	}
	if len(fq.lastUsedCalls) != 0 {
		t.Fatalf("UpdateApiKeyLastUsedAt must NOT run on unknown key; got %d calls", len(fq.lastUsedCalls))
	}
}

// TestResolveFromHeaders_GuestRoleIsUnauthenticated locks the
// `!user || role == GUEST` rule — a key whose creator is no longer
// a member of the org is treated as unauthenticated for the
// required-auth surface.
func TestResolveFromHeaders_GuestRoleIsUnauthenticated(t *testing.T) {
	const encKey = "k"
	secret := "orphaned"
	hash := HashSecret(encKey, secret)

	lookup := validLookup(hash)
	lookup.Role = "GUEST"

	h := http.Header{}
	h.Set(ApiKeyHeader, ApiKeyPrefix+secret)
	fq := &fakeQuerier{lookupResult: lookup}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveFromHeaders(ctx, h, encKey, fq)
	if !errors.Is(err, ErrGuestRole) {
		t.Fatalf("got %v, want ErrGuestRole", err)
	}
	if len(fq.lastUsedCalls) != 0 {
		t.Fatalf("UpdateApiKeyLastUsedAt must NOT run for GUEST: got %d calls", len(fq.lastUsedCalls))
	}
}

// TestResolveFromHeaders_BearerWithoutApiKeyPrefixIsNoCredentials
// confirms that a Bearer header carrying a token without an
// API-key prefix is treated as "no credentials" — those tokens are
// reserved for other strategies (session, OAuth).
func TestResolveFromHeaders_BearerWithoutApiKeyPrefixIsNoCredentials(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sboa_oauth-token-value")
	fq := &fakeQuerier{}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveFromHeaders(ctx, h, "k", fq)
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("got %v, want ErrNoCredentials", err)
	}
}

// TestResolveFromHeaders_EmptyEncryptionKey confirms an empty
// encryption key is accepted at the function boundary but produces
// a deterministic hash — never a panic. Production wiring rejects
// empty keys at startup; this guard catches programming errors
// that bypass startup validation.
func TestResolveFromHeaders_EmptyEncryptionKey(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ResolveFromHeaders panicked on empty encKey: %v", r)
		}
	}()
	h := http.Header{}
	h.Set(ApiKeyHeader, "cik_anything")
	fq := &fakeQuerier{lookupErr: db.ErrApiKeyNotFound}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveFromHeaders(ctx, h, "", fq)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("got %v, want ErrUnknownKey", err)
	}
}

// TestResolveFromHeaders_BearerWithExtraWhitespace defends against
// clients that emit double-spaced or tab-separated Bearer headers.
// All such variants collapse to ErrNoCredentials — never silently
// admit post-prefix garbage as a key.
func TestResolveFromHeaders_BearerWithExtraWhitespace(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr error
	}{
		{"double_space", "Bearer  cik_x", ErrNoCredentials},
		{"tab_separator", "Bearer\tcik_x", ErrNoCredentials},
		{"trailing_space_no_token", "Bearer ", ErrNoCredentials},
		{"missing_space", "Bearercik_x", ErrNoCredentials},
		{"lowercase_scheme", "bearer cik_x", ErrNoCredentials},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			h.Set("Authorization", tc.value)
			fq := &fakeQuerier{}
			ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
			defer cancel()
			_, err := ResolveFromHeaders(ctx, h, "k", fq)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Authorization=%q: got %v, want %v", tc.value, err, tc.wantErr)
			}
			if fq.lookupHash != "" {
				t.Errorf("DB should NOT have been queried, got lookup hash %q", fq.lookupHash)
			}
		})
	}
}

// TestResolveFromHeaders_LastUsedErrorFailsRequest locks the
// observable behaviour that an UpdateApiKeyLastUsedAt failure
// propagates to the caller — every authentication event must
// produce a consistent audit row.
func TestResolveFromHeaders_LastUsedErrorFailsRequest(t *testing.T) {
	const encKey = "k"
	secret := "ok"
	hash := HashSecret(encKey, secret)
	wireErr := errors.New("simulated read-only standby")

	h := http.Header{}
	h.Set(ApiKeyHeader, ApiKeyPrefix+secret)
	fq := &fakeQuerier{
		lookupResult: validLookup(hash),
		lastUsedErr:  wireErr,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_, err := ResolveFromHeaders(ctx, h, encKey, fq)
	if err == nil {
		t.Fatalf("ResolveFromHeaders must propagate the lastUsedAt error")
	}
	if !errors.Is(err, wireErr) {
		t.Fatalf("expected wrapped wireErr in cause chain, got %v", err)
	}
}
