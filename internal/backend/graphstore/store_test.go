package graphstore

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"codeintel/pkg/graphschema"
	nebula "github.com/vesoft-inc/nebula-go/v3"
)

// fakeExecutor is a hand-rolled NgqlExecutor for tests. It
// records every statement issued and returns either a fixed
// success (nil error, nil ResultSet) or a fixed error keyed by
// statement-substring matchers.
type fakeExecutor struct {
	mu    sync.Mutex
	calls []string

	// errorFor lets a test inject an error for any statement
	// whose substring matches the key. Keys are checked in
	// insertion order — first match wins.
	errorFor []errSubmatch

	// resultFor pins a ResultSet for a substring-matched call.
	// nil ResultSet returned otherwise.
	resultFor map[string]*nebula.ResultSet
}

type errSubmatch struct {
	substr string
	err    error
}

func (f *fakeExecutor) Execute(_ context.Context, stmt string) (*nebula.ResultSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, stmt)
	for _, sm := range f.errorFor {
		if strings.Contains(stmt, sm.substr) {
			return nil, sm.err
		}
	}
	for sub, rs := range f.resultFor {
		if strings.Contains(stmt, sub) {
			return rs, nil
		}
	}
	return nil, nil
}

func (f *fakeExecutor) callsContaining(sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, s := range f.calls {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func newTestStore(exec *fakeExecutor) *NebulaCodeGraphStore {
	return NewWithRetry(exec, slog.New(slog.NewTextHandler(io.Discard, nil)), SchemaRetryConfig{
		Attempts: 3,
		Delay:    1 * time.Millisecond,
	})
}

// TestIsSchemaPropagationError locks the propagation-error
// classifier the retry loop gates on. Mirrors the TS regex
// `/schema|tag|edge|space/i` from store.ts:197.
func TestIsSchemaPropagationError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain text", errors.New("connection refused"), false},
		{"contains schema", errors.New("schema not yet propagated"), true},
		{"contains tag (case-insensitive)", errors.New("TAG not found"), true},
		{"contains edge", errors.New("edge type missing"), true},
		{"contains space", errors.New("Space `codeintel` was not chosen"), true},
		{"unrelated 'sage'", errors.New("usage exceeded"), false}, // doesn't contain any of the keywords
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSchemaPropagationError(tc.err); got != tc.want {
				t.Errorf("isSchemaPropagationError(%v): got %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestCountLinkedEdges locks the anchor-linker filter the writer
// uses to report linked-edge count on every successful write.
// Edges with `source != "anchor-linker"` are NOT counted.
func TestCountLinkedEdges(t *testing.T) {
	snap := graphschema.CodeGraphSnapshot{
		Edges: []graphschema.CodeGraphEdge{
			{Properties: map[string]graphschema.CodeGraphPrimitive{"source": "anchor-linker"}},
			{Properties: map[string]graphschema.CodeGraphPrimitive{"source": "anchor-linker"}},
			{Properties: map[string]graphschema.CodeGraphPrimitive{"source": "ast-extractor"}},
			{Properties: map[string]graphschema.CodeGraphPrimitive{"source": nil}},
			{Properties: map[string]graphschema.CodeGraphPrimitive{}}, // missing source key
		},
	}
	if got := countLinkedEdges(snap); got != 2 {
		t.Errorf("countLinkedEdges: got %d, want 2", got)
	}
}

// TestShortCommit covers the >12-char truncation path used in
// log messages.
func TestShortCommit(t *testing.T) {
	if got := shortCommit("deadbeef"); got != "deadbeef" {
		t.Errorf("short hash: got %q", got)
	}
	if got := shortCommit("deadbeefcafe1234"); got != "deadbeefcafe" {
		t.Errorf("long hash truncation: got %q", got)
	}
	if got := shortCommit(""); got != "" {
		t.Errorf("empty: got %q", got)
	}
}

// TestExecuteWithSchemaRetry_RetriesOnPropagationError confirms
// the retry loop honours the propagation classifier: the first
// two calls return a "schema not ready" error; the third
// succeeds. The store should swallow the first two errors and
// return success.
func TestExecuteWithSchemaRetry_RetriesOnPropagationError(t *testing.T) {
	attempt := 0
	exec := &flakeyExecutor{
		execFn: func(stmt string) (*nebula.ResultSet, error) {
			attempt++
			if attempt < 3 {
				return nil, errors.New("schema propagation in progress")
			}
			return nil, nil
		},
	}
	store := newTestStore(nil)
	store.executor = exec
	_, err := store.executeWithSchemaRetry(context.Background(), "DESCRIBE TAG `x`;")
	if err != nil {
		t.Errorf("expected success after retries, got %v", err)
	}
	if attempt != 3 {
		t.Errorf("expected 3 attempts, got %d", attempt)
	}
}

// TestExecuteWithSchemaRetry_NonPropagationErrorFailsFast confirms
// errors NOT matching the propagation classifier bypass the
// retry loop entirely — a single attempt, then surface.
func TestExecuteWithSchemaRetry_NonPropagationErrorFailsFast(t *testing.T) {
	attempt := 0
	exec := &flakeyExecutor{
		execFn: func(stmt string) (*nebula.ResultSet, error) {
			attempt++
			return nil, errors.New("connection refused")
		},
	}
	store := newTestStore(nil)
	store.executor = exec
	_, err := store.executeWithSchemaRetry(context.Background(), "X")
	if err == nil {
		t.Errorf("expected error to surface immediately")
	}
	if attempt != 1 {
		t.Errorf("expected 1 attempt (no retry), got %d", attempt)
	}
}

// TestExecuteWithSchemaRetry_ExhaustsBudget confirms an
// always-propagation-error path exhausts the retry budget and
// surfaces the final error.
func TestExecuteWithSchemaRetry_ExhaustsBudget(t *testing.T) {
	attempt := 0
	exec := &flakeyExecutor{
		execFn: func(stmt string) (*nebula.ResultSet, error) {
			attempt++
			return nil, errors.New("schema propagation in progress")
		},
	}
	store := newTestStore(nil)
	store.executor = exec
	_, err := store.executeWithSchemaRetry(context.Background(), "X")
	if err == nil {
		t.Errorf("expected error after budget exhaustion")
	}
	if attempt != 3 {
		t.Errorf("expected 3 attempts (budget), got %d", attempt)
	}
}

// TestExecuteWithSchemaRetry_RespectsContext confirms a
// cancelled context short-circuits the retry-delay sleep and
// returns ctx.Err().
func TestExecuteWithSchemaRetry_RespectsContext(t *testing.T) {
	exec := &flakeyExecutor{
		execFn: func(stmt string) (*nebula.ResultSet, error) {
			return nil, errors.New("schema propagation in progress")
		},
	}
	store := newTestStore(nil)
	store.executor = exec
	store.retry.Delay = 1 * time.Hour // would deadlock without ctx cancel

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.executeWithSchemaRetry(ctx, "X")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestMarkSnapshotForDeletion_EmptyResultSetNoOp confirms a
// LOOKUP that resolves zero vids skips the DELETE phase
// entirely. Mirrors store.ts:128-130.
func TestMarkSnapshotForDeletion_EmptyResultSetNoOp(t *testing.T) {
	exec := &fakeExecutor{} // returns nil ResultSet for every call
	store := newTestStore(exec)

	err := store.MarkSnapshotForDeletion(context.Background(), graphschema.CodeGraphDeleteInput{
		OrgID:          1,
		WorkspaceID:    "ws",
		RepoID:         1,
		CommitHash:     "abc",
		SchemaVersion:  1,
		BuilderVersion: "v",
	})
	if err != nil {
		t.Errorf("expected nil error on empty LOOKUP, got %v", err)
	}
	if got := exec.callsContaining("DELETE VERTEX"); got != 0 {
		t.Errorf("expected 0 DELETE statements when LOOKUP returns 0 vids, got %d", got)
	}
}

// TestMarkSnapshotForDeletion_LookupFailureSurfaces confirms a
// LOOKUP transport error propagates rather than masquerading as
// a no-op.
func TestMarkSnapshotForDeletion_LookupFailureSurfaces(t *testing.T) {
	exec := &fakeExecutor{
		errorFor: []errSubmatch{{substr: "LOOKUP", err: errors.New("transport reset")}},
	}
	store := newTestStore(exec)
	err := store.MarkSnapshotForDeletion(context.Background(), graphschema.CodeGraphDeleteInput{
		OrgID: 1, WorkspaceID: "ws", RepoID: 1, CommitHash: "x", SchemaVersion: 1, BuilderVersion: "v",
	})
	if err == nil {
		t.Errorf("expected error to surface")
	}
	if !strings.Contains(err.Error(), "LOOKUP for retirement") {
		t.Errorf("expected wrapped LOOKUP-for-retirement error, got %v", err)
	}
}

// TestWriteSnapshot_EnsureSchemaFailureSurfacesFAILED confirms
// the Go-only additive return path: when ensureSchema's first
// statement returns a non-propagation error, WriteSnapshot
// returns (Status=FAILED, ErrorMessage!="") AND an error.
// The TS source threw at this point; the Go port surfaces both
// the typed status and the underlying error so the caller can
// branch on either.
func TestWriteSnapshot_EnsureSchemaFailureSurfacesFAILED(t *testing.T) {
	exec := &fakeExecutor{
		errorFor: []errSubmatch{{substr: "CREATE TAG", err: errors.New("connection refused")}},
	}
	store := newTestStore(exec)
	result, err := store.WriteSnapshot(context.Background(), graphschema.CodeGraphSnapshot{})
	if err == nil {
		t.Fatalf("expected error from WriteSnapshot")
	}
	if result.Status != graphschema.WriteStatusFailed {
		t.Errorf("Status: got %q, want FAILED", result.Status)
	}
	if result.ErrorMessage == "" {
		t.Errorf("expected non-empty ErrorMessage on FAILED")
	}
	if !strings.Contains(result.ErrorMessage, "schema create") {
		t.Errorf("ErrorMessage should wrap schema-create context: %s", result.ErrorMessage)
	}
}

// TestWriteSnapshot_HappyPathSkipsSchemaStatements confirms the
// schema-statement-skip math: the rendered statements list
// starts with len(RenderSchemaStatements()) entries that
// WriteSnapshot must skip (already applied by ensureSchema).
// A regression that off-by-one'd the skip would re-issue
// schema statements (harmless but wasteful) OR drop the first
// vertex INSERT (silent data loss).
//
// The test counts how many INSERT VERTEX statements landed on
// the executor — exactly 1 (one batch of two vertices).
func TestWriteSnapshot_HappyPathSkipsSchemaStatements(t *testing.T) {
	exec := &fakeExecutor{} // every Execute returns (nil, nil) — success
	store := newTestStore(exec)
	snapshot := graphschema.CodeGraphSnapshot{
		Vertices: []graphschema.CodeGraphVertex{
			{VID: "v-1", Properties: map[string]graphschema.CodeGraphPrimitive{"kind": "symbol"}},
			{VID: "v-2", Properties: map[string]graphschema.CodeGraphPrimitive{"kind": "file"}},
		},
		Edges: []graphschema.CodeGraphEdge{
			{
				FromVID: "v-1", ToVID: "v-2", Rank: 1,
				Properties: map[string]graphschema.CodeGraphPrimitive{"kind": "DEFINES", "source": "anchor-linker"},
			},
		},
	}
	result, err := store.WriteSnapshot(context.Background(), snapshot)
	if err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}
	if result.Status != graphschema.WriteStatusReady {
		t.Errorf("Status: got %q, want READY", result.Status)
	}
	if result.VertexCount != 2 || result.EdgeCount != 1 || result.LinkedEdgeCount != 1 {
		t.Errorf("counts: got vertex=%d edge=%d linked=%d, want 2/1/1", result.VertexCount, result.EdgeCount, result.LinkedEdgeCount)
	}
	// Skip-math invariant: exactly 1 INSERT VERTEX + 1 INSERT
	// EDGE land on the executor (schema statements happen
	// during ensureSchema's earlier loop — they're a different
	// set of issuances).
	if got := exec.callsContaining("INSERT VERTEX"); got != 1 {
		t.Errorf("INSERT VERTEX count: got %d, want 1", got)
	}
	if got := exec.callsContaining("INSERT EDGE"); got != 1 {
		t.Errorf("INSERT EDGE count: got %d, want 1", got)
	}
	// And the schema bootstrap loop should have issued one create
	// statement per RenderSchemaStatements entry before the snapshot writes. Use full-prefix
	// substrings to distinguish CREATE TAG vs CREATE TAG INDEX
	// (both contain the substring "CREATE TAG").
	if got := exec.callsContaining("CREATE TAG IF NOT EXISTS `code_graph_node`("); got != 1 {
		t.Errorf("CREATE TAG (the node tag) count: got %d, want 1", got)
	}
	if got := exec.callsContaining("CREATE EDGE IF NOT EXISTS `code_graph_edge`("); got != 1 {
		t.Errorf("CREATE EDGE (the edge type) count: got %d, want 1", got)
	}
	if got := exec.callsContaining("CREATE TAG INDEX"); got != 5 {
		t.Errorf("CREATE TAG INDEX count: got %d, want 5", got)
	}
	// Plus the 2 DESCRIBE statements ensureSchema issues for the
	// missing-prop diff pass.
	if got := exec.callsContaining("DESCRIBE TAG"); got != 1 {
		t.Errorf("DESCRIBE TAG count: got %d, want 1", got)
	}
	if got := exec.callsContaining("DESCRIBE EDGE"); got != 1 {
		t.Errorf("DESCRIBE EDGE count: got %d, want 1", got)
	}
}

// TestWriteRenderedStatements_HappyPathValidatesAndExecutesInserts
// covers the R.9l Rust handoff: the store receives a schema-prefixed
// statement list, validates the scope embedded in every row, skips the
// schema prefix, and executes only INSERT chunks through the Nebula
// executor.
func TestWriteRenderedStatements_HappyPathValidatesAndExecutesInserts(t *testing.T) {
	exec := &fakeExecutor{}
	store := newTestStore(exec)
	input := renderedFixtureInput()

	result, err := store.WriteRenderedStatements(context.Background(), input)
	if err != nil {
		t.Fatalf("WriteRenderedStatements: %v", err)
	}
	if result.Status != graphschema.WriteStatusReady {
		t.Errorf("Status: got %q, want READY", result.Status)
	}
	if result.VertexCount != 2 || result.EdgeCount != 1 {
		t.Errorf("counts: got vertices=%d edges=%d, want 2/1", result.VertexCount, result.EdgeCount)
	}
	if got := exec.callsContaining("INSERT VERTEX"); got != 1 {
		t.Errorf("INSERT VERTEX executions: got %d, want 1", got)
	}
	if got := exec.callsContaining("INSERT EDGE"); got != 1 {
		t.Errorf("INSERT EDGE executions: got %d, want 1", got)
	}
}

// TestWriteRenderedStatements_RejectsScopeMismatch proves a
// malicious/stale queue payload cannot claim org A in metadata
// while carrying org B rows inside the NGQL itself.
func TestWriteRenderedStatements_RejectsScopeMismatch(t *testing.T) {
	exec := &fakeExecutor{}
	store := newTestStore(exec)
	input := renderedFixtureInput()
	input.Statements[len(graphschema.RenderSchemaStatements())] = strings.Replace(input.Statements[len(graphschema.RenderSchemaStatements())], `"ws-1"`, `"other-ws"`, 1)

	result, err := store.WriteRenderedStatements(context.Background(), input)
	if !errors.Is(err, ErrRenderedStatementValidation) {
		t.Fatalf("got %v, want ErrRenderedStatementValidation", err)
	}
	if result.Status != graphschema.WriteStatusFailed {
		t.Errorf("Status: got %q, want FAILED", result.Status)
	}
	if got := exec.callsContaining("INSERT VERTEX"); got != 0 {
		t.Errorf("poison payload executed INSERT VERTEX %d times", got)
	}
}

// TestWriteRenderedStatements_RejectsVIDScopeMismatch covers the
// second half of the row validation contract: payload scope must match
// both row properties and the deterministic scoped VIDs/endpoints.
func TestWriteRenderedStatements_RejectsVIDScopeMismatch(t *testing.T) {
	exec := &fakeExecutor{}
	store := newTestStore(exec)
	input := renderedFixtureInput()
	insertIdx := len(graphschema.RenderSchemaStatements())
	input.Statements[insertIdx] = strings.Replace(input.Statements[insertIdx], `cg:o7:`, `cg:o8:`, 1)

	result, err := store.WriteRenderedStatements(context.Background(), input)
	if !errors.Is(err, ErrRenderedStatementValidation) {
		t.Fatalf("got %v, want ErrRenderedStatementValidation", err)
	}
	if result.Status != graphschema.WriteStatusFailed {
		t.Errorf("Status: got %q, want FAILED", result.Status)
	}
	if got := exec.callsContaining("INSERT VERTEX"); got != 0 {
		t.Errorf("poison payload executed INSERT VERTEX %d times", got)
	}
}

// TestWriteRenderedStatements_RejectsEdgeRankMismatch proves the
// rendered edge endpoint/rank identity is recomputed in Go before
// Nebula execution, matching the Rust builder's legacy rank input.
func TestWriteRenderedStatements_RejectsEdgeRankMismatch(t *testing.T) {
	exec := &fakeExecutor{}
	store := newTestStore(exec)
	input := renderedFixtureInput()
	edgeIdx := len(input.Statements) - 1
	input.Statements[edgeIdx] = strings.Replace(input.Statements[edgeIdx], `@`, `@1`, 1)

	_, err := store.WriteRenderedStatements(context.Background(), input)
	if !errors.Is(err, ErrRenderedStatementValidation) {
		t.Fatalf("got %v, want ErrRenderedStatementValidation", err)
	}
	if got := exec.callsContaining("INSERT EDGE"); got != 0 {
		t.Errorf("poison payload executed INSERT EDGE %d times", got)
	}
}

// TestWriteRenderedStatements_RejectsNonInsertPayload locks the
// statement envelope: after the schema prefix only code graph
// INSERT VERTEX / INSERT EDGE statements are legal.
func TestWriteRenderedStatements_RejectsNonInsertPayload(t *testing.T) {
	exec := &fakeExecutor{}
	store := newTestStore(exec)
	input := renderedFixtureInput()
	input.Statements = append(input.Statements, "DROP SPACE `codeintel`;")

	_, err := store.WriteRenderedStatements(context.Background(), input)
	if !errors.Is(err, ErrRenderedStatementValidation) {
		t.Fatalf("got %v, want ErrRenderedStatementValidation", err)
	}
	if got := exec.callsContaining("DROP SPACE"); got != 0 {
		t.Errorf("poison payload executed DROP SPACE %d times", got)
	}
}

func TestExtractRenderedRowsReturnsValidatedVerticesAndEdges(t *testing.T) {
	input := renderedFixtureInput()
	vertices, edges, err := ExtractRenderedRows(input)
	if err != nil {
		t.Fatalf("ExtractRenderedRows: %v", err)
	}
	if len(vertices) != 2 || len(edges) != 1 {
		t.Fatalf("rows: vertices=%d edges=%d", len(vertices), len(edges))
	}
	if vertices[1].Props["kind"] != "file" || vertices[1].Props["revision"] != "refs/heads/main" {
		t.Fatalf("vertex props not decoded: %+v", vertices[1])
	}
	if edges[0].Props["kind"] != "CONTAINS" || edges[0].Props["source"] != "syntactic-ast" {
		t.Fatalf("edge props not decoded: %+v", edges[0])
	}
	if edges[0].FromVID == "" || edges[0].ToVID == "" || edges[0].Rank == "" {
		t.Fatalf("edge identity incomplete: %+v", edges[0])
	}
}

// TestExtractSchemaPropertyNames_NilSafe confirms a nil ResultSet
// returns an empty (non-nil) set rather than panicking.
func TestExtractSchemaPropertyNames_NilSafe(t *testing.T) {
	got := extractSchemaPropertyNames(nil)
	if got == nil {
		t.Errorf("expected non-nil empty map")
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %d entries", len(got))
	}
}

// flakeyExecutor lets a test inject per-call behaviour. Used by
// the retry-loop tests above.
type flakeyExecutor struct {
	execFn func(stmt string) (*nebula.ResultSet, error)
}

func (f *flakeyExecutor) Execute(_ context.Context, stmt string) (*nebula.ResultSet, error) {
	return f.execFn(stmt)
}

func renderedFixtureInput() RenderedStatementWrite {
	scope := RenderedStatementWrite{
		OrgID:          7,
		WorkspaceID:    "ws-1",
		RepoID:         42,
		Revision:       "refs/heads/main",
		CommitHash:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SchemaVersion:  1,
		BuilderVersion: "codeintel-code-graph-v7",
		Source:         "syntactic-ast",
	}
	repoVID := testScopedVID(scope, "repo", strings.Repeat("a", 32))
	fileVID := testScopedVID(scope, "file", strings.Repeat("b", 32))
	v1 := graphschema.CodeGraphVertex{
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
		},
	}
	v2 := graphschema.CodeGraphVertex{
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
		},
	}
	e := graphschema.CodeGraphEdge{
		FromVID: repoVID,
		ToVID:   fileVID,
		Rank:    graphschema.EdgeRank(repoVID + "->" + fileVID + ":CONTAINS:syntactic-ast"),
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
			"source":         "syntactic-ast",
		},
	}
	scope.Statements = graphschema.RenderSnapshotStatements(graphschema.CodeGraphSnapshot{
		Vertices: []graphschema.CodeGraphVertex{v1, v2},
		Edges:    []graphschema.CodeGraphEdge{e},
	})
	return scope
}

func testScopedVID(scope RenderedStatementWrite, kind, keyHash string) string {
	return strings.Join([]string{
		"cg",
		"o" + strconv.FormatInt(scope.OrgID, 10),
		"w" + hashParts([]string{scope.WorkspaceID}, 8),
		"r" + strconv.FormatInt(scope.RepoID, 10),
		"c" + scope.CommitHash[:12],
		"s" + strconv.FormatInt(scope.SchemaVersion, 10),
		"b" + hashParts([]string{scope.BuilderVersion}, 8),
		kind,
		keyHash,
	}, ":")
}
