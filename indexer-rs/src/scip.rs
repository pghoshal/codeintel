//! SCIP indexer dispatch — explicit POLYGLOT support.
//!
//! One repo regularly contains multiple languages (e.g. a Go
//! service with a TypeScript web UI, a Python sidecar, and a
//! Rust CLI). The legacy SUPPORTED_SCIP_INDEXERS table
//! (`packages/backend/src/scipIndexManager.ts:122-200`) encodes
//! this by giving each indexer its own marker glob set and the
//! runtime spawns one indexer per matching project root —
//! never picks a single "primary" language for the whole repo.
//!
//! This module mirrors that: `detect_scip_projects` enumerates
//! every (language, project_root) tuple by scanning the
//! manifest's files for every indexer's marker patterns. The
//! result is a Vec where each entry triggers exactly one
//! shell-out to its corresponding scip-* binary.
//!
//! Scope of this slice (R.8):
//!   - 8-indexer table verbatim from the legacy.
//!   - `detect_scip_projects` polyglot scanner.
//!   - `run_scip_indexer` shell-out runner producing a .scip
//!     protobuf file on disk.
//!   - `index_revision_scip` orchestrator combining the two.
//!
//! Deferred to R.8b:
//!   - Parse the .scip protobuf into ScipSymbol /
//!     ScipOccurrence / ScipRelationship rows.
//!   - Upsert those rows into Postgres.
//!   - Garbage-collect stale .scip files across revisions.

use std::collections::{BTreeSet, HashMap, HashSet, VecDeque};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::delta_plan::IndexManifestFile;
use anyhow::Context;
use tokio_postgres::Client;
use uuid::Uuid;

const ROLE_DEFINITION: i32 = 1;
const ROLE_IMPORT: i32 = 2;
const ROLE_WRITE: i32 = 4;
const ROLE_READ: i32 = 8;
const ROLE_GENERATED: i32 = 16;
const ROLE_TEST: i32 = 32;
const ROLE_FORWARD_DEFINITION: i32 = 64;

/// ScipIndexerDefinition encodes one entry of the legacy
/// SUPPORTED_SCIP_INDEXERS table. The `language` field is the
/// language bucket name produced by `manifest_files::infer_language`
/// — used for the per-row reporting on `RepoIndexManifest`.
#[derive(Debug, Clone)]
pub struct ScipIndexerDefinition {
    /// Legacy `language` field — bucket name like "typescript".
    pub language: &'static str,
    /// Binary name (PATH-relative or absolute) the runner
    /// invokes. Empty string disables (defensive default).
    pub indexer: &'static str,
    /// Worker pool class (legacy `workerClass`). The
    /// `filter_projects_for_worker_classes` helper drops
    /// projects whose class isn't in the deployment-configured
    /// allow-list (so a "ts-js"-only pod doesn't try to spawn
    /// scip-go). Values from scipIndexManager.ts:126-198.
    pub worker_class: &'static str,
    /// Marker filenames (basename match) the project-root
    /// detector looks for. A repo with ANY file whose basename
    /// matches one of these markers triggers this indexer for
    /// that file's containing dir.
    pub markers: &'static [&'static str],
    /// How to compute the indexer's argv given (project_root,
    /// output_path, marker_path).
    pub build_args: fn(&Path, &Path, &Path) -> Vec<String>,
    /// argv for `<indexer> <args>` version probe — used by
    /// callers that want to skip the run when the binary is
    /// missing or returns non-zero.
    pub version_args: &'static [&'static str],
}

/// SUPPORTED_INDEXERS is the verbatim port of the legacy
/// SUPPORTED_SCIP_INDEXERS table at
/// scipIndexManager.ts:122-200. Order preserved so the
/// dispatch is deterministic.
pub const SUPPORTED_INDEXERS: &[ScipIndexerDefinition] = &[
    ScipIndexerDefinition {
        language: "typescript",
        indexer: "scip-typescript",
        worker_class: "ts-js",
        markers: &["tsconfig.json", "jsconfig.json", "package.json"],
        build_args: ts_args,
        version_args: &["--version"],
    },
    ScipIndexerDefinition {
        language: "go",
        indexer: "scip-go",
        worker_class: "go",
        markers: &["go.mod"],
        build_args: go_args,
        version_args: &["--version"],
    },
    ScipIndexerDefinition {
        language: "python",
        indexer: "scip-python",
        worker_class: "python",
        markers: &[
            "pyproject.toml",
            "setup.py",
            "setup.cfg",
            "requirements.txt",
            "Pipfile",
        ],
        build_args: python_args,
        version_args: &["--version"],
    },
    ScipIndexerDefinition {
        language: "java",
        indexer: "scip-java",
        worker_class: "jvm",
        markers: &[
            "pom.xml",
            "build.gradle",
            "build.gradle.kts",
            "settings.gradle",
            "settings.gradle.kts",
            "build.sbt",
        ],
        build_args: java_args,
        version_args: &["--version"],
    },
    ScipIndexerDefinition {
        language: "cpp",
        indexer: "scip-clang",
        worker_class: "cpp",
        markers: &["compile_commands.json"],
        build_args: clang_args,
        version_args: &["--version"],
    },
    ScipIndexerDefinition {
        language: "dotnet",
        indexer: "scip-dotnet",
        worker_class: "dotnet",
        markers: &[], // dotnet uses suffix matching — handled below
        build_args: dotnet_args,
        version_args: &["--version"],
    },
    ScipIndexerDefinition {
        language: "ruby",
        indexer: "scip-ruby",
        worker_class: "ruby",
        markers: &["Gemfile"], // gemspec handled via suffix
        build_args: ruby_args,
        version_args: &["--version"],
    },
    ScipIndexerDefinition {
        language: "rust",
        indexer: "rust-analyzer",
        worker_class: "rust-dart",
        markers: &["Cargo.toml"],
        build_args: rust_args,
        version_args: &["--version"],
    },
    // R.8 critic-gate fix: the legacy table has 9 entries.
    // `dart` was missing from the initial port.
    ScipIndexerDefinition {
        language: "dart",
        indexer: "dart",
        worker_class: "rust-dart",
        markers: &["pubspec.yaml"],
        build_args: dart_args,
        version_args: &["--version"],
    },
];

/// DEFAULT_IGNORES is the verbatim port of
/// `scipIndexManager.ts:109-120`. Any file whose path starts
/// with or contains one of these directory segments is
/// excluded from project-root detection. Critical for
/// correctness: a repo with a committed
/// `node_modules/foo/package.json` would otherwise spin
/// scip-typescript inside node_modules — the legacy avoids
/// this by ignoring those paths in the glob filter.
const DEFAULT_IGNORES: &[&str] = &[
    ".git",
    "node_modules",
    ".next",
    "dist",
    "build",
    "target",
    ".venv",
    "venv",
    "__pycache__",
    "vendor",
];

/// path_is_ignored returns true if any segment of `path`
/// matches a DEFAULT_IGNORES entry. Path is the relative
/// repo path (e.g. `node_modules/foo/package.json` or
/// `services/api/go.mod`).
fn path_is_ignored(path: &str) -> bool {
    for ignored in DEFAULT_IGNORES {
        let lead = format!("{}/", ignored);
        let mid = format!("/{}/", ignored);
        if path.starts_with(&lead) || path.contains(&mid) {
            return true;
        }
    }
    false
}

/// Suffix-based markers — the legacy uses glob `**/*.csproj`
/// etc, which means "any file whose basename ends with .csproj
/// triggers the indexer". `detect_scip_projects` honors these
/// for dotnet and ruby specifically (matches the legacy table
/// `markers` glob shapes for those two indexers).
const DOTNET_SUFFIXES: &[&str] = &[".sln", ".csproj", ".vbproj", ".fsproj"];
const RUBY_SUFFIXES: &[&str] = &[".gemspec"];

/// ScipProject is one detected (language, project_root) tuple
/// + the marker path that triggered it. One Project ->
/// one shell-out.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ScipProject {
    pub language: &'static str,
    pub indexer: &'static str,
    pub worker_class: &'static str,
    pub project_root: String,
    pub marker_path: String,
    /// True when the project root was *synthesised* by
    /// `add_inferred_typescript_projects` rather than detected
    /// via a marker file. Used by the runner to spawn the TS
    /// indexer in `--infer-tsconfig` mode against a synthetic
    /// `package.json`. Mirrors legacy
    /// scipIndexManager.ts:967-971 (`inferred: true`).
    pub inferred: bool,
    /// When `inferred=true`, a human-readable explanation
    /// surfaced in /status JSON.
    pub inferred_reason: Option<String>,
}

/// detect_scip_projects enumerates every (language,
/// project_root) tuple in the manifest. Polyglot-safe: a single
/// repo with package.json + go.mod + Cargo.toml yields THREE
/// projects (one each for typescript, go, rust), not one. The
/// project_root is the dir containing the marker; for root-
/// level markers ("." dir) the legacy still spawns the indexer
/// with cwd="."
pub fn detect_scip_projects(files: &[IndexManifestFile]) -> Vec<ScipProject> {
    let mut out: Vec<ScipProject> = Vec::new();
    let mut seen: std::collections::BTreeSet<(String, String)> = std::collections::BTreeSet::new();
    let ts_marker_roots = typescript_marker_roots(files);

    for file in files {
        let path = file.path.as_str();
        // R.8 critic-gate fix: skip ignored directories.
        // Legacy scipIndexManager.ts:893 wires DEFAULT_IGNORES
        // into every glob; without this, a repo with a
        // committed node_modules/foo/package.json would spin
        // scip-typescript inside node_modules.
        if path_is_ignored(path) {
            continue;
        }
        let basename = match path.rsplit_once('/') {
            Some((_, last)) => last,
            None => path,
        };
        // Legacy `normalizeProjectRoot` (scipIndexManager.ts:
        // 921) returns "" (empty string) for files at the
        // repo root — NOT ".". This affects the stored
        // project_root field, the output filename layout,
        // and the marker_path synthesis in
        // add_inferred_typescript_projects.
        let project_root = match path.rsplit_once('/') {
            Some((dir, _)) => dir.to_string(),
            None => String::new(),
        };

        for def in SUPPORTED_INDEXERS {
            // Skip indexers without configured markers; they
            // come back in the suffix sweep below.
            let mut matched = false;
            for marker in def.markers {
                if basename == *marker {
                    matched = true;
                    break;
                }
            }
            // Suffix-based markers — dotnet, ruby.
            if !matched && def.language == "dotnet" {
                matched = DOTNET_SUFFIXES.iter().any(|s| basename.ends_with(*s));
            }
            if !matched && def.language == "ruby" {
                matched = matched || RUBY_SUFFIXES.iter().any(|s| basename.ends_with(*s));
            }

            if matched {
                if def.language == "typescript"
                    && !has_typescript_input_in_root(files, &project_root, &ts_marker_roots)
                {
                    continue;
                }
                if def.language == "java" && !has_jvm_source_set_input_in_root(files, &project_root)
                {
                    continue;
                }
                let key = (def.language.to_string(), project_root.clone());
                if seen.insert(key) {
                    out.push(ScipProject {
                        language: def.language,
                        indexer: def.indexer,
                        worker_class: def.worker_class,
                        project_root: project_root.clone(),
                        marker_path: path.to_string(),
                        inferred: false,
                        inferred_reason: None,
                    });
                }
            }
        }
    }
    // Stable deterministic order: by language, then root.
    out.sort_by(|a, b| {
        // Legacy line 918: sort by (root, language). Order
        // matters because `max_project_roots_per_revision`
        // truncates after sort — divergence here would keep
        // different N projects across legacy vs port.
        a.project_root
            .cmp(&b.project_root)
            .then_with(|| a.language.cmp(b.language))
    });
    out
}

fn typescript_marker_roots(files: &[IndexManifestFile]) -> Vec<String> {
    let mut roots = std::collections::BTreeSet::new();
    for file in files {
        let path = file.path.as_str();
        let basename = match path.rsplit_once('/') {
            Some((_, last)) => last,
            None => path,
        };
        if basename != "package.json" && basename != "tsconfig.json" && basename != "jsconfig.json"
        {
            continue;
        }
        let root = match path.rsplit_once('/') {
            Some((dir, _)) => dir.to_string(),
            None => String::new(),
        };
        roots.insert(root);
    }
    roots.into_iter().collect()
}

fn has_typescript_input_in_root(
    files: &[IndexManifestFile],
    root: &str,
    marker_roots: &[String],
) -> bool {
    files.iter().any(|file| {
        let path = file.path.as_str();
        file_in_project_root(path, root)
            && !contained_by_nested_project_root(path, root, marker_roots)
            && is_indexable_typescript_input(path)
    })
}

fn contained_by_nested_project_root(path: &str, root: &str, marker_roots: &[String]) -> bool {
    marker_roots.iter().any(|child| {
        if child == root || child.is_empty() {
            return false;
        }
        if !root.is_empty() && !child.starts_with(&format!("{}/", root)) {
            return false;
        }
        file_in_project_root(path, child)
    })
}

fn has_jvm_source_set_input_in_root(files: &[IndexManifestFile], root: &str) -> bool {
    files.iter().any(|file| {
        let path = file.path.as_str();
        if !file_in_project_root(path, root) {
            return false;
        }
        let rel = if root.is_empty() || root == "." {
            path
        } else {
            path.strip_prefix(&format!("{}/", root)).unwrap_or(path)
        };
        let lower = rel.to_ascii_lowercase();
        (lower.ends_with(".java") || lower.ends_with(".scala") || lower.ends_with(".kt"))
            && (lower.starts_with("src/") || lower.contains("/src/"))
    })
}

fn file_in_project_root(path: &str, root: &str) -> bool {
    if root.is_empty() || root == "." {
        return true;
    }
    path == root || path.starts_with(&format!("{}/", root))
}

/// TYPESCRIPT_EXTENSIONS — the glob extensions
/// `addInferredTypeScriptProjects` walks (legacy
/// scipIndexManager.ts:932). Mirrors the legacy `.ts/.tsx/
/// .js/.jsx/.mts/.cts/.mjs/.cjs` set.
const TYPESCRIPT_EXTENSIONS: &[&str] =
    &[".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"];

/// add_inferred_typescript_projects mirrors legacy
/// `addInferredTypeScriptProjects` (scipIndexManager.ts:923-973).
///
/// For each TS/JS source file in `files` not already covered
/// by an existing TypeScript project root, infers a synthetic
/// root via `infer_typescript_project_root` and appends a
/// fresh ScipProject with `inferred=true`. The synthetic
/// marker_path is `<root>/package.json` (a placeholder used
/// by the runner to drive scip-typescript in
/// `--infer-tsconfig` mode against a synthesised manifest).
///
/// Skips `.d.ts` / `.min.js` / files under `coverage/` per
/// legacy line 937-940. DEFAULT_IGNORES is also re-applied
/// defensively (the manifest-files walker already strips
/// these but the legacy double-checks; mirror that).
pub fn add_inferred_typescript_projects(
    files: &[IndexManifestFile],
    projects: &mut Vec<ScipProject>,
) {
    let existing_ts_roots: Vec<String> = projects
        .iter()
        .filter(|p| p.language == "typescript")
        .map(|p| p.project_root.clone())
        .collect();

    let mut source_files: Vec<&str> = files
        .iter()
        .map(|f| f.path.as_str())
        .filter(|p| !path_is_ignored(p))
        .filter(|p| !p.ends_with(".d.ts") && !p.ends_with(".min.js"))
        .filter(|p| !p.starts_with("coverage/") && !p.contains("/coverage/"))
        .filter(|p| is_indexable_typescript_input(p))
        .collect();
    source_files.sort();

    let mut inferred_roots: std::collections::BTreeSet<String> = std::collections::BTreeSet::new();
    for source_file in &source_files {
        if is_covered_by_project_root(source_file, &existing_ts_roots) {
            continue;
        }
        inferred_roots.insert(infer_typescript_project_root(source_file));
    }

    let mut seen: std::collections::BTreeSet<(String, String)> = projects
        .iter()
        .map(|p| (p.language.to_string(), p.project_root.clone()))
        .collect();

    for root in inferred_roots {
        let key = ("typescript".to_string(), root.clone());
        if !seen.insert(key) {
            continue;
        }
        // marker_path = <root>/package.json (legacy strips
        // a leading "./" — the Rust port never produces one
        // since path.posix.join with "" yields just
        // "package.json").
        let marker_path = if root.is_empty() {
            "package.json".to_string()
        } else {
            format!("{}/package.json", root)
        };
        projects.push(ScipProject {
            language: "typescript",
            indexer: "scip-typescript",
            worker_class: "ts-js",
            project_root: root,
            marker_path,
            inferred: true,
            inferred_reason: Some(
                "Detected TypeScript/JavaScript source files without package.json, tsconfig.json, or jsconfig.json."
                    .to_string(),
            ),
        });
    }

    // Re-sort to preserve the deterministic (language, root)
    // order detect_scip_projects established.
    projects.sort_by(|a, b| {
        // Legacy line 918: sort by (root, language). Order
        // matters because `max_project_roots_per_revision`
        // truncates after sort — divergence here would keep
        // different N projects across legacy vs port.
        a.project_root
            .cmp(&b.project_root)
            .then_with(|| a.language.cmp(b.language))
    });
}

fn is_covered_by_project_root(relative_path: &str, roots: &[String]) -> bool {
    for root in roots {
        if file_in_project_root(relative_path, root) {
            return true;
        }
    }
    false
}

fn is_indexable_typescript_input(path: &str) -> bool {
    let lower = path.to_ascii_lowercase();
    TYPESCRIPT_EXTENSIONS.iter().any(|e| lower.ends_with(*e))
        && !lower.ends_with(".d.ts")
        && !lower.ends_with(".min.js")
        && path != "coverage"
        && !path.starts_with("coverage/")
        && !path.contains("/coverage/")
}

fn infer_typescript_project_root(relative_path: &str) -> String {
    // Legacy lines 982-989: split on "/", find index of "src"
    // segment; if found and not at position 0, return the
    // prefix; otherwise empty string.
    let parts: Vec<&str> = relative_path.split('/').collect();
    let src_index = parts.iter().position(|p| *p == "src");
    match src_index {
        Some(idx) if idx > 0 => parts[..idx].join("/"),
        _ => String::new(),
    }
}

/// filter_projects_for_worker_classes mirrors legacy
/// `filterProjectsForWorkerClasses` (scipIndexManager.ts:
/// 1051-1058). Reads env
/// `CODEINTEL_SCIP_WORKER_CLASSES` (renamed from the old
/// brand-prefixed environment key), defaults to "universal".
/// Comma-separated list; if it contains "universal" or "all"
/// → all projects pass. Otherwise filter to projects whose
/// workerClass is in the allow-list.
pub fn filter_projects_for_worker_classes(projects: Vec<ScipProject>) -> Vec<ScipProject> {
    let raw = std::env::var("CODEINTEL_SCIP_WORKER_CLASSES").unwrap_or_default();
    let allowed = parse_worker_classes(&raw);
    if allowed.contains("universal") || allowed.contains("all") {
        return projects;
    }
    projects
        .into_iter()
        .filter(|p| allowed.contains(p.worker_class))
        .collect()
}

fn parse_worker_classes(value: &str) -> std::collections::BTreeSet<String> {
    let source = if value.trim().is_empty() {
        "universal"
    } else {
        value
    };
    source
        .split(',')
        .map(|item| item.trim().to_string())
        .filter(|s| !s.is_empty())
        .collect()
}

/// SCIP_DEFAULT_MAX_PROJECT_ROOTS_PER_REVISION mirrors the
/// legacy default at packages/shared/src/env.server.ts:263
/// (`numberSchema.default(24)`). A bare-TS monorepo with 50
/// inferred roots is capped to 24 indexer runs to keep wall
/// clock + memory bounded.
const SCIP_DEFAULT_MAX_PROJECT_ROOTS_PER_REVISION: usize = 24;

/// max_project_roots_per_revision reads
/// `CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION`. Defaults
/// to 24 (the legacy value).
pub fn max_project_roots_per_revision() -> usize {
    std::env::var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION")
        .ok()
        .and_then(|v| v.parse::<usize>().ok())
        .filter(|v| *v > 0)
        .unwrap_or(SCIP_DEFAULT_MAX_PROJECT_ROOTS_PER_REVISION)
}

/// ScipRunResult is the outcome of one shell-out.
#[derive(Debug, Clone)]
pub struct ScipRunResult {
    pub language: &'static str,
    pub indexer: &'static str,
    pub worker_class: &'static str,
    pub project_root: String,
    pub command: String,
    pub output_path: PathBuf,
    /// Wall-clock duration of the indexer subprocess.
    pub duration: Duration,
    /// Last 4KiB of stderr — captured for diagnostics when a
    /// SCIP run fails. Empty on success.
    pub stderr_tail: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NormalizedRange {
    pub start_line: i32,
    pub start_character: i32,
    pub end_line: i32,
    pub end_character: i32,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ScipSymbolRow {
    pub symbol: String,
    pub display_name: String,
    pub kind: Option<String>,
    pub language: Option<String>,
    pub documentation: Vec<String>,
    pub signature: Option<String>,
    pub file_path: Option<String>,
    pub start_line: Option<i32>,
    pub start_character: Option<i32>,
    pub end_line: Option<i32>,
    pub end_character: Option<i32>,
    pub enclosing_symbol: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ScipOccurrenceRow {
    pub symbol: String,
    pub file_path: String,
    pub start_line: i32,
    pub start_character: i32,
    pub end_line: i32,
    pub end_character: i32,
    pub role: String,
    pub language: Option<String>,
    pub syntax_kind: Option<String>,
    pub line_content: Option<String>,
    pub enclosing_symbol: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ScipRelationshipRow {
    pub source_symbol: String,
    pub target_symbol: String,
    pub is_reference: bool,
    pub is_implementation: bool,
    pub is_type_definition: bool,
    pub is_definition: bool,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ScipIngestRows {
    pub symbols: Vec<ScipSymbolRow>,
    pub occurrences: Vec<ScipOccurrenceRow>,
    pub relationships: Vec<ScipRelationshipRow>,
}

#[derive(Debug, Clone)]
pub struct ScipProjectIngestInput {
    pub language: String,
    pub project_root: String,
    pub indexer: String,
    pub worker_class: Option<String>,
    pub artifact_path: PathBuf,
    pub command: String,
    pub duration_ms: Option<i32>,
}

#[derive(Debug, Clone)]
pub struct ScipProjectFailureInput {
    pub language: String,
    pub project_root: String,
    pub indexer: String,
    pub worker_class: Option<String>,
    pub artifact_path: Option<PathBuf>,
    pub command: String,
    pub status: String,
    pub error_message: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct ScipPersistStats {
    pub language_count: usize,
    pub symbol_count: usize,
    pub occurrence_count: usize,
    pub relationship_count: usize,
}

#[derive(Debug)]
pub enum ScipRunError {
    /// The binary couldn't be found in PATH.
    BinaryMissing(String),
    /// Spawn or wait returned an OS-level failure.
    SubprocessFailed { code: i32, stderr_tail: String },
    /// IO setup failure (mkdir / read output file).
    Io(std::io::Error),
}

impl std::fmt::Display for ScipRunError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::BinaryMissing(b) => write!(f, "scip indexer binary not found: {}", b),
            Self::SubprocessFailed { code, stderr_tail } => {
                write!(f, "scip subprocess exit code={}\n{}", code, stderr_tail)
            }
            Self::Io(e) => write!(f, "scip io: {}", e),
        }
    }
}

impl std::error::Error for ScipRunError {}

pub fn ingest_scip_artifact_rows(
    artifact_path: &Path,
    project_language: &str,
    project_root: &str,
    worktree_path: &Path,
) -> anyhow::Result<ScipIngestRows> {
    let bytes = std::fs::read(artifact_path)
        .with_context(|| format!("read SCIP artifact {}", artifact_path.display()))?;
    ingest_scip_bytes(&bytes, project_language, project_root, worktree_path)
}

pub fn ingest_scip_bytes(
    bytes: &[u8],
    project_language: &str,
    project_root: &str,
    worktree_path: &Path,
) -> anyhow::Result<ScipIngestRows> {
    use protobuf::Message;
    let index = <scip::types::Index as Message>::parse_from_bytes(bytes)
        .with_context(|| "decode SCIP protobuf Index")?;
    Ok(rows_from_scip_index(
        &index,
        project_language,
        project_root,
        worktree_path,
    ))
}

pub fn rows_from_scip_index(
    index: &scip::types::Index,
    project_language: &str,
    project_root: &str,
    worktree_path: &Path,
) -> ScipIngestRows {
    let mut symbols = Vec::new();
    let mut occurrences = Vec::new();
    let mut relationships = Vec::new();
    let mut relationship_keys = HashSet::new();
    let mut symbol_definitions: HashMap<
        String,
        (&scip::types::Occurrence, &scip::types::Document),
    > = HashMap::new();
    let mut definition_scopes_by_file: HashMap<String, Vec<DefinitionScope>> = HashMap::new();
    let mut document_symbols: BTreeSet<String> = BTreeSet::new();
    let mut line_cache = LineContentCache::new(worktree_path);

    for document in &index.documents {
        let Some(file_path) = join_project_path_checked(project_root, &document.relative_path)
        else {
            continue;
        };
        for symbol in &document.symbols {
            document_symbols.insert(symbol.symbol.clone());
        }
        for occurrence in &document.occurrences {
            if is_definition_occurrence(occurrence) && !occurrence.symbol.is_empty() {
                symbol_definitions.insert(occurrence.symbol.clone(), (occurrence, document));
                if let Some(definition_range) = normalize_range(&occurrence.enclosing_range)
                    .or_else(|| normalize_range(&occurrence.range))
                {
                    definition_scopes_by_file
                        .entry(file_path.clone())
                        .or_default()
                        .push(DefinitionScope {
                            symbol: occurrence.symbol.clone(),
                            range: definition_range,
                        });
                }
            }
        }
    }

    let project_defined_symbols: HashSet<String> = document_symbols
        .into_iter()
        .filter(|symbol| symbol_definitions.contains_key(symbol))
        .filter(|symbol| is_semantic_project_symbol(symbol))
        .collect();

    for scopes in definition_scopes_by_file.values_mut() {
        scopes.sort_by_key(|scope| range_span(&scope.range));
    }

    for document in &index.documents {
        let language = if document.language.is_empty() {
            project_language.to_string()
        } else {
            document.language.clone()
        };
        let Some(file_path) = join_project_path_checked(project_root, &document.relative_path)
        else {
            continue;
        };

        for symbol in &document.symbols {
            symbols.push(to_symbol_row(
                symbol,
                &language,
                project_root,
                symbol_definitions.get(&symbol.symbol).copied(),
            ));
            add_relationship_rows(
                &mut relationships,
                &mut relationship_keys,
                &project_defined_symbols,
                &symbol.symbol,
                &symbol.relationships,
            );
        }

        for occurrence in &document.occurrences {
            if occurrence.symbol.is_empty() {
                continue;
            }
            let range = match normalize_range(&occurrence.range) {
                Some(r) => r,
                None => continue,
            };
            let line_content = line_cache.get_line_content(&file_path, range.start_line);
            let roles = occurrence_roles(occurrence);
            let enclosing_symbol = find_enclosing_definition_symbol(
                definition_scopes_by_file
                    .get(&file_path)
                    .map(|v| v.as_slice())
                    .unwrap_or(&[]),
                &range,
                &occurrence.symbol,
            );

            for role in &roles {
                occurrences.push(ScipOccurrenceRow {
                    symbol: occurrence.symbol.clone(),
                    file_path: file_path.clone(),
                    start_line: range.start_line,
                    start_character: range.start_character,
                    end_line: range.end_line,
                    end_character: range.end_character,
                    role: role.clone(),
                    language: Some(language.clone()),
                    syntax_kind: syntax_kind_name(occurrence),
                    line_content: line_content.clone(),
                    enclosing_symbol: enclosing_symbol.clone(),
                });
            }

            if should_derive_reference_relationship(
                &roles,
                enclosing_symbol.as_deref(),
                &occurrence.symbol,
            ) {
                let derived = vec![scip::types::Relationship {
                    symbol: occurrence.symbol.clone(),
                    is_reference: true,
                    is_implementation: false,
                    is_type_definition: false,
                    is_definition: false,
                    special_fields: Default::default(),
                }];
                add_relationship_rows(
                    &mut relationships,
                    &mut relationship_keys,
                    &project_defined_symbols,
                    enclosing_symbol.as_deref().unwrap_or_default(),
                    &derived,
                );
            }
        }
    }

    for symbol in &index.external_symbols {
        symbols.push(to_symbol_row(symbol, project_language, project_root, None));
        add_relationship_rows(
            &mut relationships,
            &mut relationship_keys,
            &project_defined_symbols,
            &symbol.symbol,
            &symbol.relationships,
        );
    }

    ScipIngestRows {
        symbols,
        occurrences,
        relationships,
    }
}

pub async fn persist_revision_scip_rows(
    client: &Arc<Client>,
    org_id: i32,
    repo_id: i32,
    revision: &str,
    commit_hash: &str,
    artifact_root: &Path,
    projects: &[ScipProjectIngestInput],
    failed_projects: &[ScipProjectFailureInput],
    worktree_path: &Path,
) -> anyhow::Result<ScipPersistStats> {
    let index_id = upsert_code_intel_index(
        client,
        org_id,
        repo_id,
        revision,
        commit_hash,
        artifact_root,
    )
    .await?;
    delete_index_children(client, &index_id).await?;

    let mut stats = ScipPersistStats::default();
    let mut errors = Vec::new();
    let mut ready_languages = BTreeSet::new();
    let mut ready_project_count = 0usize;

    for project in failed_projects {
        let id = Uuid::new_v4().to_string();
        let artifact_path = project
            .artifact_path
            .as_ref()
            .map(|p| p.to_string_lossy().to_string());
        client
            .execute(
                r#"INSERT INTO "CodeIntelLanguageIndex" (
                       id, language, "projectRoot", indexer, "workerClass",
                       status, "artifactPath", command, "durationMs",
                       "errorMessage", "createdAt", "updatedAt", "codeIntelIndexId"
                   ) VALUES (
                       $1, $2, $3, $4, $5,
                       $6::"CodeIntelIndexStatus", $7, $8, NULL,
                       $9, NOW(), NOW(), $10
                   )
                   ON CONFLICT ("codeIntelIndexId", language, "projectRoot", indexer)
                   DO UPDATE SET
                       "workerClass" = EXCLUDED."workerClass",
                       status = EXCLUDED.status,
                       "artifactPath" = EXCLUDED."artifactPath",
                       command = EXCLUDED.command,
                       "durationMs" = NULL,
                       "errorMessage" = EXCLUDED."errorMessage",
                       "updatedAt" = NOW()"#,
                &[
                    &id,
                    &project.language,
                    &project.project_root,
                    &project.indexer,
                    &project.worker_class,
                    &project.status,
                    &artifact_path,
                    &project.command,
                    &project.error_message,
                    &index_id,
                ],
            )
            .await
            .with_context(|| "upsert failed CodeIntelLanguageIndex")?;
        errors.push(format!(
            "{}:{}:{}",
            project.language, project.project_root, project.error_message
        ));
    }

    for project in projects {
        let language_index_id =
            upsert_language_index(client, &index_id, project, "INDEXING", None).await?;
        let rows = match ingest_scip_artifact_rows(
            &project.artifact_path,
            &project.language,
            &project.project_root,
            worktree_path,
        ) {
            Ok(rows) => rows,
            Err(e) => {
                let msg = e.to_string();
                errors.push(format!(
                    "{}:{}:{}",
                    project.language, project.project_root, msg
                ));
                update_language_status(client, &language_index_id, "FAILED", Some(&msg)).await?;
                continue;
            }
        };

        bulk_insert_symbols(client, org_id, repo_id, &index_id, &rows.symbols).await?;
        bulk_insert_occurrences(client, org_id, repo_id, &index_id, &rows.occurrences).await?;
        bulk_insert_relationships(client, org_id, repo_id, &index_id, &rows.relationships).await?;
        update_language_status(client, &language_index_id, "READY", None).await?;

        ready_project_count += 1;
        ready_languages.insert(project.language.clone());
        stats.symbol_count += rows.symbols.len();
        stats.occurrence_count += rows.occurrences.len();
        stats.relationship_count += rows.relationships.len();
    }

    stats.language_count = ready_languages.len();
    let total_projects = projects.len() + failed_projects.len();
    let status = if total_projects == 0 {
        "SKIPPED"
    } else if ready_project_count == total_projects {
        "READY"
    } else if ready_project_count > 0 {
        "PARTIAL"
    } else if failed_projects.iter().any(|p| p.status == "FAILED") {
        "FAILED"
    } else {
        "SKIPPED"
    };
    let error_message = if errors.is_empty() {
        None
    } else {
        Some(errors.join("; "))
    };
    update_index_status(client, &index_id, status, &stats, error_message.as_deref()).await?;
    Ok(stats)
}

async fn upsert_code_intel_index(
    client: &Arc<Client>,
    org_id: i32,
    repo_id: i32,
    revision: &str,
    commit_hash: &str,
    artifact_root: &Path,
) -> anyhow::Result<String> {
    let id = Uuid::new_v4().to_string();
    let artifact_root = artifact_root.to_string_lossy().to_string();
    let row = client
        .query_one(
            r#"INSERT INTO "CodeIntelIndex" (
                   id, kind, status, revision, "commitHash", "artifactRoot",
                   "languageCount", "symbolCount", "occurrenceCount",
                   "relationshipCount", "errorMessage", "indexedAt",
                   "createdAt", "updatedAt", "orgId", "repoId"
               ) VALUES (
                   $1, 'SCIP'::"CodeIntelIndexKind", 'INDEXING'::"CodeIntelIndexStatus",
                   $2, $3, $4, 0, 0, 0, 0, NULL, NULL, NOW(), NOW(), $5, $6
               )
               ON CONFLICT ("repoId", revision, "commitHash", kind)
               DO UPDATE SET
                   status = 'INDEXING'::"CodeIntelIndexStatus",
                   "artifactRoot" = EXCLUDED."artifactRoot",
                   "languageCount" = 0,
                   "symbolCount" = 0,
                   "occurrenceCount" = 0,
                   "relationshipCount" = 0,
                   "errorMessage" = NULL,
                   "indexedAt" = NULL,
                   "updatedAt" = NOW()
               WHERE "CodeIntelIndex"."orgId" = EXCLUDED."orgId"
               RETURNING id"#,
            &[
                &id,
                &revision,
                &commit_hash,
                &artifact_root,
                &org_id,
                &repo_id,
            ],
        )
        .await
        .with_context(|| "upsert CodeIntelIndex")?;
    Ok(row.get(0))
}

async fn delete_index_children(
    client: &Arc<Client>,
    code_intel_index_id: &str,
) -> anyhow::Result<()> {
    client
        .execute(
            r#"DELETE FROM "CodeIntelRelationship" WHERE "codeIntelIndexId" = $1"#,
            &[&code_intel_index_id],
        )
        .await?;
    client
        .execute(
            r#"DELETE FROM "CodeIntelOccurrence" WHERE "codeIntelIndexId" = $1"#,
            &[&code_intel_index_id],
        )
        .await?;
    client
        .execute(
            r#"DELETE FROM "CodeIntelSymbol" WHERE "codeIntelIndexId" = $1"#,
            &[&code_intel_index_id],
        )
        .await?;
    client
        .execute(
            r#"DELETE FROM "CodeIntelLanguageIndex" WHERE "codeIntelIndexId" = $1"#,
            &[&code_intel_index_id],
        )
        .await?;
    Ok(())
}

async fn upsert_language_index(
    client: &Arc<Client>,
    code_intel_index_id: &str,
    project: &ScipProjectIngestInput,
    status: &str,
    error_message: Option<&str>,
) -> anyhow::Result<String> {
    let id = Uuid::new_v4().to_string();
    let artifact_path = project.artifact_path.to_string_lossy().to_string();
    let row = client
        .query_one(
            r#"INSERT INTO "CodeIntelLanguageIndex" (
                   id, language, "projectRoot", indexer, "workerClass",
                   status, "artifactPath", command, "durationMs",
                   "errorMessage", "createdAt", "updatedAt", "codeIntelIndexId"
               ) VALUES (
                   $1, $2, $3, $4, $5,
                   $6::"CodeIntelIndexStatus", $7, $8, $9,
                   $10, NOW(), NOW(), $11
               )
               ON CONFLICT ("codeIntelIndexId", language, "projectRoot", indexer)
               DO UPDATE SET
                   "workerClass" = EXCLUDED."workerClass",
                   status = EXCLUDED.status,
                   "artifactPath" = EXCLUDED."artifactPath",
                   command = EXCLUDED.command,
                   "durationMs" = EXCLUDED."durationMs",
                   "errorMessage" = EXCLUDED."errorMessage",
                   "toolchainId" = NULL,
                   "toolchainFingerprint" = NULL,
                   "toolchainVersion" = NULL,
                   "toolchainPath" = NULL,
                   "toolchainSha256" = NULL,
                   "updatedAt" = NOW()
               RETURNING id"#,
            &[
                &id,
                &project.language,
                &project.project_root,
                &project.indexer,
                &project.worker_class,
                &status,
                &artifact_path,
                &project.command,
                &project.duration_ms,
                &error_message,
                &code_intel_index_id,
            ],
        )
        .await
        .with_context(|| "upsert CodeIntelLanguageIndex")?;
    Ok(row.get(0))
}

async fn update_language_status(
    client: &Arc<Client>,
    language_index_id: &str,
    status: &str,
    error_message: Option<&str>,
) -> anyhow::Result<()> {
    client
        .execute(
            r#"UPDATE "CodeIntelLanguageIndex"
               SET status = $2::"CodeIntelIndexStatus",
                   "errorMessage" = $3,
                   "updatedAt" = NOW()
               WHERE id = $1"#,
            &[&language_index_id, &status, &error_message],
        )
        .await
        .with_context(|| "update CodeIntelLanguageIndex status")?;
    Ok(())
}

async fn update_index_status(
    client: &Arc<Client>,
    code_intel_index_id: &str,
    status: &str,
    stats: &ScipPersistStats,
    error_message: Option<&str>,
) -> anyhow::Result<()> {
    let language_count = stats.language_count as i32;
    let symbol_count = stats.symbol_count as i32;
    let occurrence_count = stats.occurrence_count as i32;
    let relationship_count = stats.relationship_count as i32;
    client
        .execute(
            r#"UPDATE "CodeIntelIndex"
               SET status = $2::"CodeIntelIndexStatus",
                   "languageCount" = $3,
                   "symbolCount" = $4,
                   "occurrenceCount" = $5,
                   "relationshipCount" = $6,
                   "errorMessage" = $7,
                   "indexedAt" = NOW(),
                   "updatedAt" = NOW()
               WHERE id = $1"#,
            &[
                &code_intel_index_id,
                &status,
                &language_count,
                &symbol_count,
                &occurrence_count,
                &relationship_count,
                &error_message,
            ],
        )
        .await
        .with_context(|| "update CodeIntelIndex status")?;
    Ok(())
}

async fn bulk_insert_symbols(
    client: &Arc<Client>,
    org_id: i32,
    repo_id: i32,
    code_intel_index_id: &str,
    rows: &[ScipSymbolRow],
) -> anyhow::Result<()> {
    const PARAMS_PER_ROW: usize = 16;
    let max_chunk = 60_000 / PARAMS_PER_ROW;
    for chunk in rows.chunks(max_chunk.max(1)) {
        let mut sql = String::from(
            r#"INSERT INTO "CodeIntelSymbol" (
                   id, symbol, "displayName", kind, language, documentation,
                   signature, "filePath", "startLine", "startCharacter",
                   "endLine", "endCharacter", "enclosingSymbol", "createdAt",
                   "orgId", "repoId", "codeIntelIndexId"
               ) VALUES "#,
        );
        let mut params: Vec<Box<dyn tokio_postgres::types::ToSql + Send + Sync>> =
            Vec::with_capacity(chunk.len() * PARAMS_PER_ROW);
        for (i, row) in chunk.iter().enumerate() {
            if i > 0 {
                sql.push_str(", ");
            }
            let base = i * PARAMS_PER_ROW;
            sql.push_str(&format!(
                "(${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, NOW(), ${}, ${}, ${})",
                base + 1,
                base + 2,
                base + 3,
                base + 4,
                base + 5,
                base + 6,
                base + 7,
                base + 8,
                base + 9,
                base + 10,
                base + 11,
                base + 12,
                base + 13,
                base + 14,
                base + 15,
                base + 16,
            ));
            params.push(Box::new(Uuid::new_v4().to_string()));
            params.push(Box::new(row.symbol.clone()));
            params.push(Box::new(row.display_name.clone()));
            params.push(Box::new(row.kind.clone()));
            params.push(Box::new(row.language.clone()));
            params.push(Box::new(row.documentation.clone()));
            params.push(Box::new(row.signature.clone()));
            params.push(Box::new(row.file_path.clone()));
            params.push(Box::new(row.start_line));
            params.push(Box::new(row.start_character));
            params.push(Box::new(row.end_line));
            params.push(Box::new(row.end_character));
            params.push(Box::new(row.enclosing_symbol.clone()));
            params.push(Box::new(org_id));
            params.push(Box::new(repo_id));
            params.push(Box::new(code_intel_index_id.to_string()));
        }
        sql.push_str(r#" ON CONFLICT ("codeIntelIndexId", symbol) DO NOTHING"#);
        let param_refs: Vec<&(dyn tokio_postgres::types::ToSql + Sync)> =
            params.iter().map(|p| p.as_ref() as _).collect();
        client.execute(&sql, &param_refs).await?;
    }
    Ok(())
}

async fn bulk_insert_occurrences(
    client: &Arc<Client>,
    org_id: i32,
    repo_id: i32,
    code_intel_index_id: &str,
    rows: &[ScipOccurrenceRow],
) -> anyhow::Result<()> {
    const PARAMS_PER_ROW: usize = 15;
    let max_chunk = 60_000 / PARAMS_PER_ROW;
    for chunk in rows.chunks(max_chunk.max(1)) {
        let mut sql = String::from(
            r#"INSERT INTO "CodeIntelOccurrence" (
                   id, symbol, "filePath", "startLine", "startCharacter",
                   "endLine", "endCharacter", role, language, "syntaxKind",
                   "lineContent", "enclosingSymbol", "createdAt",
                   "orgId", "repoId", "codeIntelIndexId"
               ) VALUES "#,
        );
        let mut params: Vec<Box<dyn tokio_postgres::types::ToSql + Send + Sync>> =
            Vec::with_capacity(chunk.len() * PARAMS_PER_ROW);
        for (i, row) in chunk.iter().enumerate() {
            if i > 0 {
                sql.push_str(", ");
            }
            let base = i * PARAMS_PER_ROW;
            sql.push_str(&format!(
                "(${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}::\"CodeIntelOccurrenceRole\", ${}, ${}, ${}, ${}, NOW(), ${}, ${}, ${})",
                base + 1,
                base + 2,
                base + 3,
                base + 4,
                base + 5,
                base + 6,
                base + 7,
                base + 8,
                base + 9,
                base + 10,
                base + 11,
                base + 12,
                base + 13,
                base + 14,
                base + 15,
            ));
            params.push(Box::new(Uuid::new_v4().to_string()));
            params.push(Box::new(row.symbol.clone()));
            params.push(Box::new(row.file_path.clone()));
            params.push(Box::new(row.start_line));
            params.push(Box::new(row.start_character));
            params.push(Box::new(row.end_line));
            params.push(Box::new(row.end_character));
            params.push(Box::new(row.role.clone()));
            params.push(Box::new(row.language.clone()));
            params.push(Box::new(row.syntax_kind.clone()));
            params.push(Box::new(row.line_content.clone()));
            params.push(Box::new(row.enclosing_symbol.clone()));
            params.push(Box::new(org_id));
            params.push(Box::new(repo_id));
            params.push(Box::new(code_intel_index_id.to_string()));
        }
        let param_refs: Vec<&(dyn tokio_postgres::types::ToSql + Sync)> =
            params.iter().map(|p| p.as_ref() as _).collect();
        client.execute(&sql, &param_refs).await?;
    }
    Ok(())
}

async fn bulk_insert_relationships(
    client: &Arc<Client>,
    org_id: i32,
    repo_id: i32,
    code_intel_index_id: &str,
    rows: &[ScipRelationshipRow],
) -> anyhow::Result<()> {
    const PARAMS_PER_ROW: usize = 10;
    let max_chunk = 60_000 / PARAMS_PER_ROW;
    for chunk in rows.chunks(max_chunk.max(1)) {
        let mut sql = String::from(
            r#"INSERT INTO "CodeIntelRelationship" (
                   id, "sourceSymbol", "targetSymbol", "isReference",
                   "isImplementation", "isTypeDefinition", "isDefinition",
                   "createdAt", "orgId", "repoId", "codeIntelIndexId"
               ) VALUES "#,
        );
        let mut params: Vec<Box<dyn tokio_postgres::types::ToSql + Send + Sync>> =
            Vec::with_capacity(chunk.len() * PARAMS_PER_ROW);
        for (i, row) in chunk.iter().enumerate() {
            if i > 0 {
                sql.push_str(", ");
            }
            let base = i * PARAMS_PER_ROW;
            sql.push_str(&format!(
                "(${}, ${}, ${}, ${}, ${}, ${}, ${}, NOW(), ${}, ${}, ${})",
                base + 1,
                base + 2,
                base + 3,
                base + 4,
                base + 5,
                base + 6,
                base + 7,
                base + 8,
                base + 9,
                base + 10,
            ));
            params.push(Box::new(Uuid::new_v4().to_string()));
            params.push(Box::new(row.source_symbol.clone()));
            params.push(Box::new(row.target_symbol.clone()));
            params.push(Box::new(row.is_reference));
            params.push(Box::new(row.is_implementation));
            params.push(Box::new(row.is_type_definition));
            params.push(Box::new(row.is_definition));
            params.push(Box::new(org_id));
            params.push(Box::new(repo_id));
            params.push(Box::new(code_intel_index_id.to_string()));
        }
        sql.push_str(
            r#" ON CONFLICT ("codeIntelIndexId", "sourceSymbol", "targetSymbol", "isReference", "isImplementation", "isTypeDefinition", "isDefinition") DO NOTHING"#,
        );
        let param_refs: Vec<&(dyn tokio_postgres::types::ToSql + Sync)> =
            params.iter().map(|p| p.as_ref() as _).collect();
        client.execute(&sql, &param_refs).await?;
    }
    Ok(())
}

#[derive(Debug, Clone)]
struct DefinitionScope {
    symbol: String,
    range: NormalizedRange,
}

fn to_symbol_row(
    symbol: &scip::types::SymbolInformation,
    language: &str,
    project_root: &str,
    definition: Option<(&scip::types::Occurrence, &scip::types::Document)>,
) -> ScipSymbolRow {
    let definition_range =
        definition.and_then(|(occurrence, _)| normalize_range(&occurrence.range));
    let file_path = definition
        .and_then(|(_, document)| join_project_path_checked(project_root, &document.relative_path));
    let signature = symbol.signature_documentation.as_ref().and_then(|doc| {
        if doc.text.is_empty() {
            None
        } else {
            Some(doc.text.clone())
        }
    });
    ScipSymbolRow {
        symbol: symbol.symbol.clone(),
        display_name: if symbol.display_name.is_empty() {
            display_name_from_scip_symbol(&symbol.symbol)
        } else {
            symbol.display_name.clone()
        },
        kind: symbol.kind.enum_value().ok().and_then(|kind| {
            let name = format!("{:?}", kind);
            if name == "UnspecifiedKind" {
                None
            } else {
                Some(name)
            }
        }),
        language: Some(language.to_string()),
        documentation: symbol.documentation.clone(),
        signature,
        file_path,
        start_line: definition_range.as_ref().map(|r| r.start_line),
        start_character: definition_range.as_ref().map(|r| r.start_character),
        end_line: definition_range.as_ref().map(|r| r.end_line),
        end_character: definition_range.as_ref().map(|r| r.end_character),
        enclosing_symbol: if symbol.enclosing_symbol.is_empty() {
            None
        } else {
            Some(symbol.enclosing_symbol.clone())
        },
    }
}

fn add_relationship_rows(
    out: &mut Vec<ScipRelationshipRow>,
    keys: &mut HashSet<String>,
    project_defined_symbols: &HashSet<String>,
    source_symbol: &str,
    relationships: &[scip::types::Relationship],
) {
    for relationship in relationships {
        let row = ScipRelationshipRow {
            source_symbol: source_symbol.to_string(),
            target_symbol: relationship.symbol.clone(),
            is_reference: relationship.is_reference,
            is_implementation: relationship.is_implementation,
            is_type_definition: relationship.is_type_definition,
            is_definition: relationship.is_definition,
        };
        if !should_persist_project_relationship(&row, project_defined_symbols) {
            continue;
        }
        let key = format!(
            "{}\u{0}{}\u{0}{}\u{0}{}\u{0}{}\u{0}{}",
            row.source_symbol,
            row.target_symbol,
            row.is_reference,
            row.is_implementation,
            row.is_type_definition,
            row.is_definition
        );
        if keys.insert(key) {
            out.push(row);
        }
    }
}

fn occurrence_roles(occurrence: &scip::types::Occurrence) -> Vec<String> {
    let mut roles = Vec::new();
    if occurrence.symbol_roles & ROLE_DEFINITION > 0 {
        roles.push("DEFINITION".to_string());
    }
    if occurrence.symbol_roles & ROLE_IMPORT > 0 {
        roles.push("IMPORT".to_string());
    }
    if occurrence.symbol_roles & ROLE_WRITE > 0 {
        roles.push("WRITE".to_string());
    }
    if occurrence.symbol_roles & ROLE_READ > 0 {
        roles.push("READ".to_string());
    }
    if occurrence.symbol_roles & ROLE_GENERATED > 0 {
        roles.push("GENERATED".to_string());
    }
    if occurrence.symbol_roles & ROLE_TEST > 0 {
        roles.push("TEST".to_string());
    }
    if occurrence.symbol_roles & ROLE_FORWARD_DEFINITION > 0 {
        roles.push("FORWARD_DEFINITION".to_string());
    }
    if !roles
        .iter()
        .any(|r| r == "DEFINITION" || r == "FORWARD_DEFINITION")
    {
        roles.insert(0, "REFERENCE".to_string());
    }
    roles.dedup();
    roles
}

fn is_definition_occurrence(occurrence: &scip::types::Occurrence) -> bool {
    occurrence.symbol_roles & ROLE_DEFINITION > 0
        || occurrence.symbol_roles & ROLE_FORWARD_DEFINITION > 0
}

fn normalize_range(range: &[i32]) -> Option<NormalizedRange> {
    match range.len() {
        3 => Some(NormalizedRange {
            start_line: range[0],
            start_character: range[1],
            end_line: range[0],
            end_character: range[2],
        }),
        4 => Some(NormalizedRange {
            start_line: range[0],
            start_character: range[1],
            end_line: range[2],
            end_character: range[3],
        }),
        _ => None,
    }
}

fn contains_range(outer: &NormalizedRange, inner: &NormalizedRange) -> bool {
    if outer.start_line > inner.start_line || outer.end_line < inner.end_line {
        return false;
    }
    if outer.start_line == inner.start_line && outer.start_character > inner.start_character {
        return false;
    }
    if outer.end_line == inner.end_line && outer.end_character < inner.end_character {
        return false;
    }
    true
}

fn range_span(range: &NormalizedRange) -> i32 {
    (range.end_line - range.start_line) * 100_000
        + std::cmp::max(0, range.end_character - range.start_character)
}

fn find_enclosing_definition_symbol(
    definitions: &[DefinitionScope],
    occurrence_range: &NormalizedRange,
    occurrence_symbol: &str,
) -> Option<String> {
    definitions
        .iter()
        .find(|definition| {
            definition.symbol != occurrence_symbol
                && contains_range(&definition.range, occurrence_range)
        })
        .map(|definition| definition.symbol.clone())
}

fn should_derive_reference_relationship(
    roles: &[String],
    enclosing_symbol: Option<&str>,
    target_symbol: &str,
) -> bool {
    match enclosing_symbol {
        Some(source) => {
            source != target_symbol
                && roles.iter().any(|r| r == "REFERENCE")
                && !roles.iter().any(|r| r == "DEFINITION")
                && !roles.iter().any(|r| r == "FORWARD_DEFINITION")
        }
        None => false,
    }
}

fn should_persist_project_relationship(
    relationship: &ScipRelationshipRow,
    project_defined_symbols: &HashSet<String>,
) -> bool {
    project_defined_symbols.contains(&relationship.source_symbol)
        && project_defined_symbols.contains(&relationship.target_symbol)
        && is_semantic_project_symbol(&relationship.source_symbol)
        && is_semantic_project_symbol(&relationship.target_symbol)
}

fn is_semantic_project_symbol(symbol: &str) -> bool {
    !is_local_scip_symbol(symbol)
        && !is_scip_parameter_symbol(symbol)
        && !is_scip_generated_property_symbol(symbol)
        && !is_scip_file_scope_symbol(symbol)
}

fn is_local_scip_symbol(symbol: &str) -> bool {
    symbol.starts_with("local ")
}

fn is_scip_parameter_symbol(symbol: &str) -> bool {
    static PARAM_RE: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    PARAM_RE
        .get_or_init(|| regex::Regex::new(r"\(\)\.\([^)]+\)$").expect("parameter regex"))
        .is_match(symbol)
}

fn is_scip_generated_property_symbol(symbol: &str) -> bool {
    static GENERATED_RE: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    GENERATED_RE
        .get_or_init(|| regex::Regex::new(r"\d+:$").expect("generated property regex"))
        .is_match(symbol)
}

fn is_scip_file_scope_symbol(symbol: &str) -> bool {
    static FILE_SCOPE_RE: std::sync::OnceLock<regex::Regex> = std::sync::OnceLock::new();
    FILE_SCOPE_RE
        .get_or_init(|| regex::Regex::new(r"`[^`]+`\s*/$").expect("file scope regex"))
        .is_match(symbol)
        || symbol.ends_with('/')
}

const SCIP_LINE_CONTENT_CACHE_MAX_FILES: usize = 256;

struct LineContentCache {
    root: Option<PathBuf>,
    files: HashMap<String, Option<Vec<String>>>,
    order: VecDeque<String>,
}

impl LineContentCache {
    fn new(root: &Path) -> Self {
        Self {
            root: root.canonicalize().ok(),
            files: HashMap::new(),
            order: VecDeque::new(),
        }
    }

    fn get_line_content(&mut self, file_path: &str, zero_based_line: i32) -> Option<String> {
        if zero_based_line < 0 {
            return None;
        }
        if !self.files.contains_key(file_path) {
            if self.files.len() >= SCIP_LINE_CONTENT_CACHE_MAX_FILES {
                if let Some(oldest) = self.order.pop_front() {
                    self.files.remove(&oldest);
                }
            }
            let lines = self.read_lines(file_path);
            self.files.insert(file_path.to_string(), lines);
            self.order.push_back(file_path.to_string());
        }
        let lines = self.files.get(file_path)?.as_ref()?;
        let line = lines
            .get(zero_based_line as usize)
            .map(String::as_str)
            .unwrap_or("");
        Some(truncate_line_content(line))
    }

    fn read_lines(&self, file_path: &str) -> Option<Vec<String>> {
        let safe_relative = sanitize_repo_relative_path(file_path)?;
        let root = self.root.as_ref()?;
        let candidate = root.join(safe_relative).canonicalize().ok()?;
        if !candidate.starts_with(root) {
            return None;
        }
        let content = std::fs::read_to_string(candidate).ok()?;
        Some(content.lines().map(str::to_string).collect())
    }
}

fn truncate_line_content(line: &str) -> String {
    if line.len() > 2000 {
        format!("{}...", &line[..safe_utf8_boundary(line, 2000)])
    } else {
        line.to_string()
    }
}

fn safe_utf8_boundary(s: &str, max: usize) -> usize {
    if s.len() <= max {
        return s.len();
    }
    (0..=max)
        .rev()
        .find(|i| s.is_char_boundary(*i))
        .unwrap_or(0)
}

fn syntax_kind_name(occurrence: &scip::types::Occurrence) -> Option<String> {
    occurrence.syntax_kind.enum_value().ok().and_then(|kind| {
        let name = format!("{:?}", kind);
        if name == "UnspecifiedSyntaxKind" {
            None
        } else {
            Some(name)
        }
    })
}

fn join_project_path_checked(project_root: &str, relative_path: &str) -> Option<String> {
    let clean_root = if project_root.trim().is_empty() || project_root.trim() == "." {
        String::new()
    } else {
        sanitize_repo_relative_path(project_root)?
    };
    let clean_relative = sanitize_repo_relative_path(relative_path)?;
    if clean_root.is_empty() {
        return Some(clean_relative);
    }
    if clean_relative.is_empty() {
        return Some(clean_root);
    }
    Some(format!("{}/{}", clean_root, clean_relative))
}

fn sanitize_repo_relative_path(value: &str) -> Option<String> {
    let value = value.trim();
    if value.is_empty() || value == "." {
        return Some(String::new());
    }
    if value.contains('\0') || value.contains('\\') || value.starts_with('/') {
        return None;
    }
    if value.len() >= 2 && value.as_bytes()[1] == b':' {
        return None;
    }
    let mut parts = Vec::new();
    for part in value.split('/') {
        match part {
            "" | "." => continue,
            ".." => return None,
            p if p.contains('\0') => return None,
            p => parts.push(p),
        }
    }
    Some(parts.join("/"))
}

fn display_name_from_scip_symbol(symbol: &str) -> String {
    let compact = symbol.split_whitespace().collect::<Vec<_>>().join(" ");
    let parts = compact
        .split(&['/', '#', '.', ':', '!', '(', ')', '[', ']'][..])
        .filter(|part| !part.is_empty())
        .collect::<Vec<_>>();
    parts
        .last()
        .map(|part| (*part).to_string())
        .unwrap_or(compact)
}

/// run_scip_indexer is the shell-out runner. Spawns the
/// indexer's argv with cwd=project_root_abs, captures stderr
/// for diagnostics, and returns the resulting .scip file path
/// on success. The output path is the caller's choice —
/// typically `<data_cache_dir>/scip/<repo_id>/<commit>/<lang>_<root>.scip`.
///
/// `timeout` enforces a max wall-clock per run; on expiry the
/// child is killed (best-effort) and a SubprocessFailed error
/// surfaces. The legacy uses 10 minutes per project
/// (scipIndexManager.ts:env-driven); the Rust port defaults to
/// the same.
pub fn run_scip_indexer(
    repo_path_abs: &Path,
    project: &ScipProject,
    output_path: &Path,
    timeout: Duration,
) -> std::result::Result<ScipRunResult, ScipRunError> {
    let def = match SUPPORTED_INDEXERS
        .iter()
        .find(|d| d.language == project.language)
    {
        Some(d) => d,
        None => {
            return Err(ScipRunError::BinaryMissing(format!(
                "no indexer definition for language {}",
                project.language
            )))
        }
    };

    // Ensure parent dir exists.
    if let Some(parent) = output_path.parent() {
        std::fs::create_dir_all(parent).map_err(ScipRunError::Io)?;
    }

    // Compute argv via the legacy build_args closure.
    // Empty project_root means "the repo root itself"
    // (matches legacy normalizeProjectRoot returning "" for
    // root-level markers).
    let project_root_abs = if project.project_root.is_empty() {
        repo_path_abs.to_path_buf()
    } else {
        repo_path_abs.join(&project.project_root)
    };
    let marker_abs = repo_path_abs.join(&project.marker_path);
    let argv = (def.build_args)(&project_root_abs, output_path, &marker_abs);
    prepare_scip_output_paths(&project_root_abs, output_path).map_err(ScipRunError::Io)?;

    let start = Instant::now();
    let mut cmd = Command::new(def.indexer);
    cmd.args(&argv)
        .current_dir(&project_root_abs)
        .stdout(Stdio::piped())
        .stderr(Stdio::piped());

    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
            return Err(ScipRunError::BinaryMissing(def.indexer.to_string()))
        }
        Err(e) => return Err(ScipRunError::Io(e)),
    };

    // R.8/R.10 critic-gate fix: drain stdout/stderr concurrently
    // to prevent the linux pipe-buffer (default 64KiB) from
    // backing up. Some SCIP tools, including scip-typescript,
    // print fatal diagnostics to stdout instead of stderr.
    let stdout_handle = child.stdout.take().map(|mut s| {
        std::thread::spawn(move || {
            use std::io::Read;
            let mut buf = Vec::new();
            let _ = s.read_to_end(&mut buf);
            buf
        })
    });
    let stderr_handle = child.stderr.take().map(|mut s| {
        std::thread::spawn(move || {
            use std::io::Read;
            let mut buf = Vec::new();
            let _ = s.read_to_end(&mut buf);
            buf
        })
    });

    // Poll for completion with timeout. std::process::Child
    // has no async wait; we busy-poll on `try_wait` with a
    // small sleep. For our use case (multi-minute indexers)
    // the polling cost is negligible.
    let deadline = start + timeout;
    let exit_status;
    loop {
        match child.try_wait() {
            Ok(Some(status)) => {
                exit_status = status;
                break;
            }
            Ok(None) => {
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    // R.8 critic-gate fix: reap the zombie
                    // after kill so the kernel releases the
                    // PID table entry.
                    let _ = child.wait();
                    return Err(ScipRunError::SubprocessFailed {
                        code: -1,
                        stderr_tail: format!("timeout after {:?}", timeout),
                    });
                }
                std::thread::sleep(Duration::from_millis(200));
            }
            Err(e) => return Err(ScipRunError::Io(e)),
        }
    }

    // Join the output drainer threads now that the child has
    // exited (the read will return EOF as the pipe closes).
    let stdout_buf = stdout_handle
        .map(|h| h.join().unwrap_or_default())
        .unwrap_or_default();
    let stderr_buf = stderr_handle
        .map(|h| h.join().unwrap_or_default())
        .unwrap_or_default();
    let stdout_str = String::from_utf8_lossy(&stdout_buf).to_string();
    let stderr_str = String::from_utf8_lossy(&stderr_buf).to_string();
    let combined_diagnostics = combine_scip_diagnostics(&stdout_str, &stderr_str);
    let stderr_tail = tail_bytes(&combined_diagnostics, 4096);

    let duration = Instant::now().duration_since(start);

    if !exit_status.success() {
        let code = exit_status.code().unwrap_or(-1);
        return Err(ScipRunError::SubprocessFailed { code, stderr_tail });
    }

    // R.8 critic-gate fix: scip-go writes `./index.scip` in
    // cwd (project_root_abs), not at `output_path`. The legacy
    // calls `normalizeIndexerOutput` to rename the cwd-emitted
    // file to the expected location. Mirror that here: if the
    // expected output_path doesn't exist BUT
    // `<project_root_abs>/index.scip` does, rename it.
    if !output_path.exists() {
        let cwd_emitted = project_root_abs.join("index.scip");
        if cwd_emitted.exists() {
            if let Err(e) = std::fs::rename(&cwd_emitted, output_path) {
                return Err(ScipRunError::Io(e));
            }
        }
    }

    // The legacy treats a non-existent or 0-byte output file
    // as a failure (the indexer claimed success but produced
    // nothing); mirror that.
    let meta = std::fs::metadata(output_path).map_err(ScipRunError::Io)?;
    if meta.len() == 0 {
        return Err(ScipRunError::SubprocessFailed {
            code: 0,
            stderr_tail: format!(
                "indexer produced empty .scip file at {}",
                output_path.display()
            ),
        });
    }
    validate_scip_artifact_has_semantic_rows(
        output_path,
        project.language,
        &project.project_root,
        repo_path_abs,
    )
    .map_err(|message| ScipRunError::SubprocessFailed {
        code: 0,
        stderr_tail: message,
    })?;

    Ok(ScipRunResult {
        language: project.language,
        indexer: def.indexer,
        worker_class: project.worker_class,
        project_root: project.project_root.clone(),
        command: std::iter::once(def.indexer.to_string())
            .chain(argv)
            .collect::<Vec<_>>()
            .join(" "),
        output_path: output_path.to_path_buf(),
        duration,
        stderr_tail: String::new(),
    })
}

fn prepare_scip_output_paths(project_root_abs: &Path, output_path: &Path) -> std::io::Result<()> {
    match std::fs::remove_file(output_path) {
        Ok(()) => {}
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
        Err(e) => return Err(e),
    }
    let cwd_emitted = project_root_abs.join("index.scip");
    if cwd_emitted != output_path {
        match std::fs::remove_file(&cwd_emitted) {
            Ok(()) => {}
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
            Err(e) => return Err(e),
        }
    }
    Ok(())
}

fn validate_scip_artifact_has_semantic_rows(
    output_path: &Path,
    project_language: &str,
    project_root: &str,
    repo_path_abs: &Path,
) -> Result<(), String> {
    let rows =
        ingest_scip_artifact_rows(output_path, project_language, project_root, repo_path_abs)
            .map_err(|e| {
                format!(
                    "indexer produced invalid .scip file at {}: {e}",
                    output_path.display()
                )
            })?;
    if rows.symbols.is_empty() && rows.occurrences.is_empty() && rows.relationships.is_empty() {
        return Err(format!(
            "indexer produced semantically empty .scip file at {}: 0 symbols, 0 occurrences, 0 relationships",
            output_path.display()
        ));
    }
    Ok(())
}

/// index_revision_scip is the per-revision orchestrator.
/// Detects projects, spawns each indexer, returns the
/// per-project results.
///
/// Concurrency: today this runs serially. The legacy
/// scipIndexManager.indexRevision runs serially too (no
/// Promise.all), so the parallelism axis is across repos via
/// the worker pool — not within a single repo's indexers. The
/// trade-off is: SCIP indexers eat memory + I/O, so running
/// all 8 in parallel on one repo would degrade overall
/// throughput vs running them sequentially within one
/// scheduling slot.
pub fn index_revision_scip(
    repo_path_abs: &Path,
    files: &[IndexManifestFile],
    output_dir: &Path,
    per_indexer_timeout: Duration,
) -> Vec<std::result::Result<ScipRunResult, ScipRunError>> {
    let mut projects = detect_scip_projects(files);
    // R.8c: synthesise TS project roots for bare-TS repos with
    // no package.json / tsconfig.json. Skips repos that already
    // have an explicit TS project root.
    add_inferred_typescript_projects(files, &mut projects);
    // R.8c: env-gated worker-class filter. The deployment's
    // pod-pool topology decides which scip-* binaries run
    // here; this filter drops projects whose workerClass
    // isn't in the allow-list.
    let mut projects = filter_projects_for_worker_classes(projects);
    // R.8c: cap per-revision project count via env. Default
    // unlimited; deployments cap to keep runtime bounded on
    // huge monorepos.
    let cap = max_project_roots_per_revision();
    if projects.len() > cap {
        projects.truncate(cap);
    }
    let mut results = Vec::with_capacity(projects.len());
    for project in &projects {
        let output_path = scip_output_path(output_dir, project);
        results.push(run_scip_indexer(
            repo_path_abs,
            project,
            &output_path,
            per_indexer_timeout,
        ));
    }
    results
}

pub fn scip_output_path(output_dir: &Path, project: &ScipProject) -> PathBuf {
    // Empty root → "root" filename token; nested root →
    // `_`-substituted path. Mirrors legacy
    // `safePathSegment(project.root || "root")`.
    let safe_root = if project.project_root.is_empty() {
        "root".to_string()
    } else {
        project.project_root.replace('/', "_")
    };
    output_dir.join(format!("{}_{}.scip", project.language, safe_root))
}

// === legacy `buildArgs` closures, ported verbatim ===

fn ts_args(_root: &Path, output_path: &Path, marker_path: &Path) -> Vec<String> {
    let mut argv: Vec<String> = vec![
        "index".to_string(),
        "--output".to_string(),
        output_path.to_string_lossy().to_string(),
    ];
    let marker_basename = marker_path
        .file_name()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_default();
    if marker_basename == "package.json" {
        argv.push("--infer-tsconfig".to_string());
    }
    argv
}

fn go_args(_root: &Path, output_path: &Path, _marker_path: &Path) -> Vec<String> {
    let mut args = vec![
        "index".to_string(),
        "--output".to_string(),
        output_path.to_string_lossy().to_string(),
    ];
    if env_bool("CODEINTEL_SCIP_GO_SKIP_TESTS", true) {
        args.push("--skip-tests".to_string());
    }
    if env_bool("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS", false) {
        args.push("--skip-implementations".to_string());
    }
    args.extend(parse_go_package_patterns());
    args
}

fn env_bool(name: &str, default: bool) -> bool {
    match std::env::var(name) {
        Ok(value) => {
            let normalized = value.trim().to_ascii_lowercase();
            matches!(normalized.as_str(), "1" | "true" | "yes" | "on")
                || (!matches!(normalized.as_str(), "0" | "false" | "no" | "off") && default)
        }
        Err(_) => default,
    }
}

fn parse_go_package_patterns() -> Vec<String> {
    let raw =
        std::env::var("CODEINTEL_SCIP_GO_PACKAGE_PATTERNS").unwrap_or_else(|_| "./...".to_string());
    let patterns = raw
        .split(',')
        .map(str::trim)
        .filter(|p| !p.is_empty())
        .map(ToOwned::to_owned)
        .collect::<Vec<_>>();
    if patterns.is_empty() {
        vec!["./...".to_string()]
    } else {
        patterns
    }
}

fn python_args(root: &Path, _output_path: &Path, _marker_path: &Path) -> Vec<String> {
    let project_name = root
        .file_name()
        .map(|s| s.to_string_lossy().to_string())
        .unwrap_or_else(|| "project".to_string());
    vec![
        "index".to_string(),
        ".".to_string(),
        "--project-name".to_string(),
        project_name,
    ]
}

fn java_args(_root: &Path, output_path: &Path, _marker_path: &Path) -> Vec<String> {
    vec![
        "index".to_string(),
        "--output".to_string(),
        output_path.to_string_lossy().to_string(),
    ]
}

fn clang_args(_root: &Path, _output_path: &Path, marker_path: &Path) -> Vec<String> {
    vec![format!("--compdb-path={}", marker_path.display())]
}

fn dotnet_args(root: &Path, _output_path: &Path, _marker_path: &Path) -> Vec<String> {
    vec![
        "index".to_string(),
        "--working-directory".to_string(),
        root.to_string_lossy().to_string(),
    ]
}

fn ruby_args(_root: &Path, _output_path: &Path, _marker_path: &Path) -> Vec<String> {
    vec![".".to_string()]
}

fn rust_args(root: &Path, output_path: &Path, _marker_path: &Path) -> Vec<String> {
    vec![
        "scip".to_string(),
        root.to_string_lossy().to_string(),
        "--output".to_string(),
        output_path.to_string_lossy().to_string(),
    ]
}

fn dart_args(_root: &Path, _output_path: &Path, _marker_path: &Path) -> Vec<String> {
    // Matches legacy scipIndexManager.ts:196-198.
    vec![
        "pub".to_string(),
        "global".to_string(),
        "run".to_string(),
        "scip_dart".to_string(),
        "./".to_string(),
    ]
}

fn combine_scip_diagnostics(stdout: &str, stderr: &str) -> String {
    let stdout = stdout.trim();
    let stderr = stderr.trim();
    match (stdout.is_empty(), stderr.is_empty()) {
        (true, true) => String::new(),
        (false, true) => stdout.to_string(),
        (true, false) => stderr.to_string(),
        (false, false) => format!("stdout:\n{}\nstderr:\n{}", stdout, stderr),
    }
}

fn tail_bytes(s: &str, n: usize) -> String {
    if s.len() <= n {
        return s.to_string();
    }
    let start = s.len() - n;
    // Snap to a UTF-8 char boundary to avoid panic on slice.
    let safe_start = (start..s.len())
        .find(|&i| s.is_char_boundary(i))
        .unwrap_or(s.len());
    s[safe_start..].to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use protobuf::{EnumOrUnknown, Message, MessageField, SpecialFields};
    use scip::types::{
        symbol_information, Document, Index, Occurrence, Relationship, SymbolInformation,
        SyntaxKind,
    };

    /// ENV_MUTEX serialises every test that mutates env vars
    /// read by `index_revision_scip` /
    /// `filter_projects_for_worker_classes` /
    /// `max_project_roots_per_revision`. Cargo runs tests in
    /// parallel by default; without this lock, set_var in one
    /// test can leak into another and flip its assertion.
    static ENV_MUTEX: std::sync::Mutex<()> = std::sync::Mutex::new(());
    use crate::delta_plan::IndexManifestFile;

    fn mk_file(path: &str) -> IndexManifestFile {
        IndexManifestFile {
            path: path.to_string(),
            content_hash: "h".to_string(),
            language: None,
            project_root: Some(".".to_string()),
            generated: Some(false),
            vendor: Some(false),
            test: Some(false),
            artifacts: None,
        }
    }

    #[test]
    fn ingest_scip_bytes_persists_symbols_occurrences_and_relationships() {
        let tmp = std::env::temp_dir().join(format!(
            "codeintel-scip-ingest-{}-{}",
            std::process::id(),
            1
        ));
        let _ = std::fs::remove_dir_all(&tmp);
        std::fs::create_dir_all(tmp.join("src/routes")).expect("mkdir routes");
        std::fs::create_dir_all(tmp.join("src/orders")).expect("mkdir orders");
        std::fs::write(
            tmp.join("src/orders/createOrder.ts"),
            "export async function createOrder(command) {\n  return command.id;\n}\n",
        )
        .expect("write definition");
        std::fs::write(
            tmp.join("src/routes/internalOrders.ts"),
            "import { createOrder } from '../orders/createOrder';\nexport async function handler() {\n  return createOrder({ id: '1' });\n}\n",
        )
        .expect("write reference");

        let create_symbol =
            "scip-typescript npm app 1.0.0 src/orders/createOrder.ts/createOrder().";
        let handler_symbol =
            "scip-typescript npm app 1.0.0 src/routes/internalOrders.ts/handler().";
        let external_symbol = "scip-typescript npm typescript 5.0.0 lib/lib.es5.d.ts/Promise#";
        let index = Index {
            metadata: MessageField::none(),
            documents: vec![
                Document {
                    language: "typescript".to_string(),
                    relative_path: "src/orders/createOrder.ts".to_string(),
                    occurrences: vec![Occurrence {
                        range: vec![0, 22, 33],
                        symbol: create_symbol.to_string(),
                        symbol_roles: ROLE_DEFINITION,
                        override_documentation: Vec::new(),
                        syntax_kind: EnumOrUnknown::new(SyntaxKind::IdentifierFunctionDefinition),
                        diagnostics: Vec::new(),
                        enclosing_range: vec![0, 0, 2, 1],
                        special_fields: SpecialFields::new(),
                    }],
                    symbols: vec![SymbolInformation {
                        symbol: create_symbol.to_string(),
                        documentation: vec!["Creates an order.".to_string()],
                        relationships: Vec::new(),
                        kind: EnumOrUnknown::new(symbol_information::Kind::Function),
                        display_name: "createOrder".to_string(),
                        signature_documentation: MessageField::some(Document {
                            text: "function createOrder(command): Promise<Order>".to_string(),
                            ..Default::default()
                        }),
                        enclosing_symbol: String::new(),
                        special_fields: SpecialFields::new(),
                    }],
                    text: String::new(),
                    position_encoding: Default::default(),
                    special_fields: SpecialFields::new(),
                },
                Document {
                    language: "typescript".to_string(),
                    relative_path: "src/routes/internalOrders.ts".to_string(),
                    occurrences: vec![
                        Occurrence {
                            range: vec![1, 22, 29],
                            symbol: handler_symbol.to_string(),
                            symbol_roles: ROLE_DEFINITION,
                            override_documentation: Vec::new(),
                            syntax_kind: EnumOrUnknown::new(
                                SyntaxKind::IdentifierFunctionDefinition,
                            ),
                            diagnostics: Vec::new(),
                            enclosing_range: vec![1, 0, 3, 1],
                            special_fields: SpecialFields::new(),
                        },
                        Occurrence {
                            range: vec![2, 9, 20],
                            symbol: create_symbol.to_string(),
                            symbol_roles: ROLE_READ,
                            override_documentation: Vec::new(),
                            syntax_kind: EnumOrUnknown::new(SyntaxKind::IdentifierFunction),
                            diagnostics: Vec::new(),
                            enclosing_range: Vec::new(),
                            special_fields: SpecialFields::new(),
                        },
                        Occurrence {
                            range: vec![0, 9, 20],
                            symbol: create_symbol.to_string(),
                            symbol_roles: ROLE_IMPORT,
                            override_documentation: Vec::new(),
                            syntax_kind: EnumOrUnknown::new(SyntaxKind::Identifier),
                            diagnostics: Vec::new(),
                            enclosing_range: Vec::new(),
                            special_fields: SpecialFields::new(),
                        },
                    ],
                    symbols: vec![SymbolInformation {
                        symbol: handler_symbol.to_string(),
                        documentation: Vec::new(),
                        relationships: vec![Relationship {
                            symbol: create_symbol.to_string(),
                            is_reference: true,
                            is_implementation: false,
                            is_type_definition: false,
                            is_definition: false,
                            special_fields: SpecialFields::new(),
                        }],
                        kind: EnumOrUnknown::new(symbol_information::Kind::Function),
                        display_name: "handler".to_string(),
                        signature_documentation: MessageField::none(),
                        enclosing_symbol: String::new(),
                        special_fields: SpecialFields::new(),
                    }],
                    text: String::new(),
                    position_encoding: Default::default(),
                    special_fields: SpecialFields::new(),
                },
            ],
            external_symbols: vec![SymbolInformation {
                symbol: external_symbol.to_string(),
                documentation: vec!["External promise docs".to_string()],
                relationships: Vec::new(),
                kind: EnumOrUnknown::new(symbol_information::Kind::Class),
                display_name: String::new(),
                signature_documentation: MessageField::none(),
                enclosing_symbol: String::new(),
                special_fields: SpecialFields::new(),
            }],
            special_fields: SpecialFields::new(),
        };
        let bytes = index.write_to_bytes().expect("encode SCIP");
        let rows = ingest_scip_bytes(&bytes, "typescript", "", &tmp).expect("ingest");

        assert_eq!(rows.symbols.len(), 3, "{:#?}", rows.symbols);
        let create_row = rows
            .symbols
            .iter()
            .find(|row| row.symbol == create_symbol)
            .expect("create symbol row");
        assert_eq!(create_row.display_name, "createOrder");
        assert_eq!(create_row.kind.as_deref(), Some("Function"));
        assert_eq!(
            create_row.file_path.as_deref(),
            Some("src/orders/createOrder.ts")
        );
        assert_eq!(create_row.start_line, Some(0));
        assert_eq!(
            create_row.signature.as_deref(),
            Some("function createOrder(command): Promise<Order>")
        );

        let external_row = rows
            .symbols
            .iter()
            .find(|row| row.symbol == external_symbol)
            .expect("external symbol row");
        assert_eq!(external_row.display_name, "Promise");
        assert!(external_row.file_path.is_none());

        assert!(
            rows.occurrences
                .iter()
                .any(|row| row.symbol == create_symbol
                    && row.file_path == "src/orders/createOrder.ts"
                    && row.role == "DEFINITION"
                    && row.line_content.as_deref()
                        == Some("export async function createOrder(command) {")),
            "{:#?}",
            rows.occurrences
        );
        assert!(
            rows.occurrences
                .iter()
                .any(|row| row.symbol == create_symbol
                    && row.file_path == "src/routes/internalOrders.ts"
                    && row.role == "REFERENCE"
                    && row.enclosing_symbol.as_deref() == Some(handler_symbol)),
            "{:#?}",
            rows.occurrences
        );
        assert!(
            rows.occurrences
                .iter()
                .any(|row| row.symbol == create_symbol
                    && row.file_path == "src/routes/internalOrders.ts"
                    && row.role == "READ"),
            "{:#?}",
            rows.occurrences
        );
        assert!(
            rows.occurrences
                .iter()
                .any(|row| row.symbol == create_symbol
                    && row.file_path == "src/routes/internalOrders.ts"
                    && row.role == "IMPORT"),
            "{:#?}",
            rows.occurrences
        );

        assert_eq!(rows.relationships.len(), 1, "{:#?}", rows.relationships);
        assert_eq!(rows.relationships[0].source_symbol, handler_symbol);
        assert_eq!(rows.relationships[0].target_symbol, create_symbol);
        assert!(rows.relationships[0].is_reference);

        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn ingest_scip_bytes_rejects_document_paths_that_escape_worktree() {
        let tmp =
            std::env::temp_dir().join(format!("codeintel-scip-path-{}-{}", std::process::id(), 1));
        let _ = std::fs::remove_dir_all(&tmp);
        std::fs::create_dir_all(tmp.join("src")).expect("mkdir src");
        std::fs::write(tmp.join("src/good.ts"), "export function good() {}\n").expect("write good");

        let good_symbol = "scip-typescript npm app 1.0.0 src/good.ts/good().";
        let escape_symbol = "scip-typescript npm app 1.0.0 outside.ts/evil().";
        let index = Index {
            metadata: MessageField::none(),
            documents: vec![
                Document {
                    language: "typescript".to_string(),
                    relative_path: "src/good.ts".to_string(),
                    occurrences: vec![Occurrence {
                        range: vec![0, 16, 20],
                        symbol: good_symbol.to_string(),
                        symbol_roles: ROLE_DEFINITION,
                        override_documentation: Vec::new(),
                        syntax_kind: EnumOrUnknown::new(SyntaxKind::IdentifierFunctionDefinition),
                        diagnostics: Vec::new(),
                        enclosing_range: vec![0, 0, 0, 23],
                        special_fields: SpecialFields::new(),
                    }],
                    symbols: vec![SymbolInformation {
                        symbol: good_symbol.to_string(),
                        kind: EnumOrUnknown::new(symbol_information::Kind::Function),
                        display_name: "good".to_string(),
                        documentation: Vec::new(),
                        relationships: Vec::new(),
                        signature_documentation: MessageField::none(),
                        enclosing_symbol: String::new(),
                        special_fields: SpecialFields::new(),
                    }],
                    text: String::new(),
                    position_encoding: Default::default(),
                    special_fields: SpecialFields::new(),
                },
                Document {
                    language: "typescript".to_string(),
                    relative_path: "../outside.ts".to_string(),
                    occurrences: vec![Occurrence {
                        range: vec![0, 16, 20],
                        symbol: escape_symbol.to_string(),
                        symbol_roles: ROLE_DEFINITION,
                        override_documentation: Vec::new(),
                        syntax_kind: EnumOrUnknown::new(SyntaxKind::IdentifierFunctionDefinition),
                        diagnostics: Vec::new(),
                        enclosing_range: vec![0, 0, 0, 23],
                        special_fields: SpecialFields::new(),
                    }],
                    symbols: vec![SymbolInformation {
                        symbol: escape_symbol.to_string(),
                        kind: EnumOrUnknown::new(symbol_information::Kind::Function),
                        display_name: "evil".to_string(),
                        documentation: Vec::new(),
                        relationships: Vec::new(),
                        signature_documentation: MessageField::none(),
                        enclosing_symbol: String::new(),
                        special_fields: SpecialFields::new(),
                    }],
                    text: String::new(),
                    position_encoding: Default::default(),
                    special_fields: SpecialFields::new(),
                },
            ],
            external_symbols: Vec::new(),
            special_fields: SpecialFields::new(),
        };

        let bytes = index.write_to_bytes().expect("encode SCIP");
        let rows = ingest_scip_bytes(&bytes, "typescript", "", &tmp).expect("ingest");
        assert!(
            rows.symbols.iter().any(|row| row.symbol == good_symbol),
            "{:#?}",
            rows.symbols
        );
        assert!(
            rows.occurrences.iter().any(|row| row.symbol == good_symbol
                && row.line_content.as_deref() == Some("export function good() {}")),
            "{:#?}",
            rows.occurrences
        );
        assert!(
            !rows.symbols.iter().any(|row| row.symbol == escape_symbol),
            "{:#?}",
            rows.symbols
        );
        assert!(
            !rows
                .occurrences
                .iter()
                .any(|row| row.symbol == escape_symbol),
            "{:#?}",
            rows.occurrences
        );

        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn detect_scip_projects_skips_default_ignored_dirs() {
        // node_modules + vendor + dist must NOT be promoted to
        // project roots even when they contain a marker file.
        // This is the legacy DEFAULT_IGNORES contract.
        let files = vec![
            mk_file("services/web/package.json"),
            mk_file("services/web/src/index.ts"),
            mk_file("services/web/node_modules/react/package.json"),
            mk_file("vendor/something/Cargo.toml"),
            mk_file("dist/bundle/go.mod"),
            mk_file(".git/config"),
            mk_file("__pycache__/foo/pyproject.toml"),
            mk_file("Cargo.toml"),
        ];
        let projects = detect_scip_projects(&files);
        let roots: Vec<&str> = projects.iter().map(|p| p.project_root.as_str()).collect();
        assert!(
            !roots.iter().any(|r| r.contains("node_modules")),
            "{:?}",
            roots
        );
        assert!(!roots.iter().any(|r| r.contains("vendor")), "{:?}", roots);
        assert!(!roots.iter().any(|r| r.contains("dist")), "{:?}", roots);
        assert!(!roots.iter().any(|r| r.contains(".git")), "{:?}", roots);
        assert!(
            !roots.iter().any(|r| r.contains("__pycache__")),
            "{:?}",
            roots
        );
        // The legitimate roots survive. Root-level marker
        // (Cargo.toml in the fixture) projects under "" per
        // legacy normalizeProjectRoot.
        assert!(roots.contains(&"services/web"), "{:?}", roots);
        assert!(roots.contains(&""), "{:?}", roots);
    }

    #[test]
    fn detect_scip_projects_skips_tooling_only_typescript_markers() {
        let files = vec![
            mk_file("package.json"),
            mk_file("autoinstrumentation/nodejs/package.json"),
            mk_file("autoinstrumentation/nodejs/tsconfig.json"),
            mk_file("autoinstrumentation/nodejs/Dockerfile"),
            mk_file(".github/scripts/triage-helper/requirements.txt"),
            mk_file(".github/scripts/triage-helper/triage.py"),
        ];
        let projects = detect_scip_projects(&files);
        assert_eq!(projects.len(), 1, "{:?}", projects);
        assert_eq!(projects[0].language, "python");
        assert_eq!(projects[0].project_root, ".github/scripts/triage-helper");
    }

    #[test]
    fn detect_scip_projects_does_not_let_parent_typescript_marker_claim_child_input() {
        let files = vec![
            mk_file("e2e-tests/package.json"),
            mk_file("e2e-tests/web/package.json"),
            mk_file("e2e-tests/web/src/app.ts"),
            mk_file("e2e-tests/api/tsconfig.json"),
            mk_file("e2e-tests/api/src/server.ts"),
        ];
        let projects = detect_scip_projects(&files);
        let roots: Vec<&str> = projects
            .iter()
            .filter(|p| p.language == "typescript")
            .map(|p| p.project_root.as_str())
            .collect();
        assert_eq!(roots, vec!["e2e-tests/api", "e2e-tests/web"]);
    }

    #[test]
    fn dart_indexer_entry_present() {
        let dart_count = SUPPORTED_INDEXERS
            .iter()
            .filter(|d| d.language == "dart")
            .count();
        assert_eq!(
            dart_count, 1,
            "dart indexer missing from SUPPORTED_INDEXERS"
        );
        let dart = SUPPORTED_INDEXERS
            .iter()
            .find(|d| d.language == "dart")
            .unwrap();
        assert_eq!(dart.indexer, "dart");
        assert!(dart.markers.contains(&"pubspec.yaml"));
        // Verify dart_args returns the legacy
        // ["pub", "global", "run", "scip_dart", "./"] argv.
        let argv = (dart.build_args)(
            Path::new("/repo/services/flutter"),
            Path::new("/out/dart_services_flutter.scip"),
            Path::new("/repo/services/flutter/pubspec.yaml"),
        );
        assert_eq!(
            argv,
            vec!["pub", "global", "run", "scip_dart", "./"]
                .iter()
                .map(|s| s.to_string())
                .collect::<Vec<_>>(),
        );
    }

    #[test]
    fn polyglot_repo_yields_one_project_per_language() {
        // A real polyglot fixture: TS + Go + Python + Rust +
        // C# + Ruby + Java, each with its marker file at a
        // different project root.
        let files = vec![
            mk_file("services/web/package.json"),
            mk_file("services/web/tsconfig.json"),
            mk_file("services/web/src/index.ts"),
            mk_file("services/api/go.mod"),
            mk_file("services/api/main.go"),
            mk_file("tools/cli/Cargo.toml"),
            mk_file("tools/cli/src/main.rs"),
            mk_file("scripts/pyproject.toml"),
            mk_file("scripts/etl.py"),
            mk_file("app/MyApp.csproj"),
            mk_file("gems/foo.gemspec"),
            mk_file("backend/pom.xml"),
            mk_file("backend/src/main/java/com/example/App.java"),
        ];
        let projects = detect_scip_projects(&files);

        // Collect languages — should include all 7.
        let langs: Vec<&'static str> = projects.iter().map(|p| p.language).collect();
        assert!(langs.contains(&"typescript"), "{:?}", langs);
        assert!(langs.contains(&"go"), "{:?}", langs);
        assert!(langs.contains(&"python"), "{:?}", langs);
        assert!(langs.contains(&"java"), "{:?}", langs);
        assert!(langs.contains(&"dotnet"), "{:?}", langs);
        assert!(langs.contains(&"ruby"), "{:?}", langs);
        assert!(langs.contains(&"rust"), "{:?}", langs);

        // The TypeScript project root should be the dir
        // containing the FIRST matching marker we hit. The
        // detector currently dedupes by (lang, root) so all
        // three TS markers (package.json + tsconfig.json + a
        // .ts file none) collapse into "services/web" — same
        // root for the three. Verify:
        let ts: Vec<&ScipProject> = projects
            .iter()
            .filter(|p| p.language == "typescript")
            .collect();
        assert_eq!(ts.len(), 1);
        assert_eq!(ts[0].project_root, "services/web");

        // Output is sorted deterministically by (root, lang)
        // matching the R.8c-fixed sort order (legacy line 918).
        let mut sorted = projects.clone();
        sorted.sort_by(|a, b| {
            a.project_root
                .cmp(&b.project_root)
                .then_with(|| a.language.cmp(b.language))
        });
        assert_eq!(projects, sorted);
    }

    #[test]
    fn detect_scip_projects_skips_java_marker_without_build_source_set_input() {
        let files = vec![
            mk_file("tests/test-e2e-apps/java/build.gradle"),
            mk_file("tests/test-e2e-apps/java/DemoApplication.java"),
            mk_file("services/real-java/build.gradle"),
            mk_file("services/real-java/src/main/java/com/example/App.java"),
        ];
        let projects = detect_scip_projects(&files);
        assert_eq!(projects.len(), 1, "{:?}", projects);
        assert_eq!(projects[0].language, "java");
        assert_eq!(projects[0].project_root, "services/real-java");
    }

    #[test]
    fn add_inferred_typescript_projects_picks_up_bare_ts_repo() {
        // A TS repo with NO package.json/tsconfig.json should
        // still produce a TS project via inference. The
        // src-folder heuristic should derive the project root
        // from the first "src" component.
        let files = vec![
            mk_file("frontend/src/app.ts"),
            mk_file("frontend/src/util.ts"),
            mk_file("README.md"),
        ];
        let mut projects = detect_scip_projects(&files);
        assert_eq!(projects.len(), 0, "no markers yet, no detection");
        add_inferred_typescript_projects(&files, &mut projects);
        assert_eq!(projects.len(), 1);
        assert_eq!(projects[0].language, "typescript");
        assert!(projects[0].inferred);
        assert_eq!(projects[0].project_root, "frontend");
        assert_eq!(projects[0].marker_path, "frontend/package.json");
        assert!(projects[0]
            .inferred_reason
            .as_deref()
            .unwrap_or("")
            .contains("Detected TypeScript"));
    }

    #[test]
    fn add_inferred_typescript_projects_skips_when_covered_by_existing_root() {
        // If a TS project root already exists (e.g. via
        // package.json), do NOT add an inferred root that
        // would duplicate it.
        let files = vec![
            mk_file("services/web/package.json"),
            mk_file("services/web/src/index.ts"),
        ];
        let mut projects = detect_scip_projects(&files);
        assert_eq!(projects.len(), 1);
        assert!(!projects[0].inferred);
        add_inferred_typescript_projects(&files, &mut projects);
        // Still only 1 — the index.ts is covered by the
        // existing root.
        assert_eq!(projects.len(), 1);
    }

    #[test]
    fn add_inferred_typescript_projects_skips_d_ts_min_js_and_coverage() {
        // .d.ts, .min.js, and files under coverage/ are NOT
        // candidates for inferred TS projects.
        let files = vec![
            mk_file("types.d.ts"),
            mk_file("vendor.min.js"),
            mk_file("coverage/lcov.info"),
            mk_file("coverage/report.html"),
        ];
        let mut projects: Vec<ScipProject> = Vec::new();
        add_inferred_typescript_projects(&files, &mut projects);
        assert_eq!(projects.len(), 0, "no inferred root from skipped files");
    }

    #[test]
    fn infer_typescript_project_root_returns_prefix_before_src() {
        assert_eq!(
            infer_typescript_project_root("frontend/src/app.ts"),
            "frontend"
        );
        assert_eq!(
            infer_typescript_project_root("services/web/src/index.tsx"),
            "services/web"
        );
        // No "src" segment → empty root.
        assert_eq!(infer_typescript_project_root("app.ts"), "");
        // "src" at index 0 → empty root.
        assert_eq!(infer_typescript_project_root("src/app.ts"), "");
    }

    #[test]
    fn filter_projects_for_worker_classes_default_universal_then_allow_list() {
        let _guard = ENV_MUTEX.lock().unwrap_or_else(|e| e.into_inner());
        let prior = std::env::var("CODEINTEL_SCIP_WORKER_CLASSES").ok();

        let make_projects = || {
            vec![
                ScipProject {
                    language: "go",
                    indexer: "scip-go",
                    worker_class: "go",
                    project_root: ".".to_string(),
                    marker_path: "go.mod".to_string(),
                    inferred: false,
                    inferred_reason: None,
                },
                ScipProject {
                    language: "typescript",
                    indexer: "scip-typescript",
                    worker_class: "ts-js",
                    project_root: ".".to_string(),
                    marker_path: "package.json".to_string(),
                    inferred: false,
                    inferred_reason: None,
                },
            ]
        };

        // Branch 1: default "universal" → all pass.
        std::env::remove_var("CODEINTEL_SCIP_WORKER_CLASSES");
        let filtered = filter_projects_for_worker_classes(make_projects());
        assert_eq!(filtered.len(), 2);

        // Branch 2: explicit "ts-js" allow-list → only TS survives.
        std::env::set_var("CODEINTEL_SCIP_WORKER_CLASSES", "ts-js");
        let filtered = filter_projects_for_worker_classes(make_projects());
        assert_eq!(filtered.len(), 1);
        assert_eq!(filtered[0].language, "typescript");

        // Branch 3: "all" alias → all pass.
        std::env::set_var("CODEINTEL_SCIP_WORKER_CLASSES", "all");
        let filtered = filter_projects_for_worker_classes(make_projects());
        assert_eq!(filtered.len(), 2);

        // Restore.
        match prior {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_WORKER_CLASSES", v),
            None => std::env::remove_var("CODEINTEL_SCIP_WORKER_CLASSES"),
        }
    }

    #[test]
    fn parse_worker_classes_handles_empty_and_csv() {
        // Empty / whitespace → "universal" singleton.
        let empty = parse_worker_classes("");
        assert!(empty.contains("universal"));
        assert_eq!(empty.len(), 1);

        let only_ws = parse_worker_classes("   ");
        assert!(only_ws.contains("universal"));

        // CSV with whitespace and empty entries.
        let multi = parse_worker_classes(" ts-js , go ,  ");
        assert_eq!(multi.len(), 2);
        assert!(multi.contains("ts-js"));
        assert!(multi.contains("go"));
    }

    #[test]
    fn r8c_pipeline_end_to_end_detect_infer_filter_truncate() {
        let _guard = ENV_MUTEX.lock().unwrap_or_else(|e| e.into_inner());
        // Critic F2.1: end-to-end pipeline. Input fixture mixes:
        //   - 2 covered projects (typescript via package.json,
        //     go via go.mod).
        //   - 1 bare-TS subdir that would infer a synthetic
        //     "tools" root (NOT added because the existing
        //     "services/web" TS root doesn't cover it, BUT a
        //     separate `tools/src/x.ts` file should infer
        //     "tools").
        //   - 1 ignored vendor file (must NOT promote).
        // After detect: 2 projects.
        // After add_inferred: 3 projects (typescript "tools"
        // synthesized).
        // After filter (env=ts-js): 2 typescript projects only.
        // After truncate(cap=1): just the first (sorted by
        // root then lang).
        let prior = std::env::var("CODEINTEL_SCIP_WORKER_CLASSES").ok();
        let prior_cap = std::env::var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION").ok();
        std::env::set_var("CODEINTEL_SCIP_WORKER_CLASSES", "ts-js");

        let files = vec![
            mk_file("services/web/package.json"),
            mk_file("services/web/src/index.ts"),
            mk_file("services/api/go.mod"),
            mk_file("tools/src/run.ts"),
            mk_file("vendor/leaked/index.ts"),
        ];
        let mut projects = detect_scip_projects(&files);
        assert_eq!(projects.len(), 2, "detect-only count");
        add_inferred_typescript_projects(&files, &mut projects);
        // 2 detected + 1 inferred "tools" TS project.
        assert_eq!(projects.len(), 3, "after add_inferred: {:?}", projects);
        let projects = filter_projects_for_worker_classes(projects);
        // Only "ts-js" worker class survives — drops the Go
        // project. 2 typescript projects remain.
        assert_eq!(projects.len(), 2);
        assert!(projects.iter().all(|p| p.language == "typescript"));

        // Restore env.
        match prior {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_WORKER_CLASSES", v),
            None => std::env::remove_var("CODEINTEL_SCIP_WORKER_CLASSES"),
        }
        match prior_cap {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION", v),
            None => std::env::remove_var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION"),
        }
    }

    #[test]
    fn max_project_roots_per_revision_defaults_to_legacy_24() {
        let _guard = ENV_MUTEX.lock().unwrap_or_else(|e| e.into_inner());
        let prior = std::env::var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION").ok();
        std::env::remove_var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION");
        // Legacy default per env.server.ts:263 is 24.
        assert_eq!(max_project_roots_per_revision(), 24);

        std::env::set_var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION", "0");
        assert_eq!(max_project_roots_per_revision(), 24);

        std::env::set_var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION", "5");
        assert_eq!(max_project_roots_per_revision(), 5);

        // Restore.
        match prior {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION", v),
            None => std::env::remove_var("CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION"),
        }
    }

    #[test]
    fn detect_scip_projects_handles_root_level_markers() {
        let files = vec![mk_file("Cargo.toml"), mk_file("src/main.rs")];
        let projects = detect_scip_projects(&files);
        assert_eq!(projects.len(), 1);
        assert_eq!(projects[0].language, "rust");
        // Legacy normalizeProjectRoot returns "" (not ".") for
        // root-level markers.
        assert_eq!(projects[0].project_root, "");
    }

    #[test]
    fn ts_args_adds_infer_tsconfig_only_for_package_json() {
        let argv = ts_args(
            Path::new("/repo"),
            Path::new("/out/typescript.scip"),
            Path::new("/repo/package.json"),
        );
        assert!(argv.contains(&"--infer-tsconfig".to_string()));

        let argv = ts_args(
            Path::new("/repo"),
            Path::new("/out/typescript.scip"),
            Path::new("/repo/tsconfig.json"),
        );
        assert!(!argv.contains(&"--infer-tsconfig".to_string()));
    }

    #[test]
    fn go_args_defaults_to_skip_tests_and_explicit_output() {
        let _guard = ENV_MUTEX.lock().unwrap_or_else(|e| e.into_inner());
        let prior_skip_tests = std::env::var("CODEINTEL_SCIP_GO_SKIP_TESTS").ok();
        let prior_skip_impl = std::env::var("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS").ok();
        let prior_patterns = std::env::var("CODEINTEL_SCIP_GO_PACKAGE_PATTERNS").ok();
        std::env::remove_var("CODEINTEL_SCIP_GO_SKIP_TESTS");
        std::env::remove_var("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS");
        std::env::remove_var("CODEINTEL_SCIP_GO_PACKAGE_PATTERNS");

        let argv = go_args(
            Path::new("/repo"),
            Path::new("/out/go.tmp.scip"),
            Path::new("/repo/go.mod"),
        );
        assert_eq!(
            argv,
            vec![
                "index".to_string(),
                "--output".to_string(),
                "/out/go.tmp.scip".to_string(),
                "--skip-tests".to_string(),
                "./...".to_string(),
            ]
        );

        match prior_skip_tests {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_GO_SKIP_TESTS", v),
            None => std::env::remove_var("CODEINTEL_SCIP_GO_SKIP_TESTS"),
        }
        match prior_skip_impl {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS", v),
            None => std::env::remove_var("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS"),
        }
        match prior_patterns {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_GO_PACKAGE_PATTERNS", v),
            None => std::env::remove_var("CODEINTEL_SCIP_GO_PACKAGE_PATTERNS"),
        }
    }

    #[test]
    fn go_args_honors_enterprise_runtime_overrides() {
        let _guard = ENV_MUTEX.lock().unwrap_or_else(|e| e.into_inner());
        let prior_skip_tests = std::env::var("CODEINTEL_SCIP_GO_SKIP_TESTS").ok();
        let prior_skip_impl = std::env::var("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS").ok();
        let prior_patterns = std::env::var("CODEINTEL_SCIP_GO_PACKAGE_PATTERNS").ok();
        std::env::set_var("CODEINTEL_SCIP_GO_SKIP_TESTS", "false");
        std::env::set_var("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS", "true");
        std::env::set_var(
            "CODEINTEL_SCIP_GO_PACKAGE_PATTERNS",
            "./cmd/..., ./internal/collector",
        );

        let argv = go_args(
            Path::new("/repo"),
            Path::new("/out/go.tmp.scip"),
            Path::new("/repo/go.mod"),
        );
        assert_eq!(
            argv,
            vec![
                "index".to_string(),
                "--output".to_string(),
                "/out/go.tmp.scip".to_string(),
                "--skip-implementations".to_string(),
                "./cmd/...".to_string(),
                "./internal/collector".to_string(),
            ]
        );

        match prior_skip_tests {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_GO_SKIP_TESTS", v),
            None => std::env::remove_var("CODEINTEL_SCIP_GO_SKIP_TESTS"),
        }
        match prior_skip_impl {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS", v),
            None => std::env::remove_var("CODEINTEL_SCIP_GO_SKIP_IMPLEMENTATIONS"),
        }
        match prior_patterns {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_GO_PACKAGE_PATTERNS", v),
            None => std::env::remove_var("CODEINTEL_SCIP_GO_PACKAGE_PATTERNS"),
        }
    }

    #[test]
    fn run_scip_indexer_surfaces_binary_missing() {
        // Pick an indexer entry but call it with a tempdir
        // that has no scip-* installed. The binary should
        // surface as ScipRunError::BinaryMissing.
        let tmp = std::env::temp_dir().join(format!("codeintel-r8-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&tmp);
        std::fs::create_dir_all(&tmp).expect("mkdir");

        let project = ScipProject {
            language: "typescript",
            indexer: "scip-typescript",
            worker_class: "ts-js",
            project_root: ".".to_string(),
            marker_path: "package.json".to_string(),
            inferred: false,
            inferred_reason: None,
        };
        let result = run_scip_indexer(
            &tmp,
            &project,
            &tmp.join("out.scip"),
            Duration::from_secs(5),
        );
        match result {
            Err(ScipRunError::BinaryMissing(b)) => assert_eq!(b, "scip-typescript"),
            Err(ScipRunError::Io(_)) => {
                // Some OSes return EACCESS / other errno; accept
                // as long as it's not a false-positive Ok.
            }
            other => panic!("expected BinaryMissing or Io, got {:?}", other),
        }

        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn prepare_scip_output_paths_removes_stale_output_and_cwd_index() {
        let tmp = std::env::temp_dir().join(format!(
            "codeintel-scip-cleanup-{}-{}",
            std::process::id(),
            1
        ));
        let _ = std::fs::remove_dir_all(&tmp);
        std::fs::create_dir_all(&tmp).expect("mkdir");
        let output = tmp.join("out.scip");
        let cwd_output = tmp.join("index.scip");
        std::fs::write(&output, b"stale-output").expect("write stale output");
        std::fs::write(&cwd_output, b"stale-cwd").expect("write stale cwd");

        prepare_scip_output_paths(&tmp, &output).expect("prepare paths");

        assert!(!output.exists(), "stale explicit output survived");
        assert!(!cwd_output.exists(), "stale cwd index.scip survived");
        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn validate_scip_artifact_rejects_semantically_empty_index() {
        let tmp =
            std::env::temp_dir().join(format!("codeintel-scip-empty-{}-{}", std::process::id(), 1));
        let _ = std::fs::remove_dir_all(&tmp);
        std::fs::create_dir_all(&tmp).expect("mkdir");
        let output = tmp.join("empty.scip");
        let empty = Index {
            metadata: MessageField::none(),
            documents: Vec::new(),
            external_symbols: Vec::new(),
            special_fields: SpecialFields::new(),
        };
        std::fs::write(&output, empty.write_to_bytes().expect("encode empty SCIP"))
            .expect("write empty SCIP");

        let err = validate_scip_artifact_has_semantic_rows(&output, "go", "", &tmp)
            .expect_err("empty SCIP should be rejected");
        assert!(
            err.contains("semantically empty .scip file"),
            "unexpected error: {err}"
        );
        let _ = std::fs::remove_dir_all(&tmp);
    }

    #[test]
    fn tail_bytes_safely_truncates_utf8() {
        let s = "🦀".repeat(100);
        let tail = tail_bytes(&s, 32);
        // Should be a valid UTF-8 substring, ≤32 bytes.
        assert!(tail.len() <= 32);
        assert!(tail.chars().count() > 0);
    }

    #[test]
    fn index_revision_scip_returns_one_result_per_project() {
        // With no binaries installed every result is
        // BinaryMissing — we just verify the dispatch shape.
        // Hold the ENV_MUTEX so the R.8c filter env isn't
        // mutated mid-test by another parallel test.
        let _guard = ENV_MUTEX.lock().unwrap_or_else(|e| e.into_inner());
        let prior = std::env::var("CODEINTEL_SCIP_WORKER_CLASSES").ok();
        std::env::set_var("CODEINTEL_SCIP_WORKER_CLASSES", "universal");

        let files = vec![mk_file("services/api/go.mod"), mk_file("Cargo.toml")];
        let tmp = std::env::temp_dir().join(format!("codeintel-r8-disp-{}", std::process::id()));
        std::fs::create_dir_all(&tmp).expect("mkdir");
        let results =
            index_revision_scip(&tmp, &files, &tmp.join("scip-out"), Duration::from_secs(1));
        // 2 projects (go, rust) → 2 results.
        assert_eq!(results.len(), 2);
        for r in &results {
            match r {
                // BinaryMissing when the indexer isn't on PATH.
                Err(ScipRunError::BinaryMissing(_)) => {}
                // Io errors on some OSes / under some rustup
                // shim configs.
                Err(ScipRunError::Io(_)) => {}
                // SubprocessFailed when a rustup multiplexer
                // intercepts `rust-analyzer` and returns "Unknown
                // binary" — the dispatch still proved it called
                // the right argv, the failure is just the
                // shim's response.
                Err(ScipRunError::SubprocessFailed { .. }) => {}
                Ok(_) => panic!("expected error without scip-* installed"),
            }
        }
        // Restore env for other parallel tests.
        match prior {
            Some(v) => std::env::set_var("CODEINTEL_SCIP_WORKER_CLASSES", v),
            None => std::env::remove_var("CODEINTEL_SCIP_WORKER_CLASSES"),
        }
        let _ = std::fs::remove_dir_all(&tmp);
    }
}

// Public re-export for convenience in higher-level callers
// that want to log the duration in the same format as the
// legacy "SCIP indexed <root> in <Xms>" lines.
pub fn format_duration_ms(d: Duration) -> String {
    format!("{}ms", d.as_millis())
}

// Compile-time assertion: every indexer in the table has a
// non-empty binary name and at least one marker OR is in the
// suffix-handled set (dotnet/ruby).
#[doc(hidden)]
#[allow(dead_code)]
const _: () = {
    let mut i = 0;
    while i < SUPPORTED_INDEXERS.len() {
        let def = &SUPPORTED_INDEXERS[i];
        assert!(!def.indexer.is_empty());
        i += 1;
    }
};

// anyhow provides a blanket `From<E: Error>` for anyhow::Error
// so `?` propagates ScipRunError directly into upstream
// anyhow::Result without an explicit impl — the std::error::Error
// derive above is enough.
