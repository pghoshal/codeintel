// Wire-format parity tests for GET /api/secrets.
//
// For an org with no OrgSecret rows the response is the literal
// 2-byte body `[]` — no trailing newline. The golden fixture
// (golden/secrets_empty.json) is the byte-equal target.
package parity

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// staticAuthQuerier is a minimal stand-in that returns a pinned
// OWNER-role AuthLookup and an empty OrgSecret slice. Parity
// testing is about the wire format under known inputs — not
// exercising the DB layer (that's covered by internal/db tests).
type staticAuthQuerier struct {
	lookup     db.AuthLookup
	secretRows []db.OrgSecret
}

func (s *staticAuthQuerier) GetApiKeyAuth(ctx context.Context, hash string) (db.AuthLookup, error) {
	return s.lookup, nil
}
func (s *staticAuthQuerier) UpdateApiKeyLastUsedAt(ctx context.Context, hash string) error {
	return nil
}
func (s *staticAuthQuerier) ListOrgSecrets(ctx context.Context, orgID int32) ([]db.OrgSecret, error) {
	if s.secretRows == nil {
		return make([]db.OrgSecret, 0), nil
	}
	return s.secretRows, nil
}
func (s *staticAuthQuerier) UpsertOrgSecret(ctx context.Context, p db.UpsertOrgSecretParams) (db.OrgSecret, error) {
	return db.OrgSecret{}, nil
}
func (s *staticAuthQuerier) ListOrgConnectionsForRefcheck(ctx context.Context, orgID int32) ([]db.ConfigOwner, error) {
	return nil, nil
}
func (s *staticAuthQuerier) ListOrgLanguageModelsForRefcheck(ctx context.Context, orgID int32) ([]db.ConfigOwner, error) {
	return nil, nil
}
func (s *staticAuthQuerier) DeleteOrgSecret(ctx context.Context, orgID int32, key string) error {
	return nil
}
func (s *staticAuthQuerier) GetOrgWithMetadata(ctx context.Context, id int32) (db.Org, error) {
	return db.Org{}, db.ErrOrgNotFound
}
func (s *staticAuthQuerier) ListEnabledOrgLanguageModels(ctx context.Context, orgID int32) ([]db.OrgLanguageModelRow, error) {
	return nil, nil
}
func (s *staticAuthQuerier) SelectMissingOrgSecretKeys(ctx context.Context, orgID int32, keys []string) ([]string, error) {
	return nil, nil
}
func (s *staticAuthQuerier) ReplaceOrgLanguageModels(ctx context.Context, orgID int32, models []db.OrgLanguageModelInsert) error {
	return nil
}
func (s *staticAuthQuerier) ListOrgConnectionsForRead(ctx context.Context, orgID int32) ([]db.ConnectionListRow, error) {
	return nil, nil
}
func (s *staticAuthQuerier) DeleteOrgConnection(ctx context.Context, orgID int32, connectionID int32) error {
	return nil
}
func (s *staticAuthQuerier) UpsertOrgConnection(ctx context.Context, p db.UpsertOrgConnectionParams) (db.ConnectionListRow, error) {
	return db.ConnectionListRow{}, nil
}
func (s *staticAuthQuerier) GetOrgConnectionForUpdate(ctx context.Context, orgID, connectionID int32) (db.ConnectionListRow, error) {
	return db.ConnectionListRow{}, db.ErrConnectionNotFound
}
func (s *staticAuthQuerier) CheckOrgConnectionNameAvailable(ctx context.Context, orgID int32, name string, excludeID int32) error {
	return nil
}
func (s *staticAuthQuerier) ConnectionExistsInOrg(ctx context.Context, orgID, connectionID int32) (bool, error) {
	return false, nil
}
func (s *staticAuthQuerier) GetOrgConnectionMeta(ctx context.Context, orgID, connectionID int32) (db.ConnectionMetaRow, error) {
	return db.ConnectionMetaRow{}, db.ErrConnectionNotFound
}
func (s *staticAuthQuerier) ListConnectionSyncJobs(ctx context.Context, orgID, connectionID, limit int32) ([]db.ConnectionSyncJobRow, error) {
	return nil, nil
}
func (s *staticAuthQuerier) CountConnectionRepos(ctx context.Context, orgID, connectionID int32) (int32, error) {
	return 0, nil
}
func (s *staticAuthQuerier) GetOrgStatusRollup(ctx context.Context, orgID int32) (db.OrgStatusRollup, error) {
	return db.OrgStatusRollup{}, nil
}
func (s *staticAuthQuerier) ListRecentFailedConnectionSyncJobs(ctx context.Context, orgID, limit int32) ([]db.RecentFailedConnectionSyncJobRow, error) {
	return nil, nil
}
func (s *staticAuthQuerier) ListRecentFailedRepoIndexingJobs(ctx context.Context, orgID, limit int32) ([]db.RecentFailedRepoIndexingJobRow, error) {
	return nil, nil
}
func (s *staticAuthQuerier) ListOrgRepos(ctx context.Context, p db.ListOrgReposParams) ([]db.RepoListRow, error) {
	return make([]db.RepoListRow, 0), nil
}
func (s *staticAuthQuerier) CountOrgRepos(ctx context.Context, p db.CountOrgReposParams) (int32, error) {
	return 0, nil
}

// TestSecrets_EmptyOrg_BodyByteEqualToGolden locks the wire-format
// parity for the most common path: an OWNER-authenticated GET
// against an org with zero configured secrets must return exactly
// `[]` byte-for-byte.
func TestSecrets_EmptyOrg_BodyByteEqualToGolden(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("golden", "secrets_empty.json"))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}

	const encKey = "test-encryption-key-32-bytes-long"
	secret := "ownersec"
	expectedHash := auth.HashSecret(encKey, secret)

	fq := &staticAuthQuerier{
		lookup: db.AuthLookup{
			UserID: "u-1",
			Org: db.Org{
				ID:     7,
				Name:   "Org A",
				Domain: "orga",
			},
			Role:       "OWNER",
			ApiKeyHash: expectedHash,
		},
	}

	server := api.NewServer(api.Config{
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Queries:       fq,
		EncryptionKey: encKey,
	})
	ts := httptest.NewServer(server.Router())
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/secrets", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Api-Key", "cik_"+secret)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/secrets: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want %d", res.StatusCode, http.StatusOK)
	}
	if ct := res.Header.Get("Content-Type"); ct == "" || ct[:16] != "application/json" {
		t.Fatalf("Content-Type: got %q, want application/json", ct)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != string(golden) {
		t.Fatalf("body parity failure:\n  got    %q (%d bytes)\n  golden %q (%d bytes)",
			string(body), len(body), string(golden), len(golden))
	}
}

// TestSecrets_EmptyOrg_ResponseSize asserts the 2-byte payload
// length so trailing-newline regressions fail immediately.
func TestSecrets_EmptyOrg_ResponseSize(t *testing.T) {
	const encKey = "k"
	secret := "ownersec"
	hash := auth.HashSecret(encKey, secret)

	fq := &staticAuthQuerier{
		lookup: db.AuthLookup{
			UserID:     "u-1",
			Org:        db.Org{ID: 1, Name: "n", Domain: "d"},
			Role:       "OWNER",
			ApiKeyHash: hash,
		},
	}
	server := api.NewServer(api.Config{
		Logger:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Queries:       fq,
		EncryptionKey: encKey,
	})
	ts := httptest.NewServer(server.Router())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/secrets", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/secrets: %v", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if len(body) != 2 {
		t.Fatalf("body size: got %d, want 2 (`[]`)", len(body))
	}
}
