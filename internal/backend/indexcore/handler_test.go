package indexcore

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"codeintel/internal/backend/graphstore"
	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/pkg/graphschema"

	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
)

func TestMergeGraphTreatsAlreadyReadySnapshotAsIdempotentSuccess(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-workspace-1"
	payload := indexsubjobtask.Payload{
		SubjobID:          "graph-merge-1",
		RepoIndexingJobID: "index-job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             indexsubjobtask.LayerGraphMerge,
	}

	mock.ExpectQuery(`SELECT id\s+FROM "CodeGraphIndex"`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Revision, payload.CommitHash, defaultSchemaVersion, defaultBuilder).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("graph-index-ready-1"))
	mock.ExpectQuery(`SELECT s\.symbol`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash).
		WillReturnRows(pgxmock.NewRows([]string{"symbol", "displayName", "kind", "language", "filePath", "startLine", "endLine"}))
	mock.ExpectQuery(`SELECT r\."sourceSymbol"`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash).
		WillReturnRows(pgxmock.NewRows([]string{"sourceSymbol", "targetSymbol", "isReference", "isImplementation", "isTypeDefinition", "isDefinition", "filePath", "startLine", "endLine"}))
	mock.ExpectQuery(`WITH candidate_files`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash, scipOccurrenceCap(), scipFileCap()).
		WillReturnRows(pgxmock.NewRows([]string{"symbol", "filePath", "startLine", "endLine", "role", "syntaxKind", "lineContent", "enclosingSymbol"}))
	mock.ExpectExec(`UPDATE "CodeGraphIndex" g`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Revision, payload.CommitHash, defaultSchemaVersion, defaultBuilder, payload.RepoIndexingJobID, payload.Branch).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectQuery(`SELECT EXISTS \(`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Revision, payload.CommitHash, defaultSchemaVersion, defaultBuilder, payload.RepoIndexingJobID, payload.Branch).
		WillReturnRows(pgxmock.NewRows([]string{"exists"}).AddRow(true))

	h := &Handler{
		db:     mock,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := h.mergeGraph(context.Background(), payload); err != nil {
		t.Fatalf("mergeGraph: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestMergeGraphCreatesSCIPOnlySnapshotWhenASTWasSkipped(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-workspace-1"
	payload := indexsubjobtask.Payload{
		SubjobID:          "graph-merge-scip-only",
		RepoIndexingJobID: "index-job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             indexsubjobtask.LayerGraphMerge,
	}
	symbol := "scip-go github.com/acme/orders internal/webhook/handler.go/Handle()."

	mock.ExpectQuery(`SELECT id\s+FROM "CodeGraphIndex"`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Revision, payload.CommitHash, defaultSchemaVersion, defaultBuilder).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`SELECT s\.symbol`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash).
		WillReturnRows(pgxmock.NewRows([]string{"symbol", "displayName", "kind", "language", "filePath", "startLine", "endLine"}).
			AddRow(symbol, "Handle", "Function", "go", "internal/webhook/handler.go", int32(12), int32(20)))
	mock.ExpectQuery(`SELECT r\."sourceSymbol"`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash).
		WillReturnRows(pgxmock.NewRows([]string{"sourceSymbol", "targetSymbol", "isReference", "isImplementation", "isTypeDefinition", "isDefinition", "filePath", "startLine", "endLine"}))
	mock.ExpectQuery(`WITH candidate_files`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash, scipOccurrenceCap(), scipFileCap()).
		WillReturnRows(pgxmock.NewRows([]string{"symbol", "filePath", "startLine", "endLine", "role", "syntaxKind", "lineContent", "enclosingSymbol"}))
	mock.ExpectQuery(`INSERT INTO "CodeGraphIndex"`).
		WithArgs(pgxmock.AnyArg(), payload.Revision, payload.CommitHash, graphschema.SpaceName, workspaceID, defaultSchemaVersion, defaultBuilder, payload.RepoIndexingJobID, payload.OrgID, payload.RepoID).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow("graph-index-scip-only"))
	mock.ExpectExec(`INSERT INTO "CodeGraphSemanticFact"`).
		WithArgs(anyArgs(17)...).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE "CodeGraphIndex" g`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Revision, payload.CommitHash, defaultSchemaVersion, defaultBuilder, payload.RepoIndexingJobID, payload.Branch).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	h := &Handler{
		db:     mock,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		newID:  func() string { return "graph-index-scip-only" },
	}
	if err := h.mergeGraph(context.Background(), payload); err != nil {
		t.Fatalf("mergeGraph: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestMergeGraphDoesNotBlockActivationWhenNoGraphEvidenceExists(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-workspace-1"
	payload := indexsubjobtask.Payload{
		SubjobID:          "graph-merge-no-evidence",
		RepoIndexingJobID: "index-job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             indexsubjobtask.LayerGraphMerge,
	}

	mock.ExpectQuery(`SELECT id\s+FROM "CodeGraphIndex"`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Revision, payload.CommitHash, defaultSchemaVersion, defaultBuilder).
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectQuery(`SELECT s\.symbol`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash).
		WillReturnRows(pgxmock.NewRows([]string{"symbol", "displayName", "kind", "language", "filePath", "startLine", "endLine"}))
	mock.ExpectQuery(`SELECT r\."sourceSymbol"`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash).
		WillReturnRows(pgxmock.NewRows([]string{"sourceSymbol", "targetSymbol", "isReference", "isImplementation", "isTypeDefinition", "isDefinition", "filePath", "startLine", "endLine"}))
	mock.ExpectQuery(`WITH candidate_files`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Branch, payload.Revision, payload.CommitHash, scipOccurrenceCap(), scipFileCap()).
		WillReturnRows(pgxmock.NewRows([]string{"symbol", "filePath", "startLine", "endLine", "role", "syntaxKind", "lineContent", "enclosingSymbol"}))

	h := &Handler{
		db:     mock,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := h.mergeGraph(context.Background(), payload); err != nil {
		t.Fatalf("mergeGraph: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestBuildSCIPSnapshotProjectsOccurrenceCallsAndImports(t *testing.T) {
	workspaceID := "atom-workspace-1"
	payload := indexsubjobtask.Payload{
		OrgID:       7,
		WorkspaceID: &workspaceID,
		RepoID:      42,
		Branch:      "refs/heads/main",
		Revision:    "refs/heads/main",
		CommitHash:  strings.Repeat("a", 40),
	}
	caller := "scip-go github.com/acme/orders internal/webhook/handler.go/Handle()."
	callee := "scip-go github.com/acme/orders internal/instrumentation/podmutator.go/Mutate()."
	imported := "scip-go github.com/acme/orders apis/v1alpha1/Instrumentation#"
	h := &Handler{}

	snapshot := h.buildSCIPSnapshot(payload,
		[]scipSymbolProjection{
			{Symbol: caller, DisplayName: "Handle", Kind: "Function", Language: "go", FilePath: "internal/webhook/handler.go"},
			{Symbol: callee, DisplayName: "Mutate", Kind: "Function", Language: "go", FilePath: "internal/instrumentation/podmutator.go"},
			{Symbol: imported, DisplayName: "Instrumentation", Kind: "Struct", Language: "go", FilePath: "apis/v1alpha1/instrumentation_types.go"},
		},
		nil,
		[]scipOccurrenceProjection{
			{
				Symbol:          callee,
				FilePath:        "internal/webhook/handler.go",
				StartLine:       sql.NullInt32{Int32: 24, Valid: true},
				EndLine:         sql.NullInt32{Int32: 24, Valid: true},
				Role:            "REFERENCE",
				SyntaxKind:      "IdentifierFunction",
				LineContent:     "return pm.Mutate(ctx, pod)",
				EnclosingSymbol: caller,
			},
			{
				Symbol:      imported,
				FilePath:    "internal/webhook/handler.go",
				StartLine:   sql.NullInt32{Int32: 8, Valid: true},
				EndLine:     sql.NullInt32{Int32: 8, Valid: true},
				Role:        "READ",
				SyntaxKind:  "IdentifierNamespace",
				LineContent: `v1alpha1 "github.com/acme/orders/apis/v1alpha1"`,
			},
		})

	if !snapshotHasEdge(snapshot, "CALLS", "call") {
		t.Fatalf("snapshot missing SCIP CALLS/call edge: %+v", snapshot.Edges)
	}
	if !snapshotHasEdge(snapshot, "IMPORTS", "import") {
		t.Fatalf("snapshot missing SCIP IMPORTS/import edge: %+v", snapshot.Edges)
	}
	if !snapshotHasVertexKind(snapshot, "file") {
		t.Fatalf("snapshot missing file vertex for import occurrence: %+v", snapshot.Vertices)
	}
}

func TestWriteSCIPSnapshotToGraphStreamsBoundedChunks(t *testing.T) {
	t.Setenv("CODEINTEL_SCIP_GRAPH_NGQL_BATCH_SIZE", "2")

	workspaceID := "atom-workspace-1"
	payload := indexsubjobtask.Payload{
		OrgID:       7,
		WorkspaceID: &workspaceID,
		RepoID:      42,
		Branch:      "refs/heads/main",
		Revision:    "refs/heads/main",
		CommitHash:  strings.Repeat("a", 40),
	}
	var vertices []graphschema.CodeGraphVertex
	for _, key := range []string{"a", "b", "c", "d", "e"} {
		vertices = append(vertices, graphschema.CodeGraphVertex{
			VID:  codeGraphVID(payload, "symbol", key),
			Kind: "symbol",
			Properties: scopedProps(payload, map[string]graphschema.CodeGraphPrimitive{
				"kind":       "symbol",
				"key":        key,
				"label":      key,
				"confidence": 0.95,
				"source":     "scip",
			}),
		})
	}
	edges := []graphschema.CodeGraphEdge{
		scipTestEdge(payload, "a", "b"),
		scipTestEdge(payload, "b", "c"),
		scipTestEdge(payload, "c", "d"),
	}
	fake := &fakeGraphWriter{}
	h := &Handler{graph: fake}

	if err := h.writeSCIPSnapshotToGraph(context.Background(), payload, graphschema.CodeGraphSnapshot{
		OrgID:          int64(payload.OrgID),
		WorkspaceID:    workspaceID,
		RepoID:         int64(payload.RepoID),
		Revision:       payload.Revision,
		CommitHash:     payload.CommitHash,
		SchemaVersion:  int64(defaultSchemaVersion),
		BuilderVersion: defaultBuilder,
		Vertices:       vertices,
		Edges:          edges,
	}); err != nil {
		t.Fatalf("writeSCIPSnapshotToGraph: %v", err)
	}

	if fake.calls != 5 {
		t.Fatalf("streamed calls=%d want 5", fake.calls)
	}
	if fake.maxStatements > len(graphschema.RenderSchemaStatements())+1 {
		t.Fatalf("max statements per chunk=%d exceeds schema+single insert", fake.maxStatements)
	}
}

func TestSCIPGraphProjectionBudgetsAreBounded(t *testing.T) {
	t.Setenv("CODEINTEL_SCIP_GRAPH_OCCURRENCE_LIMIT", "9999999")
	t.Setenv("CODEINTEL_SCIP_GRAPH_SYMBOL_ROLE_LIMIT", "1")
	t.Setenv("CODEINTEL_SCIP_GRAPH_FILE_LIMIT", "0")

	if got := scipOccurrenceCap(); got != 250000 {
		t.Fatalf("occurrence cap=%d want 250000", got)
	}
	if got := scipSymbolRoleCap(); got != 8 {
		t.Fatalf("symbol role cap=%d want 8", got)
	}
	if got := scipFileCap(); got != defaultSCIPFileCap {
		t.Fatalf("file cap=%d want default %d", got, defaultSCIPFileCap)
	}
}

func TestHandleHeartbeatsCoreSubjobBeforeExecuting(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	workspaceID := "atom-workspace-1"
	payload := indexsubjobtask.Payload{
		SubjobID:          "graph-merge-1",
		RepoIndexingJobID: "index-job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             indexsubjobtask.LayerGraphMerge,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
		Attempt:           1,
	}
	raw, err := indexsubjobtask.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	store := &fakeCoreStore{claimOK: true, heartbeatOK: true, failedOK: true}
	h, err := NewHandler(mock, store, slog.New(slog.NewTextHandler(io.Discard, nil)), Config{
		LeaseOwner:        "core-test",
		LeaseDuration:     time.Minute,
		HeartbeatInterval: 10 * time.Millisecond,
		Now:               func() time.Time { return time.Date(2026, 5, 28, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	mock.ExpectQuery(`SELECT id\s+FROM "CodeGraphIndex"`).
		WithArgs(payload.OrgID, payload.RepoID, workspaceID, payload.Revision, payload.CommitHash, defaultSchemaVersion, defaultBuilder).
		WillReturnError(context.Canceled)

	err = h.Handle(context.Background(), asynq.NewTask(payload.QueueName, raw))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if store.claims != 1 {
		t.Fatalf("claims=%d want 1", store.claims)
	}
	if store.heartbeats == 0 {
		t.Fatalf("core subjob did not heartbeat")
	}
	if store.failures != 1 {
		t.Fatalf("failures=%d want 1", store.failures)
	}
	if store.successes != 0 {
		t.Fatalf("successes=%d want 0", store.successes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func scipTestEdge(payload indexsubjobtask.Payload, from, to string) graphschema.CodeGraphEdge {
	fromVID := codeGraphVID(payload, "symbol", from)
	toVID := codeGraphVID(payload, "symbol", to)
	return graphschema.CodeGraphEdge{
		FromVID: fromVID,
		ToVID:   toVID,
		Kind:    "CALLS",
		Rank:    graphschema.EdgeRank(fromVID + "->" + toVID + ":CALLS:scip"),
		Properties: scopedProps(payload, map[string]graphschema.CodeGraphPrimitive{
			"kind":          "CALLS",
			"normalizedKey": from + "->" + to,
			"confidence":    0.95,
			"source":        "scip",
			"provenance":    "scip",
			"context":       "call",
		}),
	}
}

func snapshotHasEdge(snapshot graphschema.CodeGraphSnapshot, kind, context string) bool {
	for _, edge := range snapshot.Edges {
		if edge.Kind == kind && edge.Properties["context"] == context && edge.Properties["source"] == "scip" && edge.Properties["provenance"] == "scip" {
			return true
		}
	}
	return false
}

func snapshotHasVertexKind(snapshot graphschema.CodeGraphSnapshot, kind string) bool {
	for _, vertex := range snapshot.Vertices {
		if vertex.Kind == kind && vertex.Properties["source"] == "scip" && vertex.Properties["provenance"] == "scip" {
			return true
		}
	}
	return false
}

func anyArgs(count int) []any {
	args := make([]any, count)
	for i := range args {
		args[i] = pgxmock.AnyArg()
	}
	return args
}

type fakeGraphWriter struct {
	calls         int
	maxStatements int
}

func (f *fakeGraphWriter) WriteRenderedStatements(_ context.Context, input graphstore.RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error) {
	f.calls++
	if len(input.Statements) > f.maxStatements {
		f.maxStatements = len(input.Statements)
	}
	return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusReady}, nil
}

type fakeCoreStore struct {
	claimOK     bool
	heartbeatOK bool
	successOK   bool
	failedOK    bool
	claims      int
	heartbeats  int
	successes   int
	failures    int
}

func (f *fakeCoreStore) ClaimScoped(context.Context, indexsubjobs.ClaimScope, string, string, time.Time) (bool, error) {
	f.claims++
	return f.claimOK, nil
}

func (f *fakeCoreStore) Heartbeat(context.Context, string, string, string, time.Time) (bool, error) {
	f.heartbeats++
	return f.heartbeatOK, nil
}

func (f *fakeCoreStore) MarkSucceeded(context.Context, string, string, string) (bool, error) {
	f.successes++
	return f.successOK, nil
}

func (f *fakeCoreStore) MarkFailed(context.Context, string, string, string, string, string) (bool, error) {
	f.failures++
	return f.failedOK, nil
}
