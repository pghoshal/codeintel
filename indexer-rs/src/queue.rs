//! Wire types matching the Go-side
//! `codeintel/pkg/repoindex.TaskPayload`. The Rust worker
//! decodes JSON from asynq task bytes; the encoded shape is
//! byte-equal to what Go's repoindex.Marshal produces.
//!
//! No queue-protocol code here — asynq itself owns the Redis
//! schema, retries, stalled-job recovery, etc. Rust just
//! consumes via the `asynq` crate (v0.1.8), which is wire-
//! compatible with `hibiken/asynq` (Go).

use serde::{Deserialize, Serialize};

use crate::nebula_ngql::CodeGraphSnapshotAnchor;

/// JobType mirrors Go's `repoindex.JobType` enum:
/// INDEX | CLEANUP | REMOVE_INDEX. Only INDEX is handled by
/// the Rust indexer; CLEANUP and REMOVE_INDEX stay on the Go
/// worker (DB + filesystem cleanup lives there).
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "SCREAMING_SNAKE_CASE")]
pub enum JobType {
    Index,
    Cleanup,
    RemoveIndex,
}

/// TaskPayload mirrors Go's
/// `codeintel/pkg/repoindex.TaskPayload`. JSON field names
/// match (camelCase) so the encoded bytes round-trip byte-equal
/// between the two languages.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TaskPayload {
    #[serde(rename = "type")]
    pub job_type: JobType,
    #[serde(rename = "jobId")]
    pub job_id: String,
    #[serde(rename = "repoId")]
    pub repo_id: i32,
    #[serde(rename = "repoName")]
    pub repo_name: String,
}

/// Queue name the Rust indexer subscribes to. The Go producer
/// (AsynqRepoIndexer.Schedule) routes INDEX tasks to THIS
/// queue, and CLEANUP / REMOVE_INDEX to the legacy
/// `repo-index-queue` consumed by Go's repoindexmanager.
///
/// MUST agree with the Go-side constant in
/// `codeintel/pkg/asynqueues`.
pub const RUST_INDEX_QUEUE: &str = "repo-index-rust";

/// TASK_TYPE_NAME is the asynq task TypeName label the Go
/// producer stamps on INDEX tasks. The Rust ServeMux registers
/// a handler under this exact name. Sharing the constant with
/// the Go side keeps a future renames in lockstep — the rename
/// would touch both `codeintel/pkg/asynqueues` and this file.
pub const TASK_TYPE_NAME: &str = "repo-index-rust";

/// CODE_GRAPH_WRITE_QUEUE is the queue the Rust worker
/// PRODUCES to after rendering NGQL snapshot statements for
/// a revision. The matching CONSUMER is the Go-side
/// `nebulaCodeGraphStore` worker (slice 45). Decoupling Rust
/// rendering from Go execution via asynq:
///   - HPA-safe: many Rust pods enqueue concurrently; many Go
///     pods consume concurrently; asynq's at-least-once
///     delivery + stalled-job sweeper covers crashes.
///   - Failure-isolated: a transient Nebula outage doesn't
///     fail the indexing job; the write task retries on the
///     Go side per asynq's retry policy.
///   - Brand-renamed: legacy didn't have a separate write
///     queue (the legacy indexer wrote to Nebula in-process).
///     The Rust port introduces the queue boundary because no
///     Rust Nebula client exists.
///
/// MUST agree with the Go-side const that subscribes the
/// consumer (added in a follow-up R.9l-go slice).
pub const CODE_GRAPH_WRITE_QUEUE: &str = "code-graph-write";

/// CODE_GRAPH_WRITE_TASK_TYPE — single asynq Type per queue
/// (same pattern as RUST_INDEX_QUEUE).
pub const CODE_GRAPH_WRITE_TASK_TYPE: &str = "code-graph-write";

/// CodeGraphWritePayload is the JSON shape Rust enqueues
/// for the Go-side consumer to decode. Contains the snapshot
/// scope (so the consumer can route to the right Nebula
/// space / tenant) plus the already-rendered NGQL statements
/// ready for sequential execution.
///
/// JSON field naming matches the Go-side struct conventions
/// (camelCase) so both ends round-trip byte-equal.
///
/// **Wire-protocol divergence vs legacy in-process path**:
/// The legacy code-graph write (Go-side
/// `nebulaCodeGraphStore.WriteSnapshot` in slice 45) takes a
/// `CodeGraphSnapshot` struct and renders NGQL statements
/// inside the Go process via `graphschema.RenderSnapshotStatements`.
/// R.9l instead ships ALREADY-RENDERED statements as a
/// `Vec<String>` because the Rust port renders Nebula NGQL
/// natively (`nebula_ngql::render_snapshot_statements`).
///
/// **Consumer contract**:
///   - The first N statements where N == len(legacy
///     RenderSchemaStatements()) are schema DDL — the
///     consumer SHOULD execute them with the legacy
///     ensureSchema/executeWithSchemaRetry path so a missing
///     tag / index is recreated transparently. The remaining
///     statements are INSERT VERTEX / INSERT EDGE chunks
///     produced by `render_vertex_insert` / `render_edge_insert`
///     in deterministic order.
///   - The consumer MUST validate that the payload's
///     `(orgId, workspaceId, repoId)` triple matches the
///     repo's owner via the same tenant-isolation rules every
///     other Go-side handler enforces. A malicious Redis
///     writer could otherwise spoof a payload across tenants.
///   - Retries are safe: NGQL `INSERT VERTEX` and `INSERT
///     EDGE` are upsert-by-VID/(from, to, rank) — duplicate
///     execution on asynq's at-least-once delivery overwrites
///     idempotently.
///
/// **MaxRetry / Retention / Unique task options**: the asynq
/// 0.1.8 Rust crate doesn't expose these in its public Task
/// builder (only `with_queue`, `with_task_id`,
/// `with_process_at`). Tasks therefore use the asynq Go
/// defaults (25 retries, default retention). If the Go-side
/// consumer needs stricter retention, it should be configured
/// on the Server side via `ServerConfig`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CodeGraphWritePayload {
    #[serde(rename = "orgId")]
    pub org_id: i64,
    #[serde(rename = "workspaceId")]
    pub workspace_id: String,
    #[serde(rename = "repoId")]
    pub repo_id: i64,
    pub branch: String,
    pub revision: String,
    #[serde(rename = "commitHash")]
    pub commit_hash: String,
    #[serde(rename = "schemaVersion")]
    pub schema_version: i64,
    #[serde(rename = "builderVersion")]
    pub builder_version: String,
    /// The RepoIndexingJob that produced this snapshot. The Go
    /// consumer stores it as CodeGraphIndex.indexRunId and uses it
    /// with manifestId to reject stale delayed tasks.
    #[serde(rename = "indexJobId")]
    pub index_job_id: String,
    /// The exact RepoIndexManifest row this snapshot belongs to.
    /// Go activates CodeGraphRevision only if this manifest is READY.
    #[serde(rename = "manifestId")]
    pub manifest_id: String,
    /// Mirrors RepoIndexManifest.providerConnectionId. None means the
    /// repo has no provider-connection scope.
    #[serde(rename = "providerConnectionId")]
    pub provider_connection_id: Option<String>,
    /// The coarse producer path ("syntactic-ast", "scip-builder",
    /// ...). Individual edge rows still carry their own `source`
    /// property inside the rendered statements.
    pub source: String,
    /// Rendered NGQL statements in the order the consumer
    /// MUST execute them (schema DDL first, then INSERTs).
    pub statements: Vec<String>,
    /// Postgres-backed anchor sidecar rows. These are not part of
    /// NGQL rendering; the Go writer persists them to CodeGraphAnchor
    /// and uses them to report anchorCount.
    #[serde(default)]
    pub anchors: Vec<CodeGraphSnapshotAnchor>,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn code_graph_write_payload_round_trips_camelcase_json() {
        // Confirm the JSON shape the Go-side consumer will
        // decode matches the legacy camelCase wire convention.
        let payload = CodeGraphWritePayload {
            org_id: 42,
            workspace_id: "ws-atom".to_string(),
            repo_id: 7,
            branch: "refs/heads/main".to_string(),
            revision: "refs/heads/main".to_string(),
            commit_hash: "a".repeat(40),
            schema_version: 1,
            builder_version: "codeintel-code-graph-v7".to_string(),
            index_job_id: "job-1".to_string(),
            manifest_id: "manifest-1".to_string(),
            provider_connection_id: Some("conn-1".to_string()),
            source: "syntactic-ast".to_string(),
            statements: vec![
                "CREATE TAG IF NOT EXISTS x;".to_string(),
                "INSERT VERTEX y;".to_string(),
            ],
            anchors: vec![CodeGraphSnapshotAnchor {
                kind: "symbol".to_string(),
                direction: "PROVIDES".to_string(),
                key: "handler".to_string(),
                normalized_key: "handler".to_string(),
                node_vid: "cg:o42:wabc:r7:caaaaaaaaaaaa:s1:babc:function:key".to_string(),
                evidence_file_path: Some("src/index.ts".to_string()),
                start_line: Some(1),
                end_line: Some(3),
                confidence: 0.95,
                confidence_tier: "EXTRACTED".to_string(),
                source: "scip-typescript".to_string(),
            }],
        };
        let encoded = serde_json::to_string(&payload).expect("encode");
        // Wire-key checks: camelCase + snake-case-where-legacy-uses-it.
        assert!(encoded.contains("\"orgId\":42"), "{}", encoded);
        assert!(
            encoded.contains("\"workspaceId\":\"ws-atom\""),
            "{}",
            encoded
        );
        assert!(encoded.contains("\"repoId\":7"), "{}", encoded);
        assert!(encoded.contains("\"commitHash\":"), "{}", encoded);
        assert!(encoded.contains("\"schemaVersion\":1"), "{}", encoded);
        assert!(encoded.contains("\"builderVersion\":"), "{}", encoded);
        assert!(encoded.contains("\"indexJobId\":\"job-1\""), "{}", encoded);
        assert!(
            encoded.contains("\"manifestId\":\"manifest-1\""),
            "{}",
            encoded
        );
        assert!(
            encoded.contains("\"providerConnectionId\":\"conn-1\""),
            "{}",
            encoded
        );
        assert!(
            encoded.contains("\"branch\":\"refs/heads/main\""),
            "{}",
            encoded
        );
        assert!(encoded.contains("\"revision\":"), "{}", encoded);
        assert!(
            encoded.contains("\"source\":\"syntactic-ast\""),
            "{}",
            encoded
        );
        assert!(encoded.contains("\"statements\":"), "{}", encoded);
        assert!(encoded.contains("\"anchors\":"), "{}", encoded);
        assert!(
            encoded.contains("\"normalizedKey\":\"handler\""),
            "{}",
            encoded
        );
        // Round-trip.
        let decoded: CodeGraphWritePayload = serde_json::from_str(&encoded).expect("decode");
        assert_eq!(decoded.org_id, 42);
        assert_eq!(decoded.index_job_id, "job-1");
        assert_eq!(decoded.manifest_id, "manifest-1");
        assert_eq!(decoded.provider_connection_id.as_deref(), Some("conn-1"));
        assert_eq!(decoded.branch, "refs/heads/main");
        assert_eq!(decoded.statements.len(), 2);
        assert_eq!(decoded.anchors.len(), 1);
        assert_eq!(
            decoded.anchors[0].node_vid,
            "cg:o42:wabc:r7:caaaaaaaaaaaa:s1:babc:function:key"
        );
    }

    #[test]
    fn queue_and_task_type_names_are_stable() {
        // Wire constants — any rename here is a coordinated
        // multi-language migration.
        assert_eq!(CODE_GRAPH_WRITE_QUEUE, "code-graph-write");
        assert_eq!(CODE_GRAPH_WRITE_TASK_TYPE, "code-graph-write");
    }
}
