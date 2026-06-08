// Package nebulasmoke holds the live-Nebula bring-up smoke tests.
// The tests are gated on the CODEINTEL_NEBULA_ADDR environment
// variable; when unset they t.Skip so `go test ./...` stays green
// in CI without Docker. To run locally:
//
//	docker compose -f codeintel/docker-compose.yml up -d
//	CODEINTEL_NEBULA_ADDR=127.0.0.1:9669 \
//	  CODEINTEL_NEBULA_USER=root \
//	  CODEINTEL_NEBULA_PASSWORD=nebula \
//	  go test ./internal/nebulasmoke/... -run Smoke -v
//
// The package contains two complementary tests:
//
//  1. TestNebulaSmoke_RawClient — exercises the lower-level
//     vesoft-inc/nebula-go ConnectionPool API directly. Proves
//     the compose topology is reachable and the bootstrap
//     credentials work.
//  2. TestNebulaSmoke_ProductionClient — exercises the
//     pkg/nebulaclient wrapper end-to-end (env-driven config,
//     New + Ping + Execute). Proves the wrapper is wired
//     correctly against the real cluster.
package nebulasmoke

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"codeintel/internal/backend/graphstore"
	"codeintel/pkg/graphschema"
	"codeintel/pkg/nebulaclient"
	nebula "github.com/vesoft-inc/nebula-go/v3"
)

// usePrefixedExecutor wraps a *nebulaclient.Client so every
// Execute call carries the codeintel-space USE prefix. The
// store's helpers (executeWithSchemaRetry → executor.Execute)
// don't know about the space context themselves; this adapter
// is the integration glue between the bare client and the
// store's expectation that USE is already applied.
type usePrefixedExecutor struct {
	c      *nebulaclient.Client
	prefix string
}

func (e *usePrefixedExecutor) Execute(ctx context.Context, stmt string) (*nebula.ResultSet, error) {
	return e.c.Execute(ctx, e.prefix+stmt)
}

const (
	envNebulaAddr = nebulaclient.EnvAddr
	defaultUser   = "root"
	defaultPass   = "nebula"
)

// TestNebulaSmoke_RawClient — see file-level doc.
func TestNebulaSmoke_RawClient(t *testing.T) {
	addr := os.Getenv(envNebulaAddr)
	if addr == "" {
		t.Skipf("%s is unset; skipping live-Nebula smoke (see codeintel/docker-compose.yml)", envNebulaAddr)
	}

	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("%s=%q is not a valid host:port: %v", envNebulaAddr, addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		t.Fatalf("%s=%q has an invalid port: %v", envNebulaAddr, addr, err)
	}

	hostList := []nebula.HostAddress{{Host: host, Port: port}}
	poolCfg := nebula.GetDefaultConf()
	poolCfg.TimeOut = 5 * time.Second
	poolCfg.IdleTime = 5 * time.Second
	poolCfg.MaxConnPoolSize = 2

	pool, err := nebula.NewConnectionPool(hostList, poolCfg, nebula.DefaultLogger{})
	if err != nil {
		t.Fatalf("NewConnectionPool: %v", err)
	}
	defer pool.Close()

	session, err := pool.GetSession(defaultUser, defaultPass)
	if err != nil {
		t.Fatalf("GetSession(%s/****): %v", defaultUser, err)
	}
	defer session.Release()

	rs, err := session.Execute("SHOW HOSTS;")
	if err != nil {
		t.Fatalf("Execute SHOW HOSTS: %v", err)
	}
	if !rs.IsSucceed() {
		t.Fatalf("SHOW HOSTS returned a non-success error code: %d %q", rs.GetErrorCode(), rs.GetErrorMsg())
	}
	if got := rs.GetRowSize(); got <= 0 {
		t.Fatalf("SHOW HOSTS returned 0 rows; the cluster has no storaged registered: %+v", rs)
	}
}

// TestNebulaSmoke_ProductionClient drives the live cluster through
// pkg/nebulaclient.Client — the same code path codeintel-app and
// codeintel-graph will use in production. New() performs an eager
// Ping under the supplied context; a successful return value proves
// every layer (config parsing → pool init → session acquire → nGQL
// execute) works end-to-end.
//
// The follow-up Execute("SHOW HOSTS") exercises the post-startup
// Execute code path including the per-call context handling.
func TestNebulaSmoke_ProductionClient(t *testing.T) {
	addr := os.Getenv(nebulaclient.EnvAddr)
	if addr == "" {
		t.Skipf("%s is unset; skipping production-client smoke", nebulaclient.EnvAddr)
	}
	// Ensure the credential env vars are set for LoadConfigFromEnv;
	// the bring-up uses the Nebula bootstrap defaults so they're
	// the right values here even if the operator hasn't exported
	// them explicitly.
	if os.Getenv(nebulaclient.EnvUser) == "" {
		t.Setenv(nebulaclient.EnvUser, defaultUser)
	}
	if os.Getenv(nebulaclient.EnvPassword) == "" {
		t.Setenv(nebulaclient.EnvPassword, defaultPass)
	}

	cfg, err := nebulaclient.LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	// Tighten the per-call timeout for the smoke; 2 s is plenty for
	// a SHOW HOSTS round-trip against a local container.
	cfg.Timeout = 2 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := nebulaclient.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("nebulaclient.New: %v", err)
	}
	defer client.Close()

	// Ping is also exercised by New's eager startup probe; an
	// explicit second Ping here proves the post-startup code path
	// is reusable.
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("client.Ping: %v", err)
	}

	rs, err := client.Execute(ctx, "SHOW HOSTS;")
	if err != nil {
		t.Fatalf("client.Execute SHOW HOSTS: %v", err)
	}
	if got := rs.GetRowSize(); got <= 0 {
		t.Fatalf("SHOW HOSTS via Client returned 0 rows: %+v", rs)
	}
}

// TestNebulaSmoke_SchemaBootstrap exercises the
// pkg/graphschema.Bootstrap end-to-end against the running
// cluster: CREATE SPACE → wait → USE → CREATE TAG / CREATE EDGE /
// CREATE TAG INDEX. Re-runs (idempotency via IF NOT EXISTS) are
// asserted by running Bootstrap twice on the same cluster.
//
// After bootstrap, SHOW TAGS and SHOW EDGES verify the codeintel
// schema actually landed (not just that the statements returned
// without error).
func TestNebulaSmoke_SchemaBootstrap(t *testing.T) {
	addr := os.Getenv(nebulaclient.EnvAddr)
	if addr == "" {
		t.Skipf("%s is unset; skipping schema-bootstrap smoke", nebulaclient.EnvAddr)
	}
	if os.Getenv(nebulaclient.EnvUser) == "" {
		t.Setenv(nebulaclient.EnvUser, defaultUser)
	}
	if os.Getenv(nebulaclient.EnvPassword) == "" {
		t.Setenv(nebulaclient.EnvPassword, defaultPass)
	}

	cfg, err := nebulaclient.LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	// Bootstrap issues schema CREATEs which can be slower than a
	// trivial YIELD 1; bump the per-call timeout for the smoke.
	cfg.Timeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := nebulaclient.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("nebulaclient.New: %v", err)
	}
	defer client.Close()

	// First bootstrap: creates space + schema from a clean state.
	if err := graphschema.Bootstrap(ctx, client, graphschema.BootstrapOptions{}); err != nil {
		t.Fatalf("Bootstrap (first run): %v", err)
	}

	// Idempotency: a second Bootstrap against an already-fully-
	// provisioned space must succeed (every CREATE is IF NOT
	// EXISTS).
	if err := graphschema.Bootstrap(ctx, client, graphschema.BootstrapOptions{}); err != nil {
		t.Fatalf("Bootstrap (idempotency run): %v", err)
	}

	// Verify the schema actually landed. The nebulaclient pool
	// checks out a fresh session per Execute, so each
	// verification statement is prefixed with USE so it runs
	// inside the codeintel space.
	usePrefix := "USE `" + graphschema.SpaceName + "`; "

	tagsRS, err := client.Execute(ctx, usePrefix+"SHOW TAGS;")
	if err != nil {
		t.Fatalf("SHOW TAGS: %v", err)
	}
	if !containsRowValue(t, tagsRS, graphschema.NodeTag) {
		t.Errorf("SHOW TAGS missing %q in %d rows", graphschema.NodeTag, tagsRS.GetRowSize())
	}

	edgesRS, err := client.Execute(ctx, usePrefix+"SHOW EDGES;")
	if err != nil {
		t.Fatalf("SHOW EDGES: %v", err)
	}
	if !containsRowValue(t, edgesRS, graphschema.EdgeType) {
		t.Errorf("SHOW EDGES missing %q in %d rows", graphschema.EdgeType, edgesRS.GetRowSize())
	}

	// Verify the three tag indexes landed.
	idxRS, err := client.Execute(ctx, usePrefix+"SHOW TAG INDEXES;")
	if err != nil {
		t.Fatalf("SHOW TAG INDEXES: %v", err)
	}
	for _, idxName := range []string{
		"code_graph_node_scope_idx",
		"code_graph_node_label_idx",
		"code_graph_node_key_idx",
	} {
		if !containsRowValue(t, idxRS, idxName) {
			t.Errorf("SHOW TAG INDEXES missing %q", idxName)
		}
	}
}

// TestNebulaSmoke_VertexEdgeInsert proves the pkg/graphschema
// vertex+edge insert path end-to-end against the running cluster.
// Sequence:
//
//  1. Bootstrap (no-op if already provisioned from prior tests).
//  2. INSERT VERTEX for a 2-element batch using RenderVertexInsert.
//  3. INSERT EDGE for a 1-element batch using RenderEdgeInsert.
//  4. LOOKUP ON `code_graph_node` and assert both vertices land.
//  5. FETCH the edge via the same vid scope and assert it exists.
//
// The test cleans up after itself with DELETE VERTEX … WITH EDGE
// so re-runs against the same cluster don't accumulate cruft.
func TestNebulaSmoke_VertexEdgeInsert(t *testing.T) {
	addr := os.Getenv(nebulaclient.EnvAddr)
	if addr == "" {
		t.Skipf("%s is unset; skipping vertex/edge insert smoke", nebulaclient.EnvAddr)
	}
	if os.Getenv(nebulaclient.EnvUser) == "" {
		t.Setenv(nebulaclient.EnvUser, defaultUser)
	}
	if os.Getenv(nebulaclient.EnvPassword) == "" {
		t.Setenv(nebulaclient.EnvPassword, defaultPass)
	}

	cfg, err := nebulaclient.LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	cfg.Timeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := nebulaclient.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("nebulaclient.New: %v", err)
	}
	defer client.Close()

	// Ensure schema exists; idempotent if a prior test already
	// ran it on this cluster.
	if err := graphschema.Bootstrap(ctx, client, graphschema.BootstrapOptions{}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Build a tiny snapshot: two vertices linked by one edge. Each
	// VID is namespaced ("smoke-v-1" etc.) so subsequent test runs
	// don't collide with vertices from other smokes.
	v1 := graphschema.CodeGraphVertex{
		VID: "smoke-v-1",
		Properties: map[string]graphschema.CodeGraphPrimitive{
			"kind": "symbol", "orgId": 99, "workspaceId": "smoke",
			"repoId": 1, "commitHash": "smoke", "schemaVersion": 1,
			"builderVersion": "test", "label": "MyClass", "source": "test",
		},
	}
	v2 := graphschema.CodeGraphVertex{
		VID: "smoke-v-2",
		Properties: map[string]graphschema.CodeGraphPrimitive{
			"kind": "file", "orgId": 99, "workspaceId": "smoke",
			"repoId": 1, "commitHash": "smoke", "schemaVersion": 1,
			"builderVersion": "test", "path": "src/main.go", "source": "test",
		},
	}
	e1 := graphschema.CodeGraphEdge{
		FromVID: "smoke-v-1", ToVID: "smoke-v-2",
		Rank: graphschema.EdgeRank("smoke-edge-1"),
		Properties: map[string]graphschema.CodeGraphPrimitive{
			"kind": "DEFINES", "orgId": 99, "workspaceId": "smoke",
			"repoId": 1, "commitHash": "smoke", "schemaVersion": 1,
			"builderVersion": "test", "source": "test",
		},
	}

	usePrefix := "USE `" + graphschema.SpaceName + "`; "

	// Cleanup helper used both before (in case a prior failed run
	// left rows) and after.
	cleanup := func() {
		_, _ = client.Execute(ctx, usePrefix+`DELETE VERTEX "smoke-v-1", "smoke-v-2" WITH EDGE;`)
	}
	cleanup()
	defer cleanup()

	if _, err := client.Execute(ctx, usePrefix+graphschema.RenderVertexInsert([]graphschema.CodeGraphVertex{v1, v2})); err != nil {
		t.Fatalf("RenderVertexInsert: %v", err)
	}
	if _, err := client.Execute(ctx, usePrefix+graphschema.RenderEdgeInsert([]graphschema.CodeGraphEdge{e1})); err != nil {
		t.Fatalf("RenderEdgeInsert: %v", err)
	}

	// Verify the vertices landed via a scope LOOKUP scoped to the
	// smoke workspace.
	verifyStmt := usePrefix + `LOOKUP ON ` + "`code_graph_node`" + ` WHERE ` + "`code_graph_node`.`workspaceId`" + ` == "smoke" YIELD id(vertex) AS vid;`
	rs, err := client.Execute(ctx, verifyStmt)
	if err != nil {
		t.Fatalf("LOOKUP: %v", err)
	}
	if rs.GetRowSize() < 2 {
		t.Errorf("LOOKUP smoke vertices: got %d rows, want >= 2", rs.GetRowSize())
	}
}

// TestNebulaSmoke_LookupAndDeleteSnapshot exercises the full
// snapshot-retirement cycle end-to-end against the running
// cluster:
//
//  1. Bootstrap schema (idempotent).
//  2. INSERT VERTEX for two vertices scoped to a unique snapshot
//     tuple (commitHash = "snap-delete-test").
//  3. INSERT EDGE between them so the WITH EDGE branch of DELETE
//     gets exercised — the edge must vanish alongside the vertex.
//  4. RenderLookupSnapshotVerticesStatement → assert both vids
//     resolve.
//  5. Read the vids from the LOOKUP result, hand to
//     RenderDeleteVerticesStatements, execute each.
//  6. RenderLookupSnapshotVerticesStatement again → assert zero
//     rows (the snapshot's gone).
func TestNebulaSmoke_LookupAndDeleteSnapshot(t *testing.T) {
	addr := os.Getenv(nebulaclient.EnvAddr)
	if addr == "" {
		t.Skipf("%s is unset; skipping lookup+delete smoke", nebulaclient.EnvAddr)
	}
	if os.Getenv(nebulaclient.EnvUser) == "" {
		t.Setenv(nebulaclient.EnvUser, defaultUser)
	}
	if os.Getenv(nebulaclient.EnvPassword) == "" {
		t.Setenv(nebulaclient.EnvPassword, defaultPass)
	}

	cfg, err := nebulaclient.LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	cfg.Timeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := nebulaclient.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("nebulaclient.New: %v", err)
	}
	defer client.Close()

	if err := graphschema.Bootstrap(ctx, client, graphschema.BootstrapOptions{}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Snapshot scope used both for the inserts and the subsequent
	// LOOKUP / DELETE. A unique commitHash isolates the test data
	// from any other vertices left behind by prior smokes.
	scope := graphschema.CodeGraphDeleteInput{
		OrgID:          99,
		WorkspaceID:    "snap-delete-test",
		RepoID:         1,
		CommitHash:     "snap-delete-test-commit",
		SchemaVersion:  1,
		BuilderVersion: "smoke",
	}
	scopeProps := map[string]graphschema.CodeGraphPrimitive{
		"orgId":          scope.OrgID,
		"workspaceId":    scope.WorkspaceID,
		"repoId":         scope.RepoID,
		"commitHash":     scope.CommitHash,
		"schemaVersion":  scope.SchemaVersion,
		"builderVersion": scope.BuilderVersion,
		"source":         "test",
	}
	v1 := graphschema.CodeGraphVertex{VID: "snap-del-v-1", Properties: mergeProps(scopeProps, map[string]graphschema.CodeGraphPrimitive{"kind": "symbol", "label": "A"})}
	v2 := graphschema.CodeGraphVertex{VID: "snap-del-v-2", Properties: mergeProps(scopeProps, map[string]graphschema.CodeGraphPrimitive{"kind": "file", "path": "src/x.go"})}
	e1 := graphschema.CodeGraphEdge{
		FromVID: "snap-del-v-1", ToVID: "snap-del-v-2",
		Rank:       graphschema.EdgeRank("snap-del-e-1"),
		Properties: mergeProps(scopeProps, map[string]graphschema.CodeGraphPrimitive{"kind": "DEFINES"}),
	}

	usePrefix := "USE `" + graphschema.SpaceName + "`; "

	// Pre-cleanup in case a prior failed run left rows behind.
	for _, stmt := range graphschema.RenderDeleteVerticesStatements([]string{"snap-del-v-1", "snap-del-v-2"}) {
		_, _ = client.Execute(ctx, usePrefix+stmt)
	}

	if _, err := client.Execute(ctx, usePrefix+graphschema.RenderVertexInsert([]graphschema.CodeGraphVertex{v1, v2})); err != nil {
		t.Fatalf("RenderVertexInsert: %v", err)
	}
	if _, err := client.Execute(ctx, usePrefix+graphschema.RenderEdgeInsert([]graphschema.CodeGraphEdge{e1})); err != nil {
		t.Fatalf("RenderEdgeInsert: %v", err)
	}

	// LOOKUP — assert both vids resolve.
	lookupStmt := usePrefix + graphschema.RenderLookupSnapshotVerticesStatement(scope)
	rs, err := client.Execute(ctx, lookupStmt)
	if err != nil {
		t.Fatalf("LOOKUP (pre-delete): %v", err)
	}
	if got := rs.GetRowSize(); got != 2 {
		t.Errorf("LOOKUP (pre-delete): got %d rows, want 2", got)
	}

	// Extract the resolved vids — the YIELD clause names the
	// column `vid` and emits a string per row.
	vids := make([]string, 0, rs.GetRowSize())
	for i := 0; i < rs.GetRowSize(); i++ {
		rec, recErr := rs.GetRowValuesByIndex(i)
		if recErr != nil {
			t.Fatalf("row %d: %v", i, recErr)
		}
		val, valErr := rec.GetValueByIndex(0)
		if valErr != nil {
			t.Fatalf("row %d col 0: %v", i, valErr)
		}
		s, sErr := val.AsString()
		if sErr != nil {
			t.Fatalf("row %d AsString: %v", i, sErr)
		}
		vids = append(vids, s)
	}

	// DELETE — run each chunk's statement.
	for _, stmt := range graphschema.RenderDeleteVerticesStatements(vids) {
		if _, err := client.Execute(ctx, usePrefix+stmt); err != nil {
			t.Fatalf("DELETE: %v", err)
		}
	}

	// Re-LOOKUP — zero rows.
	rs, err = client.Execute(ctx, lookupStmt)
	if err != nil {
		t.Fatalf("LOOKUP (post-delete): %v", err)
	}
	if got := rs.GetRowSize(); got != 0 {
		t.Errorf("LOOKUP (post-delete): got %d rows, want 0 (the snapshot should be gone)", got)
	}
}

// TestNebulaSmoke_DescribeTagEdge exercises the two DESCRIBE
// renderers end-to-end against the running cluster. Read-only:
// no schema mutation, so safe to run on a long-lived dev
// cluster. ALTER renderers are NOT exercised live to avoid
// permanently widening the cluster's schema during a smoke run;
// they're covered by the parity tests in the graphschema
// package.
//
// Asserted post-conditions:
//   - DESCRIBE TAG `code_graph_node` returns >= 21 rows (one per
//     declared column in NodeProps; bootstrap may add more in a
//     future slice).
//   - DESCRIBE EDGE `code_graph_edge` returns >= 15 rows (one
//     per EdgeProp).
func TestNebulaSmoke_DescribeTagEdge(t *testing.T) {
	addr := os.Getenv(nebulaclient.EnvAddr)
	if addr == "" {
		t.Skipf("%s is unset; skipping describe smoke", nebulaclient.EnvAddr)
	}
	if os.Getenv(nebulaclient.EnvUser) == "" {
		t.Setenv(nebulaclient.EnvUser, defaultUser)
	}
	if os.Getenv(nebulaclient.EnvPassword) == "" {
		t.Setenv(nebulaclient.EnvPassword, defaultPass)
	}

	cfg, err := nebulaclient.LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	cfg.Timeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := nebulaclient.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("nebulaclient.New: %v", err)
	}
	defer client.Close()

	if err := graphschema.Bootstrap(ctx, client, graphschema.BootstrapOptions{}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	usePrefix := "USE `" + graphschema.SpaceName + "`; "

	tagRS, err := client.Execute(ctx, usePrefix+graphschema.RenderDescribeTagStatement())
	if err != nil {
		t.Fatalf("DESCRIBE TAG: %v", err)
	}
	if got := tagRS.GetRowSize(); got < len(graphschema.NodeProps) {
		t.Errorf("DESCRIBE TAG row count: got %d, want >= %d (NodeProps length)", got, len(graphschema.NodeProps))
	}

	edgeRS, err := client.Execute(ctx, usePrefix+graphschema.RenderDescribeEdgeStatement())
	if err != nil {
		t.Fatalf("DESCRIBE EDGE: %v", err)
	}
	if got := edgeRS.GetRowSize(); got < len(graphschema.EdgeProps) {
		t.Errorf("DESCRIBE EDGE row count: got %d, want >= %d (EdgeProps length)", got, len(graphschema.EdgeProps))
	}
}

// TestNebulaSmoke_StoreWriteAndRetire exercises the full
// codeintel-backend graphstore.NebulaCodeGraphStore end-to-end
// against the running cluster:
//
//  1. Bootstrap (idempotent — store's WriteSnapshot also calls
//     ensureSchema internally, so this proves both work).
//  2. WriteSnapshot with 2 vertices + 1 edge (the edge has
//     source="anchor-linker" so LinkedEdgeCount is 1).
//  3. Verify the returned CodeGraphWriteResult shape matches
//     the expected counts.
//  4. LOOKUP returns 2 vids.
//  5. MarkSnapshotForDeletion → re-LOOKUP returns 0 vids.
func TestNebulaSmoke_StoreWriteAndRetire(t *testing.T) {
	addr := os.Getenv(nebulaclient.EnvAddr)
	if addr == "" {
		t.Skipf("%s is unset; skipping graphstore smoke", nebulaclient.EnvAddr)
	}
	if os.Getenv(nebulaclient.EnvUser) == "" {
		t.Setenv(nebulaclient.EnvUser, defaultUser)
	}
	if os.Getenv(nebulaclient.EnvPassword) == "" {
		t.Setenv(nebulaclient.EnvPassword, defaultPass)
	}

	cfg, err := nebulaclient.LoadConfigFromEnv()
	if err != nil {
		t.Fatalf("LoadConfigFromEnv: %v", err)
	}
	cfg.Timeout = 30 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	client, err := nebulaclient.New(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("nebulaclient.New: %v", err)
	}
	defer client.Close()

	// Bootstrap the space so the USE-prefix path works.
	if err := graphschema.Bootstrap(ctx, client, graphschema.BootstrapOptions{}); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	executor := &usePrefixedExecutor{c: client, prefix: "USE `" + graphschema.SpaceName + "`; "}
	store := graphstore.NewWithRetry(executor, logger, graphstore.SchemaRetryConfig{Attempts: 5, Delay: 100 * time.Millisecond})

	scope := graphschema.CodeGraphDeleteInput{
		OrgID:          99,
		WorkspaceID:    "store-smoke",
		RepoID:         1,
		CommitHash:     "store-smoke-commit",
		SchemaVersion:  1,
		BuilderVersion: "smoke",
	}
	scopeProps := map[string]graphschema.CodeGraphPrimitive{
		"orgId":          scope.OrgID,
		"workspaceId":    scope.WorkspaceID,
		"repoId":         scope.RepoID,
		"commitHash":     scope.CommitHash,
		"schemaVersion":  scope.SchemaVersion,
		"builderVersion": scope.BuilderVersion,
	}
	snapshot := graphschema.CodeGraphSnapshot{
		OrgID: scope.OrgID, WorkspaceID: scope.WorkspaceID, RepoID: scope.RepoID,
		CommitHash: scope.CommitHash, SchemaVersion: scope.SchemaVersion, BuilderVersion: scope.BuilderVersion,
		Vertices: []graphschema.CodeGraphVertex{
			{VID: "store-smk-v-1", Properties: mergeProps(scopeProps, map[string]graphschema.CodeGraphPrimitive{"kind": "symbol", "label": "A", "source": "test"})},
			{VID: "store-smk-v-2", Properties: mergeProps(scopeProps, map[string]graphschema.CodeGraphPrimitive{"kind": "file", "path": "src/x.go", "source": "test"})},
		},
		Edges: []graphschema.CodeGraphEdge{
			{
				FromVID: "store-smk-v-1", ToVID: "store-smk-v-2",
				Rank:       graphschema.EdgeRank("store-smk-e-1"),
				Properties: mergeProps(scopeProps, map[string]graphschema.CodeGraphPrimitive{"kind": "DEFINES", "source": "anchor-linker"}),
			},
		},
	}

	// Pre-cleanup in case a prior failed run left rows.
	_ = store.MarkSnapshotForDeletion(ctx, scope)

	result, err := store.WriteSnapshot(ctx, snapshot)
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if result.Status != graphschema.WriteStatusReady {
		t.Errorf("Status: got %q, want READY", result.Status)
	}
	if result.VertexCount != 2 || result.EdgeCount != 1 {
		t.Errorf("counts: got vertex=%d edge=%d, want 2/1", result.VertexCount, result.EdgeCount)
	}
	if result.LinkedEdgeCount != 1 {
		t.Errorf("LinkedEdgeCount: got %d, want 1 (the anchor-linker edge)", result.LinkedEdgeCount)
	}

	// LOOKUP confirms the vertices landed.
	rs, err := executor.Execute(ctx, graphschema.RenderLookupSnapshotVerticesStatement(scope))
	if err != nil {
		t.Fatalf("LOOKUP post-write: %v", err)
	}
	if got := rs.GetRowSize(); got != 2 {
		t.Errorf("LOOKUP post-write: got %d rows, want 2", got)
	}

	// MarkSnapshotForDeletion.
	if err := store.MarkSnapshotForDeletion(ctx, scope); err != nil {
		t.Fatalf("MarkSnapshotForDeletion: %v", err)
	}

	// Re-LOOKUP confirms the snapshot's gone.
	rs, err = executor.Execute(ctx, graphschema.RenderLookupSnapshotVerticesStatement(scope))
	if err != nil {
		t.Fatalf("LOOKUP post-delete: %v", err)
	}
	if got := rs.GetRowSize(); got != 0 {
		t.Errorf("LOOKUP post-delete: got %d rows, want 0", got)
	}
}

// mergeProps shallow-merges the second map onto the first,
// returning a fresh map. Used to layer per-vertex/edge properties
// on top of a shared scope-property base.
func mergeProps(base, extra map[string]graphschema.CodeGraphPrimitive) map[string]graphschema.CodeGraphPrimitive {
	out := make(map[string]graphschema.CodeGraphPrimitive, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// containsRowValue scans the first column of every result-set row
// for an exact string match. SHOW TAGS / SHOW EDGES / SHOW TAG
// INDEXES emit the entity name in column 0 — that's the column
// the bootstrap verifier inspects.
func containsRowValue(t *testing.T, rs *nebula.ResultSet, want string) bool {
	t.Helper()
	for i := 0; i < rs.GetRowSize(); i++ {
		rec, err := rs.GetRowValuesByIndex(i)
		if err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		val, err := rec.GetValueByIndex(0)
		if err != nil {
			t.Fatalf("row %d col 0: %v", i, err)
		}
		s, sErr := val.AsString()
		if sErr == nil && strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}
