package graphschema

// This file ports the last four renderers from
// packages/backend/src/codeGraph/nebulaNgql.ts (lines 82-92):
//
//   RenderDescribeTagStatement   — TS renderNebulaGraphDescribeTagStatement   (line 82)
//   RenderDescribeEdgeStatement  — TS renderNebulaGraphDescribeEdgeStatement  (line 84)
//   RenderAlterTagAddStatement   — TS renderNebulaGraphAlterTagAddStatement   (lines 86-88)
//   RenderAlterEdgeAddStatement  — TS renderNebulaGraphAlterEdgeAddStatement  (lines 90-92)
//
// After this file lands the TS source's emit surface is fully
// ported. Byte parity is locked by describe_alter_test.go against
// captured-golden output from the JS algorithm.
//
// DESCRIBE statements are the schema-introspection probe a
// CodeGraphStore health check uses to confirm the live schema
// matches the binary's expected NodeProps / EdgeProps. ALTER
// statements feed the forward-compat migration path: the
// CodeGraphStore diffs the live schema against the binary's
// expected prop set and synthesises an ADD-only ALTER for each
// missing column.

// RenderDescribeTagStatement returns the no-arg statement that
// asks graphd to describe the code_graph_node tag's columns.
// The result-set shape (column name + type + nullable + default)
// is determined by Nebula's DESCRIBE TAG implementation and is
// out of scope for this renderer.
func RenderDescribeTagStatement() string {
	return "DESCRIBE TAG " + quoteIdentifier(NodeTag) + ";"
}

// RenderDescribeEdgeStatement is the edge-type equivalent of
// RenderDescribeTagStatement.
func RenderDescribeEdgeStatement() string {
	return "DESCRIBE EDGE " + quoteIdentifier(EdgeType) + ";"
}

// RenderAlterTagAddStatement emits
//
//	ALTER TAG `code_graph_node` ADD (<col defs>);
//
// for every name in props. Column type is determined per-prop by
// propType (the same function that drives CREATE TAG), so the
// emitted ADD clauses are type-consistent with the original
// CREATE — a writer upgrading a deployed schema cannot
// accidentally widen orgId from int to string.
//
// Empty props produces `ALTER TAG ... ADD ();`. The JS source
// emits the same shape — graphd rejects it as a syntax error.
// The renderer preserves that behaviour rather than swallowing
// the case silently so the migration call site fails loudly on
// an empty diff (which is itself a writer bug).
func RenderAlterTagAddStatement(props []string) string {
	return "ALTER TAG " + quoteIdentifier(NodeTag) + " ADD (" + renderPropDefs(props) + ");"
}

// RenderAlterEdgeAddStatement is the edge-type equivalent of
// RenderAlterTagAddStatement. Same propType-driven column types,
// same empty-input behaviour. EdgeProps is the operational
// expectation for what edge property names look like, but the
// renderer takes any string slice — callers feed the diff
// between the live edge schema and EdgeProps.
func RenderAlterEdgeAddStatement(props []string) string {
	return "ALTER EDGE " + quoteIdentifier(EdgeType) + " ADD (" + renderPropDefs(props) + ");"
}

