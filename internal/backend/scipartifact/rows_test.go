package scipartifact

import (
	"os"
	"path/filepath"
	"testing"

	scippb "codeintel/proto/scip/v1"
	"google.golang.org/protobuf/proto"
)

func TestRowsFromArtifactBytesPersistsSymbolsOccurrencesAndRelationships(t *testing.T) {
	tmp := t.TempDir()
	mustWrite(t, filepath.Join(tmp, "src/orders/createOrder.ts"),
		"export async function createOrder(command) {\n  return command.id;\n}\n")
	mustWrite(t, filepath.Join(tmp, "src/routes/internalOrders.ts"),
		"import { createOrder } from '../orders/createOrder';\nexport async function handler() {\n  return createOrder({ id: '1' });\n}\n")

	createSymbol := "scip-typescript npm app 1.0.0 src/orders/createOrder.ts/createOrder()."
	handlerSymbol := "scip-typescript npm app 1.0.0 src/routes/internalOrders.ts/handler()."
	externalSymbol := "scip-typescript npm typescript 5.0.0 lib/lib.es5.d.ts/Promise#"
	index := &scippb.Index{
		Documents: []*scippb.Document{
			{
				Language:     "typescript",
				RelativePath: "src/orders/createOrder.ts",
				Occurrences: []*scippb.Occurrence{{
					Range:          []int32{0, 22, 33},
					Symbol:         createSymbol,
					SymbolRoles:    int32(scippb.SymbolRole_DEFINITION),
					SyntaxKind:     scippb.SyntaxKind_IDENTIFIER_FUNCTION_DEFINITION,
					EnclosingRange: []int32{0, 0, 2, 1},
				}},
				Symbols: []*scippb.SymbolInformation{{
					Symbol:        createSymbol,
					Documentation: []string{"Creates an order."},
					Kind:          scippb.SymbolInformation_FUNCTION,
					DisplayName:   "createOrder",
					SignatureDocumentation: &scippb.Document{
						Text: "function createOrder(command): Promise<Order>",
					},
				}},
			},
			{
				Language:     "typescript",
				RelativePath: "src/routes/internalOrders.ts",
				Occurrences: []*scippb.Occurrence{
					{
						Range:          []int32{1, 22, 29},
						Symbol:         handlerSymbol,
						SymbolRoles:    int32(scippb.SymbolRole_DEFINITION),
						SyntaxKind:     scippb.SyntaxKind_IDENTIFIER_FUNCTION_DEFINITION,
						EnclosingRange: []int32{1, 0, 3, 1},
					},
					{
						Range:       []int32{2, 9, 20},
						Symbol:      createSymbol,
						SymbolRoles: int32(scippb.SymbolRole_READ_ACCESS),
						SyntaxKind:  scippb.SyntaxKind_IDENTIFIER_FUNCTION,
					},
					{
						Range:       []int32{0, 9, 20},
						Symbol:      createSymbol,
						SymbolRoles: int32(scippb.SymbolRole_IMPORT),
						SyntaxKind:  scippb.SyntaxKind_IDENTIFIER,
					},
				},
				Symbols: []*scippb.SymbolInformation{{
					Symbol: handlerSymbol,
					Kind:   scippb.SymbolInformation_FUNCTION,
					Relationships: []*scippb.Relationship{{
						Symbol:      createSymbol,
						IsReference: true,
					}},
					DisplayName: "handler",
				}},
			},
		},
		ExternalSymbols: []*scippb.SymbolInformation{{
			Symbol:        externalSymbol,
			Documentation: []string{"External promise docs"},
			Kind:          scippb.SymbolInformation_CLASS,
		}},
	}
	raw, err := proto.Marshal(index)
	if err != nil {
		t.Fatalf("marshal SCIP: %v", err)
	}

	rows, err := RowsFromArtifactBytes(raw, "typescript", "", tmp)
	if err != nil {
		t.Fatalf("RowsFromArtifactBytes: %v", err)
	}
	if got, want := len(rows.Symbols), 3; got != want {
		t.Fatalf("symbols=%d want %d: %#v", got, want, rows.Symbols)
	}
	createRow := findSymbol(rows.Symbols, createSymbol)
	if createRow == nil {
		t.Fatalf("missing create symbol")
	}
	if createRow.DisplayName != "createOrder" || value(createRow.Kind) != "Function" {
		t.Fatalf("create symbol row = %#v", createRow)
	}
	if value(createRow.FilePath) != "src/orders/createOrder.ts" || value(createRow.Signature) != "function createOrder(command): Promise<Order>" {
		t.Fatalf("create symbol location/signature = %#v", createRow)
	}
	externalRow := findSymbol(rows.Symbols, externalSymbol)
	if externalRow == nil || externalRow.DisplayName != "Promise" || externalRow.FilePath != nil {
		t.Fatalf("external row = %#v", externalRow)
	}
	if !hasOccurrence(rows.Occurrences, createSymbol, "src/orders/createOrder.ts", "DEFINITION", "export async function createOrder(command) {", "") {
		t.Fatalf("missing definition occurrence: %#v", rows.Occurrences)
	}
	if !hasOccurrence(rows.Occurrences, createSymbol, "src/routes/internalOrders.ts", "REFERENCE", "  return createOrder({ id: '1' });", handlerSymbol) {
		t.Fatalf("missing semantic reference occurrence: %#v", rows.Occurrences)
	}
	if !hasOccurrence(rows.Occurrences, createSymbol, "src/routes/internalOrders.ts", "READ", "  return createOrder({ id: '1' });", handlerSymbol) {
		t.Fatalf("missing READ occurrence: %#v", rows.Occurrences)
	}
	if !hasOccurrence(rows.Occurrences, createSymbol, "src/routes/internalOrders.ts", "IMPORT", "import { createOrder } from '../orders/createOrder';", "") {
		t.Fatalf("missing IMPORT occurrence: %#v", rows.Occurrences)
	}
	if got, want := len(rows.Relationships), 1; got != want {
		t.Fatalf("relationships=%d want %d: %#v", got, want, rows.Relationships)
	}
	if rows.Relationships[0].SourceSymbol != handlerSymbol || rows.Relationships[0].TargetSymbol != createSymbol || !rows.Relationships[0].IsReference {
		t.Fatalf("relationship row = %#v", rows.Relationships[0])
	}
}

func TestRowsFromArtifactBytesDropsEscapedDocumentPaths(t *testing.T) {
	index := &scippb.Index{
		Documents: []*scippb.Document{{
			Language:     "typescript",
			RelativePath: "../outside.ts",
			Occurrences: []*scippb.Occurrence{{
				Range:       []int32{0, 0, 4},
				Symbol:      "scip-typescript npm app 1.0.0 outside.ts/leak().",
				SymbolRoles: int32(scippb.SymbolRole_DEFINITION),
				SyntaxKind:  scippb.SyntaxKind_IDENTIFIER_FUNCTION_DEFINITION,
			}},
			Symbols: []*scippb.SymbolInformation{{
				Symbol:      "scip-typescript npm app 1.0.0 outside.ts/leak().",
				Kind:        scippb.SymbolInformation_FUNCTION,
				DisplayName: "leak",
			}},
		}},
	}
	raw, err := proto.Marshal(index)
	if err != nil {
		t.Fatalf("marshal SCIP: %v", err)
	}
	rows, err := RowsFromArtifactBytes(raw, "typescript", "", t.TempDir())
	if err != nil {
		t.Fatalf("RowsFromArtifactBytes: %v", err)
	}
	if len(rows.Symbols) != 0 || len(rows.Occurrences) != 0 || len(rows.Relationships) != 0 {
		t.Fatalf("escaped document produced rows: %#v", rows)
	}
}

func TestRowsFromArtifactBytesDoesNotReadLineContentThroughSymlink(t *testing.T) {
	worktree := t.TempDir()
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "secret.ts"), "export const secret = 1;\n")
	if err := os.MkdirAll(filepath.Join(worktree, "src"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.ts"), filepath.Join(worktree, "src/link.ts")); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	symbol := "scip-typescript npm app 1.0.0 src/link.ts/secret()."
	index := &scippb.Index{
		Documents: []*scippb.Document{{
			Language:     "typescript",
			RelativePath: "src/link.ts",
			Occurrences: []*scippb.Occurrence{{
				Range:       []int32{0, 13, 19},
				Symbol:      symbol,
				SymbolRoles: int32(scippb.SymbolRole_READ_ACCESS),
				SyntaxKind:  scippb.SyntaxKind_IDENTIFIER,
			}},
		}},
	}
	raw, err := proto.Marshal(index)
	if err != nil {
		t.Fatalf("marshal SCIP: %v", err)
	}
	rows, err := RowsFromArtifactBytes(raw, "typescript", "", worktree)
	if err != nil {
		t.Fatalf("RowsFromArtifactBytes: %v", err)
	}
	if len(rows.Occurrences) == 0 {
		t.Fatalf("expected occurrence row")
	}
	for _, occurrence := range rows.Occurrences {
		if occurrence.LineContent != nil {
			t.Fatalf("symlinked source produced line content: %#v", occurrence)
		}
	}
}

func mustWrite(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func findSymbol(rows []SymbolRow, symbol string) *SymbolRow {
	for i := range rows {
		if rows[i].Symbol == symbol {
			return &rows[i]
		}
	}
	return nil
}

func hasOccurrence(rows []OccurrenceRow, symbol, filePath, role, line, enclosing string) bool {
	for _, row := range rows {
		if row.Symbol == symbol && row.FilePath == filePath && row.Role == role && value(row.LineContent) == line && value(row.EnclosingSymbol) == enclosing {
			return true
		}
	}
	return false
}

func value(ptr *string) string {
	if ptr == nil {
		return ""
	}
	return *ptr
}
