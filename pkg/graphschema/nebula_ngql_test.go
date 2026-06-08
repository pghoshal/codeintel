package graphschema

import (
	"strings"
	"testing"
)

// Captured golden — produced by re-running the JS
// renderNebulaGraphSchemaStatements() function from
// packages/backend/src/codeGraph/nebulaNgql.ts under Node and
// snapshotting the output. Every byte of every statement
// matters: a property re-ordering, a missing IF NOT EXISTS, a
// changed type, or a changed index width breaks compatibility
// with any pre-bootstrapped space that was provisioned by the
// TS source. Re-capture this fixture only when the underlying
// TS source actually changes — the parity test then catches
// the port mismatch.
const (
	// Q.A appends `provenance` to NodeProps and `provenance` +
	// `context` to EdgeProps. The golden strings here are
	// updated in lockstep with NodeProps / EdgeProps in
	// nebula_ngql.go; the Rust side in
	// indexer-rs/src/nebula_ngql.rs carries the identical
	// appends so Rust-rendered NGQL matches Go-rendered NGQL
	// byte-for-byte (the cross-side parity contract that the
	// graphstore rendered-statement validator enforces).
	goldenStmt0 = "CREATE TAG IF NOT EXISTS `code_graph_node`(`kind` string, `orgId` int, `workspaceId` string, `repoId` int, `revision` string, `commitHash` string, `schemaVersion` int, `builderVersion` string, `key` string, `label` string, `path` string, `language` string, `routeMethod` string, `routePath` string, `packageManager` string, `confidence` double, `confidenceTier` string, `evidenceFilePath` string, `startLine` int, `endLine` int, `source` string, `provenance` string);"
	goldenStmt1 = "CREATE EDGE IF NOT EXISTS `code_graph_edge`(`kind` string, `orgId` int, `workspaceId` string, `repoId` int, `revision` string, `commitHash` string, `schemaVersion` int, `builderVersion` string, `confidence` double, `confidenceTier` string, `evidenceFilePath` string, `startLine` int, `endLine` int, `normalizedKey` string, `source` string, `provenance` string, `context` string);"
	goldenStmt2 = "CREATE TAG INDEX IF NOT EXISTS `code_graph_node_scope_idx` ON `code_graph_node`(`orgId`, `workspaceId`(128), `repoId`, `commitHash`(40), `schemaVersion`, `builderVersion`(128));"
	goldenStmt3 = "CREATE TAG INDEX IF NOT EXISTS `code_graph_node_label_idx` ON `code_graph_node`(`orgId`, `workspaceId`(128), `label`(128));"
	goldenStmt4 = "CREATE TAG INDEX IF NOT EXISTS `code_graph_node_key_idx` ON `code_graph_node`(`orgId`, `workspaceId`(128), `key`(128));"
	goldenStmt5 = "CREATE TAG INDEX IF NOT EXISTS `code_graph_node_path_idx` ON `code_graph_node`(`orgId`, `workspaceId`(128), `path`(128));"
	goldenStmt6 = "CREATE TAG INDEX IF NOT EXISTS `code_graph_node_route_path_idx` ON `code_graph_node`(`orgId`, `workspaceId`(128), `routePath`(128));"
)

// TestRenderSchemaStatements_Parity locks the byte-for-byte
// equivalence between the Go port and the captured golden output.
// A drift in property order, propType mapping, identifier quoting,
// or index width fails this test before any cluster-level damage
// can occur.
func TestRenderSchemaStatements_Parity(t *testing.T) {
	want := []string{goldenStmt0, goldenStmt1, goldenStmt2, goldenStmt3, goldenStmt4, goldenStmt5, goldenStmt6}
	got := RenderSchemaStatements()

	if len(got) != len(want) {
		t.Fatalf("statement count: got %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d byte-mismatch:\n got: %s\nwant: %s\nlen got=%d want=%d", i, got[i], want[i], len(got[i]), len(want[i]))
		}
	}
}

// TestRenderSchemaStatements_AllIfNotExists locks the idempotency
// invariant — every emitted statement must carry IF NOT EXISTS so
// the codeintel-graph-init Job can re-run on every deploy without
// pre-checking cluster state.
func TestRenderSchemaStatements_AllIfNotExists(t *testing.T) {
	for i, stmt := range RenderSchemaStatements() {
		if !strings.Contains(stmt, "IF NOT EXISTS") {
			t.Errorf("stmt %d missing IF NOT EXISTS: %s", i, stmt)
		}
	}
}

// TestPropType_PropTypeTable locks the int / double / string match-
// set verbatim. The TS source names exactly five int columns and
// one double column; everything else is string. A divergence here
// changes the on-disk schema width and breaks every writer.
func TestPropType_PropTypeTable(t *testing.T) {
	intProps := []string{"orgId", "repoId", "schemaVersion", "startLine", "endLine"}
	for _, p := range intProps {
		if got := propType(p); got != "int" {
			t.Errorf("propType(%q): got %q, want int", p, got)
		}
	}
	if got := propType("confidence"); got != "double" {
		t.Errorf("propType(confidence): got %q, want double", got)
	}
	stringProps := []string{
		"kind", "workspaceId", "revision", "commitHash", "builderVersion",
		"key", "label", "path", "language", "routeMethod", "routePath",
		"packageManager", "confidenceTier", "evidenceFilePath", "source",
		"normalizedKey",
	}
	for _, p := range stringProps {
		if got := propType(p); got != "string" {
			t.Errorf("propType(%q): got %q, want string", p, got)
		}
	}
	// Unknown props fall through to string per the TS source's
	// default branch. Important for forward-compat: a new property
	// the indexer emits but the schema hasn't been ALTER'd to
	// include is still rendered as a string column.
	if got := propType("unknownNewProperty"); got != "string" {
		t.Errorf("propType unknown: got %q, want string (default)", got)
	}
}

// TestQuoteIdentifier_HappyAndHostile locks the JS-equivalent
// behaviour: wraps in backticks, doubles embedded backticks. Used
// for every identifier in every emitted nGQL statement.
func TestQuoteIdentifier_HappyAndHostile(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "code_graph_node", "`code_graph_node`"},
		{"empty", "", "``"},
		{"with backtick", "weird`name", "`weird``name`"},
		{"with multiple backticks", "a`b`c", "`a``b``c`"},
		// "```" (3 backticks) → each replaced with "``" (6 total)
		// → wrapped in outer backticks = 8 backticks total.
		{"only backticks", "```", "````````"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteIdentifier(tc.in); got != tc.want {
				t.Errorf("quoteIdentifier(%q): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNodePropsCount + TestEdgePropsCount lock the property-array
// lengths against the captured golden. Adding or removing a
// property is a schema change that must update the golden
// fixture in lockstep.
func TestNodePropsCount(t *testing.T) {
	// Q.A appends `provenance` → 22 columns.
	if got := len(NodeProps); got != 22 {
		t.Errorf("NodeProps length: got %d, want 22 (golden fixture)", got)
	}
}

func TestEdgePropsCount(t *testing.T) {
	// Q.A appends `provenance` + `context` → 17 columns.
	if got := len(EdgeProps); got != 17 {
		t.Errorf("EdgeProps length: got %d, want 17 (golden fixture)", got)
	}
}

// TestPropArrays_Order locks the exact prop-array order against
// the TS source. INSERT VERTEX / INSERT EDGE statements (port
// in a follow-up slice) bind values positionally — a reorder
// would silently mis-bind every column.
func TestPropArrays_Order(t *testing.T) {
	wantNode := []string{
		"kind", "orgId", "workspaceId", "repoId", "revision",
		"commitHash", "schemaVersion", "builderVersion", "key", "label",
		"path", "language", "routeMethod", "routePath", "packageManager",
		"confidence", "confidenceTier", "evidenceFilePath", "startLine",
		"endLine", "source",
		// Q.A append.
		"provenance",
	}
	for i := range wantNode {
		if NodeProps[i] != wantNode[i] {
			t.Errorf("NodeProps[%d]: got %q, want %q", i, NodeProps[i], wantNode[i])
		}
	}
	wantEdge := []string{
		"kind", "orgId", "workspaceId", "repoId", "revision",
		"commitHash", "schemaVersion", "builderVersion", "confidence",
		"confidenceTier", "evidenceFilePath", "startLine", "endLine",
		"normalizedKey", "source",
		// Q.A appends.
		"provenance", "context",
	}
	for i := range wantEdge {
		if EdgeProps[i] != wantEdge[i] {
			t.Errorf("EdgeProps[%d]: got %q, want %q", i, EdgeProps[i], wantEdge[i])
		}
	}
}
