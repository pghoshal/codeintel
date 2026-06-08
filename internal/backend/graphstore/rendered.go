package graphstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"codeintel/pkg/graphschema"
)

// Store is the backend graph writer contract. It extends the
// structured snapshot writer with the R.9l rendered-statement
// handoff used by codeintel-indexer-rs.
type Store interface {
	graphschema.CodeGraphStore
	WriteRenderedStatements(ctx context.Context, input RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error)
}

// RenderedStatementWrite is the validated scope + statement bag
// the Rust indexer enqueues on the code-graph-write queue.
type RenderedStatementWrite struct {
	OrgID          int64
	WorkspaceID    string
	RepoID         int64
	Revision       string
	CommitHash     string
	SchemaVersion  int64
	BuilderVersion string
	Source         string
	Statements     []string
}

type RenderedVertexRow struct {
	VID   string
	Props map[string]string
}

type RenderedEdgeRow struct {
	FromVID string
	ToVID   string
	Rank    string
	Props   map[string]string
}

// ErrRenderedStatementValidation marks poison payloads that
// should not be retried by asynq. Transport/schema propagation
// failures are returned without this sentinel so they retry.
var ErrRenderedStatementValidation = errors.New("graphstore: rendered statement validation")

// ValidateRenderedStatementWrite validates the pre-rendered NGQL
// emitted by Rust without touching Nebula or Postgres. Backend
// queue handlers call this before mutating graph metadata so a
// poison payload cannot downgrade an existing graph index.
func ValidateRenderedStatementWrite(input RenderedStatementWrite) (int64, int64, error) {
	return validateRenderedStatements(input)
}

func ExtractRenderedRows(input RenderedStatementWrite) ([]RenderedVertexRow, []RenderedEdgeRow, error) {
	if _, _, err := ValidateRenderedStatementWrite(input); err != nil {
		return nil, nil, err
	}
	schema := graphschema.RenderSchemaStatements()
	vertexPrefix := insertPrefix("VERTEX", graphschema.NodeTag, graphschema.NodeProps)
	edgePrefix := insertPrefix("EDGE", graphschema.EdgeType, graphschema.EdgeProps)
	var vertices []RenderedVertexRow
	var edges []RenderedEdgeRow
	for _, stmt := range input.Statements[len(schema):] {
		switch {
		case strings.HasPrefix(stmt, vertexPrefix):
			rows, err := parseInsertRows(strings.TrimSuffix(strings.TrimPrefix(stmt, vertexPrefix), ";"))
			if err != nil {
				return nil, nil, err
			}
			for _, row := range rows {
				parsed, err := parseRenderedVertexRow(row)
				if err != nil {
					return nil, nil, err
				}
				vertices = append(vertices, parsed)
			}
		case strings.HasPrefix(stmt, edgePrefix):
			rows, err := parseInsertRows(strings.TrimSuffix(strings.TrimPrefix(stmt, edgePrefix), ";"))
			if err != nil {
				return nil, nil, err
			}
			for _, row := range rows {
				parsed, err := parseRenderedEdgeRow(row)
				if err != nil {
					return nil, nil, err
				}
				edges = append(edges, parsed)
			}
		}
	}
	return vertices, edges, nil
}

// WriteRenderedStatements validates the pre-rendered NGQL emitted
// by Rust, ensures schema through the existing store path, then
// executes only INSERT chunks. The validation is deliberately
// strict: schema statements must byte-match Go's renderer, and
// every INSERT row must carry the same scope tuple as the payload.
func (s *NebulaCodeGraphStore) WriteRenderedStatements(ctx context.Context, input RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error) {
	vertexCount, edgeCount, err := ValidateRenderedStatementWrite(input)
	if err != nil {
		return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
	}
	if err := s.ensureSchema(ctx); err != nil {
		return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
	}

	skip := len(graphschema.RenderSchemaStatements())
	for _, stmt := range input.Statements[skip:] {
		if _, err := s.executeWithSchemaRetry(ctx, stmt); err != nil {
			return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
		}
	}

	s.logger.Info("wrote rendered code graph snapshot",
		"repoId", input.RepoID,
		"revision", input.Revision,
		"commitHash", shortCommit(input.CommitHash),
		"vertices", vertexCount,
		"edges", edgeCount,
		"source", input.Source,
	)
	return graphschema.CodeGraphWriteResult{
		Status:      graphschema.WriteStatusReady,
		VertexCount: vertexCount,
		EdgeCount:   edgeCount,
	}, nil
}

func parseRenderedVertexRow(row string) (RenderedVertexRow, error) {
	idx, err := indexOutside(row, ':')
	if err != nil {
		return RenderedVertexRow{}, err
	}
	vid, err := parseStringLiteral(strings.TrimSpace(row[:idx]))
	if err != nil {
		return RenderedVertexRow{}, err
	}
	values, err := parseInsertRowValues(row)
	if err != nil {
		return RenderedVertexRow{}, err
	}
	props := renderedProps(graphschema.NodeProps, values)
	return RenderedVertexRow{VID: vid, Props: props}, nil
}

func parseRenderedEdgeRow(row string) (RenderedEdgeRow, error) {
	idx, err := indexOutside(row, ':')
	if err != nil {
		return RenderedEdgeRow{}, err
	}
	target := strings.TrimSpace(row[:idx])
	arrowIdx, err := indexTokenOutside(target, "->")
	if err != nil {
		return RenderedEdgeRow{}, err
	}
	rankIdx, err := indexTokenOutside(target[arrowIdx+2:], "@")
	if err != nil {
		return RenderedEdgeRow{}, err
	}
	fromVID, err := parseStringLiteral(strings.TrimSpace(target[:arrowIdx]))
	if err != nil {
		return RenderedEdgeRow{}, err
	}
	targetAndRank := target[arrowIdx+2:]
	toVID, err := parseStringLiteral(strings.TrimSpace(targetAndRank[:rankIdx]))
	if err != nil {
		return RenderedEdgeRow{}, err
	}
	rank := strings.TrimSpace(targetAndRank[rankIdx+1:])
	values, err := parseInsertRowValues(row)
	if err != nil {
		return RenderedEdgeRow{}, err
	}
	props := renderedProps(graphschema.EdgeProps, values)
	return RenderedEdgeRow{FromVID: fromVID, ToVID: toVID, Rank: rank, Props: props}, nil
}

// edgePropsIndex returns the index of `name` in graphschema.EdgeProps
// or -1 when the column doesn't exist. Used by validators that need
// to read a known column by name rather than by absolute position
// (which would break whenever the schema grows — see Q.A's
// `provenance` / `context` appends).
func edgePropsIndex(name string) int {
	for i, p := range graphschema.EdgeProps {
		if p == name {
			return i
		}
	}
	return -1
}

func renderedProps(keys, values []string) map[string]string {
	props := make(map[string]string, len(keys))
	for i, key := range keys {
		if i >= len(values) {
			break
		}
		props[key] = decodeRenderedValue(values[i])
	}
	return props
}

func decodeRenderedValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "NULL") {
		return ""
	}
	if strings.HasPrefix(value, "\"") {
		decoded, err := parseStringLiteral(value)
		if err == nil {
			return decoded
		}
	}
	return value
}

func validateRenderedStatements(input RenderedStatementWrite) (int64, int64, error) {
	if err := validateRenderedScope(input); err != nil {
		return 0, 0, err
	}
	schema := graphschema.RenderSchemaStatements()
	if len(input.Statements) < len(schema) {
		return 0, 0, renderedValidation("expected at least %d schema statements, got %d", len(schema), len(input.Statements))
	}
	for i, want := range schema {
		if input.Statements[i] != want {
			return 0, 0, renderedValidation("schema statement %d mismatch", i)
		}
	}

	vertexPrefix := insertPrefix("VERTEX", graphschema.NodeTag, graphschema.NodeProps)
	edgePrefix := insertPrefix("EDGE", graphschema.EdgeType, graphschema.EdgeProps)
	var vertexCount, edgeCount int64
	for i, stmt := range input.Statements[len(schema):] {
		if err := validateSingleTerminatedStatement(stmt); err != nil {
			return 0, 0, renderedValidation("statement %d: %v", i+len(schema), err)
		}
		switch {
		case strings.HasPrefix(stmt, vertexPrefix):
			rows, err := parseInsertRows(strings.TrimSuffix(strings.TrimPrefix(stmt, vertexPrefix), ";"))
			if err != nil {
				return 0, 0, renderedValidation("vertex statement %d: %v", i+len(schema), err)
			}
			for _, row := range rows {
				values, err := parseInsertRowValues(row)
				if err != nil {
					return 0, 0, renderedValidation("vertex statement %d: %v", i+len(schema), err)
				}
				if err := validateVertexRowIdentity(row, values, input); err != nil {
					return 0, 0, renderedValidation("vertex statement %d: %v", i+len(schema), err)
				}
				if err := validateScopeValues(values, graphschema.NodeProps, input); err != nil {
					return 0, 0, renderedValidation("vertex statement %d: %v", i+len(schema), err)
				}
			}
			vertexCount += int64(len(rows))
		case strings.HasPrefix(stmt, edgePrefix):
			rows, err := parseInsertRows(strings.TrimSuffix(strings.TrimPrefix(stmt, edgePrefix), ";"))
			if err != nil {
				return 0, 0, renderedValidation("edge statement %d: %v", i+len(schema), err)
			}
			for _, row := range rows {
				values, err := parseInsertRowValues(row)
				if err != nil {
					return 0, 0, renderedValidation("edge statement %d: %v", i+len(schema), err)
				}
				if err := validateEdgeRowIdentity(row, values, input); err != nil {
					return 0, 0, renderedValidation("edge statement %d: %v", i+len(schema), err)
				}
				if err := validateScopeValues(values, graphschema.EdgeProps, input); err != nil {
					return 0, 0, renderedValidation("edge statement %d: %v", i+len(schema), err)
				}
			}
			edgeCount += int64(len(rows))
		default:
			return 0, 0, renderedValidation("statement %d is not an allowed graph INSERT", i+len(schema))
		}
	}
	return vertexCount, edgeCount, nil
}

func validateRenderedScope(input RenderedStatementWrite) error {
	if input.OrgID <= 0 {
		return renderedValidation("invalid orgId %d", input.OrgID)
	}
	if input.RepoID <= 0 {
		return renderedValidation("invalid repoId %d", input.RepoID)
	}
	if strings.TrimSpace(input.WorkspaceID) == "" {
		return renderedValidation("workspaceId is required")
	}
	if strings.TrimSpace(input.Revision) == "" {
		return renderedValidation("revision is required")
	}
	if !isHexCommit(input.CommitHash) {
		return renderedValidation("commitHash must be a 40-character SHA")
	}
	if input.SchemaVersion <= 0 {
		return renderedValidation("invalid schemaVersion %d", input.SchemaVersion)
	}
	if strings.TrimSpace(input.BuilderVersion) == "" {
		return renderedValidation("builderVersion is required")
	}
	if strings.TrimSpace(input.Source) == "" {
		return renderedValidation("source is required")
	}
	return nil
}

func validateScopeValues(values, props []string, input RenderedStatementWrite) error {
	if len(values) != len(props) {
		return fmt.Errorf("value count got %d, want %d", len(values), len(props))
	}
	want := map[string]string{
		"orgId":          strconv.FormatInt(input.OrgID, 10),
		"workspaceId":    renderStringLiteral(input.WorkspaceID),
		"repoId":         strconv.FormatInt(input.RepoID, 10),
		"revision":       renderStringLiteral(input.Revision),
		"commitHash":     renderStringLiteral(input.CommitHash),
		"schemaVersion":  strconv.FormatInt(input.SchemaVersion, 10),
		"builderVersion": renderStringLiteral(input.BuilderVersion),
	}
	for i, prop := range props {
		if expected, ok := want[prop]; ok && strings.TrimSpace(values[i]) != expected {
			return fmt.Errorf("scope property %s got %s, want %s", prop, strings.TrimSpace(values[i]), expected)
		}
	}
	return nil
}

func parseInsertRows(valuesPart string) ([]string, error) {
	rows, err := splitTopLevel(valuesPart, ',')
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("INSERT has no rows")
	}
	return rows, nil
}

func parseInsertRowValues(row string) ([]string, error) {
	idx, err := indexOutside(row, ':')
	if err != nil {
		return nil, err
	}
	valueTuple := strings.TrimSpace(row[idx+1:])
	if !strings.HasPrefix(valueTuple, "(") || !strings.HasSuffix(valueTuple, ")") {
		return nil, fmt.Errorf("row values are not a tuple: %s", row)
	}
	return splitTopLevel(valueTuple[1:len(valueTuple)-1], ',')
}

func validateVertexRowIdentity(row string, values []string, input RenderedStatementWrite) error {
	idx, err := indexOutside(row, ':')
	if err != nil {
		return err
	}
	vid, err := parseStringLiteral(strings.TrimSpace(row[:idx]))
	if err != nil {
		return fmt.Errorf("vertex VID: %w", err)
	}
	if len(values) == 0 {
		return errors.New("vertex values are empty")
	}
	kind, err := parseStringLiteral(strings.TrimSpace(values[0]))
	if err != nil {
		return fmt.Errorf("vertex kind: %w", err)
	}
	return validateScopedVID(vid, kind, input)
}

func validateEdgeRowIdentity(row string, values []string, input RenderedStatementWrite) error {
	idx, err := indexOutside(row, ':')
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return errors.New("edge values are empty")
	}
	target := strings.TrimSpace(row[:idx])
	arrowIdx, err := indexTokenOutside(target, "->")
	if err != nil {
		return fmt.Errorf("edge target: %w", err)
	}
	rankIdx, err := indexTokenOutside(target[arrowIdx+2:], "@")
	if err != nil {
		return fmt.Errorf("edge rank: %w", err)
	}
	rankIdx += arrowIdx + 2

	fromVID, err := parseStringLiteral(strings.TrimSpace(target[:arrowIdx]))
	if err != nil {
		return fmt.Errorf("edge from VID: %w", err)
	}
	toVID, err := parseStringLiteral(strings.TrimSpace(target[arrowIdx+2 : rankIdx]))
	if err != nil {
		return fmt.Errorf("edge to VID: %w", err)
	}
	rankText := strings.TrimSpace(target[rankIdx+1:])
	rank, err := strconv.ParseInt(rankText, 10, 64)
	if err != nil || rank < 0 || rank > 0xFFFFFFFF {
		return fmt.Errorf("edge rank %q is outside uint32 range", rankText)
	}
	kind, err := parseStringLiteral(strings.TrimSpace(values[0]))
	if err != nil {
		return fmt.Errorf("edge kind: %w", err)
	}
	// Q.A: `source` is no longer the last EdgeProps column (we
	// appended `provenance` + `context`). Look it up by name so
	// the validator survives any future EdgeProps additions.
	sourceIdx := edgePropsIndex("source")
	if sourceIdx < 0 || sourceIdx >= len(values) {
		return fmt.Errorf("edge source: column index out of range (sourceIdx=%d, values=%d)", sourceIdx, len(values))
	}
	source, err := parseStringLiteral(strings.TrimSpace(values[sourceIdx]))
	if err != nil {
		return fmt.Errorf("edge source: %w", err)
	}
	expectedRank := graphschema.EdgeRank(fromVID + "->" + toVID + ":" + kind + ":" + source)
	if rank != expectedRank {
		return fmt.Errorf("edge rank got %d, want %d", rank, expectedRank)
	}
	if err := validateScopedVID(fromVID, "", input); err != nil {
		return fmt.Errorf("from VID: %w", err)
	}
	if err := validateScopedVID(toVID, "", input); err != nil {
		return fmt.Errorf("to VID: %w", err)
	}
	return nil
}

func validateScopedVID(vid, expectedKind string, input RenderedStatementWrite) error {
	workspaceHash := hashParts([]string{input.WorkspaceID}, 8)
	builderHash := hashParts([]string{input.BuilderVersion}, 8)
	scopeParts := []string{
		"cg",
		"o" + strconv.FormatInt(input.OrgID, 10),
		"w" + workspaceHash,
		"r" + strconv.FormatInt(input.RepoID, 10),
		"c" + input.CommitHash[:12],
		"s" + strconv.FormatInt(input.SchemaVersion, 10),
		"b" + builderHash,
	}
	prefix := strings.Join(scopeParts, ":") + ":"
	if !strings.HasPrefix(vid, prefix) {
		return fmt.Errorf("VID %q does not start with scoped prefix %q", vid, prefix)
	}
	rest := strings.TrimPrefix(vid, prefix)
	kind, key, ok := strings.Cut(rest, ":")
	if !ok {
		return fmt.Errorf("VID %q missing kind/key hash", vid)
	}
	if expectedKind != "" && kind != expectedKind {
		return fmt.Errorf("VID kind got %q, want %q", kind, expectedKind)
	}
	if expectedKind == "" && kind == "" {
		return errors.New("VID kind is empty")
	}
	if strings.Contains(key, ":") {
		return fmt.Errorf("VID key hash %q contains ':'", key)
	}
	if len(key) != 32 || !isLowerHex(key) {
		return fmt.Errorf("VID key hash %q is not 32 lowercase hex chars", key)
	}
	return nil
}

func parseStringLiteral(raw string) (string, error) {
	if !strings.HasPrefix(raw, "\"") || !strings.HasSuffix(raw, "\"") {
		return "", fmt.Errorf("not a string literal: %s", raw)
	}
	var out string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", err
	}
	return out, nil
}

func splitTopLevel(s string, sep rune) ([]string, error) {
	var out []string
	start := 0
	depth := 0
	inString := false
	escaped := false
	for i, r := range s {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == '"' {
				inString = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return nil, errors.New("unbalanced parentheses")
			}
		case sep:
			if depth == 0 {
				part := strings.TrimSpace(s[start:i])
				if part == "" {
					return nil, errors.New("empty top-level part")
				}
				out = append(out, part)
				start = i + len(string(r))
			}
		}
	}
	if inString || depth != 0 {
		return nil, errors.New("unterminated string or tuple")
	}
	last := strings.TrimSpace(s[start:])
	if last == "" {
		return nil, errors.New("empty trailing part")
	}
	out = append(out, last)
	return out, nil
}

func indexOutside(s string, needle rune) (int, error) {
	inString := false
	escaped := false
	for i, r := range s {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == '"' {
				inString = false
			}
			continue
		}
		if r == '"' {
			inString = true
			continue
		}
		if r == needle {
			return i, nil
		}
	}
	return -1, fmt.Errorf("missing %q outside string", needle)
}

func indexTokenOutside(s, token string) (int, error) {
	if token == "" {
		return -1, errors.New("empty token")
	}
	inString := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}
		if ch == '"' {
			inString = true
			continue
		}
		if strings.HasPrefix(s[i:], token) {
			return i, nil
		}
	}
	return -1, fmt.Errorf("missing %q outside string", token)
}

func validateSingleTerminatedStatement(stmt string) error {
	if strings.TrimSpace(stmt) != stmt {
		return errors.New("statement has leading or trailing whitespace")
	}
	if !strings.HasSuffix(stmt, ";") {
		return errors.New("statement must terminate with semicolon")
	}
	inString := false
	inIdent := false
	escaped := false
	runes := []rune(stmt)
	for i, r := range runes[:len(runes)-1] {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if r == '\\' {
				escaped = true
				continue
			}
			if r == '"' {
				inString = false
			}
			continue
		}
		if inIdent {
			if r == '`' {
				if i+1 < len(runes)-1 && runes[i+1] == '`' {
					continue
				}
				inIdent = false
			}
			continue
		}
		switch r {
		case '"':
			inString = true
		case '`':
			inIdent = true
		case ';':
			return errors.New("statement contains an extra top-level semicolon")
		}
	}
	if inString || inIdent {
		return errors.New("unterminated quoted literal or identifier")
	}
	return nil
}

func insertPrefix(kind, name string, props []string) string {
	quoted := make([]string, 0, len(props))
	for _, prop := range props {
		quoted = append(quoted, quoteIdentifierLocal(prop))
	}
	return "INSERT " + kind + " " + quoteIdentifierLocal(name) + "(" + strings.Join(quoted, ", ") + ") VALUES "
}

func quoteIdentifierLocal(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

func renderStringLiteral(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

func isHexCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !unicode.Is(unicode.ASCII_Hex_Digit, r) {
			return false
		}
	}
	return true
}

func hashParts(parts []string, length int) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	encoded := hex.EncodeToString(sum[:])
	if length < 0 {
		return ""
	}
	if length > len(encoded) {
		return encoded
	}
	return encoded[:length]
}

func isLowerHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func renderedValidation(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRenderedStatementValidation, fmt.Sprintf(format, args...))
}
