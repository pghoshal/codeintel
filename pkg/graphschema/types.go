package graphschema

import "context"

// Types ported from packages/backend/src/codeGraph/types.ts.
// The TS source uses TypeScript's structural typing; Go uses
// nominal types. Where TS declares `type X = ...` we mirror with
// a Go type and (for the unions) document the allowed
// underlying-type set.
//
// Only the types needed for the schema bootstrap + vertex/edge
// insert paths land in this slice. CodeGraphSnapshotAnchor,
// CodeGraphSnapshotSemantic, CodeGraphWriteResult, CodeGraphStore,
// and CodeGraphDeleteInput port in follow-up slices alongside the
// store-interface implementation.

// CodeGraphPrimitive is the union of value types that can land
// in a vertex / edge property map. TS source:
//
//	export type CodeGraphPrimitive = string | number | boolean | null;
//
// Go has no native union type. The renderers (ngqlValue, etc.)
// type-switch on the underlying kind; nil maps to NULL, strings
// get JSON-quoted, numbers and booleans render their literal nGQL
// form.
type CodeGraphPrimitive any

// CodeGraphConfidenceTier mirrors the TS literal-union:
//
//	export type CodeGraphConfidenceTier =
//	    "EXTRACTED" | "INFERRED" | "AMBIGUOUS";
//
// The values are wire-frozen — they land verbatim in the
// `confidenceTier` property on every vertex / edge / anchor.
type CodeGraphConfidenceTier string

const (
	ConfidenceTierExtracted CodeGraphConfidenceTier = "EXTRACTED"
	ConfidenceTierInferred  CodeGraphConfidenceTier = "INFERRED"
	ConfidenceTierAmbiguous CodeGraphConfidenceTier = "AMBIGUOUS"
)

// CodeGraphVertex is one row to be INSERT-ed under the
// code_graph_node tag. TS source:
//
//	export type CodeGraphVertex = {
//	    vid: string;
//	    kind: CodeGraphNodeKind;
//	    properties: Record<string, CodeGraphPrimitive>;
//	};
//
// Kind is a string here; the TS source narrows to the
// CodeGraphNodeKind enum (ported in a follow-up slice). Properties
// is iterated via the NodeProps slice — NOT via the map keys — so
// Go's map iteration order is irrelevant.
type CodeGraphVertex struct {
	VID        string
	Kind       string
	Properties map[string]CodeGraphPrimitive
}

// CodeGraphEdge is one row to be INSERT-ed under the
// code_graph_edge edge type. TS source:
//
//	export type CodeGraphEdge = {
//	    fromVid: string;
//	    toVid: string;
//	    kind: string;
//	    rank: number;
//	    properties: Record<string, CodeGraphPrimitive>;
//	};
//
// rank is the int64 form of the sha256-derived collision-avoidance
// suffix the renderer attaches at the @rank position in INSERT
// EDGE values. See edgeRank().
type CodeGraphEdge struct {
	FromVID    string
	ToVID      string
	Kind       string
	Rank       int64
	Properties map[string]CodeGraphPrimitive
}

// CodeGraphSnapshot is the bag of vertices + edges + anchors the
// writer path consumes. Mirrors `packages/backend/src/codeGraph/
// types.ts` CodeGraphSnapshot. The `Semantic` sub-field (CodeGraphSnapshotSemantic
// — LLM extraction batches, accepted/rejected nodes, etc.) ports
// alongside the semantic-extraction slice; this struct only
// carries what the renderer + store paths need.
type CodeGraphSnapshot struct {
	OrgID          int64
	WorkspaceID    string
	RepoID         int64
	Revision       string
	CommitHash     string
	SchemaVersion  int64
	BuilderVersion string
	Vertices       []CodeGraphVertex
	Edges          []CodeGraphEdge
	Anchors        []CodeGraphSnapshotAnchor
}

// CodeGraphSnapshotAnchor mirrors the inline anchor type at
// `packages/backend/src/codeGraph/types.ts` lines 21-32. Anchors
// label specific node-vid endpoints with a kind + direction (the
// indexer's framework-route detector emits these so the
// downstream linker can connect "this route provides /api/users"
// to "this handler consumes /api/users"). The store uses the
// anchor count only — full anchor processing ports with the
// linker slice.
type CodeGraphSnapshotAnchor struct {
	Kind             string
	Direction        string // "PROVIDES" | "CONSUMES" | "REFERENCES"
	Key              string
	NodeVID          string
	EvidenceFilePath *string
	StartLine        *int64
	EndLine          *int64
	Confidence       float64
	ConfidenceTier   CodeGraphConfidenceTier
	Source           string
}

// CodeGraphWriteResult is the return value of CodeGraphStore.WriteSnapshot.
// Mirrors `types.ts` lines 109-116. Status discriminates the three
// terminal outcomes:
//
//   - READY    — vertices + edges landed; counts are accurate.
//   - SKIPPED  — store wasn't configured (no nebula address); the
//                snapshot was dropped on the floor.
//   - FAILED   — schema setup or insert raised an error after
//                retry exhaustion; ErrorMessage carries the
//                operator-facing diagnostic.
//
// AnchorCount + LinkedEdgeCount are derived during the write —
// see countLinkedEdges in store.go.
type CodeGraphWriteResult struct {
	Status          CodeGraphWriteStatus
	VertexCount     int64
	EdgeCount       int64
	AnchorCount     int64
	LinkedEdgeCount int64
	ErrorMessage    string
}

// CodeGraphWriteStatus is the TS literal-union
// `"READY" | "SKIPPED" | "FAILED"` from types.ts line 110.
type CodeGraphWriteStatus string

const (
	WriteStatusReady   CodeGraphWriteStatus = "READY"
	WriteStatusSkipped CodeGraphWriteStatus = "SKIPPED"
	WriteStatusFailed  CodeGraphWriteStatus = "FAILED"
)

// CodeGraphStore is the writer contract every storage backend
// implements. Mirrors `packages/backend/src/codeGraph/types.ts`
// lines 118-128.
//
// Two implementations exist today:
//   - internal/backend/graphstore.NebulaCodeGraphStore — the
//     production write-to-Nebula path.
//   - internal/backend/graphstore.UnconfiguredCodeGraphStore —
//     no-op for deployments without a configured graph
//     backend; returns SKIPPED.
//
// The interface lives in pkg/graphschema so test code in either
// binary can construct a fake; production wiring stays in
// internal/backend/graphstore.
type CodeGraphStore interface {
	// WriteSnapshot persists the supplied snapshot's vertices +
	// edges. Returns the per-snapshot write result; the bool
	// error return is the operator-visible diagnostic surface
	// (test failures, transport errors, etc.).
	WriteSnapshot(ctx context.Context, snapshot CodeGraphSnapshot) (CodeGraphWriteResult, error)

	// MarkSnapshotForDeletion retires every vertex matching the
	// supplied scope tuple. Idempotent: a tuple that resolves to
	// zero vertices is a no-op.
	MarkSnapshotForDeletion(ctx context.Context, input CodeGraphDeleteInput) error
}

// CodeGraphDeleteInput is the scope tuple that identifies one
// snapshot for deletion. Mirrors the inline object literal at
// packages/backend/src/codeGraph/types.ts lines 120-127 (the
// markSnapshotForDeletion parameter shape); every field is
// required.
//
// RenderLookupSnapshotVerticesStatement consumes this directly to
// build the WHERE clause that scopes a LOOKUP / DELETE.
//
// Callers MUST enforce tenant-scoping at the policy layer above
// — populating OrgID/WorkspaceID from authenticated request
// context, never from un-validated client input. The renderer
// itself is policy-agnostic and faithfully serialises whatever
// tuple it receives.
type CodeGraphDeleteInput struct {
	OrgID          int64
	WorkspaceID    string
	RepoID         int64
	CommitHash     string
	SchemaVersion  int64
	BuilderVersion string
}
