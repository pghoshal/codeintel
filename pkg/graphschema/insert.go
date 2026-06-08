package graphschema

import (
	"strconv"
	"strings"
)

// This file ports the vertex / edge / snapshot insert path from
// packages/backend/src/codeGraph/nebulaNgql.ts lines 94-142:
//
//   RenderVertexInsert        — TS renderVertexInsert (128-134)
//   RenderEdgeInsert          — TS renderEdgeInsert   (136-142)
//   RenderSnapshotStatements  — TS renderNebulaGraphSnapshotStatements (94-103)
//
// Byte parity is locked by insert_test.go against captured-golden
// outputs from the JS algorithm.

// snapshotBatchSize is the per-INSERT vertex / edge count. The TS
// source hardcodes 250 inline (lines 96 / 99); duplicating the
// magic number into a const here keeps the chunker call site
// readable and gives a single place to update if a future TS
// change rebalances the batch.
const snapshotBatchSize = 250

// RenderVertexInsert emits one INSERT VERTEX statement for the
// supplied batch. The body of the statement is exactly:
//
//	INSERT VERTEX `code_graph_node`(`kind`, `orgId`, ..., `source`)
//	VALUES "vid1": (val, val, ...), "vid2": (val, val, ...);
//
// Property iteration uses the NodeProps slice — NOT vertex.Properties
// map iteration — so the column order is deterministic regardless
// of Go's map-iteration randomisation. An absent property in the
// map renders as NULL (matching the JS `?? null` behaviour).
//
// Empty input is a writer bug; the JS source also produces a
// malformed statement (`... VALUES ;`) which graphd rejects.
// The Go port preserves that behaviour rather than swallowing the
// case silently.
func RenderVertexInsert(vertices []CodeGraphVertex) string {
	parts := make([]string, 0, len(vertices))
	for _, v := range vertices {
		valueParts := make([]string, 0, len(NodeProps))
		for _, prop := range NodeProps {
			valueParts = append(valueParts, ngqlValue(v.Properties[prop]))
		}
		parts = append(parts, ngqlValue(v.VID)+": ("+strings.Join(valueParts, ", ")+")")
	}
	colParts := make([]string, 0, len(NodeProps))
	for _, p := range NodeProps {
		colParts = append(colParts, quoteIdentifier(p))
	}
	return "INSERT VERTEX " + quoteIdentifier(NodeTag) + "(" + strings.Join(colParts, ", ") + ") VALUES " + strings.Join(parts, ", ") + ";"
}

// RenderEdgeInsert emits one INSERT EDGE statement for the
// supplied batch. The body of the statement is exactly:
//
//	INSERT EDGE `code_graph_edge`(`kind`, ..., `source`)
//	VALUES "from1"->"to1"@rank1: (val, ...), "from2"->"to2"@rank2: (val, ...);
//
// Rank is the disambiguation suffix nebula's INSERT EDGE syntax
// requires — see edgeRank() / EdgeRank() for how callers derive
// it. Iteration drives column order off the EdgeProps slice
// (same rationale as RenderVertexInsert).
func RenderEdgeInsert(edges []CodeGraphEdge) string {
	parts := make([]string, 0, len(edges))
	for _, e := range edges {
		valueParts := make([]string, 0, len(EdgeProps))
		for _, prop := range EdgeProps {
			valueParts = append(valueParts, ngqlValue(e.Properties[prop]))
		}
		parts = append(parts,
			ngqlValue(e.FromVID)+"->"+ngqlValue(e.ToVID)+"@"+strconv.FormatInt(e.Rank, 10)+
				": ("+strings.Join(valueParts, ", ")+")")
	}
	colParts := make([]string, 0, len(EdgeProps))
	for _, p := range EdgeProps {
		colParts = append(colParts, quoteIdentifier(p))
	}
	return "INSERT EDGE " + quoteIdentifier(EdgeType) + "(" + strings.Join(colParts, ", ") + ") VALUES " + strings.Join(parts, ", ") + ";"
}

// RenderSnapshotStatements composes the full bootstrap-and-write
// statement sequence for one snapshot:
//
//  1. The schema statements (RenderSchemaStatements).
//  2. ceil(len(vertices)/250) INSERT VERTEX statements.
//  3. ceil(len(edges)/250)    INSERT EDGE statements.
//
// Direct port of the TS:
//
//	export const renderNebulaGraphSnapshotStatements = (snapshot) => {
//	    const statements = [...renderNebulaGraphSchemaStatements()];
//	    for (const chunk of chunkArray(snapshot.vertices, 250)) {
//	        statements.push(renderVertexInsert(chunk));
//	    }
//	    for (const chunk of chunkArray(snapshot.edges, 250)) {
//	        statements.push(renderEdgeInsert(chunk));
//	    }
//	    return statements;
//	};
//
// The empty-vertices / empty-edges paths produce schema-only
// output (5 statements), matching the JS behaviour where
// chunkArray returns [] and the for-of loop runs zero times.
func RenderSnapshotStatements(snapshot CodeGraphSnapshot) []string {
	return RenderSnapshotStatementsWithBatchSize(snapshot, snapshotBatchSize)
}

// RenderSnapshotStatementsWithBatchSize is the operational variant used by
// high-volume backend projections that already own their parity boundary.
// RenderSnapshotStatements keeps the legacy TypeScript batch size of 250 for
// byte-parity tests; this helper lets real SCIP graph projections reduce
// graphd round trips without changing the default ported behavior.
func RenderSnapshotStatementsWithBatchSize(snapshot CodeGraphSnapshot, batchSize int) []string {
	if batchSize <= 0 {
		batchSize = snapshotBatchSize
	}
	schema := RenderSchemaStatements()
	statements := make([]string, 0, len(schema)+
		(len(snapshot.Vertices)+batchSize-1)/batchSize+
		(len(snapshot.Edges)+batchSize-1)/batchSize)
	statements = append(statements, schema...)
	for _, chunk := range chunkArray(snapshot.Vertices, batchSize) {
		statements = append(statements, RenderVertexInsert(chunk))
	}
	for _, chunk := range chunkArray(snapshot.Edges, batchSize) {
		statements = append(statements, RenderEdgeInsert(chunk))
	}
	return statements
}
