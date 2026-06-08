package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// fakeAuthQuerier is a hand-rolled stand-in for both the auth.Querier
// surface (GetApiKeyAuth + UpdateApiKeyLastUsedAt) and the handler's
// secrets-lookup Querier surface (ListOrgSecrets). Using one fake for
// both narrows the dependency wiring tests have to express.
type fakeAuthQuerier struct {
	lookupResult       db.AuthLookup
	lookupErr          error
	lastUsedErr        error
	listSecrets        []db.OrgSecret
	listSecretsForOrg  int32
	listSecretsErr     error
	lastUsedCalls      []string
}

func (f *fakeAuthQuerier) GetApiKeyAuth(ctx context.Context, hash string) (db.AuthLookup, error) {
	return f.lookupResult, f.lookupErr
}
func (f *fakeAuthQuerier) UpdateApiKeyLastUsedAt(ctx context.Context, hash string) error {
	f.lastUsedCalls = append(f.lastUsedCalls, hash)
	return f.lastUsedErr
}
func (f *fakeAuthQuerier) ListOrgSecrets(ctx context.Context, orgID int32) ([]db.OrgSecret, error) {
	f.listSecretsForOrg = orgID
	if f.listSecretsErr != nil {
		return nil, f.listSecretsErr
	}
	if f.listSecrets == nil {
		return make([]db.OrgSecret, 0), nil
	}
	return f.listSecrets, nil
}
func (f *fakeAuthQuerier) UpsertOrgSecret(ctx context.Context, p db.UpsertOrgSecretParams) (db.OrgSecret, error) {
	return db.OrgSecret{}, errors.New("UpsertOrgSecret not stubbed in fakeAuthQuerier; tests that need it must use upsertSecretSpy")
}
func (f *fakeAuthQuerier) ListOrgConnectionsForRefcheck(ctx context.Context, orgID int32) ([]db.ConfigOwner, error) {
	return nil, nil
}
func (f *fakeAuthQuerier) ListOrgLanguageModelsForRefcheck(ctx context.Context, orgID int32) ([]db.ConfigOwner, error) {
	return nil, nil
}
func (f *fakeAuthQuerier) DeleteOrgSecret(ctx context.Context, orgID int32, key string) error {
	return nil
}
func (f *fakeAuthQuerier) GetOrgWithMetadata(ctx context.Context, id int32) (db.Org, error) {
	return db.Org{}, db.ErrOrgNotFound
}
func (f *fakeAuthQuerier) ListEnabledOrgLanguageModels(ctx context.Context, orgID int32) ([]db.OrgLanguageModelRow, error) {
	return nil, nil
}
func (f *fakeAuthQuerier) SelectMissingOrgSecretKeys(ctx context.Context, orgID int32, keys []string) ([]string, error) {
	return nil, nil
}
func (f *fakeAuthQuerier) ReplaceOrgLanguageModels(ctx context.Context, orgID int32, models []db.OrgLanguageModelInsert) error {
	return nil
}
func (f *fakeAuthQuerier) ListOrgConnectionsForRead(ctx context.Context, orgID int32) ([]db.ConnectionListRow, error) {
	return nil, nil
}
func (f *fakeAuthQuerier) DeleteOrgConnection(ctx context.Context, orgID int32, connectionID int32) error {
	return nil
}
func (f *fakeAuthQuerier) UpsertOrgConnection(ctx context.Context, p db.UpsertOrgConnectionParams) (db.ConnectionListRow, error) {
	return db.ConnectionListRow{}, nil
}
func (f *fakeAuthQuerier) GetOrgConnectionForUpdate(ctx context.Context, orgID, connectionID int32) (db.ConnectionListRow, error) {
	return db.ConnectionListRow{}, db.ErrConnectionNotFound
}
func (f *fakeAuthQuerier) CheckOrgConnectionNameAvailable(ctx context.Context, orgID int32, name string, excludeID int32) error {
	return nil
}
func (f *fakeAuthQuerier) ConnectionExistsInOrg(ctx context.Context, orgID, connectionID int32) (bool, error) {
	return false, nil
}
func (f *fakeAuthQuerier) GetOrgConnectionMeta(ctx context.Context, orgID, connectionID int32) (db.ConnectionMetaRow, error) {
	return db.ConnectionMetaRow{}, db.ErrConnectionNotFound
}
func (f *fakeAuthQuerier) ListConnectionSyncJobs(ctx context.Context, orgID, connectionID, limit int32) ([]db.ConnectionSyncJobRow, error) {
	return nil, nil
}
func (f *fakeAuthQuerier) CountConnectionRepos(ctx context.Context, orgID, connectionID int32) (int32, error) {
	return 0, nil
}
func (f *fakeAuthQuerier) GetOrgStatusRollup(ctx context.Context, orgID int32) (db.OrgStatusRollup, error) {
	return db.OrgStatusRollup{}, nil
}
func (f *fakeAuthQuerier) ListRecentFailedConnectionSyncJobs(ctx context.Context, orgID, limit int32) ([]db.RecentFailedConnectionSyncJobRow, error) {
	return nil, nil
}
func (f *fakeAuthQuerier) ListRecentFailedRepoIndexingJobs(ctx context.Context, orgID, limit int32) ([]db.RecentFailedRepoIndexingJobRow, error) {
	return nil, nil
}
func (f *fakeAuthQuerier) ListOrgRepos(ctx context.Context, p db.ListOrgReposParams) ([]db.RepoListRow, error) {
	return make([]db.RepoListRow, 0), nil
}
func (f *fakeAuthQuerier) CountOrgRepos(ctx context.Context, p db.CountOrgReposParams) (int32, error) {
	return 0, nil
}

func strPtr(s string) *string { return &s }

// newTestServer builds a Server with discard logging and the supplied
// fake querier as the auth + secrets dependency. Encryption-key is
// fixed so test fixtures stay deterministic.
func newTestServer(fq *fakeAuthQuerier) *Server {
	cfg := Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       fq,
		EncryptionKey: "test-encryption-key-32-bytes-long",
	}
	return NewServer(cfg)
}

// validOwnerLookup returns the canonical "API key resolves to org 7
// OWNER" AuthLookup row used by happy-path tests.
func validOwnerLookup(hash string) db.AuthLookup {
	return db.AuthLookup{
		UserID:    "u-1",
		UserEmail: strPtr("owner@example.com"),
		UserName:  strPtr("Owner"),
		Org: db.Org{
			ID:              7,
			Name:            "Atom Org A",
			Domain:          "orga",
			AtomWorkspaceID: strPtr("atom-org-a-kind"),
		},
		Role:       "OWNER",
		ApiKeyHash: hash,
	}
}

// TestListOrgSecrets_NoCredentials_Returns401 locks the byte-equal
// 401 response for a request that omits the API-key header
// entirely. Body must match the canonical notAuthenticated()
// service-error shape.
func TestListOrgSecrets_NoCredentials_Returns401(t *testing.T) {
	fq := &fakeAuthQuerier{}
	srv := newTestServer(fq)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", got)
	}
	want := `{"statusCode":401,"errorCode":"NOT_AUTHENTICATED","message":"Not authenticated"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
}

// TestListOrgSecrets_UnknownKey_Returns401 confirms an API-key whose
// hash does not match any row produces the SAME 401 body — never
// reveal whether the issue was "no header", "bad header", or "valid
// header but unknown key" (information-leak protection).
func TestListOrgSecrets_UnknownKey_Returns401(t *testing.T) {
	fq := &fakeAuthQuerier{lookupErr: db.ErrApiKeyNotFound}
	srv := newTestServer(fq)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	req.Header.Set("X-Api-Key", "cik_unknownsecret")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
	want := `{"statusCode":401,"errorCode":"NOT_AUTHENTICATED","message":"Not authenticated"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestListOrgSecrets_MalformedKey_Returns401 covers ParseApiKey's
// rejection path. Identical body to the no-credentials case.
func TestListOrgSecrets_MalformedKey_Returns401(t *testing.T) {
	fq := &fakeAuthQuerier{}
	srv := newTestServer(fq)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	req.Header.Set("X-Api-Key", "no-prefix-here")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestListOrgSecrets_GuestRole_Returns401 covers the LEFT-JOIN-miss
// case: a well-formed key whose creator is no longer an org member.
func TestListOrgSecrets_GuestRole_Returns401(t *testing.T) {
	const secret = "guestsec"
	hash := auth.HashSecret("test-encryption-key-32-bytes-long", secret)
	lookup := validOwnerLookup(hash)
	lookup.Role = "GUEST"

	fq := &fakeAuthQuerier{lookupResult: lookup}
	srv := newTestServer(fq)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestListOrgSecrets_MemberRole_Returns403 locks the OWNER-only
// role guard. Non-owners must receive the
// INSUFFICIENT_PERMISSIONS envelope byte-for-byte.
func TestListOrgSecrets_MemberRole_Returns403(t *testing.T) {
	const secret = "membersec"
	hash := auth.HashSecret("test-encryption-key-32-bytes-long", secret)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"

	fq := &fakeAuthQuerier{lookupResult: lookup}
	srv := newTestServer(fq)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can list secrets."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
}

// TestListOrgSecrets_OwnerEmpty_Returns200WithEmptyArray is the
// canonical golden-fixture case: a valid OWNER key on an org with
// no secrets must return 200 + `[]` byte-for-byte.
func TestListOrgSecrets_OwnerEmpty_Returns200WithEmptyArray(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("test-encryption-key-32-bytes-long", secret)

	fq := &fakeAuthQuerier{
		lookupResult: validOwnerLookup(hash),
		// listSecrets nil → handler must coerce to `[]`, not `null`.
	}
	srv := newTestServer(fq)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type: got %q", got)
	}
	if got := rec.Body.String(); got != "[]" {
		t.Fatalf("body: got %q, want %q", got, "[]")
	}
	// Org-scoping: ListOrgSecrets must have been called with the
	// AuthLookup's Org.ID, never the raw header / a hardcoded value.
	if fq.listSecretsForOrg != 7 {
		t.Errorf("ListOrgSecrets called with org %d, want 7", fq.listSecretsForOrg)
	}
}

// TestListOrgSecrets_OwnerWithRows_Returns200WithJSONArray covers
// the non-empty branch. Field order is critical for byte-parity:
// key, createdAt, updatedAt, ref (in that order).
func TestListOrgSecrets_OwnerWithRows_Returns200WithJSONArray(t *testing.T) {
	const secret = "ownersec2"
	hash := auth.HashSecret("test-encryption-key-32-bytes-long", secret)

	// Use fixed UTC timestamps so the JSON encoding is deterministic.
	fq := &fakeAuthQuerier{
		lookupResult: validOwnerLookup(hash),
		listSecrets: []db.OrgSecret{
			{Key: "GH_TOKEN"},
			{Key: "GITLAB_TOKEN"},
		},
	}
	// Stamp deterministic timestamps.
	fq.listSecrets[0].CreatedAt = parseFixedTime("2025-01-15T10:30:00.000Z")
	fq.listSecrets[0].UpdatedAt = parseFixedTime("2025-01-15T10:30:00.000Z")
	fq.listSecrets[1].CreatedAt = parseFixedTime("2025-02-20T08:15:00.500Z")
	fq.listSecrets[1].UpdatedAt = parseFixedTime("2025-02-20T08:15:00.500Z")

	srv := newTestServer(fq)
	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}

	// (".000Z" / ".500Z" three-digit millisecond precision).
	want := `[{"key":"GH_TOKEN","createdAt":"2025-01-15T10:30:00.000Z","updatedAt":"2025-01-15T10:30:00.000Z","ref":{"secretRef":"GH_TOKEN"}},{"key":"GITLAB_TOKEN","createdAt":"2025-02-20T08:15:00.500Z","updatedAt":"2025-02-20T08:15:00.500Z","ref":{"secretRef":"GITLAB_TOKEN"}}]`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
}

// TestListOrgSecrets_BumpsLastUsedAt confirms the auth middleware's
// lastUsedAt UPDATE actually runs on the happy path — DB-row parity.
func TestListOrgSecrets_BumpsLastUsedAt(t *testing.T) {
	const secret = "ownersec3"
	hash := auth.HashSecret("test-encryption-key-32-bytes-long", secret)

	fq := &fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}
	srv := newTestServer(fq)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(fq.lastUsedCalls) != 1 {
		t.Fatalf("expected 1 lastUsedAt call, got %d", len(fq.lastUsedCalls))
	}
}

// TestListOrgSecrets_DBError_Returns500 covers the DB-failure path
// (other than not-found) so an outage surfaces as a 500, not a
// silent empty 200.
func TestListOrgSecrets_DBError_Returns500(t *testing.T) {
	const secret = "ownersec4"
	hash := auth.HashSecret("test-encryption-key-32-bytes-long", secret)

	fq := &fakeAuthQuerier{
		lookupResult:   validOwnerLookup(hash),
		listSecretsErr: errors.New("simulated db outage"),
	}
	srv := newTestServer(fq)

	req := httptest.NewRequest(http.MethodGet, "/api/secrets", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	// Response body must be the generic UNEXPECTED_ERROR shape; raw
	// DB error must NOT leak to the client.
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response body is not JSON: %v (%q)", err, rec.Body.String())
	}
	if got, _ := body["errorCode"].(string); got != "UNEXPECTED_ERROR" {
		t.Errorf("errorCode: got %q, want UNEXPECTED_ERROR", got)
	}
	if msg, _ := body["message"].(string); strings.Contains(msg, "simulated db outage") {
		t.Errorf("raw DB error leaked into response: %q", msg)
	}
}

// upsertSecretSpy extends fakeAuthQuerier with PUT-handler hooks so
// we can assert the upsert was called with the encrypted ciphertext
// (NEVER the plaintext) and the right org-scoped arguments.
type upsertSecretSpy struct {
	fakeAuthQuerier
	upsertParams *db.UpsertOrgSecretParams
	upsertResult db.OrgSecret
	upsertErr    error
}

func (s *upsertSecretSpy) UpsertOrgSecret(ctx context.Context, p db.UpsertOrgSecretParams) (db.OrgSecret, error) {
	s.upsertParams = &p
	if s.upsertErr != nil {
		return db.OrgSecret{}, s.upsertErr
	}
	return s.upsertResult, nil
}

// TestPutOrgSecret_OwnerCreatesRow_Returns200WithJSON locks the
// happy-path wire shape:
//   - 200 status
//   - response body byte-equals
//     {"key":"NEW_KEY","createdAt":"<iso>","updatedAt":"<iso>","ref":{"secretRef":"NEW_KEY"}}
//   - upsert was called with encrypted ciphertext (NOT plaintext)
//   - org-id came from authCtx, never the request
func TestPutOrgSecret_OwnerCreatesRow_Returns200WithJSON(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)

	created := parseFixedTime("2025-06-01T12:00:00.000Z")
	spy := &upsertSecretSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		upsertResult:    db.OrgSecret{Key: "NEW_KEY", CreatedAt: created, UpdatedAt: created},
	}
	cfg := Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: encKey,
	}
	srv := NewServer(cfg)

	body := strings.NewReader(`{"key":"NEW_KEY","value":"super-secret-value"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets", body)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	want := `{"key":"NEW_KEY","createdAt":"2025-06-01T12:00:00.000Z","updatedAt":"2025-06-01T12:00:00.000Z","ref":{"secretRef":"NEW_KEY"}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
	if spy.upsertParams == nil {
		t.Fatalf("UpsertOrgSecret was not called")
	}
	if spy.upsertParams.OrgID != 7 {
		t.Errorf("OrgID: got %d, want 7 (from authCtx, NEVER request)", spy.upsertParams.OrgID)
	}
	if spy.upsertParams.Key != "NEW_KEY" {
		t.Errorf("Key: got %q", spy.upsertParams.Key)
	}
	if spy.upsertParams.EncryptedValue == "super-secret-value" {
		t.Errorf("EncryptedValue equals plaintext — encrypt step was skipped")
	}
	if spy.upsertParams.EncryptedValue == "" || spy.upsertParams.IV == "" {
		t.Errorf("EncryptedValue/IV empty: %+v", spy.upsertParams)
	}
	// Confirm we can decrypt back to the original value (full
	// roundtrip parity).
	plain, err := auth.Decrypt(encKey, spy.upsertParams.IV, spy.upsertParams.EncryptedValue)
	if err != nil {
		t.Fatalf("Decrypt roundtrip: %v", err)
	}
	if plain != "super-secret-value" {
		t.Errorf("roundtrip plaintext: got %q, want %q", plain, "super-secret-value")
	}
}

// TestPutOrgSecret_MemberRole_Returns403 confirms the OWNER guard
// applies to the write path identically to the read path. The
// body must be byte-equal to the canonical "Only organization
// owners can create or update secrets." message.
func TestPutOrgSecret_MemberRole_Returns403(t *testing.T) {
	const secret = "membersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"

	spy := &upsertSecretSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup}}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: encKey,
	})

	body := strings.NewReader(`{"key":"NEW_KEY","value":"v"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets", body)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can create or update secrets."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
	if spy.upsertParams != nil {
		t.Fatalf("UpsertOrgSecret was called for MEMBER role — must be skipped")
	}
}

// TestPutOrgSecret_RejectsInvalidBody covers the Zod-schema-equivalent
// boundary checks: empty key, key too long, key with disallowed
// characters, empty value, and a non-JSON body.
func TestPutOrgSecret_RejectsInvalidBody(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)
	spy := &upsertSecretSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: encKey,
	})

	cases := []struct {
		name string
		body string
	}{
		{"empty_key", `{"key":"","value":"v"}`},
		{"key_too_long", `{"key":"` + strings.Repeat("a", 129) + `","value":"v"}`},
		{"key_invalid_chars", `{"key":"has space","value":"v"}`},
		{"empty_value", `{"key":"K","value":""}`},
		{"not_json", `not-json-at-all`},
		{"missing_value", `{"key":"K"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy.upsertParams = nil
			req := httptest.NewRequest(http.MethodPut, "/api/secrets", strings.NewReader(c.body))
			req.Header.Set("X-Api-Key", "cik_"+secret)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400 (body=%q)", rec.Code, rec.Body.String())
			}
			if spy.upsertParams != nil {
				t.Fatalf("UpsertOrgSecret was called for invalid body — must be skipped")
			}
		})
	}
}

// TestPutOrgSecret_OversizedBody_Returns413 locks the DoS defence
// added per  security critic finding: a client sending a body
// larger than maxPutOrgSecretBodyBytes (1 MiB) must surface 413
// PAYLOAD_TOO_LARGE without buffering the whole body into memory.
func TestPutOrgSecret_OversizedBody_Returns413(t *testing.T) {
	const secret = "ownersec"
	const encKey = "0123456789abcdef0123456789abcdef"
	hash := auth.HashSecret(encKey, secret)
	spy := &upsertSecretSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: encKey,
	})

	// 2 MiB of `a`s — well over the 1 MiB cap.
	huge := strings.Repeat("a", 2*1024*1024)
	body := strings.NewReader(`{"key":"K","value":"` + huge + `"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/secrets", body)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413 (body=%q)", rec.Code, rec.Body.String())
	}
	if spy.upsertParams != nil {
		t.Fatalf("UpsertOrgSecret was called for oversized body — must be skipped")
	}
}

// TestPutOrgSecret_NoCredentials_Returns401 confirms PUT shares the
// same 401 path as GET — no information leak across HTTP verbs.
func TestPutOrgSecret_NoCredentials_Returns401(t *testing.T) {
	spy := &upsertSecretSpy{}
	srv := NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
	})
	req := httptest.NewRequest(http.MethodPut, "/api/secrets", strings.NewReader(`{"key":"K","value":"v"}`))
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestIso8601MilliTime_MarshalsAsUTC locks the timezone-normalization
// contract: a non-UTC time.Time input must serialise as the
// UTC-shifted instant with the '.000Z' suffix, NEVER with a non-UTC
// offset like '+05:30'. Without this, a row whose CreatedAt was
// stored in a non-UTC zone would emit `2025-01-15T16:00:00+05:30`
// instead of `2025-01-15T10:30:00.000Z` — diverging from JS
// Date.prototype.toISOString().
func TestIso8601MilliTime_MarshalsAsUTC(t *testing.T) {
	ist := time.FixedZone("IST", 5*60*60+30*60)
	t1 := time.Date(2025, 1, 15, 16, 0, 0, 0, ist)
	got, err := iso8601MilliTime(t1).MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	want := `"2025-01-15T10:30:00.000Z"`
	if string(got) != want {
		t.Fatalf("non-UTC input: got %s, want %s", string(got), want)
	}
}

// TestIso8601MilliTime_ThreeDigitMilliseconds locks the millisecond
// precision: not 6-digit microseconds, not 9-digit nanoseconds — only
// three. Both fractional-zero and fractional-non-zero inputs must
// emit 3-digit ms.
func TestIso8601MilliTime_ThreeDigitMilliseconds(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC), `"2025-01-15T10:30:00.000Z"`},
		{time.Date(2025, 2, 20, 8, 15, 0, 500_000_000, time.UTC), `"2025-02-20T08:15:00.500Z"`},
		{time.Date(2025, 6, 1, 0, 0, 0, 1_000_000, time.UTC), `"2025-06-01T00:00:00.001Z"`},
		{time.Date(2025, 12, 31, 23, 59, 59, 999_000_000, time.UTC), `"2025-12-31T23:59:59.999Z"`},
	}
	for _, c := range cases {
		got, err := iso8601MilliTime(c.in).MarshalJSON()
		if err != nil {
			t.Fatalf("MarshalJSON(%v): %v", c.in, err)
		}
		if string(got) != c.want {
			t.Errorf("input %v: got %s, want %s", c.in, string(got), c.want)
		}
	}
}

// parseFixedTime parses an ISO 8601 timestamp used as a test fixture.
// Panics on malformed input — that's a test-fixture bug, not a
// runtime path. RFC3339Nano handles both ".000Z" and ".500Z"
// fractional-second precisions used by the fixtures.
func parseFixedTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic("parseFixedTime: " + err.Error())
	}
	return t.UTC()
}
