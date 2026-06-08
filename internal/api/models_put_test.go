package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// modelsPutSpy extends modelsSpy with the write-side hooks: a
// recording of every ReplaceOrgLanguageModels call (so tests can
// assert the bound config + order) and a controllable
// SelectMissingOrgSecretKeys.
type modelsPutSpy struct {
	modelsSpy
	missingKeys []string
	missingErr  error

	replaceErr      error
	replaceCalls    []replaceCall
	postReplaceRows []db.OrgLanguageModelRow
}

type replaceCall struct {
	OrgID  int32
	Models []db.OrgLanguageModelInsert
}

func (m *modelsPutSpy) SelectMissingOrgSecretKeys(ctx context.Context, orgID int32, keys []string) ([]string, error) {
	if m.missingErr != nil {
		return nil, m.missingErr
	}
	if m.missingKeys == nil {
		return make([]string, 0), nil
	}
	return m.missingKeys, nil
}

func (m *modelsPutSpy) ReplaceOrgLanguageModels(ctx context.Context, orgID int32, models []db.OrgLanguageModelInsert) error {
	if m.replaceErr != nil {
		return m.replaceErr
	}
	m.replaceCalls = append(m.replaceCalls, replaceCall{OrgID: orgID, Models: models})
	return nil
}

// ListEnabledOrgLanguageModels returns the canned post-replace
// projection if set, otherwise empty.
func (m *modelsPutSpy) ListEnabledOrgLanguageModels(ctx context.Context, orgID int32) ([]db.OrgLanguageModelRow, error) {
	if m.postReplaceRows != nil {
		return m.postReplaceRows, nil
	}
	return make([]db.OrgLanguageModelRow, 0), nil
}

func newPutTestServer(spy *modelsPutSpy) *Server {
	return NewServer(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:           spy,
		EncryptionKey:     "0123456789abcdef0123456789abcdef",
		SingleTenantOrgID: 1,
	})
}

func ownerHash(t *testing.T) (string, string) {
	t.Helper()
	const encKey = "0123456789abcdef0123456789abcdef"
	const secret = "ownersec"
	return secret, auth.HashSecret(encKey, secret)
}

func putRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	secret, _ := ownerHash(t)
	req := httptest.NewRequest(http.MethodPut, "/api/models", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "cik_"+secret)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestPutModels_OwnerHappyPath_ReplaceWorks locks the canonical
// happy path: bind orgId to every secretref, missing-secret check
// returns empty, ReplaceOrgLanguageModels called with the bound
// configs, response is the post-write GET-shape.
func TestPutModels_OwnerHappyPath_ReplaceWorks(t *testing.T) {
	_, hash := ownerHash(t)
	spy := &modelsPutSpy{
		modelsSpy: modelsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}},
		postReplaceRows: []db.OrgLanguageModelRow{
			{Config: []byte(`{"provider":"z-ai","model":"opus-4","displayName":"Opus Test Model"}`)},
		},
	}
	srv := newPutTestServer(spy)

	rec := httptest.NewRecorder()
	body := `{"models":[
		{"provider":"z-ai","model":"opus-4","displayName":"Opus Test Model","apiKey":{"secretRef":"LLM_SECRET"}}
	]}`
	srv.Router().ServeHTTP(rec, putRequest(t, body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	want := `[{"provider":"z-ai","model":"opus-4","displayName":"Opus Test Model"}]`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
	if len(spy.replaceCalls) != 1 {
		t.Fatalf("expected 1 ReplaceOrgLanguageModels call, got %d", len(spy.replaceCalls))
	}
	call := spy.replaceCalls[0]
	if call.OrgID != 7 {
		t.Errorf("OrgID: got %d, want 7", call.OrgID)
	}
	if len(call.Models) != 1 {
		t.Fatalf("expected 1 model in replace, got %d", len(call.Models))
	}
	m := call.Models[0]
	if m.Name != "z-ai:opus-4" || m.Order != 0 {
		t.Errorf("model: Name=%q Order=%d", m.Name, m.Order)
	}
	// The bound config must have orgId stamped on the secretRef
	// reference — proves BindToOrg ran.
	cfg, _ := m.Config.(map[string]any)
	if cfg == nil {
		t.Fatalf("Config not a map: %T", m.Config)
	}
	apiKey, _ := cfg["apiKey"].(map[string]any)
	if apiKey == nil {
		t.Fatalf("apiKey missing or not a map: %v", cfg)
	}
	if got, _ := apiKey["secretRef"].(string); got != "LLM_SECRET" {
		t.Errorf("apiKey.secretRef: got %v, want LLM_SECRET", apiKey["secretRef"])
	}
	if got, _ := apiKey["orgId"].(float64); int(got) != 7 {
		t.Errorf("apiKey.orgId: got %v, want 7", apiKey["orgId"])
	}
}

// TestPutModels_MissingSecretRefs_Returns400 locks the
// referential-integrity check: a config referencing an undefined
// secret blocks the write with a diagnostic body.
func TestPutModels_MissingSecretRefs_Returns400(t *testing.T) {
	_, hash := ownerHash(t)
	spy := &modelsPutSpy{
		modelsSpy:   modelsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}},
		missingKeys: []string{"GHOST_A", "GHOST_B"},
	}
	srv := newPutTestServer(spy)
	rec := httptest.NewRecorder()
	body := `{"models":[
		{"provider":"x","model":"y","k":{"secretRef":"GHOST_A"}},
		{"provider":"x","model":"z","k":{"secretRef":"GHOST_B"}}
	]}`
	srv.Router().ServeHTTP(rec, putRequest(t, body))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d (body=%q), want 400", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "GHOST_A, GHOST_B") {
		t.Errorf("body missing sorted missing-keys diagnostic: %s", rec.Body.String())
	}
	if len(spy.replaceCalls) != 0 {
		t.Fatalf("ReplaceOrgLanguageModels must NOT run when refs are missing")
	}
}

// TestPutModels_DuplicateNames_Returns400 locks the
// <provider>:<model> uniqueness check.
func TestPutModels_DuplicateNames_Returns400(t *testing.T) {
	_, hash := ownerHash(t)
	spy := &modelsPutSpy{
		modelsSpy: modelsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}},
	}
	srv := newPutTestServer(spy)
	rec := httptest.NewRecorder()
	body := `{"models":[
		{"provider":"x","model":"y"},
		{"provider":"x","model":"y"}
	]}`
	srv.Router().ServeHTTP(rec, putRequest(t, body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "x:y") {
		t.Errorf("body missing duplicate-name diagnostic: %s", rec.Body.String())
	}
	if len(spy.replaceCalls) != 0 {
		t.Fatalf("ReplaceOrgLanguageModels must NOT run on duplicate names")
	}
}

// TestPutModels_MalformedBody covers the four schema-failure modes.
func TestPutModels_MalformedBody(t *testing.T) {
	_, hash := ownerHash(t)
	spy := &modelsPutSpy{
		modelsSpy: modelsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}},
	}
	srv := newPutTestServer(spy)
	cases := []struct {
		name string
		body string
	}{
		{"not_json", `not-json`},
		{"missing_models_field", `{}`},
		{"models_not_array", `{"models":"oops"}`},
		{"model_entry_not_object", `{"models":[42]}`},
		{"model_missing_provider", `{"models":[{"model":"y"}]}`},
		{"model_missing_model", `{"models":[{"provider":"x"}]}`},
		{"empty_provider", `{"models":[{"provider":"","model":"y"}]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy.replaceCalls = nil
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, putRequest(t, c.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("body=%q: status %d, want 400 (body=%q)", c.body, rec.Code, rec.Body.String())
			}
			if len(spy.replaceCalls) != 0 {
				t.Fatalf("ReplaceOrgLanguageModels must NOT run for invalid body")
			}
		})
	}
}

// TestPutModels_OversizedBody_Returns413 confirms the 4 MiB cap.
func TestPutModels_OversizedBody_Returns413(t *testing.T) {
	_, hash := ownerHash(t)
	spy := &modelsPutSpy{
		modelsSpy: modelsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}},
	}
	srv := newPutTestServer(spy)

	huge := strings.Repeat("a", 5*1024*1024)
	body := `{"models":[{"provider":"x","model":"y","note":"` + huge + `"}]}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, putRequest(t, body))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", rec.Code)
	}
}

// TestPutModels_MemberRole_Returns403 covers the OWNER guard:
// the canonical "Only organization owners can configure language
// models." message must be byte-equal.
func TestPutModels_MemberRole_Returns403(t *testing.T) {
	_, hash := ownerHash(t)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &modelsPutSpy{
		modelsSpy: modelsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup}},
	}
	srv := newPutTestServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, putRequest(t, `{"models":[]}`))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can configure language models."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body:\n  got  %s\n  want %s", got, want)
	}
}

// TestPutModels_NoCredentials_Returns401 confirms the 401 path.
func TestPutModels_NoCredentials_Returns401(t *testing.T) {
	spy := &modelsPutSpy{}
	srv := newPutTestServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/models", strings.NewReader(`{"models":[]}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestPutModels_ReplaceError_Returns500 surfaces a tx failure as
// 500 without raw-error leak.
func TestPutModels_ReplaceError_Returns500(t *testing.T) {
	_, hash := ownerHash(t)
	spy := &modelsPutSpy{
		modelsSpy:  modelsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}},
		replaceErr: errors.New("simulated tx outage"),
	}
	srv := newPutTestServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, putRequest(t, `{"models":[{"provider":"x","model":"y"}]}`))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated tx outage") {
		t.Errorf("raw error leaked: %s", rec.Body.String())
	}
}

// TestPutModels_EmptyModelsArray_AllowsClear locks that PUT with an
// empty models array clears the org's set (DELETE-all, no inserts).
func TestPutModels_EmptyModelsArray_AllowsClear(t *testing.T) {
	_, hash := ownerHash(t)
	spy := &modelsPutSpy{
		modelsSpy: modelsSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}},
	}
	srv := newPutTestServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, putRequest(t, `{"models":[]}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "[]" {
		t.Fatalf("body: got %q, want []", got)
	}
	if len(spy.replaceCalls) != 1 {
		t.Fatalf("expected 1 replace call (with 0 models), got %d", len(spy.replaceCalls))
	}
	if len(spy.replaceCalls[0].Models) != 0 {
		t.Errorf("expected empty models slice, got %d", len(spy.replaceCalls[0].Models))
	}
}
