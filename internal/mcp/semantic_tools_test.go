package mcp

import (
	"strings"
	"testing"

	"codeintel/internal/db"
	"codeintel/internal/graphreader"
)

func TestGraphSeedsFromEvidenceIncludesSCIPSymbolVertices(t *testing.T) {
	scope := db.CodeGraphActiveScope{
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "abcdef1234567890",
		WorkspaceID:    "ws-1",
		SchemaVersion:  1,
		BuilderVersion: "codeintel-code-graph-v7",
	}
	evidence := db.CodeGraphInspectionEvidence{
		OrgID:        7,
		ActiveScopes: []db.CodeGraphActiveScope{scope},
		Symbols: []db.CodeGraphSymbolEvidence{{
			RepoID:     42,
			Symbol:     "scip-go github.com/acme/orders internal/orders/create.go/createOrder().",
			Revision:   "refs/heads/main",
			CommitHash: "abcdef1234567890",
		}},
		Relationships: []db.CodeGraphRelationshipEvidence{{
			RepoID:       42,
			SourceSymbol: "scip-go github.com/acme/orders internal/routes/orders.go/route().",
			TargetSymbol: "scip-go github.com/acme/orders internal/orders/create.go/createOrder().",
			Revision:     "refs/heads/main",
			CommitHash:   "abcdef1234567890",
		}},
	}
	got := graphSeedsFromEvidence(evidence)
	expected := map[string]bool{
		mcpCodeGraphVID(7, scope, "symbol", "scip-go github.com/acme/orders internal/orders/create.go/createOrder()."): true,
		mcpCodeGraphVID(7, scope, "symbol", "scip-go github.com/acme/orders internal/routes/orders.go/route()."):       true,
	}
	if len(got) != len(expected) {
		t.Fatalf("seed count = %d want %d: %#v", len(got), len(expected), got)
	}
	for _, seed := range got {
		if seed.WorkspaceID != "ws-1" {
			t.Fatalf("workspace = %q want ws-1", seed.WorkspaceID)
		}
		if !expected[seed.NodeVID] {
			t.Fatalf("unexpected seed VID %q", seed.NodeVID)
		}
	}
}

func TestCompactGraphTraversalSeedsCapsWorkspacesAndSeeds(t *testing.T) {
	seeds := []graphreader.Seed{
		{WorkspaceID: "ws-a", NodeVID: "a1"},
		{WorkspaceID: "ws-a", NodeVID: "a2"},
		{WorkspaceID: "ws-a", NodeVID: "a3"},
		{WorkspaceID: "ws-b", NodeVID: "b1"},
		{WorkspaceID: "ws-b", NodeVID: "b2"},
		{WorkspaceID: "ws-c", NodeVID: "c1"},
		{WorkspaceID: "ws-a", NodeVID: "a2"},
		{WorkspaceID: "ws-d", NodeVID: "d1"},
	}
	got := compactGraphTraversalSeeds(seeds, 2, 2)
	want := []graphreader.Seed{
		{WorkspaceID: "ws-a", NodeVID: "a1"},
		{WorkspaceID: "ws-a", NodeVID: "a2"},
		{WorkspaceID: "ws-b", NodeVID: "b1"},
		{WorkspaceID: "ws-b", NodeVID: "b2"},
	}
	if len(got) != len(want) {
		t.Fatalf("compact seed count = %d want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seed %d = %#v want %#v; all=%#v", i, got[i], want[i], got)
		}
	}
}

func TestGraphSeedsFromEvidencePrioritizesSemanticFlowEdges(t *testing.T) {
	scope := db.CodeGraphActiveScope{
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "abcdef1234567890",
		WorkspaceID:    "ws-1",
		SchemaVersion:  1,
		BuilderVersion: "codeintel-code-graph-v7",
	}
	lowKind := "Package"
	lowPath := "vendor/logger/log.go"
	highSource := mcpCodeGraphVID(7, scope, "symbol", "scip-go github.com/acme/orders internal/webhook/handler.go/Handle().")
	highTarget := mcpCodeGraphVID(7, scope, "symbol", "scip-go github.com/acme/orders internal/instrumentation/podmutator.go/Mutate().")
	evidence := db.CodeGraphInspectionEvidence{
		OrgID:        7,
		ActiveScopes: []db.CodeGraphActiveScope{scope},
		Symbols: []db.CodeGraphSymbolEvidence{{
			RepoID:     42,
			Symbol:     "scip-go github.com/acme/orders vendor/logger/log.go/Logger#",
			Kind:       &lowKind,
			FilePath:   &lowPath,
			Revision:   "refs/heads/main",
			CommitHash: "abcdef1234567890",
		}},
		SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
			RepoID:           42,
			WorkspaceID:      "ws-1",
			SourceExternalID: highSource,
			TargetExternalID: highTarget,
			Relation:         "CALLS",
			SourceFile:       "internal/webhook/handler.go",
			Confidence:       0.95,
			Source:           "scip",
			Revision:         "refs/heads/main",
			CommitHash:       "abcdef1234567890",
		}},
	}
	got := graphSeedsFromEvidence(evidence)
	if len(got) < 2 {
		t.Fatalf("seed count = %d want at least 2: %#v", len(got), got)
	}
	if got[0].NodeVID != highSource || got[1].NodeVID != highTarget {
		t.Fatalf("semantic flow seeds should come first: %#v", got[:2])
	}
}

func TestGraphSeedsFromEvidenceRejectsForeignOrgVIDs(t *testing.T) {
	evidence := db.CodeGraphInspectionEvidence{
		OrgID: 7,
		SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
			RepoID:           42,
			WorkspaceID:      "ws-1",
			SourceExternalID: "cg:o8:r42:main:symbol:foreign",
			TargetExternalID: "cg:o8:r42:main:symbol:foreign-target",
			Relation:         "CALLS",
			SourceFile:       "internal/orders/create.go",
			Confidence:       0.95,
			Source:           "scip",
			Revision:         "refs/heads/main",
			CommitHash:       "abcdef1234567890",
		}},
	}
	got := graphSeedsFromEvidence(evidence)
	if len(got) != 0 {
		t.Fatalf("foreign-org graph seeds must be rejected, got %#v", got)
	}
}

func TestGraphSeedsFromEvidenceUsesCompactFallbackSemanticEdges(t *testing.T) {
	sourceVID := "cg:o7:wabc:r42:cmain:s1:babc:file:tenant"
	targetVID := "cg:o7:wabc:r42:cmain:s1:babc:symbol:tenantMarker"
	evidence := db.CodeGraphInspectionEvidence{
		OrgID: 7,
		Query: "tenant_one_secret_symbol",
		SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{{
			RepoID:           42,
			WorkspaceID:      "ws-1",
			SourceExternalID: sourceVID,
			TargetExternalID: targetVID,
			Relation:         "DEFINES",
			SourceFile:       "src/tenant.ts",
			Confidence:       0.95,
			ConfidenceTier:   "EXTRACTED",
			Source:           "ast-typescript",
			Revision:         "refs/heads/release-a",
			CommitHash:       "abc123",
			GraphIndexID:     "graph-1",
		}},
	}
	got := graphSeedsFromEvidence(evidence)
	if len(got) < 2 {
		t.Fatalf("fallback semantic edge should produce source and target seeds, got %#v", got)
	}
	want := map[string]bool{sourceVID: true, targetVID: true}
	for _, seed := range got[:2] {
		if seed.WorkspaceID != "ws-1" {
			t.Fatalf("workspace = %q want ws-1", seed.WorkspaceID)
		}
		if !want[seed.NodeVID] {
			t.Fatalf("unexpected seed %q from compact fallback evidence; all=%#v", seed.NodeVID, got)
		}
	}
}

func TestGraphSeedsFromEvidenceDiversifiesRequestedPolyglotBuckets(t *testing.T) {
	scope := db.CodeGraphActiveScope{
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "abcdef1234567890",
		WorkspaceID:    "ws-1",
		SchemaVersion:  1,
		BuilderVersion: "codeintel-code-graph-v7",
	}
	mkEdge := func(symbol, path, evidence string) db.CodeGraphSemanticEdgeEvidence {
		return db.CodeGraphSemanticEdgeEvidence{
			RepoID:           42,
			WorkspaceID:      "ws-1",
			SourceExternalID: mcpCodeGraphVID(7, scope, "symbol", "scip-go github.com/acme/orders "+path+"/"+symbol+"()."),
			TargetExternalID: mcpCodeGraphVID(7, scope, "symbol", "scip-go github.com/acme/orders internal/instrumentation/sdk.go/sdkInjector#"+symbol+"()."),
			Relation:         "CALLS",
			SourceFile:       path,
			Evidence:         &evidence,
			Confidence:       0.95,
			Source:           "scip",
			Revision:         "refs/heads/main",
			CommitHash:       "abcdef1234567890",
		}
	}
	evidence := db.CodeGraphInspectionEvidence{
		OrgID:        7,
		Query:        "Explain the NodeJS, Python, and .NET auto instrumentation flow",
		ActiveScopes: []db.CodeGraphActiveScope{scope},
		SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{
			mkEdge("injectPython", "internal/instrumentation/python.go", "injectPython CALLS injectPythonSDKToContainer"),
			mkEdge("injectPythonSDK", "internal/instrumentation/python.go", "injectPythonSDK CALLS injectPythonSDKToContainer"),
			mkEdge("injectNodeJS", "internal/instrumentation/nodejs.go", "injectNodeJS CALLS injectNodeJSSDKToContainer"),
			mkEdge("injectDotNet", "internal/instrumentation/dotnet.go", "injectDotNet CALLS injectDotNetSDKToContainer"),
		},
	}
	got := graphSeedsFromEvidence(evidence)
	if len(got) < 6 {
		t.Fatalf("seed count = %d want at least 6: %#v", len(got), got)
	}
	firstThree := got[:3]
	wantSources := []string{
		evidence.SemanticEdges[2].SourceExternalID,
		evidence.SemanticEdges[0].SourceExternalID,
		evidence.SemanticEdges[3].SourceExternalID,
	}
	for i, want := range wantSources {
		if firstThree[i].NodeVID != want {
			t.Fatalf("polyglot source seed %d = %q want %q; first seeds=%#v", i, firstThree[i].NodeVID, want, firstThree)
		}
	}
}

func TestGraphSeedsFromEvidenceDemotesUnrequestedRuntimeFamily(t *testing.T) {
	scope := db.CodeGraphActiveScope{
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "abcdef1234567890",
		WorkspaceID:    "ws-1",
		SchemaVersion:  1,
		BuilderVersion: "codeintel-code-graph-v7",
	}
	mkEdge := func(symbol, path, evidence string) db.CodeGraphSemanticEdgeEvidence {
		return db.CodeGraphSemanticEdgeEvidence{
			RepoID:           42,
			WorkspaceID:      "ws-1",
			SourceExternalID: mcpCodeGraphVID(7, scope, "symbol", "scip-go github.com/acme/operator "+path+"/"+symbol+"()."),
			TargetExternalID: mcpCodeGraphVID(7, scope, "symbol", "scip-go github.com/acme/operator internal/instrumentation/annotation.go/"+evidence+"()."),
			Relation:         "REFERENCES",
			SourceFile:       path,
			Evidence:         &evidence,
			Confidence:       0.95,
			Source:           "scip",
			Revision:         "refs/heads/main",
			CommitHash:       "abcdef1234567890",
		}
	}
	evidence := db.CodeGraphInspectionEvidence{
		OrgID:        7,
		Query:        "Explain the NodeJS, Python, and .NET auto instrumentation flow",
		ActiveScopes: []db.CodeGraphActiveScope{scope},
		SemanticEdges: []db.CodeGraphSemanticEdgeEvidence{
			mkEdge("Mutate", "internal/instrumentation/podmutator.go", "annotationInjectApacheHttpd"),
			mkEdge("injectNodeJS", "internal/instrumentation/sdk.go", "injectNodeJS references injectCommonSDKConfig"),
		},
	}
	got := graphSeedsFromEvidence(evidence)
	if len(got) < 2 {
		t.Fatalf("seed count = %d want at least 2: %#v", len(got), got)
	}
	if got[0].NodeVID != evidence.SemanticEdges[1].SourceExternalID {
		t.Fatalf("requested NodeJS seed should outrank precise Apache seed: %#v", got[:2])
	}
}

func TestCodegraphContextCompactGraphOutputPreservesSCIPProvenance(t *testing.T) {
	output := strings.Join([]string{
		`Graph minimal context for "inject flow"`,
		"Shared NebulaGraph read: yes",
		"",
		`Minimal graph development context for "inject flow"`,
		"- [repoId:1051] symbol injectNodeJS at internal/instrumentation/sdk.go --REFERENCES--> [repoId:1051] symbol nodejsInitContainerName at internal/instrumentation/nodejs.go @ internal/instrumentation/helper.go:39",
		"  source=scip; provenance=scip; confidence=0.95",
		"- [repoId:1051] function noisyHelper --CALLS--> [repoId:1051] function fmt.Println @ internal/noise.go:10",
		"  source=ast-go; provenance=heuristic; confidence=0.60",
	}, "\n")
	got := codegraphContextCompactGraphOutput(output, 6, 1000)
	for _, want := range []string{
		"injectNodeJS",
		"source=scip",
		"provenance=scip",
		"confidence=0.95",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("compact graph output missing %q:\n%s", want, got)
		}
	}
}

func TestCodegraphContextCriticalManifestIncludesSCIPGraphProof(t *testing.T) {
	output := strings.Join([]string{
		`Graph minimal context for "inject flow"`,
		"Shared NebulaGraph read: yes",
		"",
		"High-signal connected flow (native NebulaGraph traversal edges):",
		"- [repoId:1051] symbol injectDotNet at internal/instrumentation/sdk.go --REFERENCES--> [repoId:1051] symbol dotnetVolumeName at internal/instrumentation/dotnet.go @ internal/instrumentation/dotnet.go:43",
		"  source=scip; provenance=scip; confidence=0.95",
	}, "\n")
	got := codegraphContextCriticalManifest("inject DotNet flow", []layer{{
		Title:  "Graph minimal context",
		Tool:   "graph_minimal_context",
		Output: output,
	}, {
		Title:  "SCIP definitions for injectDotNet",
		Tool:   "find_symbol_definitions",
		Output: "Found 1 precise SCIP definitions and supplemental Zoekt text matches",
	}}, nil)
	for _, want := range []string{
		"Native/high-signal graph traversal evidence is present",
		"Graph proof lines:",
		"injectDotNet",
		"source=scip; provenance=scip; confidence=0.95",
		"Precise SCIP evidence is present",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("manifest missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "- - ") {
		t.Fatalf("manifest should not double-prefix graph proof bullets:\n%s", got)
	}
	if strings.Contains(got, "heuristic/low-confidence AST traversal only") {
		t.Fatalf("manifest incorrectly labels SCIP graph proof as heuristic-only:\n%s", got)
	}
}

func TestCodegraphContextCriticalManifestRanksRequestedRuntimeProofOverApache(t *testing.T) {
	output := strings.Join([]string{
		`Graph minimal context for "inject flow"`,
		"Shared NebulaGraph read: yes",
		"",
		"High-signal connected flow (native NebulaGraph traversal edges):",
		"- [repoId:1051] method sdkInjector.injectNodeJS at internal/instrumentation/sdk.go --CALLS--> [repoId:1051] function injectCommonSDKConfig at internal/instrumentation/sdk.go @ internal/instrumentation/sdk.go:115",
		"  source=scip; provenance=scip; confidence=0.95",
		"- [repoId:1051] method Mutate at internal/instrumentation/podmutator.go --REFERENCES--> [repoId:1051] constant annotationInjectApacheHttpd at internal/instrumentation/annotation.go @ internal/instrumentation/annotation.go:88",
		"  source=scip; provenance=scip; confidence=0.95",
	}, "\n")
	got := codegraphContextCriticalManifest("Explain NodeJS, Python, and .NET auto instrumentation flow", []layer{{
		Title:  "Graph minimal context",
		Tool:   "graph_minimal_context",
		Output: output,
	}}, nil)
	nodeIndex := strings.Index(got, "sdkInjector.injectNodeJS")
	apacheIndex := strings.Index(got, "annotationInjectApacheHttpd")
	if nodeIndex < 0 {
		t.Fatalf("manifest missing requested NodeJS proof:\n%s", got)
	}
	if apacheIndex >= 0 && apacheIndex < nodeIndex {
		t.Fatalf("Apache proof should not outrank requested NodeJS proof:\n%s", got)
	}
}

func TestSplitGraphEdgesDemotesPreciseButOffQuestionRuntimeEdges(t *testing.T) {
	conf := 0.95
	edges := []graphreader.Edge{{
		Relation:         "REFERENCES",
		Source:           "scip",
		Provenance:       "scip",
		Confidence:       &conf,
		EvidenceFilePath: "cmd/mdatagen/internal/command.go",
		Start:            graphreader.Endpoint{Kind: "symbol", Label: "injectInternalMetadataDefs", Path: "cmd/mdatagen/internal/command.go"},
		Neighbor:         graphreader.Endpoint{Kind: "symbol", Label: "ResourceAttributes", Path: "cmd/mdatagen/internal/metadata.go"},
	}, {
		Relation:         "CALLS",
		Source:           "scip",
		Provenance:       "scip",
		Confidence:       &conf,
		EvidenceFilePath: "internal/instrumentation/sdk.go",
		Start:            graphreader.Endpoint{Kind: "method", Label: "sdkInjector.injectNodeJS", Path: "internal/instrumentation/sdk.go"},
		Neighbor:         graphreader.Endpoint{Kind: "method", Label: "sdkInjector.injectCommonSDKConfig", Path: "internal/instrumentation/sdk.go"},
	}}
	trusted, heuristic := splitGraphEdgesByEvidenceTier("Explain NodeJS, Python, and .NET auto-instrumentation flow", edges)
	if len(trusted) != 1 || !strings.Contains(graphEdgeEvidenceText(trusted[0]), "injectNodeJS") {
		t.Fatalf("trusted edges should keep requested runtime flow only, got trusted=%+v heuristic=%+v", trusted, heuristic)
	}
	if len(heuristic) != 1 || !strings.Contains(graphEdgeEvidenceText(heuristic[0]), "ResourceAttributes") {
		t.Fatalf("precise but off-question SCIP edge should be demoted, got trusted=%+v heuristic=%+v", trusted, heuristic)
	}
}

func TestCodegraphContextCriticalManifestIncludesASTProof(t *testing.T) {
	output := strings.Join([]string{
		`Graph path for "webhook flow"`,
		"AST/tree-sitter semantic edges related to the path:",
		"- [operator] CALLS: podMutationWebhook.Handle -> instPodMutator.Mutate (INFERRED, 0.60) at internal/webhook/podmutation/webhookhandler.go:47",
		"  source=ast-go",
	}, "\n")
	got := codegraphContextCriticalManifest("webhook flow", []layer{{
		Title:  "Graph path context",
		Tool:   "graph_path",
		Output: output,
	}}, nil)
	for _, want := range []string{
		"AST/tree-sitter evidence is present",
		"AST/tree-sitter proof lines:",
		"podMutationWebhook.Handle",
		"source=ast-go",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("manifest missing %q:\n%s", want, got)
		}
	}
}

func TestCodegraphContextASTProofLinesPreferReadableGraphEdges(t *testing.T) {
	output := strings.Join([]string{
		"AST/tree-sitter facts:",
		"  source=ast-go",
		"Ranked heuristic AST path edges (low-confidence; source verification required):",
		"- step 1 depth 1: [repoId:1051] function injectDotNetSDKToContainer at internal/instrumentation/dotnet.go --CALLS--> [repoId:1051] function validateContainerEnv at internal/instrumentation/dotnet.go at internal/instrumentation/dotnet.go:45",
		"  source=ast-go; provenance=heuristic; confidence=0.60",
		"AST/tree-sitter semantic edges related to the path:",
		"- [operator] CALLS: function:3fd7e48caa8ddea0b5c4768645bb3234 -> function:a024297198cc11fbf7586e6ed1417ae7 at internal/instrumentation/dotnet.go:43",
		"  source=ast-go",
	}, "\n")
	got := codegraphContextASTProofLines("dotnet flow", output, 1)
	if len(got) != 1 {
		t.Fatalf("proof count = %d want 1: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "injectDotNetSDKToContainer") || strings.Contains(got[0], "function:3fd7") {
		t.Fatalf("AST proof should prefer readable native graph edge over opaque IDs: %#v", got)
	}
}

func TestCodegraphContextASTProofLinesDemotesMixedUnrequestedRuntime(t *testing.T) {
	output := strings.Join([]string{
		"Ranked heuristic AST path edges (low-confidence; source verification required):",
		"- step 1 depth 1: [repoId:1051] function injectNodeJSSDKToContainer at internal/instrumentation/nodejs.go --CALLS--> [repoId:1051] function instrVolume at internal/instrumentation/apachehttpd.go at internal/instrumentation/nodejs.go:21",
		"  source=ast-go; provenance=heuristic; confidence=0.60",
		"- step 2 depth 1: [repoId:1051] method sdkInjector.injectNodeJS at internal/instrumentation/sdk.go --CALLS--> [repoId:1051] method sdkInjector.injectCommonSDKConfig at internal/instrumentation/sdk.go at internal/instrumentation/sdk.go:115",
		"  source=ast-go; provenance=heuristic; confidence=0.60",
	}, "\n")
	got := codegraphContextASTProofLines("Explain NodeJS, Python, and .NET auto-instrumentation flow", output, 1)
	if len(got) != 1 {
		t.Fatalf("proof count = %d want 1: %#v", len(got), got)
	}
	if !strings.Contains(got[0], "injectCommonSDKConfig") || strings.Contains(got[0], "apachehttpd.go") {
		t.Fatalf("AST proof should prefer clean requested-runtime flow over mixed Apache edge: %#v", got)
	}
}

func TestCodegraphContextASTProofLinesFiltersGenericInjectForRuntimeQuery(t *testing.T) {
	output := strings.Join([]string{
		"AST/tree-sitter semantic edges related to the path:",
		"- [github.com/open-telemetry/opentelemetry-collector] cmd/mdatagen/internal/command_test.go:654 REFERENCE go.opentelemetry.io/collector/cmd/mdatagen/internal/injectInternalMetadataDefs().",
		"  source=ast-go",
		"- [repoId:1051] method sdkInjector.injectNodeJS at internal/instrumentation/sdk.go --CALLS--> [repoId:1051] method sdkInjector.injectCommonSDKConfig at internal/instrumentation/sdk.go at internal/instrumentation/sdk.go:115",
		"  source=ast-go; provenance=heuristic; confidence=0.60",
	}, "\n")
	got := codegraphContextASTProofLines("Explain NodeJS, Python, and .NET auto-instrumentation flow", output, 3)
	if len(got) != 1 {
		t.Fatalf("proof count = %d want 1: %#v", len(got), got)
	}
	if strings.Contains(got[0], "injectInternalMetadataDefs") || !strings.Contains(got[0], "injectCommonSDKConfig") {
		t.Fatalf("AST proof should filter generic inject rows for runtime-specific query: %#v", got)
	}
}

func TestCodegraphContextASTProofLinesFiltersOpaqueIDs(t *testing.T) {
	output := strings.Join([]string{
		"AST/tree-sitter semantic edges related to the path:",
		"- [github.com/open-telemetry/opentelemetry-operator] CALLS: function:3fd7e48caa8ddea0b5c4768645bb3234 -> function:a024297198cc11fbf7586e6ed1417ae7 at internal/instrumentation/dotnet.go:43",
		"  source=ast-go",
	}, "\n")
	got := codegraphContextASTProofLines("Explain NodeJS, Python, and .NET auto-instrumentation flow", output, 3)
	if len(got) != 0 {
		t.Fatalf("opaque AST function IDs should not be promoted into compact manifest proof: %#v", got)
	}
}

func TestGraphEdgeMatchesRequestedRuntimeRejectsMixedRuntimeWithoutCoreFlow(t *testing.T) {
	requested := graphEvidenceRequestedLanguageBuckets("Explain NodeJS, Python, and .NET flow")
	mixed := "function injectNodeJSSDKToContainer at internal/instrumentation/nodejs.go --CALLS--> function instrVolume at internal/instrumentation/apachehttpd.go"
	if graphEdgeMatchesRequestedRuntimeOrCoreFlow(mixed, requested) {
		t.Fatalf("mixed requested/unrequested runtime edge without core flow should not be trusted")
	}
	core := "method sdkInjector.injectNodeJS --CALLS--> method sdkInjector.injectCommonSDKConfig at internal/instrumentation/sdk.go"
	if !graphEdgeMatchesRequestedRuntimeOrCoreFlow(core, requested) {
		t.Fatalf("requested runtime edge with core flow should remain trusted")
	}
}

func TestAppendGraphMinimalEdgesPreservesASTProvenance(t *testing.T) {
	conf := 0.60
	line := int32(45)
	lines := []string{}
	appendGraphMinimalEdges(&lines, []graphreader.Edge{{
		Relation:         "CALLS",
		Source:           "ast-go",
		Provenance:       "heuristic",
		Confidence:       &conf,
		EvidenceFilePath: "internal/instrumentation/dotnet.go",
		StartLine:        &line,
		Start:            graphreader.Endpoint{Kind: "function", Label: "injectDotNetSDKToContainer", Path: "internal/instrumentation/dotnet.go"},
		Neighbor:         graphreader.Endpoint{Kind: "function", Label: "validateContainerEnv", Path: "internal/instrumentation/dotnet.go"},
	}}, 10)
	got := strings.Join(lines, "\n")
	for _, want := range []string{"injectDotNetSDKToContainer", "validateContainerEnv", "source=ast-go", "provenance=heuristic", "confidence=0.60"} {
		if !strings.Contains(got, want) {
			t.Fatalf("minimal graph edge missing %q:\n%s", want, got)
		}
	}
}

func TestCodegraphContextLayersRequireASTEvidence(t *testing.T) {
	withAST := []layer{{
		Title:  "Graph path context",
		Tool:   "graph_path",
		Output: "Semantic architecture facts\n- [repo] CALLS: a -> b at src/a.go:4\n  source=ast-go",
	}}
	withoutAST := []layer{{
		Title:  "Graph path context",
		Tool:   "graph_path",
		Output: "- [repoId:1] symbol a --CALLS--> symbol b\n  source=scip; provenance=scip; confidence=0.95",
	}}
	if !codegraphContextLayersHaveASTEvidence(withAST) {
		t.Fatalf("expected AST evidence to satisfy full-stack gate")
	}
	if codegraphContextLayersHaveASTEvidence(withoutAST) {
		t.Fatalf("SCIP-only graph output must not satisfy AST/tree-sitter gate")
	}
}
