//! Per-revision manifest file enumeration. Direct port of
//! `buildManifestFilesForRevision` + its supporting heuristics
//! in `packages/backend/src/indexManifestManager.ts:301-403`.
//!
//! Given a repo working tree + a commit hash, this module:
//!   1. Walks the commit's tree via git2 (replacing the
//!      legacy `git ls-tree -r -z --long` shell-out).
//!   2. Classifies each file:
//!      - language (typescript / go / python / etc — same set
//!        the legacy returns).
//!      - project_root (nearest directory containing a
//!        package.json / go.mod / pyproject.toml / Cargo.toml /
//!        ... marker).
//!      - generated (dist/, build/, .min.js, .generated. etc).
//!      - vendor (node_modules/, vendor/, third_party/, .venv/).
//!      - test (__tests__/, *.test.X, *.spec.X).
//!   3. Returns the files sorted by path (matches legacy
//!      `sort((l,r) => l.path.localeCompare(r.path))`).
//!
//! This is the input to `delta_plan::IndexRunManifest.files`.
//! The next slice (R.6b) wires this output into the
//! IndexManifestManager DB writer.
//!
//! Why git2 and not shelling to `git ls-tree`: the codeintel
//! indexer runs in scratch containers without a git binary. The
//! libgit2 binding gives the same byte-equal blob OIDs as
//! `git ls-tree --long` (both pull from the same packfile
//! reader), so parity is preserved without a shell-out.

use anyhow::{Context, Result};
use git2::{ObjectType, Repository, TreeWalkMode, TreeWalkResult};
use std::collections::BTreeSet;
use std::path::{Component, Path, PathBuf};

use crate::delta_plan::IndexManifestFile;

/// FileEntry is the trimmed analog of the legacy
/// `GitTreeFileEntry` (git.ts:322-328). The legacy carries
/// `mode`, `objectType`, and optional `size` too, but
/// `buildManifestFilesForRevision` only reads `path` and
/// `objectHash`, so the port surface stops at the two fields
/// that are actually used.
#[derive(Debug, Clone)]
pub struct FileEntry {
    pub path: String,
    pub object_hash: String,
}

/// list_files_for_ref enumerates every blob under the given
/// commit's tree. The output mirrors the legacy
/// `listFilesForRef` filtering: only objectType=blob, no
/// subdirectories.
///
/// `ref_name` is anything git2 can `revparse_single` —
/// "refs/heads/main", "main", "HEAD", a full SHA, or any
/// abbreviation. Matches legacy `revparse("<ref>^{commit}")`
/// dereference behavior since `peel_to_commit` walks tag
/// chains automatically.
pub fn list_files_for_ref(repo_path: &Path, ref_name: &str) -> Result<Vec<FileEntry>> {
    let repo = Repository::open(repo_path)
        .with_context(|| format!("open {} for tree walk", repo_path.display()))?;
    let object = repo
        .revparse_single(ref_name)
        .with_context(|| format!("revparse {}", ref_name))?;
    let commit = object
        .peel_to_commit()
        .with_context(|| format!("peel {} to commit", ref_name))?;
    let tree = commit.tree().with_context(|| "load commit tree")?;

    let mut out: Vec<FileEntry> = Vec::new();
    tree.walk(TreeWalkMode::PreOrder, |dir, entry| {
        if let Some(ObjectType::Blob) = entry.kind() {
            // Legacy `git ls-tree -r -z` emits the raw bytes
            // of each filename. Git itself supports non-UTF-8
            // filenames; `entry.name()` returns None for those.
            // Use name_bytes() + lossy-UTF-8 so a non-UTF-8
            // filename surfaces as `?`-substituted string
            // rather than being silently dropped (legacy
            // parity: the file IS in the manifest, just with
            // a replacement-char path).
            let name_bytes = entry.name_bytes();
            if name_bytes.is_empty() {
                return TreeWalkResult::Ok;
            }
            let name = String::from_utf8_lossy(name_bytes);
            let full = if dir.is_empty() {
                name.into_owned()
            } else {
                format!("{}{}", dir, name)
            };
            out.push(FileEntry {
                path: full,
                object_hash: entry.id().to_string(),
            });
        }
        // Note: submodule entries (ObjectType::Commit) are
        // intentionally skipped — legacy `git ls-tree -r`
        // filters by `objectType === "blob"` (git.ts:346),
        // and submodule gitlinks have objectType "commit".
        TreeWalkResult::Ok
    })
    .with_context(|| "walk commit tree")?;
    Ok(out)
}

/// get_commit_hash_for_ref_name returns the fully dereferenced
/// commit SHA for a ref. Returns Ok(None) on the
/// empty-repository or non-existent-ref path the legacy
/// catches and returns `undefined` for (git.ts:316-319).
pub fn get_commit_hash_for_ref_name(repo_path: &Path, ref_name: &str) -> Result<Option<String>> {
    let repo = match Repository::open(repo_path) {
        Ok(r) => r,
        Err(_) => return Ok(None),
    };
    // Nest the borrows so they all drop before the outer
    // `repo` goes out of scope — git2's lifetime chain
    // (Repository -> Object -> Commit) requires this.
    let id = {
        let object = match repo.revparse_single(ref_name) {
            Ok(o) => o,
            Err(_) => return Ok(None),
        };
        match object.peel_to_commit() {
            Ok(c) => c.id().to_string(),
            Err(_) => return Ok(None),
        }
    };
    drop(repo);
    Ok(Some(id))
}

/// build_manifest_files_for_revision is the direct port of
/// indexManifestManager.ts:301-328.
///
/// For each blob in the commit's tree, normalizes the path,
/// runs heuristics, and emits an IndexManifestFile. The output
/// is sorted by path to match the legacy
/// `.sort((l, r) => l.path.localeCompare(r.path))`.
pub fn build_manifest_files_for_revision(
    repo_path: &Path,
    commit_hash: &str,
) -> Result<Vec<IndexManifestFile>> {
    let entries = list_files_for_ref(repo_path, commit_hash)?;
    let paths: Vec<String> = entries
        .iter()
        .map(|e| normalize_manifest_path(&e.path))
        .collect();
    let project_roots = detect_project_roots(&paths);

    let mut out: Vec<IndexManifestFile> = entries
        .iter()
        .map(|entry| {
            let path = normalize_manifest_path(&entry.path);
            IndexManifestFile {
                content_hash: entry.object_hash.clone(),
                language: infer_language(&path),
                project_root: Some(nearest_project_root(&path, &project_roots)),
                generated: Some(is_generated_path(&path)),
                vendor: Some(is_vendor_path(&path)),
                test: Some(is_test_path(&path)),
                artifacts: None,
                path,
            }
        })
        .collect();
    out.sort_by(|l, r| l.path.cmp(&r.path));
    Ok(out)
}

/// materialize_revision_tree writes the exact commit tree to a
/// separate read-only snapshot directory. SCIP and AST extraction need
/// a real filesystem, while manifests can read git objects directly;
/// this bridges the two without mutating the shared checkout between
/// branch iterations.
pub fn materialize_revision_tree(
    repo_path: &Path,
    commit_hash: &str,
    output_dir: &Path,
) -> Result<()> {
    if output_dir.exists() {
        std::fs::remove_dir_all(output_dir)
            .with_context(|| format!("remove stale revision snapshot {}", output_dir.display()))?;
    }
    std::fs::create_dir_all(output_dir)
        .with_context(|| format!("mkdir revision snapshot {}", output_dir.display()))?;

    let repo = Repository::open(repo_path)
        .with_context(|| format!("open {} for tree materialization", repo_path.display()))?;
    let files = list_files_for_ref(repo_path, commit_hash)?;
    for file in files {
        let oid = git2::Oid::from_str(&file.object_hash)
            .with_context(|| format!("parse blob oid {} for {}", file.object_hash, file.path))?;
        let blob = repo
            .find_blob(oid)
            .with_context(|| format!("load blob {} for {}", file.object_hash, file.path))?;
        let target = safe_materialized_path(output_dir, &file.path)?;
        if let Some(parent) = target.parent() {
            std::fs::create_dir_all(parent)
                .with_context(|| format!("mkdir parent {}", parent.display()))?;
        }
        std::fs::write(&target, blob.content())
            .with_context(|| format!("write materialized file {}", target.display()))?;
    }
    Ok(())
}

fn safe_materialized_path(root: &Path, repo_path: &str) -> Result<PathBuf> {
    let rel = Path::new(repo_path);
    if rel.is_absolute() {
        return Err(anyhow::anyhow!("absolute path {} in git tree", repo_path));
    }
    let mut out = root.to_path_buf();
    for component in rel.components() {
        match component {
            Component::Normal(part) => out.push(part),
            Component::CurDir => {}
            Component::ParentDir | Component::RootDir | Component::Prefix(_) => {
                return Err(anyhow::anyhow!("unsafe path {} in git tree", repo_path));
            }
        }
    }
    Ok(out)
}

/// provider_connection_scope mirrors
/// indexManifestManager.ts:330-335. Sorts the connection IDs
/// numerically and joins with commas; returns None if no
/// connections (legacy returns `undefined`).
pub fn provider_connection_scope(connection_ids: &[i32]) -> Option<String> {
    if connection_ids.is_empty() {
        return None;
    }
    let mut sorted: Vec<i32> = connection_ids.to_vec();
    sorted.sort();
    Some(
        sorted
            .iter()
            .map(|id| id.to_string())
            .collect::<Vec<_>>()
            .join(","),
    )
}

/// normalize_manifest_path mirrors the helper in
/// indexManifestManager.ts:337 (same shape as the
/// delta_plan-private helper but exposed here so the file-
/// listing path can reuse it without crossing the module
/// boundary in a fragile way).
fn normalize_manifest_path(value: &str) -> String {
    let forward = value.replace('\\', "/");
    let trimmed = forward.trim_start_matches('/');
    trimmed.to_string()
}

const ROOT_MARKERS: &[&str] = &[
    "package.json",
    "tsconfig.json",
    "jsconfig.json",
    "go.mod",
    "pyproject.toml",
    "setup.py",
    "requirements.txt",
    "pom.xml",
    "build.gradle",
    "build.sbt",
    "compile_commands.json",
    "Cargo.toml",
    "Gemfile",
    "pubspec.yaml",
];

const ROOT_MARKER_SUFFIXES: &[&str] = &[".csproj", ".vbproj", ".fsproj", ".sln", ".gemspec"];

/// detect_project_roots scans the file list for marker files
/// (package.json, go.mod, Cargo.toml, ...) and returns the
/// set of containing directories. The list is sorted by
/// descending length so `nearest_project_root` can do a
/// first-match lookup (longest-prefix wins).
///
/// Always includes "." so files outside any marker still
/// resolve to a project root.
///
/// Parity note: legacy uses `new Set<string>([".",...])` which
/// preserves *insertion order*, then `.sort((l,r) => r.length
/// - l.length)` which is stable in ES2019. So equal-length
/// roots emit in iteration-of-input order, with "." first.
/// We mirror that with an insertion-ordered Vec + a stable
/// length-only sort.
fn detect_project_roots(paths: &[String]) -> Vec<String> {
    let mut roots: Vec<String> = Vec::with_capacity(8);
    roots.push(".".to_string());
    let mut seen: BTreeSet<String> = BTreeSet::new();
    seen.insert(".".to_string());
    for path in paths {
        let basename = match path.rsplit_once('/') {
            Some((_, last)) => last,
            None => path.as_str(),
        };
        let is_marker = ROOT_MARKERS.iter().any(|m| *m == basename)
            || ROOT_MARKER_SUFFIXES
                .iter()
                .any(|suffix| basename.ends_with(*suffix));
        if is_marker {
            let dir = dirname_or_dot(path);
            if seen.insert(dir.clone()) {
                roots.push(dir);
            }
        }
    }
    // Stable sort by descending length — ties keep insertion
    // order to match the legacy Array.sort ES2019 stability.
    roots.sort_by(|a, b| b.len().cmp(&a.len()));
    roots
}

fn nearest_project_root(file_path: &str, project_roots: &[String]) -> String {
    for root in project_roots {
        if root == "." || file_path == root || file_path.starts_with(&format!("{}/", root)) {
            return root.clone();
        }
    }
    ".".to_string()
}

fn dirname_or_dot(file_path: &str) -> String {
    match file_path.rfind('/') {
        Some(idx) => file_path[..idx].to_string(),
        None => ".".to_string(),
    }
}

/// infer_language mirrors indexManifestManager.ts:375-389.
/// Returns the legacy bucket name verbatim
/// ("typescript"/"go"/"python"/"jvm"/"cpp"/"dotnet"/"ruby"/
/// "rust"/"dart"/"docs"/"config"), or None for unrecognized
/// extensions.
fn infer_language(file_path: &str) -> Option<String> {
    let ext_raw = match file_path.rfind('.') {
        Some(idx) => &file_path[idx..],
        None => return None,
    };
    let ext = ext_raw.to_ascii_lowercase();
    match ext.as_str() {
        ".ts" | ".tsx" | ".js" | ".jsx" | ".mjs" | ".cjs" => Some("typescript".to_string()),
        ".go" => Some("go".to_string()),
        ".py" => Some("python".to_string()),
        ".java" | ".kt" | ".scala" => Some("jvm".to_string()),
        ".c" | ".cc" | ".cpp" | ".cxx" | ".h" | ".hpp" | ".hh" => Some("cpp".to_string()),
        ".cs" | ".vb" | ".fs" => Some("dotnet".to_string()),
        ".rb" => Some("ruby".to_string()),
        ".rs" => Some("rust".to_string()),
        ".dart" => Some("dart".to_string()),
        ".md" | ".mdx" | ".rst" | ".txt" => Some("docs".to_string()),
        ".json" | ".yaml" | ".yml" | ".toml" | ".xml" | ".gradle" | ".sbt" => {
            Some("config".to_string())
        }
        _ => None,
    }
}

/// is_generated_path mirrors indexManifestManager.ts:391-395.
fn is_generated_path(file_path: &str) -> bool {
    const GENERATED_DIRS: &[&str] = &["dist", "build", "coverage", "target", "generated", "gen"];
    for d in GENERATED_DIRS {
        if has_dir_segment(file_path, d) {
            return true;
        }
    }
    // "vendor/bundle" is the Ruby gems bundle dir — two-segment
    // match, not single-segment vendor.
    if has_dir_path_prefix(file_path, "vendor/bundle") {
        return true;
    }
    if file_path.contains(".generated.") {
        return true;
    }
    if file_path.ends_with(".min.js") {
        return true;
    }
    false
}

/// is_vendor_path mirrors indexManifestManager.ts:397.
fn is_vendor_path(file_path: &str) -> bool {
    const VENDOR_DIRS: &[&str] = &["node_modules", "vendor", "third_party", ".venv", "venv"];
    VENDOR_DIRS.iter().any(|d| has_dir_segment(file_path, d))
}

/// is_test_path mirrors indexManifestManager.ts:399-402.
fn is_test_path(file_path: &str) -> bool {
    const TEST_DIRS: &[&str] = &["__tests__", "test", "tests", "spec"];
    if TEST_DIRS.iter().any(|d| has_dir_segment(file_path, d)) {
        return true;
    }
    // *.test.X or *.spec.X where X has no further dots (the
    // [^.]+$ in the legacy regex).
    let basename = match file_path.rsplit_once('/') {
        Some((_, last)) => last,
        None => file_path,
    };
    let parts: Vec<&str> = basename.split('.').collect();
    if parts.len() >= 3 {
        let suffix = parts[parts.len() - 1];
        let category = parts[parts.len() - 2];
        if (category == "test" || category == "spec") && !suffix.is_empty() {
            return true;
        }
    }
    false
}

/// has_dir_segment returns true if `segment` appears as a
/// directory component (i.e. surrounded by `/` or at the
/// start) in `file_path`. Mirrors the regex
/// `(^|\/)<segment>\/`.
fn has_dir_segment(file_path: &str, segment: &str) -> bool {
    let lead = format!("{}/", segment);
    let mid = format!("/{}/", segment);
    file_path.starts_with(&lead) || file_path.contains(&mid)
}

/// has_dir_path_prefix is the same as has_dir_segment but
/// for a multi-segment prefix like `vendor/bundle`.
fn has_dir_path_prefix(file_path: &str, prefix: &str) -> bool {
    let lead = format!("{}/", prefix);
    let mid = format!("/{}/", prefix);
    file_path.starts_with(&lead) || file_path.contains(&mid)
}

#[cfg(test)]
mod tests {
    //! Mirrors the heuristic assertions implicit in
    //! `indexManifestManager.test.ts` (which exercises
    //! `prepareRevisionManifests` end-to-end). The Rust port
    //! also adds standalone tests for each heuristic so a
    //! regression in (say) `infer_language` doesn't have to be
    //! tracked down through the integration test.

    use super::*;

    #[test]
    fn infer_language_covers_full_legacy_set() {
        // Every extension the legacy regex enumerates must
        // produce its documented bucket. A regression that
        // drops one alternative from the match arm would be
        // caught here.
        let cases: &[(&str, Option<&str>)] = &[
            ("src/foo.ts", Some("typescript")),
            ("src/foo.tsx", Some("typescript")),
            ("a.js", Some("typescript")),
            ("a.jsx", Some("typescript")),
            ("a.mjs", Some("typescript")),
            ("a.cjs", Some("typescript")),
            ("main.go", Some("go")),
            ("pkg/foo.py", Some("python")),
            ("src/Foo.java", Some("jvm")),
            ("Bar.kt", Some("jvm")),
            ("baz.scala", Some("jvm")),
            ("a.c", Some("cpp")),
            ("a.cc", Some("cpp")),
            ("a.cpp", Some("cpp")),
            ("a.cxx", Some("cpp")),
            ("a.h", Some("cpp")),
            ("a.hpp", Some("cpp")),
            ("a.hh", Some("cpp")),
            ("Foo.cs", Some("dotnet")),
            ("Foo.vb", Some("dotnet")),
            ("Mod.fs", Some("dotnet")),
            ("a.rb", Some("ruby")),
            ("src/lib.rs", Some("rust")),
            ("main.dart", Some("dart")),
            ("README.md", Some("docs")),
            ("doc.mdx", Some("docs")),
            ("doc.rst", Some("docs")),
            ("note.txt", Some("docs")),
            ("pkg.json", Some("config")),
            ("app.yaml", Some("config")),
            ("app.yml", Some("config")),
            ("Cargo.toml", Some("config")),
            ("conf.xml", Some("config")),
            ("build.gradle", Some("config")),
            ("build.sbt", Some("config")),
            ("README", None),
            ("Makefile", None),
            // Case-insensitive — the legacy lower-cases first.
            ("FOO.TS", Some("typescript")),
            ("FOO.JSON", Some("config")),
        ];
        for (path, expected) in cases {
            assert_eq!(
                infer_language(path).as_deref(),
                *expected,
                "infer_language({:?})",
                path
            );
        }
    }

    #[test]
    fn is_generated_detects_dist_build_minjs_dot_generated() {
        // Every legacy generated-dir bucket exercised.
        assert!(is_generated_path("dist/foo.js"));
        assert!(is_generated_path("build/x.go"));
        assert!(is_generated_path("packages/web/build/output.js"));
        assert!(is_generated_path("coverage/lcov.info"));
        assert!(is_generated_path("target/debug/x"));
        assert!(is_generated_path("src/generated/foo.rs"));
        assert!(is_generated_path("gen/proto.go"));
        assert!(is_generated_path("src/foo.generated.ts"));
        assert!(is_generated_path("lib/foo.min.js"));
        assert!(is_generated_path("vendor/bundle/gems/x.rb"));
        assert!(!is_generated_path("src/foo.ts"));
        assert!(!is_generated_path("vendor/foo.ts")); // vendor alone is NOT generated
    }

    #[test]
    fn is_vendor_detects_node_modules_vendor_third_party() {
        assert!(is_vendor_path("node_modules/react/index.js"));
        assert!(is_vendor_path("packages/web/node_modules/x"));
        assert!(is_vendor_path("vendor/foo.go"));
        assert!(is_vendor_path("third_party/lib/x.cpp"));
        assert!(is_vendor_path(".venv/lib/python3.10/site-packages/foo.py"));
        assert!(is_vendor_path("venv/foo.py"));
        assert!(!is_vendor_path("src/foo.ts"));
    }

    #[test]
    fn is_test_detects_directory_and_suffix_patterns() {
        assert!(is_test_path("__tests__/foo.ts"));
        assert!(is_test_path("packages/web/__tests__/bar.ts"));
        assert!(is_test_path("test/foo.go"));
        assert!(is_test_path("tests/foo.py"));
        assert!(is_test_path("spec/foo.rb"));
        assert!(is_test_path("src/foo.test.ts"));
        assert!(is_test_path("src/foo.spec.tsx"));
        assert!(!is_test_path("src/foo.ts"));
        // .test. or .spec. in middle but with another dot after — NOT a test file
        // (the legacy regex requires `\.(test|spec)\.[^.]+$`)
        assert!(!is_test_path("src/foo.test.config.ts")); // ends with .ts but second-to-last is "config" not "test"
        assert!(!is_test_path("test.ts")); // not enough dots
    }

    #[test]
    fn detect_project_roots_picks_marker_dirs_sorted_by_length() {
        let paths = vec![
            "package.json".to_string(),
            "services/api/go.mod".to_string(),
            "services/api/main.go".to_string(),
            "tools/cli/Cargo.toml".to_string(),
            "tools/cli/src/main.rs".to_string(),
            "src/foo.ts".to_string(),
        ];
        let roots = detect_project_roots(&paths);
        // Should include all detected roots + ".", sorted by length desc.
        assert!(roots.contains(&".".to_string()));
        assert!(roots.contains(&"services/api".to_string()));
        assert!(roots.contains(&"tools/cli".to_string()));
        // Verify descending-length order
        let lens: Vec<usize> = roots.iter().map(|r| r.len()).collect();
        for w in lens.windows(2) {
            assert!(w[0] >= w[1], "roots not sorted desc-by-len: {:?}", roots);
        }
    }

    #[test]
    fn detect_project_roots_supports_dotnet_suffixes() {
        let paths = vec![
            "app/MyApp.csproj".to_string(),
            "app/Program.cs".to_string(),
            "tests/Foo.gemspec".to_string(),
        ];
        let roots = detect_project_roots(&paths);
        assert!(roots.contains(&"app".to_string()));
        assert!(roots.contains(&"tests".to_string()));
    }

    #[test]
    fn nearest_project_root_picks_longest_prefix() {
        let roots = vec![
            "services/api".to_string(),
            "services".to_string(),
            ".".to_string(),
        ];
        let mut sorted = roots.clone();
        sorted.sort_by(|a, b| b.len().cmp(&a.len()));
        assert_eq!(
            nearest_project_root("services/api/main.go", &sorted),
            "services/api"
        );
        assert_eq!(
            nearest_project_root("services/other/x.go", &sorted),
            "services"
        );
        assert_eq!(nearest_project_root("README.md", &sorted), ".");
    }

    #[test]
    fn provider_connection_scope_joins_sorted_ids() {
        assert_eq!(provider_connection_scope(&[]), None);
        assert_eq!(provider_connection_scope(&[10]), Some("10".to_string()));
        assert_eq!(
            provider_connection_scope(&[30, 10, 20]),
            Some("10,20,30".to_string())
        );
        // Legacy joins numbers as strings; "11" sorts AFTER "10" numerically (10,11,2) → no.
        // But the legacy uses .sort((l,r) => l - r) which is numeric.
        // Our Rust impl also sorts numerically. Verify:
        assert_eq!(
            provider_connection_scope(&[11, 2, 10]),
            Some("2,10,11".to_string())
        );
    }

    #[test]
    fn dirname_or_dot_handles_root_and_nested() {
        assert_eq!(dirname_or_dot("foo.ts"), ".");
        assert_eq!(dirname_or_dot("src/foo.ts"), "src");
        assert_eq!(dirname_or_dot("services/api/main.go"), "services/api");
    }

    #[test]
    fn build_manifest_files_against_real_git_repo() {
        use git2::{Repository, Signature};
        use std::fs;

        let tmp = std::env::temp_dir().join(format!("codeintel-r6a-{}", std::process::id()));
        let _ = fs::remove_dir_all(&tmp);
        fs::create_dir_all(&tmp).expect("mkdir tmp");

        let repo = Repository::init(&tmp).expect("init repo");
        let sig = Signature::now("test", "test@example.com").expect("sig");

        // Lay down a polyglot tree.
        let files: &[(&str, &str)] = &[
            ("package.json", "{}\n"),
            ("src/orders.ts", "export const x = 1;\n"),
            ("src/orders.test.ts", "test('x', () => {});\n"),
            ("services/api/go.mod", "module api\n"),
            ("services/api/main.go", "package main\n"),
            ("node_modules/react/index.js", "module.exports={};\n"),
            ("dist/bundle.js", "console.log(0);\n"),
            ("docs/intro.md", "# intro\n"),
            ("Cargo.toml", "[package]\nname=\"x\"\n"),
        ];
        for (rel, body) in files {
            let abs = tmp.join(rel);
            if let Some(parent) = abs.parent() {
                fs::create_dir_all(parent).expect("mkdir parent");
            }
            fs::write(abs, body).expect("write file");
        }

        let mut index = repo.index().expect("index");
        for (rel, _) in files {
            index.add_path(std::path::Path::new(rel)).expect("add");
        }
        index.write().expect("index write");
        let tree_oid = index.write_tree().expect("write tree");
        let tree = repo.find_tree(tree_oid).expect("tree");
        let commit_oid = repo
            .commit(Some("HEAD"), &sig, &sig, "init", &tree, &[])
            .expect("commit");

        let manifest =
            build_manifest_files_for_revision(&tmp, &commit_oid.to_string()).expect("build");
        assert_eq!(manifest.len(), files.len(), "all files enumerated");

        // Output must be sorted by path.
        let mut prev = "".to_string();
        for f in &manifest {
            assert!(f.path > prev, "not sorted at {}", f.path);
            prev = f.path.clone();
        }

        // Spot-check classifications.
        let by_path = |needle: &str| manifest.iter().find(|f| f.path == needle).cloned();
        let orders_ts = by_path("src/orders.ts").expect("orders.ts present");
        assert_eq!(orders_ts.language.as_deref(), Some("typescript"));
        assert_eq!(orders_ts.project_root.as_deref(), Some("."));
        assert_eq!(orders_ts.test, Some(false));
        assert_eq!(orders_ts.generated, Some(false));
        assert_eq!(orders_ts.vendor, Some(false));

        let orders_test = by_path("src/orders.test.ts").expect("orders.test.ts present");
        assert_eq!(orders_test.test, Some(true));

        let api_main = by_path("services/api/main.go").expect("main.go present");
        assert_eq!(api_main.language.as_deref(), Some("go"));
        assert_eq!(api_main.project_root.as_deref(), Some("services/api"));

        let node_module = by_path("node_modules/react/index.js").expect("react present");
        assert_eq!(node_module.vendor, Some(true));

        let bundle = by_path("dist/bundle.js").expect("bundle.js present");
        assert_eq!(bundle.generated, Some(true));

        // contentHash must be a real 40-hex git blob OID.
        for f in &manifest {
            assert_eq!(f.content_hash.len(), 40, "{} has odd-length OID", f.path);
            assert!(
                f.content_hash.chars().all(|c| c.is_ascii_hexdigit()),
                "{} OID not hex",
                f.path
            );
        }

        // get_commit_hash_for_ref_name resolves the commit.
        let resolved = get_commit_hash_for_ref_name(&tmp, "HEAD")
            .expect("resolve")
            .expect("Some commit");
        assert_eq!(resolved, commit_oid.to_string());

        // Non-existent ref → Ok(None).
        let missing =
            get_commit_hash_for_ref_name(&tmp, "refs/heads/does-not-exist").expect("call");
        assert_eq!(missing, None);

        let _ = fs::remove_dir_all(&tmp);
    }

    #[test]
    fn materialize_revision_tree_writes_exact_commit_content() {
        use git2::{Repository, Signature};
        use std::fs;

        let tmp =
            std::env::temp_dir().join(format!("codeintel-r6a-materialize-{}", std::process::id()));
        let _ = fs::remove_dir_all(&tmp);
        fs::create_dir_all(&tmp).expect("mkdir tmp");

        let repo_path = tmp.join("repo");
        fs::create_dir_all(&repo_path).expect("mkdir repo");
        let repo = Repository::init(&repo_path).expect("init repo");
        let sig = Signature::now("test", "test@example.com").expect("sig");

        fs::create_dir_all(repo_path.join("src")).expect("mkdir src");
        fs::write(repo_path.join("src/value.txt"), "main\n").expect("write main");
        let mut index = repo.index().expect("index");
        index
            .add_path(Path::new("src/value.txt"))
            .expect("add main");
        index.write().expect("index write");
        let tree_oid = index.write_tree().expect("tree main");
        let tree = repo.find_tree(tree_oid).expect("find main tree");
        let main_commit = repo
            .commit(Some("HEAD"), &sig, &sig, "main", &tree, &[])
            .expect("commit main");
        drop(tree);

        fs::write(repo_path.join("src/value.txt"), "feature\n").expect("write feature");
        let mut index = repo.index().expect("index2");
        index
            .add_path(Path::new("src/value.txt"))
            .expect("add feature");
        index.write().expect("index2 write");
        let tree_oid = index.write_tree().expect("tree feature");
        let tree = repo.find_tree(tree_oid).expect("find feature tree");
        let feature_commit = repo
            .commit(
                Some("HEAD"),
                &sig,
                &sig,
                "feature",
                &tree,
                &[&repo.find_commit(main_commit).expect("find parent")],
            )
            .expect("commit feature");
        drop(tree);

        let main_out = tmp.join("snap-main");
        let feature_out = tmp.join("snap-feature");
        materialize_revision_tree(&repo_path, &main_commit.to_string(), &main_out)
            .expect("materialize main");
        materialize_revision_tree(&repo_path, &feature_commit.to_string(), &feature_out)
            .expect("materialize feature");

        assert_eq!(
            fs::read_to_string(main_out.join("src/value.txt")).expect("read main"),
            "main\n"
        );
        assert_eq!(
            fs::read_to_string(feature_out.join("src/value.txt")).expect("read feature"),
            "feature\n"
        );

        let _ = fs::remove_dir_all(&tmp);
    }
}
