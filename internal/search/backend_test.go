package search

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/db"

	zoektpb "github.com/sourcegraph/zoekt/grpc/protos/zoekt/webserver/v1"
	zoektquery "github.com/sourcegraph/zoekt/query"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"
)

type fakeClient struct {
	resp  *zoektpb.SearchResponse
	resps []*zoektpb.SearchResponse
	err   error
	calls []fakeClientCall
}

type fakeClientCall struct {
	md  metadata.MD
	req *zoektpb.SearchRequest
}

func (f *fakeClient) Search(ctx context.Context, req *zoektpb.SearchRequest, _ ...grpc.CallOption) (*zoektpb.SearchResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	f.calls = append(f.calls, fakeClientCall{md: md, req: req})
	if f.err != nil {
		return nil, f.err
	}
	if len(f.resps) > 0 {
		idx := len(f.calls) - 1
		if idx >= len(f.resps) {
			idx = len(f.resps) - 1
		}
		return f.resps[idx], nil
	}
	return f.resp, nil
}

type fakeRepoLookup struct {
	orgID       int32
	ids         []int32
	names       []string
	policyOrgID int32
	policyNames []string
	rows        []db.SearchRepoRow
	policyRows  []db.SearchRepoRow
	err         error
	policyErr   error
}

func (f *fakeRepoLookup) ListOrgSearchRepos(_ context.Context, orgID int32, ids []int32, names []string) ([]db.SearchRepoRow, error) {
	f.orgID = orgID
	f.ids = append([]int32(nil), ids...)
	f.names = append([]string(nil), names...)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func (f *fakeRepoLookup) ListOrgSearchPolicyRepos(_ context.Context, orgID int32, repoNames []string) ([]db.SearchRepoRow, error) {
	f.policyOrgID = orgID
	f.policyNames = append([]string(nil), repoNames...)
	if f.policyErr != nil {
		return nil, f.policyErr
	}
	if f.policyRows != nil {
		return f.policyRows, nil
	}
	return f.rows, nil
}

func TestSearch_FanoutScopesTenantAndNormalizesResponse(t *testing.T) {
	display := "Collector"
	webURL := "https://github.com/open-telemetry/opentelemetry-collector"
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{ID: 101, Name: "github.com/open-telemetry/opentelemetry-collector", DisplayName: &display, CodeHostType: "github", WebURL: &webURL, IndexedAt: &indexedAt},
		{ID: 102, Name: "github.com/open-telemetry/opentelemetry-demo", CodeHostType: "github", IndexedAt: &indexedAt},
	}}
	clientA := &fakeClient{resp: zoektResp(101, "github.com/open-telemetry/opentelemetry-collector", "exporter/otlp.go", "go", "func ExportOTLP() {}", 7)}
	clientB := &fakeClient{resp: zoektResp(102, "github.com/open-telemetry/opentelemetry-demo", "src/frontend.ts", "TypeScript", "export const ExportOTLP = true", 11)}

	backend, err := NewBackend(context.Background(), Config{
		Endpoints:    []string{"http://zoekt-a:6070", "zoekt-b:6070"},
		QueryTimeout: 250 * time.Millisecond,
		RepoLookup:   lookup,
		ClientFactory: func(_ context.Context, endpoint string, _ Config) (ZoektClient, error) {
			if strings.Contains(endpoint, "zoekt-a") {
				return clientA, nil
			}
			return clientB, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID:     7,
		OrgDomain: "orga",
		Query:     "ExportOTLP",
		Options: map[string]any{
			"matches":      float64(5),
			"contextLines": float64(2),
		},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	for _, client := range []*fakeClient{clientA, clientB} {
		if len(client.calls) != 1 {
			t.Fatalf("client calls: got %d, want 1", len(client.calls))
		}
		call := client.calls[0]
		if got := call.md.Get(MetadataOrgID); len(got) != 1 || got[0] != "7" {
			t.Fatalf("org metadata: got %v, want [7]", got)
		}
		if got := call.md.Get(MetadataTenantID); len(got) != 1 || got[0] != "7" {
			t.Fatalf("tenant metadata: got %v, want [7]", got)
		}
		if !call.req.GetOpts().GetChunkMatches() || call.req.GetOpts().GetMaxMatchDisplayCount() != 5 || call.req.GetOpts().GetTotalMaxMatchCount() != 6 {
			t.Fatalf("search opts wrong: %+v", call.req.GetOpts())
		}
		q, err := zoektquery.QFromProto(call.req.GetQuery())
		if err != nil {
			t.Fatalf("QFromProto: %v", err)
		}
		if !containsBranch(q) || !strings.Contains(q.String(), `branch="HEAD"`) {
			t.Fatalf("query should include default HEAD scope: %s", q.String())
		}
	}
	if lookup.orgID != 7 {
		t.Fatalf("repo lookup org: got %d, want 7", lookup.orgID)
	}

	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 2 {
		t.Fatalf("files: got %d, want 2; raw=%s", len(decoded.Files), raw)
	}
	if decoded.Stats.ActualMatchCount != 2 || decoded.Stats.TotalMatchCount != 2 {
		t.Fatalf("stats mismatch: %+v", decoded.Stats)
	}
	if len(decoded.RepositoryInfo) != 2 {
		t.Fatalf("repositoryInfo: got %d, want 2", len(decoded.RepositoryInfo))
	}
	if decoded.Files[0].WebURL != "" {
		t.Fatalf("headless webUrl must stay empty, got %q", decoded.Files[0].WebURL)
	}
}

func TestSearch_ExplicitBranchesAndRepoScope(t *testing.T) {
	client := &fakeClient{resp: zoektResp(101, "github.com/acme/api", "main.go", "go", "func handler() {}", 1)}
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{{
		ID:           101,
		Name:         "github.com/acme/api",
		CodeHostType: "github",
		IndexedAt:    &indexedAt,
		Metadata:     []byte(`{"branches":["main","release"],"indexedRevisions":["refs/heads/main","refs/heads/release"]}`),
	}}}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	_, err = backend.Search(context.Background(), api.SearchRequest{
		OrgID:     7,
		OrgDomain: "orga",
		Query:     "handler",
		Options: map[string]any{
			"branches":        []any{"main", "release"},
			"repoSearchScope": []any{"github.com/acme/api"},
		},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	q, err := zoektquery.QFromProto(client.calls[0].req.GetQuery())
	if err != nil {
		t.Fatalf("QFromProto: %v", err)
	}
	qstr := q.String()
	for _, want := range []string{`branch="main"`, `branch="release"`, "reposet"} {
		if !strings.Contains(qstr, want) {
			t.Fatalf("query %q missing %q", qstr, want)
		}
	}
}

func TestSearch_NormalizesLegacyRevFilterToBranch(t *testing.T) {
	client := &fakeClient{resp: zoektResp(101, "github.com/acme/api", "branch.go", "go", "const marker = true", 1)}
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	defaultBranch := "release-a"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{{
		ID:            101,
		Name:          "github.com/acme/api",
		CodeHostType:  "github",
		DefaultBranch: &defaultBranch,
		IndexedAt:     &indexedAt,
		Metadata:      []byte(`{"branches":["release-a"],"indexedRevisions":["refs/heads/release-a"]}`),
	}}}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	_, err = backend.Search(context.Background(), api.SearchRequest{
		OrgID:     7,
		OrgDomain: "orga",
		Query:     `"tenant_one_secret_symbol" rev:refs/heads/release-a`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	q, err := zoektquery.QFromProto(client.calls[0].req.GetQuery())
	if err != nil {
		t.Fatalf("QFromProto: %v", err)
	}
	qstr := q.String()
	if !strings.Contains(qstr, `branch:"release-a"`) {
		t.Fatalf("legacy rev: filter was not normalized to the indexed branch name: %s", qstr)
	}
	if strings.Contains(qstr, "rev:") || strings.Contains(qstr, "refs/heads/release-a") {
		t.Fatalf("legacy rev filter leaked into Zoekt query: %s", qstr)
	}
}

func TestSearch_DoesNotRewriteQuotedRevText(t *testing.T) {
	got := normalizeRevisionFilters(`"rev:main" rev:feature`)
	if got != `"rev:main" branch:feature` {
		t.Fatalf("normalizeRevisionFilters = %q", got)
	}
}

func TestSearch_NormalizesBranchFilterRefs(t *testing.T) {
	got := normalizeRevisionFilters(`branch:refs/heads/release-a b:"refs/heads/main" rev:refs/tags/v1`)
	if got != `branch:release-a branch:"main" branch:v1` {
		t.Fatalf("normalizeRevisionFilters = %q", got)
	}
}

func TestSearch_DropsResultWhenRepositoryIDIsOutsideOrgEvenIfNameMatches(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{ID: 101, Name: "github.com/acme/api", CodeHostType: "github", IndexedAt: &indexedAt},
	}}
	client := &fakeClient{resp: zoektResp(202, "github.com/acme/api", "secret.go", "go", "const OtherTenantSecret = true", 1)}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: "OtherTenantSecret",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(lookup.ids) != 1 || lookup.ids[0] != 202 {
		t.Fatalf("lookup ids: got %v, want [202]", lookup.ids)
	}
	if len(lookup.names) != 0 {
		t.Fatalf("repo name fallback must not be requested when repositoryId is present, got %v", lookup.names)
	}

	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 {
		t.Fatalf("cross-org hit should be dropped, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_DropsResultWhenRepositoryIDMissingUnderOrgLookup(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{ID: 101, Name: "github.com/acme/api", CodeHostType: "github", IndexedAt: &indexedAt},
	}}
	client := &fakeClient{resp: zoektResp(0, "github.com/acme/api", "main.go", "go", "func handler() {}", 1)}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{OrgID: 7, Query: "handler"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(lookup.ids) != 0 {
		t.Fatalf("lookup ids: got %v, want none", lookup.ids)
	}
	if len(lookup.names) != 1 || lookup.names[0] != "github.com/acme/api" {
		t.Fatalf("lookup names: got %v, want repo name lookup only for diagnostics", lookup.names)
	}
	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 {
		t.Fatalf("id-less result must be dropped under tenant repo lookup: %+v raw=%s", decoded.Files, raw)
	}
}

func TestSearch_DropsResultWhenReturnedBranchIsHiddenByPolicy(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	defaultBranch := "release-b"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:            949,
			Name:          "github.com/acme/shared",
			CodeHostType:  "github",
			DefaultBranch: &defaultBranch,
			IndexedAt:     &indexedAt,
			Metadata:      []byte(`{"branches":["release-b"],"indexedRevisions":["refs/heads/release-b","refs/heads/hidden-orgb"]}`),
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(949, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "tenant_two_secret_symbol_hidden";`, 1, []string{"hidden-orgb"})}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"tenant_two_secret_symbol_hidden" rev:hidden-orgb`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("hidden branch should be rejected before Zoekt dispatch, calls=%d", len(client.calls))
	}
	if lookup.policyOrgID != 7 {
		t.Fatalf("policy lookup org: got %d, want 7", lookup.policyOrgID)
	}

	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 || decoded.Stats.TotalMatchCount != 0 {
		t.Fatalf("hidden branch hit should be invisible, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_DropsBranchlessResultWhenRevisionVisibilityIsActive(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	defaultBranch := "release-b"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:            949,
			Name:          "github.com/acme/shared",
			CodeHostType:  "github",
			DefaultBranch: &defaultBranch,
			IndexedAt:     &indexedAt,
			Metadata:      []byte(`{"branches":["release-b"],"indexedRevisions":["refs/heads/release-b"]}`),
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(949, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "tenant_two_secret_symbol_hidden";`, 1, nil)}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"tenant_two_secret_symbol_hidden"`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(client.calls) != 1 {
		t.Fatalf("policy-visible branch search should dispatch once, calls=%d", len(client.calls))
	}
	q, err := zoektquery.QFromProto(client.calls[0].req.GetQuery())
	if err != nil {
		t.Fatalf("QFromProto: %v", err)
	}
	qstr := q.String()
	if !strings.Contains(qstr, `branch="release-b"`) {
		t.Fatalf("query should be pre-scoped to the policy-visible branch, got %s", qstr)
	}
	if strings.Contains(qstr, `branch="HEAD"`) {
		t.Fatalf("policy-scoped query must not add default HEAD fallback, got %s", qstr)
	}

	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 {
		t.Fatalf("branchless match should fail closed when visibility is active, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_NoBranchUsesDefaultVisibleBranchOnly(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	defaultBranch := "main"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:            949,
			Name:          "github.com/acme/shared",
			CodeHostType:  "github",
			DefaultBranch: &defaultBranch,
			IndexedAt:     &indexedAt,
			Metadata:      []byte(`{"branches":["main","feature"],"indexedRevisions":["refs/heads/main","refs/heads/feature"]}`),
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(949, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "tenant_two_secret_symbol_feature";`, 1, []string{"feature"})}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"tenant_two_secret_symbol"`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(client.calls) != 1 {
		t.Fatalf("default branch search should dispatch once, calls=%d", len(client.calls))
	}
	q, err := zoektquery.QFromProto(client.calls[0].req.GetQuery())
	if err != nil {
		t.Fatalf("QFromProto: %v", err)
	}
	qstr := q.String()
	if !strings.Contains(qstr, `branch="main"`) {
		t.Fatalf("query should be pre-scoped to the repo default branch, got %s", qstr)
	}
	if strings.Contains(qstr, `branch="feature"`) {
		t.Fatalf("query must not fan out to every visible branch by default, got %s", qstr)
	}
	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 {
		t.Fatalf("feature result should be filtered from default branch search, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_NegatedBranchFilterDoesNotBecomeRequiredBranch(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	defaultBranch := "main"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:            949,
			Name:          "github.com/acme/shared",
			CodeHostType:  "github",
			DefaultBranch: &defaultBranch,
			IndexedAt:     &indexedAt,
			Metadata:      []byte(`{"branches":["main"],"indexedRevisions":["refs/heads/main"]}`),
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(949, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "tenant_two_secret_symbol";`, 1, []string{"main"})}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	_, err = backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"tenant_two_secret_symbol" -branch:hidden`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(client.calls) != 1 {
		t.Fatalf("negated branch filter should not reject before dispatch, calls=%d", len(client.calls))
	}
	q, err := zoektquery.QFromProto(client.calls[0].req.GetQuery())
	if err != nil {
		t.Fatalf("QFromProto: %v", err)
	}
	qstr := q.String()
	if !strings.Contains(qstr, `branch="main"`) || !strings.Contains(qstr, `not branch:"hidden"`) {
		t.Fatalf("query should preserve negative branch semantics and default scope, got %s", qstr)
	}
}

func TestSearch_NegatedRevisionAliasesDoNotBecomeRequiredBranch(t *testing.T) {
	for _, query := range []string{
		`"tenant_two_secret_symbol" -rev:hidden`,
		`"tenant_two_secret_symbol" -b:hidden`,
	} {
		t.Run(query, func(t *testing.T) {
			indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
			defaultBranch := "main"
			lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
				{
					ID:            949,
					Name:          "github.com/acme/shared",
					CodeHostType:  "github",
					DefaultBranch: &defaultBranch,
					IndexedAt:     &indexedAt,
					Metadata:      []byte(`{"branches":["main"],"indexedRevisions":["refs/heads/main"]}`),
				},
			}}
			client := &fakeClient{resp: zoektRespWithBranches(949, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "tenant_two_secret_symbol";`, 1, []string{"main"})}
			backend, err := NewBackend(context.Background(), Config{
				Endpoints:  []string{"zoekt:6070"},
				RepoLookup: lookup,
				ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
					return client, nil
				},
			})
			if err != nil {
				t.Fatalf("NewBackend: %v", err)
			}

			_, err = backend.Search(context.Background(), api.SearchRequest{
				OrgID: 7,
				Query: query,
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(client.calls) != 1 {
				t.Fatalf("negated revision alias should not reject before dispatch, calls=%d", len(client.calls))
			}
			q, err := zoektquery.QFromProto(client.calls[0].req.GetQuery())
			if err != nil {
				t.Fatalf("QFromProto: %v", err)
			}
			qstr := q.String()
			if !strings.Contains(qstr, `branch="main"`) || !strings.Contains(qstr, `not branch:"hidden"`) {
				t.Fatalf("query should preserve negative revision alias semantics and default scope, got %s", qstr)
			}
		})
	}
}

func TestSearch_MixedPolicyAndLegacyReposKeepsLegacyDefaultScope(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	defaultBranch := "release"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:            949,
			Name:          "github.com/acme/policy",
			CodeHostType:  "github",
			DefaultBranch: &defaultBranch,
			IndexedAt:     &indexedAt,
			Metadata:      []byte(`{"branches":["release"],"indexedRevisions":["refs/heads/release"]}`),
		},
		{
			ID:           950,
			Name:         "github.com/acme/legacy",
			CodeHostType: "github",
			IndexedAt:    &indexedAt,
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(950, "github.com/acme/legacy", "README.md", "Markdown", `tenant_legacy_symbol`, 1, []string{"HEAD"})}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"tenant_legacy_symbol"`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	q, err := zoektquery.QFromProto(client.calls[0].req.GetQuery())
	if err != nil {
		t.Fatalf("QFromProto: %v", err)
	}
	qstr := q.String()
	if !strings.Contains(qstr, `github.com/acme/legacy`) || !strings.Contains(qstr, `branch="HEAD"`) {
		t.Fatalf("mixed policy filter should retain legacy repo HEAD scope, got %s", qstr)
	}
	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 1 || decoded.Files[0].Repository != "github.com/acme/legacy" {
		t.Fatalf("legacy repo result should remain visible in mixed org, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_MetadataIndexedRevisionsWithoutIndexedAtIsNotSearchable(t *testing.T) {
	defaultBranch := "main"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:            949,
			Name:          "github.com/acme/stale",
			CodeHostType:  "github",
			DefaultBranch: &defaultBranch,
			Metadata:      []byte(`{"branches":["main"],"indexedRevisions":["refs/heads/main"]}`),
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(949, "github.com/acme/stale", "README.md", "Markdown", `tenant_stale_symbol`, 1, []string{"main"})}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"tenant_stale_symbol"`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("stale indexedRevisions without indexedAt should be rejected before dispatch, calls=%d", len(client.calls))
	}
	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 {
		t.Fatalf("stale metadata should not expose search results, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_AllowsResultWhenReturnedBranchIsPolicyVisible(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	defaultBranch := "release-b"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:            949,
			Name:          "github.com/acme/shared",
			CodeHostType:  "github",
			DefaultBranch: &defaultBranch,
			IndexedAt:     &indexedAt,
			Metadata:      []byte(`{"branches":["release-b"],"indexedRevisions":["refs/heads/release-b","refs/heads/hidden-orgb"]}`),
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(949, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "tenant_two_secret_symbol_release";`, 1, []string{"release-b"})}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"tenant_two_secret_symbol_release" rev:release-b`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 1 || decoded.Stats.ActualMatchCount != 1 {
		t.Fatalf("visible branch hit should remain, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_HiddenBranchOptionIsRejectedBeforeDispatch(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	defaultBranch := "release-b"
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:            949,
			Name:          "github.com/acme/shared",
			CodeHostType:  "github",
			DefaultBranch: &defaultBranch,
			IndexedAt:     &indexedAt,
			Metadata:      []byte(`{"branches":["release-b"],"indexedRevisions":["refs/heads/release-b","refs/heads/hidden-orgb"]}`),
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(949, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "tenant_two_secret_symbol_hidden";`, 1, []string{"hidden-orgb"})}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"tenant_two_secret_symbol_hidden"`,
		Options: map[string]any{
			"branch": "refs/heads/hidden-orgb",
		},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("hidden branch option should be rejected before Zoekt dispatch, calls=%d", len(client.calls))
	}
	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 {
		t.Fatalf("hidden branch option should return empty response, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_BlocksResultsWhileRemoveIndexIsPendingInProgressOrFailed(t *testing.T) {
	for _, status := range []string{"PENDING", "IN_PROGRESS", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
			defaultBranch := "release-a"
			jobType := "REMOVE_INDEX"
			jobStatus := status
			lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
				{
					ID:              948,
					Name:            "github.com/acme/shared",
					CodeHostType:    "github",
					DefaultBranch:   &defaultBranch,
					IndexedAt:       &indexedAt,
					Metadata:        []byte(`{"branches":["release-a"],"indexedRevisions":["refs/heads/release-a"]}`),
					LatestJobType:   &jobType,
					LatestJobStatus: &jobStatus,
				},
			}}
			client := &fakeClient{resp: zoektRespWithBranches(948, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "tenant_one_secret_symbol";`, 1, []string{"release-a"})}
			backend, err := NewBackend(context.Background(), Config{
				Endpoints:  []string{"zoekt:6070"},
				RepoLookup: lookup,
				ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
					return client, nil
				},
			})
			if err != nil {
				t.Fatalf("NewBackend: %v", err)
			}

			raw, err := backend.Search(context.Background(), api.SearchRequest{
				OrgID: 7,
				Query: `"tenant_one_secret_symbol"`,
			})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(client.calls) != 0 {
				t.Fatalf("remove-index %s should be rejected before Zoekt dispatch, calls=%d", status, len(client.calls))
			}

			var decoded searchResponse
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("unmarshal response: %v\n%s", err, raw)
			}
			if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 || decoded.Stats.TotalMatchCount != 0 {
				t.Fatalf("remove-index %s should hide existing shards, decoded=%+v raw=%s", status, decoded, raw)
			}
		})
	}
}

func TestSearch_BlocksStaleShardWhenRepoHasNoActiveIndex(t *testing.T) {
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{
		{
			ID:           948,
			Name:         "github.com/acme/shared",
			CodeHostType: "github",
			Metadata:     []byte(`{}`),
		},
	}}
	client := &fakeClient{resp: zoektRespWithBranches(948, "github.com/acme/shared", "src/tenant.ts", "TypeScript", `export const marker = "stale_secret_symbol";`, 1, []string{"main"})}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{
		OrgID: 7,
		Query: `"stale_secret_symbol"`,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(client.calls) != 0 {
		t.Fatalf("repo without active index should be rejected before Zoekt dispatch, calls=%d", len(client.calls))
	}
	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if len(decoded.Files) != 0 || decoded.Stats.ActualMatchCount != 0 || decoded.Stats.TotalMatchCount != 0 {
		t.Fatalf("stale shard should be hidden, decoded=%+v raw=%s", decoded, raw)
	}
}

func TestSearch_ReplicatedUsesFirstAvailable(t *testing.T) {
	first := &fakeClient{err: errors.New("down")}
	second := &fakeClient{resp: zoektResp(101, "github.com/acme/api", "main.go", "go", "handler()", 1)}
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{{ID: 101, Name: "github.com/acme/api", CodeHostType: "github", IndexedAt: &indexedAt}}}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt-a:6070", "zoekt-b:6070"},
		Replicated: true,
		RepoLookup: lookup,
		ClientFactory: func(_ context.Context, endpoint string, _ Config) (ZoektClient, error) {
			if strings.Contains(endpoint, "zoekt-a") {
				return first, nil
			}
			return second, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	if _, err := backend.Search(context.Background(), api.SearchRequest{OrgID: 7, Query: "handler"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(first.calls) != 1 || len(second.calls) != 1 {
		t.Fatalf("replicated calls: first=%d second=%d, want one each", len(first.calls), len(second.calls))
	}
}

func TestSearch_RetriesZeroShardWarmup(t *testing.T) {
	oldDelay := shardWarmupRetryDelay
	shardWarmupRetryDelay = func(int) time.Duration { return 0 }
	defer func() { shardWarmupRetryDelay = oldDelay }()

	client := &fakeClient{resps: []*zoektpb.SearchResponse{
		zeroShardResp(),
		zoektResp(101, "github.com/acme/api", "tenant.ts", "TypeScript", `export const marker = "tenant_one_secret_symbol";`, 1),
	}}
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{{ID: 101, Name: "github.com/acme/api", CodeHostType: "github", IndexedAt: &indexedAt}}}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return client, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	raw, err := backend.Search(context.Background(), api.SearchRequest{OrgID: 7, Query: "tenant_one_secret_symbol"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(client.calls) != 2 {
		t.Fatalf("client calls: got %d, want 2", len(client.calls))
	}
	var decoded searchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal response: %v\n%s", err, raw)
	}
	if decoded.Stats.ActualMatchCount != 1 || decoded.Stats.ShardsScanned != 1 {
		t.Fatalf("retry should return loaded shard result, got %+v raw=%s", decoded.Stats, raw)
	}
}

func TestSearch_InvalidQueryReturnsTypedError(t *testing.T) {
	indexedAt := time.Date(2026, 5, 28, 6, 30, 0, 0, time.UTC)
	lookup := &fakeRepoLookup{rows: []db.SearchRepoRow{{ID: 101, Name: "github.com/acme/api", CodeHostType: "github", IndexedAt: &indexedAt}}}
	backend, err := NewBackend(context.Background(), Config{
		Endpoints:  []string{"zoekt:6070"},
		RepoLookup: lookup,
		ClientFactory: func(context.Context, string, Config) (ZoektClient, error) {
			return &fakeClient{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}

	_, err = backend.Search(context.Background(), api.SearchRequest{OrgID: 7, Query: `"unterminated`})
	if !errors.Is(err, api.ErrSearchInvalidQuery) {
		t.Fatalf("got %v, want ErrSearchInvalidQuery", err)
	}
}

func TestNewBackend_NoEndpoints(t *testing.T) {
	_, err := NewBackend(context.Background(), Config{})
	if !errors.Is(err, api.ErrSearchBackendNotConfigured) {
		t.Fatalf("got %v, want ErrSearchBackendNotConfigured", err)
	}
}

func zeroShardResp() *zoektpb.SearchResponse {
	return &zoektpb.SearchResponse{
		Stats: &zoektpb.Stats{
			Duration:    durationpb.New(1 * time.Millisecond),
			FlushReason: zoektpb.FlushReason_FLUSH_REASON_FINAL_FLUSH,
		},
	}
}

func zoektResp(repoID uint32, repoName, fileName, language, content string, line uint32) *zoektpb.SearchResponse {
	return zoektRespWithBranches(repoID, repoName, fileName, language, content, line, []string{"HEAD"})
}

func zoektRespWithBranches(repoID uint32, repoName, fileName, language, content string, line uint32, branches []string) *zoektpb.SearchResponse {
	return &zoektpb.SearchResponse{
		Stats: &zoektpb.Stats{
			MatchCount:           1,
			FileCount:            1,
			ShardFilesConsidered: 1,
			FilesConsidered:      1,
			FilesLoaded:          1,
			ShardsScanned:        1,
			Duration:             durationpb.New(2 * time.Millisecond),
			FlushReason:          zoektpb.FlushReason_FLUSH_REASON_FINAL_FLUSH,
		},
		Files: []*zoektpb.FileMatch{{
			FileName:     []byte(fileName),
			Repository:   repoName,
			RepositoryId: repoID,
			Language:     language,
			Branches:     branches,
			ChunkMatches: []*zoektpb.ChunkMatch{{
				Content:      []byte(content),
				ContentStart: &zoektpb.Location{LineNumber: line, Column: 1},
				Ranges: []*zoektpb.Range{{
					Start: &zoektpb.Location{ByteOffset: 0, LineNumber: line, Column: 1},
					End:   &zoektpb.Location{ByteOffset: 6, LineNumber: line, Column: 7},
				}},
				SymbolInfo: []*zoektpb.SymbolInfo{{
					Sym:  "ExportOTLP",
					Kind: "function",
				}},
			}},
		}},
	}
}
