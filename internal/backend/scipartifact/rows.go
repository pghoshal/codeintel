// Package scipartifact parses and persists SCIP artifacts produced by
// split executor workers. Backend owns this package because it writes
// Postgres state; Rust only produces scoped artifacts over gRPC.
package scipartifact

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	scippb "codeintel/proto/scip/v1"
	"google.golang.org/protobuf/proto"
)

type NormalizedRange struct {
	StartLine      int32
	StartCharacter int32
	EndLine        int32
	EndCharacter   int32
}

type SymbolRow struct {
	Symbol          string
	DisplayName     string
	Kind            *string
	Language        *string
	Documentation   []string
	Signature       *string
	FilePath        *string
	StartLine       *int32
	StartCharacter  *int32
	EndLine         *int32
	EndCharacter    *int32
	EnclosingSymbol *string
}

type OccurrenceRow struct {
	Symbol          string
	FilePath        string
	StartLine       int32
	StartCharacter  int32
	EndLine         int32
	EndCharacter    int32
	Role            string
	Language        *string
	SyntaxKind      *string
	LineContent     *string
	EnclosingSymbol *string
}

type RelationshipRow struct {
	SourceSymbol     string
	TargetSymbol     string
	IsReference      bool
	IsImplementation bool
	IsTypeDefinition bool
	IsDefinition     bool
}

type Rows struct {
	Symbols       []SymbolRow
	Occurrences   []OccurrenceRow
	Relationships []RelationshipRow
}

type definitionScope struct {
	symbol string
	rng    NormalizedRange
}

func RowsFromArtifactBytes(raw []byte, projectLanguage, projectRoot, worktreePath string) (Rows, error) {
	if len(raw) == 0 {
		return Rows{}, errors.New("empty SCIP artifact")
	}
	var index scippb.Index
	if err := proto.Unmarshal(raw, &index); err != nil {
		return Rows{}, err
	}
	return RowsFromIndex(&index, projectLanguage, projectRoot, worktreePath), nil
}

func RowsFromIndex(index *scippb.Index, projectLanguage, projectRoot, worktreePath string) Rows {
	if index == nil {
		return Rows{}
	}
	var symbols []SymbolRow
	var occurrences []OccurrenceRow
	var relationships []RelationshipRow
	relationshipKeys := map[string]struct{}{}
	symbolDefinitions := map[string]definitionOccurrence{}
	definitionScopesByFile := map[string][]definitionScope{}
	documentSymbols := map[string]struct{}{}
	lineCache := newLineContentCache(worktreePath)

	for _, document := range index.GetDocuments() {
		filePath, ok := joinProjectPathChecked(projectRoot, document.GetRelativePath())
		if !ok {
			continue
		}
		for _, symbol := range document.GetSymbols() {
			documentSymbols[symbol.GetSymbol()] = struct{}{}
		}
		for _, occurrence := range document.GetOccurrences() {
			if occurrence.GetSymbol() == "" || !isDefinitionOccurrence(occurrence) {
				continue
			}
			symbolDefinitions[occurrence.GetSymbol()] = definitionOccurrence{
				occurrence: occurrence,
				document:   document,
			}
			if rng, ok := normalizeRange(occurrence.GetEnclosingRange()); ok {
				definitionScopesByFile[filePath] = append(definitionScopesByFile[filePath], definitionScope{symbol: occurrence.GetSymbol(), rng: rng})
			} else if rng, ok := normalizeRange(occurrence.GetRange()); ok {
				definitionScopesByFile[filePath] = append(definitionScopesByFile[filePath], definitionScope{symbol: occurrence.GetSymbol(), rng: rng})
			}
		}
	}

	projectDefinedSymbols := map[string]struct{}{}
	for symbol := range documentSymbols {
		if _, ok := symbolDefinitions[symbol]; ok && isSemanticProjectSymbol(symbol) {
			projectDefinedSymbols[symbol] = struct{}{}
		}
	}
	for file := range definitionScopesByFile {
		sort.Slice(definitionScopesByFile[file], func(i, j int) bool {
			return rangeSpan(definitionScopesByFile[file][i].rng) < rangeSpan(definitionScopesByFile[file][j].rng)
		})
	}

	for _, document := range index.GetDocuments() {
		language := document.GetLanguage()
		if language == "" {
			language = projectLanguage
		}
		filePath, ok := joinProjectPathChecked(projectRoot, document.GetRelativePath())
		if !ok {
			continue
		}

		for _, symbol := range document.GetSymbols() {
			symbols = append(symbols, toSymbolRow(symbol, language, projectRoot, symbolDefinitions[symbol.GetSymbol()]))
			addRelationshipRows(&relationships, relationshipKeys, projectDefinedSymbols, symbol.GetSymbol(), symbol.GetRelationships())
		}

		for _, occurrence := range document.GetOccurrences() {
			if occurrence.GetSymbol() == "" {
				continue
			}
			rng, ok := normalizeRange(occurrence.GetRange())
			if !ok {
				continue
			}
			roles := occurrenceRoles(occurrence)
			enclosing := findEnclosingDefinitionSymbol(definitionScopesByFile[filePath], rng, occurrence.GetSymbol())
			for _, role := range roles {
				occurrences = append(occurrences, OccurrenceRow{
					Symbol:          occurrence.GetSymbol(),
					FilePath:        filePath,
					StartLine:       rng.StartLine,
					StartCharacter:  rng.StartCharacter,
					EndLine:         rng.EndLine,
					EndCharacter:    rng.EndCharacter,
					Role:            role,
					Language:        ptrIfNotEmpty(language),
					SyntaxKind:      syntaxKindName(occurrence),
					LineContent:     lineCache.getLineContent(filePath, rng.StartLine),
					EnclosingSymbol: ptrIfNotEmpty(enclosing),
				})
			}
			if shouldDeriveReferenceRelationship(roles, enclosing, occurrence.GetSymbol()) {
				addRelationshipRows(
					&relationships,
					relationshipKeys,
					projectDefinedSymbols,
					enclosing,
					[]*scippb.Relationship{{Symbol: occurrence.GetSymbol(), IsReference: true}},
				)
			}
		}
	}

	for _, symbol := range index.GetExternalSymbols() {
		symbols = append(symbols, toSymbolRow(symbol, projectLanguage, projectRoot, definitionOccurrence{}))
		addRelationshipRows(&relationships, relationshipKeys, projectDefinedSymbols, symbol.GetSymbol(), symbol.GetRelationships())
	}

	return Rows{Symbols: symbols, Occurrences: occurrences, Relationships: relationships}
}

type definitionOccurrence struct {
	occurrence *scippb.Occurrence
	document   *scippb.Document
}

func toSymbolRow(symbol *scippb.SymbolInformation, language, projectRoot string, definition definitionOccurrence) SymbolRow {
	var definitionRange *NormalizedRange
	if definition.occurrence != nil {
		if rng, ok := normalizeRange(definition.occurrence.GetRange()); ok {
			definitionRange = &rng
		}
	}
	var filePath *string
	if definition.document != nil {
		if joined, ok := joinProjectPathChecked(projectRoot, definition.document.GetRelativePath()); ok {
			filePath = &joined
		}
	}
	var signature *string
	if doc := symbol.GetSignatureDocumentation(); doc != nil && doc.GetText() != "" {
		text := doc.GetText()
		signature = &text
	}
	displayName := symbol.GetDisplayName()
	if displayName == "" {
		displayName = displayNameFromSCIPSymbol(symbol.GetSymbol())
	}
	kind := symbolKindName(symbol.GetKind())
	row := SymbolRow{
		Symbol:        symbol.GetSymbol(),
		DisplayName:   displayName,
		Kind:          ptrIfNotEmpty(kind),
		Language:      ptrIfNotEmpty(language),
		Documentation: symbol.GetDocumentation(),
		Signature:     signature,
		FilePath:      filePath,
		EnclosingSymbol: ptrIfNotEmpty(
			symbol.GetEnclosingSymbol(),
		),
	}
	if definitionRange != nil {
		row.StartLine = &definitionRange.StartLine
		row.StartCharacter = &definitionRange.StartCharacter
		row.EndLine = &definitionRange.EndLine
		row.EndCharacter = &definitionRange.EndCharacter
	}
	return row
}

func addRelationshipRows(out *[]RelationshipRow, keys map[string]struct{}, projectDefinedSymbols map[string]struct{}, sourceSymbol string, relationships []*scippb.Relationship) {
	for _, relationship := range relationships {
		row := RelationshipRow{
			SourceSymbol:     sourceSymbol,
			TargetSymbol:     relationship.GetSymbol(),
			IsReference:      relationship.GetIsReference(),
			IsImplementation: relationship.GetIsImplementation(),
			IsTypeDefinition: relationship.GetIsTypeDefinition(),
			IsDefinition:     relationship.GetIsDefinition(),
		}
		if !shouldPersistProjectRelationship(row, projectDefinedSymbols) {
			continue
		}
		key := strings.Join([]string{
			row.SourceSymbol,
			row.TargetSymbol,
			boolKey(row.IsReference),
			boolKey(row.IsImplementation),
			boolKey(row.IsTypeDefinition),
			boolKey(row.IsDefinition),
		}, "\x00")
		if _, ok := keys[key]; ok {
			continue
		}
		keys[key] = struct{}{}
		*out = append(*out, row)
	}
}

func occurrenceRoles(occurrence *scippb.Occurrence) []string {
	roles := make([]string, 0, 8)
	if occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_DEFINITION) > 0 {
		roles = append(roles, "DEFINITION")
	}
	if occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_IMPORT) > 0 {
		roles = append(roles, "IMPORT")
	}
	if occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_WRITE_ACCESS) > 0 {
		roles = append(roles, "WRITE")
	}
	if occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_READ_ACCESS) > 0 {
		roles = append(roles, "READ")
	}
	if occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_GENERATED) > 0 {
		roles = append(roles, "GENERATED")
	}
	if occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_TEST) > 0 {
		roles = append(roles, "TEST")
	}
	if occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_FORWARD_DEFINITION) > 0 {
		roles = append(roles, "FORWARD_DEFINITION")
	}
	hasDefinition := false
	for _, role := range roles {
		if role == "DEFINITION" || role == "FORWARD_DEFINITION" {
			hasDefinition = true
			break
		}
	}
	if !hasDefinition {
		roles = append([]string{"REFERENCE"}, roles...)
	}
	return dedupeRoles(roles)
}

func dedupeRoles(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, role := range in {
		if _, ok := seen[role]; ok {
			continue
		}
		seen[role] = struct{}{}
		out = append(out, role)
	}
	return out
}

func isDefinitionOccurrence(occurrence *scippb.Occurrence) bool {
	return occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_DEFINITION) > 0 ||
		occurrence.GetSymbolRoles()&int32(scippb.SymbolRole_FORWARD_DEFINITION) > 0
}

func normalizeRange(values []int32) (NormalizedRange, bool) {
	switch len(values) {
	case 3:
		return NormalizedRange{StartLine: values[0], StartCharacter: values[1], EndLine: values[0], EndCharacter: values[2]}, true
	case 4:
		return NormalizedRange{StartLine: values[0], StartCharacter: values[1], EndLine: values[2], EndCharacter: values[3]}, true
	default:
		return NormalizedRange{}, false
	}
}

func containsRange(outer, inner NormalizedRange) bool {
	if outer.StartLine > inner.StartLine || outer.EndLine < inner.EndLine {
		return false
	}
	if outer.StartLine == inner.StartLine && outer.StartCharacter > inner.StartCharacter {
		return false
	}
	if outer.EndLine == inner.EndLine && outer.EndCharacter < inner.EndCharacter {
		return false
	}
	return true
}

func rangeSpan(rng NormalizedRange) int32 {
	span := (rng.EndLine - rng.StartLine) * 100000
	charSpan := rng.EndCharacter - rng.StartCharacter
	if charSpan < 0 {
		charSpan = 0
	}
	return span + charSpan
}

func findEnclosingDefinitionSymbol(definitions []definitionScope, occurrenceRange NormalizedRange, occurrenceSymbol string) string {
	for _, definition := range definitions {
		if definition.symbol != occurrenceSymbol && containsRange(definition.rng, occurrenceRange) {
			return definition.symbol
		}
	}
	return ""
}

func shouldDeriveReferenceRelationship(roles []string, enclosingSymbol, targetSymbol string) bool {
	if enclosingSymbol == "" || enclosingSymbol == targetSymbol {
		return false
	}
	hasReference := false
	for _, role := range roles {
		switch role {
		case "REFERENCE":
			hasReference = true
		case "DEFINITION", "FORWARD_DEFINITION":
			return false
		}
	}
	return hasReference
}

func shouldPersistProjectRelationship(row RelationshipRow, projectDefinedSymbols map[string]struct{}) bool {
	if _, ok := projectDefinedSymbols[row.SourceSymbol]; !ok {
		return false
	}
	if _, ok := projectDefinedSymbols[row.TargetSymbol]; !ok {
		return false
	}
	return isSemanticProjectSymbol(row.SourceSymbol) && isSemanticProjectSymbol(row.TargetSymbol)
}

func isSemanticProjectSymbol(symbol string) bool {
	return !strings.HasPrefix(symbol, "local ") &&
		!scipParameterSymbol.MatchString(symbol) &&
		!scipGeneratedPropertySymbol.MatchString(symbol) &&
		!isSCIPFileScopeSymbol(symbol)
}

var (
	scipParameterSymbol         = regexp.MustCompile(`\(\)\.\([^)]+\)$`)
	scipGeneratedPropertySymbol = regexp.MustCompile(`\d+:$`)
	scipFileScopeSymbol         = regexp.MustCompile("`[^`]+`\\s*/$")
)

func isSCIPFileScopeSymbol(symbol string) bool {
	return scipFileScopeSymbol.MatchString(symbol) || strings.HasSuffix(symbol, "/")
}

type lineContentCache struct {
	root  string
	files map[string]*[]string
	order []string
}

func newLineContentCache(root string) *lineContentCache {
	clean, err := filepath.Abs(root)
	if err != nil {
		clean = ""
	}
	if clean != "" {
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			clean = filepath.Clean(resolved)
		} else {
			clean = ""
		}
	}
	return &lineContentCache{root: clean, files: map[string]*[]string{}}
}

func (c *lineContentCache) getLineContent(filePath string, zeroBasedLine int32) *string {
	if zeroBasedLine < 0 || c.root == "" {
		return nil
	}
	if _, ok := c.files[filePath]; !ok {
		if len(c.files) >= 64 && len(c.order) > 0 {
			oldest := c.order[0]
			c.order = c.order[1:]
			delete(c.files, oldest)
		}
		lines := c.readLines(filePath)
		c.files[filePath] = lines
		c.order = append(c.order, filePath)
	}
	lines := c.files[filePath]
	if lines == nil {
		return nil
	}
	line := ""
	if int(zeroBasedLine) < len(*lines) {
		line = (*lines)[zeroBasedLine]
	}
	truncated := truncateLineContent(line)
	return &truncated
}

func (c *lineContentCache) readLines(filePath string) *[]string {
	safe, ok := sanitizeRepoRelativePath(filePath)
	if !ok || safe == "" {
		return nil
	}
	candidate, err := filepath.Abs(filepath.Join(c.root, filepath.FromSlash(safe)))
	if err != nil || !strings.HasPrefix(candidate, c.root+string(filepath.Separator)) {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return nil
	}
	resolved = filepath.Clean(resolved)
	if resolved != candidate || !strings.HasPrefix(resolved, c.root+string(filepath.Separator)) {
		return nil
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
		return nil
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		return nil
	}
	parts := strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n")
	return &parts
}

func truncateLineContent(line string) string {
	if len(line) <= 2000 {
		return line
	}
	i := 2000
	for i > 0 && !utf8.RuneStart(line[i]) {
		i--
	}
	return line[:i] + "..."
}

func syntaxKindName(occurrence *scippb.Occurrence) *string {
	return ptrIfNotEmpty(syntaxKindDisplayName(occurrence.GetSyntaxKind()))
}

func joinProjectPathChecked(projectRoot, relativePath string) (string, bool) {
	cleanRoot, ok := sanitizeRepoRelativePath(projectRoot)
	if !ok {
		return "", false
	}
	cleanRelative, ok := sanitizeRepoRelativePath(relativePath)
	if !ok {
		return "", false
	}
	if cleanRoot == "" {
		return cleanRelative, true
	}
	if cleanRelative == "" {
		return cleanRoot, true
	}
	return cleanRoot + "/" + cleanRelative, true
}

func sanitizeRepoRelativePath(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || value == "." {
		return "", true
	}
	if strings.Contains(value, "\x00") || strings.Contains(value, "\\") || strings.HasPrefix(value, "/") {
		return "", false
	}
	if len(value) >= 2 && value[1] == ':' {
		return "", false
	}
	parts := strings.Split(value, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", false
		default:
			if strings.Contains(part, "\x00") {
				return "", false
			}
			out = append(out, part)
		}
	}
	return strings.Join(out, "/"), true
}

func displayNameFromSCIPSymbol(symbol string) string {
	compact := strings.Join(strings.Fields(symbol), " ")
	parts := strings.FieldsFunc(compact, func(r rune) bool {
		switch r {
		case '/', '#', '.', ':', '!', '(', ')', '[', ']':
			return true
		default:
			return false
		}
	})
	if len(parts) == 0 {
		return compact
	}
	return parts[len(parts)-1]
}

func symbolKindName(kind scippb.SymbolInformation_Kind) string {
	names := map[int32]string{
		1:  "Array",
		2:  "Assertion",
		3:  "AssociatedType",
		4:  "Attribute",
		5:  "Axiom",
		6:  "Boolean",
		7:  "Class",
		8:  "Constant",
		9:  "Constructor",
		10: "DataFamily",
		11: "Enum",
		12: "EnumMember",
		13: "Event",
		14: "Fact",
		15: "Field",
		16: "File",
		17: "Function",
		18: "Getter",
		19: "Grammar",
		20: "Instance",
		21: "Interface",
		22: "Key",
		23: "Lang",
		24: "Lemma",
		25: "Macro",
		26: "Method",
		27: "MethodReceiver",
		28: "Message",
		29: "Module",
		30: "Namespace",
		31: "Null",
		32: "Number",
		33: "Object",
		34: "Operator",
		35: "Package",
		36: "PackageObject",
		37: "Parameter",
		38: "ParameterLabel",
		39: "Pattern",
		40: "Predicate",
		41: "Property",
		42: "Protocol",
		43: "Quasiquoter",
		44: "SelfParameter",
		45: "Setter",
		46: "Signature",
		47: "Subscript",
		48: "String",
		49: "Struct",
		50: "Tactic",
		51: "Theorem",
		52: "ThisParameter",
		53: "Trait",
		54: "Type",
		55: "TypeAlias",
		56: "TypeClass",
		57: "TypeFamily",
		58: "TypeParameter",
		59: "Union",
		60: "Value",
		61: "Variable",
		62: "Contract",
		63: "Error",
		64: "Library",
		65: "Modifier",
		66: "AbstractMethod",
		67: "MethodSpecification",
		68: "ProtocolMethod",
		69: "PureVirtualMethod",
		70: "TraitMethod",
		71: "TypeClassMethod",
		72: "Accessor",
		73: "Delegate",
		74: "MethodAlias",
		75: "SingletonClass",
		76: "SingletonMethod",
		77: "StaticDataMember",
		78: "StaticEvent",
		79: "StaticField",
		80: "StaticMethod",
		81: "StaticProperty",
		82: "StaticVariable",
		84: "Extension",
		85: "Mixin",
		86: "Concept",
	}
	return names[int32(kind)]
}

func syntaxKindDisplayName(kind scippb.SyntaxKind) string {
	names := map[int32]string{
		1:  "Comment",
		2:  "PunctuationDelimiter",
		3:  "PunctuationBracket",
		4:  "Keyword",
		5:  "IdentifierOperator",
		6:  "Identifier",
		7:  "IdentifierBuiltin",
		8:  "IdentifierNull",
		9:  "IdentifierConstant",
		10: "IdentifierMutableGlobal",
		11: "IdentifierParameter",
		12: "IdentifierLocal",
		13: "IdentifierShadowed",
		14: "IdentifierNamespace",
		15: "IdentifierFunction",
		16: "IdentifierFunctionDefinition",
		17: "IdentifierMacro",
		18: "IdentifierMacroDefinition",
		19: "IdentifierType",
		20: "IdentifierBuiltinType",
		21: "IdentifierAttribute",
		22: "RegexEscape",
		23: "RegexRepeated",
		24: "RegexWildcard",
		25: "RegexDelimiter",
		26: "RegexJoin",
		27: "StringLiteral",
		28: "StringLiteralEscape",
		29: "StringLiteralSpecial",
		30: "StringLiteralKey",
		31: "CharacterLiteral",
		32: "NumericLiteral",
		33: "BooleanLiteral",
		34: "Tag",
		35: "TagAttribute",
		36: "TagDelimiter",
	}
	return names[int32(kind)]
}

func ptrIfNotEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func boolKey(v bool) string {
	if v {
		return "true"
	}
	return "false"
}
