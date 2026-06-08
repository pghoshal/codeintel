//! Zoekt shell-out — invokes the vendored `zoekt-git-index`
//! binary against a freshly-cloned working tree. Direct port
//! of `packages/backend/src/zoekt.ts:indexGitRepository` with
//! Q.C quality-overhaul additions.
//!
//! Flag shape:
//!
//!   zoekt-git-index \
//!     -allow_missing_branches \
//!     -index <IndexDir> \
//!     -max_trigram_count <N> \
//!     -file_limit <N> \
//!     -branches HEAD \
//!     -prefix refs/remotes/origin/ \
//!     -tenant_id <orgId> \
//!     -repo_id <repoId> \
//!     -shard_prefix <orgId>_<repoId> \
//!     [-language_map <map>] \
//!     [-require_ctags] \
//!     [-delta -delta_threshold <N>] \
//!     [-ignore_dir <dir>]... \
//!     [-large_file <glob>]... \
//!     <RepoPath>
//!
//! Q.C additions vs legacy: `-language_map` ensures ctags
//! processing routes to the right indexer per language;
//! `-require_ctags` is opt-in via env for deployments that
//! guarantee a ctags binary on PATH. Vanilla zoekt has
//! ctags ENABLED by default (we never pass `-disable_ctags`)
//! so the symbol index is populated for every shard.
//!
//! Q.C addition: our vendored zoekt-git-index exposes
//! `-ignore_dir`, merged with `.sourcegraph/ignore` while
//! walking git trees. That preserves commit hashes while
//! keeping generated and vendored directories out of the shard.
//!
//! Why shell out (not link libzoekt as a Rust dep): zoekt is a
//! mature Go binary with its own argv contract. Re-implementing
//! its shard-writer in Rust would re-derive the shard binary
//! format from scratch — months of work — and break byte-equal
//! parity with the legacy. The shell-out keeps the on-disk
//! `.zoekt` format identical to what the legacy produced.

use anyhow::{anyhow, Context, Result};
use std::path::{Path, PathBuf};
use std::process::Command;

/// Defaults match the legacy `Settings` shape from
/// packages/backend/src/types.ts. Zero values fall through to
/// these on a fresh deployment.
pub const DEFAULT_MAX_TRIGRAM_COUNT: u32 = 20_000;
pub const DEFAULT_FILE_LIMIT_BYTES: u32 = 2_097_152; // 2 MiB

/// IndexRequest is the input shape. Mirrors the Go
/// `pkg/zoektindexer.IndexRequest` that was sketched in the
/// earlier (reverted) C.4b draft — same field names so any
/// caller bridging the two ecosystems doesn't translate.
#[derive(Debug, Clone)]
pub struct IndexRequest<'a> {
    /// Path to the cloned working tree (must contain .git/).
    pub repo_path: &'a Path,
    /// Directory to write `.zoekt` shards into.
    pub index_dir: &'a Path,
    /// `<orgId>_<repoId>` prefix on every shard filename.
    pub shard_prefix: &'a str,
    /// Org id (zoekt embeds it in shard metadata).
    pub tenant_id: i32,
    /// Repo id (zoekt embeds it in shard metadata).
    pub repo_id: i32,
    /// Branches to index. Empty defaults to `["HEAD"]` — matches
    /// the legacy default in `repoIndexManager.ts`.
    pub branches: &'a [String],
    /// Trigram cap. 0 -> default.
    pub max_trigram_count: u32,
    /// File-size cap (bytes). 0 -> default.
    pub file_limit_bytes: u32,
    /// Always-index glob patterns. Empty if not configured.
    pub large_file_patterns: &'a [String],
    /// Q.C — language→ctags-processor map for the
    /// `-language_map` flag. Empty string leaves the flag off
    /// so zoekt uses its built-in default routing. Format:
    /// "lang1:N,lang2:N,..." per upstream zoekt convention.
    pub language_map: &'a str,
    /// Q.C — when true, pass `-require_ctags` so indexing fails
    /// if the ctags binary is missing. Deployments that
    /// guarantee a ctags-bearing image should set this; default
    /// (false) tolerates missing ctags by silently dropping the
    /// symbol section.
    pub require_ctags: bool,
    /// Q.C — runtime directory/glob ignores passed through to
    /// the vendored `zoekt-git-index -ignore_dir` flag. These
    /// are merged with repo-local `.sourcegraph/ignore` and do
    /// not mutate the checkout or commit tree.
    pub ignore_dirs: &'a [&'a str],
    /// When true, request Zoekt's native file-level delta shard
    /// build. Upstream tombstones changed/deleted files in older
    /// shards and stacks a delta shard for changed files; if the
    /// existing shard set is incompatible, zoekt-git-index falls
    /// back to a normal build.
    pub use_delta: bool,
    /// Existing shard-count threshold before upstream falls back
    /// to a normal build. Sourcegraph defaults this to 150.
    pub delta_shard_number_fallback_threshold: u64,
}

#[derive(Debug, Clone)]
pub struct IndexResult {
    /// `.zoekt` files produced by this invocation (matched by
    /// the shard_prefix filter in IndexDir before vs. after).
    pub shard_paths: Vec<PathBuf>,
    pub stdout: String,
    pub stderr: String,
}

/// run invokes zoekt-git-index at `binary_path` against
/// `req.repo_path`. Returns the list of newly-produced shard
/// paths.
///
/// Errors carry the binary's stderr verbatim — operators
/// debugging a stuck index need the upstream tool's diagnostic,
/// not our wrapper's paraphrase.
pub fn run(binary_path: &Path, req: IndexRequest<'_>) -> Result<IndexResult> {
    validate(&req)?;

    let max_trigram = if req.max_trigram_count == 0 {
        DEFAULT_MAX_TRIGRAM_COUNT
    } else {
        req.max_trigram_count
    };
    let file_limit = if req.file_limit_bytes == 0 {
        DEFAULT_FILE_LIMIT_BYTES
    } else {
        req.file_limit_bytes
    };
    let branches: Vec<String> = if req.branches.is_empty() {
        vec!["HEAD".to_string()]
    } else {
        req.branches
            .iter()
            .map(|branch| zoekt_branch_arg(branch))
            .collect()
    };

    let pre_existing =
        list_shards(req.index_dir, req.shard_prefix).with_context(|| "snapshot existing shards")?;

    let mut cmd = Command::new(binary_path);
    cmd.arg("-allow_missing_branches")
        .args([
            "-index",
            req.index_dir
                .to_str()
                .ok_or_else(|| anyhow!("index_dir not utf-8"))?,
        ])
        .args(["-max_trigram_count", &max_trigram.to_string()])
        .args(["-file_limit", &file_limit.to_string()])
        .args(["-branches", &branches.join(",")])
        .args(["-prefix", "refs/remotes/origin/"])
        .args(["-tenant_id", &req.tenant_id.to_string()])
        .args(["-repo_id", &req.repo_id.to_string()])
        .args(["-shard_prefix", req.shard_prefix]);
    // Q.C — emit -language_map when caller supplied a mapping
    // (empty string = use zoekt's built-in defaults).
    if !req.language_map.is_empty() {
        cmd.args(["-language_map", req.language_map]);
    }
    // Q.C — opt-in strict-ctags. When set, the indexer fails
    // hard if the ctags binary is missing. Default (false)
    // tolerates missing ctags by skipping the symbol section.
    // ctags is ENABLED by default — we never pass
    // `-disable_ctags`, so symbol-aware ranking is on for every
    // shard whenever the ctags binary is present.
    if req.require_ctags {
        cmd.arg("-require_ctags");
    }
    if req.use_delta {
        cmd.arg("-delta").args([
            "-delta_threshold",
            &req.delta_shard_number_fallback_threshold.to_string(),
        ]);
    }
    for dir in req.ignore_dirs {
        cmd.args(["-ignore_dir", *dir]);
    }
    for pat in req.large_file_patterns {
        cmd.args(["-large_file", pat.as_str()]);
    }
    cmd.arg(
        req.repo_path
            .to_str()
            .ok_or_else(|| anyhow!("repo_path not utf-8"))?,
    );

    // Capture both streams so failures surface the real
    // upstream error.
    let output = cmd
        .output()
        .with_context(|| format!("spawn {}", binary_path.display()))?;
    let stdout = String::from_utf8_lossy(&output.stdout).to_string();
    let stderr = String::from_utf8_lossy(&output.stderr).to_string();
    if !output.status.success() {
        return Err(anyhow!(
            "zoekt-git-index exit={:?} stderr={}",
            output.status,
            stderr
        ));
    }

    let post_existing =
        list_shards(req.index_dir, req.shard_prefix).with_context(|| "snapshot post shards")?;
    let mut new_shards: Vec<PathBuf> = Vec::new();
    for path in post_existing {
        if !pre_existing.contains(&path) {
            new_shards.push(path);
        }
    }
    Ok(IndexResult {
        shard_paths: new_shards,
        stdout,
        stderr,
    })
}

fn validate(req: &IndexRequest<'_>) -> Result<()> {
    if req.shard_prefix.is_empty() {
        return Err(anyhow!("empty shard_prefix"));
    }
    if req.tenant_id <= 0 {
        return Err(anyhow!("tenant_id must be positive"));
    }
    if req.repo_id <= 0 {
        return Err(anyhow!("repo_id must be positive"));
    }
    if !req.repo_path.join(".git").exists() {
        return Err(anyhow!(
            "repo_path {} missing .git",
            req.repo_path.display()
        ));
    }
    if !req.index_dir.exists() {
        std::fs::create_dir_all(req.index_dir)
            .with_context(|| format!("mkdir index_dir {}", req.index_dir.display()))?;
    }
    Ok(())
}

/// list_shards returns the set of files in `dir` whose name
/// starts with `prefix` and ends in `.zoekt`. The pre/post
/// snapshots use this to identify which shards a single
/// invocation produced.
fn list_shards(dir: &Path, prefix: &str) -> Result<Vec<PathBuf>> {
    let mut out = Vec::new();
    let entries = match std::fs::read_dir(dir) {
        Ok(e) => e,
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Ok(out),
        Err(e) => return Err(anyhow!("readdir {}: {}", dir.display(), e)),
    };
    for entry in entries {
        let entry = entry?;
        let name = entry.file_name();
        let name_str = name.to_string_lossy();
        if !name_str.ends_with(".zoekt") {
            continue;
        }
        if !name_str.starts_with(prefix) {
            continue;
        }
        out.push(entry.path());
    }
    out.sort();
    Ok(out)
}

/// list_repo_shards returns every current shard for the repo
/// prefix. Callers that need a durable artifact manifest should
/// use this after `run`: an idempotent reindex can overwrite an
/// existing shard path, in which case `IndexResult.shard_paths`
/// (new paths only) may legitimately be empty even though the
/// repo is searchable.
pub fn list_repo_shards(dir: &Path, prefix: &str) -> Result<Vec<PathBuf>> {
    list_shards(dir, prefix)
}

/// shard_prefix mirrors the legacy `getShardPrefix(orgId, repoId)`
/// — `<orgId>_<repoId>`. Same caveat as the Go side: no
/// trailing underscore, so repoId=2 false-positives repoId=23
/// for prefix matching. Documented in the Go repopaths package;
/// preserved here for parity.
pub fn shard_prefix(org_id: i32, repo_id: i32) -> String {
    format!("{}_{}", org_id, repo_id)
}

pub fn zoekt_branch_arg(branch: &str) -> String {
    let branch = branch.trim();
    if branch.is_empty() {
        return "HEAD".to_string();
    }
    branch
        .strip_prefix("refs/heads/")
        .or_else(|| branch.strip_prefix("refs/remotes/origin/"))
        .unwrap_or(branch)
        .trim_start_matches('/')
        .to_string()
}

/// Q.C — default list of directory names to prune from a
/// materialized worktree before indexing. Each entry is a
/// directory **name** (not a path) — the walker removes every
/// occurrence wherever it sits in the tree, mimicking the
/// behaviour `git ls-files` would have if these were entries
/// in `.sourcegraph/ignore`.
///
/// The list captures the highest-traffic noise sources that
/// kill Zoekt top-K ranking when indexed (a `node_modules`
/// folder alone can balloon a 1k-LOC TypeScript repo into a
/// 200k-LOC trigram corpus, drowning the user's actual code in
/// upstream-package text). All entries are deliberately
/// generic enough to apply across language ecosystems; per-
/// language additions (e.g. `target/release/deps`) can be
/// added in future slices.
pub fn default_prune_dirs() -> &'static [&'static str] {
    &[
        // JavaScript / TypeScript
        "node_modules",
        ".next",
        ".nuxt",
        ".output",
        ".turbo",
        // Go
        "vendor",
        // Rust
        "target",
        // Python
        "__pycache__",
        ".venv",
        "venv",
        "dist",
        "build",
        ".tox",
        // Java / Kotlin / Scala
        ".gradle",
        // Common
        "coverage",
        ".coverage",
        ".idea",
        ".vscode",
        ".cache",
        // Misc generated
        "generated",
    ]
}

/// Q.C — default ctags-processor language map. Empty by
/// default; deployments can override via env to route
/// per-language to alternative ctags binaries (universal-ctags
/// vs scip-ctags, etc.). Format follows zoekt-git-index
/// `-language_map`: "lang1:N,lang2:N,..." where N is the
/// processor index.
pub fn default_language_map() -> &'static str {
    ""
}

/// Q.C — prune_known_noisy_dirs walks `root` and removes
/// every directory whose final path component matches an
/// entry in `dir_names`. Returns (removed_count, bytes_freed)
/// tuple; counts are best-effort (a child rm failure during
/// recursion is logged and skipped).
///
/// This remains useful for consumers that scan materialized
/// snapshots directly. Zoekt itself now uses the vendored
/// `-ignore_dir` flag so git-tree indexing avoids the same
/// noise without mutating commits.
///
/// **Tenant safety**: the function operates on a tree-local
/// path; it never escapes `root`. Symlinks are NOT followed
/// to avoid escaping the materialized snapshot via a
/// malicious symlink in the cloned repo (`std::fs::read_dir`
/// reports symlinks as such; we skip them).
pub fn prune_known_noisy_dirs(root: &Path, dir_names: &[&str]) -> Result<(usize, u64)> {
    use std::collections::HashSet;
    if dir_names.is_empty() {
        return Ok((0, 0));
    }
    let banned: HashSet<&&str> = dir_names.iter().collect();
    let mut removed_count = 0usize;
    let mut freed_bytes = 0u64;
    let mut stack: Vec<PathBuf> = vec![root.to_path_buf()];
    while let Some(dir) = stack.pop() {
        let entries = match std::fs::read_dir(&dir) {
            Ok(e) => e,
            Err(_) => continue,
        };
        for entry in entries.flatten() {
            let file_type = match entry.file_type() {
                Ok(ft) => ft,
                Err(_) => continue,
            };
            // Symlinks are skipped to prevent escape outside
            // the snapshot root. Files are uninteresting for
            // pruning — only directories are inspected.
            if file_type.is_symlink() || !file_type.is_dir() {
                continue;
            }
            let name = entry.file_name();
            let name_str = name.to_string_lossy();
            if banned.contains(&name_str.as_ref()) {
                let path = entry.path();
                let size = dir_size_recursive(&path).unwrap_or(0);
                if std::fs::remove_dir_all(&path).is_ok() {
                    removed_count += 1;
                    freed_bytes += size;
                }
                // Don't recurse into a directory we just deleted.
                continue;
            }
            // Skip recursion into `.git` — the repository
            // metadata is not under our control and pruning
            // anything inside would break the indexer.
            if name_str == ".git" {
                continue;
            }
            stack.push(entry.path());
        }
    }
    Ok((removed_count, freed_bytes))
}

/// dir_size_recursive returns the sum of byte sizes of every
/// regular file at or below `path`. Used by prune_known_noisy_dirs
/// to report freed-bytes for observability. Errors during
/// traversal are silently treated as 0 — the metric is best-
/// effort, not a correctness signal.
fn dir_size_recursive(path: &Path) -> Result<u64> {
    let mut total = 0u64;
    let mut stack = vec![path.to_path_buf()];
    while let Some(dir) = stack.pop() {
        let entries = match std::fs::read_dir(&dir) {
            Ok(e) => e,
            Err(_) => continue,
        };
        for entry in entries.flatten() {
            let ft = match entry.file_type() {
                Ok(t) => t,
                Err(_) => continue,
            };
            if ft.is_symlink() {
                continue;
            }
            if ft.is_dir() {
                stack.push(entry.path());
                continue;
            }
            if let Ok(meta) = entry.metadata() {
                total += meta.len();
            }
        }
    }
    Ok(total)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    #[cfg(unix)]
    use std::os::unix::fs::PermissionsExt;
    use tempfile::tempdir;

    #[test]
    fn default_prune_dirs_includes_high_traffic_noise() {
        let dirs = default_prune_dirs();
        for must_have in [
            "node_modules",
            "vendor",
            "target",
            "dist",
            "build",
            "__pycache__",
            "coverage",
        ] {
            assert!(
                dirs.contains(&must_have),
                "default prune list missing {}",
                must_have
            );
        }
    }

    #[test]
    fn zoekt_branch_arg_uses_remote_origin_branch_names() {
        assert_eq!(zoekt_branch_arg("refs/heads/main"), "main");
        assert_eq!(
            zoekt_branch_arg("refs/remotes/origin/release-a"),
            "release-a"
        );
        assert_eq!(zoekt_branch_arg("feature/x"), "feature/x");
        assert_eq!(zoekt_branch_arg(""), "HEAD");
    }

    #[cfg(unix)]
    #[test]
    fn run_passes_native_delta_flags_to_zoekt_git_index() {
        let td = tempdir().unwrap();
        let repo = td.path().join("repo");
        let index = td.path().join("index");
        fs::create_dir_all(repo.join(".git")).unwrap();
        fs::create_dir_all(&index).unwrap();
        let args_file = td.path().join("args.txt");
        let fake = td.path().join("zoekt-git-index");
        fs::write(
            &fake,
            r#"#!/bin/sh
printf '%s\n' "$@" > "$CODEINTEL_TEST_ARGS"
index_dir=""
prefix=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -index) shift; index_dir="$1" ;;
    -shard_prefix) shift; prefix="$1" ;;
  esac
  shift
done
mkdir -p "$index_dir"
touch "$index_dir/${prefix}_v16.00000.zoekt"
exit 0
"#,
        )
        .unwrap();
        let mut perms = fs::metadata(&fake).unwrap().permissions();
        perms.set_mode(0o755);
        fs::set_permissions(&fake, perms).unwrap();
        std::env::set_var("CODEINTEL_TEST_ARGS", &args_file);

        let branches = vec!["refs/heads/main".to_string()];
        let result = run(
            &fake,
            IndexRequest {
                repo_path: &repo,
                index_dir: &index,
                shard_prefix: "7_42_bmain",
                tenant_id: 7,
                repo_id: 42,
                branches: &branches,
                max_trigram_count: 0,
                file_limit_bytes: 0,
                large_file_patterns: &[],
                language_map: "",
                require_ctags: false,
                ignore_dirs: &["node_modules"],
                use_delta: true,
                delta_shard_number_fallback_threshold: 123,
            },
        )
        .unwrap();

        assert_eq!(result.shard_paths.len(), 1);
        let args = fs::read_to_string(args_file).unwrap();
        std::env::remove_var("CODEINTEL_TEST_ARGS");
        assert!(args.lines().any(|line| line == "-delta"), "args:\n{}", args);
        assert!(
            args.lines()
                .collect::<Vec<_>>()
                .windows(2)
                .any(|pair| pair == ["-delta_threshold", "123"]),
            "args:\n{}",
            args
        );
    }

    #[test]
    fn prune_known_noisy_dirs_removes_matching_dirs_and_returns_freed_bytes() {
        let td = tempdir().unwrap();
        let root = td.path();
        // Layout:
        //   root/keep.txt           (5 bytes — survives)
        //   root/node_modules/a.js  (10 bytes — pruned)
        //   root/src/index.ts       (8 bytes — survives)
        //   root/src/vendor/x.go    (12 bytes — pruned via nested)
        //   root/.git/refs/...      (3 bytes — survives — .git is skipped)
        fs::write(root.join("keep.txt"), "hello").unwrap();
        fs::create_dir_all(root.join("node_modules")).unwrap();
        fs::write(root.join("node_modules/a.js"), "1234567890").unwrap();
        fs::create_dir_all(root.join("src")).unwrap();
        fs::write(root.join("src/index.ts"), "12345678").unwrap();
        fs::create_dir_all(root.join("src/vendor")).unwrap();
        fs::write(root.join("src/vendor/x.go"), "123456789012").unwrap();
        fs::create_dir_all(root.join(".git/refs")).unwrap();
        fs::write(root.join(".git/refs/HEAD"), "abc").unwrap();

        let (count, freed) = prune_known_noisy_dirs(root, &["node_modules", "vendor"]).unwrap();
        assert_eq!(count, 2, "expected 2 pruned dirs");
        assert!(freed >= 22, "expected >=22 bytes freed, got {}", freed);
        assert!(!root.join("node_modules").exists());
        assert!(!root.join("src/vendor").exists());
        assert!(root.join("keep.txt").exists());
        assert!(root.join("src/index.ts").exists());
        // .git must be preserved.
        assert!(root.join(".git/refs/HEAD").exists());
    }

    #[test]
    fn prune_known_noisy_dirs_with_empty_list_is_noop() {
        let td = tempdir().unwrap();
        let root = td.path();
        fs::create_dir_all(root.join("node_modules")).unwrap();
        let (count, freed) = prune_known_noisy_dirs(root, &[]).unwrap();
        assert_eq!(count, 0);
        assert_eq!(freed, 0);
        assert!(root.join("node_modules").exists());
    }

    #[test]
    fn prune_known_noisy_dirs_skips_symlinks() {
        // Symlinked target outside the snapshot must NOT be
        // followed; the prune must operate only inside the
        // tree we control. We can't easily create a symlink
        // outside the tempdir scope but we can verify that a
        // symlink TO a directory matching the prune list is
        // NOT followed (the link itself is skipped).
        let td = tempdir().unwrap();
        let root = td.path();
        fs::create_dir_all(root.join("real_node_modules")).unwrap();
        fs::write(root.join("real_node_modules/x"), "y").unwrap();
        #[cfg(unix)]
        {
            std::os::unix::fs::symlink(root.join("real_node_modules"), root.join("node_modules"))
                .unwrap();
            let (count, _) = prune_known_noisy_dirs(root, &["node_modules"]).unwrap();
            // The symlink itself is NOT removed (file_type
            // reports it as a symlink, not a dir).
            assert_eq!(count, 0);
            assert!(root.join("real_node_modules/x").exists());
        }
    }
}
