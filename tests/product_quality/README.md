# Enterprise Product Quality Gate

This directory is the non-negotiable product-quality harness for the
codeintel port. It exists because static unit parity is not enough for an
agentic code-intelligence product.

The gate is intentionally built around ten related real repositories from the
OpenTelemetry ecosystem. They provide a cross-repo, polyglot scenario where a
good answer must connect instrumentation SDKs, OTLP payloads, collector
pipelines, Kubernetes operator configuration, exporters, examples, tests, and
runtime manifests.

Required retrieval layers:

- Zoekt for broad lexical recall.
- SCIP for precise symbol definitions and references.
- AST and tree-sitter extraction for framework/runtime facts.
- Graph traversal for cross-repo flow, impact, anchors, and architecture paths.
- MCP for the final developer-facing tool response.

Run the contract-only gate:

```sh
go test -count=1 ./tests/product_quality
```

Run the full live product gate:

```sh
CODEINTEL_PRODUCT_BASE_URL=http://localhost:3000 \
CODEINTEL_PRODUCT_NAMESPACE=your-kind-namespace \
CODEINTEL_PRODUCT_LIFECYCLE_CMD='node ../deploy/kind/lifecycle-smoke.mjs' \
./tests/product_quality/run_real_repos.sh
```

The lifecycle command may use pre-supplied `CODEINTEL_PRODUCT_API_KEY` and
`CODEINTEL_PRODUCT_ORG_DOMAIN`, or it may provision them dynamically and write:

```sh
CODEINTEL_PRODUCT_API_KEY=cik_...
CODEINTEL_PRODUCT_ORG_DOMAIN=created-org
```

to `CODEINTEL_PRODUCT_LIFECYCLE_ENV`. This keeps the gate aligned with the
Atom-controlled product flow where orgs, credentials, repositories, branches,
and LLM config are created behind the scenes before search/MCP validation.

The full gate is expected to fail until the port has real search and MCP
backends, layered retrieval execution, and answer-quality scoring wired. That
failure is useful: it prevents us from calling the port enterprise-ready before
it can answer real multi-repo architecture questions with exact files, symbols,
flows, diagrams, tests, and confidence.

The live gate is intentionally not a discovery-only preflight. It must run a
lifecycle command that writes `CODEINTEL_PRODUCT_LIFECYCLE_REPORT`; the
`realrepo` tests then verify the report contains Atom tenant creation,
repository sync/index/remove lifecycle, MCP, `ask_codebase`, saved
Zoekt-only / Zoekt+SCIP / full-fusion answer artifacts, and query-only
multi-round chat continuity proof before checking the ten related repositories.

Set `CODEINTEL_PRODUCT_CHAT_ROUNDS` to reduce local debug cost. Product-gate
mode defaults to 10 rounds because the acceptance contract must prove that an
agent harness can send follow-up questions by chat id without resubmitting the
full prior transcript.
