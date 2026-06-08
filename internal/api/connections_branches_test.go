package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// branchesSpy plugs in a custom GetOrgConnectionMeta result for
// the branches handler.
type branchesSpy struct {
	fakeAuthQuerier
	row db.ConnectionMetaRow
	err error
}

func (b *branchesSpy) GetOrgConnectionMeta(ctx context.Context, orgID, connectionID int32) (db.ConnectionMetaRow, error) {
	if b.err != nil {
		return db.ConnectionMetaRow{}, b.err
	}
	return b.row, nil
}

func newBranchesServer(spy *branchesSpy) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
	})
}

func branchesRequest(t *testing.T, id int) *http.Request {
	t.Helper()
	secret, _ := ownerKey(t)
	req := httptest.NewRequest(http.MethodGet, "/api/connections/"+itoa(id)+"/branches", nil)
	req.Header.Set("X-Api-Key", "cik_"+secret)
	return req
}

// TestNormaliseBranches_DedupesTrimsFilters locks the helper that
// the policy computation depends on. Pure function — no HTTP.
func TestNormaliseBranches_DedupesTrimsFilters(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil", nil, []string{}},
		{"empty", []string{}, []string{}},
		{"dedup", []string{"main", "main", "dev"}, []string{"main", "dev"}},
		{"trim_and_drop_blanks", []string{"  main  ", "", "   "}, []string{"main"}},
		{"preserve_first_occurrence_order", []string{"b", "a", "b", "c"}, []string{"b", "a", "c"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normaliseBranches(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("normaliseBranches(%v): got %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestComputeBranchPolicy_PickoutsMode covers the three policy
// modes the response surfaces.
func TestComputeBranchPolicy_PickoutsMode(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		mode branchSyncMode
		want []string
	}{
		{"no_revisions_key", `{"type":"github"}`, branchSyncModeDefault, []string{}},
		{"empty_branches", `{"revisions":{"branches":[]}}`, branchSyncModeDefault, []string{}},
		{"wildcard_all", `{"revisions":{"branches":["*"]}}`, branchSyncModeAll, []string{"*"}},
		{"explicit_patterns", `{"revisions":{"branches":["main","dev/*"]}}`, branchSyncModePatterns, []string{"main", "dev/*"}},
		{"wildcard_wins_over_others", `{"revisions":{"branches":["main","*"]}}`, branchSyncModeAll, []string{"main", "*"}},
		{"non_string_branch_filtered", `{"revisions":{"branches":["main",42,null]}}`, branchSyncModePatterns, []string{"main"}},
		{"revisions_not_object", `{"revisions":"oops"}`, branchSyncModeDefault, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cfg any
			if err := json.Unmarshal([]byte(c.cfg), &cfg); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			p := computeBranchPolicy(cfg)
			if p.Mode != c.mode {
				t.Errorf("Mode: got %q, want %q", p.Mode, c.mode)
			}
			if !reflect.DeepEqual(p.Branches, c.want) {
				t.Errorf("Branches: got %v, want %v", p.Branches, c.want)
			}
			if p.MaxIndexedRevisions != 64 {
				t.Errorf("MaxIndexedRevisions: got %d, want 64", p.MaxIndexedRevisions)
			}
			wantDefaultAlways := c.mode == branchSyncModeDefault
			if p.DefaultBranchAlwaysIncluded != wantDefaultAlways {
				t.Errorf("DefaultBranchAlwaysIncluded: got %v, want %v", p.DefaultBranchAlwaysIncluded, wantDefaultAlways)
			}
		})
	}
}

// TestBranches_OwnerHappy_Returns200WithFullShape locks the wire
// shape end to end: id/name/type/syncedAt/updatedAt/branchPolicy.
func TestBranches_OwnerHappy_Returns200WithFullShape(t *testing.T) {
	_, hash := ownerKey(t)
	synced := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	updated := time.Date(2025, 2, 20, 8, 15, 0, 500_000_000, time.UTC)
	var cfg any
	if err := json.Unmarshal([]byte(`{"type":"github","revisions":{"branches":["main","dev/*"]}}`), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	spy := &branchesSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		row: db.ConnectionMetaRow{
			ID:             42,
			Name:           "gh-prod",
			ConnectionType: "github",
			Config:         cfg,
			SyncedAt:       &synced,
			UpdatedAt:      updated,
		},
	}
	srv := newBranchesServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesRequest(t, 42))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	want := `{"connectionId":42,"connectionName":"gh-prod","connectionType":"github","syncedAt":"2025-01-15T10:30:00.000Z","updatedAt":"2025-02-20T08:15:00.500Z","branchPolicy":{"mode":"patterns","branches":["main","dev/*"],"defaultBranchAlwaysIncluded":false,"maxIndexedRevisions":64}}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body byte-equality:\n  got  %s\n  want %s", got, want)
	}
}

// TestBranches_NullSyncedAt confirms the response emits
// `"syncedAt":null` rather than omitting the field for an
// un-synced connection.
func TestBranches_NullSyncedAt(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		row: db.ConnectionMetaRow{
			ID:             1,
			Name:           "n",
			ConnectionType: "github",
			Config:         map[string]any{"type": "github"},
			SyncedAt:       nil,
			UpdatedAt:      time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	srv := newBranchesServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesRequest(t, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"syncedAt":null`) {
		t.Errorf("missing null syncedAt; got %s", rec.Body.String())
	}
}

// TestBranches_NotFound_Returns404.
func TestBranches_NotFound_Returns404(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		err:             db.ErrConnectionNotFound,
	}
	srv := newBranchesServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesRequest(t, 999))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	want := `{"statusCode":404,"errorCode":"NOT_FOUND","message":"Connection not found."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestBranches_NonIntegerID_Returns400 covers the path-segment
// validation across multiple malformed inputs including
// int32-overflow values so the parsed id can never be cast
// safely into the int32 the DB column expects.
func TestBranches_NonIntegerID_Returns400(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)}}
	srv := newBranchesServer(spy)
	secret, _ := ownerKey(t)
	cases := []string{
		"abc",
		"1.5",
		"-not-int",
		"2147483648",          // int32 overflow (max+1)
		"-2147483649",         // int32 underflow (min-1)
		"9999999999999999999", // way past int64
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/connections/"+c+"/branches", nil)
			req.Header.Set("X-Api-Key", "cik_"+secret)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("input %q: status %d, want 400", c, rec.Code)
			}
		})
	}
}

// TestBranches_WhitespaceOnlyBranchesDegradesToDefault locks the
// boundary where a config carries branches but every entry is
// whitespace: normaliseBranches drops them all, the resulting
// empty slice triggers mode="default" rather than "patterns".
func TestBranches_WhitespaceOnlyBranchesDegradesToDefault(t *testing.T) {
	_, hash := ownerKey(t)
	cfgJSON := `{"type":"github","revisions":{"branches":["   ","\t","\n"]}}`
	var cfg any
	if err := json.Unmarshal([]byte(cfgJSON), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	spy := &branchesSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		row: db.ConnectionMetaRow{
			ID:             1,
			Name:           "n",
			ConnectionType: "github",
			Config:         cfg,
			UpdatedAt:      time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	srv := newBranchesServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesRequest(t, 1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	for _, want := range []string{`"mode":"default"`, `"branches":[]`, `"defaultBranchAlwaysIncluded":true`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("response missing %q\nbody: %s", want, rec.Body.String())
		}
	}
}

// TestBranches_MemberRole_Returns403 confirms the OWNER guard with
// the exact wire message.
func TestBranches_MemberRole_Returns403(t *testing.T) {
	_, hash := ownerKey(t)
	lookup := validOwnerLookup(hash)
	lookup.Role = "MEMBER"
	spy := &branchesSpy{fakeAuthQuerier: fakeAuthQuerier{lookupResult: lookup}}
	srv := newBranchesServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesRequest(t, 42))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", rec.Code)
	}
	want := `{"statusCode":403,"errorCode":"INSUFFICIENT_PERMISSIONS","message":"Only organization owners can view branch sync policy."}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %s, want %s", got, want)
	}
}

// TestBranches_NoCredentials_Returns401.
func TestBranches_NoCredentials_Returns401(t *testing.T) {
	spy := &branchesSpy{}
	srv := newBranchesServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/connections/42/branches", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestBranches_DBError_Returns500WithoutLeak.
func TestBranches_DBError_Returns500WithoutLeak(t *testing.T) {
	_, hash := ownerKey(t)
	spy := &branchesSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		err:             errors.New("simulated db outage"),
	}
	srv := newBranchesServer(spy)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, branchesRequest(t, 42))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "simulated db outage") {
		t.Errorf("raw error leaked: %s", rec.Body.String())
	}
}

// Reuse auth import to keep dependency graph stable for future
// additions.
var _ auth.AuthContext
