//! Real git2-backed clone. Same outcome as the Go
//! `codeintel/internal/backend/gitclone` package, validated by
//! `tests/integration/rust_indexer_parity_test.go`
//! (TestParity_GoVsRustClone_SameHeadAndContent).

use anyhow::{anyhow, Context, Result};
use std::path::Path;
use std::process::Command;

/// Request mirrors the Go `gitclone.Request` struct so payloads
/// can be JSON-round-tripped between languages without
/// translation glue.
#[derive(Debug, Clone)]
pub struct Request<'a> {
    pub clone_url: &'a str,
    pub destination: &'a Path,
    pub branch: &'a str,
    pub depth: u32,
}

/// Result mirrors `gitclone.Result`.
#[derive(Debug, Clone)]
pub struct CloneResult {
    pub commit_hash: String,
    pub branch: String,
}

/// run does the real clone. Returns the observed HEAD SHA + the
/// branch HEAD resolves to. Validates the destination is empty
/// before invoking libgit2 so a half-written dir isn't merged
/// into.
pub fn run(req: Request<'_>) -> Result<CloneResult> {
    validate_dest(req.destination)?;

    let mut builder = git2::build::RepoBuilder::new();
    if !req.branch.is_empty() {
        builder.branch(req.branch);
    }
    let mut fetch_opts = git2::FetchOptions::new();
    if req.depth > 0 {
        fetch_opts.depth(req.depth as i32);
    }
    builder.fetch_options(fetch_opts);

    let repo = match builder.clone(req.clone_url, req.destination) {
        Ok(repo) => repo,
        Err(git2_err) => {
            // libgit2 is stricter than the system Git client for
            // dumb-HTTP repositories generated with
            // `git update-server-info`: it can reject valid static
            // Nginx repos when the response lacks a smart-Git
            // Content-Type. The production image carries `git`, so
            // fall back to the CLI while preserving the same
            // observable clone result.
            return run_git_cli(&req).with_context(|| {
                format!(
                    "clone {} -> {} (git2 failed first: {})",
                    req.clone_url,
                    req.destination.display(),
                    git2_err
                )
            });
        }
    };
    let head = repo
        .head()
        .with_context(|| "resolve HEAD on freshly-cloned repo")?;
    let oid = head
        .target()
        .ok_or_else(|| anyhow!("HEAD has no oid (symbolic only)"))?;
    let branch_name = head.shorthand().unwrap_or("HEAD").to_string();
    Ok(CloneResult {
        commit_hash: oid.to_string(),
        branch: branch_name,
    })
}

fn run_git_cli(req: &Request<'_>) -> Result<CloneResult> {
    if req.destination.exists() {
        std::fs::remove_dir_all(req.destination).with_context(|| {
            format!(
                "remove partial destination {} before git fallback",
                req.destination.display()
            )
        })?;
    }
    if let Some(parent) = req.destination.parent() {
        std::fs::create_dir_all(parent)
            .with_context(|| format!("mkdir parent {}", parent.display()))?;
    }

    let mut cmd = Command::new("git");
    cmd.arg("clone");
    if req.depth > 0 {
        cmd.args(["--depth", &req.depth.to_string()]);
    }
    if !req.branch.is_empty() {
        cmd.args(["--branch", req.branch]);
    }
    cmd.arg(req.clone_url).arg(req.destination);
    let output = cmd.output().with_context(|| "spawn git clone fallback")?;
    if !output.status.success() {
        return Err(anyhow!(
            "git clone fallback exit={:?} stderr={}",
            output.status,
            String::from_utf8_lossy(&output.stderr)
        ));
    }

    let commit_hash = git_stdout(req.destination, ["rev-parse", "HEAD"])?;
    let branch = git_stdout(req.destination, ["branch", "--show-current"])
        .unwrap_or_else(|_| "HEAD".to_string());
    let branch = if branch.is_empty() {
        "HEAD".to_string()
    } else {
        branch
    };
    Ok(CloneResult {
        commit_hash,
        branch,
    })
}

fn git_stdout<const N: usize>(repo: &Path, args: [&str; N]) -> Result<String> {
    let output = Command::new("git")
        .arg("-C")
        .arg(repo)
        .args(args)
        .output()
        .with_context(|| format!("spawn git -C {}", repo.display()))?;
    if !output.status.success() {
        return Err(anyhow!(
            "git -C {} exit={:?} stderr={}",
            repo.display(),
            output.status,
            String::from_utf8_lossy(&output.stderr)
        ));
    }
    Ok(String::from_utf8_lossy(&output.stdout).trim().to_string())
}

fn validate_dest(dest: &Path) -> Result<()> {
    if dest.as_os_str().is_empty() {
        return Err(anyhow!("destination is required"));
    }
    match std::fs::read_dir(dest) {
        Ok(mut entries) => {
            if entries.next().is_some() {
                return Err(anyhow!("destination {} is not empty", dest.display()));
            }
            Ok(())
        }
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(e) => Err(anyhow!("stat {}: {}", dest.display(), e)),
    }
}
