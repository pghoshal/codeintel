# codeintel

`codeintel` is a Go HTTP service that exposes an organisation's
search, code-intelligence, and connection-management surface. A
Rust workspace handles index/extraction workers; the two
components communicate over a gRPC contract.

## Layout

```
codeintel/
├── go.mod                       # module codeintel
├── cmd/
│   ├── codeintel-app/           # HTTP API + MCP gateway
│   └── codeintel-backend/       # LLM / semantic / indexer-coordinator (later)
├── internal/
│   ├── analytics/               # product-telemetry emitter abstraction
│   ├── api/                     # HTTP handlers
│   ├── audit/                   # audit-event emitter abstraction
│   ├── auth/                    # API-key resolution, encryption helpers
│   ├── db/                      # pgx pool, typed queries, migration runner
│   ├── migrate/                 # embedded SQL migrations
│   ├── obs/                     # logging, metrics, rate-limit, CORS
│   └── secretrefs/              # secret-reference walker
├── indexer-rs/                  # Rust workspace (later)
├── proto/                       # gRPC IDL between Go and Rust (later)
└── tests/
    ├── integration/             # cross-package integration tests
    └── parity/                  # wire-format byte-equality tests
        ├── golden/              # frozen JSON fixtures
        └── *_test.go
```

## Modular extension points

Several cross-cutting concerns are pluggable interfaces in the
`api.Config` struct so deployments can wire real backends without
recompiling handlers:

- `AuditEmitter` (`internal/audit`) — receives every audit event
  the mutating handlers fire. Defaults to a noop.
- `AnalyticsEmitter` (`internal/analytics`) — receives product-
  telemetry events. Defaults to a noop.
- `ConnectionSyncer` (`internal/api`) — schedules a sync after a
  successful `POST /api/connections`. Defaults to a noop.
- `RateLimiter` / `CORS` / `Metrics` (`internal/obs`) — observability
  middleware, all optional with sensible no-op fallbacks.

## Local dev stack

Every codeintel binary depends on Postgres + Redis + Nebula.
The `Makefile` brings them up + tears them down + gates their
health as a single command.

```bash
cd codeintel
make stack-up          # postgres + redis + nebula; blocks on healthcheck gate
make stack-status      # show docker compose ps
make stack-healthcheck # real-client probe (pg_isready / redis ping / nebula SHOW HOSTS)
make stack-down        # graceful shutdown; data preserved
make stack-down-volumes  # full wipe (DESTRUCTIVE; prompts)
```

Service endpoints (all bound to 127.0.0.1; non-conflicting with
the legacy `docker-compose-dev.yml` ports so both can coexist):

| Service | Address | Credentials |
|---|---|---|
| Postgres | 127.0.0.1:5433 | user `codeintel` / password `codeintel` / db `codeintel` |
| Redis (asynq backend) | 127.0.0.1:6380 | (none) |
| Nebula graphd | 127.0.0.1:9669 | user `root` / password `nebula` |
| Nebula metad | 127.0.0.1:9559 | (admin-only; not directly used) |
| Nebula storaged | 127.0.0.1:9779 | (admin-only; not directly used) |

## Running

```bash
cd codeintel
make stack-up
go build ./cmd/codeintel-app
CODEINTEL_LISTEN_ADDR=127.0.0.1:3001 \
  CODEINTEL_DATABASE_URL='postgresql://codeintel:codeintel@127.0.0.1:5433/codeintel' \
  ./codeintel-app
curl http://127.0.0.1:3001/api/health
# {"status":"ok"}
```

## Redis operations policy

Redis is cache and queue state only. It is not a durable store for code,
SCIP, AST, graph, LLM answers, or tenant configuration; those belong in
Postgres, NebulaGraph, Zoekt/EFS artifacts, and index manifests.

Production Redis must be memory bounded. Set `maxmemory` at the Redis
server or managed-service level, and use an eviction policy that can shed
cache keys under pressure. The production defaults accepted by codeintel
are:

```text
allkeys-lru, allkeys-lfu, volatile-lru, volatile-lfu
```

Set these environment variables on `codeintel-backend` and, when Redis is
enabled for app-side cache/producers, on `codeintel-app`:

```bash
CODEINTEL_REDIS_REQUIRE_BOUNDED_MEMORY=true
CODEINTEL_REDIS_ALLOWED_EVICTION_POLICIES=allkeys-lru,allkeys-lfu,volatile-lru,volatile-lfu
```

`codeintel-backend` fails startup when the bounded-memory requirement is
enabled and Redis reports `maxmemory=0` or an unsafe eviction policy such
as `noeviction`. `codeintel-app` logs the same Redis memory/keyspace
snapshot but keeps cache/producers best-effort.

Monitor Redis with a Redis exporter or managed-service metrics for:

- `used_memory / maxmemory`
- eviction policy and eviction count
- key count per DB
- asynq queue depth, retry depth, dead-task depth, and oldest pending age
- command latency and blocked clients

## Tests

```bash
go test ./...
```

Wire-format parity tests live under `tests/parity` and assert
byte-equality of HTTP response bodies against the golden fixtures
in `tests/parity/golden`. Every public endpoint MUST be covered.
Cross-package integration tests (DB pool, migration runner, etc.)
live under `tests/integration`.
