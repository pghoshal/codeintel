package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/graphreader"
	"codeintel/pkg/llmproxy"
	"codeintel/pkg/repopaths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/redis/go-redis/v9"
)

type fakeQuerier struct {
	repos         []db.RepoListRow
	total         int32
	models        []db.OrgLanguageModelRow
	readRepo      db.RepoReadRow
	readRepos     map[string]db.RepoReadRow
	secret        db.OrgSecretCiphertext
	symbolRows    []db.CodeIntelOccurrenceEvidence
	graphEvidence db.CodeGraphInspectionEvidence
	activeScopes  []db.CodeGraphActiveScope
	lastOrgID     int32
	lastRepo      string
	lastSecretKey string
	lastSymbol    db.FindOrgSymbolOccurrencesParams
	lastGraph     db.InspectOrgCodeGraphParams
	graphCalls    int
	scopeCalls    int
}

func (f *fakeQuerier) ListOrgRepos(_ context.Context, p db.ListOrgReposParams) ([]db.RepoListRow, error) {
	f.lastOrgID = p.OrgID
	return f.repos, nil
}

func (f *fakeQuerier) CountOrgRepos(_ context.Context, p db.CountOrgReposParams) (int32, error) {
	f.lastOrgID = p.OrgID
	return f.total, nil
}

func (f *fakeQuerier) ListEnabledOrgLanguageModels(_ context.Context, orgID int32) ([]db.OrgLanguageModelRow, error) {
	f.lastOrgID = orgID
	return f.models, nil
}

func (f *fakeQuerier) GetOrgRepoForRead(_ context.Context, orgID int32, repoName string) (db.RepoReadRow, error) {
	f.lastOrgID = orgID
	f.lastRepo = repoName
	if f.readRepos != nil {
		if row, ok := f.readRepos[repoName]; ok {
			return row, nil
		}
	}
	if f.readRepo.Name == "" || f.readRepo.Name != repoName {
		return db.RepoReadRow{}, db.ErrRepoNotFound
	}
	return f.readRepo, nil
}

func (f *fakeQuerier) GetOrgSecretCiphertext(_ context.Context, orgID int32, key string) (db.OrgSecretCiphertext, error) {
	f.lastOrgID = orgID
	f.lastSecretKey = key
	if f.secret.Key == "" || f.secret.Key != key {
		return db.OrgSecretCiphertext{}, db.ErrOrgSecretNotFound
	}
	return f.secret, nil
}

func (f *fakeQuerier) FindOrgSymbolOccurrences(_ context.Context, p db.FindOrgSymbolOccurrencesParams) ([]db.CodeIntelOccurrenceEvidence, error) {
	f.lastOrgID = p.OrgID
	f.lastSymbol = p
	return f.symbolRows, nil
}

func (f *fakeQuerier) ListActiveCodeGraphScopes(_ context.Context, p db.ListActiveCodeGraphScopesParams) ([]db.CodeGraphActiveScope, error) {
	f.lastOrgID = p.OrgID
	f.scopeCalls++
	if f.activeScopes != nil {
		return f.activeScopes, nil
	}
	return f.graphEvidence.ActiveScopes, nil
}

func (f *fakeQuerier) InspectOrgCodeGraph(_ context.Context, p db.InspectOrgCodeGraphParams) (db.CodeGraphInspectionEvidence, error) {
	f.lastOrgID = p.OrgID
	f.lastGraph = p
	f.graphCalls++
	if f.graphEvidence.Query == "" {
		f.graphEvidence.Query = p.Query
	}
	return f.graphEvidence, nil
}

type fakeSearchBackend struct {
	mu    sync.Mutex
	last  api.SearchRequest
	calls []api.SearchRequest
	body  json.RawMessage
	err   error
}

type fakeGraphReader struct {
	mu     sync.Mutex
	last   graphreader.InspectParams
	calls  []graphreader.InspectParams
	result graphreader.InspectResult
	err    error
}

type fakeLanguageModelClient struct {
	calls    []llmproxy.ChatRequest
	response llmproxy.ChatResponse
	err      error
}

type fakeGraphEvidenceCache struct {
	values map[string]cachedGraphInspection
	gets   int
	sets   int
}

func (f *fakeGraphEvidenceCache) GetGraphInspection(_ context.Context, key string) (cachedGraphInspection, bool, error) {
	f.gets++
	if f.values == nil {
		return cachedGraphInspection{}, false, nil
	}
	value, ok := f.values[key]
	return value, ok, nil
}

func (f *fakeGraphEvidenceCache) SetGraphInspection(_ context.Context, key string, value cachedGraphInspection, _ time.Duration) error {
	f.sets++
	if f.values == nil {
		f.values = map[string]cachedGraphInspection{}
	}
	f.values[key] = value
	return nil
}

func (f *fakeGraphEvidenceCache) GetCodegraphContext(_ context.Context, _ string) (toolResult, bool, error) {
	return toolResult{}, false, nil
}

func (f *fakeGraphEvidenceCache) SetCodegraphContext(_ context.Context, _ string, _ toolResult, _ time.Duration) error {
	return nil
}

func (f *fakeLanguageModelClient) CompleteChat(_ context.Context, req llmproxy.ChatRequest) (llmproxy.ChatResponse, error) {
	f.calls = append(f.calls, req)
	if f.err != nil {
		return llmproxy.ChatResponse{}, f.err
	}
	resp := f.response
	if resp.RequestID == "" {
		resp.RequestID = req.RequestID
	}
	if resp.Status == "" {
		resp.Status = "SUCCEEDED"
	}
	if resp.Model.Model == "" {
		resp.Model = req.Model
	}
	return resp, nil
}

type fakeAsyncLanguageModelClient struct {
	fakeLanguageModelClient
	starts []llmproxy.ChatRequest
	gets   []string
	status string
}

func (f *fakeAsyncLanguageModelClient) StartChat(_ context.Context, req llmproxy.ChatRequest) (llmproxy.ChatResponse, error) {
	f.starts = append(f.starts, req)
	status := f.status
	if status == "" {
		status = "IN_PROGRESS"
	}
	return llmproxy.ChatResponse{RequestID: req.RequestID, Status: status, Model: req.Model}, nil
}

func (f *fakeAsyncLanguageModelClient) GetChat(_ context.Context, _ int32, requestID string) (llmproxy.ChatResponse, error) {
	f.gets = append(f.gets, requestID)
	status := f.status
	if status == "" {
		status = "IN_PROGRESS"
	}
	resp := f.response
	resp.RequestID = requestID
	resp.Status = status
	if resp.Model.Model == "" && len(f.starts) > 0 {
		resp.Model = f.starts[0].Model
	}
	return resp, nil
}

func (f *fakeGraphReader) Inspect(_ context.Context, params graphreader.InspectParams) (graphreader.InspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = params
	f.calls = append(f.calls, params)
	if f.err != nil {
		return graphreader.InspectResult{}, f.err
	}
	return f.result, nil
}

func (f *fakeSearchBackend) Search(_ context.Context, req api.SearchRequest) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = req
	f.calls = append(f.calls, req)
	if f.err != nil {
		return nil, f.err
	}
	return f.body, nil
}

func TestToolsListAdvertisesFusedSemanticToolsAndHidesAskWithoutModels(t *testing.T) {
	b := NewBackend(Config{
		Queries:       &fakeQuerier{},
		SearchBackend: &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)},
		GraphReader:   &fakeGraphReader{},
	})
	resp, err := b.Handle(context.Background(), api.MCPRequest{
		OrgID:     7,
		OrgDomain: "orga",
		Method:    http.MethodPost,
		Body:      []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var decoded struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Body)
	}
	names := map[string]bool{}
	for _, tool := range decoded.Result.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"codegraph_context", "compare_branches", "find_symbol_definitions", "find_symbol_references", "graph_callers", "graph_callees", "graph_impact", "graph_minimal_context", "graph_path", "graph_status", "grep", "inspect_code_graph", "list_language_models", "list_repos", "list_tree", "read_file"} {
		if !names[want] {
			t.Fatalf("tools/list missing %s: %+v", want, names)
		}
	}
	if names["ask_codebase"] {
		t.Fatalf("ask_codebase must not be advertised until the real agent loop is ported")
	}
}

func TestToolsListHidesGraphToolsWhenNebulaReaderMissing(t *testing.T) {
	b := NewBackend(Config{
		Queries:       &fakeQuerier{},
		SearchBackend: &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)},
	})
	resp, err := b.Handle(context.Background(), api.MCPRequest{
		OrgID:     7,
		OrgDomain: "orga",
		Method:    http.MethodPost,
		Body:      []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var decoded struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Body)
	}
	names := map[string]bool{}
	for _, tool := range decoded.Result.Tools {
		names[tool.Name] = true
	}
	for _, hidden := range []string{"codegraph_context", "inspect_code_graph", "graph_callers", "graph_callees", "graph_impact", "graph_minimal_context", "graph_path", "graph_status"} {
		if names[hidden] {
			t.Fatalf("%s must not be advertised without a NebulaGraph reader: %+v", hidden, names)
		}
	}
}

func TestFindSymbolDefinitionsFusesPreciseScipAndZoektSupplement(t *testing.T) {
	main := "main"
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"docs/notes.md"},
			"repository":"github.com/acme/orders",
			"chunks":[{"content":"createOrder appears in docs too","contentStart":{"lineNumber":4},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &main,
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"branches":["main","feature"],"indexedRevisions":["refs/heads/main","refs/heads/feature"]}`),
		},
		symbolRows: []db.CodeIntelOccurrenceEvidence{{
			RepoID:         42,
			RepoName:       "github.com/acme/orders",
			DisplayName:    "createOrder",
			Symbol:         "scip-typescript github.com/acme/orders src/orders/createOrder.ts/createOrder().",
			FilePath:       "src/orders/createOrder.ts",
			StartLine:      4,
			StartCharacter: 13,
			EndLine:        4,
			EndCharacter:   24,
			Role:           "DEFINITION",
			LineContent:    ptrString("export async function createOrder(command) {"),
			Language:       ptrString("typescript"),
			Kind:           ptrString("function"),
			Revision:       "refs/heads/main",
			CommitHash:     "abc123",
		}},
	}
	b := NewBackend(Config{Queries: q, SearchBackend: search})
	resp := callTool(t, b, 7, "find_symbol_definitions", `{"symbol":"createOrder","repo":"github.com/acme/orders","revision":"main","definitionFile":"src/orders/createOrder.ts"}`)
	if toolIsError(t, resp) {
		t.Fatalf("find_symbol_definitions returned tool error: %s", resp.Body)
	}
	if q.lastSymbol.OrgID != 7 || len(q.lastSymbol.Repos) != 1 || q.lastSymbol.Repos[0] != "github.com/acme/orders" || q.lastSymbol.Mode != db.SymbolOccurrenceDefinitions {
		t.Fatalf("symbol query scope wrong: %+v", q.lastSymbol)
	}
	if len(q.lastSymbol.RepoRevisionScopes) != 1 || q.lastSymbol.RepoRevisionScopes[0].Repo != "github.com/acme/orders" || strings.Join(q.lastSymbol.RepoRevisionScopes[0].RevisionCandidates, ",") != "refs/heads/main" {
		t.Fatalf("revision scopes = %+v", q.lastSymbol.RepoRevisionScopes)
	}
	if q.lastSymbol.DefinitionFile != "src/orders/createOrder.ts" {
		t.Fatalf("definitionFile = %q", q.lastSymbol.DefinitionFile)
	}
	text := toolText(t, resp)
	for _, want := range []string{"Found 1 precise SCIP definitions and supplemental Zoekt text matches", "Precise SCIP definitions:", "src/orders/createOrder.ts", "Supplemental Zoekt text matches", "docs/notes.md"} {
		if !strings.Contains(text, want) {
			t.Fatalf("symbol output missing %q:\n%s", want, text)
		}
	}
}

func TestFindSymbolReferencesKeepsDefinitionFileAsSymbolFilter(t *testing.T) {
	main := "main"
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &main,
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		symbolRows: []db.CodeIntelOccurrenceEvidence{{
			RepoID:         42,
			RepoName:       "github.com/acme/orders",
			DisplayName:    "createOrder",
			Symbol:         "scip-typescript github.com/acme/orders src/orders/createOrder.ts/createOrder().",
			FilePath:       "src/routes/internalOrders.ts",
			StartLine:      5,
			StartCharacter: 24,
			Role:           "REFERENCE",
			LineContent:    ptrString("const order = await createOrder(command);"),
			Revision:       "refs/heads/main",
			CommitHash:     "abc123",
		}},
	}
	b := NewBackend(Config{Queries: q, SearchBackend: &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)}})
	resp := callTool(t, b, 7, "find_symbol_references", `{"symbol":"createOrder","repo":"github.com/acme/orders","definitionFile":"src/orders/createOrder.ts"}`)
	if toolIsError(t, resp) {
		t.Fatalf("find_symbol_references returned tool error: %s", resp.Body)
	}
	if q.lastSymbol.Mode != db.SymbolOccurrenceReferences || q.lastSymbol.DefinitionFile != "src/orders/createOrder.ts" {
		t.Fatalf("references query should keep definition file for symbol disambiguation: %+v", q.lastSymbol)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "src/routes/internalOrders.ts") || !strings.Contains(text, "REFERENCE") {
		t.Fatalf("references output wrong:\n%s", text)
	}
}

func TestInspectCodeGraphUsesPostgresEvidenceAndNebulaReader(t *testing.T) {
	start := int32(12)
	conf := 0.91
	main := "main"
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &main,
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		graphEvidence: db.CodeGraphInspectionEvidence{
			Query:               "trace checkout architecture flow",
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-1"},
			Symbols: []db.CodeGraphSymbolEvidence{{
				RepoID:      42,
				RepoName:    "github.com/acme/orders",
				DisplayName: "CheckoutService",
				FilePath:    ptrString("src/checkout/service.ts"),
				Revision:    "refs/heads/main",
			}},
			Relationships: []db.CodeGraphRelationshipEvidence{{
				RepoID:       42,
				RepoName:     "github.com/acme/orders",
				SourceSymbol: "CheckoutRoute",
				TargetSymbol: "CheckoutService",
				IsReference:  true,
				Revision:     "refs/heads/main",
			}},
			Anchors: []db.CodeGraphAnchorEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				Kind:             "route",
				Direction:        "PROVIDES",
				Key:              "POST /checkout",
				NormalizedKey:    "post /checkout",
				NodeVID:          "cg:o7:wabc:r42:c123:s1:babc:route:seed",
				WorkspaceID:      "ws-1",
				EvidenceFilePath: ptrString("src/routes/checkout.ts"),
				StartLine:        &start,
				Confidence:       1,
				Source:           "ast",
				Revision:         "refs/heads/main",
			}},
		},
	}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "architecture", Direction: "bidirectional", MaxDepth: 4, EdgeKinds: []string{"CALLS"}},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "cg:o7:wabc:r42:c123:s1:babc:route:seed",
			EdgeTargetVID:    "cg:o7:wabc:r42:c123:s1:babc:function:next",
			Depth:            1,
			Relation:         "CALLS",
			Confidence:       &conf,
			Source:           "ast",
			EvidenceFilePath: "src/routes/checkout.ts",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/main",
			Start:            graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:route:seed", RepoID: ptrInt32(42), Kind: "route", Label: "POST /checkout", Path: "src/routes/checkout.ts"},
			Neighbor:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:function:next", RepoID: ptrInt32(42), Kind: "function", Label: "CheckoutService.create", Path: "src/checkout/service.ts"},
		}},
	}}
	b := NewBackend(Config{Queries: q, GraphReader: reader})
	resp := callTool(t, b, 7, "inspect_code_graph", `{"query":"trace checkout architecture flow","repos":["github.com/acme/orders"],"limit":10}`)
	if toolIsError(t, resp) {
		t.Fatalf("inspect_code_graph returned tool error: %s", resp.Body)
	}
	if q.lastGraph.OrgID != 7 || len(q.lastGraph.Repos) != 1 || q.lastGraph.Repos[0] != "github.com/acme/orders" {
		t.Fatalf("graph DB scope wrong: %+v", q.lastGraph)
	}
	if reader.last.OrgID != 7 || len(reader.last.Seeds) != 1 || reader.last.Seeds[0].NodeVID == "" {
		t.Fatalf("graph reader scope/seeds wrong: %+v", reader.last)
	}
	text := toolText(t, resp)
	for _, want := range []string{"Graph DB query plan", "SCIP relationships", "Architecture facts", "Native graph traversal", "CheckoutRoute -> CheckoutService", "POST /checkout", "Files to read next"} {
		if !strings.Contains(text, want) {
			t.Fatalf("graph output missing %q:\n%s", want, text)
		}
	}
}

func TestFocusedGraphToolsRewriteIntentAndPreserveScope(t *testing.T) {
	main := "main"
	start := int32(3)
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &main,
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		graphEvidence: db.CodeGraphInspectionEvidence{
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-1"},
			Anchors: []db.CodeGraphAnchorEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				Kind:             "function",
				Direction:        "PROVIDES",
				Key:              "CheckoutController",
				NormalizedKey:    "checkoutcontroller",
				NodeVID:          "cg:o7:wabc:r42:c123:s1:babc:function:checkout",
				WorkspaceID:      "ws-1",
				EvidenceFilePath: ptrString("src/checkout/controller.ts"),
				StartLine:        &start,
				Confidence:       0.95,
				Source:           "tree-sitter-typescript",
				Revision:         "refs/heads/main",
			}},
		},
	}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "impact", Direction: "outgoing", MaxDepth: 4, EdgeKinds: []string{"CALLS"}},
	}}
	b := NewBackend(Config{Queries: q, GraphReader: reader})
	resp := callTool(t, b, 7, "graph_impact", `{"symbol":"CheckoutController","repos":["github.com/acme/orders"],"limit":10}`)
	if toolIsError(t, resp) {
		t.Fatalf("graph_impact returned tool error: %s", resp.Body)
	}
	if q.lastGraph.Query != "impact blast radius affected risk CheckoutController" {
		t.Fatalf("graph intent query = %q", q.lastGraph.Query)
	}
	if len(q.lastGraph.RepoRevisionScopes) != 1 || q.lastGraph.RepoRevisionScopes[0].Repo != "github.com/acme/orders" {
		t.Fatalf("graph scope not preserved: %+v", q.lastGraph)
	}
	if reader.last.OrgID != 7 || len(reader.last.Seeds) != 1 {
		t.Fatalf("graph reader not called with tenant seed: %+v", reader.last)
	}
	text := toolText(t, resp)
	for _, want := range []string{"Graph impact for \"CheckoutController\"", "Architecture facts", "tree-sitter-typescript"} {
		if !strings.Contains(text, want) {
			t.Fatalf("focused graph output missing %q:\n%s", want, text)
		}
	}
}

func TestGraphPathAndMinimalContextUseNebulaDepthAndCompactFormat(t *testing.T) {
	main := "main"
	start := int32(11)
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &main,
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		graphEvidence: db.CodeGraphInspectionEvidence{
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-1"},
			Anchors: []db.CodeGraphAnchorEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				Kind:             "route",
				Direction:        "PROVIDES",
				Key:              "POST /orders",
				NormalizedKey:    "post-orders",
				NodeVID:          "cg:o7:wabc:r42:c123:s1:babc:route:orders",
				WorkspaceID:      "ws-1",
				EvidenceFilePath: ptrString("src/routes/orders.ts"),
				StartLine:        &start,
				Confidence:       0.97,
				Source:           "tree-sitter-typescript",
				Revision:         "refs/heads/main",
			}},
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				Relation:         "CALLS",
				SourceExternalID: "route:POST /orders",
				TargetExternalID: "function:createOrder",
				SourceFile:       "src/routes/orders.ts",
				StartLine:        &start,
				Confidence:       0.91,
				ConfidenceTier:   "EXTRACTED",
				Source:           "tree-sitter-typescript",
				Revision:         "refs/heads/main",
			}},
		},
	}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "path", Direction: "bidirectional", MaxDepth: 5, EdgeKinds: []string{"CALLS"}},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "cg:o7:wabc:r42:c123:s1:babc:route:orders",
			EdgeTargetVID:    "cg:o7:wabc:r42:c123:s1:babc:function:create",
			Depth:            1,
			Relation:         "CALLS",
			Confidence:       ptrFloat64(0.94),
			Source:           "tree-sitter-typescript",
			EvidenceFilePath: "src/routes/orders.ts",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/main",
			Start:            graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:route:orders", RepoID: ptrInt32(42), Kind: "route", Label: "POST /orders", Path: "src/routes/orders.ts"},
			Neighbor:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:function:create", RepoID: ptrInt32(42), Kind: "function", Label: "createOrder", Path: "src/orders/createOrder.ts"},
		}},
	}}
	b := NewBackend(Config{Queries: q, GraphReader: reader})
	resp := callTool(t, b, 7, "graph_path", `{"query":"path from POST /orders to createOrder","repo":"github.com/acme/orders","depth":5,"limit":10}`)
	if toolIsError(t, resp) {
		t.Fatalf("graph_path returned tool error: %s", resp.Body)
	}
	if reader.last.MaxDepth != 5 {
		t.Fatalf("graph_path depth not forwarded: %+v", reader.last)
	}
	if !q.lastGraph.Compact {
		t.Fatalf("focused graph tools should request compact DB evidence")
	}
	text := toolText(t, resp)
	for _, want := range []string{"Graph path for", "Path-oriented graph evidence", "step 1", "POST /orders", "createOrder", "tree-sitter-typescript"} {
		if !strings.Contains(text, want) {
			t.Fatalf("graph_path output missing %q:\n%s", want, text)
		}
	}

	resp = callTool(t, b, 7, "graph_minimal_context", `{"query":"orders request flow","repo":"github.com/acme/orders","limit":10}`)
	if toolIsError(t, resp) {
		t.Fatalf("graph_minimal_context returned tool error: %s", resp.Body)
	}
	text = toolText(t, resp)
	for _, want := range []string{"Graph minimal context for", "High-signal connected flow", "AST/tree-sitter facts", "Next files for the coding agent"} {
		if !strings.Contains(text, want) {
			t.Fatalf("graph_minimal_context output missing %q:\n%s", want, text)
		}
	}
}

func TestGraphInspectLabelsLowConfidenceAstEdgesAsHeuristic(t *testing.T) {
	main := "main"
	start := int32(57)
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &main,
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		graphEvidence: db.CodeGraphInspectionEvidence{
			Query:               "appendIfNotSet flow",
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-1"},
		},
	}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "default", Direction: "bidirectional", MaxDepth: 3, EdgeKinds: []string{"CALLS"}},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "caller",
			EdgeTargetVID:    "callee",
			Depth:            1,
			Relation:         "CALLS",
			Confidence:       ptrFloat64(0.60),
			Source:           "ast-go",
			Provenance:       "heuristic",
			EvidenceFilePath: "internal/instrumentation/python.go",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/main",
			Start:            graphreader.Endpoint{VID: "caller", RepoID: ptrInt32(42), Kind: "function", Label: "injectPythonSDKToContainer", Path: "internal/instrumentation/python.go"},
			Neighbor:         graphreader.Endpoint{VID: "callee", RepoID: ptrInt32(42), Kind: "function", Label: "appendIfNotSet", Path: "internal/instrumentation/sdk.go"},
		}},
	}}
	b := NewBackend(Config{Queries: q, GraphReader: reader})
	resp := callTool(t, b, 7, "inspect_code_graph", `{"query":"appendIfNotSet flow","repo":"github.com/acme/orders","limit":10}`)
	if toolIsError(t, resp) {
		t.Fatalf("inspect_code_graph returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "Heuristic AST graph traversal") || !strings.Contains(text, "provenance=heuristic") {
		t.Fatalf("low-confidence AST edge should be labelled heuristic:\n%s", text)
	}
	if strings.Contains(text, "Native graph traversal") {
		t.Fatalf("low-confidence AST edge must not be presented as native/proven:\n%s", text)
	}
}

func TestGraphInspectWithoutRepoUsesEachRepoDefaultBranchScope(t *testing.T) {
	main := "main"
	dev := "dev"
	q := &fakeQuerier{
		repos: []db.RepoListRow{
			{RepoID: 42, RepoName: "github.com/acme/orders"},
			{RepoID: 43, RepoName: "github.com/acme/billing"},
		},
		readRepos: map[string]db.RepoReadRow{
			"github.com/acme/orders": {
				ID:            42,
				OrgID:         7,
				Name:          "github.com/acme/orders",
				DefaultBranch: &main,
				IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
				Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
			},
			"github.com/acme/billing": {
				ID:            43,
				OrgID:         7,
				Name:          "github.com/acme/billing",
				DefaultBranch: &dev,
				IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
				Metadata:      []byte(`{"indexedRevisions":["refs/heads/dev"]}`),
			},
		},
		graphEvidence: db.CodeGraphInspectionEvidence{
			SearchedRepoCount:   2,
			ActiveSnapshotCount: 2,
			WorkspaceIDs:        []string{"ws-1"},
			Anchors: []db.CodeGraphAnchorEvidence{{
				RepoID:        42,
				RepoName:      "github.com/acme/orders",
				Kind:          "service",
				Direction:     "PROVIDES",
				Key:           "OrdersService",
				NormalizedKey: "ordersservice",
				NodeVID:       "cg:o7:wabc:r42:c123:s1:babc:service:orders",
				WorkspaceID:   "ws-1",
				Confidence:    0.95,
				Source:        "tree-sitter-typescript",
				Revision:      "refs/heads/main",
			}},
		},
	}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "architecture", Direction: "bidirectional", MaxDepth: 4},
		Edges: []graphreader.Edge{{
			EdgeSourceVID: "cg:o7:wabc:r42:c123:s1:babc:service:orders",
			EdgeTargetVID: "cg:o7:wabc:r43:c456:s1:babc:service:billing",
			Depth:         1,
			Relation:      "CALLS",
			EdgeRepoID:    ptrInt32(42),
			EdgeRevision:  "refs/heads/main",
			Start:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:service:orders", RepoID: ptrInt32(42), Kind: "service", Label: "OrdersService"},
			Neighbor:      graphreader.Endpoint{VID: "cg:o7:wabc:r43:c456:s1:babc:service:billing", RepoID: ptrInt32(43), Kind: "service", Label: "BillingService"},
		}},
	}}
	b := NewBackend(Config{Queries: q, GraphReader: reader})
	resp := callTool(t, b, 7, "inspect_code_graph", `{"query":"orders to billing flow"}`)
	if toolIsError(t, resp) {
		t.Fatalf("inspect_code_graph returned tool error: %s", resp.Body)
	}
	if len(q.lastGraph.RepoRevisionScopes) != 2 {
		t.Fatalf("RepoRevisionScopes = %+v, want two default branch scopes", q.lastGraph.RepoRevisionScopes)
	}
	got := map[string]string{}
	for _, scope := range q.lastGraph.RepoRevisionScopes {
		if len(scope.RevisionCandidates) != 1 {
			t.Fatalf("scope has unexpected revisions: %+v", scope)
		}
		got[scope.Repo] = scope.RevisionCandidates[0]
	}
	if got["github.com/acme/orders"] != "refs/heads/main" || got["github.com/acme/billing"] != "refs/heads/dev" {
		t.Fatalf("default branch scopes wrong: %+v", got)
	}
}

func TestSemanticToolsRejectExplicitRefWithoutRepoScope(t *testing.T) {
	search := &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)}
	q := &fakeQuerier{}
	b := NewBackend(Config{Queries: q, SearchBackend: search})

	resp := callTool(t, b, 7, "find_symbol_definitions", `{"symbol":"createOrder","revision":"feature"}`)
	if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), "requires repo or repos") {
		t.Fatalf("explicit revision without repo scope should be rejected: %s", resp.Body)
	}
	if q.lastSymbol.Symbol != "" {
		t.Fatalf("SCIP query must not run before branch scope is verified: %+v", q.lastSymbol)
	}
	if len(search.calls) != 0 {
		t.Fatalf("Zoekt supplemental search must not run before branch scope is verified: %+v", search.calls)
	}
}

func TestGraphToolsRejectExplicitRefWithoutRepoScope(t *testing.T) {
	q := &fakeQuerier{}
	reader := &fakeGraphReader{}
	b := NewBackend(Config{Queries: q, GraphReader: reader})

	for _, tc := range []struct {
		tool string
		args string
	}{
		{"inspect_code_graph", `{"query":"orders flow","ref":"feature"}`},
		{"graph_status", `{"ref":"feature"}`},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			q.lastGraph = db.InspectOrgCodeGraphParams{}
			reader.last = graphreader.InspectParams{}
			resp := callTool(t, b, 7, tc.tool, tc.args)
			if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), "requires repo or repos") {
				t.Fatalf("explicit graph ref without repo scope should be rejected: %s", resp.Body)
			}
			if q.lastGraph.Query != "" {
				t.Fatalf("graph DB query must not run before branch scope is verified: %+v", q.lastGraph)
			}
			if reader.last.OrgID != 0 {
				t.Fatalf("Nebula traversal must not run before branch scope is verified: %+v", reader.last)
			}
		})
	}
}

func TestSemanticAndGraphToolsRejectWhileRemoveIndexActive(t *testing.T) {
	for _, status := range []string{"PENDING", "IN_PROGRESS", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			main := "main"
			jobType := "REMOVE_INDEX"
			jobStatus := status
			q := &fakeQuerier{readRepo: db.RepoReadRow{
				ID:              42,
				OrgID:           7,
				Name:            "github.com/acme/orders",
				DefaultBranch:   &main,
				IndexedAt:       ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
				Metadata:        []byte(`{"indexedRevisions":["refs/heads/main"]}`),
				LatestJobType:   &jobType,
				LatestJobStatus: &jobStatus,
			}}
			search := &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)}
			reader := &fakeGraphReader{}
			b := NewBackend(Config{Queries: q, SearchBackend: search, GraphReader: reader})

			symbol := callTool(t, b, 7, "find_symbol_definitions", `{"symbol":"createOrder","repo":"github.com/acme/orders"}`)
			if !toolIsError(t, symbol) || !strings.Contains(toolText(t, symbol), "ref is not indexed") {
				t.Fatalf("remove-index %s should block symbol tool: %s", status, symbol.Body)
			}
			if q.lastSymbol.Symbol != "" || len(search.calls) != 0 {
				t.Fatalf("symbol/Zoekt reads should not run during remove-index: symbol=%+v calls=%d", q.lastSymbol, len(search.calls))
			}

			graph := callTool(t, b, 7, "inspect_code_graph", `{"query":"orders flow","repo":"github.com/acme/orders"}`)
			if !toolIsError(t, graph) || !strings.Contains(toolText(t, graph), "ref is not indexed") {
				t.Fatalf("remove-index %s should block graph tool: %s", status, graph.Body)
			}
			if q.lastGraph.Query != "" || reader.last.OrgID != 0 {
				t.Fatalf("graph reads should not run during remove-index: query=%+v reader=%+v", q.lastGraph, reader.last)
			}
		})
	}
}

func TestToolsListHidesGrepWhenSearchBackendMissing(t *testing.T) {
	b := NewBackend(Config{Queries: &fakeQuerier{}})
	resp, err := b.Handle(context.Background(), api.MCPRequest{
		OrgID:     7,
		OrgDomain: "orga",
		Method:    http.MethodPost,
		Body:      []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var decoded struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("decode: %v\n%s", err, resp.Body)
	}
	for _, tool := range decoded.Result.Tools {
		if tool.Name == "grep" {
			t.Fatalf("grep must not be advertised without a search backend")
		}
	}
}

func TestProtocolRejectsMissingVersionBatchAndUnknownTool(t *testing.T) {
	b := NewBackend(Config{})
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing_jsonrpc", `{"id":1,"method":"tools/list"}`, `"code":-32600`},
		{"batch", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, `batch requests are not supported`},
		{"unknown_tool", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"not_real_tool","arguments":{}}}`, `"code":-32602`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := b.Handle(context.Background(), api.MCPRequest{
				OrgID:     7,
				OrgDomain: "orga",
				Method:    http.MethodPost,
				Body:      []byte(tc.body),
			})
			if err != nil {
				t.Fatalf("Handle: %v", err)
			}
			if !strings.Contains(string(resp.Body), tc.want) {
				t.Fatalf("body %s does not contain %s", resp.Body, tc.want)
			}
		})
	}
}

func TestMandatoryEvidenceRequiresBothScipDefinitionsAndReferences(t *testing.T) {
	pack := mandatoryEvidencePack{Sections: []mandatoryEvidenceSection{
		{Layer: "Zoekt broad recall", ToolName: "grep", Attempted: true, Satisfied: true, Output: "collector/exporter.go"},
		{Layer: "Postgres graph metadata + Nebula traversal", ToolName: "inspect_code_graph", Attempted: true, Satisfied: true, Output: "Native graph traversal (ranked NebulaGraph BFS neighborhood):\n- source=tree-sitter-go"},
		{Layer: "SCIP symbol definitions", ToolName: "find_symbol_definitions", Attempted: true, Satisfied: true, Output: "Found 1 precise SCIP definitions"},
		{Layer: "SCIP symbol references", ToolName: "find_symbol_references", Attempted: true, Satisfied: false, Output: "Found 0 precise SCIP references"},
	}}
	err := pack.RequireCoreEvidence()
	if err == nil || !strings.Contains(err.Error(), "SCIP symbol references returned no precise evidence") {
		t.Fatalf("expected missing references to block synthesis, got %v", err)
	}
}

func TestMandatoryEvidenceAllowsExactDirectFileLookupWhenGraphScipSparse(t *testing.T) {
	pack := mandatoryEvidencePack{Query: `Read src/tenant.ts and answer with the exact marker whose value starts with "tenant_".`, Sections: []mandatoryEvidenceSection{
		{Layer: "Direct file evidence for src/tenant.ts", ToolName: "read_file", Attempted: true, Satisfied: true, Output: "<content>\n1: tenant_one_secret_symbol\n</content>"},
		{Layer: "Primary fused codegraph context", ToolName: "codegraph_context", Attempted: true, Satisfied: false, Output: strings.Join([]string{
			`Codegraph context fused evidence pack for "read src/tenant.ts"`,
			"## Zoekt broad recall (`grep` ok)",
			"Found 1 matches in 1 files",
			"## Graph minimal context (`graph_minimal_context` ok)",
			"No minimal graph context found.",
			"## SCIP symbol precision (`find_symbol_definitions/find_symbol_references` ok)",
			"No code-like symbol token was detected; symbol tools remain available for follow-up.",
		}, "\n")},
	}}
	err := pack.RequireCoreEvidence()
	if err != nil {
		t.Fatalf("exact file lookup should be answerable from direct file evidence, got %v", err)
	}
}

func TestMandatoryEvidenceAllowsContextualLiteralFollowupWithSemanticGraph(t *testing.T) {
	pack := mandatoryEvidencePack{Query: `Round 2: without me restating the marker, return the file path, symbol, and next coding action for the marker we discussed.

Relevant prior code anchors for retrieval only:
- tenant_one_secret_symbol
- src/tenant.ts
- orga_owned_function`, Sections: []mandatoryEvidenceSection{
		{Layer: "Primary fused codegraph context", ToolName: "codegraph_context", Attempted: true, Satisfied: false, Output: strings.Join([]string{
			`Codegraph context fused evidence pack for "marker follow-up"`,
			"## Zoekt broad recall (`grep` ok)",
			"Found 2 matches in 2 files",
			"## Graph minimal context (`graph_minimal_context` ok)",
			"Native graph traversal (ranked NebulaGraph BFS neighborhood):",
			"- orga_owned_function --REFERENCES--> tenantMarker @ src/tenant.ts:2 / source=scip; confidence=0.95",
			"## SCIP definitions (`find_symbol_definitions` ok)",
			"No precise SCIP definitions found for marker literal.",
			"## SCIP references (`find_symbol_references` ok)",
			"No precise SCIP references found for marker literal.",
		}, "\n")},
	}}
	err := pack.RequireCoreEvidence()
	if err != nil {
		t.Fatalf("contextual marker follow-up should continue when semantic graph has SCIP-provenance edges, got %v", err)
	}
}

func TestMandatoryEvidenceModelMessagePreservesFullFusedCodegraphContext(t *testing.T) {
	fillerLine := "low-signal filler that should not evict fused evidence\n"
	filler := strings.Repeat(fillerLine, maxAskFusedContextBytes/len(fillerLine)+64)
	const runtimeNeedle = "DOTNET_STARTUP_HOOKS exact runtime evidence from opentelemetry-dotnet-instrumentation"
	const crdNeedle = "InstrumentationSpec exact CRD fields from api/v1alpha1/instrumentation_types.go"
	pack := mandatoryEvidencePack{Sections: []mandatoryEvidenceSection{
		{
			Layer:     "Primary fused codegraph context",
			ToolName:  "codegraph_context",
			Attempted: true,
			Satisfied: true,
			Output:    filler + runtimeNeedle + "\n" + crdNeedle,
		},
		{
			Layer:     "Direct file evidence for src/tenant.ts",
			ToolName:  "read_file",
			Attempted: true,
			Satisfied: true,
			Output:    filler + "direct-file-tail-should-be-truncated",
		},
	}}
	message := pack.ModelMessage()
	if !strings.Contains(message, runtimeNeedle) || !strings.Contains(message, crdNeedle) {
		t.Fatalf("fused codegraph context must preserve high-signal evidence beyond the generic tool cap:\n%s", message)
	}
	if strings.Contains(message, "direct-file-tail-should-be-truncated") {
		t.Fatalf("non-fused tool output should still be capped to protect model budget")
	}
}

func TestMandatoryEvidenceRejectsDirectFileBypassForArchitectureQueries(t *testing.T) {
	pack := mandatoryEvidencePack{Query: `Read src/tenant.ts and explain the architecture flow around the tenant marker.`, Sections: []mandatoryEvidenceSection{
		{Layer: "Direct file evidence for src/tenant.ts", ToolName: "read_file", Attempted: true, Satisfied: true, Output: "<content>\n1: tenant_one_secret_symbol\n</content>"},
		{Layer: "Primary fused codegraph context", ToolName: "codegraph_context", Attempted: true, Satisfied: false, Output: strings.Join([]string{
			`Codegraph context fused evidence pack for "read src/tenant.ts"`,
			"## Zoekt broad recall (`grep` ok)",
			"Found 1 matches in 1 files",
			"## Graph minimal context (`graph_minimal_context` ok)",
			"No minimal graph context found.",
			"## SCIP symbol precision (`find_symbol_definitions/find_symbol_references` ok)",
			"No code-like symbol token was detected; symbol tools remain available for follow-up.",
		}, "\n")},
	}}
	err := pack.RequireCoreEvidence()
	if err == nil || !strings.Contains(err.Error(), "direct file evidence is present") {
		t.Fatalf("architecture query must not bypass graph/SCIP requirements with one direct file, got %v", err)
	}
}

func TestPreflightSearchPatternPrefersCodeLiteralAndFileBasename(t *testing.T) {
	query := `Read src/tenant.ts and answer with the exact marker whose value starts with "tenant_".`
	pattern := preflightSearchPattern(query)
	if !strings.Contains(pattern, "tenant_") {
		t.Fatalf("preflight pattern should include code-like quoted term, got %q", pattern)
	}
	paths := preflightFilePaths(query, 4)
	if len(paths) != 1 || paths[0] != "src/tenant.ts" {
		t.Fatalf("file path extraction wrong: %+v", paths)
	}
}

func TestCodegraphContextTermsRejectStopwordsAndExpandInstrumentationFlow(t *testing.T) {
	query := "Explain the OpenTelemetry operator auto-instrumentation flow for Python, Node.js, and .NET workloads."
	symbols := preflightSymbols(query, 8)
	for _, symbol := range symbols {
		if strings.EqualFold(symbol, "the") {
			t.Fatalf("preflightSymbols must not promote stopword anchors: %+v", symbols)
		}
	}
	terms := codegraphContextExpansionTerms(query, 24)
	for _, want := range []string{"inject", "injectNodeJS", "injectPython", "injectDotNet", "sdkInjector", "InstrumentationSpec", "validateContainerEnv", "getIndexOfEnv"} {
		if !stringInSlice(terms, want) {
			t.Fatalf("expanded codegraph terms missing %q: %+v", want, terms)
		}
	}
	candidates := codegraphContextSymbolCandidates(query, terms, 5)
	for _, candidate := range candidates {
		if strings.EqualFold(candidate, "the") || strings.EqualFold(candidate, "operator") {
			t.Fatalf("symbol candidates must not include generic prose terms: %+v", candidates)
		}
	}
}

func TestCodegraphContextTermsKeepSingleRuntimeInstrumentationFocused(t *testing.T) {
	query := "Explain Python auto-instrumentation injection into pod containers."
	terms := codegraphContextExpansionTerms(query, 18)
	for _, want := range []string{"inject", "injectPython", "sdkInjector", "InstrumentationSpec"} {
		if !stringInSlice(terms, want) {
			t.Fatalf("Python-only expanded terms missing %q: %+v", want, terms)
		}
	}
	for _, unexpected := range []string{"injectNodeJS", "injectDotNet"} {
		if stringInSlice(terms, unexpected) {
			t.Fatalf("Python-only expanded terms should not include %q: %+v", unexpected, terms)
		}
	}
}

func TestCodegraphContextCompactSymbolCandidatesPrioritizeRequestedHelpers(t *testing.T) {
	candidates := []string{
		"injectNodeJS",
		"injectPython",
		"injectDotNet",
		"validateContainerEnv",
		"getIndexOfEnv",
		"appendIfNotSet",
	}
	got := codegraphContextCompactSymbolCandidates("Show helper function bodies for validateContainerEnv and getIndexOfEnv in the Python injection flow.", candidates, 3)
	for _, want := range []string{"validateContainerEnv", "getIndexOfEnv"} {
		if !containsString(got, want) {
			t.Fatalf("helper body query should prioritize %q in compact probes, got %+v", want, got)
		}
	}
	if containsString(got, "injectDotNet") {
		t.Fatalf("helper body query should not spend compact probes on unrelated language injector before helpers: %+v", got)
	}
}

func TestCodegraphContextCompactSymbolCandidatesKeepsExplicitHelpersWhenSaturated(t *testing.T) {
	candidates := []string{
		"inject",
		"injectNodeJS",
		"injectPython",
		"injectDotNet",
		"sdkInjector",
	}
	got := codegraphContextCompactSymbolCandidates("Map Python auto-instrumentation helper bodies for validateContainerEnv and getIndexOfEnv.", candidates, 3)
	for i, want := range []string{"validateContainerEnv", "getIndexOfEnv"} {
		if i >= len(got) || got[i] != want {
			t.Fatalf("explicit helper-body query should force %q before broad injectors, got %+v", want, got)
		}
	}
}

func TestCodegraphContextCompactSymbolCandidatesKeepsExplicitTestsWhenSaturated(t *testing.T) {
	candidates := []string{
		"inject",
		"injectNodeJS",
		"injectPython",
		"injectDotNet",
		"sdkInjector",
	}
	got := codegraphContextCompactSymbolCandidates("Show exact tests TestInjectPythonSDK TestInjectNodeJS and TestInjectDotNetSDK.", candidates, 3)
	for i, want := range []string{"TestInjectPythonSDK", "TestInjectNodeJS", "TestInjectDotNetSDK"} {
		if i >= len(got) || got[i] != want {
			t.Fatalf("explicit test query should force %q before broad injectors, got %+v", want, got)
		}
	}
}

func TestCodegraphContextUsesFocusedInstrumentationRecall(t *testing.T) {
	main := "main"
	start := int32(47)
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"internal/instrumentation/sdk.go"},
			"repository":"github.com/open-telemetry/opentelemetry-operator",
			"chunks":[{"content":"func (i *sdkInjector) injectPython(ctx context.Context) {}","contentStart":{"lineNumber":126},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            1051,
		OrgID:         7,
		Name:          "github.com/open-telemetry/opentelemetry-operator",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"branches":["main","feature"],"indexedRevisions":["refs/heads/main","refs/heads/feature"]}`),
	},
		graphEvidence: db.CodeGraphInspectionEvidence{
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-operator"},
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           1051,
				RepoName:         "github.com/open-telemetry/opentelemetry-operator",
				WorkspaceID:      "ws-operator",
				Relation:         "CALLS",
				SourceExternalID: "cg:o7:wotel:r1051:cmain:s1:bmain:function:podMutationWebhook.Handle",
				TargetExternalID: "cg:o7:wotel:r1051:cmain:s1:bmain:function:instPodMutator.Mutate",
				SourceFile:       "internal/webhook/podmutation/webhookhandler.go",
				StartLine:        &start,
				Confidence:       0.60,
				ConfidenceTier:   "INFERRED",
				Source:           "ast-go",
				Revision:         "refs/heads/main",
				CommitHash:       "main",
			}},
		}}
	reader := &fakeGraphReader{}
	b := NewBackend(Config{Queries: q, SearchBackend: search, GraphReader: reader})
	resp := callTool(t, b, 7, "codegraph_context", `{"query":"Explain the OpenTelemetry operator auto-instrumentation flow for Python, Node.js, and .NET workloads. Show webhook/controller entry points and test evidence.","repo":"github.com/open-telemetry/opentelemetry-operator","ref":"main","limit":5}`)
	if toolIsError(t, resp) {
		t.Fatalf("codegraph_context returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	for _, want := range []string{
		"## Zoekt focused code recall (`grep` ok)",
		"## Zoekt language implementation recall (`grep` ok)",
		"## Zoekt runtime/env recall: Node.js (`grep` ok)",
		"## Zoekt runtime/env recall: Python (`grep` ok)",
		"## Zoekt runtime/env recall: .NET (`grep` ok)",
		"## Zoekt webhook/mutator entry recall (`grep` ok)",
		"## Zoekt annotation routing recall (`grep` ok)",
		"## Zoekt test evidence recall (`grep` ok)",
		"## Critical Evidence Manifest",
		"internal/instrumentation/sdk.go",
		"## SCIP definitions for inject",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("focused codegraph context missing %q:\n%s", want, text)
		}
	}
	var sawFocusedSearch bool
	var sawImplementationSearch bool
	var sawNodeRuntimeEnvSearch bool
	var sawPythonRuntimeEnvSearch bool
	var sawDotNetRuntimeEnvSearch bool
	var sawWebhookEntrySearch bool
	var sawAnnotationSearch bool
	var sawSpecSearch bool
	var sawTestSearch bool
	for _, call := range search.calls {
		if strings.Contains(call.Query, "injectNodeJS") &&
			strings.Contains(call.Query, "injectPython") &&
			strings.Contains(call.Query, "injectDotNet") {
			sawFocusedSearch = true
		}
		if strings.Contains(call.Query, "injectPythonSDKToContainer") &&
			strings.Contains(call.Query, "injectNodeJSSDKToContainer") &&
			strings.Contains(call.Query, "injectDotNetSDKToContainer") {
			sawImplementationSearch = true
		}
		if strings.Contains(call.Query, "NODE_OPTIONS") &&
			strings.Contains(call.Query, "getDefaultNodeJSEnvVars") &&
			strings.Contains(call.Query, "file:internal/instrumentation/") {
			sawNodeRuntimeEnvSearch = true
		}
		if strings.Contains(call.Query, "PYTHONPATH") &&
			strings.Contains(call.Query, "getDefaultPythonEnvVars") &&
			strings.Contains(call.Query, "file:internal/instrumentation/") {
			sawPythonRuntimeEnvSearch = true
		}
		if strings.Contains(call.Query, "CORECLR_ENABLE_PROFILING") &&
			strings.Contains(call.Query, "DOTNET_STARTUP_HOOKS") &&
			strings.Contains(call.Query, "file:internal/instrumentation/") {
			sawDotNetRuntimeEnvSearch = true
		}
		if strings.Contains(call.Query, "NewWebhookHandler") &&
			strings.Contains(call.Query, "instPodMutator") &&
			strings.Contains(call.Query, "GetWebhookServer") {
			sawWebhookEntrySearch = true
		}
		if strings.Contains(call.Query, "annotationInjectPython") &&
			strings.Contains(call.Query, "annotationInjectNodeJS") &&
			strings.Contains(call.Query, "instrumentation.opentelemetry.io/inject-dotnet") {
			sawAnnotationSearch = true
		}
		if strings.Contains(call.Query, "type InstrumentationSpec") &&
			strings.Contains(call.Query, "type NodeJS") &&
			strings.Contains(call.Query, "type Python") &&
			strings.Contains(call.Query, "type DotNet") {
			sawSpecSearch = true
		}
		if strings.Contains(call.Query, "TestInjectPythonSDK") &&
			strings.Contains(call.Query, "TestInjectNodeJSSDK") &&
			strings.Contains(call.Query, "file:") &&
			strings.Contains(call.Query, "_test") {
			sawTestSearch = true
		}
	}
	if !sawFocusedSearch {
		t.Fatalf("expected a focused Zoekt call with language injection terms, got %+v", search.calls)
	}
	if !sawImplementationSearch {
		t.Fatalf("expected a language implementation Zoekt call with concrete helper terms, got %+v", search.calls)
	}
	if !sawNodeRuntimeEnvSearch {
		t.Fatalf("expected a Node.js runtime/env Zoekt call with exact env var terms, got %+v", search.calls)
	}
	if !sawPythonRuntimeEnvSearch {
		t.Fatalf("expected a Python runtime/env Zoekt call with exact env var terms, got %+v", search.calls)
	}
	if !sawDotNetRuntimeEnvSearch {
		t.Fatalf("expected a .NET runtime/env Zoekt call with exact env var terms, got %+v", search.calls)
	}
	if !sawWebhookEntrySearch {
		t.Fatalf("expected a webhook/mutator Zoekt call with concrete webhook entry terms, got %+v", search.calls)
	}
	if !sawAnnotationSearch {
		t.Fatalf("expected an annotation Zoekt call with concrete injection annotation terms, got %+v", search.calls)
	}
	if !sawSpecSearch {
		t.Fatalf("expected a CRD/spec Zoekt call with concrete InstrumentationSpec language terms, got %+v", search.calls)
	}
	if !sawTestSearch {
		t.Fatalf("expected a test-evidence Zoekt call constrained to *_test.go, got %+v", search.calls)
	}
	if !strings.Contains(q.lastGraph.Query, "injectNodeJS") || !strings.Contains(q.lastGraph.Query, "sdkInjector") {
		t.Fatalf("graph query should be enriched with focused code terms, got %q", q.lastGraph.Query)
	}
}

func TestCodegraphContextCompactModeKeepsRequiredEvidenceAndDropsNoise(t *testing.T) {
	main := "main"
	start := int32(11)
	longTail := strings.Repeat(" ordinary-noise", 3000) + " SHOULD_NOT_SURVIVE"
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"src/orders/createOrder.ts"},
			"repository":"github.com/acme/orders",
			"chunks":[{"content":"func createOrder(command) { return command }` + longTail + `","contentStart":{"lineNumber":12},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &main,
			IndexedAt:     ptrTime(time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		symbolRows: []db.CodeIntelOccurrenceEvidence{{
			RepoID:         42,
			RepoName:       "github.com/acme/orders",
			DisplayName:    "createOrder",
			Symbol:         "scip-typescript github.com/acme/orders src/orders/createOrder.ts/createOrder().",
			FilePath:       "src/orders/createOrder.ts",
			StartLine:      11,
			StartCharacter: 5,
			EndLine:        11,
			EndCharacter:   16,
			Role:           "DEFINITION",
			LineContent:    ptrString("func createOrder(command) { return command }"),
			Language:       ptrString("typescript"),
			Kind:           ptrString("function"),
			Revision:       "refs/heads/main",
			CommitHash:     "abc123",
		}},
		graphEvidence: db.CodeGraphInspectionEvidence{
			Query:               "createOrder",
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-1"},
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				Relation:         "CALLS",
				SourceExternalID: "route:POST /orders",
				TargetExternalID: "function:createOrder",
				SourceFile:       "src/routes/orders.ts",
				StartLine:        &start,
				Confidence:       0.91,
				ConfidenceTier:   "EXTRACTED",
				Source:           "tree-sitter-typescript",
				Revision:         "refs/heads/main",
			}},
		},
	}
	conf := 0.91
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "architecture", Direction: "bidirectional", MaxDepth: 4, EdgeKinds: []string{"CALLS"}},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "cg:o7:wabc:r42:cmain:s1:babc:route:orders",
			EdgeTargetVID:    "cg:o7:wabc:r42:cmain:s1:babc:function:createOrder",
			Depth:            1,
			Relation:         "CALLS",
			Confidence:       &conf,
			Source:           "tree-sitter-typescript",
			EvidenceFilePath: "src/routes/orders.ts",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/main",
			Start:            graphreader.Endpoint{VID: "cg:o7:wabc:r42:cmain:s1:babc:route:orders", RepoID: ptrInt32(42), Kind: "route", Label: "POST /orders", Path: "src/routes/orders.ts"},
			Neighbor:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:cmain:s1:babc:function:createOrder", RepoID: ptrInt32(42), Kind: "function", Label: "createOrder", Path: "src/orders/createOrder.ts"},
		}},
	}}
	b := NewBackend(Config{Queries: q, SearchBackend: search, GraphReader: reader})
	resp := callTool(t, b, 7, "codegraph_context", `{"query":"createOrder","repo":"github.com/acme/orders","ref":"main","limit":5,"compact":true}`)
	if toolIsError(t, resp) {
		t.Fatalf("codegraph_context compact returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	for _, want := range []string{
		"## Critical Evidence Manifest",
		"## Zoekt broad recall (`grep` ok)",
		"## Graph minimal context (`graph_minimal_context` ok)",
		"Shared NebulaGraph read: yes",
		"## SCIP definitions for createOrder (`find_symbol_definitions` ok)",
		"## SCIP references for createOrder (`find_symbol_references` ok)",
		"source=tree-sitter-typescript",
		"Found 1 precise SCIP",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("compact output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "SHOULD_NOT_SURVIVE") || len(text) > 30000 {
		t.Fatalf("compact output was not compact enough, len=%d:\n%s", len(text), text)
	}
	reader.mu.Lock()
	graphCalls := len(reader.calls)
	reader.mu.Unlock()
	if graphCalls != 1 {
		t.Fatalf("codegraph_context should share one graph DB read across minimal/path sections, got %d calls", graphCalls)
	}
}

func TestCodegraphContextSkipsUnindexedReposWithoutFailingIndexedScope(t *testing.T) {
	main := "main"
	indexedAt := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	start := int32(21)
	q := &fakeQuerier{
		readRepos: map[string]db.RepoReadRow{
			"github.com/acme/api": {
				ID:            42,
				OrgID:         7,
				Name:          "github.com/acme/api",
				DefaultBranch: &main,
				IndexedAt:     &indexedAt,
				Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
			},
			"github.com/acme/dotnet-sdk": {
				ID:            43,
				OrgID:         7,
				Name:          "github.com/acme/dotnet-sdk",
				DefaultBranch: &main,
				Metadata:      []byte(`{"branches":["main"]}`),
			},
		},
		graphEvidence: db.CodeGraphInspectionEvidence{
			Query:               "checkout flow",
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-api"},
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/api",
				Relation:         "CALLS",
				SourceExternalID: "route:POST /checkout",
				TargetExternalID: "function:createOrder",
				SourceFile:       "src/routes/checkout.ts",
				StartLine:        &start,
				Confidence:       0.91,
				ConfidenceTier:   "EXTRACTED",
				Source:           "tree-sitter-typescript",
				Revision:         "refs/heads/main",
			}},
		},
	}
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"src/routes/checkout.ts"},
			"repository":"github.com/acme/api",
			"chunks":[{"content":"router.post('/checkout', createOrder)","contentStart":{"lineNumber":21},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "architecture", Direction: "bidirectional", MaxDepth: 2, EdgeKinds: []string{"CALLS"}},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "cg:o7:wabc:r42:cmain:s1:babc:route:checkout",
			EdgeTargetVID:    "cg:o7:wabc:r42:cmain:s1:babc:function:createOrder",
			Depth:            1,
			Relation:         "CALLS",
			Source:           "tree-sitter-typescript",
			EvidenceFilePath: "src/routes/checkout.ts",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/main",
			Start:            graphreader.Endpoint{VID: "cg:o7:wabc:r42:cmain:s1:babc:route:checkout", RepoID: ptrInt32(42), Kind: "route", Label: "POST /checkout", Path: "src/routes/checkout.ts"},
			Neighbor:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:cmain:s1:babc:function:createOrder", RepoID: ptrInt32(42), Kind: "function", Label: "createOrder", Path: "src/orders/createOrder.ts"},
		}},
	}}
	b := NewBackend(Config{Queries: q, SearchBackend: search, GraphReader: reader})
	resp := callTool(t, b, 7, "codegraph_context", `{"query":"checkout flow","repos":["github.com/acme/api","github.com/acme/dotnet-sdk"],"limit":5,"compact":true}`)
	if toolIsError(t, resp) {
		t.Fatalf("codegraph_context should continue over indexed repos and report skipped repos: %s", resp.Body)
	}
	text := toolText(t, resp)
	for _, want := range []string{
		"Skipped unindexed repositories: 1",
		"## Repository index coverage (`scope_preflight` ok)",
		"github.com/acme/dotnet-sdk: invalid git reference",
		"Repositories: github.com/acme/api",
		"## Zoekt broad recall (`grep` ok)",
		"source=tree-sitter-typescript",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("partial repo output missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(search.last.Query, "dotnet-sdk") {
		t.Fatalf("unindexed repo must not be passed to Zoekt query: %s", search.last.Query)
	}
	if len(q.lastGraph.RepoRevisionScopes) != 1 || q.lastGraph.RepoRevisionScopes[0].Repo != "github.com/acme/api" {
		t.Fatalf("graph scope must only include indexed repo, got %+v", q.lastGraph.RepoRevisionScopes)
	}
	if reader.last.MaxSeedTokens != 5 || reader.last.SeedRowLimit != 8 || reader.last.SeedVIDLimit != 24 || reader.last.TraversalRows != 48 {
		t.Fatalf("compact graphreader budget = %+v, want seedTokens=5 seedRows=8 seedVIDs=24 traversalRows=48", reader.last)
	}
}

func TestCodegraphContextBoundsGraphTraversalBudget(t *testing.T) {
	if got := codegraphContextGraphTraversalLimit(80, false); got != 25 {
		t.Fatalf("full graph traversal limit = %d, want 25", got)
	}
	if got := codegraphContextGraphTraversalLimit(80, true); got != 16 {
		t.Fatalf("compact graph traversal limit = %d, want 16", got)
	}
	if got := codegraphContextGraphTraversalDepth("path", true); got != 1 {
		t.Fatalf("compact graph traversal depth = %d, want 1", got)
	}
	if got := codegraphContextGraphTraversalDepth("minimal", false); got != 2 {
		t.Fatalf("minimal graph traversal depth = %d, want 2", got)
	}
	if got := codegraphContextGraphTraversalDepth("path", false); got != 3 {
		t.Fatalf("path graph traversal depth = %d, want 3", got)
	}
}

func TestInspectCodeGraphCompactCacheBypassesEvidenceFanoutOnRepeat(t *testing.T) {
	start := int32(12)
	scope := db.CodeGraphActiveScope{
		GraphIndexID:   "graph-1",
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "abc123",
		WorkspaceID:    "ws-1",
		SchemaVersion:  1,
		BuilderVersion: "builder-v1",
	}
	mainBranch := "main"
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &mainBranch,
			IndexedAt:     ptrTime(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		activeScopes: []db.CodeGraphActiveScope{scope},
		graphEvidence: db.CodeGraphInspectionEvidence{
			OrgID:        7,
			Query:        "checkout flow",
			WorkspaceIDs: []string{"ws-1"},
			ActiveScopes: []db.CodeGraphActiveScope{scope},
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				WorkspaceID:      "ws-1",
				SourceExternalID: "cg:o7:wabc:r42:cabc:s1:bbuilder:symbol:createOrder",
				TargetExternalID: "cg:o7:wabc:r42:cabc:s1:bbuilder:symbol:saveOrder",
				Relation:         "CALLS",
				SourceFile:       "src/orders/createOrder.ts",
				StartLine:        &start,
				Confidence:       0.95,
				ConfidenceTier:   "EXTRACTED",
				Source:           "scip",
				Revision:         "refs/heads/main",
				CommitHash:       "abc123",
				GraphIndexID:     "graph-1",
			}},
		},
	}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "architecture"},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "cg:o7:wabc:r42:cabc:s1:bbuilder:symbol:createOrder",
			EdgeTargetVID:    "cg:o7:wabc:r42:cabc:s1:bbuilder:symbol:saveOrder",
			Relation:         "CALLS",
			Source:           "scip",
			Provenance:       "scip",
			EvidenceFilePath: "src/orders/createOrder.ts",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/main",
			EdgeCommitHash:   "abc123",
			Start:            graphreader.Endpoint{VID: "cg:o7:wabc:r42:cabc:s1:bbuilder:symbol:createOrder", RepoID: ptrInt32(42), Kind: "symbol", Label: "createOrder"},
			Neighbor:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:cabc:s1:bbuilder:symbol:saveOrder", RepoID: ptrInt32(42), Kind: "symbol", Label: "saveOrder"},
		}},
	}}
	cache := &fakeGraphEvidenceCache{}
	b := NewBackend(Config{
		Queries:            q,
		GraphReader:        reader,
		GraphEvidenceCache: cache,
		GraphEvidenceTTL:   time.Minute,
	})
	body := `{"query":"checkout flow","repo":"github.com/acme/orders","ref":"main","limit":5,"compact":true}`
	first := callTool(t, b, 7, "inspect_code_graph", body)
	if toolIsError(t, first) {
		t.Fatalf("first inspect_code_graph returned error: %s", first.Body)
	}
	if q.graphCalls != 1 || len(reader.calls) != 1 || cache.sets != 1 {
		t.Fatalf("first call counts graphCalls=%d reader=%d cacheSets=%d", q.graphCalls, len(reader.calls), cache.sets)
	}
	if !q.lastGraph.Compact {
		t.Fatalf("compact inspect_code_graph should request compact DB evidence")
	}
	second := callTool(t, b, 7, "inspect_code_graph", body)
	if toolIsError(t, second) {
		t.Fatalf("second inspect_code_graph returned error: %s", second.Body)
	}
	if q.graphCalls != 1 {
		t.Fatalf("cache hit should not call InspectOrgCodeGraph again, got %d", q.graphCalls)
	}
	if len(reader.calls) != 1 {
		t.Fatalf("cache hit should not call graphreader again, got %d", len(reader.calls))
	}
	if cache.gets < 2 {
		t.Fatalf("expected cache to be checked on both calls, gets=%d", cache.gets)
	}
	if !strings.Contains(toolText(t, second), "createOrder") {
		t.Fatalf("cached response lost graph output:\n%s", toolText(t, second))
	}
}

func TestInspectCodeGraphCompactTimeoutKeepsSemanticEvidence(t *testing.T) {
	start := int32(12)
	mainBranch := "main"
	scope := db.CodeGraphActiveScope{
		GraphIndexID:   "graph-1",
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "abc123",
		WorkspaceID:    "ws-1",
		SchemaVersion:  1,
		BuilderVersion: "builder-v1",
	}
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &mainBranch,
			IndexedAt:     ptrTime(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		graphEvidence: db.CodeGraphInspectionEvidence{
			OrgID:        7,
			Query:        "checkout flow",
			WorkspaceIDs: []string{"ws-1"},
			ActiveScopes: []db.CodeGraphActiveScope{scope},
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				WorkspaceID:      "ws-1",
				SourceExternalID: "symbol:createOrder",
				TargetExternalID: "symbol:saveOrder",
				Relation:         "CALLS",
				SourceFile:       "src/orders/createOrder.ts",
				StartLine:        &start,
				Confidence:       0.95,
				ConfidenceTier:   "EXTRACTED",
				Source:           "scip",
				Revision:         "refs/heads/main",
				CommitHash:       "abc123",
				GraphIndexID:     "graph-1",
			}},
		},
	}
	reader := &fakeGraphReader{err: context.DeadlineExceeded}
	b := NewBackend(Config{Queries: q, GraphReader: reader})
	resp := callTool(t, b, 7, "inspect_code_graph", `{"query":"checkout flow","repo":"github.com/acme/orders","ref":"main","limit":5,"compact":true}`)
	if toolIsError(t, resp) {
		t.Fatalf("compact inspect_code_graph timeout should return semantic evidence, got error: %s", resp.Body)
	}
	text := toolText(t, resp)
	for _, want := range []string{"CALLS", "createOrder", "saveOrder", "NebulaGraph traversal timeout"} {
		if !strings.Contains(text, want) {
			t.Fatalf("timeout response missing %q:\n%s", want, text)
		}
	}
}

func TestInspectCodeGraphCompactNoNativeEdgesKeepsSemanticEvidence(t *testing.T) {
	start := int32(1)
	mainBranch := "release-a"
	scope := db.CodeGraphActiveScope{
		GraphIndexID:   "graph-tenant",
		RepoID:         42,
		Revision:       "refs/heads/release-a",
		CommitHash:     "abc123",
		WorkspaceID:    "ws-tenant",
		SchemaVersion:  1,
		BuilderVersion: "builder-v1",
	}
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "git-fixtures.lifecycle.local/orga/repo",
			DefaultBranch: &mainBranch,
			IndexedAt:     ptrTime(time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/release-a"]}`),
		},
		graphEvidence: db.CodeGraphInspectionEvidence{
			OrgID:        7,
			Query:        "tenant_one_secret_symbol",
			WorkspaceIDs: []string{"ws-tenant"},
			ActiveScopes: []db.CodeGraphActiveScope{scope},
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           42,
				RepoName:         "git-fixtures.lifecycle.local/orga/repo",
				WorkspaceID:      "ws-tenant",
				SourceExternalID: "cg:o7:wtenant:r42:crelease:s1:bs:site:file:tenant",
				TargetExternalID: "cg:o7:wtenant:r42:crelease:s1:bs:symbol:tenantMarker",
				Relation:         "DEFINES",
				SourceFile:       "src/tenant.ts",
				StartLine:        &start,
				Confidence:       0.95,
				ConfidenceTier:   "EXTRACTED",
				Source:           "ast-typescript",
				Revision:         "refs/heads/release-a",
				CommitHash:       "abc123",
				GraphIndexID:     "graph-tenant",
			}},
		},
	}
	reader := &fakeGraphReader{err: fmt.Errorf("NebulaGraph traversal returned no native edges for the matched graph seeds")}
	b := NewBackend(Config{Queries: q, GraphReader: reader})
	resp := callTool(t, b, 7, "inspect_code_graph", `{"query":"tenant_one_secret_symbol","repo":"git-fixtures.lifecycle.local/orga/repo","ref":"release-a","limit":5,"compact":true}`)
	if toolIsError(t, resp) {
		t.Fatalf("compact inspect_code_graph no-native-edge fallback should return semantic evidence, got error: %s", resp.Body)
	}
	text := toolText(t, resp)
	for _, want := range []string{"Semantic architecture facts", "source=ast-typescript", "src/tenant.ts", "NebulaGraph traversal degraded"} {
		if !strings.Contains(text, want) {
			t.Fatalf("degraded no-native response missing %q:\n%s", want, text)
		}
	}
}

func TestCodegraphContextPrunesGenericInjectWhenSpecificSymbolsExist(t *testing.T) {
	got := codegraphContextPruneBroadSymbols([]string{"inject", "injectNodeJS", "injectPython", "sdkInjector"})
	if containsString(got, "inject") {
		t.Fatalf("generic inject should be removed when specific inject symbols exist: %+v", got)
	}
	for _, want := range []string{"injectNodeJS", "injectPython", "sdkInjector"} {
		if !containsString(got, want) {
			t.Fatalf("specific symbol %q missing after prune: %+v", want, got)
		}
	}
	if solo := codegraphContextPruneBroadSymbols([]string{"inject"}); !containsString(solo, "inject") {
		t.Fatalf("generic inject should remain when it is the only symbol: %+v", solo)
	}
}

func TestFormatSemanticEdgeDisplayUsesEvidenceInsteadOfVertexIDs(t *testing.T) {
	row := db.CodeGraphSemanticEdgeEvidence{
		SourceExternalID: "cg:o7:wabc:r42:cmain:s1:babc:symbol:sourcehash",
		TargetExternalID: "cg:o7:wabc:r42:cmain:s1:babc:symbol:targethash",
		Relation:         "CALLS",
		Evidence:         ptrString("scip-go gomod github.com/acme/orders . `github.com/acme/orders/internal/sdk`/injectPython(). CALLS scip-go gomod github.com/acme/orders . `github.com/acme/orders/internal/python`/injectPythonSDK()."),
	}
	got := formatSemanticEdgeDisplay(row)
	if strings.Contains(got, "cg:o7") {
		t.Fatalf("semantic edge display leaked raw graph VID: %s", got)
	}
	for _, want := range []string{"injectPython", "CALLS", "injectPythonSDK"} {
		if !strings.Contains(got, want) {
			t.Fatalf("semantic edge display missing %q: %s", want, got)
		}
	}
}

func TestCodegraphContextSelectsRankedSourceSlices(t *testing.T) {
	output := `Found 4 matches in 2 files

[github.com/open-telemetry/opentelemetry-operator] internal/instrumentation/podmutator.go:
  210: func (pm *instPodMutator) Mutate(ctx context.Context, ns corev1.Namespace, pod corev1.Pod) (corev1.Pod, error) {
  362: func (pm *instPodMutator) getInstrumentationInstance(ctx context.Context, ns corev1.Namespace, pod corev1.Pod, instAnnotation string) (*v1alpha1.Instrumentation, error) {

[github.com/open-telemetry/opentelemetry-operator] internal/instrumentation/sdk_test.go:
  1018: func TestInjectNodeJS(t *testing.T) {`
	candidates := codegraphContextSourceSliceCandidates(output, "webhook")
	selected := codegraphContextSelectSourceSlices("show test evidence for the webhook flow", candidates, 3)
	if len(selected) != 3 {
		t.Fatalf("expected three source slices, got %+v", selected)
	}
	if selected[0].Path != "internal/instrumentation/podmutator.go" || selected[0].Line != 210 {
		t.Fatalf("expected first podmutator slice, got %+v", selected)
	}
	if selected[1].Path != "internal/instrumentation/podmutator.go" || selected[1].Line != 362 {
		t.Fatalf("expected second podmutator slice from separate line bucket, got %+v", selected)
	}
	if selected[2].Path != "internal/instrumentation/sdk_test.go" {
		t.Fatalf("expected test slice to remain when tests are requested, got %+v", selected)
	}
}

func TestCodegraphContextMarksRequiredLayerFailureAsToolError(t *testing.T) {
	main := "main"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            1051,
		OrgID:         7,
		Name:          "github.com/open-telemetry/opentelemetry-operator",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
	}}
	b := NewBackend(Config{
		Queries:       q,
		SearchBackend: &fakeSearchBackend{err: errors.New("zoekt unavailable")},
		GraphReader:   &fakeGraphReader{},
	})
	resp := callTool(t, b, 7, "codegraph_context", `{"query":"inject Python flow","repo":"github.com/open-telemetry/opentelemetry-operator","ref":"main","limit":5}`)
	if !toolIsError(t, resp) {
		t.Fatalf("required grep layer failure must mark codegraph_context as tool error: %s", resp.Body)
	}
}

func TestMandatoryEvidenceAcceptsSCIPBackedSemanticGraphEvidence(t *testing.T) {
	pack := mandatoryEvidencePack{Sections: []mandatoryEvidenceSection{
		{Layer: "Zoekt broad recall", ToolName: "grep", Attempted: true, Satisfied: true, Output: "collector/exporter.go"},
		{Layer: "Postgres graph metadata + Nebula traversal", ToolName: "inspect_code_graph", Attempted: true, Satisfied: true, Output: "Native graph traversal (ranked NebulaGraph BFS neighborhood):\n- source=scip"},
		{Layer: "SCIP symbol definitions", ToolName: "find_symbol_definitions", Attempted: true, Satisfied: true, Output: "Found 1 precise SCIP definitions"},
		{Layer: "SCIP symbol references", ToolName: "find_symbol_references", Attempted: true, Satisfied: true, Output: "Found 1 precise SCIP references"},
	}}
	err := pack.RequireCoreEvidence()
	if err != nil {
		t.Fatalf("SCIP-backed graph evidence should satisfy the semantic graph gate, got %v", err)
	}
}

func TestMandatoryEvidenceRejectsErroredCodegraphContextLayer(t *testing.T) {
	pack := mandatoryEvidencePack{Sections: []mandatoryEvidenceSection{{
		Layer:     "Primary fused codegraph context",
		ToolName:  "codegraph_context",
		Attempted: true,
		Satisfied: true,
		Output: strings.Join([]string{
			`Codegraph context fused evidence pack for "createOrder"`,
			"## Zoekt broad recall (`grep` ok)",
			"Found 1 matches in 1 files",
			"## Graph minimal context (`graph_minimal_context` error)",
			"NebulaGraph traversal failed: timeout",
			"## SCIP definitions for createOrder (`find_symbol_definitions` ok)",
			"Found 1 precise SCIP definition",
			"## SCIP references for createOrder (`find_symbol_references` ok)",
			"Found 1 precise SCIP reference",
			"AST/tree-sitter facts:",
			"source=tree-sitter-typescript",
		}, "\n"),
	}}}
	err := pack.RequireCoreEvidence()
	if err == nil || !strings.Contains(err.Error(), "Nebula traversal returned no usable native graph evidence") {
		t.Fatalf("errored graph section inside codegraph_context must block synthesis, got %v", err)
	}
}

func TestNormalizeAskFinalAnswerTrimsModelPreamble(t *testing.T) {
	got := normalizeAskFinalAnswer("I have enough evidence now.\n\n<!--answer-->\n# Real answer")
	if got != "<!--answer-->\n# Real answer" {
		t.Fatalf("normalizeAskFinalAnswer kept preamble:\n%s", got)
	}
	got = normalizeAskFinalAnswer("# Missing tag")
	if got != "<!--answer-->\n# Missing tag" {
		t.Fatalf("normalizeAskFinalAnswer did not add tag:\n%s", got)
	}
}

func TestListReposScopesOrgAndReturnsJsonText(t *testing.T) {
	now := time.Date(2026, 5, 25, 10, 11, 12, 0, time.UTC)
	web := "https://example.test/repo"
	main := "main"
	q := &fakeQuerier{
		total: 1,
		repos: []db.RepoListRow{{
			RepoID:        42,
			RepoName:      "github.com/acme/api",
			WebUrl:        &web,
			PushedAt:      &now,
			DefaultBranch: &main,
		}},
	}
	b := NewBackend(Config{Queries: q})
	resp := callTool(t, b, 99, "list_repos", `{"page":1,"perPage":30}`)

	if q.lastOrgID != 99 {
		t.Fatalf("org scope: got %d want 99", q.lastOrgID)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, `"github.com/acme/api"`) || !strings.Contains(text, `"totalCount":1`) {
		t.Fatalf("list_repos text wrong: %s", text)
	}
}

func TestListReposEmitsNullForBlankHeadlessURL(t *testing.T) {
	blank := ""
	q := &fakeQuerier{
		total: 1,
		repos: []db.RepoListRow{{
			RepoID:   42,
			RepoName: "git.example.local/org/repo",
			WebUrl:   &blank,
		}},
	}
	b := NewBackend(Config{Queries: q})
	resp := callTool(t, b, 99, "list_repos", `{}`)

	text := toolText(t, resp)
	if !strings.Contains(text, `"url":null`) {
		t.Fatalf("blank headless repo URL should serialize as null: %s", text)
	}
}

func TestGrepUsesOrgScopedSearchAndFormatsMatches(t *testing.T) {
	main := "main"
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"collector/exporter.go"},
			"repository":"github.com/acme/api",
			"chunks":[{"content":"func ExportTraceServiceRequest() {}","contentStart":{"lineNumber":12},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "github.com/acme/api",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
	}}
	b := NewBackend(Config{Queries: q, SearchBackend: search})
	resp := callTool(t, b, 7, "grep", `{"pattern":"ExportTraceServiceRequest","repo":"github.com/acme/api","ref":"main","limit":5}`)

	if search.last.OrgID != 7 {
		t.Fatalf("search org scope: got %d want 7", search.last.OrgID)
	}
	if !strings.Contains(search.last.Query, `repo:github\.com/acme/api`) || !strings.Contains(search.last.Query, `branch:main`) {
		t.Fatalf("search query not scoped as expected: %s", search.last.Query)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "collector/exporter.go") || !strings.Contains(text, "12: func ExportTraceServiceRequest()") {
		t.Fatalf("grep output wrong: %s", text)
	}
}

func TestGrepWithoutRepoUsesOrgReposDefaultIndexedRefs(t *testing.T) {
	indexedAt := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	q := &fakeQuerier{
		repos: []db.RepoListRow{{
			RepoID:    42,
			RepoName:  "github.com/acme/api",
			IndexedAt: &indexedAt,
			Metadata:  []byte(`{"indexedRevisions":["refs/heads/main","refs/heads/feature"]}`),
		}},
		readRepo: db.RepoReadRow{
			ID:        42,
			OrgID:     7,
			Name:      "github.com/acme/api",
			IndexedAt: &indexedAt,
			Metadata:  []byte(`{"indexedRevisions":["refs/heads/main","refs/heads/feature"]}`),
		},
	}
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"README.md"},
			"repository":"github.com/acme/api",
			"chunks":[{"content":"tenant_one_secret_symbol","contentStart":{"lineNumber":1},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	b := NewBackend(Config{Queries: q, SearchBackend: search})
	resp := callTool(t, b, 7, "grep", `{"pattern":"tenant_one_secret_symbol","limit":5}`)

	if toolIsError(t, resp) {
		t.Fatalf("grep without repo should use default indexed refs: %s", resp.Body)
	}
	if !strings.Contains(search.last.Query, `repo:github\.com/acme/api`) || !strings.Contains(search.last.Query, `branch:main`) {
		t.Fatalf("grep without repo should not scan all branch shards, query=%q", search.last.Query)
	}
}

func TestGrepReposWithExplicitHEADUsesPerRepoDefaultIndexedRefs(t *testing.T) {
	indexedAt := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	main := "main"
	release := "release"
	q := &fakeQuerier{
		readRepos: map[string]db.RepoReadRow{
			"github.com/acme/api": {
				ID:            42,
				OrgID:         7,
				Name:          "github.com/acme/api",
				DefaultBranch: &main,
				IndexedAt:     &indexedAt,
				Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
			},
			"github.com/acme/worker": {
				ID:            43,
				OrgID:         7,
				Name:          "github.com/acme/worker",
				DefaultBranch: &release,
				IndexedAt:     &indexedAt,
				Metadata:      []byte(`{"indexedRevisions":["refs/heads/release"]}`),
			},
		},
	}
	search := &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)}
	b := NewBackend(Config{Queries: q, SearchBackend: search})
	resp := callTool(t, b, 7, "grep", `{"pattern":"handler","repos":["github.com/acme/api","github.com/acme/worker"],"ref":"HEAD","limit":5}`)
	if toolIsError(t, resp) {
		t.Fatalf("grep repos HEAD should use per-repo defaults: %s", resp.Body)
	}
	if len(search.calls) != 2 {
		t.Fatalf("grep repos HEAD should fan out per repo, calls=%d", len(search.calls))
	}
	if !strings.Contains(search.calls[0].Query, `repo:github\.com/acme/api`) || !strings.Contains(search.calls[0].Query, `branch:main`) {
		t.Fatalf("first repo query did not use api default branch: %s", search.calls[0].Query)
	}
	if !strings.Contains(search.calls[1].Query, `repo:github\.com/acme/worker`) || !strings.Contains(search.calls[1].Query, `branch:release`) {
		t.Fatalf("second repo query did not use worker default branch: %s", search.calls[1].Query)
	}
	if strings.Contains(search.calls[1].Query, `branch:main`) {
		t.Fatalf("explicit HEAD must not reuse first repo default across repos: %s", search.calls[1].Query)
	}
}

func TestGrepReposWithSameDefaultRefUsesOneReposetQuery(t *testing.T) {
	indexedAt := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	main := "main"
	q := &fakeQuerier{
		readRepos: map[string]db.RepoReadRow{
			"github.com/acme/api": {
				ID:            42,
				OrgID:         7,
				Name:          "github.com/acme/api",
				DefaultBranch: &main,
				IndexedAt:     &indexedAt,
				Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
			},
			"github.com/acme/worker": {
				ID:            43,
				OrgID:         7,
				Name:          "github.com/acme/worker",
				DefaultBranch: &main,
				IndexedAt:     &indexedAt,
				Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
			},
		},
	}
	search := &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)}
	b := NewBackend(Config{Queries: q, SearchBackend: search})
	resp := callTool(t, b, 7, "grep", `{"pattern":"handler","repos":["github.com/acme/api","github.com/acme/worker"],"ref":"HEAD","limit":5}`)
	if toolIsError(t, resp) {
		t.Fatalf("grep repos HEAD should group same default refs: %s", resp.Body)
	}
	if len(search.calls) != 1 {
		t.Fatalf("same default ref should use one reposet query, calls=%d", len(search.calls))
	}
	query := search.calls[0].Query
	for _, want := range []string{`reposet:github.com/acme/api,github.com/acme/worker`, `branch:main`} {
		if !strings.Contains(query, want) {
			t.Fatalf("grouped query missing %q: %s", want, query)
		}
	}
}

func TestCodegraphContextScopesScipRevisionWithRef(t *testing.T) {
	main := "main"
	start := int32(8)
	q := &fakeQuerier{
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: &main,
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"branches":["main","feature"],"indexedRevisions":["refs/heads/main","refs/heads/feature"]}`),
		},
		symbolRows: []db.CodeIntelOccurrenceEvidence{{
			RepoID:         42,
			RepoName:       "github.com/acme/orders",
			DisplayName:    "createOrder",
			Symbol:         "scip-typescript github.com/acme/orders src/orders/createOrder.ts/createOrder().",
			FilePath:       "src/orders/createOrder.ts",
			StartLine:      8,
			StartCharacter: 23,
			EndLine:        8,
			EndCharacter:   34,
			Role:           "DEFINITION",
			LineContent:    ptrString("export async function createOrder(command) {"),
			Language:       ptrString("typescript"),
			Kind:           ptrString("function"),
			Revision:       "refs/heads/feature",
			CommitHash:     "feature123",
		}},
		graphEvidence: db.CodeGraphInspectionEvidence{
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-1"},
			Anchors: []db.CodeGraphAnchorEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				Kind:             "function",
				Direction:        "PROVIDES",
				Key:              "createOrder",
				NormalizedKey:    "createorder",
				NodeVID:          "cg:o7:wabc:r42:cfeature:s1:babc:function:createOrder",
				WorkspaceID:      "ws-1",
				EvidenceFilePath: ptrString("src/orders/createOrder.ts"),
				StartLine:        &start,
				Confidence:       0.95,
				Source:           "tree-sitter-typescript",
				Revision:         "refs/heads/feature",
			}},
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				Relation:         "CALLS",
				SourceExternalID: "route:POST /orders",
				TargetExternalID: "function:createOrder",
				SourceFile:       "src/routes/orders.ts",
				StartLine:        &start,
				Confidence:       0.91,
				ConfidenceTier:   "EXTRACTED",
				Source:           "tree-sitter-typescript",
				Revision:         "refs/heads/feature",
			}},
		},
	}
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"src/orders/createOrder.ts"},
			"repository":"github.com/acme/orders",
			"chunks":[{"content":"export async function createOrder(command) {}","contentStart":{"lineNumber":8},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "architecture", Direction: "bidirectional", MaxDepth: 4, EdgeKinds: []string{"CALLS"}},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "cg:o7:wabc:r42:cfeature:s1:babc:route:orders",
			EdgeTargetVID:    "cg:o7:wabc:r42:cfeature:s1:babc:function:createOrder",
			Depth:            1,
			Relation:         "CALLS",
			Source:           "tree-sitter-typescript",
			EvidenceFilePath: "src/routes/orders.ts",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/feature",
			Start:            graphreader.Endpoint{VID: "cg:o7:wabc:r42:cfeature:s1:babc:route:orders", RepoID: ptrInt32(42), Kind: "route", Label: "POST /orders", Path: "src/routes/orders.ts"},
			Neighbor:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:cfeature:s1:babc:function:createOrder", RepoID: ptrInt32(42), Kind: "function", Label: "createOrder", Path: "src/orders/createOrder.ts"},
		}},
	}}
	b := NewBackend(Config{Queries: q, SearchBackend: search, GraphReader: reader})
	resp := callTool(t, b, 7, "codegraph_context", `{"query":"createOrder","repo":"github.com/acme/orders","ref":"feature","limit":5}`)
	if toolIsError(t, resp) {
		t.Fatalf("codegraph_context returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	if len(q.lastSymbol.RepoRevisionScopes) != 1 || strings.Join(q.lastSymbol.RepoRevisionScopes[0].RevisionCandidates, ",") != "refs/heads/feature" {
		t.Fatalf("SCIP symbol scope must use codegraph_context ref as revision, got %+v\n%s", q.lastSymbol.RepoRevisionScopes, text)
	}
	if len(q.lastGraph.RepoRevisionScopes) != 1 || strings.Join(q.lastGraph.RepoRevisionScopes[0].RevisionCandidates, ",") != "refs/heads/feature" {
		t.Fatalf("graph scope must use requested ref, got %+v", q.lastGraph.RepoRevisionScopes)
	}
	if !strings.Contains(search.last.Query, `branch:feature`) {
		t.Fatalf("supplemental Zoekt query must use requested ref, got %q", search.last.Query)
	}
	for _, want := range []string{"Requested ref: feature", "## Zoekt broad recall (`grep` ok)", "## SCIP definitions for createOrder (`find_symbol_definitions` ok)", "tree-sitter-typescript"} {
		if !strings.Contains(text, want) {
			t.Fatalf("codegraph_context output missing %q:\n%s", want, text)
		}
	}
}

func TestGrepRejectsRefQueryInjection(t *testing.T) {
	search := &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)}
	b := NewBackend(Config{SearchBackend: search})
	resp := callTool(t, b, 7, "grep", `{"pattern":"handler","ref":"main repo:other"}`)
	if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), "grep ref requires repo or repos") {
		t.Fatalf("ref injection should be rejected: %s", resp.Body)
	}
	if search.last.Query != "" {
		t.Fatalf("search backend must not be called on invalid ref, got query %q", search.last.Query)
	}
}

func TestGrepDefaultsSingleRepoToDefaultIndexedBranch(t *testing.T) {
	main := "trunk"
	search := &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)}
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "github.com/acme/api",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"indexedRevisions":["refs/heads/trunk"]}`),
	}}
	b := NewBackend(Config{Queries: q, SearchBackend: search})
	resp := callTool(t, b, 7, "grep", `{"pattern":"handler","repo":"github.com/acme/api"}`)
	if toolIsError(t, resp) {
		t.Fatalf("grep should use default indexed branch: %s", resp.Body)
	}
	if !strings.Contains(search.last.Query, `branch:trunk`) {
		t.Fatalf("grep did not pin default indexed branch: %s", search.last.Query)
	}
}

func TestReadFileUsesOrgScopedRepoPathAndRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	repoDir := makeGitRepo(t, root)
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:           42,
		OrgID:        7,
		Name:         "local/repo",
		CloneURL:     "file://" + repoDir,
		CodeHostType: "genericGitHost",
		IndexedAt:    ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "read_file", `{"repo":"local/repo","path":"README.md","offset":1,"limit":5}`)
	if q.lastOrgID != 7 || q.lastRepo != "local/repo" {
		t.Fatalf("repo read scope wrong: org=%d repo=%q", q.lastOrgID, q.lastRepo)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "<repo>local/repo</repo>") || !strings.Contains(text, "1: # Hello") {
		t.Fatalf("read_file output wrong: %s", text)
	}

	errResp := callTool(t, b, 7, "read_file", `{"repo":"local/repo","path":"../secret"}`)
	if !toolIsError(t, errResp) || !strings.Contains(toolText(t, errResp), "invalid file path") {
		t.Fatalf("path traversal should be rejected: %s", string(errResp.Body))
	}
}

func TestReadFileRejectsFileRepoOutsideManagedStorage(t *testing.T) {
	managedRoot := t.TempDir()
	repoDir := makeGitRepo(t, t.TempDir())
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:           42,
		OrgID:        7,
		Name:         "local/repo",
		CloneURL:     "file://" + repoDir,
		CodeHostType: "genericGitHost",
		IndexedAt:    ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: managedRoot}})
	resp := callTool(t, b, 7, "read_file", `{"repo":"local/repo","path":"README.md"}`)
	if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), "repository path is outside managed storage") {
		t.Fatalf("outside managed storage should be rejected: %s", resp.Body)
	}
}

func TestReadFileRejectsUnindexedRef(t *testing.T) {
	root := t.TempDir()
	repoDir := makeGitRepo(t, root)
	main := "main"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})
	resp := callTool(t, b, 7, "read_file", `{"repo":"local/repo","path":"README.md","ref":"feature"}`)
	if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), "ref is not indexed") {
		t.Fatalf("unindexed ref should be rejected: %s", resp.Body)
	}
}

func TestReadFileRejectsWhileRemoveIndexPendingInProgressOrFailed(t *testing.T) {
	for _, status := range []string{"PENDING", "IN_PROGRESS", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			root := t.TempDir()
			repoDir := makeGitRepo(t, root)
			main := "main"
			jobType := "REMOVE_INDEX"
			jobStatus := status
			q := &fakeQuerier{readRepo: db.RepoReadRow{
				ID:              42,
				OrgID:           7,
				Name:            "local/repo",
				CloneURL:        "file://" + repoDir,
				CodeHostType:    "genericGitHost",
				DefaultBranch:   &main,
				IndexedAt:       ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
				Metadata:        []byte(`{"indexedRevisions":["refs/heads/main"]}`),
				LatestJobType:   &jobType,
				LatestJobStatus: &jobStatus,
			}}
			b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})
			resp := callTool(t, b, 7, "read_file", `{"repo":"local/repo","path":"README.md","ref":"HEAD"}`)
			if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), "ref is not indexed") {
				t.Fatalf("remove-index %s should block read_file: %s", status, resp.Body)
			}
		})
	}
}

func TestGrepAndListTreeRejectWhileRemoveIndexPendingInProgressOrFailed(t *testing.T) {
	for _, status := range []string{"PENDING", "IN_PROGRESS", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			root := t.TempDir()
			repoDir := makeGitRepo(t, root)
			main := "main"
			jobType := "REMOVE_INDEX"
			jobStatus := status
			q := &fakeQuerier{readRepo: db.RepoReadRow{
				ID:              42,
				OrgID:           7,
				Name:            "local/repo",
				CloneURL:        "file://" + repoDir,
				CodeHostType:    "genericGitHost",
				DefaultBranch:   &main,
				IndexedAt:       ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
				Metadata:        []byte(`{"indexedRevisions":["refs/heads/main"]}`),
				LatestJobType:   &jobType,
				LatestJobStatus: &jobStatus,
			}}
			search := &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":1},"files":[],"isSearchExhaustive":true}`)}
			b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}, SearchBackend: search})

			grep := callTool(t, b, 7, "grep", `{"repo":"local/repo","pattern":"Hello","ref":"HEAD"}`)
			if !toolIsError(t, grep) || !strings.Contains(toolText(t, grep), "ref is not indexed") {
				t.Fatalf("remove-index %s should block grep: %s", status, grep.Body)
			}
			if len(search.calls) != 0 {
				t.Fatalf("grep must not call search backend during remove-index, calls=%d", len(search.calls))
			}

			tree := callTool(t, b, 7, "list_tree", `{"repo":"local/repo","path":"","ref":"HEAD"}`)
			if !toolIsError(t, tree) || !strings.Contains(toolText(t, tree), "ref is not indexed") {
				t.Fatalf("remove-index %s should block list_tree: %s", status, tree.Body)
			}
		})
	}
}

func TestReadFileResolvesIndexedBranchFromRemoteTrackingRef(t *testing.T) {
	root := t.TempDir()
	repoDir := makeTreeRepo(t, root)
	repo, err := git.PlainOpen(repoDir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.ReferenceName("refs/remotes/origin/release-a"), head.Hash())); err != nil {
		t.Fatalf("SetReference remote release-a: %v", err)
	}
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:           42,
		OrgID:        7,
		Name:         "local/repo",
		CloneURL:     "file://" + repoDir,
		CodeHostType: "genericGitHost",
		IndexedAt:    ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:     []byte(`{"indexedRevisions":["refs/heads/release-a"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "read_file", `{"repo":"local/repo","path":"src/tenant.ts","ref":"release-a"}`)
	if toolIsError(t, resp) {
		t.Fatalf("read_file should resolve canonical indexed branch through origin remote ref: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "tenant_one_secret_symbol") || !strings.Contains(text, `<repo>local/repo</repo>`) {
		t.Fatalf("remote-tracking branch read_file output wrong: %s", text)
	}
}

func TestListTreeUsesIndexedRefAndReturnsFlatEntries(t *testing.T) {
	root := t.TempDir()
	repoDir := makeTreeRepo(t, root)
	main := "master"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"indexedRevisions":["refs/heads/master"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "list_tree", `{"repo":"local/repo","path":"","ref":"HEAD","depth":2,"maxEntries":10}`)
	if toolIsError(t, resp) {
		t.Fatalf("list_tree returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	for _, want := range []string{`"repo":"local/repo"`, `"ref":"master"`, `"path":"src"`, `"path":"src/tenant.ts"`, `"type":"tree"`, `"type":"blob"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("list_tree output missing %q:\n%s", want, text)
		}
	}
	if q.lastOrgID != 7 || q.lastRepo != "local/repo" {
		t.Fatalf("repo tree scope wrong: org=%d repo=%q", q.lastOrgID, q.lastRepo)
	}
}

func TestListTreeRejectsTraversalAndUnindexedRef(t *testing.T) {
	root := t.TempDir()
	repoDir := makeTreeRepo(t, root)
	main := "master"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"indexedRevisions":["refs/heads/master"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	traversal := callTool(t, b, 7, "list_tree", `{"repo":"local/repo","path":"../secret"}`)
	if !toolIsError(t, traversal) || !strings.Contains(toolText(t, traversal), "invalid file path") {
		t.Fatalf("list_tree traversal should be rejected: %s", traversal.Body)
	}
	unindexed := callTool(t, b, 7, "list_tree", `{"repo":"local/repo","path":"","ref":"feature"}`)
	if !toolIsError(t, unindexed) || !strings.Contains(toolText(t, unindexed), "ref is not indexed") {
		t.Fatalf("list_tree unindexed ref should be rejected: %s", unindexed.Body)
	}
}

func TestCompareBranchesReturnsDiffForIndexedBranches(t *testing.T) {
	root := t.TempDir()
	repoDir := makeBranchRepo(t, root)
	main := "master"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"branches":["master","feature"],"indexedRevisions":["refs/heads/master","refs/heads/feature"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "compare_branches", `{"repo":"local/repo","baseRef":"master","headRef":"feature","includeDiff":true,"maxFiles":10,"maxPatchBytes":4000}`)
	if toolIsError(t, resp) {
		t.Fatalf("compare_branches returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	for _, want := range []string{
		"Branch comparison for local/repo",
		`Base: requested "master"`,
		`Head: requested "feature"`,
		"Base index coverage: indexed",
		"Head index coverage: indexed",
		"README.md",
		"feature.txt",
		"```diff",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("compare output missing %q:\n%s", want, text)
		}
	}
}

func TestCompareBranchesDefaultsBaseToDefaultIndexedBranch(t *testing.T) {
	root := t.TempDir()
	repoDir := makeBranchRepo(t, root)
	main := "master"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"branches":["master","feature"],"indexedRevisions":["refs/heads/master","refs/heads/feature"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "compare_branches", `{"repo":"local/repo","headRef":"feature"}`)
	if toolIsError(t, resp) {
		t.Fatalf("compare_branches returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, `Base: requested "HEAD"`) || !strings.Contains(text, "refs/heads/master") {
		t.Fatalf("default base should resolve through the repo default branch:\n%s", text)
	}
}

func TestCompareBranchesComparesUnindexedBranchFromGitCheckout(t *testing.T) {
	root := t.TempDir()
	repoDir := makeBranchRepo(t, root)
	main := "master"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"branches":["master","feature"],"indexedRevisions":["refs/heads/master"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "compare_branches", `{"repo":"local/repo","baseRef":"master","headRef":"feature"}`)
	if toolIsError(t, resp) {
		t.Fatalf("branch comparison should not require search indexing: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "Head index coverage: not indexed") || !strings.Contains(text, "feature.txt") || !strings.Contains(text, "Diff source: managed git checkout") {
		t.Fatalf("unindexed branch diff output wrong:\n%s", text)
	}
}

func TestCompareBranchesRejectsWhileRemoveIndexPendingInProgressOrFailed(t *testing.T) {
	for _, status := range []string{"PENDING", "IN_PROGRESS", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			main := "master"
			jobType := "REMOVE_INDEX"
			jobStatus := status
			q := &fakeQuerier{readRepo: db.RepoReadRow{
				ID:              42,
				OrgID:           7,
				Name:            "local/repo",
				CloneURL:        "file:///not-opened",
				CodeHostType:    "genericGitHost",
				DefaultBranch:   &main,
				IndexedAt:       ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
				Metadata:        []byte(`{"branches":["master","feature"],"indexedRevisions":["refs/heads/master","refs/heads/feature"]}`),
				LatestJobType:   &jobType,
				LatestJobStatus: &jobStatus,
			}}
			b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: t.TempDir()}})

			resp := callTool(t, b, 7, "compare_branches", `{"repo":"local/repo","baseRef":"master","headRef":"feature"}`)
			if !toolIsError(t, resp) {
				t.Fatalf("compare_branches should be blocked during remove-index: %s", resp.Body)
			}
			text := toolText(t, resp)
			if !strings.Contains(text, "remove-index is active or failed") || !strings.Contains(text, "Reindex the selected branch") {
				t.Fatalf("remove-index block output wrong:\n%s", text)
			}
		})
	}
}

func TestCompareBranchesRejectsBranchOutsideAtomSelectionPolicy(t *testing.T) {
	root := t.TempDir()
	repoDir := makeBranchRepo(t, root)
	main := "master"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"branches":["master","missing-branch"],"indexedRevisions":["refs/heads/master"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "compare_branches", `{"repo":"local/repo","baseRef":"master","headRef":"feature"}`)
	if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), "ref is not selected") {
		t.Fatalf("branch outside Atom selection policy should be rejected: %s", resp.Body)
	}
}

func TestCompareBranchesRejectsImplicitHeadWhenDefaultBranchOutsidePolicy(t *testing.T) {
	root := t.TempDir()
	repoDir := makeBranchRepo(t, root)
	main := "master"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"branches":["feature"],"indexedRevisions":["refs/heads/feature"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "compare_branches", `{"repo":"local/repo","headRef":"feature"}`)
	if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), "ref is not selected") {
		t.Fatalf("implicit HEAD default outside policy should be rejected: %s", resp.Body)
	}
}

func TestCompareBranchesReportsUnsyncedBranch(t *testing.T) {
	root := t.TempDir()
	repoDir := makeBranchRepo(t, root)
	main := "master"
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "local/repo",
		CloneURL:      "file://" + repoDir,
		CodeHostType:  "genericGitHost",
		DefaultBranch: &main,
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"branches":["master"],"indexedRevisions":["refs/heads/master"]}`),
	}}
	b := NewBackend(Config{Queries: q, Paths: repopaths.Config{DataCacheDir: root}})

	resp := callTool(t, b, 7, "compare_branches", `{"repo":"local/repo","baseRef":"master","headRef":"missing-branch"}`)
	if !toolIsError(t, resp) {
		t.Fatalf("missing local branch should be a tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "not available in the managed git checkout") || !strings.Contains(text, "Sync/fetch the missing branch") || !strings.Contains(text, "Indexing is not required for branch diff itself") {
		t.Fatalf("missing branch output wrong:\n%s", text)
	}
}

func TestAskCodebaseOpenAICompatibleSynthesisUsesMandatoryEvidence(t *testing.T) {
	const encKey = "0123456789abcdef0123456789abcdef"
	iv, ciphertext, err := auth.Encrypt(encKey, "zai-test-token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	modelClient := &fakeLanguageModelClient{response: llmproxy.ChatResponse{
		Content: "<!--answer-->\n**Answer / Decision**\nExportTraceServiceRequest is handled in the collector path with exact grep evidence from `grep` ok.",
	}}

	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"collector/exporter.go"},
			"repository":"github.com/acme/collector",
			"chunks":[{"content":"func ExportTraceServiceRequest() {}","contentStart":{"lineNumber":12},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	q := &fakeQuerier{
		total: 1,
		repos: []db.RepoListRow{{
			RepoID:        42,
			RepoName:      "github.com/acme/collector",
			DefaultBranch: ptrString("main"),
		}},
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/collector",
			DefaultBranch: ptrString("main"),
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"branches":["main","feature"],"indexedRevisions":["refs/heads/main","refs/heads/feature"]}`),
		},
		models: []db.OrgLanguageModelRow{{
			Config: []byte(`{"provider":"openai-compatible","model":"glm-4.6","displayName":"GLM","baseUrl":"https://api.z.ai/api/coding/paas/v4","token":{"secretRef":"GLM_KEY"}}`),
		}},
		secret: db.OrgSecretCiphertext{Key: "GLM_KEY", EncryptedValue: ciphertext, IV: iv},
		symbolRows: []db.CodeIntelOccurrenceEvidence{{
			RepoID:         42,
			RepoName:       "github.com/acme/collector",
			DisplayName:    "ExportTraceServiceRequest",
			Symbol:         "scip-go github.com/acme/collector collector/exporter.go/ExportTraceServiceRequest().",
			FilePath:       "collector/exporter.go",
			StartLine:      11,
			StartCharacter: 5,
			EndLine:        11,
			EndCharacter:   30,
			Role:           "DEFINITION",
			LineContent:    ptrString("func ExportTraceServiceRequest() {}"),
			Language:       ptrString("go"),
			Kind:           ptrString("function"),
			Revision:       "refs/heads/main",
			CommitHash:     "abc123",
		}},
		graphEvidence: db.CodeGraphInspectionEvidence{
			Query:               "Where is ExportTraceServiceRequest handled?",
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-1"},
			Anchors: []db.CodeGraphAnchorEvidence{{
				RepoID:      42,
				RepoName:    "github.com/acme/collector",
				Kind:        "function",
				Direction:   "PROVIDES",
				Key:         "ExportTraceServiceRequest",
				NodeVID:     "cg:o7:wabc:r42:c123:s1:babc:function:export",
				WorkspaceID: "ws-1",
				Source:      "tree-sitter-go",
				Revision:    "refs/heads/main",
			}},
		},
	}
	conf := 0.92
	start := int32(11)
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "default", Direction: "bidirectional", MaxDepth: 3},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "cg:o7:wabc:r42:c123:s1:babc:function:export",
			EdgeTargetVID:    "cg:o7:wabc:r42:c123:s1:babc:file:collector",
			Depth:            1,
			Relation:         "DEFINES",
			Confidence:       &conf,
			Source:           "tree-sitter-go",
			EvidenceFilePath: "collector/exporter.go",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/main",
			Start:            graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:function:export", RepoID: ptrInt32(42), Kind: "function", Label: "ExportTraceServiceRequest", Path: "collector/exporter.go"},
			Neighbor:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:file:collector", RepoID: ptrInt32(42), Kind: "file", Label: "collector/exporter.go", Path: "collector/exporter.go"},
		}},
	}}
	b := NewBackend(Config{
		Queries:              q,
		SearchBackend:        search,
		GraphReader:          reader,
		EncryptionKey:        encKey,
		AskMaxSteps:          4,
		LanguageModelClient:  modelClient,
		AllowedModelBaseURLs: []string{"https://api.z.ai/api/coding/paas/v4"},
	})

	resp := callTool(t, b, 7, "ask_codebase", `{"query":"Where is ExportTraceServiceRequest handled?","ref":"feature","conversationContext":"USER: Earlier unrelated architecture discussion.\nASSISTANT: Prior answer mentioned a decoy symbol that must not become the retrieval query."}`)
	if toolIsError(t, resp) {
		t.Fatalf("ask_codebase returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "<!--answer-->") || !strings.Contains(text, "ExportTraceServiceRequest") || !strings.Contains(text, "`grep` ok") {
		t.Fatalf("ask_codebase output wrong:\n%s", text)
	}
	if len(modelClient.calls) != 1 {
		t.Fatalf("model call count: got %d want 1", len(modelClient.calls))
	}
	modelReq := modelClient.calls[0]
	if modelReq.OpenAI.Token != "zai-test-token" {
		t.Fatalf("gateway request token = %q want decrypted secret", modelReq.OpenAI.Token)
	}
	if len(modelReq.Tools) != 0 {
		t.Fatalf("ask_codebase must synthesize from mandatory evidence without a model tool loop, got %d tools", len(modelReq.Tools))
	}
	hasEvidenceMessage := false
	for _, msg := range modelReq.Messages {
		if msg.Role == "user" {
			hasEvidenceMessage = hasEvidenceMessage || strings.Contains(msg.Content, "Mandatory fused retrieval evidence pack")
		}
	}
	if !hasEvidenceMessage {
		t.Fatalf("gateway request must include mandatory fused evidence, messages=%+v", modelReq.Messages)
	}
	if search.last.OrgID != 7 {
		t.Fatalf("grep should execute under org 7, got org %d", search.last.OrgID)
	}
	if !strings.Contains(search.last.Query, `repo:github\.com/acme/collector`) || !strings.Contains(search.last.Query, `branch:feature`) {
		t.Fatalf("org-wide ask should be expanded into a branch-coherent repo scope, query=%q", search.last.Query)
	}
	if strings.Contains(search.last.Query, "decoy symbol") || strings.Contains(q.lastGraph.Query, "decoy symbol") {
		t.Fatalf("conversation context must not pollute retrieval queries: search=%q graph=%q", search.last.Query, q.lastGraph.Query)
	}
	hasConversationContext := false
	for _, msg := range modelReq.Messages {
		if strings.Contains(msg.Content, "Conversation context for this follow-up") && strings.Contains(msg.Content, "decoy symbol") {
			hasConversationContext = true
		}
	}
	if !hasConversationContext {
		t.Fatalf("model synthesis should receive bounded conversation context separately, messages=%+v", modelReq.Messages)
	}
	if len(q.lastGraph.RepoRevisionScopes) != 1 || q.lastGraph.RepoRevisionScopes[0].Repo != "github.com/acme/collector" || strings.Join(q.lastGraph.RepoRevisionScopes[0].RevisionCandidates, ",") != "refs/heads/feature" {
		t.Fatalf("graph preflight should use the same branch-coherent repo scope: %+v", q.lastGraph.RepoRevisionScopes)
	}
	if q.lastSecretKey != "GLM_KEY" {
		t.Fatalf("secret key resolved = %q want GLM_KEY", q.lastSecretKey)
	}
}

func TestAskCodebaseUsesRetrievalQueryOnlyForEvidence(t *testing.T) {
	const encKey = "0123456789abcdef0123456789abcdef"
	iv, ciphertext, err := auth.Encrypt(encKey, "zai-test-token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	modelClient := &fakeLanguageModelClient{response: llmproxy.ChatResponse{Content: "<!--answer-->\nAnswer cites injectPythonSDKToContainer."}}
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"internal/instrumentation/python.go"},
			"repository":"github.com/acme/operator",
			"chunks":[{"content":"func injectPythonSDKToContainer() { existingPythonPath := env.Value }","contentStart":{"lineNumber":43},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	q := &fakeQuerier{
		total: 1,
		repos: []db.RepoListRow{{
			RepoID:        42,
			RepoName:      "github.com/acme/operator",
			DefaultBranch: ptrString("main"),
		}},
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/operator",
			DefaultBranch: ptrString("main"),
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"branches":["main"],"indexedRevisions":["refs/heads/main"]}`),
		},
		models: []db.OrgLanguageModelRow{{
			Config: []byte(`{"provider":"openai-compatible","model":"glm-4.6","displayName":"GLM","baseUrl":"https://api.z.ai/api/coding/paas/v4","token":{"secretRef":"GLM_KEY"}}`),
		}},
		secret: db.OrgSecretCiphertext{Key: "GLM_KEY", EncryptedValue: ciphertext, IV: iv},
		symbolRows: []db.CodeIntelOccurrenceEvidence{{
			RepoName:    "github.com/acme/operator",
			FilePath:    "internal/instrumentation/python.go",
			StartLine:   43,
			Symbol:      "scip-go gomod github.com/acme/operator . `github.com/acme/operator/internal/instrumentation.injectPythonSDKToContainer().`",
			DisplayName: "injectPythonSDKToContainer",
			Kind:        ptrString("function"),
			Language:    ptrString("go"),
			Role:        "DEFINITION",
			Revision:    "refs/heads/main",
			CommitHash:  "abc123",
		}},
		graphEvidence: db.CodeGraphInspectionEvidence{
			Query:               "internal/instrumentation/python.go injectPythonSDKToContainer PYTHONPATH",
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/operator",
				SourceExternalID: "symbol:injectPythonSDKToContainer",
				TargetExternalID: "symbol:sdkInjector",
				Relation:         "CALLS",
				SourceFile:       "internal/instrumentation/python.go",
				StartLine:        ptrInt32(43),
				Source:           "scip",
				Confidence:       0.95,
				ConfidenceTier:   "PRECISE_SCIP",
				Revision:         "refs/heads/main",
				CommitHash:       "abc123",
			}},
		},
	}
	conf := 0.95
	start := int32(43)
	b := NewBackend(Config{
		Queries:       q,
		SearchBackend: search,
		GraphReader: &fakeGraphReader{result: graphreader.InspectResult{
			Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Direction: "bidirectional", MaxDepth: 1},
			Edges: []graphreader.Edge{{
				Relation:         "CALLS",
				Confidence:       &conf,
				Source:           "scip",
				Provenance:       "scip",
				EvidenceFilePath: "internal/instrumentation/python.go",
				StartLine:        &start,
				EdgeRepoID:       ptrInt32(42),
				EdgeRevision:     "refs/heads/main",
				Start:            graphreader.Endpoint{RepoID: ptrInt32(42), Kind: "symbol", Label: "injectPythonSDKToContainer", Path: "internal/instrumentation/python.go"},
				Neighbor:         graphreader.Endpoint{RepoID: ptrInt32(42), Kind: "symbol", Label: "sdkInjector", Path: "internal/instrumentation/sdk.go"},
			}},
		}},
		EncryptionKey:        encKey,
		LanguageModelClient:  modelClient,
		AllowedModelBaseURLs: []string{"https://api.z.ai/api/coding/paas/v4"},
	})

	body := `{"query":"Using your previous answer, focus only on Python: what exact lines matter?","retrievalQuery":"Using your previous answer, focus only on Python: what exact lines matter?\nRelevant prior code anchors for retrieval only:\n- injectPythonSDKToContainer\n- PYTHONPATH\n- sdkInjector.injectPython","async":false}`
	resp := callTool(t, b, 7, "ask_codebase", body)
	if toolIsError(t, resp) {
		t.Fatalf("ask_codebase returned tool error: %s", resp.Body)
	}
	zoektUsedAnchor := false
	for _, call := range search.calls {
		if strings.Contains(call.Query, "injectPythonSDKToContainer") || strings.Contains(call.Query, "PYTHONPATH") || strings.Contains(call.Query, "sdkInjector") {
			zoektUsedAnchor = true
			break
		}
	}
	if !zoektUsedAnchor || !strings.Contains(q.lastGraph.Query, "PYTHONPATH") {
		t.Fatalf("retrieval layers must use the enriched retrieval query: searchCalls=%+v graph=%q", search.calls, q.lastGraph.Query)
	}
	modelReq := modelClient.calls[0]
	if !strings.Contains(modelReq.Messages[len(modelReq.Messages)-1].Content, "Latest user question: Using your previous answer") {
		t.Fatalf("model final question must stay clean and user-authored: %+v", modelReq.Messages)
	}
	if strings.Contains(modelReq.Messages[len(modelReq.Messages)-1].Content, "Relevant prior code anchors") {
		t.Fatalf("retrieval anchors must not be appended to the final user question: %q", modelReq.Messages[len(modelReq.Messages)-1].Content)
	}
	if got, _ := modelReq.Metadata["retrievalQuery"].(string); !strings.Contains(got, "injectPythonSDKToContainer") {
		t.Fatalf("durable metadata should record retrieval query, got %#v", modelReq.Metadata)
	}
}

func TestBuildChatRetrievalQueryUsesPriorAnchorsWithoutFullAnswer(t *testing.T) {
	longProse := strings.Repeat("This broad explanatory paragraph should not become retrieval evidence. ", 60)
	context := buildChatConversationContext([]api.ChatMessage{{
		Role:    "assistant",
		Content: "The Python path flows through `internal/instrumentation/python.go:43` and `injectPythonSDKToContainer`; it merges `PYTHONPATH` before `sdkInjector.injectPython` runs.\n" + longProse,
	}})
	retrieval := buildChatRetrievalQuery("Using your previous answer, focus only on Python.", context)
	for _, want := range []string{"Using your previous answer", "internal/instrumentation/python.go", "injectPythonSDKToContainer", "PYTHONPATH", "sdkInjector.injectPython"} {
		if !strings.Contains(retrieval, want) {
			t.Fatalf("retrieval query missing %q:\n%s", want, retrieval)
		}
	}
	if strings.Contains(retrieval, "broad explanatory paragraph") {
		t.Fatalf("retrieval query should not include whole assistant prose:\n%s", retrieval)
	}
	if len(retrieval) > 4000 {
		t.Fatalf("retrieval query not bounded: %d", len(retrieval))
	}
}

func TestAskCodebaseDefaultsToDurableAsyncWhenGatewaySupportsIt(t *testing.T) {
	const encKey = "0123456789abcdef0123456789abcdef"
	iv, ciphertext, err := auth.Encrypt(encKey, "zai-test-token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	modelClient := &fakeAsyncLanguageModelClient{}
	search := &fakeSearchBackend{body: json.RawMessage(`{
		"stats":{"actualMatchCount":1},
		"files":[{
			"fileName":{"text":"collector/exporter.go"},
			"repository":"github.com/acme/collector",
			"chunks":[{"content":"func ExportTraceServiceRequest() {}","contentStart":{"lineNumber":12},"matchRanges":[{}]}]
		}],
		"isSearchExhaustive":true
	}`)}
	q := &fakeQuerier{
		total: 1,
		repos: []db.RepoListRow{{
			RepoID:        42,
			RepoName:      "github.com/acme/collector",
			DefaultBranch: ptrString("main"),
		}},
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/collector",
			DefaultBranch: ptrString("main"),
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"branches":["main"],"indexedRevisions":["refs/heads/main"]}`),
		},
		models: []db.OrgLanguageModelRow{{
			Config: []byte(`{"provider":"openai-compatible","model":"glm-4.6","displayName":"GLM","baseUrl":"https://api.z.ai/api/coding/paas/v4","token":{"secretRef":"GLM_KEY"}}`),
		}},
		secret: db.OrgSecretCiphertext{Key: "GLM_KEY", EncryptedValue: ciphertext, IV: iv},
		graphEvidence: db.CodeGraphInspectionEvidence{
			Query:               "Where is ExportTraceServiceRequest handled?",
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			Anchors: []db.CodeGraphAnchorEvidence{{
				RepoID:   42,
				RepoName: "github.com/acme/collector",
				Kind:     "function",
				Key:      "ExportTraceServiceRequest",
				Source:   "tree-sitter-go",
				Revision: "refs/heads/main",
			}},
		},
	}
	conf := 0.92
	start := int32(11)
	b := NewBackend(Config{
		Queries:       q,
		SearchBackend: search,
		GraphReader: &fakeGraphReader{result: graphreader.InspectResult{
			Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "default", Direction: "bidirectional", MaxDepth: 3},
			Edges: []graphreader.Edge{{
				Relation:         "DEFINES",
				Confidence:       &conf,
				Source:           "tree-sitter-go",
				EvidenceFilePath: "collector/exporter.go",
				StartLine:        &start,
				EdgeRepoID:       ptrInt32(42),
				EdgeRevision:     "refs/heads/main",
				Start:            graphreader.Endpoint{RepoID: ptrInt32(42), Kind: "function", Label: "ExportTraceServiceRequest", Path: "collector/exporter.go"},
				Neighbor:         graphreader.Endpoint{RepoID: ptrInt32(42), Kind: "file", Label: "collector/exporter.go", Path: "collector/exporter.go"},
			}},
		}},
		EncryptionKey:        encKey,
		LanguageModelClient:  modelClient,
		AllowedModelBaseURLs: []string{"https://api.z.ai/api/coding/paas/v4"},
	})

	resp := callTool(t, b, 7, "ask_codebase", `{"query":"Where is ExportTraceServiceRequest handled?"}`)
	if toolIsError(t, resp) {
		t.Fatalf("ask_codebase returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "ask_codebase synthesis is IN_PROGRESS") || !strings.Contains(text, "get_ask_codebase_result") {
		t.Fatalf("async ask_codebase output wrong:\n%s", text)
	}
	if len(modelClient.starts) != 1 {
		t.Fatalf("StartChat calls: got %d want 1", len(modelClient.starts))
	}
	if len(modelClient.calls) != 0 {
		t.Fatalf("CompleteChat must not run when durable async is available by default, got %d calls", len(modelClient.calls))
	}
	if !strings.HasPrefix(modelClient.starts[0].RequestID, "mcp-") {
		t.Fatalf("durable request id = %q", modelClient.starts[0].RequestID)
	}
	startedReq := modelClient.starts[0]
	if repos := metadataStringSlice(startedReq.Metadata, "repos"); len(repos) != 1 || repos[0] != "github.com/acme/collector" {
		t.Fatalf("durable request metadata repos = %#v", startedReq.Metadata["repos"])
	}
	if startedReq.Metadata["scopeFingerprint"] == "" || startedReq.Metadata["evidenceVersion"] != askSynthesisPromptVersion {
		t.Fatalf("durable request metadata missing scope/evidence version: %#v", startedReq.Metadata)
	}
	if trace := metadataAskToolTrace(startedReq.Metadata, "toolTrace"); len(trace) == 0 || trace[0].ToolName != "codegraph_context" {
		t.Fatalf("durable request metadata tool trace = %#v", startedReq.Metadata["toolTrace"])
	}
}

func TestGetAskCodebaseResultRestoresDurableMetadata(t *testing.T) {
	modelClient := &fakeAsyncLanguageModelClient{
		status: "SUCCEEDED",
		fakeLanguageModelClient: fakeLanguageModelClient{response: llmproxy.ChatResponse{
			Content: "<!--answer-->\n**Answer / Decision**\nDurable answer.",
			Model:   llmproxy.LanguageModelInfo{Provider: "openai-compatible", Model: "glm-4.6"},
			Budget:  llmproxy.AnswerBudget{Mode: "compact", MaxOutputTokens: 6000, MaxAnswerBytes: 96000},
			Metadata: map[string]any{
				"repos":            []any{"github.com/acme/orders"},
				"scopeFingerprint": "snapshot-a",
				"evidenceVersion":  askSynthesisPromptVersion,
				"toolTrace": []any{map[string]any{
					"step":      float64(0),
					"toolName":  "codegraph_context",
					"isError":   false,
					"arguments": `{"query":"checkout flow"}`,
					"output":    "Zoekt + SCIP + AST/tree-sitter + graph proof ok",
				}},
			},
		}},
	}
	b := NewBackend(Config{LanguageModelClient: modelClient})

	resp := callTool(t, b, 7, "get_ask_codebase_result", `{"requestId":"mcp-existing"}`)
	if toolIsError(t, resp) {
		t.Fatalf("get_ask_codebase_result returned tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "`codegraph_context` ok") || !strings.Contains(text, "Durable answer.") {
		t.Fatalf("durable result text missing restored trace/answer:\n%s", text)
	}
	var decoded struct {
		Result struct {
			Meta map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("decode tool response: %v\n%s", err, resp.Body)
	}
	if repos := metadataStringSlice(decoded.Result.Meta, "repos"); len(repos) != 1 || repos[0] != "github.com/acme/orders" {
		t.Fatalf("result meta repos = %#v", decoded.Result.Meta["repos"])
	}
	if trace := metadataAskToolTrace(decoded.Result.Meta, "toolTrace"); len(trace) != 1 || trace[0].ToolName != "codegraph_context" {
		t.Fatalf("result meta tool trace = %#v", decoded.Result.Meta["toolTrace"])
	}
	if decoded.Result.Meta["scopeFingerprint"] != "snapshot-a" || decoded.Result.Meta["evidenceVersion"] != askSynthesisPromptVersion {
		t.Fatalf("result meta missing promoted durable metadata: %#v", decoded.Result.Meta)
	}
}

func TestAskSynthesisPromptRequiresCodingHarnessSections(t *testing.T) {
	prompt := createAskSynthesisPrompt([]string{
		"github.com/acme/orders",
		"github.com/acme/payments",
	}, llmproxy.AnswerBudget{Mode: "full", MaxOutputTokens: 12000, MaxAnswerBytes: maxAskAnswerBytes})
	for _, want := range []string{
		"Answer budget mode: full",
		"HLD Architecture Map",
		"LLD Execution Flow",
		"Multi-Repo Navigation",
		"Code Navigation Table",
		"Symbol And Call Graph",
		"Function / Variable / Data Flow",
		"Sequence Diagram",
		"Exact Development Touchpoints",
		"Mermaid sequenceDiagram or flowchart",
		"repo, path, line, symbol, role",
		"automated coding harness",
		"operator-side injection evidence",
		"runtime repository implementation evidence",
		"Do not hand off vague instructions",
		"not in the selected/indexed repository set",
		"github.com/acme/orders",
		"github.com/acme/payments",
		"Separate retrieved facts from inferred implementation plans",
		"do not attach exact line numbers unless the evidence contains those existing lines",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("ask_codebase synthesis prompt missing %q:\n%s", want, prompt)
		}
	}
	formatBlock := prompt
	if idx := strings.Index(prompt, "Answer format:"); idx >= 0 {
		formatBlock = prompt[idx:]
	}
	touchpoints := strings.Index(formatBlock, "Exact Development Touchpoints")
	gaps := strings.Index(formatBlock, "Gaps / Verification Needed")
	navigation := strings.Index(formatBlock, "Multi-Repo Navigation")
	graph := strings.Index(formatBlock, "Symbol And Call Graph")
	diagram := strings.Index(formatBlock, "Sequence Diagram")
	if touchpoints < 0 || gaps < 0 || navigation < 0 || graph < 0 || diagram < 0 ||
		touchpoints > navigation || gaps > navigation || touchpoints > graph || gaps > graph || touchpoints > diagram || gaps > diagram {
		t.Fatalf("touchpoints and gaps must be front-loaded before broad tables/diagrams so compact answers do not truncate them:\n%s", formatBlock)
	}
}

func TestFormatAskCompletionAddsProvenanceAndHarnessMap(t *testing.T) {
	b := NewBackend(Config{})
	trace := []askToolTrace{{
		Step:     0,
		ToolName: "codegraph_context",
		Output: strings.Join([]string{
			"## Zoekt broad recall (`grep` ok)",
			"[github.com/acme/orders] internal/order/service.go:",
			"  42: func CreateOrder(ctx context.Context) error {",
			"## SCIP definitions (`find_symbol_definitions` ok)",
			"Found 1 precise SCIP definition",
			"- source=scip function CreateOrder",
			"## Graph minimal context (`graph_minimal_context` ok)",
			"- source=ast-go confidence=0.6 CreateOrder --CALLS--> ReserveInventory at internal/order/service.go:44",
			"Gaps: missing payment repository evidence.",
		}, "\n"),
	}}
	result, err := b.formatAskCompletion(
		"mcp-test",
		languageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
		trace,
		[]string{"github.com/acme/orders"},
		answerTag+"\nAnswer / Decision\nUse CreateOrder.",
		llmproxy.AnswerBudget{Mode: "compact", MaxAnswerBytes: maxAskAnswerBytes},
		map[string]any{"scopeFingerprint": "scope-a", "evidenceVersion": askSynthesisPromptVersion},
	)
	if err != nil {
		t.Fatalf("formatAskCompletion: %v", err)
	}
	text := toolResultText(result)
	for _, want := range []string{"Evidence provenance summary", "Harness JSON", `"filesToRead"`, `"callEdges"`, "source=scip"} {
		if !strings.Contains(text, want) {
			t.Fatalf("formatted ask result missing %q:\n%s", want, text)
		}
	}
	provenance, ok := result.Meta["provenance"].(map[string]int)
	if !ok || provenance["scip"] == 0 || provenance["ast"] == 0 || provenance["graph"] == 0 {
		t.Fatalf("meta provenance missing semantic counts: %#v", result.Meta["provenance"])
	}
	harness, ok := result.Meta["harness"].(askHarnessEvidence)
	if !ok {
		t.Fatalf("meta harness type = %#v", result.Meta["harness"])
	}
	if len(harness.FilesToRead) == 0 || harness.FilesToRead[0].Path != "internal/order/service.go" {
		t.Fatalf("harness filesToRead = %#v", harness.FilesToRead)
	}
	if len(harness.CallEdges) == 0 || !strings.Contains(harness.CallEdges[0], "CreateOrder") {
		t.Fatalf("harness callEdges = %#v", harness.CallEdges)
	}
	if len(harness.Gaps) == 0 || !strings.Contains(harness.Gaps[0], "missing payment") {
		t.Fatalf("harness gaps = %#v", harness.Gaps)
	}
}

func TestFormatAskCompletionPreservesRetrievalHarnessFromMetadata(t *testing.T) {
	b := NewBackend(Config{})
	retrievalHarness := askHarnessEvidence{
		Provenance: map[string]int{"zoekt": 2, "scip": 1, "ast": 1, "treeSitter": 0, "graph": 1, "heuristic": 1},
		FilesToRead: []askHarnessFileRef{{
			Repo:   "github.com/acme/orders",
			Path:   "internal/order/service.go",
			Line:   42,
			Role:   "read-before-edit",
			Source: "retrieval",
		}},
		Symbols:     []string{"CreateOrder"},
		CallEdges:   []string{"CreateOrder --CALLS--> ReserveInventory"},
		Confidence:  "retrieval-high",
		EvidenceIDs: []string{"codegraph_context#0"},
	}
	result, err := b.formatAskCompletion(
		"mcp-test",
		languageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
		nil,
		[]string{"github.com/acme/orders"},
		answerTag+"\nAnswer / Decision\nUse the order service.",
		llmproxy.AnswerBudget{Mode: "compact", MaxAnswerBytes: maxAskAnswerBytes},
		map[string]any{"retrievalHarness": retrievalHarness},
	)
	if err != nil {
		t.Fatalf("formatAskCompletion: %v", err)
	}
	harness, ok := result.Meta["harness"].(askHarnessEvidence)
	if !ok {
		t.Fatalf("meta harness type = %#v", result.Meta["harness"])
	}
	if len(harness.FilesToRead) == 0 || harness.FilesToRead[0].Path != "internal/order/service.go" {
		t.Fatalf("retrieval file was not preserved: %#v", harness.FilesToRead)
	}
	if harness.Provenance["scip"] == 0 || harness.Provenance["graph"] == 0 {
		t.Fatalf("retrieval provenance was not preserved: %#v", harness.Provenance)
	}
	if len(harness.CallEdges) == 0 || harness.CallEdges[0] != "CreateOrder --CALLS--> ReserveInventory" {
		t.Fatalf("retrieval call edge was not preserved: %#v", harness.CallEdges)
	}
}

func TestDeterministicAskSessionIDUsesScopeFingerprintNotEvidenceText(t *testing.T) {
	req := api.MCPRequest{OrgID: 7, OrgDomain: "orga"}
	model := languageModelInfo{Provider: "openai-compatible", Model: "glm-5"}
	budget, err := normalizeAskAnswerBudget("full")
	if err != nil {
		t.Fatal(err)
	}
	a := deterministicAskSessionID(req, "checkout flow", "checkout flow", []string{"github.com/acme/b", "github.com/acme/a"}, "main", "snapshot-a", model, budget)
	b := deterministicAskSessionID(req, "checkout flow", "checkout flow", []string{"github.com/acme/a", "github.com/acme/b"}, "main", "snapshot-a", model, budget)
	if a != b {
		t.Fatalf("repo ordering should not change ask request id:\n%s\n%s", a, b)
	}
	c := deterministicAskSessionID(req, "checkout flow", "checkout flow", []string{"github.com/acme/a", "github.com/acme/b"}, "main", "snapshot-b", model, budget)
	if a == c {
		t.Fatalf("snapshot fingerprint changes must invalidate durable ask request id: %s", a)
	}
	withRetrieval := deterministicAskSessionID(req, "checkout flow", "checkout flow\nRelevant prior code anchors for retrieval only:\n- CheckoutController", []string{"github.com/acme/a", "github.com/acme/b"}, "main", "snapshot-a", model, budget)
	if a == withRetrieval {
		t.Fatalf("retrieval query changes must invalidate durable ask request id: %s", a)
	}
	compact, err := normalizeAskAnswerBudget("compact")
	if err != nil {
		t.Fatal(err)
	}
	d := deterministicAskSessionID(req, "checkout flow", "checkout flow", []string{"github.com/acme/a", "github.com/acme/b"}, "main", "snapshot-a", model, compact)
	if a == d {
		t.Fatalf("answer budget changes must invalidate durable ask request id: %s", a)
	}
}

func TestNormalizeAskAnswerBudgetModes(t *testing.T) {
	full, err := normalizeAskAnswerBudget("")
	if err != nil {
		t.Fatal(err)
	}
	compact, err := normalizeAskAnswerBudget("compact")
	if err != nil {
		t.Fatal(err)
	}
	if full.Mode != "full" || full.MaxOutputTokens <= compact.MaxOutputTokens {
		t.Fatalf("budget sizing unexpected: full=%+v compact=%+v", full, compact)
	}
	brief, err := normalizeAskAnswerBudget("brief")
	if err != nil {
		t.Fatal(err)
	}
	if brief.MaxOutputTokens < 3000 {
		t.Fatalf("brief budget must be large enough to avoid mid-structure truncation: %+v", brief)
	}
	if _, err := normalizeAskAnswerBudget("tiny"); err == nil {
		t.Fatalf("invalid answer budget must fail")
	}
}

func TestBriefAskSynthesisPromptAvoidsTables(t *testing.T) {
	prompt := createAskSynthesisPrompt([]string{"github.com/acme/orders"}, llmproxy.AnswerBudget{Mode: "brief", MaxOutputTokens: 3000, MaxAnswerBytes: 64000})
	for _, want := range []string{
		"do not use markdown tables",
		"cannot truncate mid-structure",
		"For Code Navigation, do not use a markdown table",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("brief prompt missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "For Code Navigation Table, use markdown table rows") {
		t.Fatalf("brief prompt should not request table rows:\n%s", prompt)
	}
}

func TestRedisGraphEvidenceCacheUsesConfigurablePayloadCaps(t *testing.T) {
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer redisClient.Close()

	cache := NewRedisGraphEvidenceCacheWithLimits(redisClient, nil, 1111, 2222)
	if cache == nil {
		t.Fatalf("cache must be constructed for non-nil redis client")
	}
	if cache.maxGraphInspectionBytes != 1111 {
		t.Fatalf("graph inspection max bytes = %d, want 1111", cache.maxGraphInspectionBytes)
	}
	if cache.maxCodegraphContextBytes != 2222 {
		t.Fatalf("codegraph context max bytes = %d, want 2222", cache.maxCodegraphContextBytes)
	}

	cache = NewRedisGraphEvidenceCacheWithLimits(redisClient, nil, 0, -1)
	if cache.maxGraphInspectionBytes != defaultMaxGraphInspectionCacheBytes {
		t.Fatalf("fallback graph inspection max bytes = %d, want %d", cache.maxGraphInspectionBytes, defaultMaxGraphInspectionCacheBytes)
	}
	if cache.maxCodegraphContextBytes != defaultMaxCodegraphContextCacheBytes {
		t.Fatalf("fallback codegraph context max bytes = %d, want %d", cache.maxCodegraphContextBytes, defaultMaxCodegraphContextCacheBytes)
	}
}

func TestCodegraphContextCodingHarnessTriggersDeepEvidence(t *testing.T) {
	query := "Explain OpenTelemetry Operator auto-instrumentation for NodeJS, Python, and .NET with coding harness touchpoints"
	if !codegraphContextWantsTestEvidence(query) {
		t.Fatalf("coding harness questions should fetch test evidence")
	}
	runtime := codegraphContextRuntimeSidePattern(query)
	for _, want := range []string{"NODE_OPTIONS", "PYTHONPATH", "DOTNET_STARTUP_HOOKS"} {
		if !strings.Contains(runtime, want) {
			t.Fatalf("runtime-side pattern missing %q: %s", want, runtime)
		}
	}
	crd := codegraphContextCRDSpecPattern(query)
	for _, want := range []string{"InstrumentationSpec", "VolumeClaimTemplate"} {
		if !strings.Contains(crd, want) {
			t.Fatalf("CRD/spec pattern missing %q: %s", want, crd)
		}
	}
	perRuntime := codegraphContextRuntimeSidePatterns(query)
	if len(perRuntime) != 3 {
		t.Fatalf("runtime-side per-language recall count = %d, want 3: %+v", len(perRuntime), perRuntime)
	}
	byLanguage := map[string]string{}
	for _, item := range perRuntime {
		byLanguage[item.Language] = item.Pattern
	}
	for language, want := range map[string]string{
		"NodeJS": "NODE_OPTIONS",
		"Python": "PYTHONPATH",
		".NET":   "DOTNET_STARTUP_HOOKS",
	} {
		if !strings.Contains(byLanguage[language], want) {
			t.Fatalf("%s runtime-side recall missing %q: %+v", language, want, byLanguage)
		}
	}
	mandatory := codegraphContextMandatorySourceSliceCandidates(query+" CRD webhook registration", []string{
		"github.com/open-telemetry/opentelemetry-operator",
		"github.com/open-telemetry/opentelemetry-js",
	})
	mandatoryByPath := map[string]bool{}
	for _, candidate := range mandatory {
		mandatoryByPath[candidate.Path] = true
		if candidate.Repo != "github.com/open-telemetry/opentelemetry-operator" {
			t.Fatalf("mandatory operator source candidates must not fan out to runtime repos: %+v", candidate)
		}
	}
	for _, want := range []string{
		"apis/v1alpha1/instrumentation_types.go",
		"config/webhook/manifests.yaml",
		"config/default/manager_webhook_patch.yaml",
		"main.go",
	} {
		if !mandatoryByPath[want] {
			t.Fatalf("mandatory source candidates missing %s: %+v", want, mandatory)
		}
	}
}

func TestCodegraphContextMandatorySourceSlicesIncludePythonConstantsForPythonPath(t *testing.T) {
	mandatory := codegraphContextMandatorySourceSliceCandidates("Explain Python PYTHONPATH merge with envPythonPath and pythonPathPrefix.", []string{"github.com/open-telemetry/opentelemetry-operator"})
	found := false
	for _, candidate := range mandatory {
		if candidate.Path == "internal/instrumentation/python.go" && candidate.Line == 1 && candidate.Source == "python-constants" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Python/PYTHONPATH questions must force python.go top constants, got %+v", mandatory)
	}
}

func TestCodegraphContextSourceSelectionKeepsRuntimeEnvAndImplementationSlices(t *testing.T) {
	query := "Explain .NET auto-instrumentation runtime env vars and injectDotNet implementation for a coding harness"
	selected := codegraphContextSelectSourceSlices(query, []codegraphContextSourceSliceCandidate{
		{
			Repo:   "github.com/open-telemetry/opentelemetry-operator",
			Path:   "internal/instrumentation/dotnet.go",
			Line:   42,
			Source: "implementation",
			Score:  codegraphContextSourceSliceBaseScore("implementation", "internal/instrumentation/dotnet.go", 42),
		},
		{
			Repo:   "github.com/open-telemetry/opentelemetry-operator",
			Path:   "internal/instrumentation/dotnet.go",
			Line:   42,
			Source: "implementation",
			Score:  codegraphContextSourceSliceBaseScore("implementation", "internal/instrumentation/dotnet.go", 42),
		},
		{
			Repo:   "github.com/open-telemetry/opentelemetry-operator",
			Path:   "internal/instrumentation/dotnet.go",
			Line:   21,
			Source: "runtime-side",
			Score:  codegraphContextSourceSliceBaseScore("runtime-side", "internal/instrumentation/dotnet.go", 21),
		},
	}, 8)
	seen := map[string]bool{}
	for _, slice := range selected {
		seen[slice.Source+":"+strconv.Itoa(slice.Line)] = true
	}
	if !seen["implementation:42"] || !seen["runtime-side:21"] {
		t.Fatalf("runtime constants and implementation slices must both survive selection: %+v", selected)
	}
}

func TestCodegraphContextDefinitionSourceSlicesForceHelperBodies(t *testing.T) {
	output := strings.Join([]string{
		"Found 2 precise SCIP definitions",
		"",
		"SCIP symbol: getIndexOfEnv",
		"  language=go; kind=function",
		"[github.com/open-telemetry/opentelemetry-operator] internal/instrumentation/sdk.go:",
		"  28:1 DEFINITION func getIndexOfEnv(env []corev1.EnvVar, name string) int {",
		"",
		"SCIP symbol: validateContainerEnv",
		"  language=go; kind=function",
		"[github.com/open-telemetry/opentelemetry-operator] internal/instrumentation/sdk.go:",
		"  44:1 DEFINITION func validateContainerEnv(env []corev1.EnvVar, names ...string) error {",
	}, "\n")
	candidates := codegraphContextDefinitionSourceSliceCandidates("Show helper function bodies for validateContainerEnv and getIndexOfEnv.", output)
	if len(candidates) != 2 {
		t.Fatalf("definition candidates count = %d, want 2: %+v", len(candidates), candidates)
	}
	selected := codegraphContextSelectSourceSlices("Show helper function bodies for validateContainerEnv and getIndexOfEnv.", candidates, 8)
	seen := map[int]bool{}
	for _, candidate := range selected {
		if candidate.Source != "scip-definition-requested" {
			t.Fatalf("helper body query should force definition source slices, got %+v", selected)
		}
		seen[candidate.Line] = true
	}
	if !seen[28] || !seen[44] {
		t.Fatalf("same-file helper definitions must both survive selection, got %+v", selected)
	}
}

func TestCodegraphContextRequestedDefinitionPromotionRejectsBroadTokens(t *testing.T) {
	query := "Using your previous SDK answer, include validateContainerEnv and getIndexOfEnv definitions."
	if codegraphContextLineHasExplicitQuerySymbol(query, "  102:1 DEFINITION func (i *sdkInjector) injectNodeJS(...) error {") {
		t.Fatalf("broad SDK/injector lines must not be promoted as requested helper definitions")
	}
	if !codegraphContextLineHasExplicitQuerySymbol(query, "  816:1 DEFINITION func getIndexOfEnv(envs []corev1.EnvVar, name string) int {") {
		t.Fatalf("explicit getIndexOfEnv definition should be promoted")
	}
	if !codegraphContextLineHasExplicitQuerySymbol(query, "  866:1 DEFINITION func validateContainerEnv(envs []corev1.EnvVar, envsToBeValidated ...string) error {") {
		t.Fatalf("explicit validateContainerEnv definition should be promoted")
	}
}

func TestCodegraphContextDefinitionSourceSlicesForceRequestedTestFunctions(t *testing.T) {
	output := strings.Join([]string{
		"Found 3 precise SCIP definitions",
		"",
		"SCIP symbol: TestInjectPythonSDK",
		"  language=go; kind=function",
		"[github.com/open-telemetry/opentelemetry-operator] internal/instrumentation/podmutator_test.go:",
		"  5308:1 DEFINITION func TestInjectPythonSDK(t *testing.T) {",
		"",
		"SCIP symbol: TestInjectNodeJS",
		"  language=go; kind=function",
		"[github.com/open-telemetry/opentelemetry-operator] internal/instrumentation/podmutator_test.go:",
		"  5120:1 DEFINITION func TestInjectNodeJS(t *testing.T) {",
		"",
		"SCIP symbol: TestInjectDotNetSDK",
		"  language=go; kind=function",
		"[github.com/open-telemetry/opentelemetry-operator] internal/instrumentation/dotnet_test.go:",
		"  631:1 DEFINITION func TestInjectDotNetSDK(t *testing.T) {",
	}, "\n")
	query := "Show exact tests TestInjectPythonSDK TestInjectNodeJS and TestInjectDotNetSDK."
	candidates := codegraphContextDefinitionSourceSliceCandidates(query, output)
	if len(candidates) != 3 {
		t.Fatalf("test definition candidates count = %d, want 3: %+v", len(candidates), candidates)
	}
	selected := codegraphContextDiverseSourceSlices(query, candidates, 3, 1)
	seen := map[string]bool{}
	for _, candidate := range selected {
		if candidate.Source != "scip-test-requested" {
			t.Fatalf("requested test definitions should be tagged scip-test-requested, got %+v", selected)
		}
		seen[candidate.Path+":"+strconv.Itoa(candidate.Line)] = true
	}
	for _, want := range []string{
		"internal/instrumentation/podmutator_test.go:5308",
		"internal/instrumentation/podmutator_test.go:5120",
		"internal/instrumentation/dotnet_test.go:631",
	} {
		if !seen[want] {
			t.Fatalf("requested test definition %s missing after diversity selection: %+v", want, selected)
		}
	}
}

func TestCodegraphContextDiverseSourceSlicesKeepsRequestedHelperDefinitionsPastPathCap(t *testing.T) {
	query := "Show helper function bodies for validateContainerEnv and getIndexOfEnv."
	candidates := []codegraphContextSourceSliceCandidate{
		{
			Repo:   "github.com/open-telemetry/opentelemetry-operator",
			Path:   "internal/instrumentation/sdk.go",
			Line:   55,
			Source: "scip-definition-forced",
			Score:  codegraphContextSourceSliceBaseScore("scip-definition-forced", "internal/instrumentation/sdk.go", 55),
		},
		{
			Repo:   "github.com/open-telemetry/opentelemetry-operator",
			Path:   "internal/instrumentation/sdk.go",
			Line:   102,
			Source: "scip-definition-forced",
			Score:  codegraphContextSourceSliceBaseScore("scip-definition-forced", "internal/instrumentation/sdk.go", 102),
		},
		{
			Repo:   "github.com/open-telemetry/opentelemetry-operator",
			Path:   "internal/instrumentation/sdk.go",
			Line:   140,
			Source: "scip-definition-forced",
			Score:  codegraphContextSourceSliceBaseScore("scip-definition-forced", "internal/instrumentation/sdk.go", 140),
		},
		{
			Repo:   "github.com/open-telemetry/opentelemetry-operator",
			Path:   "internal/instrumentation/sdk.go",
			Line:   816,
			Source: "scip-definition-requested",
			Score:  codegraphContextSourceSliceBaseScore("scip-definition-requested", "internal/instrumentation/sdk.go", 816),
		},
		{
			Repo:   "github.com/open-telemetry/opentelemetry-operator",
			Path:   "internal/instrumentation/sdk.go",
			Line:   866,
			Source: "scip-definition-requested",
			Score:  codegraphContextSourceSliceBaseScore("scip-definition-requested", "internal/instrumentation/sdk.go", 866),
		},
	}
	selected := codegraphContextDiverseSourceSlices(query, candidates, 4, 3)
	seen := map[int]bool{}
	for _, candidate := range selected {
		seen[candidate.Line] = true
	}
	if !seen[816] || !seen[866] {
		t.Fatalf("requested helper definitions must bypass generic same-file cap, got %+v", selected)
	}
}

func TestCodegraphContextDiverseSourceSlicesKeepsCRDAndWebhookRegistration(t *testing.T) {
	query := "Explain OpenTelemetry Operator auto-instrumentation CRD webhook registration for a coding harness"
	candidates := codegraphContextMandatorySourceSliceCandidates(query, []string{"github.com/open-telemetry/opentelemetry-operator"})
	selected := codegraphContextSelectSourceSlices(query, candidates, 72)
	selected = codegraphContextDiverseSourceSlices(query, selected, 18, 3)
	seen := map[string]bool{}
	for _, candidate := range selected {
		seen[candidate.Path] = true
	}
	for _, want := range []string{
		"apis/v1alpha1/instrumentation_types.go",
		"config/webhook/manifests.yaml",
		"config/default/manager_webhook_patch.yaml",
		"main.go",
	} {
		if !seen[want] {
			t.Fatalf("compact source selection should retain %s: %+v", want, selected)
		}
	}
}

func TestCodegraphContextDiverseSourceSlicesKeepsCrossRepoOTLPEvidence(t *testing.T) {
	query := "Explain the end-to-end OpenTelemetry OTLP trace export flow across these repositories: SDK client creation, ExportTraceServiceRequest proto shape, collector receiver/service handling, exporter path, and operator auto-instrumentation injection."
	candidates := []codegraphContextSourceSliceCandidate{
		{Repo: "github.com/open-telemetry/opentelemetry-operator", Path: "internal/instrumentation/sdk.go", Line: 102, Source: "scip-definition", Score: 240},
		{Repo: "github.com/open-telemetry/opentelemetry-operator", Path: "internal/instrumentation/podmutator.go", Line: 210, Source: "scip-definition", Score: 238},
		{Repo: "github.com/open-telemetry/opentelemetry-operator", Path: "config/webhook/manifests.yaml", Line: 1, Source: "webhook-registration", Score: 220},
		{Repo: "github.com/open-telemetry/opentelemetry-collector", Path: "pdata/internal/generated_proto_exporttraceservicerequest.go", Line: 20, Source: "otlp-flow", Score: codegraphContextSourceSliceBaseScore("otlp-flow", "pdata/internal/generated_proto_exporttraceservicerequest.go", 20)},
		{Repo: "github.com/open-telemetry/opentelemetry-dotnet", Path: "src/Shared/Proto/opentelemetry/proto/collector/trace/v1/trace_service.proto", Line: 35, Source: "otlp-flow", Score: codegraphContextSourceSliceBaseScore("otlp-flow", "src/Shared/Proto/opentelemetry/proto/collector/trace/v1/trace_service.proto", 35)},
		{Repo: "github.com/open-telemetry/opentelemetry-go", Path: "exporters/otlp/otlptrace/otlptracegrpc/client.go", Line: 45, Source: "otlp-flow", Score: codegraphContextSourceSliceBaseScore("otlp-flow", "exporters/otlp/otlptrace/otlptracegrpc/client.go", 45)},
	}
	selected := codegraphContextDiverseSourceSlices(query, candidates, 4, 3)
	seenRepo := map[string]bool{}
	for _, candidate := range selected {
		seenRepo[candidate.Repo] = true
	}
	for _, want := range []string{
		"github.com/open-telemetry/opentelemetry-operator",
		"github.com/open-telemetry/opentelemetry-collector",
		"github.com/open-telemetry/opentelemetry-dotnet",
		"github.com/open-telemetry/opentelemetry-go",
	} {
		if !seenRepo[want] {
			t.Fatalf("cross-repo OTLP source selection should retain %s: %+v", want, selected)
		}
	}
}

func TestCodegraphContextOTLPFlowPatternIncludesProtocolCollectorAndSDKTerms(t *testing.T) {
	pattern := codegraphContextOTLPFlowPattern("Explain OTLP trace export from SDK to collector using ExportTraceServiceRequest")
	for _, want := range []string{"ExportTraceServiceRequest", "TraceService", "NewExporter", "ExportSpans", "ConsumeTraces"} {
		if !strings.Contains(pattern, want) {
			t.Fatalf("OTLP flow pattern missing %q: %s", want, pattern)
		}
	}
}

func TestCodegraphContextMandatorySourceSlicesIncludeOpenTelemetryOTLPEcosystem(t *testing.T) {
	query := "Explain OTLP trace export flow across repositories with SDK client creation, ExportTraceServiceRequest, collector receiver and exporter path."
	repos := []string{
		"github.com/open-telemetry/opentelemetry-go",
		"github.com/open-telemetry/opentelemetry-collector",
		"github.com/open-telemetry/opentelemetry-proto",
		"github.com/open-telemetry/opentelemetry-specification",
		"github.com/open-telemetry/opentelemetry-operator",
		"github.com/open-telemetry/opentelemetry-js",
		"github.com/open-telemetry/opentelemetry-java",
		"github.com/open-telemetry/opentelemetry-python",
		"github.com/open-telemetry/opentelemetry-dotnet",
		"github.com/open-telemetry/opentelemetry-rust",
	}
	candidates := codegraphContextMandatorySourceSliceCandidates(query, repos)
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.Source == "otlp-flow" || (candidate.Repo == "github.com/open-telemetry/opentelemetry-operator" && candidate.Source != "") {
			seen[candidate.Repo] = true
		}
	}
	for _, repo := range repos {
		if !seen[repo] {
			t.Fatalf("mandatory OTLP source slices missing repo %s: %+v", repo, candidates)
		}
	}
}

func TestCodegraphContextSourceSliceManifestProofsStayLineLocal(t *testing.T) {
	output := strings.Join([]string{
		"<repo>github.com/open-telemetry/opentelemetry-operator</repo>",
		"<path>internal/instrumentation/podmutator.go</path>",
		"<content>",
		"111: \t\treturn false, errors.New(\"incorrect instrumentation configuration - please provide container names for all instrumentations\")",
		"124: \treturn true, nil",
		"128: func (langInsts *languageInstrumentations) setCommonInstrumentedContainers(ns corev1.Namespace, pod corev1.Pod) error {",
		"129: \tcontainersAnnotation := annotationValue(ns.ObjectMeta, pod.ObjectMeta, annotationInjectContainerName)",
		"148: func (langInsts *languageInstrumentations) setLanguageSpecificContainers(ns, pod metav1.ObjectMeta) error {",
		"167: \t\t\tannotation: annotationInjectPythonContainersName,",
		"260: func (pm *instPodMutator) Mutate(ctx context.Context, ns corev1.Namespace, pod corev1.Pod) (corev1.Pod, error) {",
		"</content>",
	}, "\n")
	line := codegraphContextSourceSliceManifestLine(codegraphContextSourceSliceCandidate{
		Path:   "internal/instrumentation/podmutator.go",
		Line:   128,
		Source: "webhook",
	}, output, false)
	if strings.Contains(line, "instPodMutator") || strings.Contains(line, "annotationInjectPython") {
		t.Fatalf("manifest must not attach non-local proof to podmutator.go:128:\n%s", line)
	}

	local := codegraphContextSourceSliceManifestLine(codegraphContextSourceSliceCandidate{
		Path:   "internal/instrumentation/nodejs.go",
		Line:   20,
		Source: "runtime-env",
	}, strings.Join([]string{
		"<repo>github.com/open-telemetry/opentelemetry-operator</repo>",
		"<path>internal/instrumentation/nodejs.go</path>",
		"<content>",
		"13: const nodejsInitContainerName = \"opentelemetry-auto-instrumentation-nodejs\"",
		"20: const envNodeOptions = \"NODE_OPTIONS\"",
		"24: const nodejsInstrMountPath = \"/otel-auto-instrumentation-nodejs\"",
		"</content>",
	}, "\n"), false)
	if !strings.Contains(local, "20 NODE_OPTIONS") {
		t.Fatalf("manifest should keep exact local proof with line number:\n%s", local)
	}
}

func TestCodegraphContextCompactReadFileKeepsWebhookYAML(t *testing.T) {
	output := strings.Join([]string{
		"<repo>github.com/open-telemetry/opentelemetry-operator</repo>",
		"<path>config/webhook/manifests.yaml</path>",
		"<content>",
		"1: ---",
		"2: apiVersion: admissionregistration.k8s.io/v1",
		"3: kind: MutatingWebhookConfiguration",
		"4: metadata:",
		"5:   name: mutating-webhook-configuration",
		"6: webhooks:",
		"9:   clientConfig:",
		"10:     service:",
		"11:       name: webhook-service",
		"13:       path: /mutate-opentelemetry-io-v1alpha1-instrumentation",
		"14:   failurePolicy: Fail",
		"16:   rules:",
		"</content>",
	}, "\n")
	compact := codegraphContextCompactReadFile(output)
	for _, want := range []string{
		"apiVersion: admissionregistration.k8s.io/v1",
		"kind: MutatingWebhookConfiguration",
		"path: /mutate-opentelemetry-io-v1alpha1-instrumentation",
		"failurePolicy: Fail",
	} {
		if !strings.Contains(compact, want) {
			t.Fatalf("compact webhook YAML missing %q:\n%s", want, compact)
		}
	}
}

func TestAskCodebasePreloadsGraphForArchitectureQuestion(t *testing.T) {
	const encKey = "0123456789abcdef0123456789abcdef"
	iv, ciphertext, err := auth.Encrypt(encKey, "zai-test-token")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	modelClient := &fakeLanguageModelClient{response: llmproxy.ChatResponse{
		Content: "<!--answer-->\n**Answer / Decision**\nGraph evidence was preloaded before synthesis.",
	}}

	start := int32(3)
	q := &fakeQuerier{
		total: 1,
		repos: []db.RepoListRow{{
			RepoID:        42,
			RepoName:      "github.com/acme/orders",
			DefaultBranch: ptrString("main"),
		}},
		readRepo: db.RepoReadRow{
			ID:            42,
			OrgID:         7,
			Name:          "github.com/acme/orders",
			DefaultBranch: ptrString("main"),
			IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
			Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
		},
		models: []db.OrgLanguageModelRow{{
			Config: []byte(`{"provider":"openai-compatible","model":"glm-4.6","displayName":"GLM","baseUrl":"https://api.z.ai/api/coding/paas/v4","token":{"secretRef":"GLM_KEY"}}`),
		}},
		secret: db.OrgSecretCiphertext{Key: "GLM_KEY", EncryptedValue: ciphertext, IV: iv},
		graphEvidence: db.CodeGraphInspectionEvidence{
			Query:               "Explain checkout architecture flow",
			SearchedRepoCount:   1,
			ActiveSnapshotCount: 1,
			WorkspaceIDs:        []string{"ws-1"},
			Anchors: []db.CodeGraphAnchorEvidence{{
				RepoID:           42,
				RepoName:         "github.com/acme/orders",
				Kind:             "route",
				Direction:        "PROVIDES",
				Key:              "POST /checkout",
				NormalizedKey:    "post /checkout",
				NodeVID:          "cg:o7:wabc:r42:c123:s1:babc:route:seed",
				WorkspaceID:      "ws-1",
				EvidenceFilePath: ptrString("src/routes/checkout.ts"),
				StartLine:        &start,
				Confidence:       1,
				Source:           "ast",
				Revision:         "refs/heads/main",
			}},
		},
		symbolRows: []db.CodeIntelOccurrenceEvidence{{
			RepoID:         42,
			RepoName:       "github.com/acme/orders",
			DisplayName:    "checkout",
			Symbol:         "scip-typescript github.com/acme/orders src/routes/checkout.ts/checkout().",
			FilePath:       "src/routes/checkout.ts",
			StartLine:      2,
			StartCharacter: 13,
			EndLine:        2,
			EndCharacter:   21,
			Role:           "DEFINITION",
			LineContent:    ptrString("export function checkout() {}"),
			Language:       ptrString("typescript"),
			Kind:           ptrString("function"),
			Revision:       "refs/heads/main",
			CommitHash:     "abc123",
		}},
	}
	conf := 0.94
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "architecture", Direction: "bidirectional", MaxDepth: 4},
		Edges: []graphreader.Edge{{
			EdgeSourceVID:    "cg:o7:wabc:r42:c123:s1:babc:route:seed",
			EdgeTargetVID:    "cg:o7:wabc:r42:c123:s1:babc:function:checkout",
			Depth:            1,
			Relation:         "CALLS",
			Confidence:       &conf,
			Source:           "tree-sitter-typescript",
			EvidenceFilePath: "src/routes/checkout.ts",
			StartLine:        &start,
			EdgeRepoID:       ptrInt32(42),
			EdgeRevision:     "refs/heads/main",
			Start:            graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:route:seed", RepoID: ptrInt32(42), Kind: "route", Label: "POST /checkout", Path: "src/routes/checkout.ts"},
			Neighbor:         graphreader.Endpoint{VID: "cg:o7:wabc:r42:c123:s1:babc:function:checkout", RepoID: ptrInt32(42), Kind: "function", Label: "checkout", Path: "src/routes/checkout.ts"},
		}},
	}}
	b := NewBackend(Config{
		Queries:              q,
		SearchBackend:        &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":1},"files":[{"fileName":{"text":"src/routes/checkout.ts"},"repository":"github.com/acme/orders","chunks":[{"content":"export function checkout() {}","contentStart":{"lineNumber":3},"matchRanges":[{}]}]}],"isSearchExhaustive":true}`)},
		GraphReader:          reader,
		EncryptionKey:        encKey,
		LanguageModelClient:  modelClient,
		AllowedModelBaseURLs: []string{"https://api.z.ai/api/coding/paas/v4"},
	})

	resp := callTool(t, b, 7, "ask_codebase", `{"query":"Explain checkout architecture flow across repos"}`)
	if toolIsError(t, resp) {
		t.Fatalf("ask_codebase returned tool error: %s", resp.Body)
	}
	if len(modelClient.calls) != 1 {
		t.Fatalf("model call count: got %d want 1", len(modelClient.calls))
	}
	modelReq := modelClient.calls[0]
	if len(modelReq.Tools) != 0 {
		t.Fatalf("architecture synthesis should not expose a model tool loop after mandatory retrieval, got %+v", modelReq.Tools)
	}
	hasPreloadedGraph := false
	for _, msg := range modelReq.Messages {
		if strings.Contains(msg.Content, "Mandatory fused retrieval evidence pack") && strings.Contains(msg.Content, "Architecture facts") {
			hasPreloadedGraph = true
		}
	}
	if !hasPreloadedGraph {
		t.Fatalf("architecture question should preload graph evidence, messages=%+v", modelReq.Messages)
	}
	if !strings.Contains(q.lastGraph.Query, "Explain checkout architecture flow across repos") {
		t.Fatalf("graph preflight query = %q", q.lastGraph.Query)
	}
	if reader.last.OrgID != 7 || len(reader.last.Seeds) != 1 {
		t.Fatalf("graph reader not called with tenant seeds: %+v", reader.last)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "`codegraph_context` ok") {
		t.Fatalf("tool trace should show preloaded graph:\n%s", text)
	}
}

func TestAskCodebaseBlocksToolsOutsideSelectedRepoScope(t *testing.T) {
	b := NewBackend(Config{})
	req := api.MCPRequest{OrgID: 7, OrgDomain: "orga"}

	out, isErr := b.executeAskTool(context.Background(), req, openAIToolCall{
		ID: "call_1",
		Function: openAIToolFunction{
			Name:      "read_file",
			Arguments: `{"repo":"github.com/acme/outside","path":"README.md"}`,
		},
	}, []string{"github.com/acme/inside"})
	if !isErr || !strings.Contains(out, "outside the selected repository scope") {
		t.Fatalf("outside repo should be blocked, isErr=%v out=%q", isErr, out)
	}

	out, isErr = b.executeAskTool(context.Background(), req, openAIToolCall{
		ID: "call_2",
		Function: openAIToolFunction{
			Name:      "grep",
			Arguments: `{"pattern":"handler","repos":["github.com/acme/inside","github.com/acme/outside"]}`,
		},
	}, []string{"github.com/acme/inside"})
	if !isErr || !strings.Contains(out, "outside the selected repository scope") {
		t.Fatalf("outside repo-set should be blocked, isErr=%v out=%q", isErr, out)
	}

	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "github.com/acme/inside",
		DefaultBranch: ptrString("main"),
		IndexedAt:     ptrTime(time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)),
		Metadata:      []byte(`{"indexedRevisions":["refs/heads/main"]}`),
	}}
	b = NewBackend(Config{Queries: q, SearchBackend: &fakeSearchBackend{body: json.RawMessage(`{"stats":{"actualMatchCount":0},"files":[],"isSearchExhaustive":true}`)}})
	out, isErr = b.executeAskTool(context.Background(), req, openAIToolCall{
		ID: "call_3",
		Function: openAIToolFunction{
			Name:      "find_symbol_definitions",
			Arguments: `{"symbol":"handleOrder"}`,
		},
	}, []string{"github.com/acme/inside"})
	if isErr {
		t.Fatalf("symbol tool should stay inside selected scope, out=%q", out)
	}
	if len(q.lastSymbol.RepoRevisionScopes) != 1 || q.lastSymbol.RepoRevisionScopes[0].Repo != "github.com/acme/inside" || q.lastSymbol.RepoRevisionScopes[0].RevisionCandidates[0] != "refs/heads/main" {
		t.Fatalf("symbol selected repo scope not applied: %+v", q.lastSymbol)
	}

	q.lastSymbol = db.FindOrgSymbolOccurrencesParams{}
	out, isErr = b.executeAskTool(context.Background(), req, openAIToolCall{
		ID: "call_4",
		Function: openAIToolFunction{
			Name:      "find_symbol_references",
			Arguments: `{"symbol":"handleOrder","repos":[]}`,
		},
	}, []string{"github.com/acme/inside"})
	if isErr {
		t.Fatalf("empty repos array should be replaced by selected scope, out=%q", out)
	}
	if len(q.lastSymbol.RepoRevisionScopes) != 1 || q.lastSymbol.RepoRevisionScopes[0].Repo != "github.com/acme/inside" {
		t.Fatalf("empty repos array bypassed selected symbol scope: %+v", q.lastSymbol)
	}

	q.graphEvidence = db.CodeGraphInspectionEvidence{
		SearchedRepoCount:   1,
		ActiveSnapshotCount: 1,
		WorkspaceIDs:        []string{"ws-1"},
	}
	reader := &fakeGraphReader{result: graphreader.InspectResult{
		Plan: graphreader.QueryPlan{Strategy: "bounded-nebula-bfs", Intent: "impact", Direction: "bidirectional", MaxDepth: 4},
	}}
	b = NewBackend(Config{Queries: q, GraphReader: reader})
	out, isErr = b.executeAskTool(context.Background(), req, openAIToolCall{
		ID: "call_5",
		Function: openAIToolFunction{
			Name:      "graph_impact",
			Arguments: `{"query":"handleOrder","repo":""}`,
		},
	}, []string{"github.com/acme/inside"})
	if isErr {
		t.Fatalf("empty repo string should be replaced by selected graph scope, out=%q", out)
	}
	if len(q.lastGraph.RepoRevisionScopes) != 1 || q.lastGraph.RepoRevisionScopes[0].Repo != "github.com/acme/inside" {
		t.Fatalf("empty repo string bypassed selected graph scope: %+v", q.lastGraph)
	}
}

func TestAskCodebaseRejectsPlaintextAndEnvCredentials(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{
			name:  "plaintext",
			model: `{"provider":"openai-compatible","model":"glm-4.6","baseUrl":"https://api.z.ai/api/coding/paas/v4","token":"raw-token"}`,
			want:  "language model credentials must use an org-scoped secretRef",
		},
		{
			name:  "env",
			model: `{"provider":"openai-compatible","model":"glm-4.6","baseUrl":"https://api.z.ai/api/coding/paas/v4","token":{"env":"CODEINTEL_DATABASE_URL"}}`,
			want:  "environment secret references are not allowed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &fakeQuerier{models: []db.OrgLanguageModelRow{{Config: []byte(tc.model)}}}
			b := NewBackend(Config{Queries: q, SearchBackend: &fakeSearchBackend{}, EncryptionKey: "0123456789abcdef0123456789abcdef"})
			resp := callTool(t, b, 7, "ask_codebase", `{"query":"test"}`)
			if !toolIsError(t, resp) || !strings.Contains(toolText(t, resp), tc.want) {
				t.Fatalf("credential config should be rejected with %q: %s", tc.want, resp.Body)
			}
		})
	}
}

func TestAskCodebaseToolErrorsUseProductPrefix(t *testing.T) {
	b := NewBackend(Config{Queries: &fakeQuerier{}})
	resp := callTool(t, b, 7, "ask_codebase", `{"query":"inspect missing repo","repos":["missing/repo"]}`)
	if !toolIsError(t, resp) {
		t.Fatalf("missing repo should be a tool error: %s", resp.Body)
	}
	text := toolText(t, resp)
	if !strings.Contains(text, "Failed to ask codebase:") || !strings.Contains(text, "not found") {
		t.Fatalf("ask_codebase error should keep product prefix and root cause: %s", text)
	}
}

func TestAskCodebaseListReposStaysInsideSelectedScope(t *testing.T) {
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "github.com/acme/inside",
		DefaultBranch: ptrString("main"),
		WebURL:        ptrString("https://github.com/acme/inside"),
	}}
	b := NewBackend(Config{Queries: q})
	req := api.MCPRequest{OrgID: 7, OrgDomain: "orga"}
	out, isErr := b.executeAskTool(context.Background(), req, openAIToolCall{
		ID: "call_1",
		Function: openAIToolFunction{
			Name:      "list_repos",
			Arguments: `{}`,
		},
	}, []string{"github.com/acme/inside"})
	if isErr || !strings.Contains(out, `"scoped":true`) || strings.Contains(out, "outside") {
		t.Fatalf("selected list_repos scope wrong, isErr=%v out=%q", isErr, out)
	}
}

func TestAskCodebaseSelectedListReposEmitsNullForBlankHeadlessURL(t *testing.T) {
	q := &fakeQuerier{readRepo: db.RepoReadRow{
		ID:            42,
		OrgID:         7,
		Name:          "github.com/acme/inside",
		DefaultBranch: ptrString("main"),
		WebURL:        ptrString(""),
	}}
	b := NewBackend(Config{Queries: q})
	req := api.MCPRequest{OrgID: 7, OrgDomain: "orga"}
	out, isErr := b.executeAskTool(context.Background(), req, openAIToolCall{
		ID: "call_1",
		Function: openAIToolFunction{
			Name:      "list_repos",
			Arguments: `{}`,
		},
	}, []string{"github.com/acme/inside"})
	if isErr || !strings.Contains(out, `"url":null`) {
		t.Fatalf("selected blank headless repo URL should serialize as null, isErr=%v out=%q", isErr, out)
	}
}

func callTool(t *testing.T, b *Backend, orgID int32, name, args string) api.MCPResponse {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":` + strconvQuote(name) + `,"arguments":` + args + `}}`
	resp, err := b.Handle(context.Background(), api.MCPRequest{
		OrgID:     orgID,
		OrgDomain: "orga",
		Method:    http.MethodPost,
		Body:      []byte(body),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d body=%s", resp.StatusCode, resp.Body)
	}
	return resp
}

func toolText(t *testing.T, resp api.MCPResponse) string {
	t.Helper()
	var decoded struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("decode tool response: %v\n%s", err, resp.Body)
	}
	if len(decoded.Result.Content) == 0 {
		t.Fatalf("no content in response: %s", resp.Body)
	}
	return decoded.Result.Content[0].Text
}

func toolIsError(t *testing.T, resp api.MCPResponse) bool {
	t.Helper()
	var decoded struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Body, &decoded); err != nil {
		t.Fatalf("decode tool response: %v\n%s", err, resp.Body)
	}
	return decoded.Result.IsError
}

func strconvQuote(s string) string {
	body, _ := json.Marshal(s)
	return string(body)
}

func makeGitRepo(t *testing.T, root string) string {
	t.Helper()
	dir, err := os.MkdirTemp(root, "repo-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hello\nsecond line\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	_, err = wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return dir
}

func makeTreeRepo(t *testing.T, root string) string {
	t.Helper()
	dir := makeGitRepo(t, root)
	if err := os.MkdirAll(filepath.Join(dir, "src", "lib"), 0o755); err != nil {
		t.Fatalf("MkdirAll src/lib: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatalf("MkdirAll docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "tenant.ts"), []byte(`export const tenantMarker = "tenant_one_secret_symbol";`+"\n"), 0o644); err != nil {
		t.Fatalf("write tenant: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "lib", "util.ts"), []byte("export const util = true;\n"), 0o644); err != nil {
		t.Fatalf("write util: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte("# Guide\n"), 0o644); err != nil {
		t.Fatalf("write guide: %v", err)
	}
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for _, path := range []string{"src/tenant.ts", "src/lib/util.ts", "docs/guide.md"} {
		if _, err := wt.Add(path); err != nil {
			t.Fatalf("Add %s: %v", path, err)
		}
	}
	if _, err := wt.Commit("tree files", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Date(2026, 5, 25, 2, 0, 0, 0, time.UTC),
		},
	}); err != nil {
		t.Fatalf("Commit tree files: %v", err)
	}
	return dir
}

func makeBranchRepo(t *testing.T, root string) string {
	t.Helper()
	dir := makeGitRepo(t, root)
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"),
		Create: true,
	}); err != nil {
		t.Fatalf("Checkout feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Hello\nfeature branch\n"), 0o644); err != nil {
		t.Fatalf("write README feature: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature work\n"), 0o644); err != nil {
		t.Fatalf("write feature file: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("Add README: %v", err)
	}
	if _, err := wt.Add("feature.txt"); err != nil {
		t.Fatalf("Add feature: %v", err)
	}
	if _, err := wt.Commit("feature work", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@example.com",
			When:  time.Date(2026, 5, 25, 1, 0, 0, 0, time.UTC),
		},
	}); err != nil {
		t.Fatalf("Commit feature: %v", err)
	}
	return dir
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

func ptrString(s string) *string {
	return &s
}

func ptrInt32(v int32) *int32 {
	return &v
}

func ptrFloat64(v float64) *float64 {
	return &v
}

func stringInSlice(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
