package graphschema

import (
	"strings"
	"testing"
)

// Golden outputs captured by re-running the JS algorithm from
// packages/backend/src/codeGraph/nebulaNgql.ts (lines 128-142)
// against the fixture vertices/edges below. Re-capture only when
// the underlying TS source actually changes.
const (
	// Q.A appended `provenance` to NodeProps and `provenance` +
	// `context` to EdgeProps. The fixtures below set those keys
	// so the byte-equal output carries them in the rightmost
	// value-slots. The legacy TS source predates Q.A — these
	// goldens DIVERGE from the legacy byte-for-byte; the
	// divergence is permitted per docs/codeintel-quality-overhaul.md
	// which trumps pre-rename byte equality for the Q.* slice
	// quality additions.
	goldenSingleVertex = `INSERT VERTEX ` + "`code_graph_node`" + `(` + "`kind`" + `, ` + "`orgId`" + `, ` + "`workspaceId`" + `, ` + "`repoId`" + `, ` + "`revision`" + `, ` + "`commitHash`" + `, ` + "`schemaVersion`" + `, ` + "`builderVersion`" + `, ` + "`key`" + `, ` + "`label`" + `, ` + "`path`" + `, ` + "`language`" + `, ` + "`routeMethod`" + `, ` + "`routePath`" + `, ` + "`packageManager`" + `, ` + "`confidence`" + `, ` + "`confidenceTier`" + `, ` + "`evidenceFilePath`" + `, ` + "`startLine`" + `, ` + "`endLine`" + `, ` + "`source`" + `, ` + "`provenance`" + `) VALUES "v-1": ("symbol", 7, "ws-1", 42, "main", "deadbeef", 1, "b-1", "k", "MyClass", "src/file.ts", "typescript", NULL, NULL, NULL, 0.95, "EXTRACTED", "src/file.ts", 10, 20, "scip", "scip");`

	goldenTwoVertices = `INSERT VERTEX ` + "`code_graph_node`" + `(` + "`kind`" + `, ` + "`orgId`" + `, ` + "`workspaceId`" + `, ` + "`repoId`" + `, ` + "`revision`" + `, ` + "`commitHash`" + `, ` + "`schemaVersion`" + `, ` + "`builderVersion`" + `, ` + "`key`" + `, ` + "`label`" + `, ` + "`path`" + `, ` + "`language`" + `, ` + "`routeMethod`" + `, ` + "`routePath`" + `, ` + "`packageManager`" + `, ` + "`confidence`" + `, ` + "`confidenceTier`" + `, ` + "`evidenceFilePath`" + `, ` + "`startLine`" + `, ` + "`endLine`" + `, ` + "`source`" + `, ` + "`provenance`" + `) VALUES "v-1": ("symbol", 7, "ws-1", 42, "main", "deadbeef", 1, "b-1", "k", "MyClass", "src/file.ts", "typescript", NULL, NULL, NULL, 0.95, "EXTRACTED", "src/file.ts", 10, 20, "scip", "scip"), "v-2": ("file", 7, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL);`

	goldenSingleEdge = `INSERT EDGE ` + "`code_graph_edge`" + `(` + "`kind`" + `, ` + "`orgId`" + `, ` + "`workspaceId`" + `, ` + "`repoId`" + `, ` + "`revision`" + `, ` + "`commitHash`" + `, ` + "`schemaVersion`" + `, ` + "`builderVersion`" + `, ` + "`confidence`" + `, ` + "`confidenceTier`" + `, ` + "`evidenceFilePath`" + `, ` + "`startLine`" + `, ` + "`endLine`" + `, ` + "`normalizedKey`" + `, ` + "`source`" + `, ` + "`provenance`" + `, ` + "`context`" + `) VALUES "v-1"->"v-2"@1234567: ("DEFINES", 7, "ws-1", 42, "main", "deadbeef", 1, "b-1", 0.9, "INFERRED", NULL, NULL, NULL, "k", "ast", "heuristic", NULL);`
)

// fullyPopulatedVertex is the fixture matching the JS golden for
// the single-vertex case.
func fullyPopulatedVertex() CodeGraphVertex {
	return CodeGraphVertex{
		VID:  "v-1",
		Kind: "symbol",
		Properties: map[string]CodeGraphPrimitive{
			"kind":             "symbol",
			"orgId":            7,
			"workspaceId":      "ws-1",
			"repoId":           42,
			"revision":         "main",
			"commitHash":       "deadbeef",
			"schemaVersion":    1,
			"builderVersion":   "b-1",
			"key":              "k",
			"label":            "MyClass",
			"path":             "src/file.ts",
			"language":         "typescript",
			"routeMethod":      nil,
			"routePath":        nil,
			"packageManager":   nil,
			"confidence":       0.95,
			"confidenceTier":   "EXTRACTED",
			"evidenceFilePath": "src/file.ts",
			"startLine":        10,
			"endLine":          20,
			"source":           "scip",
			// Q.A — fully-populated fixture sets the new
			// `provenance` slot so the byte-equal golden has a
			// concrete value to compare against.
			"provenance": "scip",
		},
	}
}

// sparseVertex is the fixture matching the JS golden for the
// second vertex in the two-vertex case — only kind + orgId set,
// every other property defaulting to NULL via the absent-key path.
func sparseVertex() CodeGraphVertex {
	return CodeGraphVertex{
		VID:  "v-2",
		Kind: "file",
		Properties: map[string]CodeGraphPrimitive{
			"kind":  "file",
			"orgId": 7,
		},
	}
}

// fullyPopulatedEdge matches the JS golden for the single-edge
// case.
func fullyPopulatedEdge() CodeGraphEdge {
	return CodeGraphEdge{
		FromVID: "v-1",
		ToVID:   "v-2",
		Kind:    "DEFINES",
		Rank:    1234567,
		Properties: map[string]CodeGraphPrimitive{
			"kind":             "DEFINES",
			"orgId":            7,
			"workspaceId":      "ws-1",
			"repoId":           42,
			"revision":         "main",
			"commitHash":       "deadbeef",
			"schemaVersion":    1,
			"builderVersion":   "b-1",
			"confidence":       0.9,
			"confidenceTier":   "INFERRED",
			"evidenceFilePath": nil,
			"startLine":        nil,
			"endLine":          nil,
			"normalizedKey":    "k",
			"source":           "ast",
			// Q.A — populate provenance with the AST-tier value
			// the indexer would write; leave context NULL.
			"provenance": "heuristic",
		},
	}
}

// TestRenderVertexInsert_Parity_FullyPopulated locks the byte-
// for-byte output for a vertex with every NodeProps slot
// supplied.
func TestRenderVertexInsert_Parity_FullyPopulated(t *testing.T) {
	got := RenderVertexInsert([]CodeGraphVertex{fullyPopulatedVertex()})
	if got != goldenSingleVertex {
		t.Errorf("byte-mismatch:\n got: %s\nwant: %s", got, goldenSingleVertex)
	}
}

// TestRenderVertexInsert_Parity_SparseSecondVertex covers the
// absent-key NULL fallback path. Vertex #1 fully populated;
// vertex #2 only has kind + orgId — every other column emits
// NULL via the map's zero-value-for-missing-key semantics.
func TestRenderVertexInsert_Parity_SparseSecondVertex(t *testing.T) {
	got := RenderVertexInsert([]CodeGraphVertex{
		fullyPopulatedVertex(),
		sparseVertex(),
	})
	if got != goldenTwoVertices {
		t.Errorf("byte-mismatch:\n got: %s\nwant: %s", got, goldenTwoVertices)
	}
}

// TestRenderEdgeInsert_Parity locks the byte-for-byte output for
// the from->to@rank: (...) edge-insert shape, including the
// integer rank rendered without quotes.
func TestRenderEdgeInsert_Parity(t *testing.T) {
	got := RenderEdgeInsert([]CodeGraphEdge{fullyPopulatedEdge()})
	if got != goldenSingleEdge {
		t.Errorf("byte-mismatch:\n got: %s\nwant: %s", got, goldenSingleEdge)
	}
}

// TestRenderVertexInsert_NullPropertyVsAbsent confirms that an
// explicit `nil` property value and an absent map key produce the
// same wire output (both → NULL). This is the JS `?? null` semantic
// the Go port preserves through Go's map zero-value-for-missing
// behaviour.
func TestRenderVertexInsert_NullPropertyVsAbsent(t *testing.T) {
	withExplicitNull := CodeGraphVertex{
		VID:        "v-x",
		Properties: map[string]CodeGraphPrimitive{"kind": "file", "orgId": 7, "label": nil},
	}
	withAbsent := CodeGraphVertex{
		VID:        "v-x",
		Properties: map[string]CodeGraphPrimitive{"kind": "file", "orgId": 7},
	}
	a := RenderVertexInsert([]CodeGraphVertex{withExplicitNull})
	b := RenderVertexInsert([]CodeGraphVertex{withAbsent})
	if a != b {
		t.Errorf("explicit-null vs absent diverged:\n explicit-null: %s\n absent:        %s", a, b)
	}
}

// TestRenderSnapshotStatements_EmptySnapshot confirms an empty
// snapshot emits exactly the schema statements (no INSERT
// VERTEX, no INSERT EDGE). Matches the JS behaviour where
// chunkArray returns [] and the for-of loop runs zero times.
func TestRenderSnapshotStatements_EmptySnapshot(t *testing.T) {
	got := RenderSnapshotStatements(CodeGraphSnapshot{})
	if len(got) != len(RenderSchemaStatements()) {
		t.Fatalf("empty snapshot: want %d schema statements, got %d", len(RenderSchemaStatements()), len(got))
	}
	// Equality against RenderSchemaStatements (already parity-
	// tested elsewhere) avoids duplicating the byte-equal
	// strings here.
	want := RenderSchemaStatements()
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stmt %d byte-mismatch:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

// TestRenderVertexInsert_DeterministicAcrossInsertOrder locks the
// invariant that iteration order of vertex.Properties does NOT
// affect output. Go map iteration is intentionally randomised; if
// a future refactor accidentally iterates the map (instead of
// NodeProps) the output would silently shuffle between Go versions
// and runs. This test inserts the same properties twice in two
// different insertion orders and asserts byte-equal output.
func TestRenderVertexInsert_DeterministicAcrossInsertOrder(t *testing.T) {
	// Two property maps with identical key/value pairs in two
	// different (Go-source) declaration orders. Go's map storage
	// further randomises traversal so the runtime order is not
	// the declaration order — that's the point.
	propsA := map[string]CodeGraphPrimitive{
		"kind": "symbol", "orgId": 7, "workspaceId": "ws", "repoId": 1,
		"revision": "main", "commitHash": "x", "schemaVersion": 1, "builderVersion": "v",
		"key": "k", "label": "L", "source": "s",
	}
	propsB := map[string]CodeGraphPrimitive{
		"source": "s", "label": "L", "key": "k",
		"builderVersion": "v", "schemaVersion": 1, "commitHash": "x", "revision": "main",
		"repoId": 1, "workspaceId": "ws", "orgId": 7, "kind": "symbol",
	}
	got1 := RenderVertexInsert([]CodeGraphVertex{{VID: "v", Properties: propsA}})
	got2 := RenderVertexInsert([]CodeGraphVertex{{VID: "v", Properties: propsB}})
	if got1 != got2 {
		t.Errorf("map iteration affected output:\n run1: %s\n run2: %s", got1, got2)
	}
}

// TestRenderSnapshotStatements_BatchesAt250 confirms the batch
// boundary matches the JS magic number. 251 vertices → 2 INSERT
// VERTEX statements (250 + 1); 250 edges → 1 INSERT EDGE
// statement.
func TestRenderSnapshotStatements_BatchesAt250(t *testing.T) {
	snap := CodeGraphSnapshot{
		Vertices: make([]CodeGraphVertex, 251),
		Edges:    make([]CodeGraphEdge, 250),
	}
	for i := range snap.Vertices {
		snap.Vertices[i] = CodeGraphVertex{VID: "v-" + iToStr(i)}
	}
	for i := range snap.Edges {
		snap.Edges[i] = CodeGraphEdge{FromVID: "v-" + iToStr(i), ToVID: "v-end", Rank: int64(i)}
	}
	got := RenderSnapshotStatements(snap)
	schemaCount := len(RenderSchemaStatements())
	// Schema + 2 vertex batches + 1 edge batch.
	if len(got) != schemaCount+3 {
		t.Errorf("statement count: got %d, want %d (schema + 2 vertex + 1 edge)", len(got), schemaCount+3)
	}
	// Sanity: each batched statement starts with the right verb.
	if !strings.HasPrefix(got[schemaCount], "INSERT VERTEX") || !strings.HasPrefix(got[schemaCount+1], "INSERT VERTEX") {
		t.Errorf("expected vertex INSERTs after schema; got %q %q", got[schemaCount][:20], got[schemaCount+1][:20])
	}
	if !strings.HasPrefix(got[schemaCount+2], "INSERT EDGE") {
		t.Errorf("expected edge INSERT after vertex batches; got %q", got[schemaCount+2][:20])
	}
}

func TestRenderSnapshotStatementsWithBatchSize_UsesOperationalBatch(t *testing.T) {
	snap := CodeGraphSnapshot{
		Vertices: make([]CodeGraphVertex, 5),
		Edges:    make([]CodeGraphEdge, 4),
	}
	for i := range snap.Vertices {
		snap.Vertices[i] = CodeGraphVertex{VID: "v-" + iToStr(i)}
	}
	for i := range snap.Edges {
		snap.Edges[i] = CodeGraphEdge{FromVID: "v-" + iToStr(i), ToVID: "v-end", Rank: int64(i)}
	}

	got := RenderSnapshotStatementsWithBatchSize(snap, 2)
	schemaCount := len(RenderSchemaStatements())
	// Schema + ceil(5/2) vertex batches + ceil(4/2) edge batches.
	if len(got) != schemaCount+5 {
		t.Errorf("statement count: got %d, want %d", len(got), schemaCount+5)
	}
}

// iToStr is a tiny helper used by the batch-boundary test to
// stamp distinct VIDs into the fixture. strconv would do, but a
// local one-liner avoids the import for a single use.
func iToStr(i int) string {
	if i == 0 {
		return "0"
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
