# Codeintel

Self-hosted code intelligence for multi-repository engineering teams.

<p align="center">
  <img src="assets/codeintel-logo.svg" alt="Codeintel logo" width="118">
</p>

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Codeintel indexes repositories per workspace, combines lexical search,
SCIP symbols, AST/tree-sitter facts, and a graph database, then exposes
the result through HTTP APIs, MCP tools, chat APIs, and an operator
verification console. It is designed for agentic development workflows
where tools such as coding assistants need exact files, symbols, call
paths, branch context, architecture evidence, and tenant-safe retrieval.

> Project status: active development. The core Go/Rust split, tenant
> model, indexing lifecycle, search/MCP/chat APIs, admin verification UI,
> and local infrastructure are present. Production packaging, provider
> app-based OAuth flows, and some large-scale graph/indexer optimizations
> are still being hardened.

## Why Codeintel

Modern engineering organizations rarely have a single repository or a
single language. A useful coding agent needs more than grep output:

- Which branch and workspace is this answer scoped to?
- Which file owns the entry point?
- Which function calls which downstream function?
- Which generated protobuf, route, receiver, event, or background worker
  completes the flow?
- Which repos participate in the architecture path?
- Which evidence is lexical, semantic, AST-derived, or graph-derived?
- What should a coding harness read, modify, and test next?

Codeintel is built around that product shape:

- Multi-tenant workspaces with per-workspace API keys.
- Per-workspace secrets, language-model configuration, VCS connections,
  repo selection, branch policy, index/reindex, and remove-index.
- Zoekt lexical search for fast code recall.
- SCIP symbol extraction for definitions, references, types, and
  implementation-level signal.
- AST/tree-sitter and framework extraction for routes, events, imports,
  calls, SDK injection points, protobuf boundaries, and other code facts.
- NebulaGraph-backed graph traversal for architecture-level code paths.
- MCP tools and chat APIs designed for coding harnesses, not just Q&A.
- Stateless app replicas and queue-backed backend/indexer execution for
  horizontal scaling.

## Architecture

Codeintel is split into three production binaries.

| Binary | Language | Responsibility |
| --- | --- | --- |
| `codeintel-app` | Go | Public HTTP API, MCP endpoint, auth, tenant scoping, fast read path, search/chat orchestration |
| `codeintel-backend` | Go | Queue workers, repo sync/index scheduling, durable LLM jobs, graph writes, audit forwarding |
| `codeintel-indexer-rs` | Rust | CPU-heavy indexing: Zoekt artifact execution, SCIP extraction, AST/tree-sitter facts, graph snapshots |

Supporting infrastructure:

| Component | Purpose |
| --- | --- |
| PostgreSQL | Tenant/workspace control plane, repos, branches, secrets, models, manifests, jobs, SCIP rows, chat state |
| Redis | asynq queues, retry state, bounded cache, request dedupe |
| NebulaGraph | Code graph vertices/edges and graph traversal |
| Zoekt | Lexical code index and search serving |
| Shared filesystem/object storage | Git worktrees and index artifacts for worker handoff |

High-level indexing flow:

```mermaid
flowchart TB
  Admin["Atom or Admin UI\nworkspace, VCS, repo, branch selection"]
  App["codeintel-app\nHTTP control surface"]
  PG["PostgreSQL\norgs, repos, branch policy,\nsecrets, jobs, manifests"]
  Backend["codeintel-backend\nscheduler and queue workers"]
  Redis["Redis / asynq\nretry-safe job queues"]
  Indexer["codeintel-indexer-rs\nper-language worker classes"]
  Git["Git providers\nGitHub, GitLab, Bitbucket, generic Git"]
  Worktree["Shared worktree/artifact storage\nrepo + branch materialization"]
  Planner["Index planner\nrepo x selected branches"]
  ZoektBuild["Zoekt indexing\nlexical shards per org/repo/ref"]
  SCIPBuild["SCIP indexing\ndefinitions, references, types"]
  ASTBuild["AST/tree-sitter indexing\nroutes, calls, imports, framework facts"]
  Snapshot["Graph snapshot builder\nanchors, nodes, semantic edges"]
  ZoektStore["Zoekt shard store\n/org-id/index"]
  SCIPStore["PostgreSQL SCIP tables\nsymbols, occurrences, relationships"]
  Manifest["PostgreSQL index manifests\nlayer status, commit, quality, delta plan"]
  Nebula["NebulaGraph\ncode graph vertices and edges"]

  Admin -->|create workspace, configure VCS, select repos and branches| App
  App -->|tenant-scoped writes| PG
  App -->|index, reindex, remove-index request| Backend
  Backend -->|enqueue durable work| Redis
  Redis --> Backend
  Backend --> Planner
  Planner -->|one plan per repo/ref\nmain, release, feature branches| Indexer
  Indexer -->|clone or fetch selected ref| Git
  Indexer --> Worktree
  Worktree --> ZoektBuild
  Worktree --> SCIPBuild
  Worktree --> ASTBuild
  SCIPBuild --> Snapshot
  ASTBuild --> Snapshot
  ZoektBuild --> ZoektStore
  SCIPBuild --> SCIPStore
  Snapshot -->|artifact handoff| Backend
  Backend --> Nebula
  Backend --> Manifest
  Backend --> PG
  Backend -->|activate or retire branch index| PG
```

Query and MCP/chat fusion flow:

```mermaid
flowchart TB
  Client["MCP client, coding harness,\nchat API, or Admin UI"]
  App["codeintel-app\nAPI, auth, tenant boundary"]
  Scope["Scope resolver\norg + repos + default/explicit branch"]
  Lexical["Zoekt recall\ntext, regex, file slices"]
  Semantic["SCIP lookup\nsymbol definitions and references"]
  Structural["AST/tree-sitter facts\nroutes, calls, imports, config, tests"]
  Graph["NebulaGraph traversal\ncall paths, impact, cross-repo flow"]
  PGRead["PostgreSQL read path\nmanifests, active snapshots,\nSCIP rows, repo metadata"]
  Evidence["Fused evidence pack\nfiles, symbols, edges, confidence, gaps"]
  LLMGateway["codeintel-backend LLM gateway\ndurable synthesis, retry, resume"]
  Answer["MCP/chat answer\nHLD, LLD, diagrams, file map,\ncode navigation, test plan"]

  Client --> App
  App --> Scope
  Scope --> PGRead
  Scope --> Lexical
  Scope --> Semantic
  Scope --> Structural
  Scope --> Graph
  Lexical --> Evidence
  Semantic --> Evidence
  Structural --> Evidence
  Graph --> Evidence
  PGRead --> Evidence
  Evidence -->|deterministic tool result| Client
  Evidence -->|ask_codebase or chat| LLMGateway
  LLMGateway --> Answer
  Answer --> Client
```

Branch behavior:

- A workspace can select different branches per repository.
- Every index plan is scoped to `{org, repo, ref}` so the same repo name
  in two workspaces, or the same repo with two branches, does not share
  search or graph state.
- If a query omits `ref`, MCP/chat resolves the repository's configured
  default branch.
- `compare_branches` can compare local materialized branches without
  requiring those branches to be search-indexed, but semantic Q&A over a
  branch needs active Zoekt/SCIP/AST/graph coverage for that ref.
- Remove-index retires the selected `{repo, ref}` from active search and
  graph visibility while leaving the remote provider repository untouched.

## Repository Layout

```text
codeintel/
├── cmd/
│   ├── codeintel-app/             # HTTP API, MCP, chat, read path
│   ├── codeintel-backend/         # queue workers, LLM gateway, schedulers
│   └── codeintel-graph-init/      # graph initialization helper
├── internal/
│   ├── api/                       # HTTP handlers
│   ├── backend/                   # backend-only worker packages
│   ├── db/                        # app-side typed queries
│   ├── graphreader/               # Nebula read path
│   ├── mcp/                       # MCP tools and chat orchestration
│   ├── migrate/                   # embedded SQL migrations
│   ├── obs/                       # HTTP observability middleware
│   ├── retrievalpolicy/           # query/evidence policy
│   └── search/                    # Zoekt search routing
├── pkg/                           # cross-binary shared packages
├── indexer-rs/                    # Rust indexer workspace
├── proto/                         # gRPC and SCIP protocol files
├── admin-ui/                      # operator verification console
├── deploy/                        # Dockerfiles and example worker manifests
├── tests/                         # integration, parity, quality gates
├── third_party/zoekt/             # vendored Zoekt integration
├── docker-compose.yml             # local Postgres/Redis/Nebula stack
└── Makefile                       # local stack lifecycle
```

## Feature Matrix

| Capability | Status |
| --- | --- |
| Atom-style workspace provisioning | Implemented through `POST /api/atom/workspaces` |
| Per-workspace API keys | Implemented |
| Per-workspace secrets | Implemented |
| Per-workspace LLM model configuration | Implemented |
| Generic Git connection records | Implemented |
| GitHub/GitLab/Bitbucket app-based OAuth | Planned; UI cards are intentionally disabled until API support lands |
| Repo listing, branch policy, index/reindex/remove-index | Implemented |
| Branch-scoped remove-index | Implemented at API/job contract level |
| Zoekt lexical search | Implemented when Zoekt endpoints are configured |
| SCIP semantic symbol tools | Implemented, coverage depends on enabled worker classes |
| AST/tree-sitter graph facts | Implemented with ongoing language-depth expansion |
| NebulaGraph code graph | Implemented |
| MCP endpoint | Implemented at `/api/{domain}/mcp` |
| `ask_codebase` durable synthesis | Implemented through backend LLM jobs |
| HTTP chat API | Implemented |
| Admin verification UI | Implemented |
| Production Helm chart | In progress |

## Prerequisites

For local development:

- Go 1.23+
- Rust 1.92+
- Docker and Docker Compose
- Make
- Git
- Node.js 20+ only for the admin verification UI

Optional, depending on what you run:

- `kubectl` and Kind for Kubernetes product gates.
- Language SCIP toolchains for full semantic indexing.
- A compatible OpenAI-style LLM endpoint for `ask_codebase` and chat.

## Quick Start

Start local infrastructure:

```bash
cd codeintel
make stack-up
```

This starts:

| Service | Local address | Credentials |
| --- | --- | --- |
| PostgreSQL | `127.0.0.1:5433` | `codeintel` / `codeintel` / db `codeintel` |
| Redis | `127.0.0.1:6380` | none |
| NebulaGraph graphd | `127.0.0.1:9669` | `root` / `nebula` |

Build the Go binaries:

```bash
cd codeintel
go build -o bin/codeintel-app ./cmd/codeintel-app
go build -o bin/codeintel-backend ./cmd/codeintel-backend
```

Run the backend:

```bash
cd codeintel
CODEINTEL_DATABASE_URL='postgresql://codeintel:codeintel@127.0.0.1:5433/codeintel?sslmode=disable' \
CODEINTEL_REDIS_URL='redis://127.0.0.1:6380/0' \
CODEINTEL_BACKEND_GRPC_ADDR='127.0.0.1:3101' \
CODEINTEL_BACKEND_CONTROL_ADDR='127.0.0.1:3102' \
CODEINTEL_BACKEND_CONTROL_TOKEN='dev-control-token' \
CODEINTEL_BACKEND_LLM_ADDR='127.0.0.1:3103' \
CODEINTEL_BACKEND_LLM_TOKEN='dev-llm-token' \
CODEINTEL_BACKEND_QUEUE_MODE='control' \
CODEINTEL_NEBULA_ADDR='127.0.0.1:9669' \
CODEINTEL_NEBULA_USER='root' \
CODEINTEL_NEBULA_PASSWORD='nebula' \
CODEINTEL_NEBULA_SPACE='codeintel' \
./bin/codeintel-backend
```

Run the app:

```bash
cd codeintel
CODEINTEL_LISTEN_ADDR='127.0.0.1:3000' \
CODEINTEL_DATABASE_URL='postgresql://codeintel:codeintel@127.0.0.1:5433/codeintel?sslmode=disable' \
CODEINTEL_ENCRYPTION_KEY='dev-encryption-key-change-me' \
CODEINTEL_AUTO_MIGRATE='true' \
CODEINTEL_REDIS_URL='redis://127.0.0.1:6380/0' \
CODEINTEL_BACKEND_GRPC_ADDR='127.0.0.1:3101' \
CODEINTEL_BACKEND_CONTROL_URL='http://127.0.0.1:3102' \
CODEINTEL_BACKEND_CONTROL_TOKEN='dev-control-token' \
CODEINTEL_LLM_GATEWAY_URL='http://127.0.0.1:3103' \
CODEINTEL_BACKEND_LLM_TOKEN='dev-llm-token' \
CODEINTEL_ATOM_CONTROL_PLANE_TOKEN='dev-atom-token' \
CODEINTEL_NEBULA_ADDR='127.0.0.1:9669' \
CODEINTEL_NEBULA_USER='root' \
CODEINTEL_NEBULA_PASSWORD='nebula' \
CODEINTEL_NEBULA_SPACE='codeintel' \
./bin/codeintel-app
```

Check health:

```bash
curl http://127.0.0.1:3000/api/health
curl http://127.0.0.1:3000/readyz
```

## Schema Bootstrap and Migrations

Codeintel has two initialization tracks: PostgreSQL migrations for the
control plane and NebulaGraph schema bootstrap for the code graph.

### PostgreSQL

PostgreSQL migrations are embedded in the Go binary from:

```text
internal/migrate/migrations/
```

Current migration files:

```text
0001_initial.sql
0002_connection_sync_jobs.sql
0003_connection_orgid_idx.sql
0004_repo_indexing_jobs.sql
0005_audit_events.sql
0006_repo_extended_columns.sql
0007_repo_indexing_jobs_createdat_idx.sql
0008_code_intel_index.sql
0009_code_graph_index.sql
0010_repo_connection_sync_columns.sql
0011_enum_types_parity.sql
0012_zoekt_cluster_tables.sql
0013_repo_additive_parity.sql
0014_auth_tables.sql
0015_permission_sync_tables.sql
0016_oauth_tables.sql
0017_chat_tables.sql
0018_codeintel_extended_tables.sql
0019_search_manifest_orgconfig.sql
0020_enum_column_reconciliation.sql
0021_codegraph_extended_tables.sql
0022_audit_rename.sql
0023_timestamp_notnull_reconciliation.sql
0024_index_subjobs.sql
0025_index_subjob_dispatch_lock.sql
0026_codeintel_language_child_scope.sql
0027_codeintel_index_workspace_branch_scope.sql
0028_repo_indexing_job_latest_lookup.sql
0029_repo_indexing_job_latest_tie_breaker.sql
0030_llm_request_state.sql
0031_codeintel_symbol_lookup_indexes.sql
0032_codeintel_occurrence_lookup_indexes.sql
0033_codegraph_semantic_edge_lookup_indexes.sql
```

The migrator creates `schema_migrations` and applies unapplied SQL files
in version order. Each migration runs in its own transaction and records
its version in the same transaction. Re-running the migrator is a no-op
when the database is current.

For local development, let `codeintel-app` apply migrations on first
boot:

```bash
CODEINTEL_AUTO_MIGRATE=true ./bin/codeintel-app
```

For production, run migrations as a controlled deployment step before
rolling app/backend pods. Today the embedded migrator is exposed through
the app boot path; a dedicated migration-only command is a recommended
release hardening item for operators who do not want application pods to
own schema changes.

### NebulaGraph

NebulaGraph is initialized by the one-shot bootstrap binary:

```text
cmd/codeintel-graph-init
```

It creates the `codeintel` graph space and every graph schema object the
reader/writer expects:

- `code_graph_node` tag
- `code_graph_edge` edge
- scope, label, key, path, and route-path tag indexes

The bootstrap is idempotent: `CREATE SPACE`, `CREATE TAG`, `CREATE
EDGE`, and `CREATE TAG INDEX` statements use `IF NOT EXISTS`, and the
binary waits for the space to become usable before creating schema
objects.

Build and run:

```bash
cd codeintel
go build -o bin/codeintel-graph-init ./cmd/codeintel-graph-init

CODEINTEL_NEBULA_ADDR='127.0.0.1:9669' \
CODEINTEL_NEBULA_USER='root' \
CODEINTEL_NEBULA_PASSWORD='nebula' \
CODEINTEL_NEBULA_SPACE='codeintel' \
CODEINTEL_NEBULA_PARTITIONS='10' \
CODEINTEL_NEBULA_REPLICA_FACTOR='1' \
CODEINTEL_NEBULA_VID_LENGTH='128' \
./bin/codeintel-graph-init
```

Production HA clusters should set `CODEINTEL_NEBULA_REPLICA_FACTOR` to
the storage replication factor used by the NebulaGraph deployment, for
example `3`.

## Marketing Assets

Repository-ready assets live under `assets/`. Use the PNG for GitHub's
repository social preview upload because GitHub only accepts PNG, GIF,
or JPG images for that setting.

| Asset | Use |
| --- | --- |
| `assets/codeintel-logo.svg` | Square logo/avatar |
| `assets/codeintel-label.svg` | README/repository label or compact wordmark |
| `assets/codeintel-social-preview.svg` | Editable vector source for the social preview |
| `assets/codeintel-social-preview.png` | GitHub social preview upload image, `1280x640` |

Preview:

<p align="center">
  <img src="assets/codeintel-label.svg" alt="Codeintel label" width="520">
</p>

## Admin Verification Console

The admin console is a human verification surface. It is useful for
testing workspace creation, LLM setup, VCS connection records, repo
selection, branch indexing, search, chat, MCP calls, and code reference
navigation.

Run it with the app API as the proxy target:

```bash
cd admin-ui
CODEINTEL_API_BASE=http://127.0.0.1:3000 node server.mjs
```

Open:

```text
http://127.0.0.1:4177
```

The console stores non-secret workspace preferences in browser storage.
Workspace API keys and control-plane tokens are session-scoped.

## Authentication

Most APIs require a workspace API key:

```http
X-Api-Key: cik_<secret>
```

The API also accepts:

```http
Authorization: Bearer cik_<secret>
```

Atom workspace provisioning uses a separate control-plane token:

```http
X-Codeintel-Atom-Token: <token>
```

or:

```http
Authorization: Bearer <token>
```

Every tenant-scoped request resolves the organization from the API key.
The requested domain, repo, branch, search, graph, and MCP operations
are then filtered by that organization.

## Core API Lifecycle

All `/api/*` routes are served by `codeintel-app`. Unless explicitly
marked as control-plane or public health, routes require `X-Api-Key:
cik_<secret>` or `Authorization: Bearer cik_<secret>`.

Full HTTP API reference:

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `GET` | `/api/health` | public | Basic app health |
| `GET` | `/api/version` | public | Build/version metadata |
| `GET` | `/healthz` | public | Kubernetes liveness |
| `GET` | `/readyz` | public | Kubernetes readiness with DB ping |
| `GET` | `/metrics` | public when enabled | Prometheus metrics |
| `POST` | `/api/atom/workspaces` | Atom token | Create/update tenant from Atom workspace id and optionally mint an API key |
| `GET` | `/api/tenants/{domain}/metadata` | API key | Verify tenant metadata for the authenticated workspace |
| `GET` | `/api/secrets` | API key | List organization secret metadata without plaintext values |
| `PUT` | `/api/secrets` | owner API key | Create/update an organization secret |
| `DELETE` | `/api/secrets/{key}` | owner API key | Delete a secret if not referenced by model/connection config |
| `GET` | `/api/models` | API key | List enabled language-model metadata |
| `PUT` | `/api/models` | owner API key | Replace the full organization language-model set |
| `GET` | `/api/connections` | API key | List VCS/provider connections |
| `POST` | `/api/connections` | owner API key | Create/update a VCS connection |
| `PATCH` | `/api/connections/{id}` | owner API key | Partially update a VCS connection |
| `DELETE` | `/api/connections/{id}` | owner API key | Delete a VCS connection |
| `POST` | `/api/connections/{id}/sync` | owner API key | Trigger provider/repo sync |
| `GET` | `/api/connections/{id}/status` | API key | Read connection sync status and failures |
| `GET` | `/api/connections/{id}/branches` | API key | Read connection branch policy |
| `PUT` | `/api/connections/{id}/branches` | owner API key | Update connection branch policy |
| `GET` | `/api/status` | API key | Organization status rollup: repos, indexing, Zoekt, graph |
| `GET` | `/api/repos` | API key | Paginated repository list with index/branch state |
| `GET` | `/api/repos/{id}/status` | API key | Detailed repo index status |
| `GET` | `/api/repos/{id}/branches` | API key | Repo selected/default branches |
| `PUT` | `/api/repos/{id}/branches` | owner API key | Update repo selected/default branches |
| `POST` | `/api/repos/{id}/index` | owner API key | Schedule index/reindex for selected branch/ref |
| `DELETE` | `/api/repos/{id}/index` | owner API key | Schedule remove-index for selected branch/ref |
| `GET` | `/api/search-contexts` | API key | List saved search/chat contexts |
| `PUT` | `/api/search-contexts` | owner API key | Create/update saved search/chat contexts |
| `POST` | `/api/search` | API key | Zoekt-backed lexical code search |
| `POST` | `/api/chat` | API key | Async chat/ask request using fused retrieval and durable backend LLM state |
| `POST` | `/api/chat/blocking` | API key | Blocking chat/ask request for manual validation |
| `GET` | `/api/chat/{id}/result` | API key | Poll async chat result |
| `POST` | `/api/{domain}/mcp` | API key | Stateless JSON-RPC MCP endpoint |
| `GET` | `/api/{domain}/mcp` | API key | MCP GET transport placeholder; stateless mode returns method-not-allowed |

Common query/body contracts:

| Surface | Important fields |
| --- | --- |
| `/api/repos` | `query`, `page`, `perPage`, `sort=name\|pushed`, `direction=asc\|desc` |
| `/api/repos/{id}/index` | JSON body or query can carry `branch`/`ref`; omitted ref uses configured default |
| `/api/repos/{id}/index` `DELETE` | `?ref=refs/heads/main` removes that branch/ref index |
| `/api/search` | JSON body must include `query`; other fields are passed as backend search options |
| `/api/chat` | `query`, `repos`, `ref`, `languageModel`, `answerBudget`, optional context/session fields |
| `/api/models` `PUT` | Replace-all `{ "models": [...] }`; preserve existing models client-side when editing one |
| `/api/secrets` `PUT` | `{ "key": "...", "value": "..." }` |

### 1. Create or Update a Workspace

```bash
curl -sS -X POST http://127.0.0.1:3000/api/atom/workspaces \
  -H 'Content-Type: application/json' \
  -H 'X-Codeintel-Atom-Token: dev-atom-token' \
  -d '{
    "workspaceId": "acme-platform",
    "workspaceName": "Acme Platform",
    "domain": "acme-platform",
    "createApiKey": true,
    "apiKeyName": "local-dev"
  }'
```

Response shape:

```json
{
  "tenant": {
    "id": 1,
    "name": "Acme Platform",
    "domain": "acme-platform",
    "atomWorkspaceId": "acme-platform",
    "atomWorkspaceName": "Acme Platform"
  },
  "apiKey": "cik_..."
}
```

### 2. Verify Tenant Metadata

```bash
curl -sS http://127.0.0.1:3000/api/tenants/acme-platform/metadata \
  -H 'X-Api-Key: cik_...'
```

### 3. Store an LLM Secret

```bash
curl -sS -X PUT http://127.0.0.1:3000/api/secrets \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "key": "glm-api-key",
    "value": "replace-with-your-token"
  }'
```

Secrets are stored in PostgreSQL under the organization and are referred
to by `secretRef`. The model configuration never needs to expose the raw
token again.

### 4. Configure a Language Model

`PUT /api/models` is replace-all: always send the full desired model
set for the workspace.

```bash
curl -sS -X PUT http://127.0.0.1:3000/api/models \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "models": [
      {
        "provider": "openai-compatible",
        "model": "glm-5",
        "displayName": "GLM 5",
        "baseUrl": "https://api.z.ai/api/coding/paas/v4",
        "apiKey": {
          "secretRef": "glm-api-key"
        }
      }
    ]
  }'
```

### 5. Create a VCS Connection

Generic Git connection records are available today. App-based
GitHub/GitLab/Bitbucket OAuth is a planned API surface and is not faked.

```bash
curl -sS -X POST http://127.0.0.1:3000/api/connections \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "name": "acme-github",
    "connectionType": "github",
    "config": {
      "type": "github",
      "token": {
        "secretRef": "github-token"
      }
    },
    "sync": true
  }'
```

Useful connection APIs:

```text
GET    /api/connections
POST   /api/connections
PATCH  /api/connections/{id}
DELETE /api/connections/{id}
POST   /api/connections/{id}/sync
GET    /api/connections/{id}/status
GET    /api/connections/{id}/branches
PUT    /api/connections/{id}/branches
```

### 6. List Repositories

```bash
curl -sS 'http://127.0.0.1:3000/api/repos?query=otel&page=1&perPage=50&sort=name&direction=asc' \
  -H 'X-Api-Key: cik_...'
```

The response includes active index state, branch status, graph health,
and pagination through the `X-Total-Count` header.

### 7. Select Branches

```bash
curl -sS -X PUT http://127.0.0.1:3000/api/repos/42/branches \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "defaultBranch": "main",
    "selectedBranches": ["main", "release-1.0"]
  }'
```

### 8. Index or Reindex a Branch

```bash
curl -sS -X POST http://127.0.0.1:3000/api/repos/42/index \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{"branch":"main"}'
```

Response:

```json
{"jobId":"..."}
```

Poll:

```bash
curl -sS http://127.0.0.1:3000/api/repos/42/status \
  -H 'X-Api-Key: cik_...'
```

### 9. Remove a Branch Index

This removes Codeintel's local/indexed artifacts for that repository
branch. It does not modify the remote provider repository.

```bash
curl -sS -X DELETE 'http://127.0.0.1:3000/api/repos/42/index?ref=refs/heads/main' \
  -H 'X-Api-Key: cik_...'
```

### 10. Search

```bash
curl -sS -X POST http://127.0.0.1:3000/api/search \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "query": "repo:github.com/acme/api branch:main trace exporter"
  }'
```

### 11. Chat

Asynchronous chat:

```bash
curl -sS -X POST http://127.0.0.1:3000/api/chat \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "query": "Trace the checkout flow across API, worker, and database layers. Include exact files, functions, call paths, tests, and a harness plan.",
    "repos": ["github.com/acme/api", "github.com/acme/worker"],
    "ref": "main",
    "languageModel": {
      "provider": "openai-compatible",
      "model": "glm-5"
    },
    "answerBudget": "full"
  }'
```

Poll:

```bash
curl -sS 'http://127.0.0.1:3000/api/chat/<chat-id>/result' \
  -H 'X-Api-Key: cik_...'
```

Blocking chat is also exposed at:

```text
POST /api/chat/blocking
```

Use the blocking endpoint for manual validation and small requests. Use
the async endpoint for large coding-harness answers.

## MCP

The MCP endpoint is:

```text
POST /api/{workspace-domain}/mcp
GET  /api/{workspace-domain}/mcp
```

Example tool list:

```bash
curl -sS -X POST http://127.0.0.1:3000/api/acme-platform/mcp \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/list"
  }'
```

MCP tool availability is configuration-aware:

- Database-backed tools require PostgreSQL queries.
- `grep` requires a configured Zoekt search backend.
- SCIP symbol tools require PostgreSQL plus Zoekt.
- Graph tools require PostgreSQL plus NebulaGraph.
- `codegraph_context` requires PostgreSQL, Zoekt, and NebulaGraph.
- `ask_codebase` requires the fused retrieval stack plus a configured LLM
  gateway and org language model.
- `get_ask_codebase_result` is advertised when the LLM gateway supports
  durable async requests.

Full MCP tool reference:

| Tool | Required args | Optional args | Purpose |
| --- | --- | --- | --- |
| `list_repos` | none | `query`, `page`, `perPage`, `sort=name\|indexedAt\|pushed`, `direction=asc\|desc` | List tenant-scoped repositories |
| `list_language_models` | none | none | List configured org language models |
| `read_file` | `repo`, `path` | `ref`, `offset`, `limit` | Read a file from an indexed checkout |
| `list_tree` | `repo` | `path`, `ref`, `depth`, `includeFiles`, `includeDirectories`, `maxEntries` | Browse an indexed repo tree |
| `grep` | `pattern` | `repo`, `repos`, `ref`, `path`, `include`, `limit`, `groupByRepo` | Zoekt-backed broad text/regex recall |
| `find_symbol_definitions` | `symbol` | `repo`, `repos`, `revision`, `definitionFile`, `limit` | SCIP-backed definitions fused with Zoekt recall |
| `find_symbol_references` | `symbol` | `repo`, `repos`, `revision`, `definitionFile`, `limit` | SCIP-backed references fused with Zoekt recall |
| `inspect_code_graph` | `query` | `repo`, `repos`, `ref`, `depth`, `limit` | Detailed Postgres graph metadata plus NebulaGraph traversal |
| `codegraph_context` | `query` | `repo`, `repos`, `ref`, `depth`, `limit`, `compact` | Fused Zoekt + SCIP + AST/tree-sitter + graph evidence pack |
| `graph_callers` | `query` or `symbol` or `seed` | `repo`, `repos`, `ref`, `depth`, `limit` | Incoming caller/reference graph context |
| `graph_callees` | `query` or `symbol` or `seed` | `repo`, `repos`, `ref`, `depth`, `limit` | Outgoing callee/dependency graph context |
| `graph_impact` | `query` or `symbol` or `seed` | `repo`, `repos`, `ref`, `depth`, `limit` | Blast-radius and impacted files/services/tests |
| `graph_path` | `query` or `symbol` or `seed` | `repo`, `repos`, `ref`, `depth`, `limit` | Implementation path or sequence between code entities |
| `graph_minimal_context` | `query` or `symbol` or `seed` | `repo`, `repos`, `ref`, `depth`, `limit` | Compact graph-backed development context |
| `graph_status` | none | `repo`, `repos`, `ref`, `limit` | Active graph snapshot coverage and health |
| `compare_branches` | `repo` plus `headRef` or `headRefs` | `baseRef`, `includeDiff`, `maxFiles`, `maxPatchBytes` | Branch/revision comparison with bounded diff output |
| `ask_codebase` | `query` | `repos`, `ref`, `languageModel`, `async` | Development-grade synthesized answer from fused retrieval |
| `get_ask_codebase_result` | `requestId` | none | Poll durable async `ask_codebase` result |

MCP branch/ref rules:

- `ref` is optional for most tools; omitted ref resolves to the repo's
  configured default indexed branch.
- Multi-repo calls can pass `repos`; each repo uses the explicit `ref` or
  its own default branch.
- Symbol tools accept `revision` for SCIP compatibility; it maps to the
  same branch/ref concept used by `ref`.
- `compare_branches` works over materialized Git refs and can return a
  raw diff even when semantic indexing is not active for the compared
  branch. Semantic Q&A still requires active index coverage.
- Tools never cross organization boundaries; same-name repos in different
  workspaces are resolved by the authenticated org.

Example `codegraph_context` call:

```bash
curl -sS -X POST http://127.0.0.1:3000/api/acme-platform/mcp \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "jsonrpc": "2.0",
    "id": 2,
    "method": "tools/call",
    "params": {
      "name": "codegraph_context",
      "arguments": {
        "query": "Trace checkout order creation from HTTP route to worker event and database write",
        "repos": ["github.com/acme/api", "github.com/acme/worker"],
        "ref": "main",
        "compact": true,
        "limit": 40
      }
    }
  }'
```

Example `ask_codebase` call:

```bash
curl -sS -X POST http://127.0.0.1:3000/api/acme-platform/mcp \
  -H 'Content-Type: application/json' \
  -H 'X-Api-Key: cik_...' \
  -d '{
    "jsonrpc": "2.0",
    "id": 3,
    "method": "tools/call",
    "params": {
      "name": "ask_codebase",
      "arguments": {
        "query": "Explain the checkout architecture across repos. Include HLD, LLD, sequence, exact files, functions, tests, risks, and a coding harness file map.",
        "repos": ["github.com/acme/api", "github.com/acme/worker"],
        "ref": "main",
        "languageModel": {
          "provider": "openai-compatible",
          "model": "glm-5"
        },
        "answerBudget": "full"
      }
    }
  }'
```

## Indexing Model

Indexing is intentionally internal to the product flow. A user selects
repos and branches; Codeintel decides which layers to run.

For each selected repo/ref, the system may produce:

- Git worktree materialization.
- Zoekt shard artifacts for lexical search.
- SCIP indexes for definitions/references/type relationships.
- AST/tree-sitter facts for framework and structural code evidence.
- Graph snapshots and graph write jobs.
- PostgreSQL manifests for status, auditability, and delta planning.
- NebulaGraph vertices and edges for traversal.

The logical indexing key is:

```text
org_id + repo_id + ref
```

That key is carried through every layer:

| Layer | Stored in | Scope |
| --- | --- | --- |
| Worktree | Shared artifact storage | `org/repo/ref/commit` |
| Zoekt | Zoekt shard directory | `org/repo/ref` |
| SCIP | PostgreSQL symbol and occurrence tables | `org/repo/ref/project-root/language` |
| AST/tree-sitter | PostgreSQL facts and graph artifacts | `org/repo/ref/file/language` |
| Graph snapshot | PostgreSQL manifest plus NebulaGraph | `org/repo/ref/snapshot` |
| Search/chat/MCP | Runtime scope resolver | selected repos plus explicit or default ref |

Reindexing is idempotent and branch-aware. The planner compares the
selected ref, observed commit, and previous manifest state before
scheduling layers. Remove-index retires the active branch/ref index and
prevents read/search/MCP tools from serving that ref while cleanup
proceeds.

## Language and Worker Classes

The indexer supports a worker-class model so app/API replicas do not
carry large language toolchains. Example classes live in:

```text
deploy/worker-classes.example.yaml
```

Typical production split:

| Worker class | Typical languages | Operating mode |
| --- | --- | --- |
| `core` | Git, Zoekt, AST/tree-sitter orchestration | hot pool |
| `scip-ts-python` | TypeScript/JavaScript, Python | hot pool |
| `scip-go` | Go | hot pool |
| `scip-jvm` | Java/Gradle/Maven/SBT | cold job or heavy pool |
| `scip-dotnet` | C#, VB, F# | cold job or heavy pool |
| `scip-native` | C/C++, Rust, Dart, Ruby | mixed, based on tenant workload |

The universal all-language worker is useful for development but is not
recommended for large API fleets because it increases image size and
memory pressure.

## Storage Model

Durable control-plane state belongs in PostgreSQL. Graph state belongs
in NebulaGraph. Redis is queue/cache state. Large indexing artifacts
belong on shared filesystem/object storage.

Recommended Kubernetes storage shape:

```text
/codeintel/data/                 # git worktrees and materialized refs
/codeintel/artifacts/            # SCIP, AST/tree-sitter, graph artifact handoff
/codeintel/zoekt/<org-id>/index  # tenant-scoped Zoekt shard layout
```

For EKS, an EFS-backed RWX volume is suitable for shared artifact paths
and tenant-scoped Zoekt directories. High-throughput deployments should
consider local hot cache plus durable shared storage for cold artifacts.

## Configuration

Common `codeintel-app` environment variables:

| Variable | Required | Description |
| --- | --- | --- |
| `CODEINTEL_LISTEN_ADDR` | no | HTTP listen address, default `0.0.0.0:3000` |
| `CODEINTEL_DATABASE_URL` | yes | PostgreSQL DSN |
| `CODEINTEL_ENCRYPTION_KEY` | yes | Secret hashing/encryption key |
| `CODEINTEL_AUTO_MIGRATE` | no | Apply embedded migrations on boot when `true` |
| `CODEINTEL_REDIS_URL` | no | Redis URL for graph/cache support |
| `CODEINTEL_BACKEND_GRPC_ADDR` | no | Backend gRPC address for audit |
| `CODEINTEL_BACKEND_CONTROL_URL` | no | Backend scheduler HTTP URL |
| `CODEINTEL_BACKEND_CONTROL_TOKEN` | no | Token for backend scheduler URL |
| `CODEINTEL_LLM_GATEWAY_URL` | no | Backend LLM gateway URL |
| `CODEINTEL_BACKEND_LLM_TOKEN` | no | Token for backend LLM gateway |
| `CODEINTEL_ATOM_CONTROL_PLANE_TOKEN` | no | Token for workspace provisioning |
| `CODEINTEL_ZOEKT_GRPC_ENDPOINTS` | no | Comma-separated Zoekt endpoints |
| `CODEINTEL_ZOEKT_REPLICATED` | no | Treat Zoekt endpoints as replicas |
| `CODEINTEL_NEBULA_ADDR` | no | NebulaGraph graphd address |
| `CODEINTEL_NEBULA_USER` | no | Nebula user |
| `CODEINTEL_NEBULA_PASSWORD` | no | Nebula password |
| `CODEINTEL_NEBULA_SPACE` | no | Nebula space name |
| `CODEINTEL_DATA_CACHE_DIR` | no | Local/shared repo materialization root |
| `CODEINTEL_ZOEKT_EFS_ROOT` | no | Shared Zoekt root |
| `CODEINTEL_RATE_LIMIT_RPS` | no | API rate limit |
| `CODEINTEL_RATE_LIMIT_BURST` | no | API rate-limit burst |

Common `codeintel-backend` environment variables:

| Variable | Required | Description |
| --- | --- | --- |
| `CODEINTEL_DATABASE_URL` | yes | PostgreSQL DSN |
| `CODEINTEL_REDIS_URL` | yes | Redis URL for asynq |
| `CODEINTEL_BACKEND_GRPC_ADDR` | no | gRPC listen address, default `127.0.0.1:3101` |
| `CODEINTEL_BACKEND_QUEUE_MODE` | no | `control`, `executor`, or `all` |
| `CODEINTEL_BACKEND_CONTROL_ADDR` | no | Internal scheduler HTTP listen address |
| `CODEINTEL_BACKEND_CONTROL_TOKEN` | no | Scheduler bearer token |
| `CODEINTEL_BACKEND_LLM_ADDR` | no | Internal LLM gateway listen address |
| `CODEINTEL_BACKEND_LLM_TOKEN` | no | LLM gateway bearer token |
| `CODEINTEL_BACKEND_LLM_TIMEOUT_SECONDS` | no | LLM request timeout |
| `CODEINTEL_BACKEND_ASYNQ_CONCURRENCY` | no | Worker concurrency |
| `CODEINTEL_INDEX_SUBJOB_DISPATCH_ENABLED` | no | Dispatch split index subjobs |
| `CODEINTEL_INDEX_EXECUTOR_CONSUMERS_ENABLED` | no | Consume executor class queues |
| `CODEINTEL_INDEX_EXECUTOR_CLASSES` | no | Comma-separated worker classes |
| `CODEINTEL_INDEX_ARTIFACT_ROOT` | no | Artifact handoff root |
| `CODEINTEL_NEBULA_ADDR` | no | Enable graph writes |

Redis production safety:

```bash
CODEINTEL_REDIS_REQUIRE_BOUNDED_MEMORY=true
CODEINTEL_REDIS_ALLOWED_EVICTION_POLICIES=allkeys-lru,allkeys-lfu,volatile-lru,volatile-lfu
```

## Tests

Run Go tests:

```bash
cd codeintel
go test ./...
```

Run Rust tests:

```bash
cd codeintel
cargo test --manifest-path indexer-rs/Cargo.toml --workspace
```

Run infrastructure health checks:

```bash
cd codeintel
make stack-healthcheck
```

Run the product gate:

```bash
cd codeintel
make product-gate
```

The product gate is expected to validate real workspace, LLM, VCS,
repo/branch, index/reindex/remove-index, search, MCP, and chat paths.
Some deployments require live secrets and Kubernetes/Docker endpoints to
be supplied before the gate can run.

## Scaling and Operations

Codeintel is designed for horizontal scaling:

- `codeintel-app` is stateless. Any replica can serve any tenant-scoped
  API/MCP/chat read request.
- `codeintel-backend` consumes Redis/asynq queues with retry-safe,
  idempotent jobs.
- `codeintel-indexer-rs` workers are split by worker class so expensive
  language toolchains scale independently from API pods.
- PostgreSQL is the source of truth for tenant, repo, branch, manifest,
  and job state.
- NebulaGraph stores graph data and can be scaled independently.
- Shared artifact storage survives pod and node replacement.
- Redis is not a durable source of code intelligence; it is cache and
  queue state and must be memory bounded.

Production recommendations:

- Run app, backend, and indexer workers as separate deployments.
- Use separate node pools for API, control workers, and heavy language
  indexers.
- Use spot nodes for retry-safe index workers, with graceful termination
  and sufficient job lease/heartbeat settings.
- Keep API images free of language toolchains.
- Put index artifacts and Zoekt shard storage on shared durable storage.
- Use HPA/KEDA against HTTP latency, queue depth, and worker utilization.
- Set database pool budgets per deployment so autoscaling does not
  overwhelm PostgreSQL.
- Enable Redis maxmemory and eviction policy checks.

## Security and Multi-Tenancy

Security boundaries in the current implementation:

- API keys are workspace/organization scoped.
- Tenant domain metadata is verified against the authenticated API key.
- Repo, branch, search, graph, chat, and MCP calls carry org scope.
- Secrets are stored by organization and referenced by key, not returned
  as plaintext by model list APIs.
- Mutating repo index operations require owner role.
- Admin console is a verification client over the public API; it is not
  a privileged backdoor.

Before public production use, review:

- External provider OAuth implementation and token refresh ownership.
- Secret encryption/key rotation policy.
- PostgreSQL backups and point-in-time recovery.
- NebulaGraph backups.
- Shared artifact retention and deletion policy.
- Audit/event retention.
- Network policies between app, backend, indexers, Postgres, Redis,
  NebulaGraph, and Zoekt.

## Known Gaps

These are known product gaps, not hidden footnotes:

- App-based GitHub/GitLab/Bitbucket OAuth APIs are planned but not yet
  complete. Generic connection records work; provider cards in the admin
  UI are intentionally disabled rather than fake.
- Production Helm/EKS packaging is still being refined.
- Some graph responses can still expose low-level symbol identifiers
  where higher-quality anchor labels should be materialized.
- Cold graph evidence for large multi-repo questions is still being
  optimized. Warm/repeated compact evidence is cached with tenant/snapshot
  invalidation.
- Full language coverage depends on which SCIP worker classes are built
  and deployed.
- Provider-specific repo pagination and very large tenant UX are still
  evolving.

## Contributing

Contributions are welcome, but code changes should preserve the product
contracts:

- No tenant-unsafe reads or writes.
- No fake success paths.
- No hidden singleton assumptions.
- No API replicas carrying indexer language toolchains.
- No queue path that loses work on pod crash.
- Tests must cover public behavior and failure modes.

Suggested contribution flow:

1. Open an issue describing the product behavior, scale concern, or bug.
2. Add or update tests that demonstrate the behavior.
3. Keep changes scoped to one slice.
4. Run Go/Rust tests and the relevant local product gate.
5. Include operational impact in the pull request: latency, memory,
   storage, migration, and tenant-safety notes.

## License

Codeintel is released under the MIT License. See [LICENSE](LICENSE).
