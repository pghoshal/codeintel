// Package asynqueues is the cross-binary registry of asynq queue
// names and task types. Mirrors the legacy BullMQ QUEUE_NAME
// literals so a producer enqueuing under name X is consumed by
// a worker subscribed to name X — the BullMQ → asynq translation
// preserves the queue identifier verbatim.
//
// Legacy mapping (from packages/backend/src/):
//
//	connectionManager.ts:20         QUEUE_NAME = "connection-sync-queue"
//	repoIndexManager.ts:24          QUEUE_NAME = "repo-index-queue"
//	ee/accountPermissionSyncer.ts:25 QUEUE_NAME = "accountPermissionSyncQueue"
//	ee/repoPermissionSyncer.ts:19   QUEUE_NAME = "repoPermissionSyncQueue"
//	R.9l Rust handoff             QUEUE_NAME = "code-graph-write"
//
// Identifier-style mismatch (kebab-case vs camelCase) is
// inherited from the legacy authors; we preserve it verbatim
// because the queue names are wire-frozen — a writer in the
// legacy fleet enqueuing under "repoPermissionSyncQueue" must
// be readable by the Go worker subscribed to the same string.
//
// asynq's task-type vs queue-name distinction: in asynq an
// individual task carries a Type string AND lands in a named
// Queue. The legacy BullMQ model is one queue per job kind,
// so we mirror that 1:1:
//
//	asynq.Queue == legacy QUEUE_NAME
//	asynq.Type  == legacy QUEUE_NAME  (single job kind per queue)
//
// This preserves the BullMQ wire shape (callers see one queue
// name) while staying idiomatic on the asynq side.
package asynqueues

// Queue names. Mirrors legacy QUEUE_NAME literals byte-for-byte.
// These strings appear on the wire (Redis stream keys, asynq
// inspector output, dashboard labels) — DO NOT renumber, DO NOT
// rename without coordinating a migration.
const (
	// QueueConnectionSync — legacy connection-sync-queue.
	// Producer: POST /api/connections/{id}/sync (slice 19);
	// also auto-enqueue after POST/PATCH on /api/connections
	// when ResetSync = true. Worker: codeintel-backend's
	// connectionmanager.ts port (Phase B.1).
	QueueConnectionSync = "connection-sync-queue"

	// QueueRepoIndex — legacy repo-index-queue. Producer:
	// connection-sync-queue worker, after a sync registers new
	// Repo rows; also POST /api/repos/{id}/index (when Kind is
	// CLEANUP or REMOVE_INDEX). Worker:
	// codeintel-backend's repoIndexManager.ts port (Phase C.1).
	QueueRepoIndex = "repo-index-queue"

	// QueueRepoIndexRust — the asynq queue the Rust indexer
	// (codeintel-indexer-rs) subscribes to. INDEX-kind tasks
	// route here so the Rust binary owns clone + (future)
	// Zoekt + SCIP. The Go worker DOES NOT subscribe to this
	// queue — see asynqueues.GoSubscribedQueues(). Producer:
	// AsynqRepoIndexer.Schedule when Kind=INDEX (R.2).
	//
	// HPA-safe: multiple Rust pods subscribe concurrently;
	// asynq's at-least-once delivery + stalled-job recovery
	// (managed by asynq itself) covers crash scenarios.
	QueueRepoIndexRust = "repo-index-rust"

	// QueueCodeGraphWrite — Rust indexer → Go backend graph
	// persistence handoff. Producer: codeintel-indexer-rs after
	// it has built a scoped graph snapshot and rendered the
	// deterministic NGQL statements. Worker: codeintel-backend's
	// codegraphwriter handler, which validates tenant scope,
	// executes via the Go Nebula graph store, and records
	// CodeGraphIndex/CodeGraphRevision state.
	//
	// This queue has no legacy BullMQ counterpart; it is a new
	// port-boundary queue introduced because the Rust worker must
	// not own the production Nebula client.
	QueueCodeGraphWrite = "code-graph-write"

	// QueueAccountPermissionSync — legacy accountPermissionSyncQueue
	// (EE). Producer: NextAuth session login flow (Phase G).
	// Worker: ee/accountPermissionSyncer.ts port (Phase H).
	QueueAccountPermissionSync = "accountPermissionSyncQueue"

	// QueueRepoPermissionSync — legacy repoPermissionSyncQueue
	// (EE). Producer: repo-index-queue worker after a successful
	// index registers the repo. Worker:
	// ee/repoPermissionSyncer.ts port (Phase H).
	QueueRepoPermissionSync = "repoPermissionSyncQueue"

	// QueueLLMCompletion is the backend-owned durable chat/ask
	// synthesis queue. Producers are internal backend LLM gateway
	// POST requests after a request row is persisted. Consumers
	// run in codeintel-backend and perform provider I/O outside
	// codeintel-app and outside the gateway HTTP request handler.
	QueueLLMCompletion = "codeintel-llm-completion"

	// QueueIndexCore is the thin codeintel-indexer-rs-core
	// executor queue. It carries clone/materialization, Zoekt,
	// AST/tree-sitter, graph merge, and activation subjobs.
	QueueIndexCore = "codeintel-index-core"

	// QueueIndexSCIPTSPython is a hot-pool queue for TS/JS and
	// Python SCIP work.
	QueueIndexSCIPTSPython = "codeintel-index-scip-ts-python"

	// QueueIndexSCIPGo is a hot-pool queue for Go SCIP work.
	QueueIndexSCIPGo = "codeintel-index-scip-go"

	// QueueIndexSCIPJVM is an ephemeral heavy-worker queue for
	// JVM SCIP work.
	QueueIndexSCIPJVM = "codeintel-index-scip-jvm"

	// QueueIndexSCIPDotnet is an ephemeral heavy-worker queue for
	// .NET SCIP work.
	QueueIndexSCIPDotnet = "codeintel-index-scip-dotnet"

	// QueueIndexSCIPRustDart is an ephemeral heavy-worker queue
	// for Cargo/rust-analyzer and Dart SCIP work.
	QueueIndexSCIPRustDart = "codeintel-index-scip-rust-dart"

	// QueueIndexSCIPCPPX86 is an amd64-only cold queue for
	// C/C++ SCIP work.
	QueueIndexSCIPCPPX86 = "codeintel-index-scip-cpp-x86"

	// QueueIndexSCIPRubyX86 is an amd64-only cold queue for Ruby
	// SCIP work.
	QueueIndexSCIPRubyX86 = "codeintel-index-scip-ruby-x86"
)

// All returns every Go-backend-subscribed queue name in
// declaration order. Used by the codeintel-backend's asynq
// Server config (`Queues: map[string]int{...}`) to subscribe a
// single Server instance to every queue. Caller decides
// priority; this slice returns each at priority 1 (BullMQ has
// no per-queue priority among the legacy queues, and the new
// graph-write queue starts at the same priority until production
// telemetry says otherwise).
//
// Note: QueueRepoIndexRust is intentionally OMITTED — that
// queue is owned by codeintel-indexer-rs (Rust), and the Go
// backend must not subscribe to it. See
// GoSubscribedQueues().
func All() []string {
	return []string{
		QueueConnectionSync,
		QueueRepoIndex,
		QueueCodeGraphWrite,
		QueueAccountPermissionSync,
		QueueRepoPermissionSync,
		QueueLLMCompletion,
	}
}

// GoSubscribedQueues is the explicit list the Go-side asynq
// Server should subscribe to. Kept as a named function so tests
// can assert Rust-owned queues stay excluded.
func GoSubscribedQueues() []string { return All() }

// DefaultPriorities returns the map shape asynq.Server.Queues
// expects (queue name → priority weight). Every Go-subscribed
// queue starts at priority 1; the asynq Server drains them
// uniformly when weights are equal.
//
// Future slices that introduce a slow queue (e.g., heavy index
// jobs that should yield to fast sync jobs) can override this
// at the call site without touching the registry.
func DefaultPriorities() map[string]int {
	queues := GoSubscribedQueues()
	out := make(map[string]int, len(queues))
	for _, q := range queues {
		out[q] = 1
	}
	return out
}

// ExecutorQueues returns the queue names used by the backend-owned
// hot/cold indexing executor fabric. They are intentionally not part
// of the default backend control-plane subscription set; production
// executor consumers are wired explicitly so queue ownership remains
// in codeintel-backend rather than in app/read-path pods.
func ExecutorQueues() []string {
	return []string{
		QueueIndexCore,
		QueueIndexSCIPTSPython,
		QueueIndexSCIPGo,
		QueueIndexSCIPJVM,
		QueueIndexSCIPDotnet,
		QueueIndexSCIPRustDart,
		QueueIndexSCIPCPPX86,
		QueueIndexSCIPRubyX86,
	}
}
