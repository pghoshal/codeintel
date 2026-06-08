//! Fact → CodeGraphSnapshot builder. Verbatim port of
//! `packages/backend/src/codeGraph/scipBuilder.ts:257-418`
//! (the AST-fact subset: `addFileVertex`, `addVertex`,
//! `addEdge`, `addLanguageAstFacts`).
//!
//! This replaces the failed R.9k-ii attempt that invented a
//! mapping from the NGQL renderer's NODE_PROPS column list
//! rather than reading the legacy producer. Critic gate
//! caught 7 byte-parity defects on that attempt; reverted.
//! This time the port mirrors legacy line-by-line:
//!
//!   - Edge kinds UPPERCASE (DEFINES/CALLS/IMPORTS_FROM/
//!     EXTENDS/IMPLEMENTS).
//!   - `source = "ast-<language>"` per-fact.
//!   - Per-batch `confidenceTier` with 0.8 threshold (no
//!     AMBIGUOUS tier).
//!   - Target-kind remap (external→package, interface→class,
//!     module→file, variable+property→symbol).
//!   - Source-kind remap (class→class, method→method, else
//!     →function); module sources coalesce into the file
//!     vertex.
//!   - `edge_rank` input = `${from}->${to}:${kind}:${source}`
//!     (matches `scipBuilder.ts:316`).
//!   - `normalizedKey` NULL on AST edges (only anchor-link
//!     edges set it).
//!   - First-fact-wins for duplicate VIDs.
//!   - Edge dedupe key: `${from}|${to}|${kind}|${rank}`.
//!
//! The output is a `GraphAccumulator { vertices, edges }`
//! that converts to `nebula_ngql::CodeGraphSnapshot` via
//! `accumulator_to_snapshot`.

use std::collections::BTreeMap;

use crate::ast_extractor::{FactKind, SymbolKind, SyntacticAstFact};
use crate::code_graph_model::{
    build_code_graph_vertex_id, get_builder_version, get_schema_version, normalize_token,
    CodeGraphScope, CodeGraphScopeError, CodeGraphVertexIdentity,
};
use crate::nebula_ngql::{
    edge_rank, CodeGraphEdge, CodeGraphPrimitive, CodeGraphSnapshot, CodeGraphSnapshotAnchor,
    CodeGraphVertex,
};
use crate::scip::{ScipIngestRows, ScipSymbolRow};

/// Provenance tier strings written into the `provenance` column
/// on every code-graph vertex + edge. Q.A adds this dimension
/// so the retrieval layer can rank semantic SCIP edges above
/// regex-grade AST edges.
pub const PROVENANCE_SCIP: &str = "scip";
pub const PROVENANCE_HEURISTIC: &str = "heuristic";

/// Confidence floor for SCIP-derived edges/vertices. SCIP
/// indexers produce typed, compiler-grade semantic edges —
/// definition/reference/implementation/type_definition — that
/// rank above regex-grade AST extractor output (0.6). The
/// 0.95 floor leaves a tiny gap below 1.0 so AST-derived
/// "definition" edges (which can be 0.99 from the syntactic
/// extractor's batch tier) don't trivially lose to a SCIP
/// "reference" — keeping the per-kind quality signal preserved.
pub const SCIP_EDGE_CONFIDENCE: f64 = 0.95;
pub const SCIP_VERTEX_CONFIDENCE: f64 = 0.95;

/// Edge context strings — Q.B vocabulary.
///
/// These are the values written into the new `context` column
/// on every edge. The Go reader infers context filters from
/// the user's NL query (graphify `_CONTEXT_HINTS` LUT port —
/// see `internal/graphreader/nl_context.go`) and restricts
/// traversal to edges whose context matches. Deliberately
/// small, behaviour-oriented vocabulary so NL parsing has a
/// tractable target.
pub const CONTEXT_CALL: &str = "call";
pub const CONTEXT_DEFINITION: &str = "definition";
pub const CONTEXT_REFERENCE: &str = "reference";
pub const CONTEXT_IMPORT: &str = "import";
pub const CONTEXT_INHERITS: &str = "inherits";
pub const CONTEXT_TYPE: &str = "type";
pub const CONTEXT_CONTAINMENT: &str = "containment";
pub const ANCHOR_LINK_SOURCE: &str = "anchor-linker";
const MAX_ANCHOR_LINK_EDGES: usize = 10_000;

/// default_context_for_edge_kind maps a CodeGraph edge kind to
/// its canonical context string. The caller (AST projector,
/// SCIP projector, etc.) can override via
/// `AddEdgeOptions.context` when it knows more — but the
/// default keeps every edge tagged so traversals don't need
/// special-case branches for "this kind has no context".
///
/// Q.B vocabulary fits the AST + SCIP edges produced by the
/// current indexer. Future edges (SET_FIELD, READ_VAR, etc.)
/// will extend this table.
pub fn default_context_for_edge_kind(kind: &str) -> Option<&'static str> {
    match kind {
        "CALLS" => Some(CONTEXT_CALL),
        "DEFINES" => Some(CONTEXT_DEFINITION),
        "REFERENCES" => Some(CONTEXT_REFERENCE),
        "IMPORTS_FROM" => Some(CONTEXT_IMPORT),
        "EXTENDS" | "IMPLEMENTS" => Some(CONTEXT_INHERITS),
        "TYPE_DEFINES" => Some(CONTEXT_TYPE),
        "CONTAINS" => Some(CONTEXT_CONTAINMENT),
        _ => None,
    }
}

/// GraphAccumulator mirrors `scipBuilder.ts:41-50`. Wraps two
/// keyed maps so dedupe is O(log N) per insert and final
/// iteration order is stable.
#[derive(Debug, Clone, Default)]
pub struct GraphAccumulator {
    /// Keyed by VID. First write wins for duplicate VIDs —
    /// matches legacy line 281 `if (!graph.vertices.has(vid))`.
    pub vertices: BTreeMap<String, CodeGraphVertex>,
    /// Keyed by `${from}|${to}|${kind}|${rank}`. Same
    /// first-write-wins dedupe.
    pub edges: BTreeMap<String, CodeGraphEdge>,
    /// Keyed by kind|direction|normalizedKey|nodeVid. Anchors are
    /// persisted in Postgres and used as graph retrieval seeds; they
    /// are not rendered into Nebula statements directly.
    pub anchors: BTreeMap<String, CodeGraphSnapshotAnchor>,
}

/// AddEdgeOptions mirrors the legacy options bag at lines
/// 308-314 — every field optional. Confidence defaults to
/// 1, confidenceTier to "EXTRACTED", line numbers + evidence
/// path to NULL.
///
/// **Q.A additions**: `provenance` defaults to "heuristic"
/// (regex/AST origin); SCIP-projected edges pass "scip" to
/// override. `context` defaults to NULL until Q.B populates
/// it.
#[derive(Debug, Clone, Default)]
pub struct AddEdgeOptions {
    pub confidence: Option<f64>,
    pub confidence_tier: Option<String>,
    pub evidence_file_path: Option<String>,
    pub start_line: Option<i64>,
    pub end_line: Option<i64>,
    /// "scip" | "heuristic" | "tree-sitter". Defaults to
    /// `PROVENANCE_HEURISTIC` when None — every existing AST
    /// regex caller is treated as heuristic without code change.
    pub provenance: Option<String>,
    /// Syntactic context of the edge. Populated by Q.B; left
    /// None means the column is rendered as NULL.
    pub context: Option<String>,
    /// Anchor-linker edges carry the stable normalized relation key
    /// used by Postgres and downstream graph evidence. Ordinary
    /// AST/SCIP edges leave this unset.
    pub normalized_key: Option<String>,
}

/// add_vertex mirrors `scipBuilder.ts:273-299`.
///
/// Builds the deterministic VID from (scope, kind, key),
/// stamps the scope tags (orgId/workspaceId/repoId/revision/
/// commitHash/schemaVersion/builderVersion) into the
/// property bag, then merges the caller's `properties` (the
/// caller's keys override the scope tags on collision —
/// matches the legacy `...properties` spread which lands
/// AFTER the scope tags).
///
/// First-fact-wins: if the VID is already present in the
/// accumulator, no overwrite. Returns the VID.
pub fn add_vertex(
    graph: &mut GraphAccumulator,
    scope: &CodeGraphScope,
    kind: &str,
    key: &str,
    properties: BTreeMap<String, CodeGraphPrimitive>,
) -> Result<String, CodeGraphScopeError> {
    let identity = CodeGraphVertexIdentity {
        scope: scope.clone(),
        kind: kind.to_string(),
        key: key.to_string(),
    };
    let vid = build_code_graph_vertex_id(&identity)?;
    if graph.vertices.contains_key(&vid) {
        return Ok(vid);
    }
    let mut bag = BTreeMap::new();
    // Scope tags (legacy lines 286-294). Order: kind, scope-
    // 6, then ...properties (overrides allowed).
    bag.insert(
        "kind".to_string(),
        CodeGraphPrimitive::String(kind.to_string()),
    );
    bag.insert("orgId".to_string(), CodeGraphPrimitive::Int(scope.org_id));
    bag.insert(
        "workspaceId".to_string(),
        CodeGraphPrimitive::String(scope.workspace_id.clone()),
    );
    bag.insert("repoId".to_string(), CodeGraphPrimitive::Int(scope.repo_id));
    bag.insert(
        "revision".to_string(),
        CodeGraphPrimitive::String(scope.revision.clone()),
    );
    bag.insert(
        "commitHash".to_string(),
        CodeGraphPrimitive::String(scope.commit_hash.clone()),
    );
    bag.insert(
        "schemaVersion".to_string(),
        CodeGraphPrimitive::Int(get_schema_version(scope)),
    );
    bag.insert(
        "builderVersion".to_string(),
        CodeGraphPrimitive::String(get_builder_version(scope)),
    );
    // Q.A — default `provenance` to "heuristic" on every vertex
    // so retrieval can rank by tier even when the producer
    // didn't set it. SCIP-side callers override via the
    // properties bag (caller-spread happens AFTER this insert).
    bag.insert(
        "provenance".to_string(),
        CodeGraphPrimitive::String(PROVENANCE_HEURISTIC.to_string()),
    );
    // Apply caller properties last so they override on key
    // collision (legacy spread semantics).
    for (k, v) in properties {
        bag.insert(k, v);
    }
    graph.vertices.insert(
        vid.clone(),
        CodeGraphVertex {
            vid: vid.clone(),
            kind: kind.to_string(),
            properties: bag,
        },
    );
    Ok(vid)
}

/// add_edge mirrors `scipBuilder.ts:301-342`.
///
/// Builds the edge rank via `edge_rank("${from}->${to}:${kind}:${source}")`
/// then keys the dedupe map by `${from}|${to}|${kind}|${rank}`.
/// First-write-wins on collision.
///
/// NOTE: `normalizedKey` is NOT written for AST edges (it's
/// only set on anchor-link edges by `manager.ts`). The Rust
/// port mirrors that by omitting the property from the bag —
/// `nebula_ngql::render_edge_insert` fills missing keys with
/// NULL.
#[allow(clippy::too_many_arguments)]
pub fn add_edge(
    graph: &mut GraphAccumulator,
    scope: &CodeGraphScope,
    from_vid: &str,
    to_vid: &str,
    kind: &str,
    source: &str,
    options: &AddEdgeOptions,
) {
    let rank_input = format!("{}->{}:{}:{}", from_vid, to_vid, kind, source);
    let rank = edge_rank(&rank_input);
    let edge_key = format!("{}|{}|{}|{}", from_vid, to_vid, kind, rank);
    if graph.edges.contains_key(&edge_key) {
        return;
    }

    let mut bag = BTreeMap::new();
    bag.insert(
        "kind".to_string(),
        CodeGraphPrimitive::String(kind.to_string()),
    );
    bag.insert("orgId".to_string(), CodeGraphPrimitive::Int(scope.org_id));
    bag.insert(
        "workspaceId".to_string(),
        CodeGraphPrimitive::String(scope.workspace_id.clone()),
    );
    bag.insert("repoId".to_string(), CodeGraphPrimitive::Int(scope.repo_id));
    bag.insert(
        "revision".to_string(),
        CodeGraphPrimitive::String(scope.revision.clone()),
    );
    bag.insert(
        "commitHash".to_string(),
        CodeGraphPrimitive::String(scope.commit_hash.clone()),
    );
    bag.insert(
        "schemaVersion".to_string(),
        CodeGraphPrimitive::Int(get_schema_version(scope)),
    );
    bag.insert(
        "builderVersion".to_string(),
        CodeGraphPrimitive::String(get_builder_version(scope)),
    );
    bag.insert(
        "confidence".to_string(),
        CodeGraphPrimitive::Double(options.confidence.unwrap_or(1.0)),
    );
    bag.insert(
        "confidenceTier".to_string(),
        CodeGraphPrimitive::String(
            options
                .confidence_tier
                .clone()
                .unwrap_or_else(|| "EXTRACTED".to_string()),
        ),
    );
    bag.insert(
        "evidenceFilePath".to_string(),
        options
            .evidence_file_path
            .clone()
            .map(CodeGraphPrimitive::String)
            .unwrap_or(CodeGraphPrimitive::Null),
    );
    bag.insert(
        "startLine".to_string(),
        options
            .start_line
            .map(CodeGraphPrimitive::Int)
            .unwrap_or(CodeGraphPrimitive::Null),
    );
    bag.insert(
        "endLine".to_string(),
        options
            .end_line
            .map(CodeGraphPrimitive::Int)
            .unwrap_or(CodeGraphPrimitive::Null),
    );
    bag.insert(
        "source".to_string(),
        CodeGraphPrimitive::String(source.to_string()),
    );
    if let Some(normalized_key) = options.normalized_key.as_deref() {
        bag.insert(
            "normalizedKey".to_string(),
            CodeGraphPrimitive::String(normalized_key.to_string()),
        );
    }
    // Q.A — provenance defaults to "heuristic" so the regex
    // extractors (all existing callers) need zero changes to
    // land in the right tier. SCIP callers override via
    // AddEdgeOptions.provenance.
    bag.insert(
        "provenance".to_string(),
        CodeGraphPrimitive::String(
            options
                .provenance
                .clone()
                .unwrap_or_else(|| PROVENANCE_HEURISTIC.to_string()),
        ),
    );
    // Q.B — `context` is populated from
    // `options.context` if supplied, else derived from the edge
    // kind via `default_context_for_edge_kind` (so every CALLS
    // edge gets "call", every IMPLEMENTS gets "inherits", etc.
    // without each caller having to remember). Falls through to
    // NULL only for edge kinds with no canonical context. The
    // Go reader filters traversals on this column based on the
    // user's NL query.
    let derived_context = options
        .context
        .clone()
        .or_else(|| default_context_for_edge_kind(kind).map(str::to_string));
    bag.insert(
        "context".to_string(),
        derived_context
            .map(CodeGraphPrimitive::String)
            .unwrap_or(CodeGraphPrimitive::Null),
    );

    graph.edges.insert(
        edge_key,
        CodeGraphEdge {
            from_vid: from_vid.to_string(),
            to_vid: to_vid.to_string(),
            kind: kind.to_string(),
            rank,
            properties: bag,
        },
    );
}

/// add_file_vertex mirrors `scipBuilder.ts:257-271`. Creates
/// a `file` kind vertex keyed by `file:<filePath>` and a
/// `CONTAINS` edge from `repo_vid → fileVid` with
/// `source="scip"`.
pub fn add_file_vertex(
    graph: &mut GraphAccumulator,
    scope: &CodeGraphScope,
    repo_vid: &str,
    file_path: &str,
) -> Result<String, CodeGraphScopeError> {
    let mut props = BTreeMap::new();
    props.insert(
        "key".to_string(),
        CodeGraphPrimitive::String(file_path.to_string()),
    );
    props.insert(
        "label".to_string(),
        CodeGraphPrimitive::String(file_path.to_string()),
    );
    props.insert(
        "path".to_string(),
        CodeGraphPrimitive::String(file_path.to_string()),
    );
    props.insert(
        "source".to_string(),
        CodeGraphPrimitive::String("scip".to_string()),
    );
    let file_vid = add_vertex(graph, scope, "file", &format!("file:{}", file_path), props)?;
    add_edge(
        graph,
        scope,
        repo_vid,
        &file_vid,
        "CONTAINS",
        "scip",
        &AddEdgeOptions::default(),
    );
    Ok(file_vid)
}

/// add_syntactic_language_ast_facts handles the
/// SYNTACTIC subset of `scipBuilder.ts:344-418` —
/// specifically the regex-extractor path (R.9a-g languages).
///
/// **Scope narrowing**: the legacy function also handles
/// `returns | mutates | flows_to` fact kinds and
/// `arrow | constructor` source kinds emitted by the
/// TypeScript compiler-API extractor (legacy
/// `typescriptAstExtractor.ts`). The Rust regex
/// extractors don't emit any of those today — so this port
/// is exhaustive over the regex extractors' `FactKind` /
/// `SymbolKind` unions but does NOT yet cover the TS-
/// extractor surface. The TS port (R.9h) will extend
/// `FactKind` + `SymbolKind` AND this function's match
/// arms when it lands.
///
/// For a batch of facts from one regex language extractor:
///   1. Compute the per-batch `confidenceTier` —
///      `facts[0].confidence >= 0.8 ? "EXTRACTED" : "INFERRED"`.
///   2. For each fact:
///      - Coalesce module-kind source vertices into the file
///        vertex.
///      - Otherwise build a source vertex with kind = class/
///        method/function (per source-kind remap).
///      - Build a target vertex with kind = (target-kind
///        remap: external→package, interface→class, module→
///        file, symbol→symbol passthrough).
///      - Add a `path` property to the target unless the
///        target was external.
///      - Add the source→target edge with kind = UPPERCASE
///        fact kind (DEFINES/CALLS/IMPORTS_FROM/EXTENDS/
///        IMPLEMENTS).
pub fn add_syntactic_language_ast_facts(
    graph: &mut GraphAccumulator,
    scope: &CodeGraphScope,
    repo_vid: &str,
    language: &str,
    facts: &[SyntacticAstFact],
) -> Result<(), CodeGraphScopeError> {
    let source = format!("ast-{}", language);

    // Per-batch confidenceTier — uses facts[0] confidence as
    // the gate. Empty batch → "INFERRED" (matches the
    // length-zero short-circuit at legacy line 362).
    let confidence_tier = if !facts.is_empty() && facts[0].confidence >= 0.8 {
        "EXTRACTED".to_string()
    } else {
        "INFERRED".to_string()
    };

    // File vertex cache (legacy line 352 `fileVids`).
    let mut file_vids: BTreeMap<String, String> = BTreeMap::new();

    for fact in facts {
        // Ensure file vertex.
        let file_vid = if let Some(existing) = file_vids.get(&fact.file_path) {
            existing.clone()
        } else {
            let v = add_file_vertex(graph, scope, repo_vid, &fact.file_path)?;
            file_vids.insert(fact.file_path.clone(), v.clone());
            v
        };

        // ── Source vertex / VID ────────────────────────────
        let source_vid = if fact.source_kind == SymbolKind::Module {
            // Coalesce: the source IS the file vertex.
            file_vid.clone()
        } else {
            // Source-kind remap (legacy line 369):
            // class → "class", method → "method", else → "function".
            let source_vertex_kind = match fact.source_kind {
                SymbolKind::Class => "class",
                SymbolKind::Method => "method",
                _ => "function",
            };
            let key = format!("ast:{}", fact.source_symbol);
            let mut props = BTreeMap::new();
            props.insert(
                "key".to_string(),
                CodeGraphPrimitive::String(fact.source_symbol.clone()),
            );
            props.insert(
                "label".to_string(),
                CodeGraphPrimitive::String(fact.source_display_name.clone()),
            );
            props.insert(
                "path".to_string(),
                CodeGraphPrimitive::String(fact.file_path.clone()),
            );
            props.insert(
                "language".to_string(),
                CodeGraphPrimitive::String(language.to_string()),
            );
            props.insert(
                "source".to_string(),
                CodeGraphPrimitive::String(source.clone()),
            );
            props.insert(
                "confidence".to_string(),
                CodeGraphPrimitive::Double(fact.confidence),
            );
            props.insert(
                "confidenceTier".to_string(),
                CodeGraphPrimitive::String(confidence_tier.clone()),
            );
            props.insert(
                "evidenceFilePath".to_string(),
                CodeGraphPrimitive::String(fact.file_path.clone()),
            );
            add_vertex(graph, scope, source_vertex_kind, &key, props)?
        };

        // ── Target vertex / VID ────────────────────────────
        let target_key = format!("ast:{}", fact.target_symbol);
        // Target-kind remap (legacy lines 381-386).
        // The legacy enum is `function | method | class |
        // module | external | interface | symbol | variable
        // | property`. The remap is:
        //   external  → package
        //   interface → class
        //   module    → file
        //   variable  → symbol  (TS-extractor-only)
        //   property  → symbol  (TS-extractor-only)
        //   else      → as-is
        // Rust `SymbolKind` is `Function | Method | Class |
        // Module | External | Symbol | Interface` — the
        // `Symbol` variant exists as a passthrough sink
        // because the syntactic Python extractor emits
        // `Symbol` for `from X import Y` symbol references.
        // The `Variable | Property` variants are TS-extractor-
        // only (R.9h) and will be added when that slice lands.
        let target_kind = match fact.target_kind {
            SymbolKind::External => "package",
            SymbolKind::Interface => "class",
            SymbolKind::Module => "file",
            // Passthrough for kinds the regex extractors emit
            // verbatim — function/method/class/symbol.
            SymbolKind::Function => "function",
            SymbolKind::Method => "method",
            SymbolKind::Class => "class",
            SymbolKind::Symbol => "symbol",
        };
        let mut target_props = BTreeMap::new();
        target_props.insert(
            "key".to_string(),
            CodeGraphPrimitive::String(fact.target_symbol.clone()),
        );
        target_props.insert(
            "label".to_string(),
            CodeGraphPrimitive::String(fact.target_display_name.clone()),
        );
        target_props.insert(
            "language".to_string(),
            CodeGraphPrimitive::String(language.to_string()),
        );
        target_props.insert(
            "source".to_string(),
            CodeGraphPrimitive::String(source.clone()),
        );
        target_props.insert(
            "confidence".to_string(),
            CodeGraphPrimitive::Double(fact.confidence),
        );
        target_props.insert(
            "confidenceTier".to_string(),
            CodeGraphPrimitive::String(confidence_tier.clone()),
        );
        target_props.insert(
            "evidenceFilePath".to_string(),
            CodeGraphPrimitive::String(fact.file_path.clone()),
        );
        // `path` only added when targetKind !== "external"
        // (legacy line 396-398).
        if fact.target_kind != SymbolKind::External {
            target_props.insert(
                "path".to_string(),
                CodeGraphPrimitive::String(fact.file_path.clone()),
            );
        }
        let target_vid = add_vertex(graph, scope, target_kind, &target_key, target_props)?;

        // ── Edge kind UPPERCASE (legacy lines 401-408) ─────
        let edge_kind = match fact.kind {
            FactKind::Defines => "DEFINES",
            FactKind::Calls => "CALLS",
            FactKind::ImportsFrom => "IMPORTS_FROM",
            FactKind::Extends => "EXTENDS",
            FactKind::Implements => "IMPLEMENTS",
        };

        add_edge(
            graph,
            scope,
            &source_vid,
            &target_vid,
            edge_kind,
            &source,
            &AddEdgeOptions {
                confidence: Some(fact.confidence),
                confidence_tier: Some(confidence_tier.clone()),
                evidence_file_path: Some(fact.file_path.clone()),
                start_line: Some(fact.start_line as i64),
                end_line: Some(fact.end_line as i64),
                // Q.A — AST regex output is the "heuristic" tier.
                // The default in add_edge would have picked this
                // up too; passing it explicitly makes the source
                // of this tier visible at the call site.
                provenance: Some(PROVENANCE_HEURISTIC.to_string()),
                context: None,
                normalized_key: None,
            },
        );
    }
    Ok(())
}

/// accumulator_to_snapshot converts the BTreeMap-keyed
/// accumulator to a Vec-backed CodeGraphSnapshot ready for
/// the NGQL renderer. Iteration order is BTreeMap's natural
/// sorted order so output is deterministic across runs.
pub fn accumulator_to_snapshot(graph: GraphAccumulator) -> CodeGraphSnapshot {
    CodeGraphSnapshot {
        vertices: graph.vertices.into_values().collect(),
        edges: graph.edges.into_values().collect(),
    }
}

/// accumulator_to_snapshot_and_anchors is the production split-index
/// handoff. It preserves the legacy NGQL snapshot while also emitting
/// the Postgres-backed anchor sidecar and bounded ANCHOR_LINK edges.
pub fn accumulator_to_snapshot_and_anchors(
    mut graph: GraphAccumulator,
) -> (CodeGraphSnapshot, Vec<CodeGraphSnapshotAnchor>) {
    materialize_graph_anchors(&mut graph);
    let anchors = graph.anchors.into_values().collect();
    (
        CodeGraphSnapshot {
            vertices: graph.vertices.into_values().collect(),
            edges: graph.edges.into_values().collect(),
        },
        anchors,
    )
}

fn materialize_graph_anchors(graph: &mut GraphAccumulator) {
    let mut primary_anchor_by_vid: BTreeMap<String, String> = BTreeMap::new();
    let vertices: Vec<CodeGraphVertex> = graph.vertices.values().cloned().collect();
    for vertex in vertices {
        let Some(anchor_kind) = anchor_kind_for_vertex(&vertex) else {
            continue;
        };
        let key = anchor_key_for_vertex(&vertex);
        if key.is_empty() {
            continue;
        }
        let normalized_key = normalize_token(&key);
        if normalized_key.is_empty() || is_low_signal_anchor_key(&normalized_key) {
            continue;
        }
        let source =
            vertex_string_prop(&vertex, "source").unwrap_or_else(|| "graph-builder".to_string());
        let confidence = vertex_double_prop(&vertex, "confidence").unwrap_or_else(|| {
            if source.starts_with("scip") {
                SCIP_VERTEX_CONFIDENCE
            } else {
                0.6
            }
        });
        let confidence_tier = vertex_string_prop(&vertex, "confidenceTier").unwrap_or_else(|| {
            if confidence >= 0.8 {
                "EXTRACTED"
            } else {
                "INFERRED"
            }
            .to_string()
        });
        let anchor = CodeGraphSnapshotAnchor {
            kind: anchor_kind.to_string(),
            direction: "PROVIDES".to_string(),
            key,
            normalized_key: normalized_key.clone(),
            node_vid: vertex.vid.clone(),
            evidence_file_path: vertex_string_prop(&vertex, "evidenceFilePath")
                .or_else(|| vertex_string_prop(&vertex, "path")),
            start_line: vertex_int_prop(&vertex, "startLine"),
            end_line: vertex_int_prop(&vertex, "endLine"),
            confidence,
            confidence_tier,
            source,
        };
        add_anchor(graph, anchor);
        primary_anchor_by_vid
            .entry(vertex.vid.clone())
            .or_insert(normalized_key);
    }

    let existing_edges: Vec<CodeGraphEdge> = graph.edges.values().cloned().collect();
    let mut emitted = 0usize;
    for edge in existing_edges {
        if emitted >= MAX_ANCHOR_LINK_EDGES {
            break;
        }
        if !edge_kind_can_emit_anchor_link(&edge.kind) {
            continue;
        }
        let Some(from_key) = primary_anchor_by_vid.get(&edge.from_vid) else {
            continue;
        };
        let Some(to_key) = primary_anchor_by_vid.get(&edge.to_vid) else {
            continue;
        };
        if from_key == to_key {
            continue;
        }
        let normalized_key = format!(
            "{}->{}:{}",
            from_key,
            to_key,
            edge.kind.to_ascii_lowercase()
        );
        let confidence = edge_double_prop(&edge, "confidence")
            .unwrap_or(0.75)
            .min(0.9);
        let confidence_tier = edge_string_prop(&edge, "confidenceTier").unwrap_or_else(|| {
            if confidence >= 0.8 {
                "EXTRACTED"
            } else {
                "INFERRED"
            }
            .to_string()
        });
        add_edge(
            graph,
            &scope_from_edge(&edge),
            &edge.from_vid,
            &edge.to_vid,
            "ANCHOR_LINK",
            ANCHOR_LINK_SOURCE,
            &AddEdgeOptions {
                confidence: Some(confidence),
                confidence_tier: Some(confidence_tier),
                evidence_file_path: edge_string_prop(&edge, "evidenceFilePath"),
                start_line: edge_int_prop(&edge, "startLine"),
                end_line: edge_int_prop(&edge, "endLine"),
                provenance: Some(ANCHOR_LINK_SOURCE.to_string()),
                context: edge_string_prop(&edge, "context"),
                normalized_key: Some(normalized_key),
            },
        );
        emitted += 1;
    }
}

fn add_anchor(graph: &mut GraphAccumulator, anchor: CodeGraphSnapshotAnchor) {
    let key = format!(
        "{}|{}|{}|{}",
        anchor.kind, anchor.direction, anchor.normalized_key, anchor.node_vid
    );
    graph.anchors.entry(key).or_insert(anchor);
}

fn anchor_kind_for_vertex(vertex: &CodeGraphVertex) -> Option<&'static str> {
    match vertex.kind.as_str() {
        "file" => Some("file"),
        "function" | "method" | "class" | "symbol" => Some("symbol"),
        _ => None,
    }
}

fn anchor_key_for_vertex(vertex: &CodeGraphVertex) -> String {
    vertex_string_prop(vertex, "label")
        .or_else(|| vertex_string_prop(vertex, "key"))
        .or_else(|| vertex_string_prop(vertex, "path"))
        .unwrap_or_default()
}

fn is_low_signal_anchor_key(normalized: &str) -> bool {
    matches!(
        normalized,
        "print" | "println" | "len" | "format" | "string" | "logger" | "log" | "error"
    )
}

fn edge_kind_can_emit_anchor_link(kind: &str) -> bool {
    matches!(
        kind,
        "CALLS"
            | "REFERENCES"
            | "IMPORTS"
            | "IMPORTS_FROM"
            | "EXTENDS"
            | "IMPLEMENTS"
            | "TYPE_DEFINES"
            | "HANDLES"
            | "EMITS"
            | "PROVIDES"
            | "CONSUMES"
    )
}

fn vertex_string_prop(vertex: &CodeGraphVertex, key: &str) -> Option<String> {
    match vertex.properties.get(key) {
        Some(CodeGraphPrimitive::String(value)) if !value.trim().is_empty() => Some(value.clone()),
        _ => None,
    }
}

fn vertex_double_prop(vertex: &CodeGraphVertex, key: &str) -> Option<f64> {
    match vertex.properties.get(key) {
        Some(CodeGraphPrimitive::Double(value)) if value.is_finite() => Some(*value),
        Some(CodeGraphPrimitive::Int(value)) => Some(*value as f64),
        _ => None,
    }
}

fn vertex_int_prop(vertex: &CodeGraphVertex, key: &str) -> Option<i64> {
    match vertex.properties.get(key) {
        Some(CodeGraphPrimitive::Int(value)) => Some(*value),
        _ => None,
    }
}

fn edge_string_prop(edge: &CodeGraphEdge, key: &str) -> Option<String> {
    match edge.properties.get(key) {
        Some(CodeGraphPrimitive::String(value)) if !value.trim().is_empty() => Some(value.clone()),
        _ => None,
    }
}

fn edge_double_prop(edge: &CodeGraphEdge, key: &str) -> Option<f64> {
    match edge.properties.get(key) {
        Some(CodeGraphPrimitive::Double(value)) if value.is_finite() => Some(*value),
        Some(CodeGraphPrimitive::Int(value)) => Some(*value as f64),
        _ => None,
    }
}

fn edge_int_prop(edge: &CodeGraphEdge, key: &str) -> Option<i64> {
    match edge.properties.get(key) {
        Some(CodeGraphPrimitive::Int(value)) => Some(*value),
        _ => None,
    }
}

fn scope_from_edge(edge: &CodeGraphEdge) -> CodeGraphScope {
    CodeGraphScope {
        org_id: edge_int_prop(edge, "orgId").unwrap_or_default(),
        repo_id: edge_int_prop(edge, "repoId").unwrap_or_default(),
        revision: edge_string_prop(edge, "revision").unwrap_or_default(),
        commit_hash: edge_string_prop(edge, "commitHash").unwrap_or_default(),
        workspace_id: edge_string_prop(edge, "workspaceId").unwrap_or_default(),
        schema_version: edge_int_prop(edge, "schemaVersion"),
        builder_version: edge_string_prop(edge, "builderVersion"),
    }
}

// ─── Q.A: SCIP semantic projection ──────────────────────────────

/// scip_symbol_key builds the deterministic `key` half of a
/// code-graph VID for a SCIP symbol moniker.
///
/// SCIP symbol strings look like
/// `"scip-typescript npm typescript 4.7.4 src/foo.ts \`Foo\`#bar()."`.
/// They are stable across builds and machines, so using them
/// verbatim (after the `scip:` discriminant prefix) means SCIP
/// edges and AST regex edges that happen to reference the same
/// symbol via different syntactic paths produce DIFFERENT VIDs
/// — which is fine: the reconciler dedupes by (from, to, kind)
/// across rank values, not by VID.
fn scip_symbol_key(symbol: &str) -> String {
    format!("scip:{}", symbol)
}

/// scip_kind_to_graph_kind maps the SCIP SymbolKind string
/// (when present on a ScipSymbolRow) into the code-graph vertex
/// kind enum used throughout the indexer.
///
/// Unknown kinds + None fall back to `"symbol"`, which is the
/// AST extractor's passthrough sink — so retrieval can still
/// find the vertex even when the indexer didn't classify it.
fn scip_kind_to_graph_kind(kind: Option<&str>) -> &'static str {
    match kind.map(str::to_ascii_lowercase).as_deref() {
        Some("class") | Some("struct") | Some("union") => "class",
        Some("interface") | Some("protocol") | Some("trait") => "class",
        Some("method") | Some("constructor") | Some("destructor") => "method",
        Some("function") | Some("staticmethod") => "function",
        Some("module") | Some("namespace") | Some("package") => "file",
        Some("field") | Some("property") | Some("variable") | Some("staticfield") => "symbol",
        Some("type") | Some("typealias") | Some("typeparameter") => "class",
        Some("enum") | Some("enummember") => "class",
        _ => "symbol",
    }
}

/// add_scip_semantic_facts projects a `ScipIngestRows` batch
/// (one per language per branch) into the GraphAccumulator with
/// provenance="scip" and confidence ≥ 0.95.
///
/// **Why this exists (Q.A)**: the SCIP indexers compute precise
/// definition/reference/implementation/type_definition edges
/// per symbol via compiler-grade analysis. The legacy
/// pipeline persisted those rows to Postgres but NEVER fed
/// them into the Nebula code-graph. Every Nebula edge was
/// 0.6-tier AST regex output. Retrieval against the graph
/// degraded to keyword search at best, polluting Zoekt
/// top-K with regex-matched call edges to `print()` / `len()`
/// / `fmt.Println()`.
///
/// This pass projects SCIP relationship rows so that Nebula
/// hops "callers of X", "implementations of interface Y",
/// "types of variable Z" land on real semantic edges, ranked
/// above the heuristic regex peers via [`reconcile_edge_provenance`].
///
/// Edge mapping:
///   - is_implementation → IMPLEMENTS
///   - is_type_definition → TYPE_DEFINES
///   - is_reference       → REFERENCES
///   - is_definition      → DEFINES (rare on the relationship
///                          surface; SCIP indexers normally
///                          attach DEFINITION to occurrences,
///                          but some emit it on relationships
///                          for synthetic inheritance edges).
///
/// `source_symbol` becomes the `from_vid`, `target_symbol` the
/// `to_vid`. Vertices are created on demand from the symbol
/// table (when available — display_name + file_path enrich the
/// property bag) or as bare-key vertices otherwise.
///
/// **Tenant safety**: every vertex + edge is stamped with the
/// caller's scope (orgId/workspaceId/repoId/...). The function
/// does not call out to the network and does not mutate any
/// state outside the supplied accumulator.
pub fn add_scip_semantic_facts(
    graph: &mut GraphAccumulator,
    scope: &CodeGraphScope,
    repo_vid: &str,
    language: &str,
    rows: &ScipIngestRows,
) -> Result<(), CodeGraphScopeError> {
    let source_tag = format!("scip-{}", language);

    // Symbol → SymbolRow index for enrichment. SCIP emits one
    // SymbolInformation per defined symbol; relationships may
    // reference symbols defined in *other* documents (we won't
    // have rows for those — fall back to bare vertex).
    let mut sym_index: BTreeMap<&str, &ScipSymbolRow> = BTreeMap::new();
    for sym in &rows.symbols {
        sym_index.insert(sym.symbol.as_str(), sym);
    }

    // Pre-add a file vertex per known file_path so the CONTAINS
    // chain from repo → file → symbol is intact when graph
    // queries pull the subgraph for a single file.
    let mut file_vids: BTreeMap<String, String> = BTreeMap::new();
    for sym in &rows.symbols {
        if let Some(file_path) = sym.file_path.as_deref() {
            if file_vids.contains_key(file_path) {
                continue;
            }
            let v = add_file_vertex(graph, scope, repo_vid, file_path)?;
            file_vids.insert(file_path.to_string(), v);
        }
    }

    // 1. Emit vertices for every SCIP symbol we know about. The
    //    relationship rows reference symbols by string; pre-
    //    creating the vertex here means the edge pass below
    //    always has a target_vid that resolves to a vertex
    //    with the SymbolRow's display_name and kind.
    let mut symbol_vids: BTreeMap<String, String> = BTreeMap::new();
    for sym in &rows.symbols {
        let kind = scip_kind_to_graph_kind(sym.kind.as_deref());
        let key = scip_symbol_key(&sym.symbol);
        let mut props = BTreeMap::new();
        props.insert(
            "key".to_string(),
            CodeGraphPrimitive::String(sym.symbol.clone()),
        );
        props.insert(
            "label".to_string(),
            CodeGraphPrimitive::String(sym.display_name.clone()),
        );
        if let Some(file_path) = sym.file_path.as_deref() {
            props.insert(
                "path".to_string(),
                CodeGraphPrimitive::String(file_path.to_string()),
            );
            props.insert(
                "evidenceFilePath".to_string(),
                CodeGraphPrimitive::String(file_path.to_string()),
            );
        }
        if let Some(start) = sym.start_line {
            props.insert(
                "startLine".to_string(),
                CodeGraphPrimitive::Int(start as i64),
            );
        }
        if let Some(end) = sym.end_line {
            props.insert("endLine".to_string(), CodeGraphPrimitive::Int(end as i64));
        }
        if let Some(lang) = sym.language.as_deref() {
            props.insert(
                "language".to_string(),
                CodeGraphPrimitive::String(lang.to_string()),
            );
        } else {
            props.insert(
                "language".to_string(),
                CodeGraphPrimitive::String(language.to_string()),
            );
        }
        props.insert(
            "source".to_string(),
            CodeGraphPrimitive::String(source_tag.clone()),
        );
        // Q.A: override the default vertex provenance.
        props.insert(
            "provenance".to_string(),
            CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()),
        );
        props.insert(
            "confidence".to_string(),
            CodeGraphPrimitive::Double(SCIP_VERTEX_CONFIDENCE),
        );
        props.insert(
            "confidenceTier".to_string(),
            CodeGraphPrimitive::String("EXTRACTED".to_string()),
        );
        let vid = add_vertex(graph, scope, kind, &key, props)?;
        symbol_vids.insert(sym.symbol.clone(), vid.clone());

        // CONTAINS edge: file → symbol when we know the file.
        if let Some(file_path) = sym.file_path.as_deref() {
            if let Some(file_vid) = file_vids.get(file_path) {
                add_edge(
                    graph,
                    scope,
                    file_vid,
                    &vid,
                    "CONTAINS",
                    &source_tag,
                    &AddEdgeOptions {
                        confidence: Some(SCIP_EDGE_CONFIDENCE),
                        confidence_tier: Some("EXTRACTED".to_string()),
                        evidence_file_path: Some(file_path.to_string()),
                        provenance: Some(PROVENANCE_SCIP.to_string()),
                        ..Default::default()
                    },
                );
            }
        }
    }

    // 2. Emit edges for each SCIP relationship row.
    for rel in &rows.relationships {
        let source_vid = ensure_scip_symbol_vid(
            graph,
            scope,
            &mut symbol_vids,
            &sym_index,
            &source_tag,
            language,
            &rel.source_symbol,
        )?;
        let target_vid = ensure_scip_symbol_vid(
            graph,
            scope,
            &mut symbol_vids,
            &sym_index,
            &source_tag,
            language,
            &rel.target_symbol,
        )?;

        // Each true flag emits one edge. A relationship row CAN
        // carry multiple flags (rare but legal — e.g., a forward
        // declaration that's both is_reference and is_definition).
        // Emit a distinct edge per kind so retrieval can filter
        // by exact relationship type.
        let evidence = sym_index
            .get(rel.source_symbol.as_str())
            .and_then(|s| s.file_path.clone());

        let edges_to_emit: &[(&str, bool)] = &[
            ("IMPLEMENTS", rel.is_implementation),
            ("TYPE_DEFINES", rel.is_type_definition),
            ("REFERENCES", rel.is_reference),
            ("DEFINES", rel.is_definition),
        ];
        for (kind, enabled) in edges_to_emit {
            if !enabled {
                continue;
            }
            add_edge(
                graph,
                scope,
                &source_vid,
                &target_vid,
                kind,
                &source_tag,
                &AddEdgeOptions {
                    confidence: Some(SCIP_EDGE_CONFIDENCE),
                    confidence_tier: Some("EXTRACTED".to_string()),
                    evidence_file_path: evidence.clone(),
                    provenance: Some(PROVENANCE_SCIP.to_string()),
                    ..Default::default()
                },
            );
        }
    }

    Ok(())
}

/// ensure_scip_symbol_vid resolves a SCIP symbol string to a
/// VID, materialising a bare-key vertex if the symbol is
/// referenced by a relationship but isn't in the symbol table
/// (the typical "external dependency" case — e.g., a Java
/// class implementing `java.lang.Runnable`).
fn ensure_scip_symbol_vid(
    graph: &mut GraphAccumulator,
    scope: &CodeGraphScope,
    symbol_vids: &mut BTreeMap<String, String>,
    sym_index: &BTreeMap<&str, &ScipSymbolRow>,
    source_tag: &str,
    language: &str,
    symbol: &str,
) -> Result<String, CodeGraphScopeError> {
    if let Some(v) = symbol_vids.get(symbol) {
        return Ok(v.clone());
    }
    let kind = sym_index
        .get(symbol)
        .map(|s| scip_kind_to_graph_kind(s.kind.as_deref()))
        .unwrap_or("symbol");
    let key = scip_symbol_key(symbol);
    let mut props = BTreeMap::new();
    props.insert(
        "key".to_string(),
        CodeGraphPrimitive::String(symbol.to_string()),
    );
    props.insert(
        "label".to_string(),
        CodeGraphPrimitive::String(symbol.to_string()),
    );
    props.insert(
        "language".to_string(),
        CodeGraphPrimitive::String(language.to_string()),
    );
    props.insert(
        "source".to_string(),
        CodeGraphPrimitive::String(source_tag.to_string()),
    );
    props.insert(
        "provenance".to_string(),
        CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()),
    );
    props.insert(
        "confidence".to_string(),
        CodeGraphPrimitive::Double(SCIP_VERTEX_CONFIDENCE),
    );
    props.insert(
        "confidenceTier".to_string(),
        CodeGraphPrimitive::String("EXTRACTED".to_string()),
    );
    let vid = add_vertex(graph, scope, kind, &key, props)?;
    symbol_vids.insert(symbol.to_string(), vid.clone());
    Ok(vid)
}

/// reconcile_edge_provenance is the Q.A post-pass that collapses
/// duplicate edges produced by the AST-regex and SCIP projection
/// stages for the same (from_vid, to_vid, kind) triple.
///
/// **Why this is needed**: `add_edge` keys its dedupe map by
/// `${from}|${to}|${kind}|${rank}`, where `rank = SHA-256(${from}
/// ->${to}:${kind}:${source})`. Because `source` differs between
/// AST ("ast-go") and SCIP ("scip-go"), the two edges land at
/// different ranks and BOTH persist in the accumulator. Without
/// this pass, the Nebula graph contains both a 0.6-confidence
/// regex edge and a 0.95-confidence SCIP edge for the same
/// logical CALLS / IMPLEMENTS / etc. relationship — polluting
/// any traversal that expects to see each relationship once.
///
/// **Algorithm**: scan all edges. For each (from, to, kind)
/// triple, keep the highest-confidence peer; drop the rest.
/// Ties break by provenance rank (`scip > heuristic`) — this
/// rule has a single tunable in [`edge_score`] so future
/// tiers (e.g., "tree-sitter") slot in cleanly.
///
/// O(n) where n = number of edges. Idempotent.
pub fn reconcile_edge_provenance(graph: &mut GraphAccumulator) {
    // First pass: score every edge.
    let scored: Vec<(String, (String, String, String), i64)> = graph
        .edges
        .iter()
        .map(|(k, e)| {
            (
                k.clone(),
                (e.from_vid.clone(), e.to_vid.clone(), e.kind.clone()),
                edge_score(e),
            )
        })
        .collect();

    // Second pass: pick the winner per (from, to, kind) triple.
    // Tie-break is by stable key order (BTreeMap iteration is
    // deterministic) so identical-score peers always pick the
    // SAME survivor across runs.
    let mut winner_keys: BTreeMap<(String, String, String), (String, i64)> = BTreeMap::new();
    for (key, triple, score) in scored {
        match winner_keys.get(&triple) {
            Some((_, existing)) if *existing >= score => {}
            _ => {
                winner_keys.insert(triple, (key, score));
            }
        }
    }

    // Third pass: retain only the winning edge keys.
    let kept: std::collections::BTreeSet<String> =
        winner_keys.into_values().map(|(k, _)| k).collect();
    graph.edges.retain(|k, _| kept.contains(k));
}

/// edge_score is the comparable ranking used by
/// [`reconcile_edge_provenance`]. Higher wins.
///
/// Composition: confidence (0..=1, scaled by 1000) plus a small
/// provenance tier bonus so two equal-confidence edges still
/// pick the SCIP one. The bonus is small enough that a 0.7
/// heuristic NEVER beats a 0.95 SCIP edge.
fn edge_score(edge: &CodeGraphEdge) -> i64 {
    let confidence = match edge.properties.get("confidence") {
        Some(CodeGraphPrimitive::Double(d)) if d.is_finite() => (*d * 1000.0) as i64,
        _ => 0,
    };
    let provenance_bonus = match edge.properties.get("provenance") {
        Some(CodeGraphPrimitive::String(s)) if s == PROVENANCE_SCIP => 5,
        Some(CodeGraphPrimitive::String(s)) if s == "tree-sitter" => 3,
        Some(CodeGraphPrimitive::String(s)) if s == PROVENANCE_HEURISTIC => 1,
        _ => 0,
    };
    confidence * 10 + provenance_bonus
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ast_extractor::CONFIDENCE_INFERRED;
    use crate::code_graph_model::DEFAULT_BUILDER_VERSION;

    fn make_scope() -> CodeGraphScope {
        CodeGraphScope {
            org_id: 10,
            repo_id: 5,
            revision: "refs/heads/main".to_string(),
            commit_hash: "a".repeat(40),
            workspace_id: "ws-atom".to_string(),
            schema_version: None,
            builder_version: None,
        }
    }

    fn make_repo_vid(scope: &CodeGraphScope) -> String {
        // Mirror the legacy repo-vertex pattern: kind="repo",
        // key=`repo:<repoId>`.
        build_code_graph_vertex_id(&CodeGraphVertexIdentity {
            scope: scope.clone(),
            kind: "repo".to_string(),
            key: format!("repo:{}", scope.repo_id),
        })
        .unwrap()
    }

    fn fact_calls(
        source_sym: &str,
        source_display: &str,
        source_kind: SymbolKind,
        target_sym: &str,
        target_display: &str,
        target_kind: SymbolKind,
        file: &str,
        line: u32,
    ) -> SyntacticAstFact {
        SyntacticAstFact {
            kind: FactKind::Calls,
            language: "go".to_string(),
            source_symbol: source_sym.to_string(),
            source_display_name: source_display.to_string(),
            source_kind,
            target_symbol: target_sym.to_string(),
            target_display_name: target_display.to_string(),
            target_kind,
            file_path: file.to_string(),
            start_line: line,
            end_line: line,
            confidence: CONFIDENCE_INFERRED,
        }
    }

    #[test]
    fn add_vertex_first_write_wins() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let v1 = add_vertex(
            &mut graph,
            &scope,
            "function",
            "key-1",
            BTreeMap::from_iter([(
                "label".to_string(),
                CodeGraphPrimitive::String("first".to_string()),
            )]),
        )
        .unwrap();
        let v2 = add_vertex(
            &mut graph,
            &scope,
            "function",
            "key-1",
            BTreeMap::from_iter([(
                "label".to_string(),
                CodeGraphPrimitive::String("second".to_string()),
            )]),
        )
        .unwrap();
        assert_eq!(v1, v2, "same VID for same scope+kind+key");
        // The stored vertex should keep the "first" label.
        let stored = graph.vertices.get(&v1).unwrap();
        assert_eq!(
            stored.properties.get("label"),
            Some(&CodeGraphPrimitive::String("first".to_string()))
        );
    }

    #[test]
    fn add_vertex_stamps_scope_tags() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let vid = add_vertex(&mut graph, &scope, "function", "k", BTreeMap::new()).unwrap();
        let v = graph.vertices.get(&vid).unwrap();
        assert_eq!(
            v.properties.get("orgId"),
            Some(&CodeGraphPrimitive::Int(10))
        );
        assert_eq!(
            v.properties.get("workspaceId"),
            Some(&CodeGraphPrimitive::String("ws-atom".to_string()))
        );
        assert_eq!(
            v.properties.get("schemaVersion"),
            Some(&CodeGraphPrimitive::Int(1))
        );
        assert_eq!(
            v.properties.get("builderVersion"),
            Some(&CodeGraphPrimitive::String(
                DEFAULT_BUILDER_VERSION.to_string()
            ))
        );
        assert_eq!(
            v.properties.get("kind"),
            Some(&CodeGraphPrimitive::String("function".to_string()))
        );
    }

    #[test]
    fn add_vertex_caller_properties_override_scope_tags() {
        // Legacy spread: caller's `kind` property wins over
        // the auto-injected `kind` scope tag. Verify.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let vid = add_vertex(
            &mut graph,
            &scope,
            "function",
            "k",
            BTreeMap::from_iter([(
                "kind".to_string(),
                CodeGraphPrimitive::String("OVERRIDE".to_string()),
            )]),
        )
        .unwrap();
        let v = graph.vertices.get(&vid).unwrap();
        assert_eq!(
            v.properties.get("kind"),
            Some(&CodeGraphPrimitive::String("OVERRIDE".to_string()))
        );
    }

    #[test]
    fn add_edge_first_write_wins_uses_full_dedupe_key() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let from = "vid-A";
        let to = "vid-B";
        add_edge(
            &mut graph,
            &scope,
            from,
            to,
            "CALLS",
            "ast-go",
            &AddEdgeOptions {
                confidence: Some(0.6),
                confidence_tier: Some("INFERRED".to_string()),
                ..Default::default()
            },
        );
        add_edge(
            &mut graph,
            &scope,
            from,
            to,
            "CALLS",
            "ast-go",
            &AddEdgeOptions {
                confidence: Some(0.99),
                confidence_tier: Some("EXTRACTED".to_string()),
                ..Default::default()
            },
        );
        assert_eq!(graph.edges.len(), 1, "duplicate edge deduped");
        let e = graph.edges.values().next().unwrap();
        // First-write-wins: keeps 0.6 / INFERRED.
        assert_eq!(
            e.properties.get("confidence"),
            Some(&CodeGraphPrimitive::Double(0.6))
        );
        assert_eq!(
            e.properties.get("confidenceTier"),
            Some(&CodeGraphPrimitive::String("INFERRED".to_string()))
        );

        // Distinct kind makes a separate edge.
        add_edge(
            &mut graph,
            &scope,
            from,
            to,
            "IMPORTS_FROM",
            "ast-go",
            &AddEdgeOptions::default(),
        );
        assert_eq!(graph.edges.len(), 2);
    }

    #[test]
    fn add_edge_rank_uses_legacy_input_format() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CALLS",
            "ast-go",
            &AddEdgeOptions::default(),
        );
        let e = graph.edges.values().next().unwrap();
        // Rank must match edge_rank("A->B:CALLS:ast-go").
        assert_eq!(e.rank, edge_rank("A->B:CALLS:ast-go"));
    }

    #[test]
    fn add_edge_normalized_key_not_set_for_ast_edges() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CALLS",
            "ast-go",
            &AddEdgeOptions::default(),
        );
        let e = graph.edges.values().next().unwrap();
        // Legacy AST edges leave normalizedKey unset →
        // renderer writes NULL. We mirror by omitting the
        // property from the bag entirely.
        assert!(e.properties.get("normalizedKey").is_none());
    }

    #[test]
    fn add_edge_defaults_to_confidence_1_and_tier_extracted() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CONTAINS",
            "scip",
            &AddEdgeOptions::default(),
        );
        let e = graph.edges.values().next().unwrap();
        // Legacy line 333: confidence default 1, tier
        // "EXTRACTED".
        assert_eq!(
            e.properties.get("confidence"),
            Some(&CodeGraphPrimitive::Double(1.0))
        );
        assert_eq!(
            e.properties.get("confidenceTier"),
            Some(&CodeGraphPrimitive::String("EXTRACTED".to_string()))
        );
        // Unset evidence / lines render as NULL.
        assert_eq!(
            e.properties.get("evidenceFilePath"),
            Some(&CodeGraphPrimitive::Null)
        );
        assert_eq!(
            e.properties.get("startLine"),
            Some(&CodeGraphPrimitive::Null)
        );
    }

    #[test]
    fn add_file_vertex_creates_file_kind_and_contains_edge() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let file_vid = add_file_vertex(&mut graph, &scope, &repo_vid, "src/main.go").unwrap();
        // One file vertex emitted; its kind is "file".
        let v = graph.vertices.get(&file_vid).unwrap();
        assert_eq!(v.kind, "file");
        assert_eq!(
            v.properties.get("source"),
            Some(&CodeGraphPrimitive::String("scip".to_string()))
        );
        // One CONTAINS edge from repo → file.
        assert_eq!(graph.edges.len(), 1);
        let e = graph.edges.values().next().unwrap();
        assert_eq!(e.from_vid, repo_vid);
        assert_eq!(e.to_vid, file_vid);
        assert_eq!(e.kind, "CONTAINS");
        assert_eq!(
            e.properties.get("source"),
            Some(&CodeGraphPrimitive::String("scip".to_string()))
        );
    }

    #[test]
    fn add_language_ast_facts_module_source_coalesces_into_file_vertex() {
        // A `defines` fact with sourceKind=module should NOT
        // emit a separate module vertex — the source becomes
        // the file vertex (legacy line 366-368).
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let mut fact = fact_calls(
            "module:src/main.go",
            "src/main.go",
            SymbolKind::Module,
            "function:src/main.go:Foo",
            "Foo",
            SymbolKind::Function,
            "src/main.go",
            5,
        );
        fact.kind = FactKind::Defines;
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "go", &[fact]).unwrap();

        // Expected vertices: repo wouldn't be in here (caller
        // passes its existing vid), but file + target function.
        // No `module:` vertex.
        let module_vertices: Vec<&CodeGraphVertex> = graph
            .vertices
            .values()
            .filter(|v| v.kind == "module")
            .collect();
        assert_eq!(module_vertices.len(), 0, "module source must coalesce");

        // file vertex exists.
        assert!(graph.vertices.values().any(|v| v.kind == "file"));
        // target function vertex exists.
        assert!(graph.vertices.values().any(|v| v.kind == "function"));
    }

    #[test]
    fn add_language_ast_facts_edge_kind_is_uppercase() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let f = fact_calls(
            "function:m.go:Main",
            "Main",
            SymbolKind::Function,
            "function:?:helper",
            "helper",
            SymbolKind::Function,
            "m.go",
            3,
        );
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "go", &[f]).unwrap();
        // The AST edge (not the CONTAINS) is the only one of
        // kind=CALLS.
        let ast_edges: Vec<&CodeGraphEdge> =
            graph.edges.values().filter(|e| e.kind == "CALLS").collect();
        assert_eq!(ast_edges.len(), 1);
        // Source property is "ast-go".
        assert_eq!(
            ast_edges[0].properties.get("source"),
            Some(&CodeGraphPrimitive::String("ast-go".to_string()))
        );
    }

    #[test]
    fn add_language_ast_facts_target_kind_remaps_external_to_package() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let f = SyntacticAstFact {
            kind: FactKind::ImportsFrom,
            language: "go".to_string(),
            source_symbol: "module:m.go".to_string(),
            source_display_name: "m.go".to_string(),
            source_kind: SymbolKind::Module,
            target_symbol: "module:fmt".to_string(),
            target_display_name: "fmt".to_string(),
            target_kind: SymbolKind::External,
            file_path: "m.go".to_string(),
            start_line: 1,
            end_line: 1,
            confidence: CONFIDENCE_INFERRED,
        };
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "go", &[f]).unwrap();
        // Target should be a `package` vertex, not `external`.
        let package_vertices: Vec<&CodeGraphVertex> = graph
            .vertices
            .values()
            .filter(|v| v.kind == "package")
            .collect();
        assert_eq!(package_vertices.len(), 1, "external→package remap");
        // External target should NOT have a `path` property
        // (legacy lines 396-398).
        assert!(package_vertices[0].properties.get("path").is_none());
    }

    #[test]
    fn add_language_ast_facts_per_batch_confidence_tier_threshold_0_8() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        // Regex-grade facts (0.6) → INFERRED batch tier.
        let f = fact_calls(
            "function:m.go:Main",
            "Main",
            SymbolKind::Function,
            "function:?:helper",
            "helper",
            SymbolKind::Function,
            "m.go",
            3,
        );
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "go", &[f.clone()])
            .unwrap();
        let calls_edge = graph.edges.values().find(|e| e.kind == "CALLS").unwrap();
        assert_eq!(
            calls_edge.properties.get("confidenceTier"),
            Some(&CodeGraphPrimitive::String("INFERRED".to_string()))
        );

        // Compiler-grade (0.85) → EXTRACTED batch tier.
        let mut graph = GraphAccumulator::default();
        let mut f_extracted = f.clone();
        f_extracted.confidence = 0.85;
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "go", &[f_extracted])
            .unwrap();
        let calls_edge = graph.edges.values().find(|e| e.kind == "CALLS").unwrap();
        assert_eq!(
            calls_edge.properties.get("confidenceTier"),
            Some(&CodeGraphPrimitive::String("EXTRACTED".to_string()))
        );
    }

    #[test]
    fn add_language_ast_facts_target_kind_remap_interface_to_class() {
        // Critic F2.1: explicit Interface→class remap.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let f = SyntacticAstFact {
            kind: FactKind::Implements,
            language: "java".to_string(),
            source_symbol: "class:M.java:Foo".to_string(),
            source_display_name: "Foo".to_string(),
            source_kind: SymbolKind::Class,
            target_symbol: "interface:?:Runnable".to_string(),
            target_display_name: "Runnable".to_string(),
            target_kind: SymbolKind::Interface,
            file_path: "M.java".to_string(),
            start_line: 1,
            end_line: 1,
            confidence: CONFIDENCE_INFERRED,
        };
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "java", &[f]).unwrap();
        // No `interface` kind vertex — Interface remaps to class.
        assert_eq!(
            graph
                .vertices
                .values()
                .filter(|v| v.kind == "interface")
                .count(),
            0,
        );
        // The Runnable target appears as a class vertex.
        let runnable: Vec<&CodeGraphVertex> = graph
            .vertices
            .values()
            .filter(|v| {
                v.kind == "class"
                    && v.properties.get("label")
                        == Some(&CodeGraphPrimitive::String("Runnable".to_string()))
            })
            .collect();
        assert_eq!(runnable.len(), 1, "Interface should remap to class");
    }

    #[test]
    fn add_language_ast_facts_target_kind_remap_module_to_file() {
        // Critic F2.1: explicit Module→file remap on target.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let f = SyntacticAstFact {
            kind: FactKind::ImportsFrom,
            language: "python".to_string(),
            source_symbol: "module:a.py".to_string(),
            source_display_name: "a.py".to_string(),
            source_kind: SymbolKind::Module,
            target_symbol: "module:./helpers".to_string(),
            target_display_name: "./helpers".to_string(),
            target_kind: SymbolKind::Module,
            file_path: "a.py".to_string(),
            start_line: 2,
            end_line: 2,
            confidence: CONFIDENCE_INFERRED,
        };
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "python", &[f]).unwrap();
        // A file vertex with label="./helpers" should exist
        // (the target was remapped Module→file).
        let helpers: Vec<&CodeGraphVertex> = graph
            .vertices
            .values()
            .filter(|v| {
                v.kind == "file"
                    && v.properties.get("label")
                        == Some(&CodeGraphPrimitive::String("./helpers".to_string()))
            })
            .collect();
        assert_eq!(helpers.len(), 1, "Module→file target remap");
        // No `module` kind vertex in output (source coalesces
        // into file; target remaps to file).
        assert_eq!(
            graph
                .vertices
                .values()
                .filter(|v| v.kind == "module")
                .count(),
            0,
        );
    }

    #[test]
    fn add_language_ast_facts_source_kind_class_and_method_passthrough() {
        // Critic F2.1: source_kind = Class produces a class
        // vertex (not function). Method produces a method
        // vertex.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let f_class = SyntacticAstFact {
            kind: FactKind::Defines,
            language: "ruby".to_string(),
            source_symbol: "class:foo.rb:Bar".to_string(),
            source_display_name: "Bar".to_string(),
            source_kind: SymbolKind::Class,
            target_symbol: "method:foo.rb:Bar#init".to_string(),
            target_display_name: "init".to_string(),
            target_kind: SymbolKind::Method,
            file_path: "foo.rb".to_string(),
            start_line: 1,
            end_line: 1,
            confidence: CONFIDENCE_INFERRED,
        };
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "ruby", &[f_class])
            .unwrap();
        // Source is a class vertex with label "Bar".
        let bar = graph.vertices.values().find(|v| {
            v.kind == "class"
                && v.properties.get("label") == Some(&CodeGraphPrimitive::String("Bar".to_string()))
        });
        assert!(bar.is_some(), "source_kind=Class → class vertex");
        // Target is a method vertex.
        let init = graph.vertices.values().find(|v| {
            v.kind == "method"
                && v.properties.get("label")
                    == Some(&CodeGraphPrimitive::String("init".to_string()))
        });
        assert!(init.is_some(), "target_kind=Method → method vertex");
    }

    #[test]
    fn add_language_ast_facts_per_batch_tier_uses_first_fact_only() {
        // Critic F2.1: the 0.8 threshold gate uses
        // facts[0].confidence; the rest of the batch's
        // confidence values are IGNORED for tier selection.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let f1 = fact_calls(
            "function:m.go:A",
            "A",
            SymbolKind::Function,
            "function:?:a1",
            "a1",
            SymbolKind::Function,
            "m.go",
            1,
        );
        let mut f2 = fact_calls(
            "function:m.go:B",
            "B",
            SymbolKind::Function,
            "function:?:b1",
            "b1",
            SymbolKind::Function,
            "m.go",
            2,
        );
        // f2 confidence would be EXTRACTED on its own — but
        // facts[0] (which is INFERRED) gates the batch.
        f2.confidence = 0.99;
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "go", &[f1, f2]).unwrap();
        // Both AST edges should carry INFERRED.
        for edge in graph.edges.values().filter(|e| e.kind == "CALLS") {
            assert_eq!(
                edge.properties.get("confidenceTier"),
                Some(&CodeGraphPrimitive::String("INFERRED".to_string())),
                "edge {:?} should reflect batch tier",
                edge.from_vid
            );
        }
    }

    #[test]
    fn add_language_ast_facts_edge_carries_per_fact_line_numbers() {
        // Critic F2.1: edge's startLine/endLine come from
        // the fact, not from defaults.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let f = fact_calls(
            "function:m.go:Main",
            "Main",
            SymbolKind::Function,
            "function:?:helper",
            "helper",
            SymbolKind::Function,
            "m.go",
            17,
        );
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "go", &[f]).unwrap();
        let calls = graph
            .edges
            .values()
            .find(|e| e.kind == "CALLS")
            .expect("calls edge");
        assert_eq!(
            calls.properties.get("startLine"),
            Some(&CodeGraphPrimitive::Int(17))
        );
        assert_eq!(
            calls.properties.get("endLine"),
            Some(&CodeGraphPrimitive::Int(17))
        );
        assert_eq!(
            calls.properties.get("evidenceFilePath"),
            Some(&CodeGraphPrimitive::String("m.go".to_string()))
        );
    }

    #[test]
    fn add_language_ast_facts_renders_via_nebula_ngql() {
        // Integration smoke: the accumulator → snapshot →
        // NGQL pipeline produces non-empty INSERT statements.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let f = fact_calls(
            "function:m.go:Main",
            "Main",
            SymbolKind::Function,
            "function:?:helper",
            "helper",
            SymbolKind::Function,
            "m.go",
            3,
        );
        add_syntactic_language_ast_facts(&mut graph, &scope, &repo_vid, "go", &[f]).unwrap();
        let snap = accumulator_to_snapshot(graph);
        // file + source-function + target-function = 3
        // vertices; CONTAINS + CALLS = 2 edges.
        assert_eq!(snap.vertices.len(), 3);
        assert_eq!(snap.edges.len(), 2);
        let stmts = crate::nebula_ngql::render_snapshot_statements(&snap);
        // Schema DDL + 1 vertex INSERT + 1 edge INSERT.
        let schema_count = crate::nebula_ngql::render_schema_statements().len();
        assert_eq!(stmts.len(), schema_count + 2);
        // Edge INSERT references the uppercase "CALLS" + the
        // CONTAINS edge.
        let edge_stmt = &stmts[schema_count + 1];
        assert!(edge_stmt.contains("\"CALLS\""), "{}", edge_stmt);
        assert!(edge_stmt.contains("\"CONTAINS\""), "{}", edge_stmt);
        assert!(edge_stmt.contains("\"ast-go\""), "{}", edge_stmt);
        assert!(edge_stmt.contains("\"scip\""), "{}", edge_stmt);
    }

    #[test]
    fn accumulator_to_snapshot_and_anchors_materializes_symbol_anchors_and_links() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        add_syntactic_language_ast_facts(
            &mut graph,
            &scope,
            &repo_vid,
            "go",
            &[fact_calls(
                "handler.Handle",
                "Handle",
                SymbolKind::Function,
                "service.CreateOrder",
                "CreateOrder",
                SymbolKind::Function,
                "src/handler.go",
                11,
            )],
        )
        .unwrap();

        let (snapshot, anchors) = accumulator_to_snapshot_and_anchors(graph);
        assert!(
            anchors.iter().any(|a| a.kind == "symbol"
                && a.direction == "PROVIDES"
                && a.normalized_key == "handle"
                && a.evidence_file_path.as_deref() == Some("src/handler.go")),
            "expected Handle symbol anchor, got {anchors:?}"
        );
        let anchor_link = snapshot
            .edges
            .iter()
            .find(|e| e.kind == "ANCHOR_LINK")
            .expect("expected ANCHOR_LINK edge");
        assert_eq!(
            anchor_link.properties.get("source"),
            Some(&CodeGraphPrimitive::String(ANCHOR_LINK_SOURCE.to_string()))
        );
        assert!(
            matches!(
                anchor_link.properties.get("normalizedKey"),
                Some(CodeGraphPrimitive::String(v)) if v.contains("handle->createorder:calls")
            ),
            "anchor link normalizedKey missing useful source/target relation: {:?}",
            anchor_link.properties.get("normalizedKey")
        );
    }

    // ── Q.A: provenance + SCIP projection + reconciler ────────

    fn scip_symbol_row(
        symbol: &str,
        display: &str,
        kind: Option<&str>,
        file: &str,
    ) -> ScipSymbolRow {
        ScipSymbolRow {
            symbol: symbol.to_string(),
            display_name: display.to_string(),
            kind: kind.map(str::to_string),
            language: Some("typescript".to_string()),
            documentation: Vec::new(),
            signature: None,
            file_path: Some(file.to_string()),
            start_line: Some(10),
            start_character: Some(4),
            end_line: Some(10),
            end_character: Some(20),
            enclosing_symbol: None,
        }
    }

    #[test]
    fn add_edge_writes_provenance_default_heuristic() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CALLS",
            "ast-go",
            &AddEdgeOptions::default(),
        );
        let e = graph.edges.values().next().unwrap();
        assert_eq!(
            e.properties.get("provenance"),
            Some(&CodeGraphPrimitive::String(
                PROVENANCE_HEURISTIC.to_string()
            ))
        );
        // Q.B — CALLS edges with no explicit context default to
        // "call" via default_context_for_edge_kind. NULL only
        // for kinds with no canonical context mapping.
        assert_eq!(
            e.properties.get("context"),
            Some(&CodeGraphPrimitive::String(CONTEXT_CALL.to_string()))
        );
    }

    #[test]
    fn add_edge_context_defaults_per_kind() {
        // Q.B: every well-known edge kind has a canonical
        // context. This locks the mapping table that the Go
        // reader's NL-driven context filter depends on.
        let cases: &[(&str, &str)] = &[
            ("CALLS", "call"),
            ("DEFINES", "definition"),
            ("REFERENCES", "reference"),
            ("IMPORTS_FROM", "import"),
            ("EXTENDS", "inherits"),
            ("IMPLEMENTS", "inherits"),
            ("TYPE_DEFINES", "type"),
            ("CONTAINS", "containment"),
        ];
        for (kind, want_ctx) in cases {
            let mut graph = GraphAccumulator::default();
            let scope = make_scope();
            add_edge(
                &mut graph,
                &scope,
                "A",
                "B",
                kind,
                "scip",
                &AddEdgeOptions::default(),
            );
            let e = graph.edges.values().next().unwrap();
            assert_eq!(
                e.properties.get("context"),
                Some(&CodeGraphPrimitive::String((*want_ctx).to_string())),
                "kind {} should derive context {}",
                kind,
                want_ctx
            );
        }
    }

    #[test]
    fn add_edge_context_null_for_unknown_kind() {
        // An edge kind with no entry in default_context_for_edge_kind
        // renders NULL — the reader treats NULL as "any context".
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CUSTOM_KIND_NEW",
            "ast",
            &AddEdgeOptions::default(),
        );
        let e = graph.edges.values().next().unwrap();
        assert_eq!(e.properties.get("context"), Some(&CodeGraphPrimitive::Null));
    }

    #[test]
    fn add_edge_explicit_context_overrides_derived_default() {
        // When the caller supplies AddEdgeOptions.context, that
        // value wins over the derived default. Used by Q.B+
        // callers that have richer context than the edge kind
        // alone — e.g. a CALLS edge through a callback dispatcher
        // can ride at context="call" via override even though
        // an unrelated CALLS edge derives from default.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CALLS",
            "ast",
            &AddEdgeOptions {
                context: Some("return_type".to_string()),
                ..Default::default()
            },
        );
        let e = graph.edges.values().next().unwrap();
        assert_eq!(
            e.properties.get("context"),
            Some(&CodeGraphPrimitive::String("return_type".to_string()))
        );
    }

    #[test]
    fn add_edge_accepts_provenance_override() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "REFERENCES",
            "scip-typescript",
            &AddEdgeOptions {
                provenance: Some(PROVENANCE_SCIP.to_string()),
                context: Some("call".to_string()),
                ..Default::default()
            },
        );
        let e = graph.edges.values().next().unwrap();
        assert_eq!(
            e.properties.get("provenance"),
            Some(&CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()))
        );
        assert_eq!(
            e.properties.get("context"),
            Some(&CodeGraphPrimitive::String("call".to_string()))
        );
    }

    #[test]
    fn add_vertex_writes_provenance_default_heuristic() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let vid = add_vertex(&mut graph, &scope, "function", "k1", BTreeMap::new()).unwrap();
        let v = graph.vertices.get(&vid).unwrap();
        assert_eq!(
            v.properties.get("provenance"),
            Some(&CodeGraphPrimitive::String(
                PROVENANCE_HEURISTIC.to_string()
            ))
        );
    }

    #[test]
    fn add_vertex_caller_provenance_overrides_default() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let mut props = BTreeMap::new();
        props.insert(
            "provenance".to_string(),
            CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()),
        );
        let vid = add_vertex(&mut graph, &scope, "function", "k1", props).unwrap();
        let v = graph.vertices.get(&vid).unwrap();
        assert_eq!(
            v.properties.get("provenance"),
            Some(&CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()))
        );
    }

    #[test]
    fn add_scip_semantic_facts_emits_implements_edge_with_scip_provenance() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let rows = ScipIngestRows {
            symbols: vec![scip_symbol_row(
                "scip-typescript . . . src/A.ts#A",
                "A",
                Some("Class"),
                "src/A.ts",
            )],
            occurrences: Vec::new(),
            relationships: vec![crate::scip::ScipRelationshipRow {
                source_symbol: "scip-typescript . . . src/A.ts#A".to_string(),
                target_symbol: "scip-typescript . . . src/A.ts#Runnable".to_string(),
                is_reference: false,
                is_implementation: true,
                is_type_definition: false,
                is_definition: false,
            }],
        };
        add_scip_semantic_facts(&mut graph, &scope, &repo_vid, "typescript", &rows).unwrap();
        let impl_edges: Vec<&CodeGraphEdge> = graph
            .edges
            .values()
            .filter(|e| e.kind == "IMPLEMENTS")
            .collect();
        assert_eq!(impl_edges.len(), 1, "one IMPLEMENTS edge expected");
        let e = impl_edges[0];
        assert_eq!(
            e.properties.get("provenance"),
            Some(&CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()))
        );
        assert_eq!(
            e.properties.get("confidence"),
            Some(&CodeGraphPrimitive::Double(SCIP_EDGE_CONFIDENCE))
        );
        assert_eq!(
            e.properties.get("confidenceTier"),
            Some(&CodeGraphPrimitive::String("EXTRACTED".to_string()))
        );
        assert_eq!(
            e.properties.get("source"),
            Some(&CodeGraphPrimitive::String("scip-typescript".to_string()))
        );
    }

    #[test]
    fn add_scip_semantic_facts_emits_references_and_type_defines_edges() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let rows = ScipIngestRows {
            symbols: vec![scip_symbol_row("src#A", "A", Some("Class"), "src/A.ts")],
            occurrences: Vec::new(),
            relationships: vec![
                crate::scip::ScipRelationshipRow {
                    source_symbol: "src#A".to_string(),
                    target_symbol: "src#B".to_string(),
                    is_reference: true,
                    is_implementation: false,
                    is_type_definition: false,
                    is_definition: false,
                },
                crate::scip::ScipRelationshipRow {
                    source_symbol: "src#A".to_string(),
                    target_symbol: "src#C".to_string(),
                    is_reference: false,
                    is_implementation: false,
                    is_type_definition: true,
                    is_definition: false,
                },
            ],
        };
        add_scip_semantic_facts(&mut graph, &scope, &repo_vid, "typescript", &rows).unwrap();
        assert!(graph.edges.values().any(|e| e.kind == "REFERENCES"));
        assert!(graph.edges.values().any(|e| e.kind == "TYPE_DEFINES"));
    }

    #[test]
    fn add_scip_semantic_facts_emits_one_edge_per_true_flag_on_multi_flag_relationship() {
        // A single ScipRelationshipRow with two flags true should
        // emit two distinct edges (one per kind).
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let rows = ScipIngestRows {
            symbols: Vec::new(),
            occurrences: Vec::new(),
            relationships: vec![crate::scip::ScipRelationshipRow {
                source_symbol: "src#A".to_string(),
                target_symbol: "src#B".to_string(),
                is_reference: true,
                is_implementation: true,
                is_type_definition: false,
                is_definition: false,
            }],
        };
        add_scip_semantic_facts(&mut graph, &scope, &repo_vid, "typescript", &rows).unwrap();
        let kinds: std::collections::BTreeSet<&str> =
            graph.edges.values().map(|e| e.kind.as_str()).collect();
        assert!(kinds.contains("REFERENCES"), "{:?}", kinds);
        assert!(kinds.contains("IMPLEMENTS"), "{:?}", kinds);
    }

    #[test]
    fn add_scip_semantic_facts_materializes_unknown_target_as_bare_symbol_vertex() {
        // Relationship references a target that has no
        // SymbolInformation row (external dep). The projector
        // should still emit a vertex for it so the IMPLEMENTS
        // edge has both endpoints.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let rows = ScipIngestRows {
            symbols: vec![scip_symbol_row("src#A", "A", Some("Class"), "src/A.ts")],
            occurrences: Vec::new(),
            relationships: vec![crate::scip::ScipRelationshipRow {
                source_symbol: "src#A".to_string(),
                target_symbol: "java.lang.Runnable".to_string(),
                is_reference: false,
                is_implementation: true,
                is_type_definition: false,
                is_definition: false,
            }],
        };
        add_scip_semantic_facts(&mut graph, &scope, &repo_vid, "java", &rows).unwrap();
        // The bare target should land as kind="symbol" since we
        // have no SymbolInformation for it.
        let runnable_vertex = graph.vertices.values().find(|v| {
            v.properties.get("key")
                == Some(&CodeGraphPrimitive::String(
                    "java.lang.Runnable".to_string(),
                ))
        });
        assert!(runnable_vertex.is_some(), "bare target vertex expected");
        assert_eq!(runnable_vertex.unwrap().kind, "symbol");
        assert_eq!(
            runnable_vertex.unwrap().properties.get("provenance"),
            Some(&CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()))
        );
    }

    #[test]
    fn add_scip_semantic_facts_creates_file_and_contains_chain_for_known_symbol() {
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        let repo_vid = make_repo_vid(&scope);
        let rows = ScipIngestRows {
            symbols: vec![scip_symbol_row(
                "src#Foo",
                "Foo",
                Some("Function"),
                "src/foo.ts",
            )],
            occurrences: Vec::new(),
            relationships: Vec::new(),
        };
        add_scip_semantic_facts(&mut graph, &scope, &repo_vid, "typescript", &rows).unwrap();
        // file vertex exists.
        let file = graph.vertices.values().find(|v| v.kind == "file");
        assert!(file.is_some(), "file vertex expected");
        // function vertex exists.
        let func = graph.vertices.values().find(|v| v.kind == "function");
        assert!(func.is_some(), "function vertex expected");
        // CONTAINS edge from file → function with scip provenance.
        let contains = graph.edges.values().find(|e| {
            e.kind == "CONTAINS"
                && e.properties.get("provenance")
                    == Some(&CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()))
        });
        assert!(contains.is_some(), "scip CONTAINS edge expected");
    }

    #[test]
    fn reconcile_edge_provenance_keeps_higher_confidence_peer() {
        // Same (from, to, kind) triple emitted twice — once at
        // 0.6/heuristic via AST regex source, once at 0.95/scip.
        // Reconciler must keep only the SCIP edge.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "vid-A",
            "vid-B",
            "CALLS",
            "ast-typescript",
            &AddEdgeOptions {
                confidence: Some(0.6),
                provenance: Some(PROVENANCE_HEURISTIC.to_string()),
                ..Default::default()
            },
        );
        add_edge(
            &mut graph,
            &scope,
            "vid-A",
            "vid-B",
            "CALLS",
            "scip-typescript",
            &AddEdgeOptions {
                confidence: Some(0.95),
                provenance: Some(PROVENANCE_SCIP.to_string()),
                ..Default::default()
            },
        );
        // Before reconcile: two CALLS edges (different ranks
        // because source differs → different SHA-256 hashes).
        assert_eq!(graph.edges.len(), 2);
        reconcile_edge_provenance(&mut graph);
        // After reconcile: one edge, the SCIP one.
        assert_eq!(graph.edges.len(), 1);
        let kept = graph.edges.values().next().unwrap();
        assert_eq!(
            kept.properties.get("provenance"),
            Some(&CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()))
        );
        assert_eq!(
            kept.properties.get("confidence"),
            Some(&CodeGraphPrimitive::Double(0.95))
        );
    }

    #[test]
    fn reconcile_edge_provenance_breaks_ties_by_provenance_rank() {
        // Both edges at 0.95 confidence but one scip one heuristic.
        // SCIP must win.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CALLS",
            "ast",
            &AddEdgeOptions {
                confidence: Some(0.95),
                provenance: Some(PROVENANCE_HEURISTIC.to_string()),
                ..Default::default()
            },
        );
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CALLS",
            "scip",
            &AddEdgeOptions {
                confidence: Some(0.95),
                provenance: Some(PROVENANCE_SCIP.to_string()),
                ..Default::default()
            },
        );
        reconcile_edge_provenance(&mut graph);
        assert_eq!(graph.edges.len(), 1);
        assert_eq!(
            graph
                .edges
                .values()
                .next()
                .unwrap()
                .properties
                .get("provenance"),
            Some(&CodeGraphPrimitive::String(PROVENANCE_SCIP.to_string()))
        );
    }

    #[test]
    fn reconcile_edge_provenance_preserves_distinct_kinds() {
        // Different kinds on the same (from, to) pair must NOT
        // collide. A REFERENCES + IMPLEMENTS pair survives.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "REFERENCES",
            "scip",
            &AddEdgeOptions {
                confidence: Some(0.95),
                provenance: Some(PROVENANCE_SCIP.to_string()),
                ..Default::default()
            },
        );
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "IMPLEMENTS",
            "scip",
            &AddEdgeOptions {
                confidence: Some(0.95),
                provenance: Some(PROVENANCE_SCIP.to_string()),
                ..Default::default()
            },
        );
        reconcile_edge_provenance(&mut graph);
        assert_eq!(graph.edges.len(), 2);
    }

    #[test]
    fn reconcile_edge_provenance_is_idempotent() {
        // Calling reconcile twice produces the same result.
        let mut graph = GraphAccumulator::default();
        let scope = make_scope();
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CALLS",
            "ast",
            &AddEdgeOptions {
                confidence: Some(0.6),
                provenance: Some(PROVENANCE_HEURISTIC.to_string()),
                ..Default::default()
            },
        );
        add_edge(
            &mut graph,
            &scope,
            "A",
            "B",
            "CALLS",
            "scip",
            &AddEdgeOptions {
                confidence: Some(0.95),
                provenance: Some(PROVENANCE_SCIP.to_string()),
                ..Default::default()
            },
        );
        reconcile_edge_provenance(&mut graph);
        let first_pass = graph.edges.len();
        reconcile_edge_provenance(&mut graph);
        assert_eq!(graph.edges.len(), first_pass);
    }

    #[test]
    fn scip_kind_to_graph_kind_maps_known_kinds() {
        assert_eq!(scip_kind_to_graph_kind(Some("Class")), "class");
        assert_eq!(scip_kind_to_graph_kind(Some("Interface")), "class");
        assert_eq!(scip_kind_to_graph_kind(Some("Trait")), "class");
        assert_eq!(scip_kind_to_graph_kind(Some("Method")), "method");
        assert_eq!(scip_kind_to_graph_kind(Some("Function")), "function");
        assert_eq!(scip_kind_to_graph_kind(Some("Module")), "file");
        assert_eq!(scip_kind_to_graph_kind(Some("Field")), "symbol");
        assert_eq!(scip_kind_to_graph_kind(None), "symbol");
        assert_eq!(scip_kind_to_graph_kind(Some("Garbage")), "symbol");
    }
}
