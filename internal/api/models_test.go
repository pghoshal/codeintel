package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// modelsSpy lets each test prescribe the row set returned by the
// db layer + the metadata of the single-tenant org.
type modelsSpy struct {
	fakeAuthQuerier
	orgWithMeta db.Org
	orgMetaErr  error
	models      []db.OrgLanguageModelRow
	modelsErr   error
}

func (m *modelsSpy) GetOrgWithMetadata(ctx context.Context, id int32) (db.Org, error) {
	return m.orgWithMeta, m.orgMetaErr
}
func (m *modelsSpy) ListEnabledOrgLanguageModels(ctx context.Context, orgID int32) ([]db.OrgLanguageModelRow, error) {
	if m.modelsErr != nil {
		return nil, m.modelsErr
	}
	if m.models == nil {
		return make([]db.OrgLanguageModelRow, 0), nil
	}
	return m.models, nil
}

func newModelsTestServer(spy *modelsSpy, singleTenantOrgID int32) *Server {
	return NewServer(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:           spy,
		EncryptionKey:     "0123456789abcdef0123456789abcdef",
		SingleTenantOrgID: singleTenantOrgID,
	})
}

// TestModels_AnonymousAllowed_Empty_Returns200WithEmptyArray locks
// the canonical anonymous path: no credentials + org metadata
// permits anonymous + no models configured → 200 + `[]`.
func TestModels_AnonymousAllowed_Empty_Returns200WithEmptyArray(t *testing.T) {
	spy := &modelsSpy{
		orgWithMeta: db.Org{ID: 1, Domain: "single", Metadata: []byte(`{"anonymousAccessEnabled":true}`)},
	}
	srv := newModelsTestServer(spy, 1)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "[]" {
		t.Fatalf("body: got %q, want []", got)
	}
}

// TestModels_AnonymousAllowed_WithRows_PicksSafeProjection asserts
// the public projection: provider/model/displayName only — NO api
// keys, base URLs, region, or other potentially-sensitive config
// fields leak to the client.
func TestModels_AnonymousAllowed_WithRows_PicksSafeProjection(t *testing.T) {
	spy := &modelsSpy{
		orgWithMeta: db.Org{ID: 1, Domain: "single", Metadata: []byte(`{"anonymousAccessEnabled":true}`)},
		models: []db.OrgLanguageModelRow{
			{Config: []byte(`{"provider":"z-ai","model":"opus-4","displayName":"Opus Test Model","apiKey":{"secretRef":"LLM_KEY"},"baseUrl":"https://api.z-ai.com"}`)},
			{Config: []byte(`{"provider":"amazon-bedrock","model":"nova-lite","region":"us-east-1","accessKeyId":{"env":"AWS"}}`)},
		},
	}
	srv := newModelsTestServer(spy, 1)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	want := `[{"provider":"z-ai","model":"opus-4","displayName":"Opus Test Model"},{"provider":"amazon-bedrock","model":"nova-lite"}]`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
	// Defence-in-depth — make sure no sensitive field leaked.
	for _, leak := range []string{"LLM_KEY", "baseUrl", "us-east-1", "AWS", "apiKey", "accessKeyId", "region"} {
		if got := rec.Body.String(); contains(got, leak) {
			t.Errorf("response leaked sensitive field %q\nbody: %s", leak, got)
		}
	}
}

// TestModels_AnonymousDisabled_Returns401 covers the metadata-off
func TestModels_AnonymousDisabled_Returns401(t *testing.T) {
	spy := &modelsSpy{orgWithMeta: db.Org{ID: 1, Metadata: []byte(`{"anonymousAccessEnabled":false}`)}}
	srv := newModelsTestServer(spy, 1)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestModels_ApiKeyCredentialsBypassAnonymousGate locks the rule
// that a valid API key admits the request even when anonymous is
// disabled — the optional path delegates to required-auth when
// credentials are present.
func TestModels_ApiKeyCredentialsBypassAnonymousGate(t *testing.T) {
	const encKey = "0123456789abcdef0123456789abcdef"
	const secret = "ownersec"
	hash := auth.HashSecret(encKey, secret)

	spy := &modelsSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		orgWithMeta:     db.Org{ID: 1, Metadata: []byte(`{"anonymousAccessEnabled":false}`)},
	}
	srv := newModelsTestServer(spy, 1)

	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (api key bypasses anonymous gate)", rec.Code)
	}
}

// TestModels_DBError_Returns500WithoutLeak confirms a DB outage
// surfaces 500 without leaking the raw error.
func TestModels_DBError_Returns500WithoutLeak(t *testing.T) {
	spy := &modelsSpy{
		orgWithMeta: db.Org{ID: 1, Metadata: []byte(`{"anonymousAccessEnabled":true}`)},
		modelsErr:   errors.New("simulated db outage"),
	}
	srv := newModelsTestServer(spy, 1)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if got := rec.Body.String(); contains(got, "simulated db outage") {
		t.Errorf("raw DB error leaked: %s", got)
	}
}

// TestModels_OrgMetadataMissing_Returns401 covers the misconfig
// case where SingleTenantOrgID points at a row that doesn't exist.
func TestModels_OrgMetadataMissing_Returns401(t *testing.T) {
	spy := &modelsSpy{orgMetaErr: db.ErrOrgNotFound}
	srv := newModelsTestServer(spy, 1)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestModels_MalformedModelConfig_Returns500 confirms a corrupt
// config row in the DB surfaces 500 — never silently drop or
// continue with an empty projection.
func TestModels_MalformedModelConfig_Returns500(t *testing.T) {
	spy := &modelsSpy{
		orgWithMeta: db.Org{ID: 1, Metadata: []byte(`{"anonymousAccessEnabled":true}`)},
		models:      []db.OrgLanguageModelRow{{Config: []byte(`{not json`)}},
	}
	srv := newModelsTestServer(spy, 1)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}
