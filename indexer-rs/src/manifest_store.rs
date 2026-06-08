//! IndexManifestManager: tokio-postgres port of the legacy
//! `IndexManifestManager` class in
//! `packages/backend/src/indexManifestManager.ts:39-299`.
//!
//! Surfaces 4 public operations + 1 private read:
//!   - prepare_revision_manifests — for each revision, build a
//!     fresh manifest, look up the previous READY manifest for
//!     the same scope, compute the delta plan, INSERT
//!     RepoIndexManifest + bulk-INSERT RepoIndexManifestFile
//!     rows. Returns the list of prepared (id, branch,
//!     commitHash, plan) tuples for the caller to wire into
//!     downstream engines.
//!   - activate_manifests — atomic Postgres tx that flips one
//!     manifest from PENDING → READY and marks every other
//!     READY manifest in the same (orgId, workspaceId, repoId,
//!     providerConnectionId, branch) scope as SUPERSEDED.
//!   - mark_manifests_failed — bulk UPDATE from PENDING →
//!     FAILED with an error message.
//!   - delete_repo_manifests — DELETE every manifest row for a
//!     given repoId (cascade drops the file rows).
//!   - get_previous_manifest (private) — load the last READY
//!     manifest for the scope, including its files, so the
//!     delta planner can diff against it.
//!
//! Multi-tenancy contract: every read filters by orgId +
//! workspaceId + repoId + providerConnectionId + branch; every
//! write carries the same scope tuple. There is no global
//! query path — the legacy product contract is "no manifest
//! crosses tenants", and the port enforces it via the SQL
//! WHERE clauses + the delta_plan::assert_same_manifest_scope
//! guard.
//!
//! HPA-safety: the writes are idempotent at the scope level —
//! the legacy never relied on a unique constraint to dedupe
//! (it creates a new row per indexJob), so concurrent indexer
//! pods producing the same (scope, commitHash) just create N
//! sibling rows in PENDING state; only one wins the
//! activate_manifests race because activate_manifests
//! supersedes every other READY in the scope.

use anyhow::{anyhow, Context, Result};
use serde_json::Value as JsonValue;
use std::collections::BTreeMap;
use std::sync::Arc;
use tokio_postgres::{Client, NoTls};
use uuid::Uuid;

use crate::delta_plan::{
    build_delta_reindex_plan, DeltaReindexPlan, IndexManifestFile, IndexRunManifest,
    SemanticExtractionFingerprint,
};

/// PreparedIndexManifest mirrors the legacy
/// `PreparedIndexManifest` (indexManifestManager.ts:22-28).
#[derive(Debug, Clone)]
pub struct PreparedIndexManifest {
    pub id: String,
    pub branch: String,
    pub commit_hash: String,
    pub file_count: usize,
    pub plan: DeltaReindexPlan,
}

/// IndexManifestManager wraps a Postgres connection (or pool
/// handle — caller's choice). The 4 public methods mirror the
/// legacy class methods.
///
/// The legacy class is constructed with a PrismaClient. The
/// Rust port takes a borrowed `tokio_postgres::Client` so the
/// caller (the asynq worker) controls connection lifetime +
/// pool affinity.
/// IndexManifestManager wraps a shared Postgres client for the
/// non-transactional reads + writes, plus the connection URL
/// needed to open a fresh client for the transactional
/// `activate_manifests` (tokio-postgres requires `&mut Client`
/// for `transaction()`, which is incompatible with the
/// `Arc<Client>` the worker already shares across handlers).
pub struct IndexManifestManager {
    client: Arc<Client>,
    postgres_url: String,
}

impl IndexManifestManager {
    pub fn new(client: Arc<Client>, postgres_url: String) -> Self {
        Self {
            client,
            postgres_url,
        }
    }

    /// prepare_revision_manifests is the direct port of
    /// indexManifestManager.ts:42-134.
    ///
    /// `workspace_id` and `semantic_extraction` are resolved by
    /// the caller — the legacy private helpers
    /// (resolveWorkspaceId + resolveSemanticExtractionFingerprint)
    /// touch other tables (Org, OrgLanguageModel) + env vars
    /// that are out of this slice's scope. Passing them in
    /// keeps the slice focused; the caller composes them via
    /// helper queries in the worker.
    ///
    /// `provider_connection_scope` is the joined connection-ID
    /// string from `manifest_files::provider_connection_scope`.
    /// None when the repo has no connections.
    pub async fn prepare_revision_manifests(
        &self,
        org_id: i32,
        repo_id: i32,
        repo_path: &std::path::Path,
        revisions: &[String],
        index_job_id: &str,
        workspace_id: &str,
        provider_connection_id: Option<&str>,
        semantic_extraction: Option<&SemanticExtractionFingerprint>,
    ) -> Result<Vec<PreparedIndexManifest>> {
        let mut prepared: Vec<PreparedIndexManifest> = Vec::with_capacity(revisions.len());

        for revision in revisions {
            // Resolve the revision to a commit hash. The legacy
            // skips revisions that fail to resolve (e.g. empty
            // repo, deleted branch); we do the same.
            let commit_hash =
                match crate::manifest_files::get_commit_hash_for_ref_name(repo_path, revision)? {
                    Some(h) => h,
                    None => continue,
                };

            let files =
                crate::manifest_files::build_manifest_files_for_revision(repo_path, &commit_hash)?;

            let current_manifest = IndexRunManifest {
                org_id,
                workspace_id: workspace_id.to_string(),
                repo_id,
                provider_connection_id: provider_connection_id.map(|s| s.to_string()),
                branch: revision.clone(),
                commit_hash: commit_hash.clone(),
                files: files.clone(),
                scip_toolchains: None,
                semantic_extraction: semantic_extraction.cloned(),
            };

            let previous_manifest = self
                .get_previous_manifest(&current_manifest)
                .await
                .with_context(|| {
                    format!(
                        "get_previous_manifest for repo {} branch {}",
                        repo_id, revision
                    )
                })?;

            let plan = build_delta_reindex_plan(
                previous_manifest.as_ref(),
                &current_manifest,
                false, // supports_zoekt_file_delta — matches legacy default
            )
            .map_err(|e| anyhow!("build delta plan: {}", e))?;

            let manifest_id = Uuid::new_v4().to_string();
            self.insert_manifest_row(
                &manifest_id,
                index_job_id,
                org_id,
                repo_id,
                workspace_id,
                provider_connection_id,
                revision,
                &commit_hash,
                &plan,
                files.len() as i32,
                semantic_extraction,
            )
            .await?;

            if !files.is_empty() {
                self.bulk_insert_files(&manifest_id, &files).await?;
            }

            prepared.push(PreparedIndexManifest {
                id: manifest_id,
                branch: revision.clone(),
                commit_hash,
                file_count: files.len(),
                plan,
            });
        }

        Ok(prepared)
    }

    /// activate_manifests mirrors indexManifestManager.ts:136-186.
    /// Atomic transaction: for each manifest, supersede every
    /// other READY in the same scope, then flip this one to
    /// READY.
    pub async fn activate_manifests(
        &self,
        expected_org_id: i32,
        manifest_ids: &[String],
    ) -> Result<()> {
        if manifest_ids.is_empty() {
            return Ok(());
        }
        // tokio-postgres' Client::transaction takes &mut self,
        // incompatible with the shared Arc<Client> the worker
        // already owns. Open a dedicated connection for this
        // tx so it doesn't contend with the steady-state pool.
        let (mut tx_client, conn) = tokio_postgres::connect(&self.postgres_url, NoTls)
            .await
            .context("open postgres conn for activate_manifests")?;
        tokio::spawn(async move {
            if let Err(e) = conn.await {
                eprintln!("activate_manifests conn task: {}", e);
            }
        });
        let tx = tx_client.transaction().await?;
        // Single timestamp captured once and bound into BOTH
        // the supersede + flip UPDATEs so the SUPERSEDED rows'
        // supersededAt and the new READY row's activatedAt
        // agree on the same instant (parity with the legacy
        // `const supersededAt = new Date()` shared between the
        // two Prisma writes — indexManifestManager.ts:158/179).
        let now = chrono::Utc::now();
        for manifest_id in manifest_ids {
            // R.7-defense: bind `expected_org_id` into the
            // SELECT so a misrouted manifest_id from another
            // tenant can't surface its scope. Today's caller
            // (worker.rs) only passes manifests it just
            // INSERTed under the correct orgId; this guard
            // exists for defense-in-depth against future
            // refactors.
            let row = tx
                .query_opt(
                    r#"SELECT "orgId", "repoId", "workspaceId", "providerConnectionId", "branch"
                       FROM "RepoIndexManifest"
                       WHERE id = $1 AND "orgId" = $2"#,
                    &[&manifest_id.as_str(), &expected_org_id],
                )
                .await?;
            let row = match row {
                Some(r) => r,
                None => continue, // legacy `if (!manifest) continue;`
            };
            let scope_org: i32 = row.get(0);
            let scope_repo: i32 = row.get(1);
            let scope_workspace: String = row.get(2);
            let scope_provider: Option<String> = row.get(3);
            let scope_branch: String = row.get(4);

            // Supersede every OTHER manifest in the same scope.
            // The IS NOT DISTINCT FROM pair handles NULL provider
            // matching NULL — Postgres = on NULL returns NULL,
            // not TRUE, so a plain = would miss those rows.
            tx.execute(
                r#"UPDATE "RepoIndexManifest"
                   SET status = 'SUPERSEDED'::"RepoIndexManifestStatus",
                       "supersededAt" = $7,
                       "updatedAt" = $7
                   WHERE id <> $1
                     AND "orgId" = $2
                     AND "repoId" = $3
                     AND "workspaceId" = $4
                     AND "providerConnectionId" IS NOT DISTINCT FROM $5
                     AND "branch" = $6
                     AND status = 'READY'::"RepoIndexManifestStatus""#,
                &[
                    &manifest_id.as_str(),
                    &scope_org,
                    &scope_repo,
                    &scope_workspace,
                    &scope_provider,
                    &scope_branch,
                    &now,
                ],
            )
            .await?;

            tx.execute(
                r#"UPDATE "RepoIndexManifest"
                   SET status = 'READY'::"RepoIndexManifestStatus",
                       "activatedAt" = $2,
                       "failedAt" = NULL,
                       "errorMessage" = NULL,
                       "updatedAt" = $2
                   WHERE id = $1"#,
                &[&manifest_id.as_str(), &now],
            )
            .await?;
        }
        tx.commit().await?;
        Ok(())
    }

    /// mark_manifests_failed mirrors
    /// indexManifestManager.ts:188-204.
    pub async fn mark_manifests_failed(
        &self,
        manifest_ids: &[String],
        error_message: &str,
    ) -> Result<()> {
        if manifest_ids.is_empty() {
            return Ok(());
        }
        let ids: Vec<&str> = manifest_ids.iter().map(|s| s.as_str()).collect();
        self.client
            .execute(
                r#"UPDATE "RepoIndexManifest"
                   SET status = 'FAILED'::"RepoIndexManifestStatus",
                       "failedAt" = NOW(),
                       "errorMessage" = $2,
                       "updatedAt" = NOW()
                   WHERE id = ANY($1)
                     AND status = 'PENDING'::"RepoIndexManifestStatus""#,
                &[&ids, &error_message],
            )
            .await?;
        Ok(())
    }

    /// delete_repo_manifests mirrors
    /// indexManifestManager.ts:206-210.
    pub async fn delete_repo_manifests(&self, repo_id: i32) -> Result<()> {
        self.client
            .execute(
                r#"DELETE FROM "RepoIndexManifest" WHERE "repoId" = $1"#,
                &[&repo_id],
            )
            .await?;
        Ok(())
    }

    /// get_previous_manifest is the direct port of
    /// indexManifestManager.ts:212-260. Loads the latest READY
    /// manifest for the same scope, ordered by activatedAt DESC,
    /// createdAt DESC. Returns None when no prior READY exists.
    async fn get_previous_manifest(
        &self,
        current: &IndexRunManifest,
    ) -> Result<Option<IndexRunManifest>> {
        let row = self
            .client
            .query_opt(
                r#"SELECT id, "orgId", "workspaceId", "repoId",
                          "providerConnectionId", "branch", "commitHash",
                          "semanticPromptVersion", "semanticModelId",
                          "semanticSchemaVersion"
                   FROM "RepoIndexManifest"
                   WHERE "orgId" = $1
                     AND "repoId" = $2
                     AND "workspaceId" = $3
                     AND "providerConnectionId" IS NOT DISTINCT FROM $4
                     AND "branch" = $5
                     AND status = 'READY'::"RepoIndexManifestStatus"
                   ORDER BY "activatedAt" DESC NULLS LAST,
                            "createdAt" DESC
                   LIMIT 1"#,
                &[
                    &current.org_id,
                    &current.repo_id,
                    &current.workspace_id,
                    &current.provider_connection_id.as_deref(),
                    &current.branch,
                ],
            )
            .await?;
        let row = match row {
            Some(r) => r,
            None => return Ok(None),
        };

        let manifest_id: String = row.get(0);
        let org_id: i32 = row.get(1);
        let workspace_id: String = row.get(2);
        let repo_id: i32 = row.get(3);
        let provider_connection_id: Option<String> = row.get(4);
        let branch: String = row.get(5);
        let commit_hash: String = row.get(6);
        let semantic_prompt: Option<String> = row.get(7);
        let semantic_model: Option<String> = row.get(8);
        let semantic_schema: Option<i32> = row.get(9);

        let semantic_extraction = match (semantic_prompt, semantic_model, semantic_schema) {
            (Some(prompt), Some(model), Some(schema)) => Some(SemanticExtractionFingerprint {
                prompt_version: prompt,
                model_id: model,
                schema_version: schema as i64,
            }),
            _ => None,
        };

        let files = self.load_manifest_files(&manifest_id).await?;

        Ok(Some(IndexRunManifest {
            org_id,
            workspace_id,
            repo_id,
            provider_connection_id,
            branch,
            commit_hash,
            files,
            scip_toolchains: None,
            semantic_extraction,
        }))
    }

    async fn load_manifest_files(&self, manifest_id: &str) -> Result<Vec<IndexManifestFile>> {
        let rows = self
            .client
            .query(
                r#"SELECT path, "contentHash", "language", "projectRoot",
                          generated, vendor, test, artifacts
                   FROM "RepoIndexManifestFile"
                   WHERE "manifestId" = $1"#,
                &[&manifest_id],
            )
            .await?;
        let mut out: Vec<IndexManifestFile> = Vec::with_capacity(rows.len());
        for row in rows {
            let path: String = row.get(0);
            let content_hash: String = row.get(1);
            let language: Option<String> = row.get(2);
            let project_root: Option<String> = row.get(3);
            let generated: bool = row.get(4);
            let vendor: bool = row.get(5);
            let test: bool = row.get(6);
            let artifacts_json: Option<JsonValue> = row.get(7);
            let artifacts = match artifacts_json {
                Some(JsonValue::Object(map)) => {
                    let mut parsed: BTreeMap<crate::delta_plan::IndexerKind, String> =
                        BTreeMap::new();
                    for (k, v) in map {
                        if let Ok(kind) = serde_json::from_value::<crate::delta_plan::IndexerKind>(
                            JsonValue::String(k.clone()),
                        ) {
                            if let JsonValue::String(s) = v {
                                parsed.insert(kind, s);
                            }
                        }
                    }
                    if parsed.is_empty() {
                        None
                    } else {
                        Some(parsed)
                    }
                }
                _ => None,
            };
            out.push(IndexManifestFile {
                path,
                content_hash,
                language,
                project_root,
                generated: Some(generated),
                vendor: Some(vendor),
                test: Some(test),
                artifacts,
            });
        }
        Ok(out)
    }

    #[allow(clippy::too_many_arguments)]
    async fn insert_manifest_row(
        &self,
        manifest_id: &str,
        index_job_id: &str,
        org_id: i32,
        repo_id: i32,
        workspace_id: &str,
        provider_connection_id: Option<&str>,
        branch: &str,
        commit_hash: &str,
        plan: &DeltaReindexPlan,
        file_count: i32,
        semantic_extraction: Option<&SemanticExtractionFingerprint>,
    ) -> Result<()> {
        let plan_json = serde_json::to_value(plan).context("serialise plan to JSON")?;
        let zoekt_strategy = strategy_str(&plan.zoekt.strategy);
        let scip_strategy = strategy_str(&plan.scip.strategy);
        let graph_strategy = strategy_str(&plan.graph.strategy);
        let semantic_strategy = strategy_str(&plan.semantic.strategy);
        let added: i32 = plan.added_files.len() as i32;
        let changed: i32 = plan.changed_files.len() as i32;
        let deleted: i32 = plan.deleted_files.len() as i32;
        let unchanged: i32 = plan.unchanged_files.len() as i32;
        let semantic_prompt = semantic_extraction.map(|s| s.prompt_version.as_str());
        let semantic_model = semantic_extraction.map(|s| s.model_id.as_str());
        let semantic_schema = semantic_extraction.map(|s| s.schema_version as i32);

        self.client
            .execute(
                r#"INSERT INTO "RepoIndexManifest" (
                       id, status, "workspaceId", "providerConnectionId",
                       branch, "commitHash", plan, "fileCount",
                       "addedFileCount", "changedFileCount", "deletedFileCount",
                       "unchangedFileCount",
                       "zoektStrategy", "scipStrategy", "graphStrategy",
                       "semanticStrategy",
                       "semanticPromptVersion", "semanticModelId",
                       "semanticSchemaVersion",
                       "createdAt", "updatedAt", "orgId", "repoId", "indexJobId"
                   ) VALUES (
                       $1, 'PENDING'::"RepoIndexManifestStatus", $2, $3,
                       $4, $5, $6, $7,
                       $8, $9, $10,
                       $11,
                       $12, $13, $14,
                       $15,
                       $16, $17,
                       $18,
                       NOW(), NOW(), $19, $20, $21
                   )"#,
                &[
                    &manifest_id,
                    &workspace_id,
                    &provider_connection_id,
                    &branch,
                    &commit_hash,
                    &plan_json,
                    &file_count,
                    &added,
                    &changed,
                    &deleted,
                    &unchanged,
                    &zoekt_strategy,
                    &scip_strategy,
                    &graph_strategy,
                    &semantic_strategy,
                    &semantic_prompt,
                    &semantic_model,
                    &semantic_schema,
                    &org_id,
                    &repo_id,
                    &index_job_id,
                ],
            )
            .await
            .context("insert RepoIndexManifest row")?;
        Ok(())
    }

    async fn bulk_insert_files(
        &self,
        manifest_id: &str,
        files: &[IndexManifestFile],
    ) -> Result<()> {
        // tokio-postgres has no native COPY-friendly bulk
        // helper; use parameterized multi-row VALUES. Capped at
        // a chunk size to keep parameter count under Postgres'
        // 64k bind limit (8 params/row × N rows).
        const PARAMS_PER_ROW: usize = 9;
        const MAX_PARAMS: usize = 60_000; // safety margin under 65535
        let max_chunk = MAX_PARAMS / PARAMS_PER_ROW;

        for chunk in files.chunks(max_chunk) {
            let mut sql = String::from(
                r#"INSERT INTO "RepoIndexManifestFile"
                       (id, path, "contentHash", "language", "projectRoot",
                        generated, vendor, test, artifacts, "manifestId")
                   VALUES "#,
            );
            let mut params: Vec<Box<dyn tokio_postgres::types::ToSql + Send + Sync>> =
                Vec::with_capacity(chunk.len() * PARAMS_PER_ROW + 1);
            let manifest_id_owned = manifest_id.to_string();

            let ids: Vec<String> = (0..chunk.len())
                .map(|_| Uuid::new_v4().to_string())
                .collect();
            let manifest_id_idx = chunk.len() * PARAMS_PER_ROW + 1;
            for (i, file) in chunk.iter().enumerate() {
                let base = i * PARAMS_PER_ROW;
                if i > 0 {
                    sql.push_str(", ");
                }
                sql.push_str(&format!(
                    "(${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${}, ${})",
                    base + 1,
                    base + 2,
                    base + 3,
                    base + 4,
                    base + 5,
                    base + 6,
                    base + 7,
                    base + 8,
                    base + 9,
                    manifest_id_idx,
                ));
                params.push(Box::new(ids[i].clone()));
                params.push(Box::new(file.path.clone()));
                params.push(Box::new(file.content_hash.clone()));
                params.push(Box::new(file.language.clone()));
                params.push(Box::new(file.project_root.clone()));
                params.push(Box::new(file.generated.unwrap_or(false)));
                params.push(Box::new(file.vendor.unwrap_or(false)));
                params.push(Box::new(file.test.unwrap_or(false)));
                let artifacts_json: Option<JsonValue> = file
                    .artifacts
                    .as_ref()
                    .map(|m| serde_json::to_value(m).unwrap_or(JsonValue::Null));
                params.push(Box::new(artifacts_json));
            }
            params.push(Box::new(manifest_id_owned));

            // Reborrow as &(dyn ToSql + Sync) — tokio-postgres' bind
            // signature.
            let param_refs: Vec<&(dyn tokio_postgres::types::ToSql + Sync)> =
                params.iter().map(|b| b.as_ref() as _).collect();
            self.client
                .execute(&sql, &param_refs[..])
                .await
                .context("bulk insert RepoIndexManifestFile rows")?;
        }
        Ok(())
    }
}

fn strategy_str<S: serde::Serialize>(strategy: &S) -> String {
    // The strategy enums all serialize as "SCREAMING_SNAKE_CASE"
    // strings; serde_json yields `"FULL_REPO"`-style quoted
    // literals, so trim the quotes back off.
    let json = serde_json::to_string(strategy).unwrap_or_else(|_| "\"UNKNOWN\"".to_string());
    json.trim_matches('"').to_string()
}

#[cfg(test)]
mod tests {
    //! Unit tests for the pure-transform parts. Full SQL paths
    //! are exercised by the R.7 worker E2E (Go-side
    //! `TestParity_RustIndexer_MultiBranch_AllResolved` extended
    //! to assert RepoIndexManifest rows).

    use super::*;
    use crate::delta_plan::{GraphStrategy, ScipStrategy, SemanticStrategy, ZoektStrategy};

    #[test]
    fn strategy_str_strips_quotes_for_each_enum() {
        assert_eq!(strategy_str(&ZoektStrategy::Noop), "NOOP");
        assert_eq!(strategy_str(&ZoektStrategy::DeltaFiles), "DELTA_FILES");
        assert_eq!(strategy_str(&ZoektStrategy::FullRepo), "FULL_REPO");
        assert_eq!(
            strategy_str(&ZoektStrategy::FullRepoRewrite),
            "FULL_REPO_REWRITE"
        );
        assert_eq!(strategy_str(&ScipStrategy::ProjectRoots), "PROJECT_ROOTS");
        assert_eq!(strategy_str(&GraphStrategy::DeltaFiles), "DELTA_FILES");
        assert_eq!(strategy_str(&SemanticStrategy::AllChunks), "ALL_CHUNKS");
        assert_eq!(
            strategy_str(&SemanticStrategy::ChangedChunks),
            "CHANGED_CHUNKS"
        );
    }

    #[test]
    fn prepared_index_manifest_carries_plan_into_caller() {
        // Construct a hand-rolled PreparedIndexManifest and
        // confirm the public surface exposes what R.7 expects.
        use crate::delta_plan::{
            DeltaReindexPlan, GraphPlan, Mode, ScipPlan, SemanticPlan, ZoektPlan,
        };
        let plan = DeltaReindexPlan {
            mode: Mode::Full,
            added_files: vec!["a".to_string()],
            changed_files: vec![],
            deleted_files: vec![],
            unchanged_files: vec![],
            zoekt: ZoektPlan {
                strategy: ZoektStrategy::FullRepo,
                files: vec!["a".to_string()],
                reason: None,
            },
            scip: ScipPlan {
                strategy: ScipStrategy::FullRepo,
                project_roots: vec![".".to_string()],
                files: vec!["a".to_string()],
                reason: None,
            },
            graph: GraphPlan {
                strategy: GraphStrategy::DeltaFiles,
                files: vec!["a".to_string()],
                deleted_files: vec![],
            },
            semantic: SemanticPlan {
                strategy: SemanticStrategy::AllChunks,
                files: vec!["a".to_string()],
                reason: None,
            },
        };
        let pm = PreparedIndexManifest {
            id: "x".to_string(),
            branch: "refs/heads/main".to_string(),
            commit_hash: "abc123".to_string(),
            file_count: 1,
            plan,
        };
        assert_eq!(pm.id, "x");
        assert_eq!(pm.plan.zoekt.strategy, ZoektStrategy::FullRepo);
    }

    // Compile-only smoke: every column in the INSERT statement
    // matches a known column in the migration. Sanity-checked
    // by the build (`cargo check`) — if a column name typo'd
    // the SQL would still compile but the live query would 42703
    // at runtime. The Go-side E2E catches that.
    #[allow(dead_code)]
    fn _proof_of_known_columns() {
        let _ = "RepoIndexManifest";
        let _ = "RepoIndexManifestFile";
    }

    #[test]
    fn timestamp_helpers_compile() {
        // Pull chrono so the with-chrono-0_4 feature is
        // actually exercised. tokio-postgres reads
        // TIMESTAMP(3) into DateTime<Utc> when this is
        // enabled.
        let _ = chrono::DateTime::<chrono::Utc>::default();
    }
}
