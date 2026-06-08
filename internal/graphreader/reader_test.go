package graphreader

import (
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestBuildQueryPlanImpactIsBidirectional(t *testing.T) {
	plan := BuildQueryPlan("impact blast radius for CheckoutController")
	if plan.Intent != "impact" {
		t.Fatalf("intent = %q", plan.Intent)
	}
	if plan.Direction != "bidirectional" {
		t.Fatalf("impact direction = %q, want bidirectional", plan.Direction)
	}
	if plan.MaxDepth < 4 {
		t.Fatalf("impact max depth = %d, want at least 4", plan.MaxDepth)
	}
}

func TestBuildQueryPlanPathUsesDeeperTraversal(t *testing.T) {
	plan := BuildQueryPlan("path from POST /orders to createOrder")
	if plan.Intent != "path" {
		t.Fatalf("intent = %q", plan.Intent)
	}
	if plan.MaxDepth < 5 {
		t.Fatalf("path max depth = %d, want at least 5", plan.MaxDepth)
	}
	if plan.Direction != "bidirectional" {
		t.Fatalf("path direction = %q, want bidirectional", plan.Direction)
	}
}

func TestBuildQueryPlanArchitectureIncludesImportEdges(t *testing.T) {
	plan := BuildQueryPlan("explain architecture flow from webhook to sdk injector")
	for _, want := range []string{"CALLS", "REFERENCES", "IMPORTS", "IMPORTS_FROM"} {
		if !stringSliceContains(plan.EdgeKinds, want) {
			t.Fatalf("architecture edge kinds missing %q: %+v", want, plan.EdgeKinds)
		}
	}
}

func TestApplyHubAwareFilterDoesNotDuplicateEdges(t *testing.T) {
	edges := []Edge{
		{EdgeSourceVID: "hub", EdgeTargetVID: "a", Depth: 1},
		{EdgeSourceVID: "hub", EdgeTargetVID: "b", Depth: 1},
		{EdgeSourceVID: "hub", EdgeTargetVID: "c", Depth: 2},
		{EdgeSourceVID: "d", EdgeTargetVID: "hub", Depth: 2},
	}
	got := applyHubAwareFilter(edges, 2)
	if len(got) != 3 {
		t.Fatalf("filtered edge count = %d, want 3: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, edge := range got {
		key := edge.EdgeSourceVID + "->" + edge.EdgeTargetVID
		if seen[key] {
			t.Fatalf("filtered result duplicated edge %s: %+v", key, got)
		}
		seen[key] = true
	}
}

func TestDiversifyEdgesByRequestedLanguagesInterleavesPolyglotResults(t *testing.T) {
	edges := []Edge{
		{EvidenceFilePath: "internal/instrumentation/python.go"},
		{EvidenceFilePath: "internal/instrumentation/python.go"},
		{EvidenceFilePath: "internal/instrumentation/python.go"},
		{EvidenceFilePath: "internal/instrumentation/nodejs.go"},
		{EvidenceFilePath: "internal/instrumentation/dotnet.go"},
	}
	got := diversifyEdgesByRequestedLanguages(edges, map[string]bool{
		"node":   true,
		"python": true,
		"dotnet": true,
	})
	if len(got) != len(edges) {
		t.Fatalf("edge count changed: got %d want %d", len(got), len(edges))
	}
	wantFirstPaths := []string{
		"internal/instrumentation/nodejs.go",
		"internal/instrumentation/python.go",
		"internal/instrumentation/dotnet.go",
	}
	for i, want := range wantFirstPaths {
		if got[i].EvidenceFilePath != want {
			t.Fatalf("edge %d path = %q want %q; got=%+v", i, got[i].EvidenceFilePath, want, got)
		}
	}
}

func TestEdgeMatchesAllowedScopesRequiresEndpointAndRevisionScope(t *testing.T) {
	repoA := int32(42)
	repoB := int32(99)
	scope := []ActiveScope{{
		RepoID:     repoA,
		Revision:   "refs/heads/main",
		CommitHash: "abc123",
	}}
	valid := Edge{
		EdgeRepoID:     &repoA,
		EdgeRevision:   "refs/heads/main",
		EdgeCommitHash: "abc123",
		Start:          Endpoint{RepoID: &repoA},
		Neighbor:       Endpoint{RepoID: &repoA},
	}
	if !edgeMatchesAllowedScopes(valid, scope) {
		t.Fatalf("valid scoped edge should match")
	}
	missingEndpoint := valid
	missingEndpoint.Neighbor.RepoID = nil
	if edgeMatchesAllowedScopes(missingEndpoint, scope) {
		t.Fatalf("edge with missing endpoint repo id must fail closed")
	}
	crossRepo := valid
	crossRepo.Neighbor.RepoID = &repoB
	if edgeMatchesAllowedScopes(crossRepo, scope) {
		t.Fatalf("edge crossing outside the allowed repo set must not match")
	}
	missingEdgeScope := valid
	missingEdgeScope.EdgeRepoID = nil
	if edgeMatchesAllowedScopes(missingEdgeScope, scope) {
		t.Fatalf("edge without scoped edge repo id must not match")
	}
	wrongRevision := valid
	wrongRevision.EdgeRevision = "refs/heads/feature"
	if edgeMatchesAllowedScopes(wrongRevision, scope) {
		t.Fatalf("edge from a different indexed revision must not match")
	}
	missingRevision := valid
	missingRevision.EdgeRevision = ""
	if edgeMatchesAllowedScopes(missingRevision, scope) {
		t.Fatalf("edge without revision must not match a revision-scoped request")
	}
	missingCommit := valid
	missingCommit.EdgeCommitHash = ""
	if edgeMatchesAllowedScopes(missingCommit, scope) {
		t.Fatalf("edge without commit must not match a commit-scoped request")
	}
}

func TestStrictInspectFailsWhenNoTenantScopedSeedsExist(t *testing.T) {
	reader := New(nil, nil)
	_, err := reader.Inspect(nil, InspectParams{
		OrgID:        7,
		Query:        "checkout flow",
		WorkspaceIDs: []string{"ws-1"},
		Strict:       true,
	})
	if err == nil || err.Error() != "NebulaGraph traversal skipped because the graph reader is not configured" {
		t.Fatalf("strict missing reader error = %v", err)
	}
}

// ── Q.B: edge context + hub-aware BFS ──────────────────────

func TestInferContextFiltersFiresOnCallHints(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  []string
	}{
		{"who calls", "who calls processOrder", []string{"call"}},
		{"caller noun", "find the caller of validateCart", []string{"call"}},
		{"invokes verb", "what invokes saveCheckout", []string{"call"}},
		{"implements", "what implements PaymentGateway", []string{"inherits"}},
		{"subclass", "subclasses of BaseHandler", []string{"inherits"}},
		{"return type", "return type of buildResponse", []string{"type"}},
		{"defined", "where is processOrder defined", []string{"definition", "reference"}},
		{"imports", "what imports lib/utils", []string{"import"}},
		{"uses dual context", "what uses fmt.Println", []string{"call", "reference"}},
		{"no hints", "checkout flow", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inferContextFilters(tc.query)
			if !stringSliceEqual(got, tc.want) {
				t.Errorf("inferContextFilters(%q): got %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

func TestBuildQueryPlanPopulatesContextFilters(t *testing.T) {
	plan := BuildQueryPlan("Who calls processOrder?")
	if len(plan.ContextFilters) != 1 || plan.ContextFilters[0] != "call" {
		t.Fatalf("context filter for call-intent: got %v, want [call]", plan.ContextFilters)
	}
}

func TestBuildQueryPlanNoContextFiltersForUnknownQuery(t *testing.T) {
	plan := BuildQueryPlan("checkout flow overview")
	if len(plan.ContextFilters) != 0 {
		t.Fatalf("expected no context filters, got %v", plan.ContextFilters)
	}
}

func TestTraversalBudgetDefaultsPreserveFullInspectBreadth(t *testing.T) {
	budget := traversalBudgetFromInspectParams(InspectParams{}, 16)
	if budget.MaxSeedTokens != 8 {
		t.Fatalf("default seed tokens = %d, want 8", budget.MaxSeedTokens)
	}
	if budget.SeedRows != 16 {
		t.Fatalf("default seed rows = %d, want 16", budget.SeedRows)
	}
	if budget.SeedVIDs != 64 {
		t.Fatalf("default seed vids = %d, want 64", budget.SeedVIDs)
	}
	if budget.TraversalRows != 100 {
		t.Fatalf("default traversal rows = %d, want 100", budget.TraversalRows)
	}

	stmt := renderTraversalStepStatement(7, "ws-1", []string{"a", "b"}, 16, BuildQueryPlan("architecture flow"), nil, 1, budget)
	if !strings.Contains(stmt, "| LIMIT 100;") {
		t.Fatalf("full inspect traversal limit changed:\n%s", stmt)
	}
}

func TestTraversalBudgetCanNarrowCompactInspect(t *testing.T) {
	budget := traversalBudgetFromInspectParams(InspectParams{
		MaxSeedTokens: 5,
		SeedRowLimit:  8,
		SeedVIDLimit:  24,
		TraversalRows: 48,
	}, 16)
	if budget.MaxSeedTokens != 5 || budget.SeedRows != 8 || budget.SeedVIDs != 24 || budget.TraversalRows != 48 {
		t.Fatalf("compact budget = %+v, want 5/8/24/48", budget)
	}

	seeds := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		seeds = append(seeds, "seed-"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	stmt := renderTraversalStepStatement(7, "ws-1", seeds, 16, BuildQueryPlan("architecture flow"), nil, 1, budget)
	if !strings.Contains(stmt, "| LIMIT 48;") {
		t.Fatalf("compact traversal limit was not applied:\n%s", stmt)
	}
	if strings.Contains(stmt, `"seed-ya"`) {
		t.Fatalf("compact seed VID budget should truncate seed list before later seeds:\n%s", stmt)
	}
}

func TestGraphReaderCacheKeyIsStableForEquivalentScopes(t *testing.T) {
	paramsA := InspectParams{
		OrgID:        7,
		Query:        "checkout flow",
		WorkspaceIDs: []string{"ws-b", "ws-a", "ws-a"},
		Seeds: []Seed{
			{WorkspaceID: "ws-b", NodeVID: "b"},
			{WorkspaceID: "ws-a", NodeVID: "a"},
		},
		AllowedScopes: []ActiveScope{
			{RepoID: 2, Revision: "main", CommitHash: "def", SchemaVersion: 1, BuilderVersion: "v1"},
			{RepoID: 1, Revision: "main", CommitHash: "abc", SchemaVersion: 1, BuilderVersion: "v1"},
		},
		Limit:         16,
		MaxDepth:      1,
		Strict:        true,
		MaxSeedTokens: 5,
		SeedRowLimit:  8,
		SeedVIDLimit:  24,
		TraversalRows: 48,
	}
	paramsB := paramsA
	paramsB.WorkspaceIDs = []string{"ws-a", "ws-b"}
	paramsB.Seeds = []Seed{
		{WorkspaceID: "ws-a", NodeVID: "a"},
		{WorkspaceID: "ws-b", NodeVID: "b"},
	}
	paramsB.AllowedScopes = []ActiveScope{
		{RepoID: 1, Revision: "main", CommitHash: "abc", SchemaVersion: 1, BuilderVersion: "v1"},
		{RepoID: 2, Revision: "main", CommitHash: "def", SchemaVersion: 1, BuilderVersion: "v1"},
	}

	keyA, err := graphReaderCacheKey(paramsA)
	if err != nil {
		t.Fatalf("cache key A: %v", err)
	}
	keyB, err := graphReaderCacheKey(paramsB)
	if err != nil {
		t.Fatalf("cache key B: %v", err)
	}
	if keyA != keyB {
		t.Fatalf("equivalent params produced different keys:\n%s\n%s", keyA, keyB)
	}
	paramsB.Query = "different flow"
	keyC, err := graphReaderCacheKey(paramsB)
	if err != nil {
		t.Fatalf("cache key C: %v", err)
	}
	if keyA == keyC {
		t.Fatalf("different query produced same key: %s", keyA)
	}
}

func TestGraphReaderCacheKeySeparatesAllowedScopeWorkspace(t *testing.T) {
	paramsA := InspectParams{
		OrgID:        7,
		Query:        "checkout flow",
		WorkspaceIDs: []string{"ws-a"},
		AllowedScopes: []ActiveScope{{
			WorkspaceID:    "ws-a",
			RepoID:         1,
			Revision:       "refs/heads/main",
			CommitHash:     "abc",
			SchemaVersion:  1,
			BuilderVersion: "v1",
		}},
		Limit: 8,
	}
	paramsB := paramsA
	paramsB.WorkspaceIDs = []string{"ws-b"}
	paramsB.AllowedScopes = []ActiveScope{{
		WorkspaceID:    "ws-b",
		RepoID:         1,
		Revision:       "refs/heads/main",
		CommitHash:     "abc",
		SchemaVersion:  1,
		BuilderVersion: "v1",
	}}
	keyA, err := graphReaderCacheKey(paramsA)
	if err != nil {
		t.Fatalf("cache key A: %v", err)
	}
	keyB, err := graphReaderCacheKey(paramsB)
	if err != nil {
		t.Fatalf("cache key B: %v", err)
	}
	if keyA == keyB {
		t.Fatalf("workspace-specific allowed scopes must not share a graphreader cache key: %s", keyA)
	}
}

func TestGraphReaderCacheUsesConfigurablePayloadCap(t *testing.T) {
	redisClient := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	defer redisClient.Close()

	inspector := NewCachedWithMaxBytes(New(nil, nil), redisClient, time.Minute, 1234, nil)
	cached, ok := inspector.(*CachedInspector)
	if !ok {
		t.Fatalf("NewCachedWithMaxBytes did not return CachedInspector: %T", inspector)
	}
	if cached.maxBytes != 1234 {
		t.Fatalf("cache maxBytes = %d, want 1234", cached.maxBytes)
	}

	inspector = NewCachedWithMaxBytes(New(nil, nil), redisClient, time.Minute, 0, nil)
	cached, ok = inspector.(*CachedInspector)
	if !ok {
		t.Fatalf("NewCachedWithMaxBytes with fallback did not return CachedInspector: %T", inspector)
	}
	if cached.maxBytes != defaultMaxGraphReaderCacheBytes {
		t.Fatalf("fallback cache maxBytes = %d, want %d", cached.maxBytes, defaultMaxGraphReaderCacheBytes)
	}
}

func TestSearchableTokensExpandsInstrumentationQueries(t *testing.T) {
	got := searchableTokens("Explain the OpenTelemetry auto-instrumentation flow for Python, Node.js, and .NET workloads", 8)
	for _, want := range []string{"inject", "injectNodeJS", "injectPython", "injectDotNet"} {
		if !stringSliceContains(got, want) {
			t.Fatalf("searchableTokens missing focused term %q: %+v", want, got)
		}
	}
}

func TestSearchableTokensKeepsSingleRuntimeInstrumentationFocused(t *testing.T) {
	got := searchableTokens("Explain Python auto-instrumentation injection into pod containers", 12)
	if !stringSliceContains(got, "injectPython") {
		t.Fatalf("Python instrumentation query should include injectPython: %+v", got)
	}
	for _, unexpected := range []string{"injectNodeJS", "injectDotNet"} {
		if stringSliceContains(got, unexpected) {
			t.Fatalf("Python-only instrumentation query should not include %q: %+v", unexpected, got)
		}
	}
}

func TestRankEdgesPrefersScipAndFocusedCodeOverImportNoise(t *testing.T) {
	confScip := 0.95
	confHeuristic := 0.60
	edges := []Edge{
		{
			Relation:   "IMPORTS_FROM",
			Source:     "heuristic",
			Provenance: "heuristic",
			Confidence: &confHeuristic,
			Start:      Endpoint{Kind: "package", Label: "cmd/operator-opamp-bridge"},
			Neighbor:   Endpoint{Kind: "package", Label: "github.com/open-telemetry/opentelemetry-operator/apis/v1alpha1"},
		},
		{
			Relation:         "REFERENCES",
			Source:           "scip",
			Provenance:       "scip",
			Confidence:       &confScip,
			EvidenceFilePath: "internal/instrumentation/sdk.go",
			Start:            Endpoint{Kind: "function", Label: "injectPython", Path: "internal/instrumentation/sdk.go"},
			Neighbor:         Endpoint{Kind: "class", Label: "sdkInjector", Path: "internal/instrumentation/sdk.go"},
		},
	}
	rankEdges(edges, []string{"inject", "instrumentation", "Python"})
	if edges[0].Relation != "REFERENCES" || edges[0].Provenance != "scip" {
		t.Fatalf("expected SCIP focused edge before import noise: %+v", edges)
	}
}

func TestRankEdgesTieBreaksDeterministically(t *testing.T) {
	conf := 0.95
	edges := []Edge{
		{
			Relation:         "CALLS",
			Source:           "scip",
			Provenance:       "scip",
			Confidence:       &conf,
			EvidenceFilePath: "b.go",
			EdgeSourceVID:    "b",
			EdgeTargetVID:    "c",
			Start:            Endpoint{Kind: "function", Label: "B"},
			Neighbor:         Endpoint{Kind: "function", Label: "C"},
		},
		{
			Relation:         "CALLS",
			Source:           "scip",
			Provenance:       "scip",
			Confidence:       &conf,
			EvidenceFilePath: "a.go",
			EdgeSourceVID:    "a",
			EdgeTargetVID:    "c",
			Start:            Endpoint{Kind: "function", Label: "A"},
			Neighbor:         Endpoint{Kind: "function", Label: "C"},
		},
	}
	rankEdges(edges, []string{"flow"})
	if edges[0].EvidenceFilePath != "a.go" {
		t.Fatalf("tie-break should be stable by edge identity, got %+v", edges)
	}
}

func TestRankEdgesDemotesTestAndUtilityNoise(t *testing.T) {
	conf := 0.60
	edges := []Edge{
		{
			Relation:         "CALLS",
			Source:           "ast-go",
			Provenance:       "heuristic",
			Confidence:       &conf,
			EvidenceFilePath: "internal/instrumentation/python_test.go",
			Start:            Endpoint{Kind: "function", Label: "TestInjectPythonSDK", Path: "internal/instrumentation/python_test.go"},
			Neighbor:         Endpoint{Kind: "function", Label: "assert.Equal", Path: "apis/v1alpha1/convert_test.go"},
		},
		{
			Relation:         "CALLS",
			Source:           "ast-go",
			Provenance:       "heuristic",
			Confidence:       &conf,
			EvidenceFilePath: "internal/instrumentation/sdk.go",
			Start:            Endpoint{Kind: "function", Label: "injectPython", Path: "internal/instrumentation/sdk.go"},
			Neighbor:         Endpoint{Kind: "function", Label: "injectPythonSDKToContainer", Path: "internal/instrumentation/python.go"},
		},
	}
	rankEdges(edges, []string{"inject", "Python"})
	if edges[0].Start.Label != "injectPython" {
		t.Fatalf("implementation edge should outrank test/assertion noise: %+v", edges)
	}
}

func TestRankEdgesDemotesUnrequestedInstrumentationFamily(t *testing.T) {
	conf := 0.95
	edges := []Edge{
		{
			Relation:         "REFERENCES",
			Source:           "scip",
			Provenance:       "scip",
			Confidence:       &conf,
			EvidenceFilePath: "internal/instrumentation/annotation.go",
			Start:            Endpoint{Kind: "function", Label: "Mutate", Path: "internal/instrumentation/podmutator.go"},
			Neighbor:         Endpoint{Kind: "constant", Label: "annotationInjectApacheHttpd", Path: "internal/instrumentation/annotation.go"},
		},
		{
			Relation:         "CALLS",
			Source:           "scip",
			Provenance:       "scip",
			Confidence:       &conf,
			EvidenceFilePath: "internal/instrumentation/sdk.go",
			Start:            Endpoint{Kind: "method", Label: "sdkInjector.injectNodeJS", Path: "internal/instrumentation/sdk.go"},
			Neighbor:         Endpoint{Kind: "function", Label: "injectCommonSDKConfig", Path: "internal/instrumentation/sdk.go"},
		},
	}
	rankEdges(edges, []string{"auto-instrumentation", "NodeJS", "Python", "DotNet", "injectNodeJS", "injectPython", "injectDotNet"})
	if edges[0].Start.Label != "sdkInjector.injectNodeJS" {
		t.Fatalf("requested language edge should outrank precise but irrelevant Apache edge: %+v", edges)
	}
}

func TestRankEdgesKeepsCoreArchitectureFlowAboveUnrequestedFamily(t *testing.T) {
	conf := 0.95
	edges := []Edge{
		{
			Relation:         "REFERENCES",
			Source:           "scip",
			Provenance:       "scip",
			Confidence:       &conf,
			EvidenceFilePath: "internal/instrumentation/annotation.go",
			Start:            Endpoint{Kind: "function", Label: "Mutate", Path: "internal/instrumentation/podmutator.go"},
			Neighbor:         Endpoint{Kind: "constant", Label: "annotationInjectApacheHttpd", Path: "internal/instrumentation/annotation.go"},
		},
		{
			Relation:         "CALLS",
			Source:           "tree-sitter-go",
			Provenance:       "heuristic",
			Confidence:       &conf,
			EvidenceFilePath: "internal/instrumentation/podmutator.go",
			Start:            Endpoint{Kind: "method", Label: "instPodMutator.Mutate", Path: "internal/instrumentation/podmutator.go"},
			Neighbor:         Endpoint{Kind: "method", Label: "sdkInjector.inject", Path: "internal/instrumentation/sdk.go"},
		},
	}
	rankEdges(edges, []string{"architecture", "flow", "NodeJS", "Python", "DotNet", "sdkInjector", "Mutate"})
	if edges[0].Neighbor.Label != "sdkInjector.inject" {
		t.Fatalf("core architecture flow should outrank unrequested Apache precision edge: %+v", edges)
	}
}

func TestAlignTraversalEndpointsPreservesStoredEdgeDirection(t *testing.T) {
	source := Endpoint{VID: "caller", Label: "callerFn", Path: "caller.go"}
	target := Endpoint{VID: "callee", Label: "calleeFn", Path: "callee.go"}
	gotSource, gotTarget := alignTraversalEndpoints("caller", "callee", target, source)
	if gotSource.Label != "callerFn" || gotSource.Path != "caller.go" {
		t.Fatalf("source endpoint not aligned to stored src(edge): %+v", gotSource)
	}
	if gotTarget.Label != "calleeFn" || gotTarget.Path != "callee.go" {
		t.Fatalf("target endpoint not aligned to stored dst(edge): %+v", gotTarget)
	}
}

func TestRenderContextFilterEmpty(t *testing.T) {
	if got := renderContextFilter(nil); got != "" {
		t.Errorf("nil contexts should produce empty filter, got %q", got)
	}
	if got := renderContextFilter([]string{}); got != "" {
		t.Errorf("empty contexts should produce empty filter, got %q", got)
	}
}

func TestRenderContextFilterSingleAndMultiple(t *testing.T) {
	if got := renderContextFilter([]string{"call"}); got != `properties(edge).context IN ["call"]` {
		t.Errorf("single context filter: got %q", got)
	}
	if got := renderContextFilter([]string{"call", "reference"}); got != `properties(edge).context IN ["call", "reference"]` {
		t.Errorf("multi context filter: got %q", got)
	}
}

func TestApplyHubAwareFilterSkipsTinyResultSets(t *testing.T) {
	// Result sets under 4 edges short-circuit: no meaningful
	// p99 distribution.
	edges := []Edge{
		{EdgeSourceVID: "a", EdgeTargetVID: "b", Depth: 2},
		{EdgeSourceVID: "b", EdgeTargetVID: "c", Depth: 3},
	}
	if got := applyHubAwareFilter(edges, hubDegreeFloor); len(got) != 2 {
		t.Errorf("tiny result set: got %d edges, want 2", len(got))
	}
}

func TestApplyHubAwareFilterDropsTransitOutOfHub(t *testing.T) {
	// Construct a hub vertex `H` that receives 60 inbound edges
	// (above the floor of 50). One depth-1 edge into H and
	// then a depth-2 edge OUT of H. The transit edge must be
	// dropped; the depth-1 edge into H must survive.
	edges := []Edge{}
	for i := 0; i < 60; i++ {
		edges = append(edges, Edge{
			EdgeSourceVID: "src-" + itoa(i),
			EdgeTargetVID: "H",
			Depth:         1,
			Relation:      "CALLS",
		})
	}
	// One transit edge OUT of H at depth 2.
	edges = append(edges, Edge{
		EdgeSourceVID: "H",
		EdgeTargetVID: "child",
		Depth:         2,
		Relation:      "CALLS",
	})
	got := applyHubAwareFilter(edges, hubDegreeFloor)
	// The 60 inbound edges should survive; the 1 outbound transit
	// edge should be dropped → 60 edges remain.
	if len(got) != 60 {
		t.Errorf("expected 60 edges after hub filter, got %d", len(got))
	}
	for _, e := range got {
		if e.EdgeSourceVID == "H" && e.Depth > 1 {
			t.Errorf("transit-out-of-hub edge survived filter: %+v", e)
		}
	}
}

func TestApplyHubAwareFilterDoesNotDropDepth1FromHub(t *testing.T) {
	// Hub at depth 1 (seed itself) — outbound edges at depth 1
	// must survive (the user asked about the hub explicitly).
	edges := []Edge{}
	for i := 0; i < 60; i++ {
		edges = append(edges, Edge{
			EdgeSourceVID: "H",
			EdgeTargetVID: "child-" + itoa(i),
			Depth:         1,
			Relation:      "CALLS",
		})
	}
	got := applyHubAwareFilter(edges, hubDegreeFloor)
	if len(got) != 60 {
		t.Errorf("hub-as-seed depth-1 edges should not be dropped: got %d of 60", len(got))
	}
}

func TestComputeHubThresholdHonoursFloor(t *testing.T) {
	// Small distribution: every vertex degree 1. p99 = 1; floor=50.
	// Threshold must be 50 not 1.
	degree := map[string]int{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1}
	if got := computeHubThreshold(degree, 50); got != 50 {
		t.Errorf("expected floor=50, got %d", got)
	}
}

func TestComputeHubThresholdPicksP99WhenAboveFloor(t *testing.T) {
	// Build a distribution where p99 is clearly above the floor.
	degree := make(map[string]int, 200)
	for i := 0; i < 199; i++ {
		degree["v-"+itoa(i)] = 1
	}
	degree["hub"] = 500
	if got := computeHubThreshold(degree, 50); got < 50 {
		t.Errorf("p99 threshold should honour floor: got %d", got)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for n := i; n > 0; n /= 10 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
	}
	return string(digits)
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
