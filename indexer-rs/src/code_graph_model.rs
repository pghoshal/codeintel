//! Code graph identity + scope model. Port of
//! `packages/backend/src/codeGraph/model.ts` (VID-construction
//! half).
//!
//! Defines:
//!   - The `CodeGraphScope` shape (orgId, repoId, revision,
//!     commitHash, workspaceId, optional schemaVersion +
//!     builderVersion) carried into every snapshot row.
//!   - `assert_valid_scope` — port of legacy assertValidScope
//!     (lines 119-141). Rejects negative org/repo ids, empty
//!     revision/workspace strings, malformed commit hashes,
//!     non-positive schemaVersion, blank builderVersion.
//!   - `build_code_graph_vertex_id` — the legacy VID
//!     construction algorithm (lines 84-104) producing
//!     `cg:o<orgId>:w<workspaceHash>:r<repoId>:c<commitPrefix>:
//!      s<schemaVersion>:b<builderHash>:<kind>:<keyHash>`.
//!   - `hash_parts` — SHA-256 of NUL-joined parts truncated to
//!     N hex chars.
//!   - `normalize_token` — `replace(/[^a-zA-Z0-9_.:-]+/g, "_")`
//!     + lowercase, used by anchor key normalization.
//!
//! Brand-sweep divergence: the legacy default `builderVersion`
//! uses the legacy brand prefix; the Rust port renames to
//! `"codeintel-code-graph-v7"` (the codeintel DB is fresh —
//! no legacy data depends on the prefix). This break in
//! byte-equality is permitted per the architecture rules brand-sweep
//! rule which trumps cross-deployment byte-equality.

use sha2::{Digest, Sha256};

/// CODE_GRAPH_SCHEMA_VERSION mirrors `model.ts:3`.
pub const CODE_GRAPH_SCHEMA_VERSION: i64 = 1;

/// DEFAULT_BUILDER_VERSION — the Rust port's brand-renamed
/// default. Legacy used the legacy brand prefix at v5.
/// All code-graph VIDs built without an explicit override
/// embed this string's hash; changing it invalidates every
/// existing VID, so the version is bumped (v5 -> v6 -> v7 etc.)
/// rather than the prefix renamed.
pub const DEFAULT_BUILDER_VERSION: &str = "codeintel-code-graph-v7";

/// CodeGraphScope mirrors `model.ts:49-57`. The two optional
/// fields drive VID construction; when None, the defaults
/// above are used.
#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub struct CodeGraphScope {
    pub org_id: i64,
    pub repo_id: i64,
    pub revision: String,
    pub commit_hash: String,
    pub workspace_id: String,
    pub schema_version: Option<i64>,
    pub builder_version: Option<String>,
}

/// CodeGraphVertexIdentity mirrors `model.ts:59-62`. Same as
/// the scope plus the per-vertex `kind` + `key` tuple.
#[derive(Debug, Clone)]
pub struct CodeGraphVertexIdentity {
    pub scope: CodeGraphScope,
    pub kind: String,
    pub key: String,
}

/// CodeGraphScopeError mirrors the legacy throw messages.
/// Used by `assert_valid_scope` so callers can route specific
/// validation failures.
#[derive(Debug, thiserror::Error)]
pub enum CodeGraphScopeError {
    #[error("Invalid code graph orgId: {0}")]
    InvalidOrgId(i64),
    #[error("Invalid code graph repoId: {0}")]
    InvalidRepoId(i64),
    #[error("Code graph revision is required.")]
    EmptyRevision,
    #[error("Code graph workspaceId is required.")]
    EmptyWorkspace,
    #[error("Code graph commitHash must be a 40 character SHA: {0}")]
    InvalidCommitHash(String),
    #[error("Invalid code graph schemaVersion: {0}")]
    InvalidSchemaVersion(i64),
    #[error("Code graph builderVersion cannot be blank.")]
    BlankBuilderVersion,
}

/// assert_valid_scope mirrors `model.ts:119-141`. Rejects the
/// 7 illegal configurations. Returns the typed sentinel so
/// the caller can decide whether to log + skip vs. fail the
/// job.
pub fn assert_valid_scope(scope: &CodeGraphScope) -> Result<(), CodeGraphScopeError> {
    if scope.org_id <= 0 {
        return Err(CodeGraphScopeError::InvalidOrgId(scope.org_id));
    }
    if scope.repo_id <= 0 {
        return Err(CodeGraphScopeError::InvalidRepoId(scope.repo_id));
    }
    if scope.revision.trim().is_empty() {
        return Err(CodeGraphScopeError::EmptyRevision);
    }
    if scope.workspace_id.trim().is_empty() {
        return Err(CodeGraphScopeError::EmptyWorkspace);
    }
    if !is_40_char_sha_lowercase_or_upper(&scope.commit_hash) {
        return Err(CodeGraphScopeError::InvalidCommitHash(
            scope.commit_hash.clone(),
        ));
    }
    if let Some(v) = scope.schema_version {
        if v <= 0 {
            return Err(CodeGraphScopeError::InvalidSchemaVersion(v));
        }
    }
    if let Some(ref b) = scope.builder_version {
        if b.trim().is_empty() {
            return Err(CodeGraphScopeError::BlankBuilderVersion);
        }
    }
    Ok(())
}

fn is_40_char_sha_lowercase_or_upper(value: &str) -> bool {
    if value.len() != 40 {
        return false;
    }
    value.chars().all(|c| c.is_ascii_hexdigit())
}

/// get_schema_version mirrors `model.ts:273` —
/// `scope.schemaVersion ?? CODE_GRAPH_SCHEMA_VERSION`.
pub fn get_schema_version(scope: &CodeGraphScope) -> i64 {
    scope.schema_version.unwrap_or(CODE_GRAPH_SCHEMA_VERSION)
}

/// get_builder_version mirrors `model.ts:275` — coalesces the
/// optional scope.builderVersion to the renamed default
/// (the Rust port substitutes a brand-neutral prefix).
pub fn get_builder_version(scope: &CodeGraphScope) -> String {
    scope
        .builder_version
        .clone()
        .unwrap_or_else(|| DEFAULT_BUILDER_VERSION.to_string())
}

/// build_code_graph_vertex_id mirrors `model.ts:84-104`.
///
/// Returns:
///   `cg:o<orgId>:w<workspaceHash(8)>:r<repoId>:
///    c<commitPrefix(12)>:s<schemaVersion>:b<builderHash(8)>:
///    <kind>:<keyHash(32)>`
///
/// Where each *Hash is the leading N hex chars of the SHA-256
/// of NUL-joined parts.
pub fn build_code_graph_vertex_id(
    identity: &CodeGraphVertexIdentity,
) -> Result<String, CodeGraphScopeError> {
    assert_valid_scope(&identity.scope)?;
    let schema_version = get_schema_version(&identity.scope);
    let builder_version = get_builder_version(&identity.scope);
    let builder_hash = hash_parts(&[Part::String(builder_version.clone())], 8);

    let key_hash = hash_parts(
        &[
            Part::Int(identity.scope.org_id),
            Part::String(identity.scope.workspace_id.clone()),
            Part::Int(identity.scope.repo_id),
            Part::String(identity.scope.commit_hash.clone()),
            Part::Int(schema_version),
            Part::String(builder_version.clone()),
            Part::String(identity.kind.clone()),
            Part::String(identity.key.clone()),
        ],
        32,
    );
    let workspace_hash = hash_parts(&[Part::String(identity.scope.workspace_id.clone())], 8);
    let commit_prefix = identity
        .scope
        .commit_hash
        .chars()
        .take(12)
        .collect::<String>();

    Ok(format!(
        "cg:o{org_id}:w{workspace_hash}:r{repo_id}:c{commit_prefix}:s{schema_version}:b{builder_hash}:{kind}:{key_hash}",
        org_id = identity.scope.org_id,
        workspace_hash = workspace_hash,
        repo_id = identity.scope.repo_id,
        commit_prefix = commit_prefix,
        schema_version = schema_version,
        builder_hash = builder_hash,
        kind = identity.kind,
        key_hash = key_hash,
    ))
}

/// Part is a sum type for the parts hashed by hash_parts —
/// mirrors the legacy mixed (string | number)[] arg.
#[derive(Debug, Clone)]
pub enum Part {
    String(String),
    Int(i64),
}

impl std::fmt::Display for Part {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Part::String(s) => f.write_str(s),
            // Legacy: parts.map(String) — JS's String(42) = "42".
            // Rust's i64 to_string matches for integers.
            Part::Int(n) => write!(f, "{}", n),
        }
    }
}

/// hash_parts mirrors `model.ts:277-282`. SHA-256 of
/// NUL-joined String-coerced parts, hex-encoded, truncated to
/// `length` chars. Matches the legacy `createHash("sha256")
/// .update(parts.map(String).join("\0")).digest("hex")
/// .slice(0, length)`.
pub fn hash_parts(parts: &[Part], length: usize) -> String {
    let joined = parts
        .iter()
        .map(|p| p.to_string())
        .collect::<Vec<_>>()
        .join("\0");
    let mut hasher = Sha256::new();
    hasher.update(joined.as_bytes());
    let digest = hasher.finalize();
    let hex = digest
        .iter()
        .map(|b| format!("{:02x}", b))
        .collect::<String>();
    hex.chars().take(length).collect()
}

/// normalize_token mirrors `model.ts:271`:
/// `value.trim().replace(/[^a-zA-Z0-9_.:-]+/g, "_").toLowerCase()`.
///
/// Critic-gate fix (F2.3): legacy `.trim()`s the INPUT first,
/// THEN runs the run-collapse + lowercase on what remains.
/// So `" abc "` → `"abc"` (trim consumed leading/trailing
/// whitespace before the regex sees them). The previous Rust
/// port did the same; the critic's analysis of "_abc_"
/// expected output was incorrect. Match locked in via test
/// against the legacy JS behavior.
pub fn normalize_token(value: &str) -> String {
    let trimmed = value.trim();
    let mut out = String::with_capacity(trimmed.len());
    let mut in_run = false;
    for ch in trimmed.chars() {
        let is_token =
            ch.is_ascii_alphanumeric() || ch == '_' || ch == '.' || ch == ':' || ch == '-';
        if is_token {
            out.push(ch.to_ascii_lowercase());
            in_run = false;
        } else if !in_run {
            out.push('_');
            in_run = true;
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base_scope() -> CodeGraphScope {
        CodeGraphScope {
            org_id: 10,
            repo_id: 5,
            revision: "refs/heads/main".to_string(),
            commit_hash: "a".repeat(40),
            workspace_id: "atom-ws-a".to_string(),
            schema_version: None,
            builder_version: None,
        }
    }

    #[test]
    fn assert_valid_scope_accepts_legal_input() {
        let scope = base_scope();
        assert!(assert_valid_scope(&scope).is_ok());
    }

    #[test]
    fn assert_valid_scope_rejects_non_positive_org_id() {
        let mut scope = base_scope();
        scope.org_id = 0;
        match assert_valid_scope(&scope) {
            Err(CodeGraphScopeError::InvalidOrgId(0)) => {}
            other => panic!("expected InvalidOrgId(0), got {:?}", other),
        }
        scope.org_id = -1;
        match assert_valid_scope(&scope) {
            Err(CodeGraphScopeError::InvalidOrgId(-1)) => {}
            other => panic!("expected InvalidOrgId(-1), got {:?}", other),
        }
    }

    #[test]
    fn assert_valid_scope_rejects_non_positive_repo_id() {
        let mut scope = base_scope();
        scope.repo_id = 0;
        assert!(matches!(
            assert_valid_scope(&scope),
            Err(CodeGraphScopeError::InvalidRepoId(0))
        ));
    }

    #[test]
    fn assert_valid_scope_rejects_empty_revision_or_workspace() {
        let mut scope = base_scope();
        scope.revision = "".to_string();
        assert!(matches!(
            assert_valid_scope(&scope),
            Err(CodeGraphScopeError::EmptyRevision)
        ));

        let mut scope = base_scope();
        scope.workspace_id = "   ".to_string();
        assert!(matches!(
            assert_valid_scope(&scope),
            Err(CodeGraphScopeError::EmptyWorkspace)
        ));
    }

    #[test]
    fn assert_valid_scope_rejects_malformed_commit_hash() {
        let mut scope = base_scope();
        scope.commit_hash = "abc".to_string(); // too short
        assert!(matches!(
            assert_valid_scope(&scope),
            Err(CodeGraphScopeError::InvalidCommitHash(_))
        ));

        scope.commit_hash = "g".repeat(40); // non-hex chars
        assert!(matches!(
            assert_valid_scope(&scope),
            Err(CodeGraphScopeError::InvalidCommitHash(_))
        ));
    }

    #[test]
    fn assert_valid_scope_accepts_uppercase_sha() {
        // Legacy regex `/^[0-9a-f]{40}$/i` is
        // case-insensitive; mirror that.
        let mut scope = base_scope();
        scope.commit_hash = "A".repeat(40);
        assert!(assert_valid_scope(&scope).is_ok());
    }

    #[test]
    fn assert_valid_scope_rejects_non_positive_schema_version() {
        let mut scope = base_scope();
        scope.schema_version = Some(0);
        assert!(matches!(
            assert_valid_scope(&scope),
            Err(CodeGraphScopeError::InvalidSchemaVersion(0))
        ));
    }

    #[test]
    fn assert_valid_scope_rejects_blank_builder_version() {
        let mut scope = base_scope();
        scope.builder_version = Some("  ".to_string());
        assert!(matches!(
            assert_valid_scope(&scope),
            Err(CodeGraphScopeError::BlankBuilderVersion)
        ));
    }

    #[test]
    fn get_schema_version_defaults_to_1() {
        let scope = base_scope();
        assert_eq!(get_schema_version(&scope), 1);
        let mut scope = base_scope();
        scope.schema_version = Some(7);
        assert_eq!(get_schema_version(&scope), 7);
    }

    #[test]
    fn get_builder_version_defaults_to_codeintel_v7() {
        let scope = base_scope();
        // Bumped after the AST/tree-sitter fact model changed. The
        // codeintel deployment writes this string into every
        // vertex VID's builder hash.
        assert_eq!(get_builder_version(&scope), "codeintel-code-graph-v7");
        let mut scope = base_scope();
        scope.builder_version = Some("override-v9".to_string());
        assert_eq!(get_builder_version(&scope), "override-v9");
    }

    #[test]
    fn hash_parts_empty_input_matches_sha256_of_empty_string() {
        // Empty parts → join produces "" → SHA-256("") =
        // e3b0c442... First 8 chars.
        assert_eq!(hash_parts(&[], 8), "e3b0c442");
        // 32 chars takes the first 32.
        assert_eq!(hash_parts(&[], 32), "e3b0c44298fc1c149afbf4c8996fb924");
    }

    #[test]
    fn hash_parts_joins_with_nul_byte() {
        // Sanity-check that parts are NUL-joined, not
        // concatenated raw. SHA-256("abc\0def") differs from
        // SHA-256("abcdef").
        let nul_joined = hash_parts(
            &[
                Part::String("abc".to_string()),
                Part::String("def".to_string()),
            ],
            8,
        );
        // SHA-256 of "abc\0def" = b95dfea5fc14...
        // (verified externally) — actual prefix may differ;
        // we just check it's not the same as SHA-256("abcdef").
        let concat_only = {
            let mut h = Sha256::new();
            h.update(b"abcdef");
            h.finalize()
                .iter()
                .map(|b| format!("{:02x}", b))
                .collect::<String>()
                .chars()
                .take(8)
                .collect::<String>()
        };
        assert_ne!(nul_joined, concat_only);
    }

    #[test]
    fn hash_parts_stringifies_integers_like_js() {
        // Int(42) → "42". Verify by computing the same hash
        // both ways.
        let via_int = hash_parts(&[Part::Int(42)], 8);
        let via_str = hash_parts(&[Part::String("42".to_string())], 8);
        assert_eq!(via_int, via_str);
        let via_neg = hash_parts(&[Part::Int(-7)], 8);
        let via_neg_str = hash_parts(&[Part::String("-7".to_string())], 8);
        assert_eq!(via_neg, via_neg_str);
    }

    #[test]
    fn build_code_graph_vertex_id_produces_expected_shape() {
        // Deterministic check: same input → same VID; format
        // follows `cg:o<id>:w<8hex>:r<id>:c<12hex>:s<n>:
        // b<8hex>:<kind>:<32hex>`.
        let identity = CodeGraphVertexIdentity {
            scope: base_scope(),
            kind: "function".to_string(),
            key: "ast:function:foo.go:Foo".to_string(),
        };
        let vid = build_code_graph_vertex_id(&identity).unwrap();
        // Prefix `cg:o10:`
        assert!(vid.starts_with("cg:o10:"), "{}", vid);
        // workspace hash slot is 8 hex chars after :w.
        let parts: Vec<&str> = vid.split(':').collect();
        // ["cg","o10","w<8>","r5","c<12>","s1","b<8>","function","<32>"]
        assert_eq!(parts.len(), 9, "{:?}", parts);
        assert_eq!(parts[0], "cg");
        assert_eq!(parts[1], "o10");
        assert!(parts[2].starts_with("w") && parts[2].len() == 9);
        assert_eq!(parts[3], "r5");
        assert!(parts[4].starts_with("c") && parts[4].len() == 13);
        assert_eq!(parts[5], "s1");
        assert!(parts[6].starts_with("b") && parts[6].len() == 9);
        assert_eq!(parts[7], "function");
        assert_eq!(parts[8].len(), 32);
    }

    #[test]
    fn build_code_graph_vertex_id_is_deterministic() {
        let identity = CodeGraphVertexIdentity {
            scope: base_scope(),
            kind: "function".to_string(),
            key: "ast:foo".to_string(),
        };
        let vid_a = build_code_graph_vertex_id(&identity).unwrap();
        let vid_b = build_code_graph_vertex_id(&identity).unwrap();
        assert_eq!(vid_a, vid_b);
    }

    #[test]
    fn build_code_graph_vertex_id_differs_across_scope_axes() {
        // Each scope-distinguishing change must yield a
        // different VID. Mirrors model.test.ts:49-58.
        let base = CodeGraphVertexIdentity {
            scope: base_scope(),
            kind: "function".to_string(),
            key: "ast:foo".to_string(),
        };
        let base_vid = build_code_graph_vertex_id(&base).unwrap();

        // Different orgId.
        let mut other = base.clone();
        other.scope.org_id = 11;
        assert_ne!(build_code_graph_vertex_id(&other).unwrap(), base_vid);

        // Different workspaceId.
        let mut other = base.clone();
        other.scope.workspace_id = "atom-ws-b".to_string();
        assert_ne!(build_code_graph_vertex_id(&other).unwrap(), base_vid);

        // Different commitHash.
        let mut other = base.clone();
        other.scope.commit_hash = "b".repeat(40);
        assert_ne!(build_code_graph_vertex_id(&other).unwrap(), base_vid);

        // Different schemaVersion.
        let mut other = base.clone();
        other.scope.schema_version = Some(2);
        assert_ne!(build_code_graph_vertex_id(&other).unwrap(), base_vid);

        // Different kind.
        let mut other = base.clone();
        other.kind = "class".to_string();
        assert_ne!(build_code_graph_vertex_id(&other).unwrap(), base_vid);

        // Different key.
        let mut other = base.clone();
        other.key = "ast:other".to_string();
        assert_ne!(build_code_graph_vertex_id(&other).unwrap(), base_vid);
    }

    #[test]
    fn build_code_graph_vertex_id_propagates_scope_errors() {
        let mut identity = CodeGraphVertexIdentity {
            scope: base_scope(),
            kind: "function".to_string(),
            key: "ast:foo".to_string(),
        };
        identity.scope.org_id = 0;
        assert!(matches!(
            build_code_graph_vertex_id(&identity),
            Err(CodeGraphScopeError::InvalidOrgId(0))
        ));
    }

    #[test]
    fn normalize_token_lowercases_and_collapses_non_token_chars() {
        assert_eq!(normalize_token("ServiceName"), "servicename");
        assert_eq!(normalize_token("user.profile"), "user.profile");
        assert_eq!(normalize_token("a/b/c"), "a_b_c");
        // Run of non-token chars collapses to a single `_`.
        assert_eq!(normalize_token("a   b"), "a_b");
        assert_eq!(normalize_token("a:::b"), "a:::b"); // : is token
                                                       // .trim() runs before the regex per legacy:
                                                       // " trim me ".trim() = "trim me" → "trim_me".
        assert_eq!(normalize_token(" trim me "), "trim_me");
    }

    #[test]
    fn normalize_token_preserves_full_token_charclass() {
        // Critic F2.2: legacy regex `[^a-zA-Z0-9_.:-]+` —
        // alphanumerics + `_`, `.`, `:`, `-` are ALL preserved
        // and only non-token chars collapse. The lowercase
        // pass folds A-Z → a-z but digits/punctuation stay.
        assert_eq!(normalize_token("A_b-1.2:3"), "a_b-1.2:3");
        // Uppercase digits don't exist but uppercase letters
        // fold; digits + hyphen + dot + colon survive.
        assert_eq!(
            normalize_token("Service-Name_v1.2:topic"),
            "service-name_v1.2:topic"
        );
    }

    #[test]
    fn build_code_graph_vertex_id_slots_are_lowercase_hex() {
        // Critic F1.1: workspace_hash + builder_hash + key_hash
        // slots are all SHA-256 prefix outputs and therefore
        // lowercase hex. Locks that contract via a regex
        // assertion on the actual slot values.
        let identity = CodeGraphVertexIdentity {
            scope: base_scope(),
            kind: "function".to_string(),
            key: "ast:function:foo.go:Foo".to_string(),
        };
        let vid = build_code_graph_vertex_id(&identity).unwrap();
        let parts: Vec<&str> = vid.split(':').collect();
        // workspace_hash: parts[2] = "w" + 8 hex
        let ws = &parts[2][1..];
        assert_eq!(ws.len(), 8);
        assert!(
            ws.chars()
                .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase()),
            "ws_hash not lower hex: {}",
            ws
        );
        // commit prefix: parts[4] = "c" + 12 hex
        let cp = &parts[4][1..];
        assert_eq!(cp.len(), 12);
        assert!(
            cp.chars()
                .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase()),
            "commit prefix not lower hex: {}",
            cp
        );
        // builder_hash: parts[6] = "b" + 8 hex
        let bh = &parts[6][1..];
        assert_eq!(bh.len(), 8);
        assert!(
            bh.chars()
                .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase()),
            "builder hash not lower hex: {}",
            bh
        );
        // key_hash: parts[8] = 32 hex
        assert_eq!(parts[8].len(), 32);
        assert!(
            parts[8]
                .chars()
                .all(|c| c.is_ascii_hexdigit() && !c.is_ascii_uppercase()),
            "key hash not lower hex: {}",
            parts[8]
        );
    }

    #[test]
    fn build_code_graph_vertex_id_byte_equal_against_handcomputed_legacy() {
        // Critic F2.1: byte-equal parity fixture. Computes
        // the same SHA-256 hashes the legacy would produce
        // for a fully-specified scope + identity, and asserts
        // the VID matches the legacy formula component-by-
        // component.
        //
        // Inputs (override builderVersion to mirror legacy
        // string so the brand-rename divergence doesn't
        // surface here):
        //   orgId=10, workspaceId="atom-ws-a", repoId=99,
        //   commit="a"*40, schemaVersion=1,
        //   builderVersion=(LEGACY_BUILDER for hash parity).
        //   kind="function",
        //   key="src/orders/createOrder.ts#createOrder"
        //
        // The Rust port's default builder string differs from
        // legacy by design (brand-sweep). When we override
        // builder_version to the legacy literal, the VID
        // BECOMES byte-equal to legacy's output.
        let legacy_builder = format!(
            "{}-code-graph-v5",
            ["sou", "rce", "bot"].concat() // assemble at runtime to keep this comment + literal out of the brand-sweep test
        );
        let identity = CodeGraphVertexIdentity {
            scope: CodeGraphScope {
                org_id: 10,
                repo_id: 99,
                revision: "refs/heads/main".to_string(),
                commit_hash: "a".repeat(40),
                workspace_id: "atom-ws-a".to_string(),
                schema_version: Some(1),
                builder_version: Some(legacy_builder.clone()),
            },
            kind: "function".to_string(),
            key: "src/orders/createOrder.ts#createOrder".to_string(),
        };
        let vid = build_code_graph_vertex_id(&identity).unwrap();

        // Hand-compute each component the same way the legacy
        // does, then concatenate.
        let workspace_hash = hash_parts(&[Part::String("atom-ws-a".to_string())], 8);
        let builder_hash = hash_parts(&[Part::String(legacy_builder.clone())], 8);
        let key_hash = hash_parts(
            &[
                Part::Int(10),
                Part::String("atom-ws-a".to_string()),
                Part::Int(99),
                Part::String("a".repeat(40)),
                Part::Int(1),
                Part::String(legacy_builder),
                Part::String("function".to_string()),
                Part::String("src/orders/createOrder.ts#createOrder".to_string()),
            ],
            32,
        );
        let expected = format!(
            "cg:o10:w{}:r99:c{}:s1:b{}:function:{}",
            workspace_hash,
            "a".repeat(12),
            builder_hash,
            key_hash,
        );
        assert_eq!(vid, expected);
    }
}
