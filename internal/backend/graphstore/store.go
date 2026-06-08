// Package graphstore is the codeintel-backend-owned graph
// writer + retire path. Direct port of
// packages/backend/src/codeGraph/store.ts — specifically the
// NebulaNgqlCodeGraphStore class (lines 102-137) and its
// supporting helpers (lines 164-258).
//
// The package lives under internal/backend because only the
// codeintel-backend binary instantiates a real store; the
// codeintel-app read path uses pkg/nebulaclient + the renderers
// directly via the future graph_* MCP tools. Promoting the
// CodeGraphStore interface to pkg/ is a follow-up slice if a
// second binary needs to consume it.
//
// Three pieces NOT ported here (parity-skipped for slice
// boundaries documented in progress.md):
//
//   - NebulaClientNgqlExecutor (store.ts:52-100): the TS source's
//     hand-rolled JS-client wrapper. pkg/nebulaclient.Client
//     already plays this role.
//   - UnconfiguredCodeGraphStore (store.ts:17-34): no-op store
//     for the unconfigured-deployment path. Ports with the
//     codeintel-backend wiring slice.
//   - createCodeGraphStore (store.ts:139-158): the env-driven
//     factory. Ports with the codeintel-backend wiring slice.
package graphstore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"codeintel/pkg/graphschema"
	nebula "github.com/vesoft-inc/nebula-go/v3"
)

// NgqlExecutor is the minimal nGQL execution surface the store
// depends on. *nebulaclient.Client satisfies it directly; tests
// inject a fake.
//
// Mirrors TS source `export type NgqlExecutor = { execute(...) }`
// (store.ts:36-38). The Go interface adds a context argument
// because every dependent network call in the codeintel
// codebase is context-bound.
type NgqlExecutor interface {
	Execute(ctx context.Context, stmt string) (*nebula.ResultSet, error)
}

// SchemaRetryConfig controls executeWithSchemaRetry behaviour.
// Mirrors the TS source's two env-driven knobs at store.ts:174-175
// (attempts default 30; delay default 1000 ms). The Go port
// surfaces these as a struct rather than env vars so the factory
// slice picks the wiring policy.
type SchemaRetryConfig struct {
	Attempts int
	Delay    time.Duration
}

// DefaultSchemaRetryConfig matches the TS defaults verbatim.
func DefaultSchemaRetryConfig() SchemaRetryConfig {
	return SchemaRetryConfig{Attempts: 30, Delay: time.Second}
}

// NebulaCodeGraphStore is the production write + retire
// implementation. Mirrors the TS NebulaNgqlCodeGraphStore class.
//
// WriteSnapshot persists a snapshot's vertices + edges; the
// schema is ensured idempotently on every call so a fresh
// cluster doesn't need a separate bootstrap step.
//
// MarkSnapshotForDeletion resolves every vertex matching the
// supplied scope tuple, then issues chunked DELETE VERTEX ...
// WITH EDGE statements. Empty result-set short-circuits to a
// no-op.
type NebulaCodeGraphStore struct {
	executor NgqlExecutor
	logger   *slog.Logger
	retry    SchemaRetryConfig
}

// New constructs a NebulaCodeGraphStore. The supplied executor
// must already be connected to the codeintel space (the slice
// that wires this in production also issues the CREATE SPACE +
// USE prefix via pkg/graphschema.Bootstrap before constructing
// the store).
func New(executor NgqlExecutor, logger *slog.Logger) *NebulaCodeGraphStore {
	return NewWithRetry(executor, logger, DefaultSchemaRetryConfig())
}

// Compile-time check: *NebulaCodeGraphStore satisfies the
// graphschema.CodeGraphStore interface. A future refactor that
// renames or re-types a method surfaces at build time.
var _ graphschema.CodeGraphStore = (*NebulaCodeGraphStore)(nil)
var _ Store = (*NebulaCodeGraphStore)(nil)

// NewWithRetry is the test-friendly constructor that lets the
// caller override the retry budget.
func NewWithRetry(executor NgqlExecutor, logger *slog.Logger, retry SchemaRetryConfig) *NebulaCodeGraphStore {
	if logger == nil {
		logger = slog.Default()
	}
	if retry.Attempts <= 0 {
		retry.Attempts = DefaultSchemaRetryConfig().Attempts
	}
	if retry.Delay <= 0 {
		retry.Delay = DefaultSchemaRetryConfig().Delay
	}
	return &NebulaCodeGraphStore{
		executor: executor,
		logger:   logger.With("logger", "graphstore"),
		retry:    retry,
	}
}

// WriteSnapshot ensures the schema is up-to-date, then executes
// every INSERT VERTEX / INSERT EDGE statement
// pkg/graphschema.RenderSnapshotStatements emits. The schema
// statements at the head of that result are skipped (already
// applied by ensureSchema). Direct port of the TS body at
// store.ts:108-123.
func (s *NebulaCodeGraphStore) WriteSnapshot(ctx context.Context, snapshot graphschema.CodeGraphSnapshot) (graphschema.CodeGraphWriteResult, error) {
	if err := s.ensureSchema(ctx); err != nil {
		return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
	}

	statements := graphschema.RenderSnapshotStatements(snapshot)
	skip := len(graphschema.RenderSchemaStatements())
	for _, stmt := range statements[skip:] {
		if _, err := s.executeWithSchemaRetry(ctx, stmt); err != nil {
			return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
		}
	}

	s.logger.Info("wrote code graph snapshot",
		"repoId", snapshot.RepoID,
		"commitHash", shortCommit(snapshot.CommitHash),
		"vertices", len(snapshot.Vertices),
		"edges", len(snapshot.Edges),
	)
	return graphschema.CodeGraphWriteResult{
		Status:          graphschema.WriteStatusReady,
		VertexCount:     int64(len(snapshot.Vertices)),
		EdgeCount:       int64(len(snapshot.Edges)),
		AnchorCount:     int64(len(snapshot.Anchors)),
		LinkedEdgeCount: countLinkedEdges(snapshot),
	}, nil
}

// MarkSnapshotForDeletion is the snapshot-retirement path:
// resolve every vid for the supplied scope, then chunked DELETE
// VERTEX ... WITH EDGE. Direct port of store.ts:125-136.
//
// Caller MUST source the snapshot input from an
// authentication-validated request context; the store trusts
// the tuple verbatim — see CodeGraphDeleteInput's docstring.
func (s *NebulaCodeGraphStore) MarkSnapshotForDeletion(ctx context.Context, input graphschema.CodeGraphDeleteInput) error {
	rs, err := s.executor.Execute(ctx, graphschema.RenderLookupSnapshotVerticesStatement(input))
	if err != nil {
		return fmt.Errorf("graphstore: LOOKUP for retirement: %w", err)
	}
	vids := extractVids(rs)
	if len(vids) == 0 {
		return nil
	}

	for _, stmt := range graphschema.RenderDeleteVerticesStatements(vids) {
		if _, err := s.executor.Execute(ctx, stmt); err != nil {
			return fmt.Errorf("graphstore: DELETE VERTEX: %w", err)
		}
	}
	s.logger.Info("deleted code graph snapshot",
		"repoId", input.RepoID,
		"commitHash", shortCommit(input.CommitHash),
		"vertices", len(vids),
	)
	return nil
}

// ensureSchema runs the schema-creation statements
// idempotently, then diffs the live tag/edge column set against
// the binary's expected NodeProps/EdgeProps and ALTERs in any
// missing columns. Direct port of ensureNebulaGraphSchema
// (store.ts:200-221).
func (s *NebulaCodeGraphStore) ensureSchema(ctx context.Context) error {
	for _, stmt := range graphschema.RenderSchemaStatements() {
		if _, err := s.executeWithSchemaRetry(ctx, stmt); err != nil {
			return fmt.Errorf("graphstore: schema create: %w", err)
		}
	}

	if err := s.ensureMissingSchemaProperties(ctx, schemaPropDiff{
		expected:        graphschema.NodeProps,
		describeStmt:    graphschema.RenderDescribeTagStatement(),
		renderAlterStmt: graphschema.RenderAlterTagAddStatement,
		label:           "tag",
	}); err != nil {
		return err
	}
	if err := s.ensureMissingSchemaProperties(ctx, schemaPropDiff{
		expected:        graphschema.EdgeProps,
		describeStmt:    graphschema.RenderDescribeEdgeStatement(),
		renderAlterStmt: graphschema.RenderAlterEdgeAddStatement,
		label:           "edge",
	}); err != nil {
		return err
	}
	return nil
}

// schemaPropDiff bundles the per-call arguments to
// ensureMissingSchemaProperties. Mirrors the inline TS options
// shape at store.ts:230-237.
type schemaPropDiff struct {
	expected        []string
	describeStmt    string
	renderAlterStmt func([]string) string
	label           string
}

// ensureMissingSchemaProperties DESCRIBEs the live schema, diffs
// the column set against `expected`, and ALTERs in any missing
// columns. Direct port of store.ts:223-251.
func (s *NebulaCodeGraphStore) ensureMissingSchemaProperties(ctx context.Context, d schemaPropDiff) error {
	rs, err := s.executeWithSchemaRetry(ctx, d.describeStmt)
	if err != nil {
		return fmt.Errorf("graphstore: DESCRIBE %s: %w", d.label, err)
	}
	existing := extractSchemaPropertyNames(rs)
	if len(existing) == 0 {
		// Empty result set means DESCRIBE returned no rows — the
		// schema doesn't exist yet. CREATE TAG / EDGE in the
		// earlier loop already added every prop, so there's
		// nothing to ALTER.
		return nil
	}
	var missing []string
	for _, prop := range d.expected {
		if _, ok := existing[prop]; !ok {
			missing = append(missing, prop)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if _, err := s.executeWithSchemaRetry(ctx, d.renderAlterStmt(missing)); err != nil {
		return fmt.Errorf("graphstore: ALTER %s: %w", d.label, err)
	}
	s.logger.Info("upgraded code graph schema with missing properties",
		"kind", d.label,
		"missing", strings.Join(missing, ", "),
	)
	return nil
}

// executeWithSchemaRetry wraps every schema-touching Execute in
// a bounded retry loop that swallows propagation-related errors
// only. Other errors surface immediately. Direct port of
// store.ts:173-194.
func (s *NebulaCodeGraphStore) executeWithSchemaRetry(ctx context.Context, stmt string) (*nebula.ResultSet, error) {
	var lastErr error
	for attempt := 1; attempt <= s.retry.Attempts; attempt++ {
		rs, err := s.executor.Execute(ctx, stmt)
		if err == nil {
			return rs, nil
		}
		if !isSchemaPropagationError(err) || attempt == s.retry.Attempts {
			return nil, err
		}
		lastErr = err
		s.logger.Warn("retrying graph statement after schema propagation delay",
			"attempt", attempt,
			"max_attempts", s.retry.Attempts,
			"err", err,
		)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(s.retry.Delay):
		}
	}
	// Unreachable — the loop's final iteration surfaces the
	// error via the `attempt == s.retry.Attempts` branch — but
	// the compiler can't prove it, so a defensive return covers
	// the path.
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("graphstore: schema retry exhausted without recorded error")
}

// isSchemaPropagationError is the TS source's regex-based
// classifier (store.ts:196-198): "schema|tag|edge|space" case-
// insensitive on the error message. Nebula's metad → storaged
// propagation surfaces these substrings on transient
// "schema not yet visible" errors that resolve after a
// heartbeat interval.
func isSchemaPropagationError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "schema") ||
		strings.Contains(msg, "tag") ||
		strings.Contains(msg, "edge") ||
		strings.Contains(msg, "space")
}

// countLinkedEdges returns the count of edges where the
// `source` property is `"anchor-linker"`. Direct port of the TS
// helper at store.ts:164-165. Linked-edge count is a separate
// metric from total edge count because the linker phase produces
// edges from anchors AFTER the main snapshot — operators want
// the breakdown visible on every write result.
func countLinkedEdges(snapshot graphschema.CodeGraphSnapshot) int64 {
	var n int64
	for _, e := range snapshot.Edges {
		if v, ok := e.Properties["source"].(string); ok && v == "anchor-linker" {
			n++
		}
	}
	return n
}

// extractSchemaPropertyNames parses a DESCRIBE TAG / DESCRIBE
// EDGE result set into the set of property names. Direct port
// of store.ts:253-258 — the TS source scans `result.data.Field`
// / `field` / `Name` / `name` because the JS client's
// ParsedResponse exposes the column-major form. The Go nebula-go
// client returns row-major data, so the port iterates rows and
// reads column 0 (the canonical DESCRIBE first-column-is-the-
// field-name contract).
//
// Returns a name → struct{} map (set semantics).
func extractSchemaPropertyNames(rs *nebula.ResultSet) map[string]struct{} {
	if rs == nil {
		return make(map[string]struct{})
	}
	out := make(map[string]struct{}, rs.GetRowSize())
	for i := 0; i < rs.GetRowSize(); i++ {
		rec, err := rs.GetRowValuesByIndex(i)
		if err != nil {
			continue
		}
		val, err := rec.GetValueByIndex(0)
		if err != nil {
			continue
		}
		s, sErr := val.AsString()
		if sErr != nil || s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

// extractVids parses a LOOKUP result set into the slice of
// resolved vids. Direct port of store.ts:260-267 — same
// column-vs-row API adaptation as extractSchemaPropertyNames.
// The LOOKUP's YIELD clause aliases the column to `vid`, so
// column 0 of each row is the vid string. nil-safe: a nil
// ResultSet returns an empty slice rather than panicking,
// matching the TS source's "data ?? []" tolerance.
func extractVids(rs *nebula.ResultSet) []string {
	if rs == nil {
		return nil
	}
	out := make([]string, 0, rs.GetRowSize())
	for i := 0; i < rs.GetRowSize(); i++ {
		rec, err := rs.GetRowValuesByIndex(i)
		if err != nil {
			continue
		}
		val, err := rec.GetValueByIndex(0)
		if err != nil {
			continue
		}
		s, sErr := val.AsString()
		if sErr != nil || s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// shortCommit truncates a commit hash to its 12-char prefix for
// log readability. Matches the TS template `commitHash.slice(0, 12)`
// at store.ts:115 / 135.
func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}
