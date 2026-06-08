//! Multi-branch resolution. Direct port of the per-branch
//! enumeration in `packages/backend/src/repoIndexManager.ts:780-832`.
//!
//! Inputs:
//!   - The Repo.metadata JSON (parsed for `branches` + `tags`
//!     glob lists).
//!   - The cloned working tree's actual refs (`refs/heads/*`
//!     and `refs/tags/*`).
//!
//! Output:
//!   - Concrete revision list (fully qualified ref names like
//!     `refs/heads/main`, `refs/tags/v1.2.3`), de-duplicated,
//!     capped at 64 (Zoekt's hard ceiling).
//!
//! The legacy uses `micromatch` for glob matching; we use
//! `globset` which understands the same `*`, `?`, `[abc]`,
//! `**` syntax. The Go-side `repoindexstatus.IsBranchAllowedByIndexPolicy`
//! (P.3b) uses `gobwas/glob` — three different libraries, same
//! patterns, equivalent semantics.
//!
//! Empty `metadata.branches`: falls back to the repo's default
//! branch (resolved from HEAD on the cloned tree), matching
//! legacy lines 792-794.

use anyhow::{anyhow, Context, Result};
use globset::{Glob, GlobSetBuilder};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
use std::path::Path;

/// RepoMetadata is the narrow subset of legacy `repoMetadataSchema`
/// (`packages/shared/src/types.ts:12`) that this module reads.
/// Other fields (gitConfig, manualIndexDisabled, codeHostMetadata)
/// stay opaque so a JSONB round-trip via tokio-postgres
/// preserves them when we write `indexedRevisions` back.
#[derive(Debug, Default, Clone, Serialize, Deserialize)]
pub struct RepoMetadata {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub branches: Option<Vec<String>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tags: Option<Vec<String>>,
    #[serde(
        default,
        skip_serializing_if = "Option::is_none",
        rename = "indexedRevisions"
    )]
    pub indexed_revisions: Option<Vec<String>>,

    /// Catch-all for fields this module doesn't read. Keeps a
    /// round-trip JSON-equivalent so an UPDATE that touches
    /// only `indexedRevisions` doesn't drop user-set fields.
    #[serde(flatten)]
    pub extra: serde_json::Map<String, serde_json::Value>,
}

/// MAX_REVISIONS is Zoekt's per-shard cap. Legacy log line at
/// repoIndexManager.ts:826 truncates with a warning. Same cap
/// + same warning here.
pub const MAX_REVISIONS: usize = 64;

/// resolve_revisions enumerates the concrete ref list that
/// should be indexed for a repo given its metadata.
///
/// Direct port of the legacy block at
/// repoIndexManager.ts:783-832:
///
/// 1. If metadata.branches is empty, use the default branch
///    (HEAD on the cloned tree) prefixed with `refs/heads/`.
/// 2. If metadata.branches has patterns, list the repo's
///    actual `refs/heads/*` and glob-match.
/// 3. If metadata.tags has patterns, append matching
///    `refs/tags/*`.
/// 4. De-dup, sort, cap at MAX_REVISIONS.
pub fn resolve_revisions(repo_path: &Path, metadata: &RepoMetadata) -> Result<Vec<String>> {
    let mut out: BTreeSet<String> = BTreeSet::new();

    // The branch lists in metadata.branches are GLOBS (or
    // empty). Two paths:
    let branches_field_set = metadata
        .branches
        .as_ref()
        .map(|v| !v.is_empty())
        .unwrap_or(false);
    if branches_field_set {
        let branch_globs = metadata.branches.as_ref().unwrap();
        let all_branches = list_local_branches(repo_path)?;
        let globset = build_globset(branch_globs)?;
        for branch in &all_branches {
            if globset.is_match(branch) {
                out.insert(format!("refs/heads/{}", branch));
            }
        }
    } else {
        // No metadata.branches → fall back to the repo's
        // default branch. Legacy resolves via
        // getLocalDefaultBranch (HEAD); we mirror via git2.
        let default = default_branch(repo_path)?;
        if !default.is_empty() {
            let with_prefix = if default.starts_with("refs/") {
                default
            } else {
                format!("refs/heads/{}", default)
            };
            out.insert(with_prefix);
        } else {
            // No default branch resolvable — match legacy
            // line 794's `['HEAD']` fallback.
            out.insert("HEAD".to_string());
        }
    }

    // Tags policy: separate axis from branches; both can be set.
    if let Some(tag_globs) = metadata.tags.as_ref() {
        if !tag_globs.is_empty() {
            let all_tags = list_local_tags(repo_path)?;
            let globset = build_globset(tag_globs)?;
            for tag in &all_tags {
                if globset.is_match(tag) {
                    out.insert(format!("refs/tags/{}", tag));
                }
            }
        }
    }

    // BTreeSet gives us de-dup + sorted order; sorted is
    // stable so log fingerprints + Zoekt cmdline are
    // deterministic across runs.
    let mut revisions: Vec<String> = out.into_iter().collect();

    // Cap. Mirror the legacy warning behavior — caller logs
    // when truncation occurs.
    if revisions.len() > MAX_REVISIONS {
        revisions.truncate(MAX_REVISIONS);
    }
    Ok(revisions)
}

/// list_local_branches returns the unqualified branch names
/// (e.g. ["main", "feature/auth"]) visible in a freshly cloned
/// working tree. Mirrors legacy
/// `packages/backend/src/git.ts:getBranches` which uses
/// simple-git's `branch.all` — that returns BOTH local refs +
/// remote-tracking refs (`origin/<name>`).
///
/// On a fresh `git clone`, only the default branch is in
/// `refs/heads/*` locally; every other branch lives under
/// `refs/remotes/origin/*`. We enumerate both, strip the
/// `origin/` prefix from the remote-tracking ones, drop the
/// `HEAD` symbolic pointer, and de-dup.
fn list_local_branches(repo_path: &Path) -> Result<Vec<String>> {
    let repo = git2::Repository::open(repo_path)
        .with_context(|| format!("open {} for branch enumeration", repo_path.display()))?;
    let mut out: BTreeSet<String> = BTreeSet::new();

    // Local branches (refs/heads/*).
    for entry in repo.branches(Some(git2::BranchType::Local))? {
        let (branch, _) = entry?;
        if let Some(name) = branch.name()? {
            if !name.is_empty() {
                out.insert(name.to_string());
            }
        }
    }

    // Remote-tracking branches (refs/remotes/origin/*).
    // git2's `branch.name()` returns the SHORT name including
    // the remote prefix, e.g. "origin/feature/auth". Strip
    // "origin/" so the result matches the local-branch shape
    // and so glob patterns like "feature/*" work uniformly.
    for entry in repo.branches(Some(git2::BranchType::Remote))? {
        let (branch, _) = entry?;
        if let Some(name) = branch.name()? {
            // Skip the symbolic HEAD pointer ("origin/HEAD") —
            // it's not a real branch.
            if name.ends_with("/HEAD") || name == "origin/HEAD" {
                continue;
            }
            // Strip the "<remote>/" prefix. We don't hard-code
            // "origin/" — read whatever remote name the clone
            // used (in practice the asynq worker only ever
            // clones with a single "origin" remote, but the
            // strip-by-slash form is more defensive).
            let stripped = match name.find('/') {
                Some(idx) => &name[idx + 1..],
                None => name,
            };
            if !stripped.is_empty() {
                out.insert(stripped.to_string());
            }
        }
    }

    Ok(out.into_iter().collect())
}

/// list_local_tags returns the tag short names (e.g. ["v1.2.3"]).
fn list_local_tags(repo_path: &Path) -> Result<Vec<String>> {
    let repo = git2::Repository::open(repo_path)
        .with_context(|| format!("open {} for tag enumeration", repo_path.display()))?;
    let mut out = Vec::new();
    repo.tag_foreach(|_oid, name| {
        // name is `refs/tags/<name>` — strip the prefix.
        let name = std::str::from_utf8(name).unwrap_or("");
        if let Some(stripped) = name.strip_prefix("refs/tags/") {
            out.push(stripped.to_string());
        }
        true
    })?;
    Ok(out)
}

/// default_branch returns the unqualified default-branch name
/// the cloned working tree's HEAD points at. Same shape as
/// the legacy getLocalDefaultBranch helper.
fn default_branch(repo_path: &Path) -> Result<String> {
    let repo = git2::Repository::open(repo_path)
        .with_context(|| format!("open {} for HEAD", repo_path.display()))?;
    let head = repo.head().with_context(|| "resolve HEAD")?;
    if head.is_branch() {
        Ok(head.shorthand().unwrap_or("").to_string())
    } else {
        // Detached HEAD post-clone — return empty so caller
        // falls back to "HEAD" symbolic.
        Ok(String::new())
    }
}

fn build_globset(patterns: &[String]) -> Result<globset::GlobSet> {
    let mut b = GlobSetBuilder::new();
    for p in patterns {
        if p.is_empty() {
            continue;
        }
        let g = Glob::new(p).map_err(|e| anyhow!("bad glob {}: {}", p, e))?;
        b.add(g);
    }
    b.build().map_err(|e| anyhow!("globset build: {}", e))
}
