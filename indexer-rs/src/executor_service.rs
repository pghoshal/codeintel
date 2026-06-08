use crate::ast_extractor::{self, SyntacticAstScanOptions};
use crate::proto::codeintel::v1::index_executor_service_server::{
    IndexExecutorService, IndexExecutorServiceServer,
};
use crate::proto::codeintel::v1::{ExecuteIndexSubjobRequest, ExecuteIndexSubjobResponse};
use crate::queue::CodeGraphWritePayload;
use crate::scip::{self, ScipProject};
use crate::zoekt;
use anyhow::{anyhow, Context, Result};
use serde::Serialize;
use sha2::{Digest, Sha256};
use std::collections::HashMap;
use std::fs;
use std::path::{Path, PathBuf};
use std::time::Duration;
use tonic::{Request, Response, Status};

#[derive(Debug, Clone)]
pub struct ExecutorConfig {
    pub listen_addr: String,
    pub data_cache_dir: String,
    pub artifact_root: String,
    pub zoekt_binary: String,
    pub scip_timeout: Duration,
}

pub async fn run(config: ExecutorConfig) -> Result<()> {
    let addr = config
        .listen_addr
        .parse()
        .with_context(|| format!("parse listen addr {}", config.listen_addr))?;
    let service = Service {
        data_cache_dir: PathBuf::from(config.data_cache_dir),
        artifact_root: PathBuf::from(config.artifact_root),
        zoekt_binary: PathBuf::from(config.zoekt_binary),
        scip_timeout: config.scip_timeout,
    };
    apply_process_resource_defaults();
    tonic::transport::Server::builder()
        .add_service(IndexExecutorServiceServer::new(service))
        .serve(addr)
        .await
        .with_context(|| "index executor gRPC server")?;
    Ok(())
}

fn apply_process_resource_defaults() {
    if std::env::var("GOMAXPROCS").is_err() {
        if let Ok(value) = std::env::var("CODEINTEL_SCIP_GO_MAX_PROCS") {
            let value = value.trim();
            if !value.is_empty() {
                std::env::set_var("GOMAXPROCS", value);
            }
        }
    }
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

#[derive(Debug, Clone)]
struct Service {
    data_cache_dir: PathBuf,
    artifact_root: PathBuf,
    zoekt_binary: PathBuf,
    scip_timeout: Duration,
}

#[tonic::async_trait]
impl IndexExecutorService for Service {
    async fn execute_subjob(
        &self,
        request: Request<ExecuteIndexSubjobRequest>,
    ) -> std::result::Result<Response<ExecuteIndexSubjobResponse>, Status> {
        let req = request.into_inner();
        validate_request(&req)?;
        let service = self.clone();
        let result = tokio::task::spawn_blocking(move || service.execute_blocking(req))
            .await
            .map_err(|e| Status::internal(format!("executor task join: {}", e)))?
            .map_err(|e| Status::internal(e.to_string()))?;
        Ok(Response::new(result))
    }
}

impl Service {
    fn execute_blocking(
        &self,
        req: ExecuteIndexSubjobRequest,
    ) -> Result<ExecuteIndexSubjobResponse> {
        match req.layer.as_str() {
            "ZOEKT" => self.execute_zoekt(req),
            "SCIP" => self.execute_scip(req),
            "AST_TREE_SITTER" => self.execute_ast(req),
            other => Err(anyhow!("unsupported executor layer {}", other)),
        }
    }

    fn execute_zoekt(&self, req: ExecuteIndexSubjobRequest) -> Result<ExecuteIndexSubjobResponse> {
        if self.zoekt_binary.as_os_str().is_empty() {
            return Err(anyhow!("zoekt binary is not configured"));
        }
        let storage_root = tenant_storage_root(&self.data_cache_dir, req.org_id);
        let worktree = storage_root.join("repos").join(req.repo_id.to_string());
        if !worktree.join(".git").exists() {
            return Err(anyhow!(
                "Zoekt worktree {} does not contain .git; clone must be on shared storage before ZOEKT subjob runs",
                worktree.display()
            ));
        }
        let index_dir = storage_root.join("index");
        std::fs::create_dir_all(&index_dir)
            .with_context(|| format!("mkdir Zoekt index dir {}", index_dir.display()))?;
        let shard_prefix = zoekt_branch_shard_prefix(req.org_id, req.repo_id, &req.branch);
        let branches = vec![zoekt_branch_arg(&req.branch)];
        let require_ctags = std::env::var("CODEINTEL_ZOEKT_REQUIRE_CTAGS")
            .map(|v| v == "true")
            .unwrap_or(false);
        let use_delta = zoekt_file_delta_enabled();
        let delta_threshold = zoekt_delta_shard_threshold();
        let language_map = std::env::var("CODEINTEL_ZOEKT_LANGUAGE_MAP")
            .unwrap_or_else(|_| zoekt::default_language_map().to_string());
        let result = zoekt::run(
            &self.zoekt_binary,
            zoekt::IndexRequest {
                repo_path: &worktree,
                index_dir: &index_dir,
                shard_prefix: &shard_prefix,
                tenant_id: req.org_id,
                repo_id: req.repo_id,
                branches: &branches,
                max_trigram_count: 0,
                file_limit_bytes: 0,
                large_file_patterns: &[],
                language_map: &language_map,
                require_ctags,
                ignore_dirs: zoekt::default_prune_dirs(),
                use_delta,
                delta_shard_number_fallback_threshold: delta_threshold,
            },
        )
        .with_context(|| "run Zoekt indexer")?;
        let all_shards = zoekt::list_repo_shards(&index_dir, &shard_prefix)
            .with_context(|| "list Zoekt shards after index")?;
        if all_shards.is_empty() {
            return Err(anyhow!(
                "Zoekt completed without producing shard files for prefix {} in {}",
                shard_prefix,
                index_dir.display()
            ));
        }
        let mut shards = Vec::with_capacity(all_shards.len());
        for path in all_shards {
            let meta = std::fs::metadata(&path)
                .with_context(|| format!("stat Zoekt shard {}", path.display()))?;
            shards.push(ZoektShardArtifact {
                path: path.to_string_lossy().to_string(),
                sha256: format!("sha256:{}", sha256_file(&path)?),
                size_bytes: meta.len(),
            });
        }
        let artifact = ZoektArtifact {
            org_id: req.org_id,
            workspace_id: req.workspace_id.clone(),
            repo_id: req.repo_id,
            branch: req.branch.clone(),
            revision: req.revision.clone(),
            commit_hash: req.commit_hash.clone(),
            index_job_id: req.repo_indexing_job_id.clone(),
            index_dir: index_dir.to_string_lossy().to_string(),
            shard_prefix,
            shards,
            stdout: result.stdout,
            stderr: result.stderr,
        };
        let bytes = serde_json::to_vec(&artifact).with_context(|| "encode Zoekt artifact")?;
        let temp_path = temp_artifact_path(&self.artifact_root, &req, "zoekt.json.tmp");
        let final_path = final_artifact_path(&self.artifact_root, &req, "zoekt.json");
        if let Some(parent) = temp_path.parent() {
            std::fs::create_dir_all(parent)
                .with_context(|| format!("mkdir {}", parent.display()))?;
        }
        std::fs::write(&temp_path, &bytes)
            .with_context(|| format!("write Zoekt artifact {}", temp_path.display()))?;
        let sha = sha256_file(&temp_path)?;
        let mut metadata = HashMap::new();
        metadata.insert("shardCount".to_string(), artifact.shards.len().to_string());
        metadata.insert("indexDir".to_string(), artifact.index_dir.clone());
        metadata.insert("shardPrefix".to_string(), artifact.shard_prefix.clone());
        metadata.insert("deltaRequested".to_string(), use_delta.to_string());
        metadata.insert("deltaThreshold".to_string(), delta_threshold.to_string());
        Ok(ExecuteIndexSubjobResponse {
            artifact_temp_path: temp_path.to_string_lossy().to_string(),
            artifact_path: final_path.to_string_lossy().to_string(),
            artifact_sha256: format!("sha256:{sha}"),
            metadata,
        })
    }

    fn execute_scip(&self, req: ExecuteIndexSubjobRequest) -> Result<ExecuteIndexSubjobResponse> {
        let language = req
            .language
            .as_deref()
            .ok_or_else(|| anyhow!("SCIP subjob requires language"))?;
        let indexer = req
            .indexer
            .as_deref()
            .ok_or_else(|| anyhow!("SCIP subjob requires indexer"))?;
        let worktree = revision_snapshot_path(
            &self.data_cache_dir,
            req.org_id,
            req.repo_id,
            &req.commit_hash,
        );
        if !worktree.exists() {
            return Err(anyhow!(
                "revision snapshot does not exist: {}",
                worktree.display()
            ));
        }
        let project_root = req.project_root.clone().unwrap_or_default();
        let prepared_worktree = prepare_scip_worktree(&worktree, &req)
            .with_context(|| format!("prepare SCIP local worktree for {}", req.subjob_id))?;
        let scip_worktree = prepared_worktree.path();
        let project = scip_project(
            scip_worktree,
            language,
            indexer,
            &req.worker_class,
            &project_root,
        )?;
        let temp_path = temp_scip_artifact_path(&self.artifact_root, &req);
        let final_path = final_artifact_path(&self.artifact_root, &req, "scip");
        let run = scip::run_scip_indexer(scip_worktree, &project, &temp_path, self.scip_timeout)
            .map_err(|e| anyhow!("run SCIP indexer: {}", e))?;
        let sha = sha256_file(&run.output_path)?;
        let mut metadata = HashMap::new();
        metadata.insert("language".to_string(), run.language.to_string());
        metadata.insert("indexer".to_string(), run.indexer.to_string());
        metadata.insert("workerClass".to_string(), run.worker_class.to_string());
        metadata.insert("projectRoot".to_string(), run.project_root);
        metadata.insert(
            "durationMs".to_string(),
            run.duration.as_millis().to_string(),
        );
        if prepared_worktree.is_local_scratch() {
            metadata.insert("localScratch".to_string(), "true".to_string());
        }
        Ok(ExecuteIndexSubjobResponse {
            artifact_temp_path: run.output_path.to_string_lossy().to_string(),
            artifact_path: final_path.to_string_lossy().to_string(),
            artifact_sha256: format!("sha256:{sha}"),
            metadata,
        })
    }

    fn execute_ast(&self, req: ExecuteIndexSubjobRequest) -> Result<ExecuteIndexSubjobResponse> {
        let worktree = revision_snapshot_path(
            &self.data_cache_dir,
            req.org_id,
            req.repo_id,
            &req.commit_hash,
        );
        if !worktree.exists() {
            return Err(anyhow!(
                "revision snapshot does not exist: {}",
                worktree.display()
            ));
        }
        let languages = [
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
        let results = ast_extractor::scan_syntactic_ast_facts_multi(
            &languages,
            SyntacticAstScanOptions {
                repo_root: &worktree,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );
        let scope = crate::code_graph_model::CodeGraphScope {
            org_id: i64::from(req.org_id),
            repo_id: i64::from(req.repo_id),
            revision: req.revision.clone(),
            commit_hash: req.commit_hash.clone(),
            workspace_id: req.workspace_id.clone(),
            schema_version: None,
            builder_version: None,
        };
        let repo_vid = crate::code_graph_model::build_code_graph_vertex_id(
            &crate::code_graph_model::CodeGraphVertexIdentity {
                scope: scope.clone(),
                kind: "repo".to_string(),
                key: format!("repo:{}", req.repo_id),
            },
        )?;
        let mut graph = crate::fact_to_snapshot::GraphAccumulator::default();
        let mut fact_count = 0usize;
        let mut scanned_file_count = 0u32;
        let mut skipped_file_count = 0u32;
        for (language, result) in &results {
            fact_count += result.facts.len();
            scanned_file_count = scanned_file_count.saturating_add(result.scanned_file_count);
            skipped_file_count = skipped_file_count.saturating_add(result.skipped_file_count);
            if result.facts.is_empty() {
                continue;
            }
            crate::fact_to_snapshot::add_syntactic_language_ast_facts(
                &mut graph,
                &scope,
                &repo_vid,
                language,
                &result.facts,
            )
            .with_context(|| format!("build AST graph facts for {language}"))?;
        }
        crate::fact_to_snapshot::reconcile_edge_provenance(&mut graph);
        let (snapshot, anchors) =
            crate::fact_to_snapshot::accumulator_to_snapshot_and_anchors(graph);
        let statements = crate::nebula_ngql::render_snapshot_statements(&snapshot);
        let graph_payload = CodeGraphWritePayload {
            org_id: scope.org_id,
            workspace_id: scope.workspace_id.clone(),
            repo_id: scope.repo_id,
            branch: req.branch.clone(),
            revision: scope.revision.clone(),
            commit_hash: scope.commit_hash.clone(),
            schema_version: crate::code_graph_model::get_schema_version(&scope),
            builder_version: crate::code_graph_model::get_builder_version(&scope),
            index_job_id: req.repo_indexing_job_id.clone(),
            // The split-executor subjob scope resolves the exact
            // manifest in the Go backend before persistence; the
            // Rust worker intentionally has no Postgres dependency.
            manifest_id: String::new(),
            provider_connection_id: None,
            source: "syntactic-ast".to_string(),
            statements,
            anchors,
        };
        let bytes =
            serde_json::to_vec(&graph_payload).with_context(|| "encode AST graph artifact")?;
        let temp_path = temp_artifact_path(&self.artifact_root, &req, "ast.json.tmp");
        let final_path = final_artifact_path(&self.artifact_root, &req, "ast.json");
        if let Some(parent) = temp_path.parent() {
            std::fs::create_dir_all(parent)
                .with_context(|| format!("mkdir {}", parent.display()))?;
        }
        std::fs::write(&temp_path, &bytes)
            .with_context(|| format!("write AST artifact {}", temp_path.display()))?;
        let sha = sha256_file(&temp_path)?;
        let mut metadata = HashMap::new();
        metadata.insert("languageCount".to_string(), results.len().to_string());
        metadata.insert("factCount".to_string(), fact_count.to_string());
        metadata.insert(
            "scannedFileCount".to_string(),
            scanned_file_count.to_string(),
        );
        metadata.insert(
            "skippedFileCount".to_string(),
            skipped_file_count.to_string(),
        );
        metadata.insert(
            "vertexCount".to_string(),
            snapshot.vertices.len().to_string(),
        );
        metadata.insert("edgeCount".to_string(), snapshot.edges.len().to_string());
        Ok(ExecuteIndexSubjobResponse {
            artifact_temp_path: temp_path.to_string_lossy().to_string(),
            artifact_path: final_path.to_string_lossy().to_string(),
            artifact_sha256: format!("sha256:{sha}"),
            metadata,
        })
    }
}

fn validate_request(req: &ExecuteIndexSubjobRequest) -> std::result::Result<(), Status> {
    if req.subjob_id.is_empty()
        || req.repo_indexing_job_id.is_empty()
        || req.org_id <= 0
        || req.workspace_id.is_empty()
        || req.repo_id <= 0
        || req.branch.is_empty()
        || req.revision.is_empty()
        || req.commit_hash.len() != 40
        || !req.commit_hash.chars().all(|c| c.is_ascii_hexdigit())
        || req.layer.is_empty()
        || req.worker_class.is_empty()
        || req.queue_name.is_empty()
        || req.attempt <= 0
    {
        return Err(Status::invalid_argument("invalid index subjob scope"));
    }
    match req.layer.as_str() {
        "ZOEKT" => {
            if req.worker_class != "core" || req.queue_name != "codeintel-index-core" {
                return Err(Status::invalid_argument(
                    "ZOEKT subjob requires core worker class and queue",
                ));
            }
            if req.language.is_some() || req.project_root.is_some() || req.indexer.is_some() {
                return Err(Status::invalid_argument(
                    "ZOEKT subjob must not carry SCIP language/project/indexer scope",
                ));
            }
        }
        "AST_TREE_SITTER" => {
            if req.worker_class != "core" || req.queue_name != "codeintel-index-core" {
                return Err(Status::invalid_argument(
                    "AST_TREE_SITTER subjob requires core worker class and queue",
                ));
            }
            if req.language.is_some() || req.project_root.is_some() || req.indexer.is_some() {
                return Err(Status::invalid_argument(
                    "AST_TREE_SITTER subjob must not carry SCIP language/project/indexer scope",
                ));
            }
        }
        "SCIP" => {
            if req.worker_class == "core" || !req.queue_name.starts_with("codeintel-index-scip-") {
                return Err(Status::invalid_argument(
                    "SCIP subjob requires a SCIP worker class and queue",
                ));
            }
            if req
                .language
                .as_deref()
                .unwrap_or_default()
                .trim()
                .is_empty()
                || req.indexer.as_deref().unwrap_or_default().trim().is_empty()
                || req.project_root.is_none()
            {
                return Err(Status::invalid_argument(
                    "SCIP subjob requires language, project_root, and indexer",
                ));
            }
            let language = req.language.as_deref().unwrap_or_default();
            let indexer = req.indexer.as_deref().unwrap_or_default();
            let def = scip::SUPPORTED_INDEXERS
                .iter()
                .find(|def| def.language == language && def.indexer == indexer)
                .ok_or_else(|| {
                    Status::invalid_argument(format!(
                        "unsupported SCIP language/indexer {language}/{indexer}"
                    ))
                })?;
            if !scip_worker_class_allowed(&req.worker_class, def.worker_class) {
                return Err(Status::invalid_argument(format!(
                    "SCIP language/indexer {language}/{indexer} requires worker class {} but got {}",
                    def.worker_class, req.worker_class
                )));
            }
            if req.queue_name != expected_queue_for_worker_class(&req.worker_class) {
                return Err(Status::invalid_argument(format!(
                    "SCIP worker class {} must use queue {}",
                    req.worker_class,
                    expected_queue_for_worker_class(&req.worker_class)
                )));
            }
        }
        other => {
            return Err(Status::invalid_argument(format!(
                "unsupported executor layer {other}"
            )))
        }
    }
    Ok(())
}

fn revision_snapshot_path(
    data_cache_dir: &Path,
    org_id: i32,
    repo_id: i32,
    commit_hash: &str,
) -> PathBuf {
    tenant_storage_root(data_cache_dir, org_id)
        .join("codeintel")
        .join("revision-snapshots")
        .join(repo_id.to_string())
        .join(commit_hash)
}

struct PreparedScipWorktree {
    path: PathBuf,
    local_scratch: bool,
    keep: bool,
}

impl PreparedScipWorktree {
    fn path(&self) -> &Path {
        &self.path
    }

    fn is_local_scratch(&self) -> bool {
        self.local_scratch
    }
}

impl Drop for PreparedScipWorktree {
    fn drop(&mut self) {
        if self.local_scratch && !self.keep {
            let _ = fs::remove_dir_all(&self.path);
        }
    }
}

fn prepare_scip_worktree(
    worktree: &Path,
    req: &ExecuteIndexSubjobRequest,
) -> Result<PreparedScipWorktree> {
    if !env_bool("CODEINTEL_SCIP_USE_LOCAL_SCRATCH", true) {
        return Ok(PreparedScipWorktree {
            path: worktree.to_path_buf(),
            local_scratch: false,
            keep: true,
        });
    }
    let scratch_root = std::env::var("CODEINTEL_SCIP_LOCAL_SCRATCH_DIR")
        .ok()
        .filter(|v| !v.trim().is_empty())
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/tmp/codeintel-scip-worktrees"));
    let target = scratch_root
        .join(req.org_id.to_string())
        .join(req.repo_id.to_string())
        .join(&req.commit_hash)
        .join(format!("{}_attempt_{}", req.subjob_id, req.attempt));
    if target.exists() {
        fs::remove_dir_all(&target)
            .with_context(|| format!("remove stale SCIP scratch {}", target.display()))?;
    }
    if let Some(parent) = target.parent() {
        fs::create_dir_all(parent)
            .with_context(|| format!("mkdir SCIP scratch parent {}", parent.display()))?;
    }
    copy_dir_for_scip(worktree, &target).with_context(|| {
        format!(
            "copy SCIP worktree {} to {}",
            worktree.display(),
            target.display()
        )
    })?;
    Ok(PreparedScipWorktree {
        path: target,
        local_scratch: true,
        keep: env_bool("CODEINTEL_SCIP_KEEP_LOCAL_SCRATCH", false),
    })
}

fn copy_dir_for_scip(src: &Path, dst: &Path) -> Result<()> {
    fs::create_dir_all(dst).with_context(|| format!("mkdir {}", dst.display()))?;
    for entry in fs::read_dir(src).with_context(|| format!("read dir {}", src.display()))? {
        let entry = entry.with_context(|| format!("read dir entry {}", src.display()))?;
        let name = entry.file_name();
        if name == ".git" {
            continue;
        }
        let from = entry.path();
        let to = dst.join(&name);
        let metadata =
            fs::symlink_metadata(&from).with_context(|| format!("stat {}", from.display()))?;
        if metadata.file_type().is_symlink() {
            copy_symlink(&from, &to)?;
        } else if metadata.is_dir() {
            copy_dir_for_scip(&from, &to)?;
        } else if metadata.is_file() {
            fs::copy(&from, &to)
                .with_context(|| format!("copy {} to {}", from.display(), to.display()))?;
        }
    }
    Ok(())
}

#[cfg(unix)]
fn copy_symlink(from: &Path, to: &Path) -> Result<()> {
    let target = fs::read_link(from).with_context(|| format!("readlink {}", from.display()))?;
    std::os::unix::fs::symlink(&target, to)
        .with_context(|| format!("symlink {} -> {}", to.display(), target.display()))?;
    Ok(())
}

#[cfg(not(unix))]
fn copy_symlink(from: &Path, to: &Path) -> Result<()> {
    let target = fs::read_link(from).with_context(|| format!("readlink {}", from.display()))?;
    let resolved = from.parent().unwrap_or_else(|| Path::new(".")).join(target);
    fs::copy(&resolved, to).with_context(|| {
        format!(
            "copy symlink target {} to {}",
            resolved.display(),
            to.display()
        )
    })?;
    Ok(())
}

fn tenant_storage_root(data_cache_dir: &Path, org_id: i32) -> PathBuf {
    let layout = std::env::var("CODEINTEL_ZOEKT_STORAGE_LAYOUT").unwrap_or_default();
    if layout == "org-directory" {
        if let Some(root) = std::env::var("CODEINTEL_ZOEKT_EFS_ROOT")
            .ok()
            .filter(|v| !v.trim().is_empty())
            .map(PathBuf::from)
        {
            return root.join(org_id.to_string());
        }
        return data_cache_dir.join("zoekt-orgs").join(org_id.to_string());
    }
    data_cache_dir.to_path_buf()
}

fn env_bool(name: &str, default: bool) -> bool {
    match std::env::var(name) {
        Ok(value) => match value.trim().to_ascii_lowercase().as_str() {
            "1" | "true" | "yes" | "on" => true,
            "0" | "false" | "no" | "off" => false,
            _ => default,
        },
        Err(_) => default,
    }
}

fn zoekt_branch_arg(branch: &str) -> String {
    zoekt::zoekt_branch_arg(branch)
}

fn zoekt_branch_shard_prefix(org_id: i32, repo_id: i32, branch: &str) -> String {
    let branch = zoekt_branch_arg(branch);
    let mut hasher = Sha256::new();
    hasher.update(branch.as_bytes());
    let digest = hasher.finalize();
    let hex = digest
        .iter()
        .map(|b| format!("{:02x}", b))
        .collect::<String>();
    format!("{}_b{}", zoekt::shard_prefix(org_id, repo_id), &hex[..16])
}

#[derive(Debug, Serialize)]
struct ZoektArtifact {
    #[serde(rename = "orgId")]
    org_id: i32,
    #[serde(rename = "workspaceId")]
    workspace_id: String,
    #[serde(rename = "repoId")]
    repo_id: i32,
    branch: String,
    revision: String,
    #[serde(rename = "commitHash")]
    commit_hash: String,
    #[serde(rename = "indexJobId")]
    index_job_id: String,
    #[serde(rename = "indexDir")]
    index_dir: String,
    #[serde(rename = "shardPrefix")]
    shard_prefix: String,
    shards: Vec<ZoektShardArtifact>,
    stdout: String,
    stderr: String,
}

#[derive(Debug, Serialize)]
struct ZoektShardArtifact {
    path: String,
    sha256: String,
    #[serde(rename = "sizeBytes")]
    size_bytes: u64,
}

fn scoped_artifact_base(root: &Path, req: &ExecuteIndexSubjobRequest) -> PathBuf {
    root.join(req.org_id.to_string())
        .join(req.repo_id.to_string())
        .join(artifact_scope_segment(&req.workspace_id))
        .join(artifact_scope_segment(&req.branch))
        .join(&req.commit_hash)
}

fn artifact_scope_segment(value: &str) -> String {
    let mut hasher = Sha256::new();
    hasher.update(value.as_bytes());
    let digest = hasher.finalize();
    let hex = digest
        .iter()
        .map(|b| format!("{:02x}", b))
        .collect::<String>();
    format!("s-{}", &hex[..16])
}

fn temp_artifact_path(root: &Path, req: &ExecuteIndexSubjobRequest, ext: &str) -> PathBuf {
    scoped_artifact_base(root, req)
        .join("tmp")
        .join(format!("{}_attempt_{}.{}", req.subjob_id, req.attempt, ext))
}

fn temp_scip_artifact_path(root: &Path, req: &ExecuteIndexSubjobRequest) -> PathBuf {
    scoped_artifact_base(root, req).join("tmp").join(format!(
        "{}_attempt_{}.tmp.scip",
        req.subjob_id, req.attempt
    ))
}

fn final_artifact_path(root: &Path, req: &ExecuteIndexSubjobRequest, ext: &str) -> PathBuf {
    let safe_layer = req.layer.to_ascii_lowercase();
    scoped_artifact_base(root, req)
        .join(safe_layer)
        .join(format!("{}_attempt_{}.{}", req.subjob_id, req.attempt, ext))
}

fn scip_project(
    repo_path_abs: &Path,
    language: &str,
    indexer: &str,
    worker_class: &str,
    project_root: &str,
) -> Result<ScipProject> {
    let def = scip::SUPPORTED_INDEXERS
        .iter()
        .find(|def| def.language == language && def.indexer == indexer)
        .ok_or_else(|| anyhow!("unsupported SCIP language/indexer {language}/{indexer}"))?;
    if !scip_worker_class_allowed(worker_class, def.worker_class) {
        return Err(anyhow!(
            "SCIP language/indexer {language}/{indexer} requires worker class {} but got {worker_class}",
            def.worker_class
        ));
    }
    let marker = selected_marker_path(repo_path_abs, project_root, def);
    Ok(ScipProject {
        language: def.language,
        indexer: def.indexer,
        worker_class: def.worker_class,
        project_root: project_root.to_string(),
        marker_path: marker.path,
        inferred: marker.inferred,
        inferred_reason: marker.inferred_reason,
    })
}

struct SelectedMarker {
    path: String,
    inferred: bool,
    inferred_reason: Option<String>,
}

fn selected_marker_path(
    repo_path_abs: &Path,
    project_root: &str,
    def: &scip::ScipIndexerDefinition,
) -> SelectedMarker {
    for marker in def.markers {
        let rel = marker_path(project_root, marker);
        if repo_path_abs.join(&rel).exists() {
            return SelectedMarker {
                path: rel,
                inferred: false,
                inferred_reason: None,
            };
        }
    }
    if def.language == "typescript" {
        return SelectedMarker {
            path: marker_path(project_root, "package.json"),
            inferred: true,
            inferred_reason: Some(
                "Detected TypeScript/JavaScript source files without package.json, tsconfig.json, or jsconfig.json."
                    .to_string(),
            ),
        };
    }
    SelectedMarker {
        path: marker_path(
            project_root,
            def.markers.first().copied().unwrap_or("project"),
        ),
        inferred: false,
        inferred_reason: None,
    }
}

fn scip_worker_class_allowed(deploy_class: &str, scip_worker_class: &str) -> bool {
    match deploy_class {
        "scip-ts-python" => matches!(scip_worker_class, "ts-js" | "python"),
        "scip-go" => scip_worker_class == "go",
        "scip-jvm" => scip_worker_class == "jvm",
        "scip-dotnet" => scip_worker_class == "dotnet",
        "scip-rust-dart" => scip_worker_class == "rust-dart",
        "scip-cpp-x86" => scip_worker_class == "cpp",
        "scip-ruby-x86" => scip_worker_class == "ruby",
        _ => false,
    }
}

fn expected_queue_for_worker_class(worker_class: &str) -> &'static str {
    match worker_class {
        "scip-ts-python" => "codeintel-index-scip-ts-python",
        "scip-go" => "codeintel-index-scip-go",
        "scip-jvm" => "codeintel-index-scip-jvm",
        "scip-dotnet" => "codeintel-index-scip-dotnet",
        "scip-rust-dart" => "codeintel-index-scip-rust-dart",
        "scip-cpp-x86" => "codeintel-index-scip-cpp-x86",
        "scip-ruby-x86" => "codeintel-index-scip-ruby-x86",
        _ => "",
    }
}

fn marker_path(project_root: &str, marker: &str) -> String {
    if project_root.is_empty() {
        marker.to_string()
    } else {
        format!("{}/{}", project_root.trim_matches('/'), marker)
    }
}

fn sha256_file(path: &Path) -> Result<String> {
    let bytes = std::fs::read(path).with_context(|| format!("read artifact {}", path.display()))?;
    let mut hasher = Sha256::new();
    hasher.update(bytes);
    Ok(format!("{:x}", hasher.finalize()))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn artifact_paths_are_scoped_by_org_repo_commit() {
        let req = ExecuteIndexSubjobRequest {
            subjob_id: "subjob-1".to_string(),
            repo_indexing_job_id: "job-1".to_string(),
            org_id: 7,
            workspace_id: "atom-ws".to_string(),
            repo_id: 42,
            branch: "refs/heads/main".to_string(),
            revision: "refs/heads/main".to_string(),
            commit_hash: "a".repeat(40),
            layer: "SCIP".to_string(),
            language: Some("go".to_string()),
            project_root: Some("".to_string()),
            indexer: Some("scip-go".to_string()),
            worker_class: "scip-go".to_string(),
            queue_name: "codeintel-index-scip-go".to_string(),
            attempt: 2,
        };
        let temp = temp_scip_artifact_path(Path::new("/efs/artifacts"), &req);
        let final_path = final_artifact_path(Path::new("/efs/artifacts"), &req, "scip");
        assert_eq!(
            temp,
            PathBuf::from("/efs/artifacts/7/42/s-a71b9b1eb0b78367/s-f921bd05e68b0374/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/tmp/subjob-1_attempt_2.tmp.scip")
        );
        assert_eq!(
            final_path,
            PathBuf::from("/efs/artifacts/7/42/s-a71b9b1eb0b78367/s-f921bd05e68b0374/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/scip/subjob-1_attempt_2.scip")
        );
    }

    #[test]
    fn zoekt_branch_arg_strips_heads_prefix() {
        assert_eq!(zoekt_branch_arg("refs/heads/main"), "main");
        assert_eq!(
            zoekt_branch_arg("refs/remotes/origin/release-a"),
            "release-a"
        );
        assert_eq!(zoekt_branch_arg("release/1.0"), "release/1.0");
        assert_eq!(zoekt_branch_arg(""), "HEAD");
    }

    #[test]
    fn zoekt_branch_shard_prefix_keeps_repo_prefix_and_separates_branches() {
        let main = zoekt_branch_shard_prefix(7, 42, "refs/heads/main");
        let release = zoekt_branch_shard_prefix(7, 42, "refs/heads/release-a");
        assert!(main.starts_with("7_42_b"));
        assert!(release.starts_with("7_42_b"));
        assert_ne!(main, release);
    }

    #[test]
    fn validate_request_rejects_misrouted_ast_and_scip_layers() {
        let mut zoekt = ExecuteIndexSubjobRequest {
            subjob_id: "subjob-1".to_string(),
            repo_indexing_job_id: "job-1".to_string(),
            org_id: 7,
            workspace_id: "atom-ws".to_string(),
            repo_id: 42,
            branch: "refs/heads/main".to_string(),
            revision: "refs/heads/main".to_string(),
            commit_hash: "a".repeat(40),
            layer: "ZOEKT".to_string(),
            language: None,
            project_root: None,
            indexer: None,
            worker_class: "core".to_string(),
            queue_name: "codeintel-index-core".to_string(),
            attempt: 1,
        };
        validate_request(&zoekt).expect("valid ZOEKT request");
        zoekt.language = Some("go".to_string());
        assert!(validate_request(&zoekt).is_err());

        let mut ast = ExecuteIndexSubjobRequest {
            subjob_id: "subjob-1".to_string(),
            repo_indexing_job_id: "job-1".to_string(),
            org_id: 7,
            workspace_id: "atom-ws".to_string(),
            repo_id: 42,
            branch: "refs/heads/main".to_string(),
            revision: "refs/heads/main".to_string(),
            commit_hash: "a".repeat(40),
            layer: "AST_TREE_SITTER".to_string(),
            language: None,
            project_root: None,
            indexer: None,
            worker_class: "core".to_string(),
            queue_name: "codeintel-index-core".to_string(),
            attempt: 1,
        };
        validate_request(&ast).expect("valid AST request");
        ast.worker_class = "scip-go".to_string();
        assert!(validate_request(&ast).is_err());

        let mut scip = ast.clone();
        scip.layer = "SCIP".to_string();
        scip.language = Some("go".to_string());
        scip.project_root = Some("".to_string());
        scip.indexer = Some("scip-go".to_string());
        scip.worker_class = "scip-go".to_string();
        scip.queue_name = "codeintel-index-scip-go".to_string();
        validate_request(&scip).expect("valid SCIP request");
        scip.worker_class = "core".to_string();
        scip.queue_name = "codeintel-index-core".to_string();
        assert!(validate_request(&scip).is_err());

        scip.worker_class = "scip-ts-python".to_string();
        scip.queue_name = "codeintel-index-scip-ts-python".to_string();
        assert!(validate_request(&scip).is_err());
    }

    #[test]
    fn scip_project_uses_package_marker_for_package_only_typescript_root() {
        let dir = tempfile::tempdir().expect("tempdir");
        std::fs::write(dir.path().join("package.json"), "{}").expect("package");

        let project = scip_project(
            dir.path(),
            "typescript",
            "scip-typescript",
            "scip-ts-python",
            "",
        )
        .expect("project");

        assert_eq!(project.marker_path, "package.json");
        assert!(!project.inferred);
    }

    #[test]
    fn scip_project_marks_bare_typescript_root_as_inferred() {
        let dir = tempfile::tempdir().expect("tempdir");
        std::fs::create_dir_all(dir.path().join("src")).expect("src");
        std::fs::write(
            dir.path().join("src/tenant.ts"),
            "export const tenantMarker = 1;",
        )
        .expect("ts");

        let project = scip_project(
            dir.path(),
            "typescript",
            "scip-typescript",
            "scip-ts-python",
            "",
        )
        .expect("project");

        assert_eq!(project.marker_path, "package.json");
        assert!(project.inferred);
        assert!(project
            .inferred_reason
            .as_deref()
            .unwrap_or_default()
            .contains("TypeScript/JavaScript"));
    }

    #[test]
    fn execute_ast_allows_schema_only_artifact_for_docs_repo() {
        let cache = tempfile::tempdir().expect("cache");
        let artifacts = tempfile::tempdir().expect("artifacts");
        let commit = "a".repeat(40);
        let snapshot = revision_snapshot_path(cache.path(), 7, 42, &commit);
        std::fs::create_dir_all(&snapshot).expect("snapshot");
        std::fs::write(snapshot.join("README.md"), "# docs only\n").expect("readme");

        let service = Service {
            data_cache_dir: cache.path().to_path_buf(),
            artifact_root: artifacts.path().to_path_buf(),
            zoekt_binary: PathBuf::new(),
            scip_timeout: Duration::from_secs(1),
        };
        let result = service
            .execute_ast(ExecuteIndexSubjobRequest {
                subjob_id: "subjob-ast".to_string(),
                repo_indexing_job_id: "job-1".to_string(),
                org_id: 7,
                workspace_id: "atom-ws".to_string(),
                repo_id: 42,
                branch: "refs/heads/main".to_string(),
                revision: "refs/heads/main".to_string(),
                commit_hash: commit,
                layer: "AST_TREE_SITTER".to_string(),
                language: None,
                project_root: None,
                indexer: None,
                worker_class: "core".to_string(),
                queue_name: "codeintel-index-core".to_string(),
                attempt: 1,
            })
            .expect("schema-only AST artifact should succeed");

        assert_eq!(
            result.metadata.get("factCount").map(String::as_str),
            Some("0")
        );
        assert_eq!(
            result.metadata.get("vertexCount").map(String::as_str),
            Some("0")
        );
        assert_eq!(
            result.metadata.get("edgeCount").map(String::as_str),
            Some("0")
        );
        let raw = std::fs::read_to_string(result.artifact_temp_path).expect("artifact");
        assert!(raw.contains("\"statements\""));
        assert!(raw.contains("CREATE TAG IF NOT EXISTS `code_graph_node`"));
    }
}
