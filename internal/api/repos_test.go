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
	"sync"
	"testing"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/pkg/audit"
)

// reposSpy captures the (params) the handler passed to the db layer
// and lets each test plug in fixed rows / errors. Embedding
// fakeAuthQuerier picks up the auth + unrelated-method stubs.
type reposSpy struct {
	fakeAuthQuerier

	mu sync.Mutex

	listParams db.ListOrgReposParams
	listCalls  int
	listRows   []db.RepoListRow
	listErr    error

	countParams db.CountOrgReposParams
	countCalls  int
	countTotal  int32
	countErr    error
}

func (s *reposSpy) ListOrgRepos(ctx context.Context, p db.ListOrgReposParams) ([]db.RepoListRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	s.listParams = p
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listRows == nil {
		return make([]db.RepoListRow, 0), nil
	}
	return s.listRows, nil
}

func (s *reposSpy) CountOrgRepos(ctx context.Context, p db.CountOrgReposParams) (int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.countCalls++
	s.countParams = p
	if s.countErr != nil {
		return 0, s.countErr
	}
	return s.countTotal, nil
}

func newReposTestServer(spy *reposSpy, emitter audit.Emitter) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       spy,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		AuditEmitter:  emitter,
	})
}

// reposAuthedRequest assembles a GET /api/repos request with the
// canonical owner-API-key header set. raw is the literal secret
// (no `cik_` prefix) that the handler hashes against the spy's
// AuthLookup row.
func reposAuthedRequest(t *testing.T, url string, raw string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Api-Key", "cik_"+raw)
	return req
}

const repoListNotIndexedFields = `,"indexStatus":"not_indexed","indexStatusColor":"gray","indexed":false,"indexedRevisions":[],"activeIndexStatus":"not_indexed","activeIndexStatusColor":"gray","activeIndexUsable":false,"latestIndexRun":{"status":"none","statusColor":"gray","blocksActiveIndex":false}`
const repoListFailedNoActiveFields = `,"indexStatus":"failed","indexStatusColor":"red","indexed":false,"indexedRevisions":[],"activeIndexStatus":"not_indexed","activeIndexStatusColor":"gray","activeIndexUsable":false,"latestIndexRun":{"status":"failed","statusColor":"red","blocksActiveIndex":true}`
const repoListIndexingNoActiveFields = `,"indexStatus":"indexing","indexStatusColor":"yellow","indexed":false,"indexedRevisions":[],"activeIndexStatus":"not_indexed","activeIndexStatusColor":"gray","activeIndexUsable":false,"latestIndexRun":{"status":"indexing","statusColor":"yellow","blocksActiveIndex":true}`

// TestRepos_HappyPath_200ByteEqualBody_AndXTotalCount locks the
// wire shape: 2-row data array, omitempty on optional fields, the
// X-Total-Count header matches CountOrgRepos return, and the
// underlying queries received the expected params (org id pulled
// from auth, default sort=name asc, perPage=30, skip=0).
func TestRepos_HappyPath_200ByteEqualBody_AndXTotalCount(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	indexedAt := time.Date(2025, 1, 15, 10, 30, 0, 0, time.UTC)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{RepoID: 101, RepoName: "alpha", RepoDisplayName: strPtr("Alpha Display"), IndexedAt: &indexedAt},
			{RepoID: 102, RepoName: "beta", RepoDisplayName: nil, IndexedAt: nil},
		},
		countTotal: 7,
	}
	emitter := &recordingEmitter{}
	srv := newReposTestServer(spy, emitter)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	wantBody := `[{"codeHostType":"","repoId":101,"repoName":"alpha","webUrl":"","repoDisplayName":"Alpha Display","indexedAt":"2025-01-15T10:30:00.000Z"` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false},{"codeHostType":"","repoId":102,"repoName":"beta","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false}]`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
	if got := rec.Header().Get("X-Total-Count"); got != "7" {
		t.Errorf("X-Total-Count: got %q, want 7", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", got)
	}
	// DB layer received the expected params.
	if spy.listCalls != 1 || spy.countCalls != 1 {
		t.Errorf("expected one List + one Count call, got list=%d count=%d", spy.listCalls, spy.countCalls)
	}
	if spy.listParams.OrgID != 7 {
		t.Errorf("ListOrgRepos OrgID: got %d, want 7 (from auth)", spy.listParams.OrgID)
	}
	if spy.listParams.Take != defaultRepoListPerPage || spy.listParams.Skip != 0 {
		t.Errorf("default pagination: got take=%d skip=%d, want take=%d skip=0", spy.listParams.Take, spy.listParams.Skip, defaultRepoListPerPage)
	}
	if spy.listParams.Sort != db.ReposSortName || spy.listParams.Direction != db.ReposSortAsc {
		t.Errorf("default sort: got (%q,%q), want (name,asc)", spy.listParams.Sort, spy.listParams.Direction)
	}
	if spy.countParams.OrgID != 7 || spy.countParams.Query != "" {
		t.Errorf("CountOrgRepos params: got %+v", spy.countParams)
	}
}

// TestRepos_IndexSummaryFields_ExposeAtomControlState locks the
// Atom-native list contract: the repo list carries the same split
// index state as GET /api/repos/{id}/status, so a failed reindex
// can still show the older active index as usable and green.
func TestRepos_IndexSummaryFields_ExposeAtomControlState(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	indexedAt := time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC)
	completedAt := time.Date(2025, 7, 2, 12, 0, 0, 0, time.UTC)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{
				RepoID:        301,
				RepoName:      "indexed-active",
				IndexedAt:     &indexedAt,
				Metadata:      json.RawMessage(`{"branches":["main","release-*"],"indexedRevisions":["refs/heads/main","refs/heads/release-a","refs/heads/private"]}`),
				DefaultBranch: strPtr("main"),
				LatestJob: &db.RepoListJobRow{
					ID: "job-failed", Type: "INDEX", Status: "FAILED",
					CreatedAt:   completedAt,
					CompletedAt: &completedAt,
				},
			},
		},
		countTotal: 1,
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	var decoded []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("rows: got %d, want 1", len(decoded))
	}
	row := decoded[0]
	if got := row["indexStatus"]; got != "indexed" {
		t.Errorf("indexStatus: got %v, want indexed", got)
	}
	if got := row["indexStatusColor"]; got != "green" {
		t.Errorf("indexStatusColor: got %v, want green", got)
	}
	if got := row["indexed"]; got != true {
		t.Errorf("indexed: got %v, want true", got)
	}
	if got := row["activeIndexUsable"]; got != true {
		t.Errorf("activeIndexUsable: got %v, want true", got)
	}
	revisions, ok := row["indexedRevisions"].([]any)
	if !ok || len(revisions) != 2 || revisions[0] != "refs/heads/main" || revisions[1] != "refs/heads/release-a" {
		t.Errorf("indexedRevisions: got %v, want policy-visible main/release-a", row["indexedRevisions"])
	}
	latestRun, ok := row["latestIndexRun"].(map[string]any)
	if !ok {
		t.Fatalf("latestIndexRun: got %T", row["latestIndexRun"])
	}
	if got := latestRun["status"]; got != "failed" {
		t.Errorf("latestIndexRun.status: got %v, want failed", got)
	}
	if got := latestRun["blocksActiveIndex"]; got != false {
		t.Errorf("latestIndexRun.blocksActiveIndex: got %v, want false", got)
	}
}

// TestRepos_AuditEmit_UserListedRepos locks the audit-emit
// contract: a single user.listed_repos event with the actor /
// target / org / metadata.source fields. The metadata.source
// comes from the X-Codeintel-Client-Source header.
func TestRepos_AuditEmit_UserListedRepos(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		countTotal:      0,
	}
	emitter := &recordingEmitter{}
	srv := newReposTestServer(spy, emitter)

	req := reposAuthedRequest(t, "/api/repos", secret)
	req.Header.Set("X-Codeintel-Client-Source", "web-ui")
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(emitter.events))
	}
	ev := emitter.events[0]
	if ev.Action != "user.listed_repos" {
		t.Errorf("Action: got %q, want user.listed_repos", ev.Action)
	}
	if ev.ActorType != audit.ActorApiKey {
		t.Errorf("ActorType: got %q, want api_key", ev.ActorType)
	}
	if ev.ActorID != "u-1" {
		t.Errorf("ActorID: got %q, want u-1", ev.ActorID)
	}
	if ev.OrgID != 7 {
		t.Errorf("OrgID: got %d, want 7", ev.OrgID)
	}
	if ev.TargetType != audit.TargetOrg {
		t.Errorf("TargetType: got %q, want org", ev.TargetType)
	}
	if ev.TargetID != "7" {
		t.Errorf("TargetID: got %q, want 7", ev.TargetID)
	}
	if got, _ := ev.Metadata["source"].(string); got != "web-ui" {
		t.Errorf("Metadata.source: got %v, want web-ui", ev.Metadata["source"])
	}
	if ev.Time.IsZero() {
		t.Errorf("Time not stamped")
	}
}

// TestRepos_AuditEmit_NoSourceHeader_OmitsMetadata confirms that
// when the client doesn't send X-Codeintel-Client-Source the audit
// event is still emitted but Metadata is empty (no `source: ""`
// noise that downstream consumers would otherwise have to filter).
func TestRepos_AuditEmit_NoSourceHeader_OmitsMetadata(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	emitter := &recordingEmitter{}
	srv := newReposTestServer(spy, emitter)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(emitter.events))
	}
	if emitter.events[0].Metadata != nil {
		t.Errorf("Metadata: got %+v, want nil when no source header", emitter.events[0].Metadata)
	}
}

// TestRepos_QueryFilterPassesThrough confirms `?query=foo` lands
// on both ListOrgRepos and CountOrgRepos so the listing window
// and the X-Total-Count header report on the same filtered set.
func TestRepos_QueryFilterPassesThrough(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos?query=foo&page=2&perPage=10", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if spy.listParams.Query != "foo" {
		t.Errorf("ListOrgRepos Query: got %q, want foo", spy.listParams.Query)
	}
	if spy.countParams.Query != "foo" {
		t.Errorf("CountOrgRepos Query: got %q, want foo", spy.countParams.Query)
	}
	if spy.listParams.Skip != 10 || spy.listParams.Take != 10 {
		t.Errorf("pagination: got skip=%d take=%d, want skip=10 take=10", spy.listParams.Skip, spy.listParams.Take)
	}
}

// TestRepos_SortPushed_MapsToIndexedAt locks the wire-compat
// remap: the `sort=pushed` token still validates and is routed to
// the indexedAt-sorted query under the current schema.
func TestRepos_SortPushed_MapsToIndexedAt(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos?sort=pushed&direction=desc", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if spy.listParams.Sort != db.ReposSortPushedAt {
		t.Errorf("sort: got %q, want pushedAt", spy.listParams.Sort)
	}
	if spy.listParams.Direction != db.ReposSortDesc {
		t.Errorf("direction: got %q, want desc", spy.listParams.Direction)
	}
}

// TestRepos_BadQueryParams_400 locks 400 responses for each
// invalid input. Body envelopes are byte-equal so the wire contract
// can't drift.
func TestRepos_BadQueryParams_400(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)

	cases := []struct {
		name string
		url  string
		body string
	}{
		{"page=0", "/api/repos?page=0", `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"page must be a positive integer."}`},
		{"page negative", "/api/repos?page=-1", `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"page must be a positive integer."}`},
		{"page non-numeric", "/api/repos?page=abc", `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"page must be a positive integer."}`},
		{"perPage=0", "/api/repos?perPage=0", `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"perPage must be a positive integer no greater than 100."}`},
		{"perPage>100", "/api/repos?perPage=101", `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"perPage must be a positive integer no greater than 100."}`},
		{"bad sort", "/api/repos?sort=random", `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"sort must be one of: name, pushed."}`},
		{"bad direction", "/api/repos?direction=sideways", `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"direction must be one of: asc, desc."}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &reposSpy{
				fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
			}
			srv := newReposTestServer(spy, nil)
			rec := httptest.NewRecorder()
			srv.Router().ServeHTTP(rec, reposAuthedRequest(t, tc.url, secret))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400", rec.Code)
			}
			if got := rec.Body.String(); got != tc.body {
				t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, tc.body)
			}
			// Bad params must short-circuit BEFORE the DB layer.
			if spy.listCalls != 0 || spy.countCalls != 0 {
				t.Errorf("DB called on bad params: list=%d count=%d", spy.listCalls, spy.countCalls)
			}
		})
	}
}

// TestRepos_NoCredentials_401 confirms the optional-auth resolver's
// "no credentials and no anonymous-access" path collapses to 401
// (not 500, not panic). The body is byte-equal to the canonical
// not-authenticated envelope so wire drift fails the test.
func TestRepos_NoCredentials_401(t *testing.T) {
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupErr: auth.ErrUnknownKey},
	}
	srv := newReposTestServer(spy, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d (body=%q), want 401", rec.Code, rec.Body.String())
	}
	wantBody := `{"statusCode":401,"errorCode":"NOT_AUTHENTICATED","message":"Not authenticated"}`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
}

// TestRepos_DBListError_500_NoAuditEmit confirms a list-query DB
// failure produces a 500 envelope AND does NOT emit an audit event
// (audit fires only on successful business operations).
func TestRepos_DBListError_500_NoAuditEmit(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listErr:         errors.New("simulated outage"),
	}
	emitter := &recordingEmitter{}
	srv := newReposTestServer(spy, emitter)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
	if len(emitter.events) != 0 {
		t.Errorf("expected no audit emit on db error, got %d events", len(emitter.events))
	}
}

// TestRepos_DBCountError_500 confirms a count-query DB failure
// (sibling errgroup task) cancels the listing and surfaces the
// canonical 500 envelope.
func TestRepos_DBCountError_500(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		countErr:        errors.New("simulated count outage"),
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

// TestRepos_EmptyResult_EncodesAsEmptyArray confirms a zero-row
// listing emits `[]` not `null`, and the X-Total-Count header is
// the literal `0` (not empty / missing).
func TestRepos_EmptyResult_EncodesAsEmptyArray(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		countTotal:      0,
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "[]" {
		t.Errorf("body: got %q, want []", got)
	}
	if got := rec.Header().Get("X-Total-Count"); got != "0" {
		t.Errorf("X-Total-Count: got %q, want 0", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", got)
	}
}

// anonymousFakeQuerier resolves the single-tenant org with
// anonymous-access enabled. Reused by the anonymous-emit-skip test.
type anonymousFakeQuerier struct{ fakeAuthQuerier }

func (a *anonymousFakeQuerier) GetOrgWithMetadata(ctx context.Context, id int32) (db.Org, error) {
	return db.Org{ID: id, Domain: "anon", Metadata: []byte(`{"anonymousAccessEnabled":true}`)}, nil
}

// TestRepos_AnonymousAuth_SkipsAuditEmit confirms the
// `AuthSource == "anonymous"` guard short-circuits before emit.
// An inverted condition would silently start auditing anonymous
// reads — a compliance regression this test makes impossible.
func TestRepos_AnonymousAuth_SkipsAuditEmit(t *testing.T) {
	spy := &reposSpy{}
	anon := &anonymousFakeQuerier{}
	// Route the embedded fake's auth lookups through the anonymous
	// path: no credentials are sent, so the resolver consults
	// GetOrgWithMetadata on the single-tenant org. The reposSpy
	// embeds fakeAuthQuerier; the anonymous querier embeds the same
	// shape, so we swap it in via a small adapter.
	emitter := &recordingEmitter{}
	srv := NewServer(Config{
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:           &compositeAnonReposQuerier{reposSpy: spy, anon: anon},
		EncryptionKey:     "0123456789abcdef0123456789abcdef",
		AuditEmitter:      emitter,
		SingleTenantOrgID: 1,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/repos", nil) // no creds
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d (body=%q), want 200", rec.Code, rec.Body.String())
	}
	if len(emitter.events) != 0 {
		t.Errorf("anonymous request must not emit audit; got %d events: %+v", len(emitter.events), emitter.events)
	}
}

// compositeAnonReposQuerier composes the anonymous org lookup with
// the reposSpy so the handler can both resolve anonymous auth AND
// hit the no-op db spy.
type compositeAnonReposQuerier struct {
	*reposSpy
	anon *anonymousFakeQuerier
}

func (c *compositeAnonReposQuerier) GetOrgWithMetadata(ctx context.Context, id int32) (db.Org, error) {
	return c.anon.GetOrgWithMetadata(ctx, id)
}

// TestRepos_QueryTooLong_400 confirms the length cap on the filter
// substring fires before the DB layer. A 10MB query string is
// well past every real Repo.name length so the bound is safe.
func TestRepos_QueryTooLong_400(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos?query="+strings.Repeat("a", 257), secret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
	wantBody := `{"statusCode":400,"errorCode":"INVALID_QUERY_PARAMS","message":"query must be 256 characters or fewer."}`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
	if spy.listCalls != 0 || spy.countCalls != 0 {
		t.Errorf("DB called on oversize query: list=%d count=%d", spy.listCalls, spy.countCalls)
	}
}

// TestRepos_PageOverflow_400 confirms an adversarial `page=` value
// past the 1,000,000 cap rejects at the boundary so `(page-1)*perPage`
// cannot overflow int32 and produce a negative skip.
func TestRepos_PageOverflow_400(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos?page=1000001", secret))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

// TestRepos_ClientSourceHeader_TruncatedAt128 confirms an oversize
// X-Codeintel-Client-Source header is capped before landing on the
// audit event. Without the truncation a hostile caller can balloon
// every audit event up to net/http's MaxHeaderBytes ceiling.
func TestRepos_ClientSourceHeader_TruncatedAt128(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
	}
	emitter := &recordingEmitter{}
	srv := newReposTestServer(spy, emitter)

	bigSource := strings.Repeat("x", 500)
	req := reposAuthedRequest(t, "/api/repos", secret)
	req.Header.Set("X-Codeintel-Client-Source", bigSource)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(emitter.events))
	}
	got, _ := emitter.events[0].Metadata["source"].(string)
	if len(got) != 128 {
		t.Errorf("source not truncated: got len=%d, want 128", len(got))
	}
}

// TestRepos_EmptyDisplayNamePreservedOnWire locks the wire-shape
// edge case: a non-nil, empty-string `displayName` column emits
// the key as `""` rather than being dropped (matching the JS
// `?? undefined` semantics, which only drops null/undefined).
func TestRepos_EmptyDisplayNamePreservedOnWire(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	empty := ""
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{RepoID: 1, RepoName: "r", RepoDisplayName: &empty},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	wantBody := `[{"codeHostType":"","repoId":1,"repoName":"r","webUrl":"","repoDisplayName":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false}]`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
}

// TestRepos_FullProjection_ByteEqualBody locks the wire shape for
// a row with every extended scalar populated: codeHostType
// surfaces as a non-empty string, externalWebUrl / imageUrl /
// pushedAt / defaultBranch all emit their keys, and the boolean
// flags echo as `true` rather than being dropped by omitempty
// (the JSON tags are explicit, so any future omitempty regression
// on isFork / isArchived fails this test).
func TestRepos_FullProjection_ByteEqualBody(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	indexedAt := time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC)
	pushedAt := time.Date(2025, 4, 15, 0, 0, 0, 0, time.UTC)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{
				RepoID:          501,
				RepoName:        "full",
				RepoDisplayName: strPtr("Full Display"),
				IndexedAt:       &indexedAt,
				CodeHostType:    strPtr("gitlab"),
				WebUrl:          strPtr("https://gitlab.com/x/full"),
				ImageUrl:        strPtr("https://cdn/img.png"),
				PushedAt:        &pushedAt,
				DefaultBranch:   strPtr("trunk"),
				IsFork:          true,
				IsArchived:      true,
			},
		},
		countTotal: 1,
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	wantBody := `[{"codeHostType":"gitlab","repoId":501,"repoName":"full","webUrl":"","repoDisplayName":"Full Display","externalWebUrl":"https://gitlab.com/x/full","imageUrl":"https://cdn/img.png","indexedAt":"2025-02-01T00:00:00.000Z","pushedAt":"2025-04-15T00:00:00.000Z","defaultBranch":"trunk"` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":true,"isArchived":true}]`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
}

// TestRepos_OptionalScalarsOmittedWhenNull confirms each optional
// scalar drops its JSON key when the underlying DB column is NULL
// — the omitempty contract is per-field, not blanket.
func TestRepos_OptionalScalarsOmittedWhenNull(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{RepoID: 600, RepoName: "barebones"},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Single byte-equal assertion locks both the absent-when-null
	// invariant for the six optional keys AND the always-present
	// invariant for the required scalars / booleans.
	wantBody := `[{"codeHostType":"","repoId":600,"repoName":"barebones","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false}]`
	if body != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", body, wantBody)
	}
}

// TestRepos_ExternalWebUrlAndImageUrl_DistinctSourceColumns locks
// the column→field binding for the two source columns that look
// similar (`webUrl` and `imageUrl`). A row with WebUrl set + ImageUrl
// NULL (and vice versa) catches an accidental swap of
// `ExternalWebUrl: row.WebUrl` ↔ `ImageUrl: row.ImageUrl` that a
// full-projection-only test would miss.
func TestRepos_ExternalWebUrlAndImageUrl_DistinctSourceColumns(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			// Row 1: external link present, no image. ExternalWebUrl
			// must be the WebUrl value; ImageUrl key must be absent.
			{RepoID: 800, RepoName: "linked", WebUrl: strPtr("https://external.example/r"), ImageUrl: nil},
			// Row 2: image present, no external link. ImageUrl must
			// be the ImageUrl value; ExternalWebUrl key must be absent.
			{RepoID: 801, RepoName: "imaged", WebUrl: nil, ImageUrl: strPtr("https://cdn/x.png")},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	wantBody := `[{"codeHostType":"","repoId":800,"repoName":"linked","webUrl":"","externalWebUrl":"https://external.example/r"` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false},{"codeHostType":"","repoId":801,"repoName":"imaged","webUrl":"","imageUrl":"https://cdn/x.png"` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false}]`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
}

// TestRepos_BooleansEchoAsTrueWhenSet confirms the flag fields
// surface as `true` JSON values when the DB columns are TRUE — no
// hidden omitempty on a bool false would mask this regression.
func TestRepos_BooleansEchoAsTrueWhenSet(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{RepoID: 700, RepoName: "fork-of-x", IsFork: true, IsArchived: false},
			{RepoID: 701, RepoName: "old-stuff", IsFork: false, IsArchived: true},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"isFork":true,"isArchived":false`) {
		t.Errorf("first-row flags not (true,false): %s", body)
	}
	if !strings.Contains(body, `"isFork":false,"isArchived":true`) {
		t.Errorf("second-row flags not (false,true): %s", body)
	}
}

// TestRepos_LatestJob_BothShapes locks the wire shape of the
// `latestJob` nested object: a Repo with a job emits all six
// sub-fields with completedAt + errorMessage either populated or
// JSON `null`; a Repo without any job emits `latestJob: null` at
// the top level rather than omitting the key.
func TestRepos_LatestJob_BothShapes(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	createdAt := time.Date(2025, 5, 1, 12, 0, 0, 0, time.UTC)
	completedAt := time.Date(2025, 5, 1, 12, 5, 0, 0, time.UTC)
	errMsg := "boom"
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			// Job present with all fields populated.
			{
				RepoID: 901, RepoName: "with-failed-job",
				LatestJob: &db.RepoListJobRow{
					ID: "ckjob1", Type: "INDEX", Status: "FAILED",
					CreatedAt:    createdAt,
					CompletedAt:  &completedAt,
					ErrorMessage: &errMsg,
				},
			},
			// Job present with completedAt + errorMessage NULL.
			{
				RepoID: 902, RepoName: "with-running-job",
				LatestJob: &db.RepoListJobRow{
					ID: "ckjob2", Type: "INDEX", Status: "IN_PROGRESS",
					CreatedAt: createdAt,
				},
			},
			// No job — latestJob: null at the top level.
			{RepoID: 903, RepoName: "no-job", LatestJob: nil},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	wantBody := `[` +
		`{"codeHostType":"","repoId":901,"repoName":"with-failed-job","webUrl":""` + repoListFailedNoActiveFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":{"id":"ckjob1","type":"INDEX","status":"FAILED","createdAt":"2025-05-01T12:00:00.000Z","completedAt":"2025-05-01T12:05:00.000Z","errorMessage":"boom"},"isFork":false,"isArchived":false},` +
		`{"codeHostType":"","repoId":902,"repoName":"with-running-job","webUrl":""` + repoListIndexingNoActiveFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":{"id":"ckjob2","type":"INDEX","status":"IN_PROGRESS","createdAt":"2025-05-01T12:00:00.000Z","completedAt":null,"errorMessage":null},"isFork":false,"isArchived":false},` +
		`{"codeHostType":"","repoId":903,"repoName":"no-job","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false}` +
		`]`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
}

// TestRepos_CodeIntelScip_PopulatedAndNull locks the wire shape of
// the nested `codeIntel.scip` block. A Repo with a CodeIntelIndex
// surfaces all eleven scalars in the wire-defined field order; a
// Repo without one emits `"scip": null`. `codeGraph` always emits
// `null` until slice 38 ports the codeGraph projection.
func TestRepos_CodeIntelScip_PopulatedAndNull(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	scipIndexedAt := time.Date(2025, 4, 1, 0, 0, 0, 0, time.UTC)
	errMsg := "indexer crashed"
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			// Row 1: SCIP index present with every scalar populated.
			{
				RepoID: 1001, RepoName: "with-scip",
				LatestScip: &db.RepoListScipRow{
					ID:                "ckscip1",
					Kind:              "SCIP",
					Status:            "READY",
					Revision:          "abc123",
					CommitHash:        "deadbeef",
					LanguageCount:     3,
					SymbolCount:       1500,
					OccurrenceCount:   8000,
					RelationshipCount: 400,
					IndexedAt:         &scipIndexedAt,
					ErrorMessage:      nil,
				},
			},
			// Row 2: SCIP index present but failed (errorMessage set,
			// indexedAt null, counts zero).
			{
				RepoID: 1002, RepoName: "failed-scip",
				LatestScip: &db.RepoListScipRow{
					ID:                "ckscip2",
					Kind:              "SCIP",
					Status:            "FAILED",
					Revision:          "xyz789",
					CommitHash:        "cafebabe",
					LanguageCount:     0,
					SymbolCount:       0,
					OccurrenceCount:   0,
					RelationshipCount: 0,
					IndexedAt:         nil,
					ErrorMessage:      &errMsg,
				},
			},
			// Row 3: no SCIP index — scip: null.
			{RepoID: 1003, RepoName: "no-scip", LatestScip: nil},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	wantBody := `[` +
		`{"codeHostType":"","repoId":1001,"repoName":"with-scip","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":{"id":"ckscip1","kind":"SCIP","status":"READY","revision":"abc123","commitHash":"deadbeef","languageCount":3,"symbolCount":1500,"occurrenceCount":8000,"relationshipCount":400,"indexedAt":"2025-04-01T00:00:00.000Z","errorMessage":null},"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false},` +
		`{"codeHostType":"","repoId":1002,"repoName":"failed-scip","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":{"id":"ckscip2","kind":"SCIP","status":"FAILED","revision":"xyz789","commitHash":"cafebabe","languageCount":0,"symbolCount":0,"occurrenceCount":0,"relationshipCount":0,"indexedAt":null,"errorMessage":"indexer crashed"},"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false},` +
		`{"codeHostType":"","repoId":1003,"repoName":"no-scip","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false}` +
		`]`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
}

// TestRepos_CodeGraph_PopulatedShape locks the wire shape of the
// nested `codeIntel.codeGraph` block when a CodeGraphIndex row
// exists. All graph scalars surface in the wire-defined order plus
// the compact active revision derived from the latest READY snapshot
// so Atom does not show a usable graph as inactive.
func TestRepos_CodeGraph_PopulatedShape(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	cgIndexedAt := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	cgSupersededAt := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	cgDeleteAfter := time.Date(2025, 7, 1, 0, 0, 0, 0, time.UTC)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{
				RepoID: 2001, RepoName: "with-cg",
				LatestCodeGraph: &db.RepoListCodeGraphRow{
					ID:              "ckcg1",
					Provider:        "NEBULA",
					Status:          "READY",
					SourceRevision:  strPtr("HEAD"),
					CommitHash:      "deadbeef",
					GraphSpace:      strPtr("codeintel"),
					WorkspaceID:     "ws-1",
					SchemaVersion:   2,
					BuilderVersion:  "builder-v3",
					VertexCount:     10000,
					EdgeCount:       50000,
					AnchorCount:     800,
					LinkedEdgeCount: 3000,
					IndexedAt:       &cgIndexedAt,
					SupersededAt:    &cgSupersededAt,
					DeleteAfter:     &cgDeleteAfter,
					ErrorMessage:    nil,
				},
			},
			{RepoID: 2002, RepoName: "no-cg"},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	wantBody := `[` +
		`{"codeHostType":"","repoId":2001,"repoName":"with-cg","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":{"id":"ckcg1","provider":"NEBULA","isActive":false,"status":"READY","sourceRevision":"HEAD","commitHash":"deadbeef","graphSpace":"codeintel","workspaceId":"ws-1","schemaVersion":2,"builderVersion":"builder-v3","vertexCount":10000,"edgeCount":50000,"anchorCount":800,"linkedEdgeCount":3000,"activeRevisionCount":0,"activeRevisions":[],"indexedAt":"2025-05-01T00:00:00.000Z","supersededAt":"2025-06-01T00:00:00.000Z","deleteAfter":"2025-07-01T00:00:00.000Z","errorMessage":null}},"latestJob":null,"isFork":false,"isArchived":false},` +
		`{"codeHostType":"","repoId":2002,"repoName":"no-cg","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":null},"latestJob":null,"isFork":false,"isArchived":false}` +
		`]`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
}

func TestRepos_CodeGraph_ActiveLatestReadySnapshot(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	cgIndexedAt := time.Date(2025, 5, 1, 0, 0, 0, 0, time.UTC)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{
				RepoID: 2001, RepoName: "with-active-cg",
				LatestCodeGraph: &db.RepoListCodeGraphRow{
					ID:              "ckcg-active",
					Provider:        "NEBULA",
					Status:          "READY",
					SourceRevision:  strPtr("refs/heads/main"),
					CommitHash:      "cafebabe",
					GraphSpace:      strPtr("codeintel"),
					WorkspaceID:     "ws-1",
					SchemaVersion:   1,
					BuilderVersion:  "builder-v7",
					VertexCount:     10,
					EdgeCount:       20,
					AnchorCount:     5,
					LinkedEdgeCount: 4,
					IndexedAt:       &cgIndexedAt,
				},
			},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	wantBody := `[` +
		`{"codeHostType":"","repoId":2001,"repoName":"with-active-cg","webUrl":""` + repoListNotIndexedFields + `,"codeIntel":{"scip":null,"codeGraph":{"id":"ckcg-active","provider":"NEBULA","isActive":true,"status":"READY","sourceRevision":"refs/heads/main","commitHash":"cafebabe","graphSpace":"codeintel","workspaceId":"ws-1","schemaVersion":1,"builderVersion":"builder-v7","vertexCount":10,"edgeCount":20,"anchorCount":5,"linkedEdgeCount":4,"activeRevisionCount":1,"activeRevisions":[{"revision":"refs/heads/main","commitHash":"cafebabe","activatedAt":"2025-05-01T00:00:00.000Z"}],"indexedAt":"2025-05-01T00:00:00.000Z","supersededAt":null,"deleteAfter":null,"errorMessage":null}},"latestJob":null,"isFork":false,"isArchived":false}` +
		`]`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body byte-mismatch:\n got: %s\nwant: %s", got, wantBody)
	}
}

// TestRepos_IndexedAtFormat_3DigitMillisUTC locks the ISO-8601
// wire shape on the timestamp: a non-UTC source time projects to
// UTC and prints with exactly 3 millisecond digits.
func TestRepos_IndexedAtFormat_3DigitMillisUTC(t *testing.T) {
	const secret = "ownersec"
	hash := auth.HashSecret("0123456789abcdef0123456789abcdef", secret)
	// Source time: 2025-06-15T08:30:00.500-05:00. UTC projection:
	// 2025-06-15T13:30:00.500Z.
	loc := time.FixedZone("CST", -5*60*60)
	src := time.Date(2025, 6, 15, 8, 30, 0, 500*int(time.Millisecond), loc)
	spy := &reposSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		listRows: []db.RepoListRow{
			{RepoID: 1, RepoName: "r", IndexedAt: &src},
		},
	}
	srv := newReposTestServer(spy, nil)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, reposAuthedRequest(t, "/api/repos", secret))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(decoded) != 1 {
		t.Fatalf("want 1 row, got %d", len(decoded))
	}
	if got := decoded[0]["indexedAt"]; got != "2025-06-15T13:30:00.500Z" {
		t.Errorf("indexedAt: got %v, want 2025-06-15T13:30:00.500Z", got)
	}
}
