//! NGQL renderer — pure-string port of
//! `packages/backend/src/codeGraph/nebulaNgql.ts`.
//!
//! Renders code-graph snapshots into Nebula NGQL statements
//! (DDL + DML). The legacy is a deterministic transform from
//! `CodeGraphSnapshot` to a list of NGQL strings; the
//! Go-side `nebulaCodeGraphStore` (slice 45) executes them.
//!
//! This port also renders strings — execution stays in the
//! Go-side path which already has the production `nebulaclient`
//! wrapper, retries, and connection pool. The Rust indexer
//! produces facts → snapshot, the renderer formats the
//! statements, and a future RPC handoff slice (R.9k) feeds
//! them to the Go executor.
//!
//! Tenant safety: pure string transform — no I/O, no DB. The
//! caller is responsible for tagging the input snapshot with
//! the correct `(orgId, workspaceId, repoId, ...)` scope.
//! Identifier quoting via backtick + double-backtick escape
//! prevents NGQL injection from arbitrary string property
//! values.

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::BTreeMap;

/// CodeGraphPrimitive mirrors the legacy union
/// `string | number | boolean | null`.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
#[serde(untagged)]
pub enum CodeGraphPrimitive {
    Null,
    String(String),
    /// Integer-typed properties (orgId, repoId, schemaVersion,
    /// startLine, endLine) per the legacy `propType` table.
    Int(i64),
    /// Double-typed property (confidence). Note: NaN/Inf are
    /// rendered as `NULL` by the legacy
    /// `Number.isFinite` check.
    Double(f64),
    Bool(bool),
}

impl CodeGraphPrimitive {
    pub fn string<S: Into<String>>(s: S) -> Self {
        Self::String(s.into())
    }
}

/// CodeGraphVertex mirrors the legacy struct (types.ts:7-11).
/// The `kind` field is preserved for round-tripping but is not
/// actually read by the renderer — only `vid` and the
/// `properties` map drive the INSERT VERTEX statement.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CodeGraphVertex {
    pub vid: String,
    pub kind: String,
    pub properties: BTreeMap<String, CodeGraphPrimitive>,
}

/// CodeGraphEdge mirrors types.ts:13-19. `rank` is the
/// deterministic SHA-256-prefix hash produced by `edge_rank`;
/// Nebula uses it to permit multiple edges of the same type
/// between the same (src, dst) pair without overwriting.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CodeGraphEdge {
    #[serde(rename = "fromVid")]
    pub from_vid: String,
    #[serde(rename = "toVid")]
    pub to_vid: String,
    pub kind: String,
    pub rank: u32,
    pub properties: BTreeMap<String, CodeGraphPrimitive>,
}

/// CodeGraphSnapshotAnchor is the non-Nebula sidecar row the
/// Rust indexer sends to the Go graph writer. It mirrors the
/// Postgres CodeGraphAnchor table, while rendered NGQL still
/// carries only vertices + edges.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct CodeGraphSnapshotAnchor {
    pub kind: String,
    pub direction: String,
    pub key: String,
    #[serde(rename = "normalizedKey")]
    pub normalized_key: String,
    #[serde(rename = "nodeVid")]
    pub node_vid: String,
    #[serde(rename = "evidenceFilePath")]
    pub evidence_file_path: Option<String>,
    #[serde(rename = "startLine")]
    pub start_line: Option<i64>,
    #[serde(rename = "endLine")]
    pub end_line: Option<i64>,
    pub confidence: f64,
    #[serde(rename = "confidenceTier")]
    pub confidence_tier: String,
    pub source: String,
}

/// CodeGraphSnapshot mirrors types.ts:34-46 (the subset the
/// renderer reads).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CodeGraphSnapshot {
    pub vertices: Vec<CodeGraphVertex>,
    pub edges: Vec<CodeGraphEdge>,
}

/// CodeGraphDeleteInput mirrors the 6-tuple scope key used to
/// LOOKUP and DELETE every vertex/edge belonging to a stale
/// snapshot (legacy types.ts:130).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CodeGraphDeleteInput {
    #[serde(rename = "orgId")]
    pub org_id: i64,
    #[serde(rename = "workspaceId")]
    pub workspace_id: String,
    #[serde(rename = "repoId")]
    pub repo_id: i64,
    #[serde(rename = "commitHash")]
    pub commit_hash: String,
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "builderVersion")]
    pub builder_version: String,
}

pub const NODE_TAG: &str = "code_graph_node";
pub const EDGE_TYPE: &str = "code_graph_edge";
const NODE_SCOPE_INDEX: &str = "code_graph_node_scope_idx";
const NODE_LABEL_INDEX: &str = "code_graph_node_label_idx";
const NODE_KEY_INDEX: &str = "code_graph_node_key_idx";
const NODE_PATH_INDEX: &str = "code_graph_node_path_idx";
const NODE_ROUTE_PATH_INDEX: &str = "code_graph_node_route_path_idx";

/// NODE_PROPS — preserved verbatim from legacy lines 15-37,
/// with the Q.A quality-overhaul `provenance` column appended.
/// Order is significant: the INSERT VERTEX values list emits
/// in this exact order.
///
/// **Q.A divergence from legacy**: `provenance` tags every
/// vertex with the producer source class ("scip" / "heuristic"
/// / "tree-sitter"). The legacy snapshot has only a free-text
/// `source` field ("ast-go"); `provenance` adds a small enum
/// that retrieval can use to weight vertices by precision tier.
pub const NODE_PROPS: &[&str] = &[
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
    // Q.A — quality-overhaul addition.
    "provenance",
];

/// EDGE_PROPS — preserved verbatim from legacy lines 38-54,
/// with the Q.A quality-overhaul `provenance` and `context`
/// columns appended.
///
/// **Q.A divergence from legacy**:
/// - `provenance`: "scip" | "heuristic" | "tree-sitter". The
///   retrieval layer ranks scip edges above heuristic so that
///   semantic relationships (definition/reference/implementation/
///   type_definition) dominate top-K over regex-grade edges.
/// - `context`: STRING NULL. Populated by Q.B as the syntactic
///   context of the edge — "call" / "field" / "param_type" /
///   "return_type" / "attribute" / "inherits" / "import". The
///   reader filters traversals by context inferred from the
///   user's NL query (graphify-style). Left NULL for Q.A.
pub const EDGE_PROPS: &[&str] = &[
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
    // Q.A — quality-overhaul additions.
    "provenance",
    "context",
];

/// render_schema_statements mirrors
/// `renderNebulaGraphSchemaStatements` (lines 56-80).
/// Returns: CREATE TAG, CREATE EDGE, then 5 CREATE TAG
/// INDEX statements (scope, label, key, path, routePath). Each is rendered as
/// a single one-line statement terminated by `;`.
pub fn render_schema_statements() -> Vec<String> {
    let mut out = Vec::with_capacity(7);

    // CREATE TAG `code_graph_node`(`kind` string, ...)
    let node_cols = NODE_PROPS
        .iter()
        .map(|p| format!("{} {}", quote_identifier(p), prop_type(p)))
        .collect::<Vec<_>>()
        .join(", ");
    out.push(format!(
        "CREATE TAG IF NOT EXISTS {}({});",
        quote_identifier(NODE_TAG),
        node_cols
    ));

    // CREATE EDGE `code_graph_edge`(`kind` string, ...)
    let edge_cols = EDGE_PROPS
        .iter()
        .map(|p| format!("{} {}", quote_identifier(p), prop_type(p)))
        .collect::<Vec<_>>()
        .join(", ");
    out.push(format!(
        "CREATE EDGE IF NOT EXISTS {}({});",
        quote_identifier(EDGE_TYPE),
        edge_cols
    ));

    // Scope index: orgId + workspaceId(128) + repoId +
    // commitHash(40) + schemaVersion + builderVersion(128).
    out.push(format!(
        "CREATE TAG INDEX IF NOT EXISTS {} ON {}({});",
        quote_identifier(NODE_SCOPE_INDEX),
        quote_identifier(NODE_TAG),
        [
            quote_identifier("orgId"),
            format!("{}(128)", quote_identifier("workspaceId")),
            quote_identifier("repoId"),
            format!("{}(40)", quote_identifier("commitHash")),
            quote_identifier("schemaVersion"),
            format!("{}(128)", quote_identifier("builderVersion")),
        ]
        .join(", ")
    ));

    // Label prefix index.
    out.push(format!(
        "CREATE TAG INDEX IF NOT EXISTS {} ON {}({});",
        quote_identifier(NODE_LABEL_INDEX),
        quote_identifier(NODE_TAG),
        [
            quote_identifier("orgId"),
            format!("{}(128)", quote_identifier("workspaceId")),
            format!("{}(128)", quote_identifier("label")),
        ]
        .join(", ")
    ));

    // Key prefix index.
    out.push(format!(
        "CREATE TAG INDEX IF NOT EXISTS {} ON {}({});",
        quote_identifier(NODE_KEY_INDEX),
        quote_identifier(NODE_TAG),
        [
            quote_identifier("orgId"),
            format!("{}(128)", quote_identifier("workspaceId")),
            format!("{}(128)", quote_identifier("key")),
        ]
        .join(", ")
    ));

    // Path prefix index.
    out.push(format!(
        "CREATE TAG INDEX IF NOT EXISTS {} ON {}({});",
        quote_identifier(NODE_PATH_INDEX),
        quote_identifier(NODE_TAG),
        [
            quote_identifier("orgId"),
            format!("{}(128)", quote_identifier("workspaceId")),
            format!("{}(128)", quote_identifier("path")),
        ]
        .join(", ")
    ));

    // Route path prefix index.
    out.push(format!(
        "CREATE TAG INDEX IF NOT EXISTS {} ON {}({});",
        quote_identifier(NODE_ROUTE_PATH_INDEX),
        quote_identifier(NODE_TAG),
        [
            quote_identifier("orgId"),
            format!("{}(128)", quote_identifier("workspaceId")),
            format!("{}(128)", quote_identifier("routePath")),
        ]
        .join(", ")
    ));

    out
}

/// render_describe_tag_statement mirrors line 82.
pub fn render_describe_tag_statement() -> String {
    format!("DESCRIBE TAG {};", quote_identifier(NODE_TAG))
}

/// render_describe_edge_statement mirrors line 84.
pub fn render_describe_edge_statement() -> String {
    format!("DESCRIBE EDGE {};", quote_identifier(EDGE_TYPE))
}

/// render_alter_tag_add_statement mirrors lines 86-88.
pub fn render_alter_tag_add_statement(props: &[&str]) -> String {
    let cols = props
        .iter()
        .map(|p| format!("{} {}", quote_identifier(p), prop_type(p)))
        .collect::<Vec<_>>()
        .join(", ");
    format!("ALTER TAG {} ADD ({});", quote_identifier(NODE_TAG), cols)
}

/// render_alter_edge_add_statement mirrors lines 90-92.
pub fn render_alter_edge_add_statement(props: &[&str]) -> String {
    let cols = props
        .iter()
        .map(|p| format!("{} {}", quote_identifier(p), prop_type(p)))
        .collect::<Vec<_>>()
        .join(", ");
    format!("ALTER EDGE {} ADD ({});", quote_identifier(EDGE_TYPE), cols)
}

/// render_snapshot_statements mirrors lines 94-103.
/// Returns: schema DDL + N vertex-INSERT statements + M
/// edge-INSERT statements, each chunked to 250 rows.
pub fn render_snapshot_statements(snapshot: &CodeGraphSnapshot) -> Vec<String> {
    let mut statements = render_schema_statements();
    for chunk in snapshot.vertices.chunks(250) {
        statements.push(render_vertex_insert(chunk));
    }
    for chunk in snapshot.edges.chunks(250) {
        statements.push(render_edge_insert(chunk));
    }
    statements
}

/// edge_rank mirrors legacy line 106. SHA-256 of input, then
/// take the first 8 hex chars and parse as u32.
pub fn edge_rank(input: &str) -> u32 {
    let mut hasher = Sha256::new();
    hasher.update(input.as_bytes());
    let digest = hasher.finalize();
    // Convert first 4 bytes (= 8 hex chars) to u32 big-endian.
    // Legacy parseInt("aabbccdd", 16) -> 0xaabbccdd which is
    // big-endian from the leftmost hex pair.
    let bytes: [u8; 4] = [digest[0], digest[1], digest[2], digest[3]];
    u32::from_be_bytes(bytes)
}

/// render_lookup_snapshot_vertices_statement mirrors lines
/// 109-120. Returns a single space-joined NGQL string that
/// LOOKUP's all vertices belonging to the supplied scope.
pub fn render_lookup_snapshot_vertices_statement(input: &CodeGraphDeleteInput) -> String {
    let parts = [
        format!("LOOKUP ON {}", quote_identifier(NODE_TAG)),
        format!(
            "WHERE {} == {}",
            prop_ref("orgId"),
            ngql_value(&CodeGraphPrimitive::Int(input.org_id))
        ),
        format!(
            "AND {} == {}",
            prop_ref("workspaceId"),
            ngql_value(&CodeGraphPrimitive::String(input.workspace_id.clone()))
        ),
        format!(
            "AND {} == {}",
            prop_ref("repoId"),
            ngql_value(&CodeGraphPrimitive::Int(input.repo_id))
        ),
        format!(
            "AND {} == {}",
            prop_ref("commitHash"),
            ngql_value(&CodeGraphPrimitive::String(input.commit_hash.clone()))
        ),
        format!(
            "AND {} == {}",
            prop_ref("schemaVersion"),
            ngql_value(&CodeGraphPrimitive::Int(input.schema_version))
        ),
        format!(
            "AND {} == {}",
            prop_ref("builderVersion"),
            ngql_value(&CodeGraphPrimitive::String(input.builder_version.clone()))
        ),
        "YIELD id(vertex) AS vid;".to_string(),
    ];
    parts.join(" ")
}

/// render_delete_vertices_statements mirrors lines 122-126.
/// VIDs are chunked into 250-row batches; each batch becomes
/// one `DELETE VERTEX a, b, c WITH EDGE;` statement.
pub fn render_delete_vertices_statements(vids: &[String]) -> Vec<String> {
    let mut out = Vec::new();
    for chunk in vids.chunks(250) {
        let values = chunk
            .iter()
            .map(|v| ngql_value(&CodeGraphPrimitive::String(v.clone())))
            .collect::<Vec<_>>()
            .join(", ");
        out.push(format!("DELETE VERTEX {} WITH EDGE;", values));
    }
    out
}

/// render_vertex_insert mirrors lines 128-134.
fn render_vertex_insert(vertices: &[CodeGraphVertex]) -> String {
    let values = vertices
        .iter()
        .map(|v| {
            let props = NODE_PROPS
                .iter()
                .map(|p| {
                    let primitive = v
                        .properties
                        .get(*p)
                        .cloned()
                        .unwrap_or(CodeGraphPrimitive::Null);
                    ngql_value(&primitive)
                })
                .collect::<Vec<_>>()
                .join(", ");
            format!(
                "{}: ({})",
                ngql_value(&CodeGraphPrimitive::String(v.vid.clone())),
                props
            )
        })
        .collect::<Vec<_>>()
        .join(", ");

    format!(
        "INSERT VERTEX {}({}) VALUES {};",
        quote_identifier(NODE_TAG),
        NODE_PROPS
            .iter()
            .map(|p| quote_identifier(p))
            .collect::<Vec<_>>()
            .join(", "),
        values
    )
}

/// render_edge_insert mirrors lines 136-142.
fn render_edge_insert(edges: &[CodeGraphEdge]) -> String {
    let values = edges
        .iter()
        .map(|e| {
            let props = EDGE_PROPS
                .iter()
                .map(|p| {
                    let primitive = e
                        .properties
                        .get(*p)
                        .cloned()
                        .unwrap_or(CodeGraphPrimitive::Null);
                    ngql_value(&primitive)
                })
                .collect::<Vec<_>>()
                .join(", ");
            format!(
                "{}->{}@{}: ({})",
                ngql_value(&CodeGraphPrimitive::String(e.from_vid.clone())),
                ngql_value(&CodeGraphPrimitive::String(e.to_vid.clone())),
                e.rank,
                props
            )
        })
        .collect::<Vec<_>>()
        .join(", ");

    format!(
        "INSERT EDGE {}({}) VALUES {};",
        quote_identifier(EDGE_TYPE),
        EDGE_PROPS
            .iter()
            .map(|p| quote_identifier(p))
            .collect::<Vec<_>>()
            .join(", "),
        values
    )
}

/// prop_type mirrors lines 144-152. Most props are `string`;
/// orgId/repoId/schemaVersion/startLine/endLine are `int`;
/// confidence is `double`.
fn prop_type(prop: &str) -> &'static str {
    match prop {
        "orgId" | "repoId" | "schemaVersion" | "startLine" | "endLine" => "int",
        "confidence" => "double",
        _ => "string",
    }
}

/// ngql_value mirrors lines 154-165.
/// - null  → NULL
/// - bool  → true / false
/// - int   → decimal string
/// - double → finite → JS-`String(num)` parity, NaN/Inf → NULL
/// - string → JSON.stringify (escapes \, ", control chars)
fn ngql_value(value: &CodeGraphPrimitive) -> String {
    match value {
        CodeGraphPrimitive::Null => "NULL".to_string(),
        CodeGraphPrimitive::Bool(b) => {
            if *b {
                "true".to_string()
            } else {
                "false".to_string()
            }
        }
        CodeGraphPrimitive::Int(n) => n.to_string(),
        CodeGraphPrimitive::Double(d) => {
            if d.is_finite() {
                format_js_number(*d)
            } else {
                "NULL".to_string()
            }
        }
        CodeGraphPrimitive::String(s) => {
            // JS JSON.stringify escapes \, ", and control chars
            // U+0000..U+001F. serde_json does the same. Both
            // leave solidus and U+2028/U+2029 unescaped per the
            // post-ES2019 JSON spec.
            serde_json::to_string(s).unwrap_or_else(|_| "\"\"".to_string())
        }
    }
}

/// format_js_number renders a finite f64 byte-equally to the
/// legacy `String(num)` (ECMAScript ToString applied to a
/// Number). The two divergences we must close versus
/// `f64::to_string`:
///
/// 1. JS `String(-0)` is `"0"` (the sign of negative zero is
///    suppressed), whereas Rust's `(-0.0f64).to_string()` is
///    `"-0"`.
/// 2. JS uses exponent notation for `|x| >= 1e21` or
///    `0 < |x| < 1e-6` (ECMA-262 §6.1.6.1.13). Rust's
///    `f64::to_string` always emits a decimal expansion.
///
/// Inside the typical realistic range that Rust → Go ever
/// feeds in (confidence ∈ [0, 1], int-typed properties pass
/// through `Int`), neither divergence triggers — but parity is
/// a hard contract and `Double` is a public-facing variant.
fn format_js_number(d: f64) -> String {
    if d == 0.0 {
        // Covers both +0.0 and -0.0 — JS collapses sign on zero.
        return "0".to_string();
    }
    let abs = d.abs();
    if abs >= 1e21 || abs < 1e-6 {
        // JS exponent form: `1e+21`, `5e-7`, `1.5e+30`. The
        // mantissa drops a trailing `.0`. `{:e}` in Rust emits
        // `1e21` (no plus sign, no trailing zero) — close, but
        // the sign is missing and very-small values can carry
        // extra precision digits. Hand-render the JS shape.
        return format_js_exponent(d);
    }
    // Decimal range — Rust's default matches JS for all
    // representable doubles inside [1e-6, 1e21).
    d.to_string()
}

/// format_js_exponent renders a finite non-zero f64 in JS
/// `String(num)` exponent form: `<mantissa>e<sign><exponent>`,
/// e.g. `1e+21`, `5e-7`, `1.5e+30`, `-2.5e-8`.
fn format_js_exponent(d: f64) -> String {
    // Rust's `{:e}` emits e.g. `1e21`, `5e-7`, `1.5e30`. We
    // need to (a) ensure the exponent always carries an
    // explicit sign and (b) leave the mantissa as-is.
    let raw = format!("{:e}", d);
    if let Some(idx) = raw.find('e') {
        let (mantissa, exp) = raw.split_at(idx);
        // `exp` includes the leading `e`.
        let exp_digits = &exp[1..];
        if exp_digits.starts_with('-') {
            format!("{}e{}", mantissa, exp_digits)
        } else {
            format!("{}e+{}", mantissa, exp_digits)
        }
    } else {
        // Unreachable: `{:e}` always emits an `e`.
        raw
    }
}

/// quote_identifier mirrors lines 175-177. Wraps in backticks
/// and doubles any internal backticks.
pub fn quote_identifier(value: &str) -> String {
    format!("`{}`", value.replace('`', "``"))
}

/// prop_ref mirrors line 179. Returns
/// `` `code_graph_node`.`<prop>` ``.
fn prop_ref(prop: &str) -> String {
    format!("{}.{}", quote_identifier(NODE_TAG), quote_identifier(prop))
}

#[cfg(test)]
mod tests {
    //! Byte-equal parity tests against hand-evaluated legacy
    //! output. The legacy `nebulaNgql.ts` is a deterministic
    //! string transform; comparing actual==expected strings is
    //! the right test surface.

    use super::*;

    #[test]
    fn quote_identifier_wraps_in_backticks() {
        assert_eq!(quote_identifier("foo"), "`foo`");
        assert_eq!(quote_identifier("code_graph_node"), "`code_graph_node`");
    }

    #[test]
    fn quote_identifier_doubles_internal_backticks() {
        // Defensive: an attacker-controlled prop name with
        // backticks doubles them to escape.
        assert_eq!(quote_identifier("a`b"), "`a``b`");
        assert_eq!(quote_identifier("`a`"), "```a```");
    }

    #[test]
    fn ngql_value_null_bool_int() {
        assert_eq!(ngql_value(&CodeGraphPrimitive::Null), "NULL");
        assert_eq!(ngql_value(&CodeGraphPrimitive::Bool(true)), "true");
        assert_eq!(ngql_value(&CodeGraphPrimitive::Bool(false)), "false");
        assert_eq!(ngql_value(&CodeGraphPrimitive::Int(42)), "42");
        assert_eq!(ngql_value(&CodeGraphPrimitive::Int(-7)), "-7");
    }

    #[test]
    fn ngql_value_double_finite_and_nonfinite() {
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(0.5)), "0.5");
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(1.0)), "1");
        // NaN and Inf both become NULL per legacy `Number.isFinite` check.
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(f64::NAN)), "NULL");
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::Double(f64::INFINITY)),
            "NULL"
        );
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::Double(f64::NEG_INFINITY)),
            "NULL"
        );
    }

    #[test]
    fn ngql_value_double_js_parity_for_special_shapes() {
        // -0 collapses to "0" per JS `String(-0)`.
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(-0.0)), "0");
        // Exponent crossover at 1e21 / 1e-6 per ECMA-262.
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(1e21)), "1e+21");
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(1e-7)), "1e-7");
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(5e-7)), "5e-7");
        // Just inside the decimal range stays decimal.
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(1e-6)), "0.000001");
        // Confidence-range values are always decimal.
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(0.85)), "0.85");
        assert_eq!(ngql_value(&CodeGraphPrimitive::Double(0.05)), "0.05");
    }

    #[test]
    fn ngql_value_string_unicode_and_solidus_match_jsonstringify() {
        // Solidus is NOT escaped (matches `JSON.stringify("a/b")`).
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::String("a/b".to_string())),
            "\"a/b\""
        );
        // U+2028 / U+2029 are NOT escaped (post-ES2019).
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::String("\u{2028}".to_string())),
            "\"\u{2028}\""
        );
        // U+0001 escapes to `` in both engines.
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::String("\u{0001}".to_string())),
            "\"\\u0001\""
        );
        // BMP-safe emoji passes through raw.
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::String("🦀".to_string())),
            "\"🦀\""
        );
    }

    #[test]
    fn ngql_value_string_jsonstringify_escapes() {
        // Plain string → JSON-quoted.
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::String("hello".to_string())),
            "\"hello\""
        );
        // Double-quote in string → escaped.
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::String("a\"b".to_string())),
            "\"a\\\"b\""
        );
        // Backslash → escaped.
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::String("a\\b".to_string())),
            "\"a\\\\b\""
        );
        // Newline → \n escape.
        assert_eq!(
            ngql_value(&CodeGraphPrimitive::String("a\nb".to_string())),
            "\"a\\nb\""
        );
    }

    #[test]
    fn edge_rank_deterministic_sha256_prefix() {
        // SHA-256 of empty string: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855
        // First 8 hex chars: e3b0c442 → u32 = 0xe3b0c442 = 3819014722.
        assert_eq!(edge_rank(""), 0xe3b0c442);
        // SHA-256 of "abc": ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad
        // First 8 hex chars: ba7816bf → u32 = 0xba7816bf.
        assert_eq!(edge_rank("abc"), 0xba7816bf);
        // Same input → same rank (deterministic).
        assert_eq!(edge_rank("foo->bar"), edge_rank("foo->bar"));
        // Different inputs → different ranks (almost certainly).
        assert_ne!(edge_rank("a"), edge_rank("b"));
    }

    #[test]
    fn prop_type_returns_int_double_or_string() {
        assert_eq!(prop_type("orgId"), "int");
        assert_eq!(prop_type("repoId"), "int");
        assert_eq!(prop_type("schemaVersion"), "int");
        assert_eq!(prop_type("startLine"), "int");
        assert_eq!(prop_type("endLine"), "int");
        assert_eq!(prop_type("confidence"), "double");
        assert_eq!(prop_type("kind"), "string");
        assert_eq!(prop_type("workspaceId"), "string");
        assert_eq!(prop_type("unknown"), "string");
    }

    #[test]
    fn render_schema_statements_returns_7_ddl_strings() {
        let out = render_schema_statements();
        assert_eq!(out.len(), 7);
        assert!(out[0].starts_with("CREATE TAG IF NOT EXISTS `code_graph_node`"));
        assert!(out[0].contains("`orgId` int"));
        assert!(out[0].contains("`workspaceId` string"));
        assert!(out[0].contains("`confidence` double"));
        assert!(out[0].ends_with(");"));

        assert!(out[1].starts_with("CREATE EDGE IF NOT EXISTS `code_graph_edge`"));
        assert!(out[1].contains("`normalizedKey` string"));

        // 5 tag indexes follow.
        assert!(out[2].starts_with("CREATE TAG INDEX IF NOT EXISTS `code_graph_node_scope_idx`"));
        assert!(out[2].contains("`workspaceId`(128)"));
        assert!(out[2].contains("`commitHash`(40)"));

        assert!(out[3].starts_with("CREATE TAG INDEX IF NOT EXISTS `code_graph_node_label_idx`"));
        assert!(out[3].contains("`label`(128)"));

        assert!(out[4].starts_with("CREATE TAG INDEX IF NOT EXISTS `code_graph_node_key_idx`"));
        assert!(out[4].contains("`key`(128)"));

        assert!(out[5].starts_with("CREATE TAG INDEX IF NOT EXISTS `code_graph_node_path_idx`"));
        assert!(out[5].contains("`path`(128)"));

        assert!(
            out[6].starts_with("CREATE TAG INDEX IF NOT EXISTS `code_graph_node_route_path_idx`")
        );
        assert!(out[6].contains("`routePath`(128)"));
    }

    #[test]
    fn render_describe_statements() {
        assert_eq!(
            render_describe_tag_statement(),
            "DESCRIBE TAG `code_graph_node`;"
        );
        assert_eq!(
            render_describe_edge_statement(),
            "DESCRIBE EDGE `code_graph_edge`;"
        );
    }

    #[test]
    fn render_alter_tag_add_statement_with_new_props() {
        let stmt = render_alter_tag_add_statement(&["newField", "anotherField"]);
        assert_eq!(
            stmt,
            "ALTER TAG `code_graph_node` ADD (`newField` string, `anotherField` string);"
        );
        // Confidence is double per the prop_type table.
        let stmt2 = render_alter_tag_add_statement(&["confidence"]);
        assert!(stmt2.contains("`confidence` double"));
    }

    #[test]
    fn render_alter_edge_add_statement_with_new_props() {
        let stmt = render_alter_edge_add_statement(&["startLine", "endLine"]);
        // Both are int per prop_type table.
        assert_eq!(
            stmt,
            "ALTER EDGE `code_graph_edge` ADD (`startLine` int, `endLine` int);"
        );
    }

    fn make_vertex(vid: &str, props: &[(&str, CodeGraphPrimitive)]) -> CodeGraphVertex {
        let mut map = BTreeMap::new();
        for (k, v) in props {
            map.insert((*k).to_string(), v.clone());
        }
        CodeGraphVertex {
            vid: vid.to_string(),
            kind: "test".to_string(),
            properties: map,
        }
    }

    fn make_edge(
        from: &str,
        to: &str,
        rank: u32,
        props: &[(&str, CodeGraphPrimitive)],
    ) -> CodeGraphEdge {
        let mut map = BTreeMap::new();
        for (k, v) in props {
            map.insert((*k).to_string(), v.clone());
        }
        CodeGraphEdge {
            from_vid: from.to_string(),
            to_vid: to.to_string(),
            kind: "test".to_string(),
            rank,
            properties: map,
        }
    }

    #[test]
    fn render_vertex_insert_single_row_byte_equal() {
        // Hand-evaluated expected. Properties NOT in the map
        // emit as `NULL`. Q.A added `provenance` (column 21,
        // i.e. one extra NULL slot after `source`).
        let v = make_vertex(
            "vid-1",
            &[
                ("kind", CodeGraphPrimitive::string("FILE")),
                ("orgId", CodeGraphPrimitive::Int(42)),
                ("workspaceId", CodeGraphPrimitive::string("ws-a")),
                ("repoId", CodeGraphPrimitive::Int(7)),
                ("confidence", CodeGraphPrimitive::Double(0.85)),
            ],
        );
        let stmt = render_vertex_insert(&[v]);
        // Column header must list every prop in NODE_PROPS
        // order.
        assert!(stmt.starts_with("INSERT VERTEX `code_graph_node`(`kind`, `orgId`, `workspaceId`"));
        // Each value position must align: kind="FILE", orgId=42,
        // workspaceId="ws-a", ... then NULL for the unset
        // properties. 22 columns total in NODE_PROPS after Q.A.
        assert!(stmt.contains("\"vid-1\": (\"FILE\", 42, \"ws-a\", 7, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, 0.85, NULL, NULL, NULL, NULL, NULL, NULL)"), "{}", stmt);
        assert!(stmt.ends_with(";"));
    }

    #[test]
    fn render_edge_insert_single_row_byte_equal() {
        let e = make_edge(
            "from-vid",
            "to-vid",
            12345,
            &[
                ("kind", CodeGraphPrimitive::string("CALLS")),
                ("orgId", CodeGraphPrimitive::Int(42)),
                ("confidence", CodeGraphPrimitive::Double(0.6)),
            ],
        );
        let stmt = render_edge_insert(&[e]);
        assert!(stmt.starts_with("INSERT EDGE `code_graph_edge`(`kind`, `orgId`"));
        // `from->to@rank: (props)`
        assert!(stmt.contains("\"from-vid\"->\"to-vid\"@12345:"), "{}", stmt);
        // First three props: kind=CALLS, orgId=42, workspaceId=NULL.
        assert!(stmt.contains("(\"CALLS\", 42, NULL"), "{}", stmt);
        // confidence=0.6 appears at index 8 in EDGE_PROPS.
        assert!(stmt.contains("0.6"), "{}", stmt);
    }

    #[test]
    fn render_snapshot_statements_chunks_at_250() {
        // 251 vertices → schema + 2 vertex-insert + 0 edge.
        let vertices: Vec<CodeGraphVertex> = (0..251)
            .map(|i| make_vertex(&format!("v{}", i), &[]))
            .collect();
        let snapshot = CodeGraphSnapshot {
            vertices,
            edges: vec![],
        };
        let stmts = render_snapshot_statements(&snapshot);
        // Schema + 2 vertex INSERTs (250 + 1).
        let schema_count = render_schema_statements().len();
        assert_eq!(stmts.len(), schema_count + 2);
        // Verify the first vertex INSERT actually has 250 row groups.
        // Each row group starts with `"vN":`. Count by searching.
        let first_insert = &stmts[schema_count];
        let row_count = first_insert.matches("\": (").count();
        assert_eq!(row_count, 250, "first INSERT should have 250 rows");
    }

    #[test]
    fn render_lookup_snapshot_vertices_statement_byte_equal() {
        let input = CodeGraphDeleteInput {
            org_id: 42,
            workspace_id: "ws-a".to_string(),
            repo_id: 7,
            commit_hash: "abc123def456".to_string(),
            schema_version: 1,
            builder_version: "1.0.0".to_string(),
        };
        let stmt = render_lookup_snapshot_vertices_statement(&input);
        let expected = concat!(
            "LOOKUP ON `code_graph_node` ",
            "WHERE `code_graph_node`.`orgId` == 42 ",
            "AND `code_graph_node`.`workspaceId` == \"ws-a\" ",
            "AND `code_graph_node`.`repoId` == 7 ",
            "AND `code_graph_node`.`commitHash` == \"abc123def456\" ",
            "AND `code_graph_node`.`schemaVersion` == 1 ",
            "AND `code_graph_node`.`builderVersion` == \"1.0.0\" ",
            "YIELD id(vertex) AS vid;",
        );
        assert_eq!(stmt, expected);
    }

    #[test]
    fn render_delete_vertices_statements_chunks_at_250() {
        // 500 vids → 2 chunks of 250 each.
        let vids: Vec<String> = (0..500).map(|i| format!("v-{}", i)).collect();
        let stmts = render_delete_vertices_statements(&vids);
        assert_eq!(stmts.len(), 2);
        for s in &stmts {
            assert!(s.starts_with("DELETE VERTEX "));
            assert!(s.ends_with(" WITH EDGE;"));
            // Comma count: 250 vids → 249 commas inside the values list.
            assert_eq!(s.matches(", ").count(), 249);
        }
    }

    #[test]
    fn render_delete_vertices_empty_input_returns_empty() {
        let stmts = render_delete_vertices_statements(&[]);
        assert_eq!(stmts.len(), 0);
    }

    #[test]
    fn snapshot_statements_full_byte_equal_one_vertex_one_edge() {
        // Hand-evaluated fixture: every emitted statement is
        // compared byte-equally to a known-good expected string.
        // This locks the entire renderer surface, not just the
        // shape.
        let v = make_vertex(
            "v1",
            &[
                ("kind", CodeGraphPrimitive::string("FUNC")),
                ("orgId", CodeGraphPrimitive::Int(42)),
                ("workspaceId", CodeGraphPrimitive::string("ws-a")),
                ("repoId", CodeGraphPrimitive::Int(7)),
            ],
        );
        let e = make_edge(
            "v1",
            "v2",
            42,
            &[("kind", CodeGraphPrimitive::string("CALLS"))],
        );
        let snap = CodeGraphSnapshot {
            vertices: vec![v],
            edges: vec![e],
        };
        let stmts = render_snapshot_statements(&snap);
        let schema_count = render_schema_statements().len();
        // Schema + 1 vertex INSERT + 1 edge INSERT.
        assert_eq!(stmts.len(), schema_count + 2);

        // Schema DDL is locked by render_schema_statements; we
        // only re-verify the two INSERT statements here. Q.A
        // appends `provenance` to NODE_PROPS and `provenance` +
        // `context` to EDGE_PROPS — adjust the expected
        // column-lists and NULL trailing slots accordingly.
        let expected_vertex = concat!(
            "INSERT VERTEX `code_graph_node`(",
            "`kind`, `orgId`, `workspaceId`, `repoId`, `revision`, `commitHash`, ",
            "`schemaVersion`, `builderVersion`, `key`, `label`, `path`, `language`, ",
            "`routeMethod`, `routePath`, `packageManager`, `confidence`, ",
            "`confidenceTier`, `evidenceFilePath`, `startLine`, `endLine`, `source`, ",
            "`provenance`",
            ") VALUES ",
            "\"v1\": (",
            "\"FUNC\", 42, \"ws-a\", 7, ",
            "NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL",
            ");",
        );
        assert_eq!(stmts[schema_count], expected_vertex);

        let expected_edge = concat!(
            "INSERT EDGE `code_graph_edge`(",
            "`kind`, `orgId`, `workspaceId`, `repoId`, `revision`, `commitHash`, ",
            "`schemaVersion`, `builderVersion`, `confidence`, `confidenceTier`, ",
            "`evidenceFilePath`, `startLine`, `endLine`, `normalizedKey`, `source`, ",
            "`provenance`, `context`",
            ") VALUES ",
            "\"v1\"->\"v2\"@42: (",
            "\"CALLS\", NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL",
            ");",
        );
        assert_eq!(stmts[schema_count + 1], expected_edge);
    }
}
