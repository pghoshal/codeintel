package graphschema

import "strings"

// This file ports the snapshot-scope LOOKUP and chunked
// DELETE VERTEX surface from
// packages/backend/src/codeGraph/nebulaNgql.ts lines 109-126:
//
//   RenderLookupSnapshotVerticesStatement
//       — TS renderNebulaGraphLookupSnapshotVerticesStatement (109-120)
//       — emits one LOOKUP statement scoped to a snapshot tuple
//         (orgId, workspaceId, repoId, commitHash, schemaVersion,
//         builderVersion), yielding `vid` for every matching
//         vertex.
//
//   RenderDeleteVerticesStatements
//       — TS renderNebulaGraphDeleteVerticesStatements (122-126)
//       — chunks a vid list into 250-element batches and emits
//         one `DELETE VERTEX ... WITH EDGE;` statement per chunk.
//         `WITH EDGE` ensures every edge incident to a deleted
//         vertex is removed in the same write, so snapshot
//         retirement leaves no dangling edges.
//
// Both renderers route every dynamic value through ngqlValue, so
// quote escaping and number formatting matches the rest of the
// package (and the JS algorithm exactly).
//
// The propRef helper is the TS source's nGQL property-reference
// builder: `code_graph_node.orgId` etc. — used by LOOKUP's WHERE
// clauses to disambiguate the property reference from a raw
// column name.

// propRef returns the nGQL `tag.prop` reference, both
// identifiers backtick-quoted. Direct port of the TS:
//
//	const propRef = (prop: string) =>
//	  `${quoteIdentifier(NODE_TAG)}.${quoteIdentifier(prop)}`;
//
// Unexported because it's an internal helper used only by the
// lookup renderer.
func propRef(prop string) string {
	return quoteIdentifier(NodeTag) + "." + quoteIdentifier(prop)
}

// RenderLookupSnapshotVerticesStatement emits the
// `LOOKUP ON code_graph_node WHERE ... YIELD id(vertex) AS vid;`
// statement that resolves every vertex belonging to the supplied
// snapshot tuple. Direct port of the TS body — eight whitespace-
// separated tokens joined with " ". Output matches the captured
// JS golden byte-for-byte (locked by lookup_test.go).
//
// The WHERE clause anchors on the indexed scope columns
// (orgId / workspaceId / repoId / commitHash / schemaVersion /
// builderVersion) so the code_graph_node_scope_idx kicks in and
// the lookup is O(matching-vertices) rather than a full scan.
//
// Tenant scoping is the caller's responsibility — the renderer
// trusts the supplied input verbatim and emits whatever tuple it
// receives. Wrap call sites with an auth-context-aware function
// that derives OrgID / WorkspaceID from the resolved identity.
func RenderLookupSnapshotVerticesStatement(input CodeGraphDeleteInput) string {
	return strings.Join([]string{
		"LOOKUP ON " + quoteIdentifier(NodeTag),
		"WHERE " + propRef("orgId") + " == " + ngqlValue(input.OrgID),
		"AND " + propRef("workspaceId") + " == " + ngqlValue(input.WorkspaceID),
		"AND " + propRef("repoId") + " == " + ngqlValue(input.RepoID),
		"AND " + propRef("commitHash") + " == " + ngqlValue(input.CommitHash),
		"AND " + propRef("schemaVersion") + " == " + ngqlValue(input.SchemaVersion),
		"AND " + propRef("builderVersion") + " == " + ngqlValue(input.BuilderVersion),
		"YIELD id(vertex) AS vid;",
	}, " ")
}

// RenderDeleteVerticesStatements returns one
// `DELETE VERTEX ... WITH EDGE;` statement per 250-vid chunk.
// Direct port of the TS:
//
//	export const renderNebulaGraphDeleteVerticesStatements =
//	  (vids: string[]) =>
//	    chunkArray(vids, 250).map(chunk => (
//	      `DELETE VERTEX ${chunk.map(ngqlValue).join(", ")} WITH EDGE;`
//	    ));
//
// Empty input → empty slice (matching JS, where chunkArray of
// an empty array returns []). Single-vid input → one
// `DELETE VERTEX "vid" WITH EDGE;`. Each vid routes through
// ngqlValue so quote escaping is consistent with INSERT / LOOKUP.
//
// WITH EDGE atomicity: nebula's per-statement transaction
// boundary covers the vertex AND its incident edges, so a
// partially-applied delete cannot leave an orphan edge whose
// to_vid no longer exists.
//
// Tenant scoping is the caller's responsibility — the renderer
// trusts the supplied vids verbatim and emits the literal
// DELETE statement. Wrap call sites with an auth-context-aware
// function that proves every vid in the list belongs to the
// caller's tenant (typically by sourcing the vid list from a
// just-issued LOOKUP scoped to that tenant).
func RenderDeleteVerticesStatements(vids []string) []string {
	chunks := chunkArray(vids, snapshotBatchSize)
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		valueParts := make([]string, 0, len(chunk))
		for _, vid := range chunk {
			valueParts = append(valueParts, ngqlValue(vid))
		}
		out = append(out, "DELETE VERTEX "+strings.Join(valueParts, ", ")+" WITH EDGE;")
	}
	return out
}
