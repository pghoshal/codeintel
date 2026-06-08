package codegraphwriter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"codeintel/internal/backend/graphstore"
	"codeintel/pkg/graphschema"

	"github.com/hibiken/asynq"
	"github.com/pashagolub/pgxmock/v4"
)

func TestPayloadCamelCaseJSONAndValidation(t *testing.T) {
	raw := []byte(`{"orgId":7,"workspaceId":"ws-1","repoId":42,"revision":"refs/heads/main","commitHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","schemaVersion":1,"builderVersion":"codeintel-code-graph-v7","indexJobId":"job-1","manifestId":"manifest-1","providerConnectionId":"conn-1","source":"syntactic-ast","statements":["CREATE TAG IF NOT EXISTS x;"]}`)
	p, err := UnmarshalPayload(raw)
	if err != nil {
		t.Fatalf("UnmarshalPayload: %v", err)
	}
	if p.OrgID != 7 || p.WorkspaceID != "ws-1" || p.RepoID != 42 || p.Revision != "refs/heads/main" {
		t.Fatalf("unexpected payload: %#v", p)
	}
	if p.IndexJobID != "job-1" || p.ManifestID != "manifest-1" || p.ProviderConnectionID == nil || *p.ProviderConnectionID != "conn-1" {
		t.Fatalf("unexpected payload IDs: %#v", p)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestRenderedEdgeDedupeKeyIsPostgresTextSafe(t *testing.T) {
	got := renderedEdgeDedupeKey("from\x00vid", "to\x00vid", "CALLS", "123")
	if strings.ContainsRune(got, 0) {
		t.Fatalf("dedupe key contains NUL byte: %q", got)
	}
	if !strings.HasPrefix(got, "rendered-edge:") || len(strings.TrimPrefix(got, "rendered-edge:")) != 64 {
		t.Fatalf("dedupe key shape = %q", got)
	}
	if got != renderedEdgeDedupeKey("from\x00vid", "to\x00vid", "CALLS", "123") {
		t.Fatal("dedupe key is not deterministic")
	}
}

func TestCountRenderedAnchorLinksCountsIncidentSemanticEdges(t *testing.T) {
	anchor := PayloadAnchor{NodeVID: "cg:o7:r42:file:tenant"}
	edges := []graphstore.RenderedEdgeRow{
		{FromVID: "cg:o7:r42:repo", ToVID: "cg:o7:r42:file:tenant", Props: map[string]string{"source": "syntactic-ast"}},
		{FromVID: "cg:o7:r42:file:tenant", ToVID: "cg:o7:r42:symbol:tenantMarker", Props: map[string]string{"source": "scip"}},
		{FromVID: "cg:o7:r42:other", ToVID: "cg:o7:r42:another", Props: map[string]string{"source": "syntactic-ast"}},
	}
	if got := countRenderedAnchorLinks(edges, []PayloadAnchor{anchor}); got != 2 {
		t.Fatalf("linked edge count = %d, want 2", got)
	}
}

func TestCountRenderedAnchorLinksStillCountsExplicitAnchorLinkerEdges(t *testing.T) {
	edges := []graphstore.RenderedEdgeRow{{
		FromVID: "cg:o7:r42:a",
		ToVID:   "cg:o7:r42:b",
		Props:   map[string]string{"source": "anchor-linker"},
	}}
	if got := countRenderedAnchorLinks(edges, nil); got != 1 {
		t.Fatalf("linked edge count = %d, want 1", got)
	}
}

func TestPersistRenderedSemanticRowsAllowsSchemaOnlyGraph(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	payload := validPayload()
	payload.Anchors = nil
	payload.Statements = graphschema.RenderSchemaStatements()
	input := renderedInputFromPayload(payload)
	mock.ExpectExec(`DELETE FROM "CodeGraphAnchor"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM "CodeGraphSemanticEdge"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM "CodeGraphSemanticFact"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	h := &Handler{db: mock}
	summary, err := h.persistRenderedSemanticRows(context.Background(), "graph-index-1", payload, input)
	if err != nil {
		t.Fatalf("persist schema-only graph: %v", err)
	}
	if summary.AnchorCount != 0 || summary.LinkedEdgeCount != 0 {
		t.Fatalf("schema-only summary = %+v, want zero counts", summary)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestUnmarshalPayloadRejectsOversizedBody(t *testing.T) {
	_, err := UnmarshalPayload(make([]byte, maxPayloadBytes+1))
	if err == nil {
		t.Fatalf("expected oversized payload error")
	}
}

func TestHandlerSkipsRetryOnRenderedValidationBeforeDatabaseMutation(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	payload := validPayload()
	payload.Statements = append(payload.Statements, "DROP SPACE `codeintel`;")
	raw, _ := json.Marshal(payload)

	graph := &fakeGraphWriter{}
	h := NewHandler(mock, graph, nil)
	err = h.Handle(context.Background(), asynq.NewTask("code-graph-write", raw))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("got %v, want asynq.SkipRetry", err)
	}
	if graph.calls != 0 {
		t.Fatalf("graph calls: got %d, want 0", graph.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerSkipsRetryOnWorkspaceMismatch(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	payload := validPayload()
	raw, _ := json.Marshal(payload)
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("org-workspace"))

	h := NewHandler(mock, &fakeGraphWriter{}, nil)
	err = h.Handle(context.Background(), asynq.NewTask("code-graph-write", raw))
	if !errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("got %v, want asynq.SkipRetry", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerRetriesWhenManifestIsStillPending(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	payload := validPayload()
	raw, _ := json.Marshal(payload)
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("ws-1"))
	mock.ExpectQuery(`SELECT status::text`).
		WithArgs(int32(7), int32(42), "ws-1", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "manifest-1", "job-1", nil).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("PENDING"))

	h := NewHandler(mock, &fakeGraphWriter{}, nil)
	err = h.Handle(context.Background(), asynq.NewTask("code-graph-write", raw))
	if err == nil || errors.Is(err, asynq.SkipRetry) {
		t.Fatalf("got %v, want retryable pending-manifest error", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerReadyManifestWritesGraphAndRevision(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	payload := validPayload()
	raw, _ := json.Marshal(payload)
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("ws-1"))
	mock.ExpectQuery(`SELECT status::text`).
		WithArgs(int32(7), int32(42), "ws-1", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "manifest-1", "job-1", nil).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("READY"))
	mock.ExpectQuery(`INSERT INTO "CodeGraphIndex"`).
		WithArgs("new-id", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", graphschema.SpaceName,
			"ws-1", int32(1), "codeintel-code-graph-v7", "job-1", int32(7), int32(42)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "status"}).AddRow("graph-index-1", "BUILDING"))
	mock.ExpectExec(`DELETE FROM "CodeGraphAnchor"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM "CodeGraphSemanticEdge"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM "CodeGraphSemanticFact"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	expectRenderedSemanticRows(t, mock, len(payload.Anchors))
	mock.ExpectExec(`UPDATE "CodeGraphIndex"`).
		WithArgs("graph-index-1", int32(7), int32(42), graphschema.SpaceName, int32(2), int32(1), int32(1), int32(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`INSERT INTO "CodeGraphRevision"`).
		WithArgs("new-id", "refs/heads/main", "ws-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			int32(1), "codeintel-code-graph-v7", int32(7), int32(42), "graph-index-1", "manifest-1", "job-1", nil, "refs/heads/main").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	graph := &fakeGraphWriter{result: graphschema.CodeGraphWriteResult{
		Status:      graphschema.WriteStatusReady,
		VertexCount: 2,
		EdgeCount:   1,
	}}
	h := NewHandler(mock, graph, nil)
	h.newID = func() string { return "new-id" }

	if err := h.Handle(context.Background(), asynq.NewTask("code-graph-write", raw)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if graph.calls != 1 {
		t.Fatalf("graph calls: got %d, want 1", graph.calls)
	}
	if graph.lastInput.OrgID != payload.OrgID || graph.lastInput.WorkspaceID != payload.WorkspaceID || graph.lastInput.RepoID != payload.RepoID {
		t.Fatalf("graph input scope mismatch: %#v", graph.lastInput)
	}
	if len(graph.lastInput.Statements) != len(payload.Statements) {
		t.Fatalf("graph input statement count: got %d, want %d", len(graph.lastInput.Statements), len(payload.Statements))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerDuplicateReadyDeliveryDoesNotRewriteGraph(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	payload := validPayload()
	raw, _ := json.Marshal(payload)
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("ws-1"))
	mock.ExpectQuery(`SELECT status::text`).
		WithArgs(int32(7), int32(42), "ws-1", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "manifest-1", "job-1", nil).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("READY"))
	mock.ExpectQuery(`INSERT INTO "CodeGraphIndex"`).
		WithArgs("new-id", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", graphschema.SpaceName,
			"ws-1", int32(1), "codeintel-code-graph-v7", "job-1", int32(7), int32(42)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "status"}).AddRow("graph-index-1", "READY"))
	mock.ExpectExec(`INSERT INTO "CodeGraphRevision"`).
		WithArgs("new-id", "refs/heads/main", "ws-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			int32(1), "codeintel-code-graph-v7", int32(7), int32(42), "graph-index-1", "manifest-1", "job-1", nil, "refs/heads/main").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`WITH counts AS`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	graph := &fakeGraphWriter{}
	h := NewHandler(mock, graph, nil)
	h.newID = func() string { return "new-id" }

	if err := h.Handle(context.Background(), asynq.NewTask("code-graph-write", raw)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if graph.calls != 0 {
		t.Fatalf("graph calls: got %d, want 0", graph.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestHandlerDoesNotActivateRevisionWhenManifestTurnsStale(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	payload := validPayload()
	raw, _ := json.Marshal(payload)
	mock.ExpectQuery(`SELECT COALESCE\(o\."atomWorkspaceId"`).
		WithArgs(int32(42), int32(7)).
		WillReturnRows(pgxmock.NewRows([]string{"workspaceId"}).AddRow("ws-1"))
	mock.ExpectQuery(`SELECT status::text`).
		WithArgs(int32(7), int32(42), "ws-1", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "manifest-1", "job-1", nil).
		WillReturnRows(pgxmock.NewRows([]string{"status"}).AddRow("READY"))
	mock.ExpectQuery(`INSERT INTO "CodeGraphIndex"`).
		WithArgs("new-id", "refs/heads/main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", graphschema.SpaceName,
			"ws-1", int32(1), "codeintel-code-graph-v7", "job-1", int32(7), int32(42)).
		WillReturnRows(pgxmock.NewRows([]string{"id", "status"}).AddRow("graph-index-1", "BUILDING"))
	mock.ExpectExec(`DELETE FROM "CodeGraphAnchor"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM "CodeGraphSemanticEdge"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectExec(`DELETE FROM "CodeGraphSemanticFact"`).
		WithArgs("graph-index-1", int32(7), int32(42)).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	expectRenderedSemanticRows(t, mock, len(payload.Anchors))
	mock.ExpectExec(`UPDATE "CodeGraphIndex"`).
		WithArgs("graph-index-1", int32(7), int32(42), graphschema.SpaceName, int32(2), int32(1), int32(1), int32(1)).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`INSERT INTO "CodeGraphRevision"`).
		WithArgs("new-id", "refs/heads/main", "ws-1", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			int32(1), "codeintel-code-graph-v7", int32(7), int32(42), "graph-index-1", "manifest-1", "job-1", nil, "refs/heads/main").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))

	graph := &fakeGraphWriter{result: graphschema.CodeGraphWriteResult{
		Status:      graphschema.WriteStatusReady,
		VertexCount: 2,
		EdgeCount:   1,
	}}
	h := NewHandler(mock, graph, nil)
	h.newID = func() string { return "new-id" }

	if err := h.Handle(context.Background(), asynq.NewTask("code-graph-write", raw)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if graph.calls != 1 {
		t.Fatalf("graph calls: got %d, want 1", graph.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

type fakeGraphWriter struct {
	calls     int
	lastInput graphstore.RenderedStatementWrite
	result    graphschema.CodeGraphWriteResult
	err       error
}

func (f *fakeGraphWriter) WriteRenderedStatements(_ context.Context, input graphstore.RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error) {
	f.calls++
	f.lastInput = input
	if f.err != nil {
		return graphschema.CodeGraphWriteResult{}, f.err
	}
	if f.result.Status == "" {
		return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusReady}, nil
	}
	return f.result, nil
}

func validPayload() Payload {
	statements := renderedStatementsForPayloadScope(Payload{
		OrgID:          7,
		WorkspaceID:    "ws-1",
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SchemaVersion:  1,
		BuilderVersion: "codeintel-code-graph-v7",
		Source:         "syntactic-ast",
	})
	return Payload{
		OrgID:          7,
		WorkspaceID:    "ws-1",
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SchemaVersion:  1,
		BuilderVersion: "codeintel-code-graph-v7",
		IndexJobID:     "job-1",
		ManifestID:     "manifest-1",
		Source:         "syntactic-ast",
		Statements:     statements,
		Anchors: []PayloadAnchor{{
			Kind:           "symbol",
			Direction:      "PROVIDES",
			Key:            "index",
			NormalizedKey:  "index",
			NodeVID:        scopedVIDForPayload(Payload{OrgID: 7, WorkspaceID: "ws-1", RepoID: 42, Revision: "refs/heads/main", CommitHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SchemaVersion: 1, BuilderVersion: "codeintel-code-graph-v7"}, "file", strings.Repeat("b", 32)),
			Confidence:     0.95,
			ConfidenceTier: "EXTRACTED",
			Source:         "scip-typescript",
		}},
	}
}

func expectRenderedSemanticRows(t *testing.T, mock pgxmock.PgxPoolIface, anchorCount int) {
	t.Helper()
	if anchorCount > 0 {
		mock.ExpectExec(`INSERT INTO "CodeGraphAnchor"`).
			WithArgs(anyArgs(anchorCount * 18)...).
			WillReturnResult(pgxmock.NewResult("INSERT", 1))
	}
	mock.ExpectExec(`INSERT INTO "CodeGraphSemanticFact"`).
		WithArgs(anyArgs(2 * 19)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`INSERT INTO "CodeGraphSemanticEdge"`).
		WithArgs(anyArgs(19)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
}

func anyArgs(n int) []any {
	args := make([]any, n)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

func renderedStatementsForPayloadScope(scope Payload) []string {
	repoVID := scopedVIDForPayload(scope, "repo", strings.Repeat("a", 32))
	fileVID := scopedVIDForPayload(scope, "file", strings.Repeat("b", 32))
	repoVertex := graphschema.CodeGraphVertex{
		VID: repoVID,
		Properties: map[string]graphschema.CodeGraphPrimitive{
			"kind":           "repo",
			"orgId":          scope.OrgID,
			"workspaceId":    scope.WorkspaceID,
			"repoId":         scope.RepoID,
			"revision":       scope.Revision,
			"commitHash":     scope.CommitHash,
			"schemaVersion":  scope.SchemaVersion,
			"builderVersion": scope.BuilderVersion,
			"key":            "repo:test",
			"label":          "test repo",
			"source":         scope.Source,
		},
	}
	fileVertex := graphschema.CodeGraphVertex{
		VID: fileVID,
		Properties: map[string]graphschema.CodeGraphPrimitive{
			"kind":           "file",
			"orgId":          scope.OrgID,
			"workspaceId":    scope.WorkspaceID,
			"repoId":         scope.RepoID,
			"revision":       scope.Revision,
			"commitHash":     scope.CommitHash,
			"schemaVersion":  scope.SchemaVersion,
			"builderVersion": scope.BuilderVersion,
			"key":            "file:src/index.ts",
			"label":          "src/index.ts",
			"path":           "src/index.ts",
			"language":       "TypeScript",
			"source":         scope.Source,
		},
	}
	edge := graphschema.CodeGraphEdge{
		FromVID: repoVID,
		ToVID:   fileVID,
		Rank:    graphschema.EdgeRank(repoVID + "->" + fileVID + ":CONTAINS:" + scope.Source),
		Properties: map[string]graphschema.CodeGraphPrimitive{
			"kind":           "CONTAINS",
			"orgId":          scope.OrgID,
			"workspaceId":    scope.WorkspaceID,
			"repoId":         scope.RepoID,
			"revision":       scope.Revision,
			"commitHash":     scope.CommitHash,
			"schemaVersion":  scope.SchemaVersion,
			"builderVersion": scope.BuilderVersion,
			"confidence":     1.0,
			"confidenceTier": "HIGH",
			"normalizedKey":  "repo:test->file:src/index.ts",
			"source":         scope.Source,
		},
	}
	return graphschema.RenderSnapshotStatements(graphschema.CodeGraphSnapshot{
		Vertices: []graphschema.CodeGraphVertex{repoVertex, fileVertex},
		Edges:    []graphschema.CodeGraphEdge{edge},
	})
}

func scopedVIDForPayload(scope Payload, kind, keyHash string) string {
	return strings.Join([]string{
		"cg",
		"o" + strconv.FormatInt(scope.OrgID, 10),
		"w" + testHashParts([]string{scope.WorkspaceID}, 8),
		"r" + strconv.FormatInt(scope.RepoID, 10),
		"c" + scope.CommitHash[:12],
		"s" + strconv.FormatInt(scope.SchemaVersion, 10),
		"b" + testHashParts([]string{scope.BuilderVersion}, 8),
		kind,
		keyHash,
	}, ":")
}

func testHashParts(parts []string, length int) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	encoded := hex.EncodeToString(sum[:])
	if length > len(encoded) {
		return encoded
	}
	return encoded[:length]
}
