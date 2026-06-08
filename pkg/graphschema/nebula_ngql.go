// Package graphschema is the cross-binary catalog of the
// codeintel NebulaGraph schema. The schema is intentionally
// generic: one tag (`code_graph_node`) and one edge type
// (`code_graph_edge`), each carrying a `kind` property that
// discriminates between specific node and edge subkinds. This
// keeps the storage layer one CREATE TAG / CREATE EDGE statement
// regardless of how many semantic kinds the indexer emits — new
// kinds are added by extending NodeProps / EdgeProps and re-
// running the bootstrap rather than by adding new tag types.
//
// This file is a deterministic port of
// packages/backend/src/codeGraph/nebulaNgql.ts lines 10–80
// (the schema-bootstrap surface: NODE_TAG / EDGE_TYPE /
// NODE_PROPS / EDGE_PROPS / renderNebulaGraphSchemaStatements +
// the quoteIdentifier / propType helpers they consume). The
// remaining helpers (INSERT VERTEX / INSERT EDGE / LOOKUP /
// DELETE VERTEX / ALTER TAG / ALTER EDGE) port in a follow-up
// slice as the codeintel-backend writer + the codeintel-app
// reader land.
//
// The port preserves byte-for-byte the SQL output of the TS-side
// renderNebulaGraphSchemaStatements() so a parity test can
// compare against the captured golden strings without adjustment.
// Identifier names use Go idiom (PascalCase) but the emitted SQL
// strings are identical.
package graphschema

import (
	"strings"
)

// NodeTag is the single tag every code-graph vertex carries.
// The `kind` property on the tag discriminates subkinds (symbol,
// file, repo, route, etc.).
const NodeTag = "code_graph_node"

// EdgeType is the single edge type every relationship carries.
// The `kind` property on the edge discriminates subkinds
// (DEFINES, REFERENCES, CALLS, IMPORTS_FROM, etc.).
const EdgeType = "code_graph_edge"

// Tag-index names. Unexported because callers reference them via
// the schema-statement builder rather than constructing nGQL by
// hand.
const (
	nodeScopeIndex = "code_graph_node_scope_idx"
	nodeLabelIndex = "code_graph_node_label_idx"
	nodeKeyIndex   = "code_graph_node_key_idx"
	nodePathIndex  = "code_graph_node_path_idx"
	nodeRouteIndex = "code_graph_node_route_path_idx"
)

// NodeProps is the property list every code_graph_node row
// carries. Order matters for INSERT VERTEX statements (a column-
// position contract with the Rust indexer), so the slice is
// declared once here and re-used everywhere.
//
// Q.A quality-overhaul append: `provenance` tags each vertex
// with the producer source class ("scip" / "heuristic" /
// "tree-sitter") so retrieval can rank by precision tier.
var NodeProps = []string{
	"kind",
	"orgId",
	"workspaceId",
	"repoId",
	"revision",
	"commitHash",
	"schemaVersion",
	"builderVersion",
	"key",
	"label",
	"path",
	"language",
	"routeMethod",
	"routePath",
	"packageManager",
	"confidence",
	"confidenceTier",
	"evidenceFilePath",
	"startLine",
	"endLine",
	"source",
	// Q.A — quality-overhaul addition. Must match the Rust
	// indexer's NODE_PROPS append at the same position.
	"provenance",
}

// EdgeProps is the property list every code_graph_edge row
// carries. Differs from NodeProps in two ways: no `key` / `label`
// / `path` / `language` / `routeMethod` / `routePath` /
// `packageManager` (those are vertex-only), and adds
// `normalizedKey` (the cross-revision deduplication key).
//
// Q.A quality-overhaul append:
//   - `provenance`: "scip" | "heuristic" | "tree-sitter".
//     Retrieval ranks scip edges above heuristic so semantic
//     SCIP relationships dominate top-K over regex-grade edges.
//   - `context`: the syntactic context of the edge (call /
//     field / param_type / return_type / attribute / inherits /
//     import). NULL until Q.B populates it; reader filters
//     traversals by context inferred from the NL query.
var EdgeProps = []string{
	"kind",
	"orgId",
	"workspaceId",
	"repoId",
	"revision",
	"commitHash",
	"schemaVersion",
	"builderVersion",
	"confidence",
	"confidenceTier",
	"evidenceFilePath",
	"startLine",
	"endLine",
	"normalizedKey",
	"source",
	// Q.A — must match the Rust indexer's EDGE_PROPS append at
	// the same positions.
	"provenance",
	"context",
}

// RenderSchemaStatements returns the seven nGQL statements the
// graph-schema bootstrap must run inside the codeintel space:
// CREATE TAG, CREATE EDGE, plus five CREATE TAG INDEX
// statements. Output ordering and string formatting mirror the
// TS-side renderNebulaGraphSchemaStatements function byte-for-byte
// — the parity test in nebula_ngql_test.go locks each emitted
// statement against the captured golden output.
//
// Every CREATE uses IF NOT EXISTS so the bootstrap is idempotent:
// re-running against a fully-provisioned space is a no-op.
func RenderSchemaStatements() []string {
	return []string{
		"CREATE TAG IF NOT EXISTS " + quoteIdentifier(NodeTag) + "(" + renderPropDefs(NodeProps) + ");",
		"CREATE EDGE IF NOT EXISTS " + quoteIdentifier(EdgeType) + "(" + renderPropDefs(EdgeProps) + ");",
		"CREATE TAG INDEX IF NOT EXISTS " + quoteIdentifier(nodeScopeIndex) + " ON " + quoteIdentifier(NodeTag) + "(" + strings.Join([]string{
			quoteIdentifier("orgId"),
			quoteIdentifier("workspaceId") + "(128)",
			quoteIdentifier("repoId"),
			quoteIdentifier("commitHash") + "(40)",
			quoteIdentifier("schemaVersion"),
			quoteIdentifier("builderVersion") + "(128)",
		}, ", ") + ");",
		// Prefix indexes on label and key so LOOKUP can do index-backed STARTS WITH
		// predicates and `==` lookups efficiently. Tag-index property length is
		// capped at 128 chars — symbols longer than that fall back to full scan.
		"CREATE TAG INDEX IF NOT EXISTS " + quoteIdentifier(nodeLabelIndex) + " ON " + quoteIdentifier(NodeTag) + "(" + strings.Join([]string{
			quoteIdentifier("orgId"),
			quoteIdentifier("workspaceId") + "(128)",
			quoteIdentifier("label") + "(128)",
		}, ", ") + ");",
		"CREATE TAG INDEX IF NOT EXISTS " + quoteIdentifier(nodeKeyIndex) + " ON " + quoteIdentifier(NodeTag) + "(" + strings.Join([]string{
			quoteIdentifier("orgId"),
			quoteIdentifier("workspaceId") + "(128)",
			quoteIdentifier("key") + "(128)",
		}, ", ") + ");",
		"CREATE TAG INDEX IF NOT EXISTS " + quoteIdentifier(nodePathIndex) + " ON " + quoteIdentifier(NodeTag) + "(" + strings.Join([]string{
			quoteIdentifier("orgId"),
			quoteIdentifier("workspaceId") + "(128)",
			quoteIdentifier("path") + "(128)",
		}, ", ") + ");",
		"CREATE TAG INDEX IF NOT EXISTS " + quoteIdentifier(nodeRouteIndex) + " ON " + quoteIdentifier(NodeTag) + "(" + strings.Join([]string{
			quoteIdentifier("orgId"),
			quoteIdentifier("workspaceId") + "(128)",
			quoteIdentifier("routePath") + "(128)",
		}, ", ") + ");",
	}
}

// renderPropDefs builds the `prop1 type1, prop2 type2, ...`
// fragment used in both CREATE TAG and CREATE EDGE bodies. Direct
// port of the JS arrow-function `props.map(...).join(", ")`
// pattern.
func renderPropDefs(props []string) string {
	parts := make([]string, 0, len(props))
	for _, prop := range props {
		parts = append(parts, quoteIdentifier(prop)+" "+propType(prop))
	}
	return strings.Join(parts, ", ")
}

// propType returns the nGQL column type for a property name.
// Direct port of the JS propType helper:
//
//	if (prop === "orgId" || "repoId" || "schemaVersion" ||
//	    "startLine" || "endLine") return "int";
//	if (prop === "confidence") return "double";
//	return "string";
//
// The match-set is kept verbatim — a port that diverges on which
// columns are int vs string changes the on-disk schema width and
// breaks every indexer write against a previously-bootstrapped
// space.
func propType(prop string) string {
	switch prop {
	case "orgId", "repoId", "schemaVersion", "startLine", "endLine":
		return "int"
	case "confidence":
		return "double"
	default:
		return "string"
	}
}

// quoteIdentifier wraps an identifier in backticks, doubling any
// embedded backtick. Direct port of the JS regex-replace pattern
// `value.replace(/`/g, "“")` wrapped in backticks. Used for
// every tag / edge / index / property name that lands in nGQL.
func quoteIdentifier(value string) string {
	if !strings.ContainsRune(value, '`') {
		return "`" + value + "`"
	}
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}
