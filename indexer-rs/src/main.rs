//! codeintel-indexer-rs — the Rust counterpart to the Go-side
//! `codeintel/internal/backend/gitclone` package.
//!
//! Two subcommands:
//!   - `clone`  (R.1): standalone clone-and-print-HEAD CLI.
//!     Used today by the Go side's parity test
//!     (TestParity_GoVsRustClone_SameHeadAndContent) and as a
//!     manual operator tool.
//!   - `worker` (legacy/dev only): asynq-driven INDEX-task
//!     consumer retained for old parity fixtures. Production
//!     deployments run `executor`; Redis/Postgres ownership lives
//!     in codeintel-backend.
//!
//! Architecture position (per architecture rules
//! docs/codeintel-architecture-rules.md): this binary is one
//! leg of the 3-binary topology (codeintel-app +
//! codeintel-backend + codeintel-indexer-rs). HPA-safe via
//! asynq's at-least-once delivery + stalled-job recovery.

use anyhow::{anyhow, Context, Result};
use clap::{Parser, Subcommand};
use std::{env, path::PathBuf};

use codeintel_indexer_rs::{clone, executor_service, worker};

#[derive(Parser, Debug)]
#[command(name = "codeintel-indexer-rs", version, about = "codeintel indexer")]
struct Cli {
    #[command(subcommand)]
    cmd: Cmd,
}

#[derive(Subcommand, Debug)]
enum Cmd {
    /// Clone a git repository and print the observed HEAD SHA.
    /// Sync entry point — no tokio runtime needed.
    Clone(CloneArgs),

    /// Run the legacy asynq worker. Disabled unless
    /// CODEINTEL_INDEXER_LEGACY_WORKER_ENABLED=true.
    Worker(WorkerCliArgs),

    /// Run the gRPC executor service used by codeintel-backend
    /// class-specific subjob consumers.
    Executor(ExecutorCliArgs),
}

#[derive(Parser, Debug)]
struct CloneArgs {
    /// Clone URL. file://, http://, https://, ssh:// supported.
    #[arg(long)]
    clone_url: String,

    /// Destination directory. Must not exist or must be empty.
    #[arg(long)]
    dest: PathBuf,

    /// Optional branch name (single-branch clone semantics).
    /// Empty -> clone the remote default branch.
    #[arg(long, default_value = "")]
    branch: String,

    /// Optional shallow depth (--depth N). 0 -> full clone.
    #[arg(long, default_value_t = 0)]
    depth: u32,
}

#[derive(Parser, Debug)]
struct WorkerCliArgs {
    /// Redis URL — must match what the Go side enqueues
    /// against. e.g. `redis://127.0.0.1:6379/0`.
    #[arg(long, env = "CODEINTEL_REDIS_URL")]
    redis_url: String,

    /// Postgres URL — connection string for the codeintel
    /// schema. e.g.
    /// `postgres://codeintel:codeintel@127.0.0.1:5432/codeintel?sslmode=disable`.
    #[arg(long, env = "CODEINTEL_DATABASE_URL")]
    postgres_url: String,

    /// Data cache dir — base path for the per-repo clone
    /// directories. Mirrors the Go side's
    /// CODEINTEL_DATA_CACHE_DIR.
    #[arg(long, env = "CODEINTEL_DATA_CACHE_DIR", default_value = "./data")]
    data_cache_dir: String,

    /// asynq concurrency — number of tasks processed in
    /// parallel by THIS pod. HPA layers on top: scale pods
    /// horizontally for more parallelism.
    #[arg(long, default_value_t = 4)]
    concurrency: u32,

    /// Path to the `zoekt-git-index` binary (R.3). Empty -> the
    /// Zoekt step is skipped (clone-only mode). In prod the
    /// binary is shipped alongside this one + the path is
    /// wired via CODEINTEL_ZOEKT_GIT_INDEX_PATH.
    #[arg(long, env = "CODEINTEL_ZOEKT_GIT_INDEX_PATH", default_value = "")]
    zoekt_binary: String,
}

#[derive(Parser, Debug)]
struct ExecutorCliArgs {
    /// gRPC listen address for IndexExecutorService.
    #[arg(
        long,
        env = "CODEINTEL_INDEX_EXECUTOR_GRPC_LISTEN_ADDR",
        default_value = "0.0.0.0:3201"
    )]
    listen_addr: String,

    /// Data cache dir containing revision snapshots.
    #[arg(long, env = "CODEINTEL_DATA_CACHE_DIR", default_value = "./data")]
    data_cache_dir: String,

    /// Shared artifact root. Executor writes temp artifacts here;
    /// backend validates, checksums, and atomically publishes them.
    #[arg(long, env = "CODEINTEL_INDEX_ARTIFACT_ROOT")]
    artifact_root: String,

    /// Path to the vendored zoekt-git-index binary. Core executor
    /// workers must have this on PATH or pass an absolute path; the
    /// split ZOEKT subjob fails closed when it cannot run.
    #[arg(
        long,
        env = "CODEINTEL_ZOEKT_GIT_INDEX_PATH",
        default_value = "zoekt-git-index"
    )]
    zoekt_binary: String,

    /// Per-SCIP-project timeout in seconds.
    #[arg(long, env = "CODEINTEL_SCIP_TIMEOUT_SECONDS", default_value_t = 600)]
    scip_timeout_seconds: u64,
}

fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.cmd {
        Cmd::Clone(args) => run_clone(args),
        Cmd::Worker(args) => run_worker(args),
        Cmd::Executor(args) => run_executor(args),
    }
}

/// run_clone delegates to the library `clone::run` and prints
/// the HEAD SHA on stdout.
fn run_clone(args: CloneArgs) -> Result<()> {
    let res = clone::run(clone::Request {
        clone_url: &args.clone_url,
        destination: &args.dest,
        branch: &args.branch,
        depth: args.depth,
    })
    .with_context(|| "clone subcommand")?;
    println!("{}", res.commit_hash);
    Ok(())
}

/// run_worker spins up the tokio runtime + invokes the worker
/// loop. Blocks until shutdown.
fn run_worker(args: WorkerCliArgs) -> Result<()> {
    if !legacy_worker_enabled() {
        return Err(anyhow!(
            "worker subcommand is legacy/dev only; set CODEINTEL_INDEXER_LEGACY_WORKER_ENABLED=true or run the extraction-only executor subcommand"
        ));
    }
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|e| anyhow!("tokio runtime: {}", e))?;
    rt.block_on(worker::run(worker::WorkerArgs {
        redis_url: args.redis_url,
        postgres_url: args.postgres_url,
        data_cache_dir: args.data_cache_dir,
        concurrency: args.concurrency,
        zoekt_binary: args.zoekt_binary,
    }))
}

fn legacy_worker_enabled() -> bool {
    matches!(
        env::var("CODEINTEL_INDEXER_LEGACY_WORKER_ENABLED")
            .unwrap_or_default()
            .trim()
            .to_ascii_lowercase()
            .as_str(),
        "1" | "true" | "yes" | "on"
    )
}

fn run_executor(args: ExecutorCliArgs) -> Result<()> {
    if args.artifact_root.trim().is_empty() {
        return Err(anyhow!("CODEINTEL_INDEX_ARTIFACT_ROOT is required"));
    }
    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .map_err(|e| anyhow!("tokio runtime: {}", e))?;
    rt.block_on(executor_service::run(executor_service::ExecutorConfig {
        listen_addr: args.listen_addr,
        data_cache_dir: args.data_cache_dir,
        artifact_root: args.artifact_root,
        zoekt_binary: args.zoekt_binary,
        scip_timeout: std::time::Duration::from_secs(args.scip_timeout_seconds),
    }))
}
