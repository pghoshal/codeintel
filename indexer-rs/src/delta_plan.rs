//! Per-engine delta plan computation. Direct port of
//! `packages/backend/src/indexDeltaPlan.ts` (261 LOC).
//!
//! Pure transform — no I/O, no database. Given a previous and
//! a current `IndexRunManifest` for the same scope (org +
//! workspace + repo + provider + branch), decide for each
//! engine whether to NOOP, do a delta, or reindex everything.
//!
//! Strategies are deliberately granular per engine because:
//!   - Zoekt builds shards per repo + branch. Today the wrapper
//!     doesn't support file-level shard updates, so any non-zero
//!     touch list forces FULL_REPO_REWRITE. The legacy carries a
//!     `supportsZoektFileDelta` boolean for the future where
//!     shard-level deltas land.
//!   - SCIP indexers need PROJECT_ROOT context to resolve
//!     symbols, so the granularity is "re-extract the project
//!     roots that contain touched files". If the toolchain
//!     fingerprint changed (e.g. scip-typescript version bump),
//!     every symbol could resolve differently → FULL_REPO.
//!   - Code graph is per-file: re-extract added + changed,
//!     drop deleted.
//!   - Semantic facts come from an LLM prompt; if the prompt /
//!     model / schema fingerprint changed, every chunk's meaning
//!     could differ → ALL_CHUNKS. Otherwise only re-run on
//!     touched files.
//!
//! Wire-byte parity: the strategy values (NOOP, DELTA_FILES,
//! FULL_REPO, FULL_REPO_REWRITE, PROJECT_ROOTS, CHANGED_CHUNKS, ALL_CHUNKS) are
//! persisted to the database columns
//! RepoIndexManifest.{zoektStrategy, scipStrategy,
//! graphStrategy, semanticStrategy} — they MUST match the
//! legacy strings byte-for-byte. The serde rename_all directive
//! on each enum locks this.
//!
//! Sort note: legacy uses `localeCompare` which is Unicode-
//! aware. The Rust port uses byte-order `str::cmp` which is
//! equivalent for ASCII paths (the only kind a Zoekt-indexed
//! repo ever contains in practice — git itself enforces UTF-8
//! at a minimum and the test fixtures are pure ASCII). A
//! follow-up could plug in `unicode-segmentation` if a Unicode-
//! path fixture surfaces a mismatch.

use serde::{Deserialize, Serialize};
use serde_json::Value as JsonValue;
use std::collections::{BTreeMap, BTreeSet};

/// IndexerKind mirrors the legacy union "zoekt" | "scip" |
/// "graph" | "semantic-llm". Used to key the per-engine
/// `artifacts` map on each IndexManifestFile.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq, Hash, Ord, PartialOrd)]
#[serde(rename_all = "kebab-case")]
pub enum IndexerKind {
    Zoekt,
    Scip,
    Graph,
    SemanticLlm,
}

/// IndexManifestFile mirrors the legacy struct
/// (indexDeltaPlan.ts:3-12). The `artifacts` field captures
/// the per-engine artefact pointer the prior run produced —
/// today only Zoekt populates it, the rest reserve the slot.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct IndexManifestFile {
    pub path: String,
    #[serde(rename = "contentHash")]
    pub content_hash: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub language: Option<String>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "projectRoot"
    )]
    pub project_root: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub generated: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub vendor: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub test: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub artifacts: Option<BTreeMap<IndexerKind, String>>,
}

/// SemanticExtractionFingerprint mirrors indexDeltaPlan.ts:14-18.
/// The trio (promptVersion, modelId, schemaVersion) defines
/// "what does a fact MEAN" — a change in any one invalidates
/// every previously-extracted semantic fact.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SemanticExtractionFingerprint {
    #[serde(rename = "promptVersion")]
    pub prompt_version: String,
    #[serde(rename = "modelId")]
    pub model_id: String,
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
}

/// IndexRunManifest mirrors indexDeltaPlan.ts:20-30.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct IndexRunManifest {
    #[serde(rename = "orgId")]
    pub org_id: i32,
    #[serde(rename = "workspaceId")]
    pub workspace_id: String,
    #[serde(rename = "repoId")]
    pub repo_id: i32,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "providerConnectionId"
    )]
    pub provider_connection_id: Option<String>,
    pub branch: String,
    #[serde(rename = "commitHash")]
    pub commit_hash: String,
    pub files: Vec<IndexManifestFile>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "scipToolchains"
    )]
    pub scip_toolchains: Option<BTreeMap<String, String>>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "semanticExtraction"
    )]
    pub semantic_extraction: Option<SemanticExtractionFingerprint>,
}

/// Mode mirrors the top-level `mode` field on DeltaReindexPlan.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum Mode {
    Noop,
    Delta,
    Full,
}

/// ZoektStrategy mirrors indexDeltaPlan.ts:39.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum ZoektStrategy {
    Noop,
    DeltaFiles,
    FullRepo,
    FullRepoRewrite,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ZoektPlan {
    pub strategy: ZoektStrategy,
    pub files: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

/// ScipStrategy mirrors indexDeltaPlan.ts:44.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum ScipStrategy {
    Noop,
    ProjectRoots,
    FullRepo,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct ScipPlan {
    pub strategy: ScipStrategy,
    #[serde(rename = "projectRoots")]
    pub project_roots: Vec<String>,
    pub files: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

/// GraphStrategy mirrors indexDeltaPlan.ts:50.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum GraphStrategy {
    Noop,
    DeltaFiles,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct GraphPlan {
    pub strategy: GraphStrategy,
    pub files: Vec<String>,
    #[serde(rename = "deletedFiles")]
    pub deleted_files: Vec<String>,
}

/// SemanticStrategy mirrors indexDeltaPlan.ts:55.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum SemanticStrategy {
    Noop,
    ChangedChunks,
    AllChunks,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SemanticPlan {
    pub strategy: SemanticStrategy,
    pub files: Vec<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reason: Option<String>,
}

/// DeltaReindexPlan mirrors the legacy return shape
/// (indexDeltaPlan.ts:32-59).
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct DeltaReindexPlan {
    pub mode: Mode,
    #[serde(rename = "addedFiles")]
    pub added_files: Vec<String>,
    #[serde(rename = "changedFiles")]
    pub changed_files: Vec<String>,
    #[serde(rename = "deletedFiles")]
    pub deleted_files: Vec<String>,
    #[serde(rename = "unchangedFiles")]
    pub unchanged_files: Vec<String>,
    pub zoekt: ZoektPlan,
    pub scip: ScipPlan,
    pub graph: GraphPlan,
    pub semantic: SemanticPlan,
}

/// ScopeMismatch is the error returned when previous + current
/// disagree on (orgId, workspaceId, repoId,
/// providerConnectionId, branch). The legacy throws an Error;
/// the Rust port returns a typed sentinel so the caller can
/// decide whether to log + skip vs. fail the job.
#[derive(Debug, thiserror::Error)]
pub enum DeltaPlanError {
    #[error("Index manifest scope mismatch for {field}. Delta reindex cannot share artifacts across tenants, workspaces, repos, providers, or branches.")]
    ScopeMismatch { field: &'static str },
}

/// build_delta_reindex_plan is the direct port of
/// indexDeltaPlan.ts:67-217.
///
/// `supports_zoekt_file_delta` defaults to false to mirror the
/// legacy default. The legacy wires it from a deployment flag
/// when shard-level deltas land; the Rust port surfaces the
/// same knob.
pub fn build_delta_reindex_plan(
    previous: Option<&IndexRunManifest>,
    current: &IndexRunManifest,
    supports_zoekt_file_delta: bool,
) -> std::result::Result<DeltaReindexPlan, DeltaPlanError> {
    let current_files = normalized_file_map(&current.files);

    // ---- No previous manifest -> FULL reindex on every engine. ----
    if previous.is_none() {
        let all_files: Vec<String> = sorted(current_files.keys().cloned());
        let all_project_roots: Vec<String> =
            sorted(unique_project_roots(&current.files).into_iter());
        return Ok(DeltaReindexPlan {
            mode: Mode::Full,
            added_files: all_files.clone(),
            changed_files: vec![],
            deleted_files: vec![],
            unchanged_files: vec![],
            zoekt: ZoektPlan {
                strategy: ZoektStrategy::FullRepo,
                files: all_files.clone(),
                reason: Some("No previous manifest exists.".to_string()),
            },
            scip: ScipPlan {
                strategy: ScipStrategy::FullRepo,
                project_roots: all_project_roots,
                files: all_files.clone(),
                reason: Some("No previous manifest exists.".to_string()),
            },
            graph: GraphPlan {
                strategy: GraphStrategy::DeltaFiles,
                files: all_files.clone(),
                deleted_files: vec![],
            },
            semantic: SemanticPlan {
                strategy: SemanticStrategy::AllChunks,
                files: all_files,
                reason: Some("No previous semantic extraction cache exists.".to_string()),
            },
        });
    }
    let previous = previous.unwrap();

    assert_same_manifest_scope(previous, current)?;

    let previous_files = normalized_file_map(&previous.files);

    let mut added_files: Vec<String> = Vec::new();
    let mut changed_files: Vec<String> = Vec::new();
    let mut deleted_files: Vec<String> = Vec::new();
    let mut unchanged_files: Vec<String> = Vec::new();

    for (path, file) in &current_files {
        match previous_files.get(path) {
            None => added_files.push(path.clone()),
            Some(prev_file) if prev_file.content_hash != file.content_hash => {
                changed_files.push(path.clone())
            }
            Some(_) => unchanged_files.push(path.clone()),
        }
    }
    for path in previous_files.keys() {
        if !current_files.contains_key(path) {
            deleted_files.push(path.clone());
        }
    }

    let touched_files: Vec<String> = {
        let mut v: Vec<String> = added_files
            .iter()
            .chain(changed_files.iter())
            .chain(deleted_files.iter())
            .cloned()
            .collect();
        v.sort();
        v
    };

    let changed_current_files: Vec<&IndexManifestFile> = added_files
        .iter()
        .chain(changed_files.iter())
        .filter_map(|p| current_files.get(p))
        .collect();
    let deleted_previous_files: Vec<&IndexManifestFile> = deleted_files
        .iter()
        .filter_map(|p| previous_files.get(p))
        .collect();

    let scip_toolchain_changed = !stable_json_equal(
        &json_or_empty_object(&previous.scip_toolchains),
        &json_or_empty_object(&current.scip_toolchains),
    );
    let semantic_fingerprint_changed = !stable_json_equal(
        &json_or_null(&previous.semantic_extraction),
        &json_or_null(&current.semantic_extraction),
    );

    // ---- NOOP path: nothing touched + fingerprints unchanged + same commit. ----
    if touched_files.is_empty()
        && !scip_toolchain_changed
        && !semantic_fingerprint_changed
        && previous.commit_hash == current.commit_hash
    {
        return Ok(DeltaReindexPlan {
            mode: Mode::Noop,
            added_files: vec![],
            changed_files: vec![],
            deleted_files: vec![],
            unchanged_files: sorted(unchanged_files.into_iter()),
            zoekt: ZoektPlan {
                strategy: ZoektStrategy::Noop,
                files: vec![],
                reason: None,
            },
            scip: ScipPlan {
                strategy: ScipStrategy::Noop,
                project_roots: vec![],
                files: vec![],
                reason: None,
            },
            graph: GraphPlan {
                strategy: GraphStrategy::Noop,
                files: vec![],
                deleted_files: vec![],
            },
            semantic: SemanticPlan {
                strategy: SemanticStrategy::Noop,
                files: vec![],
                reason: None,
            },
        });
    }

    // ---- DELTA path. ----
    let changed_project_roots: Vec<String> = {
        let mut set: BTreeSet<String> = BTreeSet::new();
        for f in changed_current_files
            .iter()
            .chain(deleted_previous_files.iter())
        {
            set.insert(f.project_root.clone().unwrap_or_else(|| ".".to_string()));
        }
        set.into_iter().collect()
    };
    let all_project_roots: Vec<String> = sorted(unique_project_roots(&current.files).into_iter());
    let all_current_files: Vec<String> = sorted(current_files.keys().cloned());

    let added_sorted = sorted(added_files.iter().cloned());
    let changed_sorted = sorted(changed_files.iter().cloned());
    let deleted_sorted = sorted(deleted_files.iter().cloned());
    let unchanged_sorted = sorted(unchanged_files.iter().cloned());
    let added_plus_changed: Vec<String> = {
        let mut v: Vec<String> = added_sorted
            .iter()
            .chain(changed_sorted.iter())
            .cloned()
            .collect();
        v.sort();
        v
    };
    let touched_for_scip: Vec<String> = {
        let mut v: Vec<String> = added_sorted
            .iter()
            .chain(changed_sorted.iter())
            .chain(deleted_sorted.iter())
            .cloned()
            .collect();
        v.sort();
        v
    };

    let mode = if !touched_files.is_empty() {
        Mode::Delta
    } else {
        Mode::Noop
    };

    let zoekt = if touched_files.is_empty() {
        ZoektPlan {
            strategy: ZoektStrategy::Noop,
            files: vec![],
            reason: None,
        }
    } else if supports_zoekt_file_delta {
        ZoektPlan {
            strategy: ZoektStrategy::DeltaFiles,
            files: touched_files.clone(),
            reason: Some("Search engine supports file-level shard updates.".to_string()),
        }
    } else {
        ZoektPlan {
            strategy: ZoektStrategy::FullRepoRewrite,
            files: all_current_files.clone(),
            reason: Some(
                "Zoekt rewrites repository shard files because file-level shard delta is not enabled.".to_string(),
            ),
        }
    };

    let scip = if scip_toolchain_changed {
        ScipPlan {
            strategy: ScipStrategy::FullRepo,
            project_roots: all_project_roots.clone(),
            files: all_current_files.clone(),
            reason: Some("SCIP toolchain fingerprint changed.".to_string()),
        }
    } else if !changed_project_roots.is_empty() {
        ScipPlan {
            strategy: ScipStrategy::ProjectRoots,
            project_roots: changed_project_roots,
            files: touched_for_scip,
            reason: Some(
                "Language indexers require project-root context for precise symbols.".to_string(),
            ),
        }
    } else {
        ScipPlan {
            strategy: ScipStrategy::Noop,
            project_roots: vec![],
            files: vec![],
            reason: None,
        }
    };

    let graph = if touched_files.is_empty() {
        GraphPlan {
            strategy: GraphStrategy::Noop,
            files: vec![],
            deleted_files: vec![],
        }
    } else {
        GraphPlan {
            strategy: GraphStrategy::DeltaFiles,
            files: added_plus_changed.clone(),
            deleted_files: deleted_sorted.clone(),
        }
    };

    let semantic = if semantic_fingerprint_changed {
        SemanticPlan {
            strategy: SemanticStrategy::AllChunks,
            files: all_current_files,
            reason: Some("Semantic extraction prompt, model, or schema changed.".to_string()),
        }
    } else if !touched_files.is_empty() {
        SemanticPlan {
            strategy: SemanticStrategy::ChangedChunks,
            files: added_plus_changed,
            reason: None,
        }
    } else {
        SemanticPlan {
            strategy: SemanticStrategy::Noop,
            files: vec![],
            reason: None,
        }
    };

    Ok(DeltaReindexPlan {
        mode,
        added_files: added_sorted,
        changed_files: changed_sorted,
        deleted_files: deleted_sorted,
        unchanged_files: unchanged_sorted,
        zoekt,
        scip,
        graph,
        semantic,
    })
}

fn assert_same_manifest_scope(
    previous: &IndexRunManifest,
    current: &IndexRunManifest,
) -> std::result::Result<(), DeltaPlanError> {
    if previous.org_id != current.org_id {
        return Err(DeltaPlanError::ScopeMismatch { field: "orgId" });
    }
    if previous.workspace_id != current.workspace_id {
        return Err(DeltaPlanError::ScopeMismatch {
            field: "workspaceId",
        });
    }
    if previous.repo_id != current.repo_id {
        return Err(DeltaPlanError::ScopeMismatch { field: "repoId" });
    }
    if previous.provider_connection_id != current.provider_connection_id {
        return Err(DeltaPlanError::ScopeMismatch {
            field: "providerConnectionId",
        });
    }
    if previous.branch != current.branch {
        return Err(DeltaPlanError::ScopeMismatch { field: "branch" });
    }
    Ok(())
}

/// normalized_file_map mirrors the legacy normalizedFileMap.
/// Keys are normalized paths; values carry the same normalized
/// path so downstream code can rely on the BTreeMap key + the
/// file's `path` field agreeing.
fn normalized_file_map(files: &[IndexManifestFile]) -> BTreeMap<String, IndexManifestFile> {
    let mut map = BTreeMap::new();
    for file in files {
        let normalized = normalize_manifest_path(&file.path);
        let mut entry = file.clone();
        entry.path = normalized.clone();
        entry.project_root = file.project_root.as_deref().map(normalize_manifest_path);
        map.insert(normalized, entry);
    }
    map
}

/// normalize_manifest_path mirrors indexDeltaPlan.ts:248. Two
/// rules: backslash -> forward slash, then strip leading
/// forward slashes.
fn normalize_manifest_path(value: &str) -> String {
    let forward = value.replace('\\', "/");
    let trimmed = forward.trim_start_matches('/');
    trimmed.to_string()
}

fn unique_project_roots(files: &[IndexManifestFile]) -> BTreeSet<String> {
    let mut set = BTreeSet::new();
    for f in files {
        set.insert(f.project_root.clone().unwrap_or_else(|| ".".to_string()));
    }
    set
}

/// sorted mirrors the legacy `sorted` helper — collects into a
/// Vec and sorts. localeCompare-vs-byte-cmp note is in the
/// module doc.
fn sorted(items: impl IntoIterator<Item = String>) -> Vec<String> {
    let mut v: Vec<String> = items.into_iter().collect();
    v.sort();
    v
}

/// stable_json_equal mirrors the legacy stableJsonEqual: compare
/// two JsonValues for structural equality, key-order-invariant.
/// Walks objects recursively, normalizes to BTreeMap-equivalent
/// before comparison.
fn stable_json_equal(left: &JsonValue, right: &JsonValue) -> bool {
    sort_json(left) == sort_json(right)
}

fn sort_json(value: &JsonValue) -> JsonValue {
    match value {
        JsonValue::Array(arr) => JsonValue::Array(arr.iter().map(sort_json).collect()),
        JsonValue::Object(obj) => {
            let sorted: BTreeMap<String, JsonValue> =
                obj.iter().map(|(k, v)| (k.clone(), sort_json(v))).collect();
            // Re-emit as a serde_json Map (which preserves
            // insertion order). BTreeMap iter is alphabetic.
            let mut out = serde_json::Map::new();
            for (k, v) in sorted {
                out.insert(k, v);
            }
            JsonValue::Object(out)
        }
        _ => value.clone(),
    }
}

fn json_or_empty_object<T: Serialize>(v: &Option<T>) -> JsonValue {
    match v {
        // The two call sites pass a BTreeMap<String,String> and
        // a 3-field POD struct (SemanticExtractionFingerprint).
        // Both are infallible under serde_json; .expect surfaces
        // a regression if the input types change.
        Some(inner) => {
            serde_json::to_value(inner).expect("scip toolchain map serialises to JSON infallibly")
        }
        None => JsonValue::Object(serde_json::Map::new()),
    }
}

fn json_or_null<T: Serialize>(v: &Option<T>) -> JsonValue {
    match v {
        Some(inner) => serde_json::to_value(inner)
            .expect("semantic-extraction fingerprint serialises to JSON infallibly"),
        None => JsonValue::Null,
    }
}

#[cfg(test)]
mod tests {
    //! Mirrors `packages/backend/src/indexDeltaPlan.test.ts`
    //! (5 test blocks) and adds 4 Rust-side internal-helper
    //! tests beyond the legacy surface:
    //!   - `no_previous_manifest_returns_full_plan` (covers the
    //!     `previous = None` branch the legacy spec assumes via
    //!     a separate caller)
    //!   - `scip_toolchain_change_forces_scip_full_repo`
    //!   - `normalize_manifest_path_strips_backslash_and_leading_slash`
    //!   - `stable_json_equal_is_key_order_invariant`

    use super::*;

    fn base_file_orders() -> IndexManifestFile {
        let mut artifacts = BTreeMap::new();
        artifacts.insert(IndexerKind::Zoekt, "zoekt-shard-a".to_string());
        artifacts.insert(IndexerKind::Scip, "scip-index-a".to_string());
        artifacts.insert(IndexerKind::Graph, "graph-node-a".to_string());
        artifacts.insert(IndexerKind::SemanticLlm, "semantic-chunk-a".to_string());
        IndexManifestFile {
            path: "src/orders/createOrder.ts".to_string(),
            content_hash: "hash-orders-v1".to_string(),
            language: Some("typescript".to_string()),
            project_root: Some(".".to_string()),
            generated: None,
            vendor: None,
            test: None,
            artifacts: Some(artifacts),
        }
    }

    fn base_file_docs() -> IndexManifestFile {
        let mut artifacts = BTreeMap::new();
        artifacts.insert(IndexerKind::Graph, "graph-doc-a".to_string());
        artifacts.insert(IndexerKind::SemanticLlm, "semantic-doc-a".to_string());
        IndexManifestFile {
            path: "docs/architecture.md".to_string(),
            content_hash: "hash-docs-v1".to_string(),
            language: Some("markdown".to_string()),
            project_root: Some(".".to_string()),
            generated: None,
            vendor: None,
            test: None,
            artifacts: Some(artifacts),
        }
    }

    fn base_manifest() -> IndexRunManifest {
        let mut toolchains = BTreeMap::new();
        toolchains.insert(
            "typescript:scip-typescript".to_string(),
            "sha256:ts-v1".to_string(),
        );
        IndexRunManifest {
            org_id: 42,
            workspace_id: "atom-workspace-a".to_string(),
            repo_id: 7,
            provider_connection_id: Some("github-installation-1".to_string()),
            branch: "refs/heads/main".to_string(),
            commit_hash: "commit-a".to_string(),
            files: vec![base_file_orders(), base_file_docs()],
            scip_toolchains: Some(toolchains),
            semantic_extraction: Some(SemanticExtractionFingerprint {
                prompt_version: "graphify-v1".to_string(),
                model_id: "glm-5".to_string(),
                schema_version: 1,
            }),
        }
    }

    #[test]
    fn noop_when_scope_commit_files_toolchain_and_semantic_unchanged() {
        let prev = base_manifest();
        let curr = base_manifest();
        let plan = build_delta_reindex_plan(Some(&prev), &curr, false).unwrap();
        assert_eq!(plan.mode, Mode::Noop);
        assert_eq!(plan.zoekt.strategy, ZoektStrategy::Noop);
        assert_eq!(plan.scip.strategy, ScipStrategy::Noop);
        assert_eq!(plan.graph.strategy, GraphStrategy::Noop);
        assert_eq!(plan.semantic.strategy, SemanticStrategy::Noop);
        assert_eq!(
            plan.unchanged_files,
            vec![
                "docs/architecture.md".to_string(),
                "src/orders/createOrder.ts".to_string(),
            ]
        );
    }

    #[test]
    fn delta_for_changed_file_with_zoekt_delta_support() {
        let prev = base_manifest();
        let mut curr = base_manifest();
        curr.commit_hash = "commit-b".to_string();
        curr.files = vec![
            IndexManifestFile {
                path: "src/orders/createOrder.ts".to_string(),
                content_hash: "hash-orders-v2".to_string(),
                language: Some("typescript".to_string()),
                project_root: Some(".".to_string()),
                generated: None,
                vendor: None,
                test: None,
                artifacts: None,
            },
            IndexManifestFile {
                path: "docs/architecture.md".to_string(),
                content_hash: "hash-docs-v1".to_string(),
                language: Some("markdown".to_string()),
                project_root: Some(".".to_string()),
                generated: None,
                vendor: None,
                test: None,
                artifacts: None,
            },
        ];
        let plan = build_delta_reindex_plan(Some(&prev), &curr, true).unwrap();

        assert_eq!(plan.mode, Mode::Delta);
        assert_eq!(
            plan.changed_files,
            vec!["src/orders/createOrder.ts".to_string()]
        );
        assert_eq!(
            plan.unchanged_files,
            vec!["docs/architecture.md".to_string()]
        );

        assert_eq!(plan.zoekt.strategy, ZoektStrategy::DeltaFiles);
        assert_eq!(
            plan.zoekt.files,
            vec!["src/orders/createOrder.ts".to_string()]
        );

        assert_eq!(plan.scip.strategy, ScipStrategy::ProjectRoots);
        assert_eq!(plan.scip.project_roots, vec![".".to_string()]);
        assert_eq!(
            plan.scip.files,
            vec!["src/orders/createOrder.ts".to_string()]
        );

        assert_eq!(plan.graph.strategy, GraphStrategy::DeltaFiles);
        assert_eq!(
            plan.graph.files,
            vec!["src/orders/createOrder.ts".to_string()]
        );
        assert_eq!(plan.graph.deleted_files, Vec::<String>::new());

        assert_eq!(plan.semantic.strategy, SemanticStrategy::ChangedChunks);
        assert_eq!(
            plan.semantic.files,
            vec!["src/orders/createOrder.ts".to_string()]
        );
        assert_eq!(plan.semantic.reason, None);
    }

    #[test]
    fn removes_deleted_file_graph_facts_and_invalidates_scip_roots() {
        let prev = base_manifest();
        let mut curr = base_manifest();
        curr.commit_hash = "commit-b".to_string();
        curr.files = vec![IndexManifestFile {
            path: "docs/architecture.md".to_string(),
            content_hash: "hash-docs-v1".to_string(),
            language: Some("markdown".to_string()),
            project_root: Some(".".to_string()),
            generated: None,
            vendor: None,
            test: None,
            artifacts: None,
        }];
        let plan = build_delta_reindex_plan(Some(&prev), &curr, false).unwrap();
        assert_eq!(
            plan.deleted_files,
            vec!["src/orders/createOrder.ts".to_string()]
        );
        assert_eq!(plan.graph.strategy, GraphStrategy::DeltaFiles);
        assert_eq!(plan.graph.files, Vec::<String>::new());
        assert_eq!(
            plan.graph.deleted_files,
            vec!["src/orders/createOrder.ts".to_string()]
        );
        assert_eq!(plan.scip.strategy, ScipStrategy::ProjectRoots);
        assert_eq!(plan.scip.project_roots, vec![".".to_string()]);
        assert_eq!(
            plan.scip.files,
            vec!["src/orders/createOrder.ts".to_string()]
        );
        assert_eq!(plan.zoekt.strategy, ZoektStrategy::FullRepoRewrite);
        assert!(plan
            .zoekt
            .reason
            .as_deref()
            .unwrap_or("")
            .contains("rewrites repository shard"));
    }

    #[test]
    fn semantic_fingerprint_change_reruns_only_semantic_chunks() {
        let prev = base_manifest();
        let mut curr = base_manifest();
        curr.commit_hash = "commit-b".to_string();
        curr.semantic_extraction = Some(SemanticExtractionFingerprint {
            prompt_version: "graphify-v2".to_string(),
            model_id: "glm-5".to_string(),
            schema_version: 1,
        });
        let plan = build_delta_reindex_plan(Some(&prev), &curr, false).unwrap();
        assert_eq!(plan.changed_files, Vec::<String>::new());
        assert_eq!(plan.scip.strategy, ScipStrategy::Noop);
        assert_eq!(plan.graph.strategy, GraphStrategy::Noop);
        assert_eq!(plan.semantic.strategy, SemanticStrategy::AllChunks);
        assert_eq!(
            plan.semantic.files,
            vec![
                "docs/architecture.md".to_string(),
                "src/orders/createOrder.ts".to_string(),
            ]
        );
        assert_eq!(
            plan.semantic.reason.as_deref(),
            Some("Semantic extraction prompt, model, or schema changed.")
        );
    }

    #[test]
    fn blocks_artifact_sharing_across_scopes() {
        // Legacy test only asserts orgId + branch. Rust extends
        // to all 5 scope axes since `scopeKeys` in
        // indexDeltaPlan.ts:225 enumerates all 5.
        let prev = base_manifest();

        let mut curr_org = base_manifest();
        curr_org.org_id = 43;
        match build_delta_reindex_plan(Some(&prev), &curr_org, false) {
            Err(DeltaPlanError::ScopeMismatch { field }) => assert_eq!(field, "orgId"),
            other => panic!("expected ScopeMismatch(orgId), got {:?}", other),
        }

        let mut curr_ws = base_manifest();
        curr_ws.workspace_id = "atom-workspace-b".to_string();
        match build_delta_reindex_plan(Some(&prev), &curr_ws, false) {
            Err(DeltaPlanError::ScopeMismatch { field }) => assert_eq!(field, "workspaceId"),
            other => panic!("expected ScopeMismatch(workspaceId), got {:?}", other),
        }

        let mut curr_repo = base_manifest();
        curr_repo.repo_id = 99;
        match build_delta_reindex_plan(Some(&prev), &curr_repo, false) {
            Err(DeltaPlanError::ScopeMismatch { field }) => assert_eq!(field, "repoId"),
            other => panic!("expected ScopeMismatch(repoId), got {:?}", other),
        }

        let mut curr_provider = base_manifest();
        curr_provider.provider_connection_id = Some("github-installation-2".to_string());
        match build_delta_reindex_plan(Some(&prev), &curr_provider, false) {
            Err(DeltaPlanError::ScopeMismatch { field }) => {
                assert_eq!(field, "providerConnectionId")
            }
            other => panic!(
                "expected ScopeMismatch(providerConnectionId), got {:?}",
                other
            ),
        }

        let mut curr_branch = base_manifest();
        curr_branch.branch = "refs/heads/release".to_string();
        match build_delta_reindex_plan(Some(&prev), &curr_branch, false) {
            Err(DeltaPlanError::ScopeMismatch { field }) => assert_eq!(field, "branch"),
            other => panic!("expected ScopeMismatch(branch), got {:?}", other),
        }
    }

    #[test]
    fn no_previous_manifest_returns_full_plan() {
        let curr = base_manifest();
        let plan = build_delta_reindex_plan(None, &curr, false).unwrap();
        assert_eq!(plan.mode, Mode::Full);
        assert_eq!(plan.zoekt.strategy, ZoektStrategy::FullRepo);
        assert_eq!(plan.scip.strategy, ScipStrategy::FullRepo);
        assert_eq!(plan.graph.strategy, GraphStrategy::DeltaFiles);
        assert_eq!(plan.semantic.strategy, SemanticStrategy::AllChunks);
        assert_eq!(
            plan.added_files,
            vec![
                "docs/architecture.md".to_string(),
                "src/orders/createOrder.ts".to_string(),
            ]
        );
    }

    #[test]
    fn scip_toolchain_change_forces_scip_full_repo() {
        let prev = base_manifest();
        let mut curr = base_manifest();
        let mut new_toolchains = BTreeMap::new();
        new_toolchains.insert(
            "typescript:scip-typescript".to_string(),
            "sha256:ts-v2".to_string(),
        );
        curr.scip_toolchains = Some(new_toolchains);
        let plan = build_delta_reindex_plan(Some(&prev), &curr, false).unwrap();
        assert_eq!(plan.scip.strategy, ScipStrategy::FullRepo);
        assert_eq!(plan.scip.project_roots, vec![".".to_string()]);
        assert_eq!(
            plan.scip.reason.as_deref(),
            Some("SCIP toolchain fingerprint changed.")
        );
    }

    #[test]
    fn normalize_manifest_path_strips_backslash_and_leading_slash() {
        // Legacy regex: replace(/\\/g,"/").replace(/^\/+/,"") —
        // does NOT collapse interior double slashes; that's a
        // shared parity-preserving quirk.
        assert_eq!(normalize_manifest_path("/src/app.ts"), "src/app.ts");
        assert_eq!(normalize_manifest_path("src\\app.ts"), "src/app.ts");
        assert_eq!(normalize_manifest_path("\\src\\app.ts"), "src/app.ts");
        assert_eq!(normalize_manifest_path("///src/app.ts"), "src/app.ts");
        assert_eq!(normalize_manifest_path("src/app.ts"), "src/app.ts");
    }

    #[test]
    fn stable_json_equal_is_key_order_invariant() {
        let a: JsonValue = serde_json::from_str(r#"{"b":2,"a":{"y":1,"x":[1,2]}}"#).unwrap();
        let b: JsonValue = serde_json::from_str(r#"{"a":{"x":[1,2],"y":1},"b":2}"#).unwrap();
        assert!(stable_json_equal(&a, &b));
    }
}
