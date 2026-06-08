//! Asynq-driven worker for the Rust INDEX dispatch path.
//!
//! Wire compatibility: this binary subscribes to the
//! `repo-index-rust` queue using the `asynq` Rust crate
//! (v0.1.8), which is wire-compatible with `hibiken/asynq`
//! (Go). The Go producer (AsynqRepoIndexer.Schedule) routes
//! INDEX tasks to this queue; the Rust pod consumes via
//! standard asynq semantics.
//!
//! HPA correctness: multiple `codeintel-indexer-rs` pods can
//! subscribe to the same queue concurrently. asynq's internal
//! state machine (PENDING → ACTIVE → COMPLETED) guarantees
//! at-least-once delivery and crash recovery — if a pod dies
//! mid-process the task transitions back to PENDING via
//! asynq's stalled-job sweeper. No custom sweeper required.
//!
//! What the handler does, per legacy
//! `repoindexmanager.dispatchIndex` (Go side):
//!   1. UPDATE RepoIndexingJob SET status='IN_PROGRESS'
//!   2. SELECT Repo metadata (orgId, cloneUrl) for the clone.
//!   3. Resolve destination via the repopaths protocol (the
//!      Go side stamps `destination` into the task payload via
//!      P-future-slice; until then we accept it inline).
//!   4. Run the clone (src/clone.rs).
//!   5. UPDATE Repo SET indexedAt=NOW(), indexedCommitHash=...
//!   6. UPDATE RepoIndexingJob SET status='COMPLETED',
//!      completedAt=NOW().
//! On any error: UPDATE RepoIndexingJob SET status='FAILED',
//! errorMessage=err — and return the error so asynq retries
//! per its policy.

use crate::branches::{self, RepoMetadata};
use crate::clone;
use crate::delta_plan::{ScipStrategy, SemanticExtractionFingerprint, ZoektStrategy};
use crate::manifest_files;
use crate::manifest_store::{IndexManifestManager, PreparedIndexManifest};
use crate::proto::codeintel::v1::{
    index_plan_service_client::IndexPlanServiceClient, IndexPlanRevision, ScipProjectPlan,
    WriteIndexPlanRequest, WriteIndexPlanResponse,
};
use crate::queue::{
    CodeGraphWritePayload, JobType, TaskPayload, CODE_GRAPH_WRITE_QUEUE,
    CODE_GRAPH_WRITE_TASK_TYPE, RUST_INDEX_QUEUE, TASK_TYPE_NAME,
};
use crate::scip;
use crate::zoekt;

use anyhow::{anyhow, Context, Result};
use asynq::backend::RedisConnectionType;
use asynq::config::ServerConfig;
use asynq::error::Error as AsynqError;
use asynq::serve_mux::ServeMux;
use asynq::server::ServerBuilder;
use asynq::task::Task;
use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::env;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use tokio_postgres::NoTls;
use tonic::transport::Endpoint;

/// WorkerArgs is the configuration the binary's worker
/// subcommand passes in.
#[derive(Debug, Clone)]
pub struct WorkerArgs {
    pub redis_url: String,
    pub postgres_url: String,
    pub data_cache_dir: String,
    pub concurrency: u32,
    /// Path to the `zoekt-git-index` binary. Empty -> skip the
    /// Zoekt step (clone-only mode, useful in dev environments
    /// where the vendored zoekt binary isn't built yet).
    pub zoekt_binary: String,
}

/// run starts the asynq Server and blocks until shutdown
/// (SIGTERM/SIGINT). Returns Ok(()) on clean shutdown, Err on
/// fatal startup failure.
pub async fn run(args: WorkerArgs) -> Result<()> {
    eprintln!(
        "codeintel-indexer-rs worker starting: redis={} concurrency={} data_cache_dir={}",
        scrub_creds(&args.redis_url),
        args.concurrency,
        args.data_cache_dir
    );

    // Postgres connection (shared across all concurrent task
    // handlers via Arc). One handler per task — connection
    // pooling at scale needs deadpool-postgres but for the
    // first slice the single Client suffices.
    let (client, conn) = tokio_postgres::connect(&args.postgres_url, NoTls)
        .await
        .with_context(|| format!("connect postgres: {}", scrub_creds(&args.postgres_url)))?;
    let pg = Arc::new(client);
    tokio::spawn(async move {
        if let Err(e) = conn.await {
            eprintln!("postgres connection task: {}", e);
        }
    });

    // Verify the connection is live before claiming we're ready.
    pg.simple_query("SELECT 1")
        .await
        .with_context(|| "postgres connectivity probe")?;
    eprintln!("postgres connected");

    let redis_config = RedisConnectionType::single(args.redis_url.clone())
        .map_err(|e| anyhow!("redis config: {}", e))?;

    // Subscribe to ONLY the rust-index queue. CLEANUP /
    // REMOVE_INDEX still go to the Go worker via
    // `repo-index-queue` — this Rust binary stays narrowly
    // scoped to clone-the-repo-and-record-HEAD.
    let mut queues = HashMap::new();
    queues.insert(RUST_INDEX_QUEUE.to_string(), 6);
    let cfg = ServerConfig::new()
        .concurrency(args.concurrency as usize)
        .queues(queues);

    // Build the ServeMux + handler.
    let mut mux = ServeMux::new();
    let data_cache_dir = args.data_cache_dir.clone();
    let zoekt_binary = args.zoekt_binary.clone();
    let postgres_url = args.postgres_url.clone();
    let redis_url = args.redis_url.clone();
    let pg_for_handler = pg.clone();
    mux.handle_async_func(TASK_TYPE_NAME, move |task: Task| {
        let pg = pg_for_handler.clone();
        let data_cache_dir = data_cache_dir.clone();
        let zoekt_binary = zoekt_binary.clone();
        let postgres_url = postgres_url.clone();
        let redis_url = redis_url.clone();
        async move {
            handle_index(
                pg,
                postgres_url,
                redis_url,
                data_cache_dir,
                zoekt_binary,
                task,
            )
            .await
        }
    });

    let mut server = ServerBuilder::new()
        .redis_config(redis_config)
        .server_config(cfg)
        .build()
        .await
        .map_err(|e| anyhow!("build asynq server: {}", e))?;
    eprintln!("asynq server starting on queue {}", RUST_INDEX_QUEUE);

    server
        .run(mux)
        .await
        .map_err(|e| anyhow!("asynq run: {}", e))?;
    Ok(())
}

/// handle_index is the per-task handler. Decodes payload,
/// drives the clone + DB updates, surfaces failures so asynq
/// retries. Returns `asynq::error::Error` on failure (the
/// crate's ServeMux::handle_async_func signature).
async fn handle_index(
    pg: Arc<tokio_postgres::Client>,
    postgres_url: String,
    redis_url: String,
    data_cache_dir: String,
    zoekt_binary: String,
    task: Task,
) -> std::result::Result<(), AsynqError> {
    let payload: TaskPayload = serde_json::from_slice(&task.payload)
        .map_err(|e| asynq_err(format!("decode payload: {}", e)))?;

    // The Rust worker only services INDEX. CLEANUP /
    // REMOVE_INDEX should never land here (different queue);
    // surface a typed error if it does so asynq archives the
    // misrouted task instead of looping on retries.
    if payload.job_type != JobType::Index {
        return Err(asynq_err(format!(
            "Rust indexer received non-INDEX task on {} queue: type={:?} jobId={}",
            RUST_INDEX_QUEUE, payload.job_type, payload.job_id
        )));
    }

    eprintln!(
        "INDEX received: job_id={} repo_id={} repo={}",
        payload.job_id, payload.repo_id, payload.repo_name
    );

    // 1. MarkInProgress on the RepoIndexingJob row (parity with
    //    repoindexmanager.Store.MarkInProgress).
    let mark_in_progress = pg
        .execute(
            r#"
            UPDATE "RepoIndexingJob"
            SET status = 'IN_PROGRESS', "updatedAt" = NOW()
            WHERE id = $1 AND "repoId" = $2 AND status IN ('PENDING', 'IN_PROGRESS')
            "#,
            &[&payload.job_id, &payload.repo_id],
        )
        .await
        .map_err(pg_err)?;
    if mark_in_progress == 0 {
        // Terminal-state job — return Ok so asynq treats it as
        // consumed and doesn't retry. Mirrors the Go-side
        // ErrJobInTerminalState short-circuit.
        eprintln!("job {} already terminal; skipping", payload.job_id);
        return Ok(());
    }

    // 2. Fetch the Repo's clone URL + cloneUrl + orgId + the
    //    `metadata` JSON. The metadata.branches / metadata.tags
    //    globs drive multi-branch resolution (R.4); we also
    //    need the whole JSON so the post-index UPDATE stamps
    //    `indexedRevisions` without dropping other fields.
    let row = pg
        .query_one(
            r#"
            SELECT "orgId", COALESCE("cloneUrl", ''),
                   COALESCE("external_codeHostType"::text, ''),
                   r.metadata
            FROM   "RepoIndexingJob" j
            JOIN   "Repo" r ON r.id = j."repoId"
            WHERE  j.id = $1 AND j."repoId" = $2 AND r.id = $2
            "#,
            &[&payload.job_id, &payload.repo_id],
        )
        .await
        .map_err(|e| {
            let msg = format!("FetchRepoForIndex: {}", e);
            mark_failed_blocking(&pg, &payload.job_id, &msg);
            pg_err(e)
        })?;
    let org_id: i32 = row.get(0);
    let clone_url: String = row.get(1);
    let _code_host_type: String = row.get(2);
    let metadata_value: serde_json::Value = row.get(3);
    let mut metadata: RepoMetadata =
        serde_json::from_value(metadata_value.clone()).unwrap_or_default();

    if clone_url.is_empty() {
        let msg = format!("repo {} has no cloneUrl", payload.repo_id);
        mark_failed(&pg, &payload.job_id, &msg).await;
        return Err(asynq_err(msg));
    }

    // 3. Resolve the tenant-scoped storage root. In org-directory
    //    mode every horizontally-scaled worker writes under the
    //    same EFS/RWX base path but isolates physical clone,
    //    Zoekt shard, SCIP artifact, and AST snapshot data by
    //    org id:
    //
    //      <CODEINTEL_ZOEKT_EFS_ROOT>/<orgId>/{repos,index,codeintel}
    //
    //    This matches the app/search pod's gRPC metadata routing
    //    and prevents a READY DB row from pointing at a shard path
    //    the search service will never open.
    let storage_root = tenant_storage_root(&data_cache_dir, org_id);
    let dest = storage_root.join("repos").join(payload.repo_id.to_string());
    eprintln!(
        "INDEX storage resolved: job_id={} org_id={} repo_id={} root={} dest={}",
        payload.job_id,
        org_id,
        payload.repo_id,
        storage_root.display(),
        dest.display()
    );

    // Fresh-clone semantics: nuke any stale working tree.
    if dest.exists() {
        if let Err(e) = std::fs::remove_dir_all(&dest) {
            let msg = format!("rm stale {}: {}", dest.display(), e);
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
    }
    if let Err(e) = std::fs::create_dir_all(&dest) {
        let msg = format!("mkdir {}: {}", dest.display(), e);
        mark_failed(&pg, &payload.job_id, &msg).await;
        return Err(asynq_err(msg));
    }

    // 4. Real clone.
    let cloned = match clone::run(clone::Request {
        clone_url: &clone_url,
        destination: &dest,
        branch: "",
        depth: 0,
    }) {
        Ok(r) => r,
        Err(e) => {
            let _ = std::fs::remove_dir_all(&dest);
            let msg = format!("clone: {}", e);
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
    };
    eprintln!(
        "INDEX clone done: job_id={} repo_id={} head={} branch={}",
        payload.job_id, payload.repo_id, cloned.commit_hash, cloned.branch
    );

    // 4a. Multi-branch resolution (R.4). Glob-match
    //     metadata.branches + metadata.tags against the cloned
    //     repo's actual refs; cap at 64 (Zoekt's hard ceiling).
    //     Empty metadata.branches -> default branch only,
    //     matching legacy repoIndexManager.ts:792-794.
    let revisions = match branches::resolve_revisions(&dest, &metadata) {
        Ok(r) => r,
        Err(e) => {
            let msg = format!("resolve_revisions: {}", e);
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
    };
    if revisions.is_empty() {
        let msg = format!(
            "repo {} resolved to zero revisions; metadata.branches={:?} tags={:?}",
            payload.repo_id, metadata.branches, metadata.tags
        );
        mark_failed(&pg, &payload.job_id, &msg).await;
        return Err(asynq_err(msg));
    }
    eprintln!(
        "INDEX revisions resolved: job_id={} count={} revisions={:?}",
        payload.job_id,
        revisions.len(),
        revisions
    );
    if revisions.len() == branches::MAX_REVISIONS {
        eprintln!(
            "WARN repo {} hit MAX_REVISIONS cap ({}); some refs may be excluded",
            payload.repo_id,
            branches::MAX_REVISIONS
        );
    }

    // 4b. Prepare per-branch RepoIndexManifest rows (R.6b).
    //     For each revision: build the current manifest from
    //     the working tree, look up the previous READY
    //     manifest for the same scope, compute the per-engine
    //     delta plan, INSERT a PENDING manifest row + bulk
    //     INSERT its files. Returns the list of plans so the
    //     downstream engines can consult them.
    //
    //     R.6c: workspace_id resolves via Org.atomWorkspaceId →
    //     Org.domain → `org-<id>` cascade (mirrors legacy
    //     indexManifestManager.ts:262-272). semantic_extraction
    //     resolves via OrgLanguageModel + the env gate
    //     CODEINTEL_CODE_GRAPH_SEMANTIC_EXTRACTION_ENABLED.
    //     Critic-gate fix: pg errors from both resolvers
    //     propagate (mark the job FAILED) rather than silently
    //     falling back — legacy Prisma queries throw on
    //     connection errors and fail the indexing job.
    //     provider_connection_id is the comma-joined sorted
    //     connection-IDs list (manifest_files helper).
    let workspace_id = match fetch_workspace_id(&pg, org_id).await {
        Ok(w) => w,
        Err(e) => {
            let msg = format!("fetch_workspace_id: {}", e);
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
    };
    let semantic_extraction = match fetch_semantic_extraction_fingerprint(&pg, org_id).await {
        Ok(s) => s,
        Err(e) => {
            let msg = format!("fetch_semantic_extraction_fingerprint: {}", e);
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
    };
    let provider_connection_id = match fetch_connection_ids(&pg, payload.repo_id, org_id).await {
        Ok(ids) => manifest_files::provider_connection_scope(&ids),
        Err(e) => {
            let msg = format!("fetch_connection_ids: {}", e);
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
    };
    let manifest_mgr = IndexManifestManager::new(pg.clone(), postgres_url.clone());
    let prepared: Vec<PreparedIndexManifest> = match manifest_mgr
        .prepare_revision_manifests(
            org_id,
            payload.repo_id,
            &dest,
            &revisions,
            &payload.job_id,
            &workspace_id,
            provider_connection_id.as_deref(),
            semantic_extraction.as_ref(),
        )
        .await
    {
        Ok(p) => p,
        Err(e) => {
            let msg = format!("prepare_revision_manifests: {}", e);
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
    };
    eprintln!(
        "INDEX manifests prepared: job_id={} count={} strategies=[{}]",
        payload.job_id,
        prepared.len(),
        prepared
            .iter()
            .map(|p| format!(
                "{}:{:?}/{:?}/{:?}/{:?}",
                p.branch,
                p.plan.zoekt.strategy,
                p.plan.scip.strategy,
                p.plan.graph.strategy,
                p.plan.semantic.strategy
            ))
            .collect::<Vec<_>>()
            .join(", ")
    );
    let mut materialized_revision_roots: BTreeSet<PathBuf> = BTreeSet::new();
    let mut planned_scip_projects_by_branch: BTreeMap<String, Vec<scip::ScipProject>> =
        BTreeMap::new();

    // H.1: optional durable hot/cold executor planning handoff.
    // Rust owns checkout inspection today, but Go owns durable
    // subjob persistence. When enabled, send the detected
    // revision/project facts to codeintel-backend over gRPC; the
    // current all-in-one Rust execution below continues unchanged
    // until class-specific executor consumers are production-ready.
    if std::env::var("CODEINTEL_INDEX_PLAN_WRITE_ENABLED")
        .map(|v| v == "true")
        .unwrap_or(false)
    {
        let plan_request = match build_index_plan_write_request(
            &payload,
            org_id,
            &workspace_id,
            &dest,
            &prepared,
            &mut planned_scip_projects_by_branch,
        ) {
            Ok(p) => Some(p),
            Err(e) => {
                let msg = format!("build index plan handoff: {}", e);
                if index_plan_write_required() {
                    return Err(asynq_err(msg));
                }
                eprintln!("WARN {} — continuing all-in-one index path", msg);
                None
            }
        };
        if let Some(plan_request) = plan_request {
            if plan_request.revisions.is_empty() {
                eprintln!(
                    "INDEX plan handoff skipped: job_id={} no revision-scoped layers planned",
                    payload.job_id
                );
            } else {
                match index_plan_grpc_addr() {
                    Some(addr) => match write_index_plan(&addr, plan_request).await {
                        Ok(resp) => eprintln!(
                            "INDEX plan handoff written: job_id={} addr={} revisions={} subjobs={}",
                            payload.job_id, addr, resp.revision_count, resp.subjob_count
                        ),
                        Err(e) => {
                            let msg = format!("index plan gRPC handoff failed: {}", e);
                            if index_plan_write_required() {
                                return Err(asynq_err(msg));
                            }
                            eprintln!("WARN {} — continuing all-in-one index path", msg);
                        }
                    },
                    None => {
                        let msg = "CODEINTEL_INDEX_PLAN_GRPC_ADDR is required when CODEINTEL_INDEX_PLAN_WRITE_ENABLED=true";
                        if index_plan_write_required() {
                            return Err(asynq_err(msg.to_string()));
                        }
                        eprintln!("WARN {} — continuing all-in-one index path", msg);
                    }
                }
            }
        }
    }

    // 4c. Zoekt indexing (R.3 + R.4 + R.7). Honors the
    //     per-branch plan: if every prepared revision's zoekt
    //     strategy is NOOP, skip the Zoekt run entirely.
    //     Otherwise we run Zoekt over the full selected branch
    //     set and request upstream's native delta shard build
    //     when CODEINTEL_ZOEKT_FILE_DELTA is enabled. Zoekt
    //     tombstones changed/deleted files in older shards and
    //     falls back to a normal build when existing shard
    //     options are incompatible.
    //
    //     Skipped when zoekt_binary is empty (dev/test mode).
    //     In prod the binary path is wired via
    //     CODEINTEL_ZOEKT_GIT_INDEX_PATH.
    // Critic-gate fix (R.7-c1): the NOOP filter decides
    // WHETHER to run Zoekt, but when it DOES run we hand the
    // FULL revisions list to zoekt-git-index because the
    // legacy all-in-one path owns a repo-level shard prefix.
    // Legacy repoIndexManager.ts:842-851 has the same shape:
    // `shouldRunZoekt` is computed from the manifest plans
    // but `indexGitRepository(repo, settings, revisions, ...)`
    // always receives the full revision list. Dropping NOOP
    // revisions from the shard would leave their READY
    // manifests pointing at content that's no longer in the
    // shard — parity violation.
    let any_zoekt_non_noop = prepared
        .iter()
        .any(|p| p.plan.zoekt.strategy != ZoektStrategy::Noop);
    let zoekt_should_run = !zoekt_binary.is_empty() && any_zoekt_non_noop;
    if zoekt_should_run {
        let index_dir = storage_root.join("index");
        if let Err(e) = std::fs::create_dir_all(&index_dir) {
            let msg = format!("mkdir index_dir {}: {}", index_dir.display(), e);
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
        let prefix = zoekt::shard_prefix(org_id, payload.repo_id);
        eprintln!(
            "ZOEKT indexing: job_id={} repo_id={} prefix={} index_dir={}",
            payload.job_id,
            payload.repo_id,
            prefix,
            index_dir.display()
        );
        // Q.C: opt-in strict ctags via env. Default false
        // (current behaviour) tolerates missing ctags binaries;
        // production deployments that bake ctags into the
        // indexer image set CODEINTEL_ZOEKT_REQUIRE_CTAGS=true
        // so a missing binary surfaces as an index failure
        // rather than silently degrading the symbol section.
        let require_ctags = std::env::var("CODEINTEL_ZOEKT_REQUIRE_CTAGS")
            .map(|v| v == "true")
            .unwrap_or(false);
        let use_delta = zoekt_file_delta_enabled();
        let delta_threshold = zoekt_delta_shard_threshold();
        let language_map = std::env::var("CODEINTEL_ZOEKT_LANGUAGE_MAP")
            .unwrap_or_else(|_| zoekt::default_language_map().to_string());
        let zres = zoekt::run(
            Path::new(&zoekt_binary),
            zoekt::IndexRequest {
                repo_path: &dest,
                index_dir: &index_dir,
                shard_prefix: &prefix,
                tenant_id: org_id,
                repo_id: payload.repo_id,
                // R.7 critic-gate fix: pass FULL revisions list
                // since Zoekt rebuilds repo-wide shards.
                branches: &revisions,
                max_trigram_count: 0,
                file_limit_bytes: 0,
                large_file_patterns: &[],
                // Q.C — ctags routing + strictness.
                language_map: &language_map,
                require_ctags,
                ignore_dirs: zoekt::default_prune_dirs(),
                use_delta,
                delta_shard_number_fallback_threshold: delta_threshold,
            },
        );
        match zres {
            Ok(zres) => eprintln!(
                "ZOEKT done: job_id={} shards={:?}",
                payload.job_id,
                zres.shard_paths
                    .iter()
                    .map(|p| p
                        .file_name()
                        .map(|n| n.to_string_lossy().to_string())
                        .unwrap_or_default())
                    .collect::<Vec<_>>()
            ),
            Err(e) => {
                let msg = format!("zoekt: {}", e);
                mark_failed(&pg, &payload.job_id, &msg).await;
                return Err(asynq_err(msg));
            }
        }
    } else if zoekt_binary.is_empty() {
        eprintln!(
            "ZOEKT skipped: zoekt_binary not configured (job_id={})",
            payload.job_id
        );
    } else {
        eprintln!(
            "ZOEKT skipped: every revision's plan is NOOP (job_id={})",
            payload.job_id
        );
    }

    // 4c'. SCIP indexing (R.8). Polyglot-aware: each prepared
    //      manifest scans its file list for per-language
    //      marker files (package.json, go.mod, Cargo.toml,
    //      ...), spawns one indexer per (language,
    //      project_root) tuple. NOOP-gated by the
    //      plan.scip.strategy from R.5/R.6b. Missing
    //      indexer binaries are tolerated (logged + skipped)
    //      — SCIP enables symbol-level navigation but the
    //      repo still indexes for search even without it.
    //      Output files live at
    //      `<data_cache_dir>/scip/<repo_id>/<commit_short>/<lang>_<root>.scip`.
    //      Parsing those protobufs into the per-symbol DB
    //      tables is the next slice (R.8b).
    // R.8 critic-gate fix: master env gate via
    // CODEINTEL_SCIP_INDEXING_ENABLED. Default-off (off
    // matches legacy production default — explicit opt-in
    // required); flip to "true" in deployments that want
    // SCIP.
    let scip_enabled = std::env::var("CODEINTEL_SCIP_INDEXING_ENABLED")
        .map(|v| v == "true")
        .unwrap_or(false);
    let scip_per_indexer_timeout = std::env::var("CODEINTEL_SCIP_INDEX_TIMEOUT_MS")
        .ok()
        .and_then(|s| s.parse::<u64>().ok())
        .map(std::time::Duration::from_millis)
        .unwrap_or_else(|| std::time::Duration::from_secs(600)); // legacy 10min default
    if !scip_enabled {
        eprintln!(
            "SCIP skipped: CODEINTEL_SCIP_INDEXING_ENABLED != 'true' (job_id={})",
            payload.job_id
        );
    }
    // Q.A: collect ingested SCIP rows per branch so the Code-Graph
    // build pass (below) can project SCIP semantic relationships
    // into the Nebula snapshot alongside AST regex facts. Keyed
    // by branch name; each entry is a list of (language,
    // ingested_rows). The ingest is "double work" in Q.A —
    // `persist_revision_scip_rows` also ingests internally before
    // writing to Postgres — Q.D will hoist this into a shared
    // ingest pass to avoid the duplicate parse.
    let mut scip_rows_by_branch: std::collections::BTreeMap<
        String,
        Vec<(String, crate::scip::ScipIngestRows)>,
    > = std::collections::BTreeMap::new();
    for prep in &prepared {
        if !scip_enabled {
            break;
        }
        if prep.plan.scip.strategy == ScipStrategy::Noop {
            continue;
        }
        // Refresh the file list for this revision only when the
        // optional plan handoff did not already compute the SCIP
        // project roots. The prepared manifest intentionally does
        // not carry files in-memory (memory pressure on large repos).
        let projects = if let Some(projects) = planned_scip_projects_by_branch.get(&prep.branch) {
            projects.clone()
        } else {
            let files =
                match manifest_files::build_manifest_files_for_revision(&dest, &prep.commit_hash) {
                    Ok(f) => f,
                    Err(e) => {
                        let msg = format!(
                            "SCIP scan failed: build_manifest_files_for_revision({}): {}",
                            prep.commit_hash, e
                        );
                        let manifest_ids_for_failure =
                            prepared.iter().map(|p| p.id.clone()).collect::<Vec<_>>();
                        let _ = manifest_mgr
                            .mark_manifests_failed(&manifest_ids_for_failure, &msg)
                            .await;
                        mark_failed(&pg, &payload.job_id, &msg).await;
                        return Err(asynq_err(msg));
                    }
                };
            let mut projects = scip::detect_scip_projects(&files);
            scip::add_inferred_typescript_projects(&files, &mut projects);
            let mut projects = scip::filter_projects_for_worker_classes(projects);
            let project_cap = scip::max_project_roots_per_revision();
            if projects.len() > project_cap {
                projects.truncate(project_cap);
            }
            projects
        };
        let revision_root = match ensure_revision_snapshot(
            &dest,
            &storage_root,
            payload.repo_id,
            &prep.commit_hash,
            &mut materialized_revision_roots,
        ) {
            Ok(path) => path,
            Err(e) => {
                let msg = format!("materialize SCIP revision {}: {}", prep.branch, e);
                let manifest_ids_for_failure =
                    prepared.iter().map(|p| p.id.clone()).collect::<Vec<_>>();
                let _ = manifest_mgr
                    .mark_manifests_failed(&manifest_ids_for_failure, &msg)
                    .await;
                mark_failed(&pg, &payload.job_id, &msg).await;
                return Err(asynq_err(msg));
            }
        };
        // R.8 critic-gate fix: include orgId in the output
        // path so two orgs that share a repo_id (shouldn't
        // happen but defense-in-depth — repo_id is a global
        // INT but the artifact filesystem should isolate by
        // tenant). Matches legacy
        // scipIndexManager.ts:321 layout
        // (<artifactRoot>/<orgId>/<repoId>/...).
        let commit_short: String = prep.commit_hash.chars().take(12).collect();
        let scip_out = storage_root
            .join("codeintel")
            .join("scip")
            .join(payload.repo_id.to_string())
            .join(&commit_short);
        if let Err(e) = std::fs::create_dir_all(&scip_out) {
            let msg = format!("SCIP mkdir {} failed: {}", scip_out.display(), e);
            let manifest_ids_for_failure =
                prepared.iter().map(|p| p.id.clone()).collect::<Vec<_>>();
            let _ = manifest_mgr
                .mark_manifests_failed(&manifest_ids_for_failure, &msg)
                .await;
            mark_failed(&pg, &payload.job_id, &msg).await;
            return Err(asynq_err(msg));
        }
        let mut ok = 0usize;
        let mut missing = 0usize;
        let mut failed = 0usize;
        let mut successful_projects: Vec<scip::ScipProjectIngestInput> = Vec::new();
        let mut failed_projects: Vec<scip::ScipProjectFailureInput> = Vec::new();
        for project in &projects {
            let output_path = scip::scip_output_path(&scip_out, project);
            match scip::run_scip_indexer(
                &revision_root,
                project,
                &output_path,
                scip_per_indexer_timeout,
            ) {
                Ok(res) => {
                    ok += 1;
                    eprintln!(
                        "SCIP ok: {}/{} -> {} ({})",
                        res.language,
                        res.project_root,
                        res.output_path.display(),
                        scip::format_duration_ms(res.duration),
                    );
                    successful_projects.push(scip::ScipProjectIngestInput {
                        language: res.language.to_string(),
                        project_root: res.project_root.clone(),
                        indexer: res.indexer.to_string(),
                        worker_class: Some(res.worker_class.to_string()),
                        artifact_path: res.output_path.clone(),
                        command: res.command.clone(),
                        duration_ms: Some(
                            std::cmp::min(res.duration.as_millis(), i32::MAX as u128) as i32,
                        ),
                    });
                }
                Err(scip::ScipRunError::BinaryMissing(b)) => {
                    missing += 1;
                    eprintln!("SCIP indexer missing: {} (continuing)", b);
                    failed_projects.push(scip::ScipProjectFailureInput {
                        language: project.language.to_string(),
                        project_root: project.project_root.clone(),
                        indexer: project.indexer.to_string(),
                        worker_class: Some(project.worker_class.to_string()),
                        artifact_path: Some(output_path),
                        command: project.indexer.to_string(),
                        status: "SKIPPED".to_string(),
                        error_message: format!(
                            "SCIP indexer command \"{}\" is not available on PATH.",
                            b
                        ),
                    });
                }
                Err(e) => {
                    failed += 1;
                    eprintln!("SCIP indexer failed: {}", e);
                    failed_projects.push(scip::ScipProjectFailureInput {
                        language: project.language.to_string(),
                        project_root: project.project_root.clone(),
                        indexer: project.indexer.to_string(),
                        worker_class: Some(project.worker_class.to_string()),
                        artifact_path: Some(output_path),
                        command: project.indexer.to_string(),
                        status: "FAILED".to_string(),
                        error_message: e.to_string(),
                    });
                }
            }
        }
        if prep.plan.scip.strategy != ScipStrategy::Noop {
            match scip::persist_revision_scip_rows(
                &pg,
                org_id,
                payload.repo_id,
                &prep.branch,
                &prep.commit_hash,
                &scip_out,
                &successful_projects,
                &failed_projects,
                &revision_root,
            )
            .await
            {
                Ok(stats) => eprintln!(
                    "SCIP persisted branch={} languages={} symbols={} occurrences={} relationships={}",
                    prep.branch,
                    stats.language_count,
                    stats.symbol_count,
                    stats.occurrence_count,
                    stats.relationship_count
                ),
                Err(e) => {
                    let msg = format!("SCIP persist branch={}: {}", prep.branch, e);
                    let manifest_ids_for_failure =
                        prepared.iter().map(|p| p.id.clone()).collect::<Vec<_>>();
                    let _ = manifest_mgr
                        .mark_manifests_failed(&manifest_ids_for_failure, &msg)
                        .await;
                    mark_failed(&pg, &payload.job_id, &msg).await;
                    return Err(asynq_err(msg));
                }
            }
        }
        eprintln!(
            "SCIP summary branch={} ok={} missing={} failed={}",
            prep.branch, ok, missing, failed
        );

        // Q.A: re-ingest the .scip artifacts for this branch into
        // in-memory rows so the AST/graph build pass below can
        // project SCIP semantic relationships into Nebula with
        // provenance="scip" + confidence>=0.95. The cost (one
        // extra protobuf decode per artifact) is documented;
        // Q.D collapses this with the persist-pass into a single
        // ingest. We tolerate per-artifact failures because the
        // SCIP indexer itself already succeeded for this project
        // — a decode error here is recoverable (we just lose the
        // graph projection for this language).
        let mut branch_scip_rows: Vec<(String, crate::scip::ScipIngestRows)> = Vec::new();
        for project in &successful_projects {
            match crate::scip::ingest_scip_artifact_rows(
                &project.artifact_path,
                &project.language,
                &project.project_root,
                &revision_root,
            ) {
                Ok(rows) => branch_scip_rows.push((project.language.clone(), rows)),
                Err(e) => eprintln!(
                    "SCIP graph projection skipped lang={} root={}: re-ingest failed: {}",
                    project.language, project.project_root, e
                ),
            }
        }
        if !branch_scip_rows.is_empty() {
            scip_rows_by_branch.insert(prep.branch.clone(), branch_scip_rows);
        }
    }

    // 4d. Syntactic AST scan (R.9k). For each prepared
    //     manifest whose plan.graph.strategy != NOOP, walk
    //     the materialized revision once and dispatch matching
    //     files to the selected language extractors. This avoids
    //     the old N-languages × repo-tree traversal cost for
    //     polyglot repositories.
    //     Fact->CodeGraphSnapshot conversion + NGQL string
    //     emission stays deferred to R.9k-ii. The wiring
    //     here proves the extractors + walker run
    //     end-to-end against real cloned repos and the
    //     per-revision plan.graph.strategy gate works.
    //
    //     Env gate: CODEINTEL_CODE_GRAPH_ENABLED (default
    //     "false"). Off-by-default matches legacy production
    //     defaults — explicit opt-in required before the
    //     worker spawns the (CPU-bound regex pass) on every
    //     repo.
    let code_graph_enabled = std::env::var("CODEINTEL_CODE_GRAPH_ENABLED")
        .map(|v| v == "true")
        .unwrap_or(false);
    if code_graph_enabled {
        const LANGUAGES: &[&str] = &[
            "typescript",
            "javascript",
            "go",
            "python",
            "java",
            "ruby",
            "csharp",
            "rust",
            "dart",
        ];
        let mut graph_payloads: Vec<(String, CodeGraphWritePayload)> = Vec::new();
        let prepared_manifest_ids: Vec<String> = prepared.iter().map(|p| p.id.clone()).collect();
        for prep in &prepared {
            if prep.plan.graph.strategy == crate::delta_plan::GraphStrategy::Noop {
                continue;
            }
            let revision_root = match ensure_revision_snapshot(
                &dest,
                &storage_root,
                payload.repo_id,
                &prep.commit_hash,
                &mut materialized_revision_roots,
            ) {
                Ok(path) => path,
                Err(e) => {
                    let msg = format!("materialize AST revision {}: {}", prep.branch, e);
                    let _ = manifest_mgr
                        .mark_manifests_failed(&prepared_manifest_ids, &msg)
                        .await;
                    mark_failed(&pg, &payload.job_id, &msg).await;
                    return Err(asynq_err(msg));
                }
            };

            // R.9k-iii-b: build a per-revision
            // GraphAccumulator. Scope is the legacy 7-tuple
            // (org_id / workspace_id / repo_id / revision /
            // commit_hash + optional schema_version +
            // builder_version). The repo vertex's VID is
            // computed via the legacy "repo" + "repo:<id>"
            // identity convention.
            //
            // Critic-correctness: the 7 byte-parity divergences
            // that killed R.9k-ii (UPPERCASE edge kinds,
            // ast-${lang} source tag, per-batch tier with 0.8
            // threshold, target-kind remap, file-vertex
            // coalescing for module sources, edge_rank input
            // shape, normalizedKey-NULL on AST edges) are all
            // covered by the fact_to_snapshot port.
            let cg_scope = crate::code_graph_model::CodeGraphScope {
                org_id: org_id as i64,
                repo_id: payload.repo_id as i64,
                revision: prep.branch.clone(),
                commit_hash: prep.commit_hash.clone(),
                workspace_id: workspace_id.clone(),
                schema_version: None,
                builder_version: None,
            };
            let repo_vid = match crate::code_graph_model::build_code_graph_vertex_id(
                &crate::code_graph_model::CodeGraphVertexIdentity {
                    scope: cg_scope.clone(),
                    kind: "repo".to_string(),
                    key: format!("repo:{}", payload.repo_id),
                },
            ) {
                Ok(v) => v,
                Err(e) => {
                    eprintln!(
                        "AST snapshot skipped branch={}: build_code_graph_vertex_id: {}",
                        prep.branch, e
                    );
                    continue;
                }
            };

            let mut graph = crate::fact_to_snapshot::GraphAccumulator::default();
            let mut total_facts: usize = 0;
            let scan_root = revision_root.clone();
            let scan_results = match tokio::task::spawn_blocking(move || {
                crate::ast_extractor::scan_syntactic_ast_facts_multi(
                    LANGUAGES,
                    crate::ast_extractor::SyntacticAstScanOptions {
                        repo_root: &scan_root,
                        max_files: None,
                        max_file_bytes: None,
                        max_directories: None,
                    },
                )
            })
            .await
            {
                Ok(result) => result,
                Err(e) => {
                    let msg = format!(
                        "AST multi-language scan task failed branch={}: {}",
                        prep.branch, e
                    );
                    let _ = manifest_mgr
                        .mark_manifests_failed(&prepared_manifest_ids, &msg)
                        .await;
                    mark_failed(&pg, &payload.job_id, &msg).await;
                    return Err(asynq_err(msg));
                }
            };
            for (lang_name, result) in scan_results {
                if !result.facts.is_empty() {
                    eprintln!(
                        "AST scan {}: branch={} lang={} facts={} scanned={} skipped={}",
                        prep.id,
                        prep.branch,
                        lang_name,
                        result.facts.len(),
                        result.scanned_file_count,
                        result.skipped_file_count,
                    );
                    total_facts += result.facts.len();
                    if let Err(e) = crate::fact_to_snapshot::add_syntactic_language_ast_facts(
                        &mut graph,
                        &cg_scope,
                        &repo_vid,
                        &lang_name,
                        &result.facts,
                    ) {
                        eprintln!(
                            "AST snapshot warn lang={} branch={}: {}",
                            lang_name, prep.branch, e
                        );
                    }
                }
                for warn in &result.warnings {
                    eprintln!("AST scan {} warn: {}", lang_name, warn);
                }
            }

            // Q.A: project SCIP semantic relationships into the
            // same accumulator. Adds vertices + edges with
            // provenance="scip" and confidence>=0.95 — the
            // post-pass below collapses (from,to,kind) duplicates
            // produced by AST regex vs SCIP, keeping the higher-
            // confidence peer. This is the single biggest
            // quality-overhaul fix: without it, the Nebula graph
            // is uniformly 0.6-tier regex output. With it,
            // semantic CALLS/REFERENCES/IMPLEMENTS land at 0.95
            // and dominate retrieval ranking.
            let mut scip_edges_added = 0usize;
            let mut scip_languages = 0usize;
            if let Some(rows_by_lang) = scip_rows_by_branch.get(&prep.branch) {
                let edges_before = graph.edges.len();
                for (lang, rows) in rows_by_lang {
                    if let Err(e) = crate::fact_to_snapshot::add_scip_semantic_facts(
                        &mut graph, &cg_scope, &repo_vid, lang, rows,
                    ) {
                        eprintln!(
                            "SCIP semantic projection warn lang={} branch={}: {}",
                            lang, prep.branch, e
                        );
                        continue;
                    }
                    scip_languages += 1;
                }
                scip_edges_added = graph.edges.len().saturating_sub(edges_before);
            }

            // Q.A: collapse (from,to,kind) duplicates across
            // sources so the AST 0.6 regex edge doesn't survive
            // alongside the SCIP 0.95 semantic edge. Highest-
            // confidence-wins; tie-breaker by provenance rank.
            let edges_before_reconcile = graph.edges.len();
            crate::fact_to_snapshot::reconcile_edge_provenance(&mut graph);
            let edges_after_reconcile = graph.edges.len();
            if scip_languages > 0 || edges_before_reconcile != edges_after_reconcile {
                eprintln!(
                    "code-graph projection branch={} scip_langs={} scip_edges_added={} reconciler_dropped={}",
                    prep.branch,
                    scip_languages,
                    scip_edges_added,
                    edges_before_reconcile - edges_after_reconcile
                );
            }

            // Convert + render NGQL statements.
            let (snapshot, anchors) =
                crate::fact_to_snapshot::accumulator_to_snapshot_and_anchors(graph);
            if snapshot.vertices.is_empty() && snapshot.edges.is_empty() {
                let msg = format!(
                    "AST snapshot empty branch={} after graph plan requested extraction",
                    prep.branch
                );
                let _ = manifest_mgr
                    .mark_manifests_failed(&prepared_manifest_ids, &msg)
                    .await;
                mark_failed(&pg, &payload.job_id, &msg).await;
                return Err(asynq_err(msg));
            }
            let stmts = crate::nebula_ngql::render_snapshot_statements(&snapshot);
            eprintln!(
                "AST snapshot built branch={} facts={} vertices={} edges={} anchors={} ngql_statements={}",
                prep.branch,
                total_facts,
                snapshot.vertices.len(),
                snapshot.edges.len(),
                anchors.len(),
                stmts.len(),
            );

            // R.9l: hand the rendered NGQL off to the Go-side
            // executor via the code-graph-write asynq queue.
            // Contract: enqueue must succeed before the job can
            // activate manifests. Redis/asynq outage is a real index
            // failure because otherwise Atom could see a READY repo
            // with no retriable graph write task.
            if stmts.is_empty() {
                eprintln!("AST snapshot empty branch={} — no enqueue", prep.branch);
                continue;
            }
            let cg_payload = CodeGraphWritePayload {
                org_id: cg_scope.org_id,
                workspace_id: cg_scope.workspace_id.clone(),
                repo_id: cg_scope.repo_id,
                branch: prep.branch.clone(),
                revision: cg_scope.revision.clone(),
                commit_hash: cg_scope.commit_hash.clone(),
                schema_version: crate::code_graph_model::get_schema_version(&cg_scope),
                builder_version: crate::code_graph_model::get_builder_version(&cg_scope),
                index_job_id: payload.job_id.clone(),
                manifest_id: prep.id.clone(),
                provider_connection_id: provider_connection_id.clone(),
                source: "syntactic-ast".to_string(),
                statements: stmts,
                anchors,
            };
            if let Err(e) = validate_code_graph_payload_budget(&cg_payload) {
                let msg = format!("AST snapshot payload invalid branch={}: {}", prep.branch, e);
                let _ = manifest_mgr
                    .mark_manifests_failed(&prepared_manifest_ids, &msg)
                    .await;
                mark_failed(&pg, &payload.job_id, &msg).await;
                return Err(asynq_err(msg));
            }
            graph_payloads.push((prep.branch.clone(), cg_payload));
        }

        for (branch, cg_payload) in graph_payloads {
            match enqueue_code_graph_write(&redis_url, &cg_payload).await {
                Ok(task_id) => eprintln!(
                    "AST snapshot enqueued branch={} task_id={} queue={}",
                    branch, task_id, CODE_GRAPH_WRITE_QUEUE
                ),
                Err(e) => {
                    let msg = format!("AST snapshot enqueue failed branch={}: {}", branch, e);
                    let _ = manifest_mgr
                        .mark_manifests_failed(&prepared_manifest_ids, &msg)
                        .await;
                    return Err(asynq_err(msg));
                }
            }
        }
    } else {
        eprintln!(
            "AST scan skipped: CODEINTEL_CODE_GRAPH_ENABLED != 'true' (job_id={})",
            payload.job_id
        );
    }

    // 5a. Stamp metadata.indexedRevisions (R.4). Mirrors the
    //     legacy `repo.metadata = { ...metadata, indexedRevisions: [...] }`
    //     write at the end of indexRepository. The status route
    //     (Go side, P.3c) reads this column to render
    //     branchStatuses[].
    //
    //     Critic-gate fix (R.7-c1-ordering): this stamp MUST
    //     run BEFORE activate_manifests so a concurrent
    //     /status reader on another pod sees consistent state
    //     (READY manifests + matching Repo.metadata.
    //     indexedRevisions). The legacy
    //     repoIndexManager.ts:602-622 has the same ordering:
    //     stamp metadata, mark Repo successful, THEN activate.
    metadata.indexed_revisions = Some(revisions.clone());
    let new_metadata_json = serde_json::to_value(&metadata)
        .map_err(|e| asynq_err(format!("serialise metadata: {}", e)))?;
    pg.execute(
        r#"
        UPDATE "Repo"
        SET metadata    = $2::jsonb,
            "updatedAt" = NOW()
        WHERE id = $1 AND "orgId" = $3
        "#,
        &[&payload.repo_id, &new_metadata_json, &org_id],
    )
    .await
    .map_err(pg_err)?;

    // 5b. RecordSuccessfulIndex on Repo.
    pg.execute(
        r#"
        UPDATE "Repo"
        SET "indexedAt"         = NOW(),
            "indexedCommitHash" = $2,
            "updatedAt"         = NOW()
        WHERE id = $1 AND "orgId" = $3
        "#,
        &[&payload.repo_id, &cloned.commit_hash, &org_id],
    )
    .await
    .map_err(pg_err)?;

    // 5c. Activate the prepared manifests. After Repo metadata
    //     + Repo.indexedAt are durable, flip the PENDING rows
    //     to READY and supersede any prior READY in the same
    //     scope. This is the atomic point at which a /status
    //     route call surfaces the new manifests — the Repo
    //     columns are already consistent by the time any
    //     reader sees READY.
    let manifest_ids: Vec<String> = prepared.iter().map(|p| p.id.clone()).collect();
    if let Err(e) = manifest_mgr.activate_manifests(org_id, &manifest_ids).await {
        let msg = format!("activate_manifests: {}", e);
        // Best-effort: mark these manifests FAILED before we
        // bubble the error out. The job itself fails too.
        let _ = manifest_mgr
            .mark_manifests_failed(&manifest_ids, &msg)
            .await;
        mark_failed(&pg, &payload.job_id, &msg).await;
        return Err(asynq_err(msg));
    }

    // 6. MarkCompleted on the job row.
    pg.execute(
        r#"
        UPDATE "RepoIndexingJob"
        SET status        = 'COMPLETED',
            "completedAt" = NOW(),
            "updatedAt"   = NOW()
        WHERE id = $1 AND "repoId" = $2
        "#,
        &[&payload.job_id, &payload.repo_id],
    )
    .await
    .map_err(pg_err)?;

    // 7. Refresh the per-repo latest-job denormalisation
    //    column (parity with the Go side's
    //    RefreshRepoLatestIndexingJobStatus). The clone +
    //    record path leaves the most recent job as COMPLETED;
    //    the UPDATE here mirrors that.
    pg.execute(
        r#"
        UPDATE "Repo" r
        SET "latestIndexingJobStatus" = sub.status
        FROM (
            SELECT j.status
            FROM   "RepoIndexingJob" j
            WHERE  j."repoId" = $1
            ORDER  BY j."createdAt" DESC NULLS LAST
            LIMIT  1
        ) sub
        WHERE r.id = $1 AND r."orgId" = $2
        "#,
        &[&payload.repo_id, &org_id],
    )
    .await
    .map_err(pg_err)?;

    eprintln!(
        "INDEX completed: job_id={} repo_id={} org_id={} head={}",
        payload.job_id, payload.repo_id, org_id, cloned.commit_hash
    );
    Ok(())
}

/// ENQUEUE_TIMEOUT bounds the total time the Rust worker
/// will block trying to hand off an NGQL batch to the
/// asynq queue. A stalled Redis must NOT block the indexing
/// loop indefinitely. Enqueue failure is surfaced to asynq so the
/// index job retries rather than activating a repo with no graph task.
const ENQUEUE_TIMEOUT: std::time::Duration = std::time::Duration::from_secs(15);
const MAX_CODE_GRAPH_PAYLOAD_BYTES: usize = 16 * 1024 * 1024;
const MAX_CODE_GRAPH_STATEMENT_COUNT: usize = 20_000;
const MAX_CODE_GRAPH_STATEMENT_BYTES: usize = 1024 * 1024;

fn tenant_storage_root(data_cache_dir: &str, org_id: i32) -> PathBuf {
    let layout = env::var("CODEINTEL_ZOEKT_STORAGE_LAYOUT").unwrap_or_default();
    if layout == "org-directory" {
        let root = env::var("CODEINTEL_ZOEKT_EFS_ROOT")
            .ok()
            .filter(|v| !v.trim().is_empty())
            .unwrap_or_else(|| data_cache_dir.to_string());
        return Path::new(&root).join(org_id.to_string());
    }
    Path::new(data_cache_dir).to_path_buf()
}

fn revision_snapshot_path(storage_root: &Path, repo_id: i32, commit_hash: &str) -> PathBuf {
    storage_root
        .join("codeintel")
        .join("revision-snapshots")
        .join(repo_id.to_string())
        .join(commit_hash)
}

fn ensure_revision_snapshot(
    repo_path: &Path,
    storage_root: &Path,
    repo_id: i32,
    commit_hash: &str,
    materialized_roots: &mut BTreeSet<PathBuf>,
) -> anyhow::Result<PathBuf> {
    let revision_root = revision_snapshot_path(storage_root, repo_id, commit_hash);
    if materialized_roots.contains(&revision_root) {
        return Ok(revision_root);
    }
    materialize_revision_tree_once(repo_path, commit_hash, &revision_root)?;
    materialized_roots.insert(revision_root.clone());
    Ok(revision_root)
}

fn materialize_revision_tree_once(
    repo_path: &Path,
    commit_hash: &str,
    revision_root: &Path,
) -> anyhow::Result<()> {
    let ready_marker = revision_snapshot_ready_marker(revision_root);
    if revision_root.exists() && ready_marker.exists() {
        return Ok(());
    }
    if ready_marker.exists() {
        let _ = std::fs::remove_file(&ready_marker);
    }
    manifest_files::materialize_revision_tree(repo_path, commit_hash, revision_root)?;
    std::fs::write(&ready_marker, b"ready\n")
        .with_context(|| format!("write revision snapshot marker {}", ready_marker.display()))?;
    Ok(())
}

fn revision_snapshot_ready_marker(revision_root: &Path) -> PathBuf {
    let mut marker = revision_root.as_os_str().to_os_string();
    marker.push(".ready");
    PathBuf::from(marker)
}

/// enqueue_code_graph_write produces an asynq task on the
/// `code-graph-write` queue carrying the rendered NGQL
/// statements + snapshot scope. The Go-side consumer
/// (R.9l-go follow-up) decodes the payload and feeds the
/// statements to the production nebulaCodeGraphStore +
/// nebulaclient wrappers.
///
/// Returns the asynq task_id on success so the caller can
/// log it for cross-pod correlation.
///
/// This opens + tears down a fresh asynq client per call.
/// Acceptable for the indexing job's revision-loop rate
/// (one enqueue per non-NOOP revision per job — bounded by
/// the per-revision plan.graph.strategy). Perf-debt note:
/// if profiling shows the per-call client cost matters,
/// hoist a long-lived client to the worker level.
///
/// Critic-gate fix (C3.3): wrapped in
/// `tokio::time::timeout(ENQUEUE_TIMEOUT, ...)` so a Redis
/// stall fails the enqueue and asynq retries the index job
/// rather than blocking the entire indexing loop.
async fn enqueue_code_graph_write(
    redis_url: &str,
    payload: &CodeGraphWritePayload,
) -> anyhow::Result<String> {
    tokio::time::timeout(
        ENQUEUE_TIMEOUT,
        enqueue_code_graph_write_inner(redis_url, payload),
    )
    .await
    .map_err(|_| {
        anyhow!(
            "enqueue code-graph task timed out after {:?}",
            ENQUEUE_TIMEOUT
        )
    })?
}

async fn enqueue_code_graph_write_inner(
    redis_url: &str,
    payload: &CodeGraphWritePayload,
) -> anyhow::Result<String> {
    validate_code_graph_payload_budget(payload)?;
    let bytes =
        serde_json::to_vec(payload).map_err(|e| anyhow!("encode code-graph payload: {}", e))?;
    if bytes.len() > MAX_CODE_GRAPH_PAYLOAD_BYTES {
        return Err(anyhow!(
            "code-graph payload size {} exceeds {} bytes",
            bytes.len(),
            MAX_CODE_GRAPH_PAYLOAD_BYTES
        ));
    }
    let redis_config = asynq::backend::RedisConnectionType::single(redis_url.to_string())
        .map_err(|e| anyhow!("redis config for code-graph enqueue: {}", e))?;
    let client = asynq::client::Client::new(redis_config)
        .await
        .map_err(|e| anyhow!("asynq client: {}", e))?;
    let task = asynq::task::Task::new(CODE_GRAPH_WRITE_TASK_TYPE, &bytes)
        .map_err(|e| anyhow!("build code-graph task: {}", e))?
        .with_queue(CODE_GRAPH_WRITE_QUEUE);
    let info = client
        .enqueue(task)
        .await
        .map_err(|e| anyhow!("enqueue code-graph task: {}", e))?;
    Ok(info.id.clone())
}

fn build_index_plan_write_request(
    task: &TaskPayload,
    org_id: i32,
    workspace_id: &str,
    repo_path: &Path,
    prepared: &[PreparedIndexManifest],
    scip_projects_by_branch: &mut BTreeMap<String, Vec<scip::ScipProject>>,
) -> anyhow::Result<WriteIndexPlanRequest> {
    let scip_enabled = std::env::var("CODEINTEL_SCIP_INDEXING_ENABLED")
        .map(|v| v == "true")
        .unwrap_or(false);
    let graph_enabled = std::env::var("CODEINTEL_CODE_GRAPH_ENABLED")
        .map(|v| v == "true")
        .unwrap_or(false);
    let mut revisions = Vec::with_capacity(prepared.len());
    for prep in prepared {
        let mut scip_projects = Vec::new();
        if scip_enabled && prep.plan.scip.strategy != ScipStrategy::Noop {
            let files =
                manifest_files::build_manifest_files_for_revision(repo_path, &prep.commit_hash)
                    .with_context(|| {
                        format!(
                            "build manifest files for plan handoff branch={} commit={}",
                            prep.branch, prep.commit_hash
                        )
                    })?;
            let mut projects = scip::detect_scip_projects(&files);
            scip::add_inferred_typescript_projects(&files, &mut projects);
            let mut projects = scip::filter_projects_for_worker_classes(projects);
            let project_cap = scip::max_project_roots_per_revision();
            if projects.len() > project_cap {
                projects.truncate(project_cap);
            }
            scip_projects_by_branch.insert(prep.branch.clone(), projects.clone());
            scip_projects.reserve(projects.len());
            for project in projects {
                scip_projects.push(ScipProjectPlan {
                    language: project.language.to_string(),
                    project_root: project.project_root,
                    indexer: project.indexer.to_string(),
                    scip_worker_class: project.worker_class.to_string(),
                    toolchain_digest: None,
                    image_digest: None,
                    project_input_hash: None,
                });
            }
        }
        let run_graph_layers =
            graph_enabled && prep.plan.graph.strategy != crate::delta_plan::GraphStrategy::Noop;
        let run_activate = run_graph_layers || !scip_projects.is_empty();
        if !run_activate {
            continue;
        }
        revisions.push(IndexPlanRevision {
            workspace_id: workspace_id.to_string(),
            branch: prep.branch.clone(),
            revision: prep.branch.clone(),
            commit_hash: prep.commit_hash.clone(),
            run_ast_tree_sitter: run_graph_layers,
            run_graph_merge: run_graph_layers,
            run_activate,
            scip_projects,
        });
    }
    Ok(WriteIndexPlanRequest {
        index_job_id: task.job_id.clone(),
        org_id,
        repo_id: task.repo_id,
        max_attempts: 3,
        revisions,
    })
}

async fn write_index_plan(
    backend_grpc_addr: &str,
    request: WriteIndexPlanRequest,
) -> anyhow::Result<WriteIndexPlanResponse> {
    let endpoint = normalize_grpc_endpoint(backend_grpc_addr);
    tokio::time::timeout(ENQUEUE_TIMEOUT, write_index_plan_inner(&endpoint, request))
        .await
        .map_err(|_| {
            anyhow!(
                "index-plan gRPC handoff timed out after {:?}",
                ENQUEUE_TIMEOUT
            )
        })?
}

async fn write_index_plan_inner(
    endpoint: &str,
    request: WriteIndexPlanRequest,
) -> anyhow::Result<WriteIndexPlanResponse> {
    if request.revisions.is_empty() {
        return Err(anyhow!("index-plan request has no revisions"));
    }
    let channel = Endpoint::from_shared(endpoint.to_string())
        .map_err(|e| anyhow!("index-plan endpoint {}: {}", endpoint, e))?
        .connect_timeout(std::time::Duration::from_secs(5))
        .timeout(ENQUEUE_TIMEOUT)
        .connect()
        .await
        .map_err(|e| anyhow!("connect index-plan endpoint {}: {}", endpoint, e))?;
    let mut client = IndexPlanServiceClient::new(channel);
    let response = client
        .write_plan(request)
        .await
        .map_err(|e| anyhow!("WritePlan RPC: {}", e))?;
    Ok(response.into_inner())
}

fn index_plan_grpc_addr() -> Option<String> {
    std::env::var("CODEINTEL_INDEX_PLAN_GRPC_ADDR")
        .ok()
        .filter(|v| !v.trim().is_empty())
}

fn index_plan_write_required() -> bool {
    std::env::var("CODEINTEL_INDEX_PLAN_WRITE_REQUIRED")
        .map(|v| v == "true")
        .unwrap_or(false)
}

fn zoekt_file_delta_enabled() -> bool {
    match std::env::var("CODEINTEL_ZOEKT_FILE_DELTA") {
        Ok(value) => value.trim().eq_ignore_ascii_case("true"),
        Err(_) => true,
    }
}

fn zoekt_delta_shard_threshold() -> u64 {
    match std::env::var("CODEINTEL_ZOEKT_DELTA_SHARD_THRESHOLD") {
        Ok(value) => value.trim().parse::<u64>().unwrap_or(150),
        Err(_) => 150,
    }
}

fn normalize_grpc_endpoint(addr: &str) -> String {
    let trimmed = addr.trim();
    if trimmed.contains("://") {
        trimmed.to_string()
    } else {
        format!("http://{}", trimmed)
    }
}

fn validate_code_graph_payload_budget(payload: &CodeGraphWritePayload) -> anyhow::Result<()> {
    if payload.statements.len() > MAX_CODE_GRAPH_STATEMENT_COUNT {
        return Err(anyhow!(
            "code-graph statement count {} exceeds {}",
            payload.statements.len(),
            MAX_CODE_GRAPH_STATEMENT_COUNT
        ));
    }
    for (idx, stmt) in payload.statements.iter().enumerate() {
        if stmt.len() > MAX_CODE_GRAPH_STATEMENT_BYTES {
            return Err(anyhow!(
                "code-graph statement {} size {} exceeds {} bytes",
                idx,
                stmt.len(),
                MAX_CODE_GRAPH_STATEMENT_BYTES
            ));
        }
    }
    let bytes =
        serde_json::to_vec(payload).map_err(|e| anyhow!("encode code-graph payload: {}", e))?;
    if bytes.len() > MAX_CODE_GRAPH_PAYLOAD_BYTES {
        return Err(anyhow!(
            "code-graph payload size {} exceeds {} bytes",
            bytes.len(),
            MAX_CODE_GRAPH_PAYLOAD_BYTES
        ));
    }
    Ok(())
}

/// fetch_connection_ids returns the RepoToConnection.connectionId
/// list for a repo, used by the manifest manager's
/// providerConnectionId scope key. Matches the legacy
/// `repo.connections.map(c => c.connectionId)` query
/// (indexManifestManager.ts:330-334).
///
/// R.7-defense: filters by BOTH repoId AND orgId via the
/// `Connection` join. The DB schema already enforces
/// repo→org uniqueness, so this is defense-in-depth — a
/// future misrouted call with a stale repo_id can't leak a
/// connection from another tenant. The HPA-safety mandate
/// in architecture rules names this exact pattern ("every per-tenant
/// read filters by orgId").
async fn fetch_connection_ids(
    pg: &Arc<tokio_postgres::Client>,
    repo_id: i32,
    org_id: i32,
) -> anyhow::Result<Vec<i32>> {
    let rows = pg
        .query(
            r#"SELECT rtc."connectionId"
               FROM "RepoToConnection" rtc
               JOIN "Connection" c ON c.id = rtc."connectionId"
               WHERE rtc."repoId" = $1 AND c."orgId" = $2"#,
            &[&repo_id, &org_id],
        )
        .await?;
    Ok(rows.iter().map(|r| r.get::<_, i32>(0)).collect())
}

/// fetch_workspace_id mirrors legacy `resolveWorkspaceId`
/// (indexManifestManager.ts:262-272). Looks up the Org row's
/// `atomWorkspaceId` (preferred, since it's the explicit
/// Atom-integration workspace identifier), falling back to
/// `domain`, falling back to `org-<id>` if both are NULL.
async fn fetch_workspace_id(
    pg: &Arc<tokio_postgres::Client>,
    org_id: i32,
) -> anyhow::Result<String> {
    let row = pg
        .query_opt(
            r#"SELECT "atomWorkspaceId", "domain" FROM "Org" WHERE id = $1"#,
            &[&org_id],
        )
        .await?;
    let Some(row) = row else {
        return Ok(format!("org-{}", org_id));
    };
    let atom_workspace_id: Option<String> = row.get(0);
    let domain: Option<String> = row.get(1);
    Ok(atom_workspace_id
        .or(domain)
        .unwrap_or_else(|| format!("org-{}", org_id)))
}

/// CODE_GRAPH_SEMANTIC_PROMPT_VERSION mirrors the legacy
/// constant in `codeGraph/semanticGraphBuilder.ts:46`. The
/// fingerprint identifies WHICH prompt + model + schema
/// produced semantic facts; changing it invalidates the
/// cached semantic chunks (per delta_plan.rs's
/// `semantic_fingerprint_changed` gate).
const CODE_GRAPH_SEMANTIC_PROMPT_VERSION: &str = "graphify-governed-v1";
/// CODE_GRAPH_SEMANTIC_EXTRACTION_VERSION mirrors
/// semanticGraphBuilder.ts:47.
const CODE_GRAPH_SEMANTIC_EXTRACTION_VERSION: i64 = 1;

/// fetch_semantic_extraction_fingerprint mirrors legacy
/// `resolveSemanticExtractionFingerprint`
/// (indexManifestManager.ts:274-298).
///
/// Gated on env `CODEINTEL_CODE_GRAPH_SEMANTIC_EXTRACTION_ENABLED`.
/// When disabled, returns Ok(None) — semantic delta planning
/// then treats every prior fingerprint as a "no-op" match.
///
/// When enabled, queries OrgLanguageModel for the first
/// `enabled=true` row ordered by (order ASC, id ASC), reads
/// the JSON `config` field, and builds a `modelId` string of
/// the form `<provider>/<model>` (or "none" if either is
/// missing OR no row matched). promptVersion + schemaVersion
/// are the two constants above.
///
/// Critic-gate fix (F1/F3): pg errors PROPAGATE via Result.
/// Legacy Prisma findFirst returns null on missing-row (which
/// the "none" fallback handles) but THROWS on connection /
/// SQL errors (which propagate up + fail the job). Mirror
/// both by returning Result<Option<...>>.
async fn fetch_semantic_extraction_fingerprint(
    pg: &Arc<tokio_postgres::Client>,
    org_id: i32,
) -> anyhow::Result<Option<SemanticExtractionFingerprint>> {
    if std::env::var("CODEINTEL_CODE_GRAPH_SEMANTIC_EXTRACTION_ENABLED")
        .map(|v| v != "true")
        .unwrap_or(true)
    {
        return Ok(None);
    }

    let row = pg
        .query_opt(
            r#"SELECT config FROM "OrgLanguageModel"
               WHERE "orgId" = $1 AND enabled = TRUE
               ORDER BY "order" ASC, id ASC
               LIMIT 1"#,
            &[&org_id],
        )
        .await?;

    // Per legacy: missing row → modelId="none" (fingerprint
    // still emitted because promptVersion + schemaVersion
    // participate in the cache key independently).
    let model_id = match row {
        Some(row) => {
            let config: serde_json::Value = row.get(0);
            let provider = config
                .get("provider")
                .and_then(|v| v.as_str())
                .unwrap_or("");
            let model = config.get("model").and_then(|v| v.as_str()).unwrap_or("");
            if !provider.is_empty() && !model.is_empty() {
                format!("{}/{}", provider, model)
            } else {
                "none".to_string()
            }
        }
        None => "none".to_string(),
    };

    Ok(Some(SemanticExtractionFingerprint {
        prompt_version: CODE_GRAPH_SEMANTIC_PROMPT_VERSION.to_string(),
        model_id,
        schema_version: CODE_GRAPH_SEMANTIC_EXTRACTION_VERSION,
    }))
}

/// mark_failed updates the RepoIndexingJob row's status to
/// FAILED with the given error message. Errors-during-error-
/// reporting are logged and swallowed: the outer handler still
/// surfaces the original failure to asynq so the task gets
/// retried per policy.
async fn mark_failed(pg: &Arc<tokio_postgres::Client>, job_id: &str, err_msg: &str) {
    if let Err(e) = pg
        .execute(
            r#"
            UPDATE "RepoIndexingJob"
            SET status         = 'FAILED',
                "errorMessage" = $2,
                "completedAt"  = NOW(),
                "updatedAt"    = NOW()
            WHERE id = $1
            "#,
            &[&job_id, &err_msg],
        )
        .await
    {
        eprintln!("mark_failed: secondary error (job={}): {}", job_id, e);
    }
}

/// Blocking variant for callsites inside non-async error
/// pipelines (rare). The underlying tokio runtime drives the
/// future to completion via block_on; safe because these
/// failure paths are not on the hot path.
fn mark_failed_blocking(pg: &Arc<tokio_postgres::Client>, job_id: &str, err_msg: &str) {
    let pg = pg.clone();
    let job_id = job_id.to_string();
    let err_msg = err_msg.to_string();
    // Spawn — we're already inside the tokio runtime here.
    tokio::spawn(async move {
        mark_failed(&pg, &job_id, &err_msg).await;
    });
}

/// asynq_err builds an asynq::error::Error from any
/// stringifiable thing so the handler signature lines up
/// with ServeMux::handle_async_func's expected
/// Result<(), asynq::Error>. The struct-variant `Other`
/// shape is asynq 0.1.8 — kept as a tagged struct so future
/// upgrades that add fields can wire them in without
/// changing call sites.
fn asynq_err<E: std::fmt::Display>(e: E) -> AsynqError {
    AsynqError::Other {
        message: format!("{}", e),
    }
}

/// pg_err shorthand for tokio_postgres::Error -> AsynqError.
fn pg_err(e: tokio_postgres::Error) -> AsynqError {
    asynq_err(e)
}

/// scrub_creds masks user:pass in Redis/Postgres URLs before
/// logging.
fn scrub_creds(url: &str) -> String {
    if let Some(at_idx) = url.find('@') {
        if let Some(scheme_idx) = url.find("://") {
            let prefix = &url[..scheme_idx + 3];
            let suffix = &url[at_idx..];
            return format!("{}***{}", prefix, suffix);
        }
    }
    url.to_string()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{Mutex, OnceLock};

    fn env_lock() -> &'static Mutex<()> {
        static LOCK: OnceLock<Mutex<()>> = OnceLock::new();
        LOCK.get_or_init(|| Mutex::new(()))
    }

    #[test]
    fn tenant_storage_root_uses_org_directory_efs_root() {
        let _guard = env_lock().lock().unwrap();
        env::set_var("CODEINTEL_ZOEKT_STORAGE_LAYOUT", "org-directory");
        env::set_var("CODEINTEL_ZOEKT_EFS_ROOT", "/efs/codeintel/zoekt");
        let got = tenant_storage_root("/tmp/codeintel", 654);
        assert_eq!(got, PathBuf::from("/efs/codeintel/zoekt/654"));
        env::remove_var("CODEINTEL_ZOEKT_STORAGE_LAYOUT");
        env::remove_var("CODEINTEL_ZOEKT_EFS_ROOT");
    }

    #[test]
    fn tenant_storage_root_flat_layout_keeps_data_cache_dir() {
        let _guard = env_lock().lock().unwrap();
        env::remove_var("CODEINTEL_ZOEKT_STORAGE_LAYOUT");
        env::remove_var("CODEINTEL_ZOEKT_EFS_ROOT");
        let got = tenant_storage_root("/tmp/codeintel", 654);
        assert_eq!(got, PathBuf::from("/tmp/codeintel"));
    }

    #[test]
    fn revision_snapshots_live_under_codeintel_artifacts() {
        let got = revision_snapshot_path(Path::new("/efs/codeintel/zoekt/654"), 771, "abc123");
        assert_eq!(
            got,
            PathBuf::from("/efs/codeintel/zoekt/654/codeintel/revision-snapshots/771/abc123")
        );
    }
}
