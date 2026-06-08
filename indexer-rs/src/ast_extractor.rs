//! Syntactic AST extractor — Rust port of
//! `packages/backend/src/codeGraph/syntacticAstExtractor.ts`.
//!
//! Scope of THIS slice (R.9a):
//!   - SyntacticAstFact + SyntacticAstScanResult types.
//!   - Shared helpers: sanitise, count_char, is_language_keyword,
//!     push_import, push_defines, push_calls.
//!   - The Go-only extractor `extract_go_facts`.
//!   - LANGUAGE_KEYWORDS table for all 7 legacy languages
//!     (interrogated by other languages' extractors in R.9b..h).
//!
//! Scope deferred to follow-up slices:
//!   - R.9b: Python extractor
//!   - R.9c: Java extractor
//!   - R.9d: Ruby extractor
//!   - R.9e: C# extractor
//!   - R.9f: Rust extractor
//!   - R.9g: Dart extractor
//!   - R.9h: TypeScript extractor (compiler-grade — needs swc
//!     or tree-sitter, not regex)
//!   - R.9i: directory walker `scan_syntactic_ast` orchestrator
//!   - R.9j: Nebula graph persistence
//!
//! Confidence tier: every fact carries CONFIDENCE_INFERRED
//! (0.6). The MCP / graph reranker uses this to distinguish
//! regex-grade facts from compiler-grade facts (which would
//! be CONFIDENCE_PARSED=0.95, emitted by the TS extractor).
//!
//! Tenant safety: pure transform — no I/O in this module, no
//! DB calls. The orchestrator (R.9i) is responsible for
//! tagging emitted facts with the org/workspace/repo scope.
//!
//! Truncation parity note: legacy `sanitise` truncates at
//! `MAX_CALLEE_TEXT_LENGTH` JS-string-units (UTF-16 code
//! units) and appends "...". The Rust port truncates at
//! `MAX_CALLEE_TEXT_LENGTH` UTF-8 BYTES while preserving char
//! boundaries (`is_char_boundary`-snap on the way down). For
//! ASCII inputs the two are byte-equal. For non-ASCII inputs
//! the Rust port may emit a slightly shorter string than the
//! legacy when the surrogate-pair-counting JS string runs to
//! the limit on a non-BMP char. No production fixture
//! currently exercises non-ASCII callees, and the truncation
//! is purely cosmetic (the resulting fact still routes to the
//! same target_symbol because the "...." suffix is the same).

use regex::Regex;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::OnceLock;
use tree_sitter::{Language, Node, Parser};

/// SyntacticAstFact mirrors the legacy struct
/// (syntacticAstExtractor.ts:24-37). Field names use camelCase
/// in the wire JSON (serde_rename) so the legacy and Rust
/// emit byte-equal records when both target the same fact
/// store.
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq)]
pub struct SyntacticAstFact {
    pub kind: FactKind,
    pub language: String,
    #[serde(rename = "sourceSymbol")]
    pub source_symbol: String,
    #[serde(rename = "sourceDisplayName")]
    pub source_display_name: String,
    #[serde(rename = "sourceKind")]
    pub source_kind: SymbolKind,
    #[serde(rename = "targetSymbol")]
    pub target_symbol: String,
    #[serde(rename = "targetDisplayName")]
    pub target_display_name: String,
    #[serde(rename = "targetKind")]
    pub target_kind: SymbolKind,
    #[serde(rename = "filePath")]
    pub file_path: String,
    #[serde(rename = "startLine")]
    pub start_line: u32,
    #[serde(rename = "endLine")]
    pub end_line: u32,
    /// 0..=1 — written as a 64-bit float so we match the
    /// legacy `number` JSON encoding byte-for-byte (0.6 →
    /// "0.6", not "6e-1"). PartialEq comparisons are done on
    /// the raw bits since 0.6 is not exactly representable in
    /// IEEE-754; for parity tests we compare via `to_bits`.
    pub confidence: f64,
}

/// FactKind mirrors the legacy union
/// ("calls" | "imports_from" | "defines" | "extends" |
///  "implements").
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq, Hash)]
#[serde(rename_all = "snake_case")]
pub enum FactKind {
    Calls,
    ImportsFrom,
    Defines,
    Extends,
    Implements,
}

/// SymbolKind mirrors the legacy union for source/targetKind.
#[derive(Debug, Clone, Copy, Serialize, Deserialize, PartialEq, Eq, Hash)]
#[serde(rename_all = "snake_case")]
pub enum SymbolKind {
    Function,
    Method,
    Class,
    Module,
    External,
    Symbol,
    Interface,
}

/// SyntacticAstScanResult mirrors lines 39-44.
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct SyntacticAstScanResult {
    pub facts: Vec<SyntacticAstFact>,
    #[serde(rename = "scannedFileCount")]
    pub scanned_file_count: u32,
    #[serde(rename = "skippedFileCount")]
    pub skipped_file_count: u32,
    pub warnings: Vec<String>,
}

/// CONFIDENCE_INFERRED is the legacy const at line 56. All
/// regex-grade facts emit at this tier.
pub const CONFIDENCE_INFERRED: f64 = 0.6;
pub const CONFIDENCE_PARSED: f64 = 0.95;

/// MAX_CALLEE_TEXT_LENGTH gates the longest callee string we
/// emit (lines 57). Above this we truncate + append "...".
const MAX_CALLEE_TEXT_LENGTH: usize = 120;

// ===== Shared helpers (legacy lines 614-706) =====

/// sanitise mirrors lines 616-631. Rejects strings containing
/// any string-literal delimiter, collapses whitespace, and
/// truncates to MAX_CALLEE_TEXT_LENGTH.
pub fn sanitise(text: &str) -> Option<String> {
    if text.is_empty() {
        return None;
    }
    // STRING_LITERAL_REJECT_RE: ["'`]
    if text.contains('"') || text.contains('\'') || text.contains('`') {
        return None;
    }
    // Collapse whitespace runs to single space + trim.
    let flat: String = {
        static WS_RE: OnceLock<Regex> = OnceLock::new();
        let re = WS_RE.get_or_init(|| Regex::new(r"\s+").unwrap());
        re.replace_all(text, " ").trim().to_string()
    };
    if flat.is_empty() {
        return None;
    }
    if flat.len() > MAX_CALLEE_TEXT_LENGTH {
        let safe_end = (0..=MAX_CALLEE_TEXT_LENGTH)
            .rev()
            .find(|&i| flat.is_char_boundary(i))
            .unwrap_or(MAX_CALLEE_TEXT_LENGTH);
        return Some(format!("{}...", &flat[..safe_end]));
    }
    Some(flat)
}

/// count_char counts occurrences of a single ASCII char.
/// Mirrors lines 633-641. We take `char` not `u8` so non-ASCII
/// inputs don't silently miscount.
pub fn count_char(s: &str, c: char) -> usize {
    s.chars().filter(|&ch| ch == c).count()
}

/// LANGUAGE_KEYWORDS is the verbatim port of lines 643-651.
/// Each language's per-keyword set decides whether a token
/// captured by a `\b(name)\s*\(` call pattern is dropped as a
/// false positive (e.g. `if (...)` is not a function call).
fn language_keywords() -> &'static HashMap<&'static str, &'static [&'static str]> {
    static MAP: OnceLock<HashMap<&'static str, &'static [&'static str]>> = OnceLock::new();
    MAP.get_or_init(|| {
        let mut m: HashMap<&'static str, &'static [&'static str]> = HashMap::new();
        m.insert(
            "go",
            &[
                "if", "for", "switch", "case", "select", "return", "go", "defer", "func", "type",
                "package", "import", "var", "const", "range", "chan", "make", "new", "len", "cap",
                "append", "append", "panic", "recover", "fmt", "context", "errors", "string",
                "int", "bool",
            ],
        );
        m.insert(
            "python",
            &[
                "if", "elif", "else", "for", "while", "return", "yield", "def", "class", "import",
                "from", "as", "with", "try", "except", "finally", "raise", "lambda", "print",
                "len", "range", "list", "dict", "set", "tuple", "str", "int", "float", "bool",
                "None", "True", "False", "self",
            ],
        );
        m.insert(
            "java",
            &[
                "if",
                "else",
                "for",
                "while",
                "switch",
                "case",
                "return",
                "new",
                "this",
                "super",
                "class",
                "interface",
                "extends",
                "implements",
                "import",
                "package",
                "public",
                "private",
                "protected",
                "static",
                "final",
                "void",
                "int",
                "String",
                "boolean",
                "long",
                "double",
                "float",
                "char",
                "byte",
                "short",
            ],
        );
        m.insert(
            "ruby",
            &[
                "if", "elsif", "else", "unless", "case", "when", "for", "while", "until", "return",
                "yield", "def", "class", "module", "require", "include", "extend", "self", "super",
                "nil", "true", "false", "puts", "print", "raise", "rescue", "begin", "end",
            ],
        );
        m.insert(
            "csharp",
            &[
                "if",
                "else",
                "for",
                "while",
                "switch",
                "case",
                "return",
                "new",
                "this",
                "base",
                "class",
                "interface",
                "struct",
                "enum",
                "namespace",
                "using",
                "public",
                "private",
                "protected",
                "internal",
                "static",
                "void",
                "int",
                "string",
                "bool",
                "var",
                "throw",
                "try",
                "catch",
                "finally",
            ],
        );
        m.insert(
            "rust",
            &[
                "if", "else", "match", "for", "while", "loop", "return", "let", "fn", "struct",
                "enum", "trait", "impl", "use", "mod", "pub", "self", "Self", "Box", "Vec",
                "Option", "Result", "Some", "None", "Ok", "Err", "i32", "u32", "u64", "i64",
                "bool", "str", "String",
            ],
        );
        m.insert(
            "dart",
            &[
                "if",
                "else",
                "for",
                "while",
                "switch",
                "case",
                "return",
                "new",
                "this",
                "super",
                "class",
                "interface",
                "extends",
                "implements",
                "import",
                "library",
                "part",
                "void",
                "int",
                "String",
                "bool",
                "double",
                "var",
                "final",
                "const",
                "static",
            ],
        );
        m.insert(
            "typescript",
            &[
                "if",
                "else",
                "for",
                "while",
                "switch",
                "case",
                "return",
                "new",
                "this",
                "super",
                "class",
                "interface",
                "type",
                "enum",
                "extends",
                "implements",
                "import",
                "export",
                "from",
                "async",
                "await",
                "function",
                "const",
                "let",
                "var",
                "public",
                "private",
                "protected",
                "static",
                "string",
                "number",
                "boolean",
                "undefined",
                "null",
                "Promise",
                "Array",
                "Object",
                "console",
                "require",
            ],
        );
        m.insert(
            "javascript",
            &[
                "if",
                "else",
                "for",
                "while",
                "switch",
                "case",
                "return",
                "new",
                "this",
                "super",
                "class",
                "extends",
                "import",
                "export",
                "from",
                "async",
                "await",
                "function",
                "const",
                "let",
                "var",
                "undefined",
                "null",
                "Promise",
                "Array",
                "Object",
                "console",
                "require",
            ],
        );
        m
    })
}

pub fn is_language_keyword(language: &str, name: &str) -> bool {
    language_keywords()
        .get(language)
        .map(|kws| kws.iter().any(|k| *k == name))
        .unwrap_or(false)
}

/// push_import mirrors lines 657-672. Emits an `imports_from`
/// fact pointing at the imported module symbol. targetKind is
/// `module` for relative imports (starts with ".") and
/// `external` otherwise — matches the legacy ternary.
pub fn push_import(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    module_symbol: &str,
    file_path: &str,
    target: &str,
    line: u32,
) {
    facts.push(SyntacticAstFact {
        kind: FactKind::ImportsFrom,
        language: language.to_string(),
        source_symbol: module_symbol.to_string(),
        source_display_name: file_path.to_string(),
        source_kind: SymbolKind::Module,
        target_symbol: format!("module:{}", target),
        target_display_name: target.to_string(),
        target_kind: if target.starts_with('.') {
            SymbolKind::Module
        } else {
            SymbolKind::External
        },
        file_path: file_path.to_string(),
        start_line: line,
        end_line: line,
        confidence: CONFIDENCE_INFERRED,
    });
}

/// push_defines mirrors lines 674-689.
#[allow(clippy::too_many_arguments)]
pub fn push_defines(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    from_symbol: &str,
    from_display: &str,
    target_symbol: &str,
    target_display: &str,
    target_kind: SymbolKind,
    line: u32,
) {
    facts.push(SyntacticAstFact {
        kind: FactKind::Defines,
        language: language.to_string(),
        source_symbol: from_symbol.to_string(),
        source_display_name: from_display.to_string(),
        source_kind: SymbolKind::Module,
        target_symbol: target_symbol.to_string(),
        target_display_name: target_display.to_string(),
        target_kind,
        file_path: from_display.to_string(),
        start_line: line,
        end_line: line,
        confidence: CONFIDENCE_INFERRED,
    });
}

/// push_calls mirrors lines 691-706.
#[allow(clippy::too_many_arguments)]
pub fn push_calls(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    from_symbol: &str,
    from_display: &str,
    from_kind: SymbolKind,
    file_path: &str,
    callee: &str,
    line: u32,
) {
    facts.push(SyntacticAstFact {
        kind: FactKind::Calls,
        language: language.to_string(),
        source_symbol: from_symbol.to_string(),
        source_display_name: from_display.to_string(),
        source_kind: from_kind,
        target_symbol: format!("function:?:{}", callee),
        target_display_name: callee.to_string(),
        target_kind: SymbolKind::Function,
        file_path: file_path.to_string(),
        start_line: line,
        end_line: line,
        confidence: CONFIDENCE_INFERRED,
    });
}

// ===== Go extractor (legacy lines 79-133) =====

fn go_import_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*import\s+(?:"([^"]+)"|(?:\(\s*([^)]+?)\s*\)))/m
    // Rust regex doesn't support multiline `^` without (?m).
    // The legacy 'm' flag is the same in Rust's regex.
    // The single-group form: import "path"
    // The block form:        import ( "a" "b" )
    // Non-greedy `?` in group 2 — supported by `regex`.
    R.get_or_init(|| {
        Regex::new(r#"(?m)^\s*import\s+(?:"([^"]+)"|(?:\(\s*([^)]+?)\s*\)))"#).unwrap()
    })
}

fn go_fn_decl_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*func\s+(?:\([^)]+\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(/gm
    // Per-line application — we apply this against each line
    // individually, so we don't need the 'm' flag in Rust.
    R.get_or_init(|| {
        Regex::new(r#"^\s*func\s+(?:\([^)]+\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\("#).unwrap()
    })
}

fn go_call_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(/g
    R.get_or_init(|| {
        Regex::new(r#"\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\("#).unwrap()
    })
}

fn go_inner_import_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy inside the block: /"([^"]+)"/
    R.get_or_init(|| Regex::new(r#""([^"]+)""#).unwrap())
}

/// extract_go_facts mirrors the legacy `extractGoFacts`
/// (lines 84-133). For each Go source file:
///   1. Detect the import statement (single or block form) and
///      emit one `imports_from` fact per imported package.
///   2. Walk lines looking for `func` declarations. For each
///      function declaration, emit a `defines` fact and start
///      tracking brace depth.
///   3. Inside the function body (brace depth > 0), every
///      identifier followed by `(` becomes a candidate call;
///      filtered through `sanitise` and the Go keyword list.
pub fn extract_go_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    let mut facts: Vec<SyntacticAstFact> = Vec::new();
    let module_symbol = format!("module:{}", file_path);

    // ── Imports ────────────────────────────────────────────
    if let Some(caps) = go_import_re().captures(source) {
        if let Some(single) = caps.get(1) {
            push_import(
                &mut facts,
                "go",
                &module_symbol,
                file_path,
                single.as_str(),
                1,
            );
        } else if let Some(block) = caps.get(2) {
            for line in block.as_str().split('\n') {
                if let Some(m) = go_inner_import_re().captures(line) {
                    if let Some(pkg) = m.get(1) {
                        push_import(&mut facts, "go", &module_symbol, file_path, pkg.as_str(), 1);
                    }
                }
            }
        }
    }

    // ── Function declarations + calls inside the body ──────
    let lines: Vec<&str> = source.split('\n').collect();
    let mut current_func: Option<(String, u32)> = None;
    let mut brace_depth: i64 = 0;
    for (i, line) in lines.iter().enumerate() {
        let line_no = (i as u32) + 1;
        if let Some(fn_match) = go_fn_decl_re().captures(line) {
            if let Some(name) = fn_match.get(1) {
                let name_s = name.as_str().to_string();
                current_func = Some((name_s.clone(), line_no));
                brace_depth = count_char(line, '{') as i64 - count_char(line, '}') as i64;
                push_defines(
                    &mut facts,
                    "go",
                    &module_symbol,
                    file_path,
                    &format!("function:{}:{}", file_path, name_s),
                    &name_s,
                    SymbolKind::Function,
                    line_no,
                );
                continue;
            }
        }
        if let Some((func_name, _)) = current_func.as_ref() {
            // Critic 3.2: borrow current_func instead of
            // cloning it per iteration. Zero-behavior fix.
            brace_depth += count_char(line, '{') as i64 - count_char(line, '}') as i64;
            for m in go_call_re().captures_iter(line) {
                if let Some(callee_raw) = m.get(1) {
                    if let Some(callee) = sanitise(callee_raw.as_str()) {
                        if callee != *func_name && !is_language_keyword("go", &callee) {
                            push_calls(
                                &mut facts,
                                "go",
                                &format!("function:{}:{}", file_path, func_name),
                                func_name,
                                SymbolKind::Function,
                                file_path,
                                &callee,
                                line_no,
                            );
                        }
                    }
                }
            }
            if brace_depth <= 0 {
                current_func = None;
                brace_depth = 0;
            }
        }
    }

    facts
}

// ===== Python extractor (legacy lines 135-226) =====

fn python_import_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy:
    // /^\s*(?:from\s+([A-Za-z_][A-Za-z0-9_.]*)\s+import\s+([A-Za-z_,\s]+)|import\s+([A-Za-z_][A-Za-z0-9_.]*))/
    R.get_or_init(|| {
        Regex::new(
            r"^\s*(?:from\s+([A-Za-z_][A-Za-z0-9_.]*)\s+import\s+([A-Za-z_,\s]+)|import\s+([A-Za-z_][A-Za-z0-9_.]*))",
        )
        .unwrap()
    })
}

fn python_def_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^(\s*)(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(/
    R.get_or_init(|| Regex::new(r"^(\s*)(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(").unwrap())
}

fn python_class_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^(\s*)class\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(([^)]*)\))?/
    R.get_or_init(|| {
        Regex::new(r"^(\s*)class\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(([^)]*)\))?").unwrap()
    })
}

fn python_call_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(/g
    // Same shape as Go's call regex.
    R.get_or_init(|| {
        Regex::new(r"\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(").unwrap()
    })
}

fn python_leading_ws_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    R.get_or_init(|| Regex::new(r"^(\s*)").unwrap())
}

fn py_symbol_name_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    R.get_or_init(|| Regex::new(r"^[A-Za-z_][A-Za-z0-9_]*$").unwrap())
}

fn py_base_class_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    R.get_or_init(|| Regex::new(r"^[A-Za-z_][A-Za-z0-9_.]*$").unwrap())
}

fn py_as_split_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    R.get_or_init(|| Regex::new(r"\s+as\s+").unwrap())
}

/// extract_python_facts mirrors `extractPythonFacts`
/// (legacy lines 135-226).
///
/// Python differs from Go on three axes:
///   1. Scope is INDENT-tracked, not brace-tracked. The
///      current_func indent is captured at `def`; the function
///      exits when a non-empty line with leading indent <=
///      that is encountered.
///   2. Imports come in two forms: `import foo` (group 3) and
///      `from foo import a, b as c, d` (groups 1+2). The
///      from-form emits ONE imports_from for the module plus
///      ONE imports_from per imported symbol with
///      target_kind=symbol.
///   3. `class Foo(Bar, Baz):` emits a `defines` fact for the
///      class plus one `extends` fact per base in the
///      parenthesised list (filtered through the base-class
///      identifier regex to drop string-default kwargs etc).
pub fn extract_python_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    let mut facts: Vec<SyntacticAstFact> = Vec::new();
    let module_symbol = format!("module:{}", file_path);
    let lines: Vec<&str> = source.split('\n').collect();
    // (name, indent, line) — line is unused after capture but
    // kept for legacy struct parity.
    let mut current_func: Option<(String, usize, u32)> = None;

    for (i, line) in lines.iter().enumerate() {
        let line_no = (i as u32) + 1;

        // ── Imports ────────────────────────────────────────
        if let Some(caps) = python_import_re().captures(line) {
            // Either group 1 (from-form module) or group 3
            // (plain-import module) is present.
            let module_name = caps
                .get(1)
                .map(|m| m.as_str().to_string())
                .or_else(|| caps.get(3).map(|m| m.as_str().to_string()));
            if let Some(module_name) = module_name {
                push_import(
                    &mut facts,
                    "python",
                    &module_symbol,
                    file_path,
                    &module_name,
                    line_no,
                );
                // If from-form, also emit one symbol fact per
                // imported name, stripping `as alias` if any.
                if let Some(symbol_list) = caps.get(2) {
                    for sym in symbol_list.as_str().split(',') {
                        let trimmed = sym.trim();
                        // Legacy `sym.trim().split(/\s+as\s+/)[0]` —
                        // since trimmed is already whitespace-free
                        // on the outer edges and `\s+as\s+` consumes
                        // any inner whitespace around `as`, the
                        // first split piece needs no further trim.
                        let cleaned = py_as_split_re().split(trimmed).next().unwrap_or(trimmed);
                        if py_symbol_name_re().is_match(cleaned) {
                            facts.push(SyntacticAstFact {
                                kind: FactKind::ImportsFrom,
                                language: "python".to_string(),
                                source_symbol: module_symbol.clone(),
                                source_display_name: file_path.to_string(),
                                source_kind: SymbolKind::Module,
                                target_symbol: format!("symbol:{}:{}", module_name, cleaned),
                                target_display_name: format!("{}.{}", module_name, cleaned),
                                target_kind: SymbolKind::Symbol,
                                file_path: file_path.to_string(),
                                start_line: line_no,
                                end_line: line_no,
                                confidence: CONFIDENCE_INFERRED,
                            });
                        }
                    }
                }
            }
            continue;
        }

        // ── Function / class def (track indent) ────────────
        if let Some(def_match) = python_def_re().captures(line) {
            let indent = def_match.get(1).map(|m| m.as_str().len()).unwrap_or(0);
            let name = def_match
                .get(2)
                .map(|m| m.as_str().to_string())
                .unwrap_or_default();
            current_func = Some((name.clone(), indent, line_no));
            push_defines(
                &mut facts,
                "python",
                &module_symbol,
                file_path,
                &format!("function:{}:{}", file_path, name),
                &name,
                SymbolKind::Function,
                line_no,
            );
            continue;
        }
        if let Some(class_match) = python_class_re().captures(line) {
            let class_name = class_match
                .get(2)
                .map(|m| m.as_str().to_string())
                .unwrap_or_default();
            let class_symbol = format!("class:{}:{}", file_path, class_name);
            push_defines(
                &mut facts,
                "python",
                &module_symbol,
                file_path,
                &class_symbol,
                &class_name,
                SymbolKind::Class,
                line_no,
            );
            if let Some(bases) = class_match.get(3) {
                for base_raw in bases.as_str().split(',') {
                    let base = base_raw.trim();
                    if base.is_empty() {
                        continue;
                    }
                    if py_base_class_re().is_match(base) {
                        facts.push(SyntacticAstFact {
                            kind: FactKind::Extends,
                            language: "python".to_string(),
                            source_symbol: class_symbol.clone(),
                            source_display_name: class_name.clone(),
                            source_kind: SymbolKind::Class,
                            target_symbol: format!("class:?:{}", base),
                            target_display_name: base.to_string(),
                            target_kind: SymbolKind::Class,
                            file_path: file_path.to_string(),
                            start_line: line_no,
                            end_line: line_no,
                            confidence: CONFIDENCE_INFERRED,
                        });
                    }
                }
            }
            continue;
        }

        // ── Calls inside the current function ──────────────
        // Critic 3.1: borrow current_func instead of cloning
        // per iteration. Mirrors the post-critic R.9a Go fix.
        if let Some((func_name, func_indent, _)) = current_func.as_ref() {
            let leading = python_leading_ws_re()
                .captures(line)
                .and_then(|c| c.get(1))
                .map(|m| m.as_str().len())
                .unwrap_or(0);
            if !line.trim().is_empty() && leading <= *func_indent {
                current_func = None;
            } else {
                for m in python_call_re().captures_iter(line) {
                    if let Some(callee_raw) = m.get(1) {
                        if let Some(callee) = sanitise(callee_raw.as_str()) {
                            if callee != *func_name && !is_language_keyword("python", &callee) {
                                push_calls(
                                    &mut facts,
                                    "python",
                                    &format!("function:{}:{}", file_path, func_name),
                                    func_name,
                                    SymbolKind::Function,
                                    file_path,
                                    &callee,
                                    line_no,
                                );
                            }
                        }
                    }
                }
            }
        }
    }

    facts
}

// ===== Java extractor (legacy lines 228-319) =====

fn java_import_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*import\s+(?:static\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*;/
    R.get_or_init(|| {
        Regex::new(r"^\s*import\s+(?:static\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*;").unwrap()
    })
}

fn java_class_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /\b(?:public\s+|private\s+|protected\s+|abstract\s+|final\s+|static\s+)*
    //          (?:class|interface)\s+([A-Za-z_][A-Za-z0-9_]*)
    //          (?:\s+extends\s+([A-Za-z_][A-Za-z0-9_.<>]*))?
    //          (?:\s+implements\s+([A-Za-z_][A-Za-z0-9_.<>,\s]+?))?
    //          \s*\{/
    R.get_or_init(|| {
        Regex::new(
            r"\b(?:public\s+|private\s+|protected\s+|abstract\s+|final\s+|static\s+)*(?:class|interface)\s+([A-Za-z_][A-Za-z0-9_]*)(?:\s+extends\s+([A-Za-z_][A-Za-z0-9_.<>]*))?(?:\s+implements\s+([A-Za-z_][A-Za-z0-9_.<>,\s]+?))?\s*\{",
        )
        .unwrap()
    })
}

fn java_method_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /\b(?:public\s+|private\s+|protected\s+|static\s+|final\s+|abstract\s+|synchronized\s+)*
    //          [A-Za-z_<>\[\]?]+\s+([A-Za-z_][A-Za-z0-9_]*)\s*\([^)]*\)
    //          \s*(?:throws\s+[A-Za-z_,\s.]+)?\s*\{?/
    R.get_or_init(|| {
        Regex::new(
            r"\b(?:public\s+|private\s+|protected\s+|static\s+|final\s+|abstract\s+|synchronized\s+)*[A-Za-z_<>\[\]?]+\s+([A-Za-z_][A-Za-z0-9_]*)\s*\([^)]*\)\s*(?:throws\s+[A-Za-z_,\s.]+)?\s*\{?",
        )
        .unwrap()
    })
}

fn java_class_or_interface_kw_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy guard: /(class|interface)\s+/
    R.get_or_init(|| Regex::new(r"(class|interface)\s+").unwrap())
}

fn java_call_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Same shape as Go/Python.
    R.get_or_init(|| {
        Regex::new(r"\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(").unwrap()
    })
}

fn java_identifier_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    R.get_or_init(|| Regex::new(r"^[A-Za-z_][A-Za-z0-9_.]*$").unwrap())
}

/// Local-scope tracker for the Java extractor. Mirrors the
/// legacy `currentScope` struct (line 242).
#[derive(Debug, Clone)]
struct JavaScope {
    symbol: String,
    name: String,
    kind: JavaScopeKind,
    depth: i64,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum JavaScopeKind {
    Class,
    /// Preserved for legacy union-shape parity. The legacy
    /// `currentScope.kind` union includes `"method"` but the
    /// legacy code never sets it — only `"class"` is ever
    /// assigned. Kept here so future slices that DO start
    /// tracking method scope can flip this without changing
    /// the type signature.
    #[allow(dead_code)]
    Method,
}

/// extract_java_facts mirrors `extractJavaFacts`
/// (legacy lines 228-319).
///
/// Three sub-passes:
///   1. Imports — single regex per line for `import [static]
///      X.Y.Z;`.
///   2. Class + method decls — combined loop tracking brace
///      depth + currentScope. Inside a class, method
///      declarations emit `defines` facts attributed to the
///      class. extends/implements emit per-base facts with
///      generic types stripped via `split("<")[0]`. The
///      `class|interface` keyword guard prevents the method
///      regex from misfiring on class lines.
///   3. Calls — simple top-to-bottom pass. Every `name(` is
///      a candidate; rejected when the FIRST dotted segment
///      is a Java keyword (so `String.format()` is rejected
///      because "String" is in the Java keyword set). Calls
///      are attributed to MODULE scope (not class/method) —
///      this matches the legacy comment + emission which says
///      "simplified" but actually unconditionally uses
///      `moduleSymbol`.
pub fn extract_java_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    let mut facts: Vec<SyntacticAstFact> = Vec::new();
    let module_symbol = format!("module:{}", file_path);
    let lines: Vec<&str> = source.split('\n').collect();

    // ── Pass 1: Imports ────────────────────────────────────
    for (i, line) in lines.iter().enumerate() {
        if let Some(caps) = java_import_re().captures(line) {
            if let Some(pkg) = caps.get(1) {
                push_import(
                    &mut facts,
                    "java",
                    &module_symbol,
                    file_path,
                    pkg.as_str(),
                    (i as u32) + 1,
                );
            }
        }
    }

    // ── Pass 2: Class + method declarations ────────────────
    let mut current_scope: Option<JavaScope> = None;
    let mut brace_depth: i64 = 0;
    for (i, line) in lines.iter().enumerate() {
        let line_no = (i as u32) + 1;

        if let Some(class_match) = java_class_re().captures(line) {
            let class_name = class_match
                .get(1)
                .map(|m| m.as_str().to_string())
                .unwrap_or_default();
            let class_symbol = format!("class:{}:{}", file_path, class_name);
            push_defines(
                &mut facts,
                "java",
                &module_symbol,
                file_path,
                &class_symbol,
                &class_name,
                SymbolKind::Class,
                line_no,
            );
            // extends — generic stripped via `.split('<')[0]`.
            if let Some(parent_raw) = class_match.get(2) {
                let parent = parent_raw.as_str().split('<').next().unwrap_or("").trim();
                if java_identifier_re().is_match(parent) {
                    facts.push(SyntacticAstFact {
                        kind: FactKind::Extends,
                        language: "java".to_string(),
                        source_symbol: class_symbol.clone(),
                        source_display_name: class_name.clone(),
                        source_kind: SymbolKind::Class,
                        target_symbol: format!("class:?:{}", parent),
                        target_display_name: parent.to_string(),
                        target_kind: SymbolKind::Class,
                        file_path: file_path.to_string(),
                        start_line: line_no,
                        end_line: line_no,
                        confidence: CONFIDENCE_INFERRED,
                    });
                }
            }
            // implements — split by comma, strip generic per item.
            if let Some(implements_raw) = class_match.get(3) {
                for iface_raw in implements_raw.as_str().split(',') {
                    let iface = iface_raw.split('<').next().unwrap_or("").trim();
                    if iface.is_empty() {
                        continue;
                    }
                    if java_identifier_re().is_match(iface) {
                        facts.push(SyntacticAstFact {
                            kind: FactKind::Implements,
                            language: "java".to_string(),
                            source_symbol: class_symbol.clone(),
                            source_display_name: class_name.clone(),
                            source_kind: SymbolKind::Class,
                            target_symbol: format!("interface:?:{}", iface),
                            target_display_name: iface.to_string(),
                            target_kind: SymbolKind::Interface,
                            file_path: file_path.to_string(),
                            start_line: line_no,
                            end_line: line_no,
                            confidence: CONFIDENCE_INFERRED,
                        });
                    }
                }
            }
            current_scope = Some(JavaScope {
                symbol: class_symbol,
                name: class_name,
                kind: JavaScopeKind::Class,
                depth: brace_depth,
            });
        }

        // Method decl inside class — guarded by the
        // class/interface keyword check so the regex doesn't
        // misfire on class declaration lines.
        if let Some(method_match) = java_method_re().captures(line) {
            if let Some(scope) = current_scope.as_ref() {
                if scope.kind == JavaScopeKind::Class
                    && !java_class_or_interface_kw_re().is_match(line)
                {
                    if let Some(method_name) = method_match.get(1) {
                        let name = method_name.as_str();
                        if !is_language_keyword("java", name) {
                            let method_symbol = format!("{}#{}", scope.symbol, name);
                            push_defines(
                                &mut facts,
                                "java",
                                &scope.symbol,
                                &scope.name,
                                &method_symbol,
                                name,
                                SymbolKind::Method,
                                line_no,
                            );
                        }
                    }
                }
            }
        }

        brace_depth += count_char(line, '{') as i64 - count_char(line, '}') as i64;
        if let Some(scope) = current_scope.as_ref() {
            if brace_depth <= scope.depth {
                current_scope = None;
            }
        }
    }

    // ── Pass 3: Calls ──────────────────────────────────────
    // Always attributed to module scope, regardless of which
    // class/method the line is in. Matches legacy line 314's
    // `pushCalls(facts, "java", moduleSymbol, filePath, "module", ...)`.
    for (i, line) in lines.iter().enumerate() {
        let line_no = (i as u32) + 1;
        for m in java_call_re().captures_iter(line) {
            if let Some(callee_raw) = m.get(1) {
                if let Some(callee) = sanitise(callee_raw.as_str()) {
                    // Keyword check on the FIRST dotted
                    // segment only — `String.format` rejects
                    // because "String" is in the Java keyword
                    // set.
                    let first_seg = callee.split('.').next().unwrap_or(&callee);
                    if !is_language_keyword("java", first_seg) {
                        push_calls(
                            &mut facts,
                            "java",
                            &module_symbol,
                            file_path,
                            SymbolKind::Module,
                            file_path,
                            &callee,
                            line_no,
                        );
                    }
                }
            }
        }
    }

    facts
}

// ===== Ruby extractor (legacy lines 321-374) =====

fn ruby_require_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*require(?:_relative)?\s+["']([^"']+)["']/
    R.get_or_init(|| Regex::new(r#"^\s*require(?:_relative)?\s+["']([^"']+)["']"#).unwrap())
}

fn ruby_class_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)(?:\s*<\s*([A-Za-z_][A-Za-z0-9_:]*))?/
    // The base class can include `:` for Ruby's `::` namespace
    // separator (e.g. `Foo::Bar`).
    R.get_or_init(|| {
        Regex::new(r"^\s*class\s+([A-Za-z_][A-Za-z0-9_]*)(?:\s*<\s*([A-Za-z_][A-Za-z0-9_:]*))?")
            .unwrap()
    })
}

fn ruby_def_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*def\s+(?:self\.)?([A-Za-z_][A-Za-z0-9_!?=]*)/
    // The `self.` prefix is optional and dropped from the
    // captured name. Method names can end with `!`, `?`, `=`
    // (Ruby naming convention for mutating/predicate/setter
    // methods).
    R.get_or_init(|| Regex::new(r"^\s*def\s+(?:self\.)?([A-Za-z_][A-Za-z0-9_!?=]*)").unwrap())
}

fn ruby_end_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*end\b/
    R.get_or_init(|| Regex::new(r"^\s*end\b").unwrap())
}

fn ruby_call_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Same shape as Go/Python/Java.
    R.get_or_init(|| {
        Regex::new(r"\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(").unwrap()
    })
}

/// extract_ruby_facts mirrors `extractRubyFacts`
/// (legacy lines 321-374).
///
/// Ruby specifics:
///   - Imports come in two forms: `require "x"` (gem/std-lib)
///     and `require_relative "y"` (path relative to current
///     file). The legacy single regex handles both since
///     `(?:_relative)?` is optional.
///   - `class Foo < Bar` emits `defines` for `Foo` plus an
///     `extends` fact for `Bar`. The base-class identifier
///     regex permits `:` for the `Foo::Bar` namespace form.
///   - `def name` and `def self.name` both produce a function
///     defines; the `self.` prefix is dropped from the name
///     captured by the regex.
///   - Scope is tracked by `end` keyword (the legacy doesn't
///     track nested `end`s precisely — any top-level `end`
///     pops the current function). Mirror that quirk.
///   - First-dotted-segment keyword filter on callees, same as
///     Java.
pub fn extract_ruby_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    let mut facts: Vec<SyntacticAstFact> = Vec::new();
    let module_symbol = format!("module:{}", file_path);
    let lines: Vec<&str> = source.split('\n').collect();
    let mut current_func: Option<String> = None;

    for (i, line) in lines.iter().enumerate() {
        let line_no = (i as u32) + 1;

        // ── Require / require_relative ─────────────────────
        if let Some(caps) = ruby_require_re().captures(line) {
            if let Some(target) = caps.get(1) {
                push_import(
                    &mut facts,
                    "ruby",
                    &module_symbol,
                    file_path,
                    target.as_str(),
                    line_no,
                );
            }
            continue;
        }

        // ── class Foo (< Bar)? ─────────────────────────────
        if let Some(class_match) = ruby_class_re().captures(line) {
            let class_name = class_match
                .get(1)
                .map(|m| m.as_str().to_string())
                .unwrap_or_default();
            let class_symbol = format!("class:{}:{}", file_path, class_name);
            push_defines(
                &mut facts,
                "ruby",
                &module_symbol,
                file_path,
                &class_symbol,
                &class_name,
                SymbolKind::Class,
                line_no,
            );
            if let Some(parent) = class_match.get(2) {
                facts.push(SyntacticAstFact {
                    kind: FactKind::Extends,
                    language: "ruby".to_string(),
                    source_symbol: class_symbol,
                    source_display_name: class_name,
                    source_kind: SymbolKind::Class,
                    target_symbol: format!("class:?:{}", parent.as_str()),
                    target_display_name: parent.as_str().to_string(),
                    target_kind: SymbolKind::Class,
                    file_path: file_path.to_string(),
                    start_line: line_no,
                    end_line: line_no,
                    confidence: CONFIDENCE_INFERRED,
                });
            }
            continue;
        }

        // ── def name / def self.name ───────────────────────
        if let Some(def_match) = ruby_def_re().captures(line) {
            if let Some(name) = def_match.get(1) {
                let name_s = name.as_str().to_string();
                current_func = Some(name_s.clone());
                push_defines(
                    &mut facts,
                    "ruby",
                    &module_symbol,
                    file_path,
                    &format!("function:{}:{}", file_path, name_s),
                    &name_s,
                    SymbolKind::Function,
                    line_no,
                );
                continue;
            }
        }

        // ── end pops current scope ─────────────────────────
        // Legacy quirk: any `end` (not just the matching one)
        // pops current_func. So a `class Foo; def bar; end; end`
        // sequence pops on the first `end` (the def's end),
        // which is correct; but `def bar; if x; end; end`
        // would pop on the inner `if`'s `end` — losing the
        // `def bar` scope prematurely. Mirror legacy as-is;
        // this is a known low-precision quirk of the regex-
        // grade extractor.
        if ruby_end_re().is_match(line) {
            current_func = None;
        }

        // ── Calls inside current function ──────────────────
        if let Some(func_name) = current_func.as_ref() {
            for m in ruby_call_re().captures_iter(line) {
                if let Some(callee_raw) = m.get(1) {
                    if let Some(callee) = sanitise(callee_raw.as_str()) {
                        let first_seg = callee.split('.').next().unwrap_or(&callee);
                        if callee != *func_name && !is_language_keyword("ruby", first_seg) {
                            push_calls(
                                &mut facts,
                                "ruby",
                                &format!("function:{}:{}", file_path, func_name),
                                func_name,
                                SymbolKind::Function,
                                file_path,
                                &callee,
                                line_no,
                            );
                        }
                    }
                }
            }
        }
    }

    facts
}

// ===== C# extractor (legacy lines 376-419) =====

fn csharp_using_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*using\s+(?:static\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*;/
    R.get_or_init(|| {
        Regex::new(r"^\s*using\s+(?:static\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*;").unwrap()
    })
}

fn csharp_class_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /\b(?:public|private|protected|internal|abstract|sealed|static)\s+
    //          (?:[A-Za-z_]+\s+)*
    //          (?:class|interface|struct|record)\s+
    //          ([A-Za-z_][A-Za-z0-9_]*)
    //          (?:\s*:\s*([A-Za-z_][A-Za-z0-9_,<>\s.]*))?/
    // Legacy quirk preserved: a visibility modifier is
    // REQUIRED — a bare `class Foo {}` won't match. Mirror
    // exactly.
    R.get_or_init(|| {
        Regex::new(
            r"\b(?:public|private|protected|internal|abstract|sealed|static)\s+(?:[A-Za-z_]+\s+)*(?:class|interface|struct|record)\s+([A-Za-z_][A-Za-z0-9_]*)(?:\s*:\s*([A-Za-z_][A-Za-z0-9_,<>\s.]*))?",
        )
        .unwrap()
    })
}

fn csharp_call_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    R.get_or_init(|| {
        Regex::new(r"\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(").unwrap()
    })
}

fn csharp_identifier_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    R.get_or_init(|| Regex::new(r"^[A-Za-z_][A-Za-z0-9_.]*$").unwrap())
}

/// extract_csharp_facts mirrors `extractCSharpFacts`
/// (legacy lines 376-419).
///
/// C# specifics:
///   - Imports via `using X;` or `using static X.Y.Z;`. One
///     regex covers both.
///   - Class/interface/struct/record declarations with
///     optional `: Base, Iface1, Iface2` colon-separated base
///     list. Comma-split with per-item generic stripping via
///     `split('<')[0]`. Identifier-regex filter rejects
///     non-identifier fragments (same quirk as Java).
///   - Legacy quirk preserved: a visibility modifier
///     (public/private/protected/internal/abstract/sealed/
///     static) is REQUIRED on the class line — bare
///     `class Foo {}` doesn't match.
///   - All four kinds (class/interface/struct/record) emit
///     `defines` with `target_kind=Class` (matches legacy's
///     uniform `pushDefines(..., "class", ...)`).
///   - Calls: line-level sweep with NO scope filter,
///     attributed to MODULE scope. First-dotted-segment
///     keyword filter via `callee.split(".")[0]` (Java-style).
///     Done INLINE on the main loop (not a separate pass like
///     Java), so call emission ordering interleaves with
///     class+import. Mirror legacy exactly.
pub fn extract_csharp_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    let mut facts: Vec<SyntacticAstFact> = Vec::new();
    let module_symbol = format!("module:{}", file_path);
    let lines: Vec<&str> = source.split('\n').collect();

    for (i, line) in lines.iter().enumerate() {
        let line_no = (i as u32) + 1;

        // ── using X; / using static X.Y.Z; ─────────────────
        if let Some(caps) = csharp_using_re().captures(line) {
            if let Some(target) = caps.get(1) {
                push_import(
                    &mut facts,
                    "csharp",
                    &module_symbol,
                    file_path,
                    target.as_str(),
                    line_no,
                );
            }
        }

        // ── class/interface/struct/record decl ─────────────
        if let Some(class_match) = csharp_class_re().captures(line) {
            let class_name = class_match
                .get(1)
                .map(|m| m.as_str().to_string())
                .unwrap_or_default();
            let class_symbol = format!("class:{}:{}", file_path, class_name);
            push_defines(
                &mut facts,
                "csharp",
                &module_symbol,
                file_path,
                &class_symbol,
                &class_name,
                SymbolKind::Class,
                line_no,
            );
            // Base list — comma-split, per-item generic
            // stripped, identifier filtered.
            if let Some(base_list) = class_match.get(2) {
                for base_raw in base_list.as_str().split(',') {
                    let base = base_raw.split('<').next().unwrap_or("").trim();
                    if base.is_empty() {
                        continue;
                    }
                    if csharp_identifier_re().is_match(base) {
                        facts.push(SyntacticAstFact {
                            kind: FactKind::Extends,
                            language: "csharp".to_string(),
                            source_symbol: class_symbol.clone(),
                            source_display_name: class_name.clone(),
                            source_kind: SymbolKind::Class,
                            target_symbol: format!("class:?:{}", base),
                            target_display_name: base.to_string(),
                            target_kind: SymbolKind::Class,
                            file_path: file_path.to_string(),
                            start_line: line_no,
                            end_line: line_no,
                            confidence: CONFIDENCE_INFERRED,
                        });
                    }
                }
            }
        }

        // ── Calls (inline; attributed to module scope) ─────
        for m in csharp_call_re().captures_iter(line) {
            if let Some(callee_raw) = m.get(1) {
                if let Some(callee) = sanitise(callee_raw.as_str()) {
                    let first_seg = callee.split('.').next().unwrap_or(&callee);
                    if !is_language_keyword("csharp", first_seg) {
                        push_calls(
                            &mut facts,
                            "csharp",
                            &module_symbol,
                            file_path,
                            SymbolKind::Module,
                            file_path,
                            &callee,
                            line_no,
                        );
                    }
                }
            }
        }
    }

    facts
}

// ===== Rust extractor (legacy lines 421-461) =====

fn rust_use_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*use\s+([A-Za-z_][A-Za-z0-9_:]*)(?:\s*::\s*\{[^}]*\})?\s*;/
    // Captures the first path identifier only — the brace
    // group is consumed but not captured, so `use foo::{a, b}`
    // emits a single `imports_from` fact for "foo".
    R.get_or_init(|| {
        Regex::new(r"^\s*use\s+([A-Za-z_][A-Za-z0-9_:]*)(?:\s*::\s*\{[^}]*\})?\s*;").unwrap()
    })
}

fn rust_fn_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z_][A-Za-z0-9_]*)\s*[<\(]/
    // The trailing `[<\(]` requires either a generic or
    // arg-list — that's what distinguishes a function decl
    // from a stray `fn` token elsewhere.
    R.get_or_init(|| {
        Regex::new(r"^\s*(?:pub\s+)?(?:async\s+)?fn\s+([A-Za-z_][A-Za-z0-9_]*)\s*[<(]").unwrap()
    })
}

fn rust_struct_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*(?:pub\s+)?(?:struct|enum|trait)\s+([A-Za-z_][A-Za-z0-9_]*)/
    R.get_or_init(|| {
        Regex::new(r"^\s*(?:pub\s+)?(?:struct|enum|trait)\s+([A-Za-z_][A-Za-z0-9_]*)").unwrap()
    })
}

fn rust_call_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /\b([A-Za-z_][A-Za-z0-9_]*(?:::[A-Za-z_][A-Za-z0-9_]*)*)\s*\(/g
    // Note: `::` separator (Rust path syntax), NOT `.` like
    // Go/Java/Python/C#/Ruby. Required for `Vec::new()`,
    // `std::fs::read()` etc.
    R.get_or_init(|| {
        Regex::new(r"\b([A-Za-z_][A-Za-z0-9_]*(?:::[A-Za-z_][A-Za-z0-9_]*)*)\s*\(").unwrap()
    })
}

/// extract_rust_facts mirrors `extractRustFacts`
/// (legacy lines 421-461).
///
/// Rust specifics:
///   - `use foo::bar;` / `use foo::{a, b};` — single regex
///     with optional non-capturing brace group. Only the head
///     identifier (`foo`) is captured; the brace contents are
///     consumed but discarded. Legacy behavior preserved.
///   - `fn name`, `pub fn name`, `async fn name`, `pub async
///     fn name` — all variants matched. The trailing
///     `[<(]` requires either generic params or an arg list.
///   - `struct`/`enum`/`trait` declarations emit `defines`
///     with `target_kind=Class` (legacy uses the same
///     `pushDefines(..., "class", ...)` call site for all
///     three kinds).
///   - Brace-tracked scope for fn body — `current_fn` is set
///     at fn decl, exited when brace_depth falls to 0 or
///     below.
///   - Calls inside fn body use `::` separator (Rust path
///     syntax). The first segment of a dotted callee like
///     `Vec::new` is checked against the Rust keyword set —
///     so `Vec::new()` is REJECTED because "Vec" is in the
///     Rust keyword set (per
///     syntacticAstExtractor.ts:649). This is a legacy quirk
///     that effectively filters out most stdlib calls,
///     preserved verbatim for parity.
pub fn extract_rust_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    let mut facts: Vec<SyntacticAstFact> = Vec::new();
    let module_symbol = format!("module:{}", file_path);
    let lines: Vec<&str> = source.split('\n').collect();
    let mut current_fn: Option<String> = None;
    let mut brace_depth: i64 = 0;

    for (i, line) in lines.iter().enumerate() {
        let line_no = (i as u32) + 1;

        // ── use foo::bar(::{a, b})? ; ──────────────────────
        if let Some(caps) = rust_use_re().captures(line) {
            if let Some(target) = caps.get(1) {
                push_import(
                    &mut facts,
                    "rust",
                    &module_symbol,
                    file_path,
                    target.as_str(),
                    line_no,
                );
            }
            continue;
        }

        // ── fn name ────────────────────────────────────────
        if let Some(fn_match) = rust_fn_re().captures(line) {
            if let Some(name) = fn_match.get(1) {
                let name_s = name.as_str().to_string();
                current_fn = Some(name_s.clone());
                push_defines(
                    &mut facts,
                    "rust",
                    &module_symbol,
                    file_path,
                    &format!("function:{}:{}", file_path, name_s),
                    &name_s,
                    SymbolKind::Function,
                    line_no,
                );
                brace_depth = count_char(line, '{') as i64 - count_char(line, '}') as i64;
                continue;
            }
        }

        // ── struct/enum/trait ──────────────────────────────
        if let Some(struct_match) = rust_struct_re().captures(line) {
            if let Some(name) = struct_match.get(1) {
                let name_s = name.as_str().to_string();
                push_defines(
                    &mut facts,
                    "rust",
                    &module_symbol,
                    file_path,
                    &format!("class:{}:{}", file_path, name_s),
                    &name_s,
                    SymbolKind::Class,
                    line_no,
                );
            }
        }

        // ── Calls inside current_fn body ───────────────────
        if let Some(fn_name) = current_fn.as_ref() {
            brace_depth += count_char(line, '{') as i64 - count_char(line, '}') as i64;
            for m in rust_call_re().captures_iter(line) {
                if let Some(callee_raw) = m.get(1) {
                    if let Some(callee) = sanitise(callee_raw.as_str()) {
                        // Split on `::` for first-segment
                        // keyword filter — Rust path syntax.
                        let first_seg = callee.split("::").next().unwrap_or(&callee);
                        if callee != *fn_name && !is_language_keyword("rust", first_seg) {
                            push_calls(
                                &mut facts,
                                "rust",
                                &format!("function:{}:{}", file_path, fn_name),
                                fn_name,
                                SymbolKind::Function,
                                file_path,
                                &callee,
                                line_no,
                            );
                        }
                    }
                }
            }
            if brace_depth <= 0 {
                current_fn = None;
                brace_depth = 0;
            }
        }
    }

    facts
}

// ===== Dart extractor (legacy lines 463-504) =====

fn dart_import_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*import\s+['"]([^'"]+)['"]/ — supports both
    // single and double-quote forms via the [...]'"... char
    // class.
    R.get_or_init(|| Regex::new(r#"^\s*import\s+['"]([^'"]+)['"]"#).unwrap())
}

fn dart_class_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    // Legacy: /^\s*(?:abstract\s+)?class\s+([A-Za-z_][A-Za-z0-9_]*)
    //          (?:\s+extends\s+([A-Za-z_][A-Za-z0-9_<>]*))?
    //          (?:\s+implements\s+([A-Za-z_][A-Za-z0-9_<>,\s]*))?/
    // Group 3 (implements list) is captured but NEVER used by
    // the legacy emission — Dart only emits the extends fact.
    R.get_or_init(|| {
        Regex::new(
            r"^\s*(?:abstract\s+)?class\s+([A-Za-z_][A-Za-z0-9_]*)(?:\s+extends\s+([A-Za-z_][A-Za-z0-9_<>]*))?(?:\s+implements\s+([A-Za-z_][A-Za-z0-9_<>,\s]*))?",
        )
        .unwrap()
    })
}

fn dart_call_re() -> &'static Regex {
    static R: OnceLock<Regex> = OnceLock::new();
    R.get_or_init(|| {
        Regex::new(r"\b([A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*)\s*\(").unwrap()
    })
}

/// extract_dart_facts mirrors `extractDartFacts`
/// (legacy lines 463-504).
///
/// Dart specifics:
///   - Imports via `import "x"` or `import 'x'` — both quote
///     styles supported. The path inside the quotes goes into
///     `target` verbatim (no transformation; package URLs like
///     `package:flutter/material.dart` pass through as-is).
///   - Class decl with optional `abstract` prefix.
///   - **`extends` is emitted** with generic stripping via
///     `.split('<')[0].trim()`. UNLIKE Java/C#, the legacy
///     does NOT validate the parent against an identifier
///     regex — whatever `.split('<')[0].trim()` produces is
///     emitted verbatim.
///   - **`implements` is captured but NEVER emitted.** Legacy
///     quirk: the class regex has a group 3 for the
///     implements list but the emission block only references
///     classM[2] (extends). Preserved for parity. Pinned by a
///     regression-guard test so a future "improvement" can't
///     silently break parity.
///   - Calls inline, attributed to MODULE scope, first-
///     dotted-segment keyword filter.
pub fn extract_dart_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    let mut facts: Vec<SyntacticAstFact> = Vec::new();
    let module_symbol = format!("module:{}", file_path);
    let lines: Vec<&str> = source.split('\n').collect();

    for (i, line) in lines.iter().enumerate() {
        let line_no = (i as u32) + 1;

        // ── import "x" / 'x' ───────────────────────────────
        if let Some(caps) = dart_import_re().captures(line) {
            if let Some(target) = caps.get(1) {
                push_import(
                    &mut facts,
                    "dart",
                    &module_symbol,
                    file_path,
                    target.as_str(),
                    line_no,
                );
            }
            continue;
        }

        // ── class decl with optional extends ───────────────
        if let Some(class_match) = dart_class_re().captures(line) {
            let class_name = class_match
                .get(1)
                .map(|m| m.as_str().to_string())
                .unwrap_or_default();
            let class_symbol = format!("class:{}:{}", file_path, class_name);
            push_defines(
                &mut facts,
                "dart",
                &module_symbol,
                file_path,
                &class_symbol,
                &class_name,
                SymbolKind::Class,
                line_no,
            );
            // extends — generic stripping; NO identifier
            // validation (legacy emits whatever
            // `.split('<')[0].trim()` produces).
            if let Some(parent_raw) = class_match.get(2) {
                let parent = parent_raw.as_str().split('<').next().unwrap_or("").trim();
                facts.push(SyntacticAstFact {
                    kind: FactKind::Extends,
                    language: "dart".to_string(),
                    source_symbol: class_symbol.clone(),
                    source_display_name: class_name.clone(),
                    source_kind: SymbolKind::Class,
                    target_symbol: format!("class:?:{}", parent),
                    target_display_name: parent.to_string(),
                    target_kind: SymbolKind::Class,
                    file_path: file_path.to_string(),
                    start_line: line_no,
                    end_line: line_no,
                    confidence: CONFIDENCE_INFERRED,
                });
            }
            // NOTE: classM[3] (implements list) is INTENTIONALLY
            // NOT emitted — legacy quirk preserved verbatim.
        }

        // ── Calls (inline; attributed to module scope) ─────
        for m in dart_call_re().captures_iter(line) {
            if let Some(callee_raw) = m.get(1) {
                if let Some(callee) = sanitise(callee_raw.as_str()) {
                    let first_seg = callee.split('.').next().unwrap_or(&callee);
                    if !is_language_keyword("dart", first_seg) {
                        push_calls(
                            &mut facts,
                            "dart",
                            &module_symbol,
                            file_path,
                            SymbolKind::Module,
                            file_path,
                            &callee,
                            line_no,
                        );
                    }
                }
            }
        }
    }

    facts
}

// ===== TypeScript / JavaScript tree-sitter extractor =====

#[derive(Debug, Clone)]
struct TsScope {
    symbol: String,
    display: String,
    kind: SymbolKind,
}

fn parsed_line(node: Node<'_>) -> u32 {
    (node.start_position().row as u32) + 1
}

fn parsed_text(node: Node<'_>, source: &[u8]) -> Option<String> {
    node.utf8_text(source)
        .ok()
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(ToString::to_string)
}

fn parsed_fact(mut fact: SyntacticAstFact) -> SyntacticAstFact {
    fact.confidence = CONFIDENCE_PARSED;
    fact
}

fn push_parsed_import(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    module_symbol: &str,
    file_path: &str,
    target: &str,
    line: u32,
) {
    facts.push(parsed_fact(SyntacticAstFact {
        kind: FactKind::ImportsFrom,
        language: language.to_string(),
        source_symbol: module_symbol.to_string(),
        source_display_name: file_path.to_string(),
        source_kind: SymbolKind::Module,
        target_symbol: format!("module:{}", target),
        target_display_name: target.to_string(),
        target_kind: if target.starts_with('.') {
            SymbolKind::Module
        } else {
            SymbolKind::External
        },
        file_path: file_path.to_string(),
        start_line: line,
        end_line: line,
        confidence: CONFIDENCE_INFERRED,
    }));
}

#[allow(clippy::too_many_arguments)]
fn push_parsed_defines(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    from: &TsScope,
    file_path: &str,
    target_symbol: &str,
    target_display: &str,
    target_kind: SymbolKind,
    line: u32,
) {
    facts.push(parsed_fact(SyntacticAstFact {
        kind: FactKind::Defines,
        language: language.to_string(),
        source_symbol: from.symbol.clone(),
        source_display_name: from.display.clone(),
        source_kind: from.kind,
        target_symbol: target_symbol.to_string(),
        target_display_name: target_display.to_string(),
        target_kind,
        file_path: file_path.to_string(),
        start_line: line,
        end_line: line,
        confidence: CONFIDENCE_INFERRED,
    }));
}

fn push_parsed_extends(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    source: &TsScope,
    file_path: &str,
    parent: &str,
    line: u32,
) {
    facts.push(parsed_fact(SyntacticAstFact {
        kind: FactKind::Extends,
        language: language.to_string(),
        source_symbol: source.symbol.clone(),
        source_display_name: source.display.clone(),
        source_kind: SymbolKind::Class,
        target_symbol: format!("class:?:{}", parent),
        target_display_name: parent.to_string(),
        target_kind: SymbolKind::Class,
        file_path: file_path.to_string(),
        start_line: line,
        end_line: line,
        confidence: CONFIDENCE_INFERRED,
    }));
}

fn push_parsed_type_relation(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    kind: FactKind,
    source: &TsScope,
    file_path: &str,
    target_prefix: &str,
    target: &str,
    target_kind: SymbolKind,
    line: u32,
) {
    facts.push(parsed_fact(SyntacticAstFact {
        kind,
        language: language.to_string(),
        source_symbol: source.symbol.clone(),
        source_display_name: source.display.clone(),
        source_kind: source.kind,
        target_symbol: format!("{}:?:{}", target_prefix, target),
        target_display_name: target.to_string(),
        target_kind,
        file_path: file_path.to_string(),
        start_line: line,
        end_line: line,
        confidence: CONFIDENCE_INFERRED,
    }));
}

#[allow(clippy::too_many_arguments)]
fn push_parsed_calls(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    from: &TsScope,
    file_path: &str,
    callee: &str,
    line: u32,
) {
    facts.push(parsed_fact(SyntacticAstFact {
        kind: FactKind::Calls,
        language: language.to_string(),
        source_symbol: from.symbol.clone(),
        source_display_name: from.display.clone(),
        source_kind: from.kind,
        target_symbol: format!("function:?:{}", callee),
        target_display_name: callee.to_string(),
        target_kind: SymbolKind::Function,
        file_path: file_path.to_string(),
        start_line: line,
        end_line: line,
        confidence: CONFIDENCE_INFERRED,
    }));
}

fn strip_string_literal(raw: &str) -> Option<String> {
    let trimmed = raw.trim();
    if trimmed.len() < 2 {
        return None;
    }
    let first = trimmed.as_bytes()[0] as char;
    let last = trimmed.as_bytes()[trimmed.len() - 1] as char;
    if (first == '"' || first == '\'' || first == '`') && first == last {
        let inner = trimmed[1..trimmed.len() - 1].trim();
        if !inner.is_empty() {
            return Some(inner.to_string());
        }
    }
    None
}

fn find_string_literal(node: Node<'_>, source: &[u8]) -> Option<String> {
    if node.kind() == "string" || node.kind() == "template_string" {
        if let Some(text) = parsed_text(node, source).and_then(|s| strip_string_literal(&s)) {
            return Some(text);
        }
    }
    let mut cursor = node.walk();
    for child in node.children(&mut cursor) {
        if let Some(found) = find_string_literal(child, source) {
            return Some(found);
        }
    }
    None
}

fn module_source_literal(node: Node<'_>, source: &[u8]) -> Option<String> {
    node.child_by_field_name("source")
        .and_then(|source_node| find_string_literal(source_node, source))
}

fn named_text(node: Node<'_>, source: &[u8]) -> Option<String> {
    parsed_text(node, source).and_then(|text| sanitise(&text))
}

fn node_name(node: Node<'_>, source: &[u8]) -> Option<String> {
    node.child_by_field_name("name")
        .and_then(|n| named_text(n, source))
}

fn node_name_or_default(node: Node<'_>, source: &[u8]) -> Option<String> {
    node_name(node, source).or_else(|| {
        parsed_text(node, source).and_then(|text| {
            if text.contains("export default") || text.starts_with("default ") {
                Some("default".to_string())
            } else {
                None
            }
        })
    })
}

fn member_name(node: Node<'_>, source: &[u8]) -> Option<String> {
    node_name(node, source).or_else(|| named_child_text_by_field(node, "key", source))
}

fn call_target(node: Node<'_>, source: &[u8]) -> Option<String> {
    let candidate = node
        .child_by_field_name("function")
        .or_else(|| node.child_by_field_name("constructor"))
        .or_else(|| node.named_child(0))?;
    named_text(candidate, source)
}

fn heritage_targets(node: Node<'_>, source: &[u8], keyword: &str) -> Vec<String> {
    if keyword == "extends" {
        if let Some(superclass) = node.child_by_field_name("superclass") {
            if let Some(target) = named_text(superclass, source) {
                return vec![target];
            }
        }
    }
    let mut out = Vec::new();
    let mut cursor = node.walk();
    for child in node.named_children(&mut cursor) {
        if child.kind() == "class_heritage"
            || child.kind() == "extends_clause"
            || child.kind() == "implements_clause"
            || child.kind() == "extends_type_clause"
        {
            out.extend(heritage_targets_from_text(
                &parsed_text(child, source).unwrap_or_default(),
                keyword,
            ));
        }
    }
    out
}

fn heritage_targets_from_text(text: &str, keyword: &str) -> Vec<String> {
    let lower = text.to_lowercase();
    let Some(start) = lower.find(keyword) else {
        return Vec::new();
    };
    let mut rest = text[start + keyword.len()..].trim();
    if keyword == "extends" {
        if let Some(idx) = rest.to_lowercase().find(" implements ") {
            rest = rest[..idx].trim();
        }
    }
    if keyword == "implements" {
        if let Some(idx) = rest.to_lowercase().find(" extends ") {
            rest = rest[..idx].trim();
        }
    }
    rest.split(',')
        .filter_map(|raw| {
            let target = raw
                .split('<')
                .next()
                .unwrap_or("")
                .trim()
                .trim_matches(|c: char| c == '{' || c == '}' || c == ';');
            sanitise(target)
        })
        .collect()
}

fn variable_function_initializer(node: Node<'_>) -> bool {
    node.child_by_field_name("value")
        .map(|value| {
            matches!(
                value.kind(),
                "arrow_function"
                    | "function"
                    | "function_expression"
                    | "generator_function"
                    | "generator_function_expression"
            )
        })
        .unwrap_or(false)
}

fn function_like_value(node: Node<'_>) -> bool {
    matches!(
        node.kind(),
        "arrow_function"
            | "function"
            | "function_expression"
            | "generator_function"
            | "generator_function_expression"
    )
}

fn child_value_is_function_like(node: Node<'_>) -> bool {
    node.child_by_field_name("value")
        .map(function_like_value)
        .unwrap_or(false)
}

fn named_child_text_by_field(node: Node<'_>, field: &str, source: &[u8]) -> Option<String> {
    node.child_by_field_name(field)
        .and_then(|n| named_text(n, source))
}

fn module_scope(file_path: &str) -> TsScope {
    TsScope {
        symbol: format!("module:{}", file_path),
        display: file_path.to_string(),
        kind: SymbolKind::Module,
    }
}

fn current_or_module(scopes: &[TsScope], file_path: &str) -> TsScope {
    scopes
        .last()
        .cloned()
        .unwrap_or_else(|| module_scope(file_path))
}

fn visit_ts_node(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    file_path: &str,
    source: &[u8],
    node: Node<'_>,
    scopes: &mut Vec<TsScope>,
) {
    match node.kind() {
        "import_statement" => {
            if let Some(target) = find_string_literal(node, source) {
                let module = module_scope(file_path);
                push_parsed_import(
                    facts,
                    language,
                    &module.symbol,
                    file_path,
                    &target,
                    parsed_line(node),
                );
            }
            return;
        }
        "export_statement" => {
            if let Some(target) = module_source_literal(node, source) {
                let module = module_scope(file_path);
                push_parsed_import(
                    facts,
                    language,
                    &module.symbol,
                    file_path,
                    &target,
                    parsed_line(node),
                );
            }
        }
        "class_declaration" | "abstract_class_declaration" => {
            if let Some(name) = node_name_or_default(node, source) {
                let parent = current_or_module(scopes, file_path);
                let class_scope = TsScope {
                    symbol: format!("class:{}:{}", file_path, name),
                    display: name.clone(),
                    kind: SymbolKind::Class,
                };
                push_parsed_defines(
                    facts,
                    language,
                    &parent,
                    file_path,
                    &class_scope.symbol,
                    &class_scope.display,
                    SymbolKind::Class,
                    parsed_line(node),
                );
                for base in heritage_targets(node, source, "extends") {
                    push_parsed_extends(
                        facts,
                        language,
                        &class_scope,
                        file_path,
                        &base,
                        parsed_line(node),
                    );
                }
                for interface in heritage_targets(node, source, "implements") {
                    push_parsed_type_relation(
                        facts,
                        language,
                        FactKind::Implements,
                        &class_scope,
                        file_path,
                        "interface",
                        &interface,
                        SymbolKind::Interface,
                        parsed_line(node),
                    );
                }
                scopes.push(class_scope);
                visit_named_children(facts, language, file_path, source, node, scopes);
                scopes.pop();
                return;
            }
        }
        "interface_declaration" => {
            if let Some(name) = node_name(node, source) {
                let parent = current_or_module(scopes, file_path);
                let interface_scope = TsScope {
                    symbol: format!("interface:{}:{}", file_path, name),
                    display: name.clone(),
                    kind: SymbolKind::Interface,
                };
                push_parsed_defines(
                    facts,
                    language,
                    &parent,
                    file_path,
                    &interface_scope.symbol,
                    &interface_scope.display,
                    SymbolKind::Interface,
                    parsed_line(node),
                );
                for base in heritage_targets(node, source, "extends") {
                    push_parsed_type_relation(
                        facts,
                        language,
                        FactKind::Extends,
                        &interface_scope,
                        file_path,
                        "interface",
                        &base,
                        SymbolKind::Interface,
                        parsed_line(node),
                    );
                }
                scopes.push(interface_scope);
                visit_named_children(facts, language, file_path, source, node, scopes);
                scopes.pop();
                return;
            }
        }
        "type_alias_declaration" | "enum_declaration" | "internal_module" => {
            if let Some(name) = node_name(node, source) {
                let parent = current_or_module(scopes, file_path);
                let (prefix, kind) = match node.kind() {
                    "internal_module" => ("module", SymbolKind::Module),
                    "enum_declaration" => ("class", SymbolKind::Class),
                    _ => ("symbol", SymbolKind::Symbol),
                };
                let scope = TsScope {
                    symbol: format!("{}:{}:{}", prefix, file_path, name),
                    display: name.clone(),
                    kind,
                };
                push_parsed_defines(
                    facts,
                    language,
                    &parent,
                    file_path,
                    &scope.symbol,
                    &scope.display,
                    kind,
                    parsed_line(node),
                );
                scopes.push(scope);
                visit_named_children(facts, language, file_path, source, node, scopes);
                scopes.pop();
                return;
            }
        }
        "function_declaration" | "generator_function_declaration" => {
            if let Some(name) = node_name_or_default(node, source) {
                let parent = current_or_module(scopes, file_path);
                let function_scope = TsScope {
                    symbol: format!("function:{}:{}", file_path, name),
                    display: name.clone(),
                    kind: SymbolKind::Function,
                };
                push_parsed_defines(
                    facts,
                    language,
                    &parent,
                    file_path,
                    &function_scope.symbol,
                    &function_scope.display,
                    SymbolKind::Function,
                    parsed_line(node),
                );
                scopes.push(function_scope);
                visit_named_children(facts, language, file_path, source, node, scopes);
                scopes.pop();
                return;
            }
        }
        "method_definition" | "method_signature" | "abstract_method_signature" | "pair" => {
            if node.kind() != "pair" || child_value_is_function_like(node) {
                if let Some(name) = member_name(node, source) {
                    let parent = current_or_module(scopes, file_path);
                    let method_symbol = if parent.kind == SymbolKind::Class {
                        format!("{}#{}", parent.symbol, name)
                    } else {
                        format!("function:{}:{}", file_path, name)
                    };
                    let method_scope = TsScope {
                        symbol: method_symbol,
                        display: name.clone(),
                        kind: SymbolKind::Method,
                    };
                    push_parsed_defines(
                        facts,
                        language,
                        &parent,
                        file_path,
                        &method_scope.symbol,
                        &method_scope.display,
                        SymbolKind::Method,
                        parsed_line(node),
                    );
                    scopes.push(method_scope);
                    visit_named_children(facts, language, file_path, source, node, scopes);
                    scopes.pop();
                    return;
                }
            }
        }
        "public_field_definition" | "property_definition" | "field_definition" => {
            if child_value_is_function_like(node) {
                if let Some(name) = member_name(node, source) {
                    let parent = current_or_module(scopes, file_path);
                    let method_scope = TsScope {
                        symbol: if parent.kind == SymbolKind::Class {
                            format!("{}#{}", parent.symbol, name)
                        } else {
                            format!("function:{}:{}", file_path, name)
                        },
                        display: name.clone(),
                        kind: SymbolKind::Method,
                    };
                    push_parsed_defines(
                        facts,
                        language,
                        &parent,
                        file_path,
                        &method_scope.symbol,
                        &method_scope.display,
                        SymbolKind::Method,
                        parsed_line(node),
                    );
                    scopes.push(method_scope);
                    visit_named_children(facts, language, file_path, source, node, scopes);
                    scopes.pop();
                    return;
                }
            }
        }
        "variable_declarator" => {
            if variable_function_initializer(node) {
                if let Some(name) = node_name(node, source) {
                    let parent = current_or_module(scopes, file_path);
                    let function_scope = TsScope {
                        symbol: format!("function:{}:{}", file_path, name),
                        display: name.clone(),
                        kind: SymbolKind::Function,
                    };
                    push_parsed_defines(
                        facts,
                        language,
                        &parent,
                        file_path,
                        &function_scope.symbol,
                        &function_scope.display,
                        SymbolKind::Function,
                        parsed_line(node),
                    );
                    scopes.push(function_scope);
                    visit_named_children(facts, language, file_path, source, node, scopes);
                    scopes.pop();
                    return;
                }
            } else if let Some(name) = node_name(node, source) {
                let parent = current_or_module(scopes, file_path);
                let symbol = format!("symbol:{}:{}", file_path, name);
                push_parsed_defines(
                    facts,
                    language,
                    &parent,
                    file_path,
                    &symbol,
                    &name,
                    SymbolKind::Symbol,
                    parsed_line(node),
                );
            }
        }
        "call_expression" | "new_expression" => {
            if let Some(callee) = call_target(node, source) {
                let first_segment = callee.split(['.', ':']).next().unwrap_or(&callee).trim();
                if (callee == "require" || callee == "import") && node.kind() == "call_expression" {
                    if let Some(target) = find_string_literal(node, source) {
                        let module = module_scope(file_path);
                        push_parsed_import(
                            facts,
                            language,
                            &module.symbol,
                            file_path,
                            &target,
                            parsed_line(node),
                        );
                    }
                } else if !is_language_keyword(language, first_segment)
                    || ((first_segment == "this" || first_segment == "super")
                        && callee.contains('.'))
                {
                    let from = current_or_module(scopes, file_path);
                    push_parsed_calls(
                        facts,
                        language,
                        &from,
                        file_path,
                        &callee,
                        parsed_line(node),
                    );
                }
            }
        }
        "jsx_opening_element" | "jsx_self_closing_element" => {
            if let Some(name) = named_child_text_by_field(node, "name", source) {
                if name
                    .chars()
                    .next()
                    .map(|c| c.is_uppercase())
                    .unwrap_or(false)
                {
                    let from = current_or_module(scopes, file_path);
                    push_parsed_calls(facts, language, &from, file_path, &name, parsed_line(node));
                }
            }
        }
        _ => {}
    }
    visit_named_children(facts, language, file_path, source, node, scopes);
}

fn visit_named_children(
    facts: &mut Vec<SyntacticAstFact>,
    language: &str,
    file_path: &str,
    source: &[u8],
    node: Node<'_>,
    scopes: &mut Vec<TsScope>,
) {
    let mut cursor = node.walk();
    for child in node.named_children(&mut cursor) {
        visit_ts_node(facts, language, file_path, source, child, scopes);
    }
}

fn extract_tree_sitter_facts(
    language_name: &str,
    parser_language: Language,
    file_path: &str,
    source: &str,
) -> Vec<SyntacticAstFact> {
    extract_tree_sitter_facts_result(language_name, parser_language, file_path, source)
        .unwrap_or_default()
}

fn extract_tree_sitter_facts_result(
    language_name: &str,
    parser_language: Language,
    file_path: &str,
    source: &str,
) -> Result<Vec<SyntacticAstFact>, String> {
    let mut parser = Parser::new();
    if parser.set_language(&parser_language).is_err() {
        return Err(format!(
            "tree-sitter set_language failed for {}",
            language_name
        ));
    }
    let tree = match parser.parse(source, None) {
        Some(tree) => tree,
        None => {
            return Err(format!(
                "tree-sitter parse returned no tree for {}",
                file_path
            ))
        }
    };
    let mut facts = Vec::new();
    let mut scopes = Vec::new();
    visit_ts_node(
        &mut facts,
        language_name,
        file_path,
        source.as_bytes(),
        tree.root_node(),
        &mut scopes,
    );
    Ok(facts)
}

pub fn extract_typescript_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    let language = if file_path.ends_with(".tsx") {
        tree_sitter_typescript::LANGUAGE_TSX.into()
    } else {
        tree_sitter_typescript::LANGUAGE_TYPESCRIPT.into()
    };
    extract_tree_sitter_facts("typescript", language, file_path, source)
}

pub fn extract_javascript_facts(file_path: &str, source: &str) -> Vec<SyntacticAstFact> {
    extract_tree_sitter_facts(
        "javascript",
        tree_sitter_javascript::LANGUAGE.into(),
        file_path,
        source,
    )
}

// ===== Walker (legacy lines 516-612) =====

/// Per-language extension whitelist (legacy
/// `LANGUAGE_EXTENSIONS`, lines 65-73). The walker only opens
/// files whose extension is in this set for the requested
/// language.
fn language_extensions(language: &str) -> Option<&'static [&'static str]> {
    match language {
        "go" => Some(&[".go"]),
        "python" => Some(&[".py"]),
        "java" => Some(&[".java"]),
        "ruby" => Some(&[".rb"]),
        "csharp" => Some(&[".cs"]),
        "rust" => Some(&[".rs"]),
        "dart" => Some(&[".dart"]),
        "typescript" => Some(&[".ts", ".tsx", ".mts", ".cts"]),
        "javascript" => Some(&[".js", ".jsx", ".mjs", ".cjs"]),
        _ => None,
    }
}

/// Dispatch table for regex and parser-grade extractors. Kept
/// as a `match` rather than a `HashMap<&str, fn>` because the
/// closure signatures are uniform and the compiler can
/// monomorphize each call site.
fn dispatch_extractor_result(
    language: &str,
    file_path: &str,
    source: &str,
) -> Result<Vec<SyntacticAstFact>, String> {
    match language {
        "go" => Ok(extract_go_facts(file_path, source)),
        "python" => Ok(extract_python_facts(file_path, source)),
        "java" => Ok(extract_java_facts(file_path, source)),
        "ruby" => Ok(extract_ruby_facts(file_path, source)),
        "csharp" => Ok(extract_csharp_facts(file_path, source)),
        "rust" => Ok(extract_rust_facts(file_path, source)),
        "dart" => Ok(extract_dart_facts(file_path, source)),
        "typescript" => {
            let parser_language = if file_path.ends_with(".tsx") {
                tree_sitter_typescript::LANGUAGE_TSX.into()
            } else {
                tree_sitter_typescript::LANGUAGE_TYPESCRIPT.into()
            };
            extract_tree_sitter_facts_result("typescript", parser_language, file_path, source)
        }
        "javascript" => extract_tree_sitter_facts_result(
            "javascript",
            tree_sitter_javascript::LANGUAGE.into(),
            file_path,
            source,
        ),
        _ => Ok(Vec::new()),
    }
}

/// SCAN_DEFAULT_MAX_FILES is the walker's per-call cap on
/// scanned files. Legacy `DEFAULT_MAX_FILES = 5000`.
pub const SCAN_DEFAULT_MAX_FILES: usize = 5000;
/// SCAN_DEFAULT_MAX_FILE_BYTES caps the size of any single
/// file the walker reads. Legacy `DEFAULT_MAX_FILE_BYTES =
/// 512 * 1024`.
pub const SCAN_DEFAULT_MAX_FILE_BYTES: u64 = 512 * 1024;
/// SCAN_DEFAULT_MAX_DIRECTORIES caps the number of directories
/// the walker visits before bailing. Legacy
/// `DEFAULT_MAX_DIRECTORIES = 20000`.
pub const SCAN_DEFAULT_MAX_DIRECTORIES: usize = 20_000;

/// IGNORED_DIRECTORY_NAMES — verbatim port of legacy lines
/// 59-63. Any directory whose basename appears here is skipped
/// during the BFS walk. Additionally, any directory whose
/// name starts with `.` is also skipped (legacy
/// `entry.name.startsWith(".")` guard) — that covers `.git`,
/// `.next`, `.turbo`, `.cache`, `.venv` etc. without listing
/// each one.
const SCAN_IGNORED_DIRS: &[&str] = &[
    "node_modules",
    "dist",
    "build",
    "out",
    "generated",
    "__generated__",
    "coverage",
    "vendor",
    "target",
    "bin",
    "obj",
    "venv",
    "__pycache__",
];

/// SyntacticAstScanOptions mirrors the legacy struct
/// (lines 46-51) with sensible defaults via Option<...>.
pub struct SyntacticAstScanOptions<'a> {
    pub repo_root: &'a std::path::Path,
    pub max_files: Option<usize>,
    pub max_file_bytes: Option<u64>,
    pub max_directories: Option<usize>,
}

/// scan_syntactic_ast_facts mirrors the legacy
/// `scanSyntacticAstFacts` (lines 518-612). Synchronous —
/// caller is responsible for `tokio::task::spawn_blocking`
/// if it runs inside an async context.
///
/// BFS over the repo tree, skipping symlinks + ignored dirs +
/// dotted dirs. Reads each file matching the language's
/// extension whitelist, dispatches to the per-language regex
/// extractor, accumulates facts. Returns a
/// SyntacticAstScanResult with the facts, file counts, and
/// any warnings (parse failures, maxDirectories ceiling).
///
/// Path-escape guard: every walked directory + file path is
/// canonicalized + checked against the repo_root prefix.
/// Symlinks that escape the repo (a common attack on shared-
/// volume tenants) are silently skipped + counted.
pub fn scan_syntactic_ast_facts(
    language: &str,
    options: SyntacticAstScanOptions<'_>,
) -> SyntacticAstScanResult {
    let mut result = SyntacticAstScanResult {
        facts: Vec::new(),
        scanned_file_count: 0,
        skipped_file_count: 0,
        warnings: Vec::new(),
    };

    let extensions = match language_extensions(language) {
        Some(exts) => exts,
        None => {
            result
                .warnings
                .push(format!("No syntactic extractor for language {}", language));
            return result;
        }
    };

    let max_files = options.max_files.unwrap_or(SCAN_DEFAULT_MAX_FILES);
    let max_bytes = options
        .max_file_bytes
        .unwrap_or(SCAN_DEFAULT_MAX_FILE_BYTES);
    let max_dirs = options
        .max_directories
        .unwrap_or(SCAN_DEFAULT_MAX_DIRECTORIES);

    let repo_root = match options.repo_root.canonicalize() {
        Ok(p) => p,
        Err(e) => {
            result.warnings.push(format!(
                "canonicalize repo_root {}: {}",
                options.repo_root.display(),
                e
            ));
            return result;
        }
    };

    let mut visited_dirs: usize = 0;
    let mut queue: std::collections::VecDeque<std::path::PathBuf> =
        std::collections::VecDeque::new();
    queue.push_back(repo_root.clone());

    while let Some(current) = queue.pop_front() {
        if (result.scanned_file_count as usize) >= max_files {
            break;
        }
        if visited_dirs >= max_dirs {
            result.warnings.push(format!(
                "Syntactic {} walk hit maxDirectories={}; remaining tree skipped.",
                language, max_dirs
            ));
            break;
        }
        visited_dirs += 1;

        // Path-escape guard: any directory dequeued from the
        // BFS must still be inside the repo root. Defense
        // against symlink-as-directory escape.
        if current != repo_root && !current.starts_with(&repo_root) {
            continue;
        }

        let entries = match std::fs::read_dir(&current) {
            Ok(e) => e,
            Err(_) => continue,
        };

        for entry_result in entries {
            let entry = match entry_result {
                Ok(e) => e,
                Err(_) => continue,
            };
            let file_type = match entry.file_type() {
                Ok(t) => t,
                Err(_) => continue,
            };
            // Reject symlinks outright (legacy line 149).
            if file_type.is_symlink() {
                result.skipped_file_count += 1;
                continue;
            }
            let name = entry.file_name();
            let name_str = name.to_string_lossy();

            if file_type.is_dir() {
                if SCAN_IGNORED_DIRS.iter().any(|d| *d == name_str.as_ref())
                    || name_str.starts_with('.')
                {
                    continue;
                }
                queue.push_back(entry.path());
                continue;
            }

            if !file_type.is_file() {
                continue;
            }

            // Extension filter — legacy uses path.extname which
            // returns the trailing `.ext`; Rust's Path::extension
            // returns just `ext` (no dot). Mirror legacy by
            // prepending the dot.
            let ext = entry
                .path()
                .extension()
                .map(|e| format!(".{}", e.to_string_lossy()))
                .unwrap_or_default();
            if !extensions.iter().any(|e| *e == ext.as_str()) {
                continue;
            }

            // Path-escape guard #2 — even after the directory
            // filter, refuse a file whose resolved path lies
            // outside the repo root.
            let abs_file = entry.path();
            let resolved = match abs_file.canonicalize() {
                Ok(p) => p,
                Err(_) => {
                    result.skipped_file_count += 1;
                    continue;
                }
            };
            if resolved != repo_root && !resolved.starts_with(&repo_root) {
                result.skipped_file_count += 1;
                continue;
            }

            // Per-file size budget.
            let meta = match std::fs::symlink_metadata(&resolved) {
                Ok(m) => m,
                Err(_) => {
                    result.skipped_file_count += 1;
                    continue;
                }
            };
            if !meta.is_file() || meta.len() > max_bytes {
                result.skipped_file_count += 1;
                continue;
            }

            // Read + extract.
            let source = match std::fs::read_to_string(&resolved) {
                Ok(s) => s,
                Err(_) => {
                    result.skipped_file_count += 1;
                    continue;
                }
            };
            let relative = resolved
                .strip_prefix(&repo_root)
                .map(|p| p.to_string_lossy().to_string())
                .unwrap_or_else(|_| resolved.to_string_lossy().to_string());

            match dispatch_extractor_result(language, &relative, &source) {
                Ok(extracted) => result.facts.extend(extracted),
                Err(err) => {
                    result.warnings.push(format!(
                        "{} parse failed for {}: {}",
                        language, relative, err
                    ));
                    result.skipped_file_count += 1;
                    continue;
                }
            }
            result.scanned_file_count += 1;

            // Parity note: legacy gates maxFiles ONLY in the
            // outer `while` header (line 540), NOT inside the
            // inner for-loop over entries. So the legacy can
            // OVERSHOOT max_files by up to (dir_entries - 1)
            // when the limit is hit mid-directory. We mirror
            // that exact behavior — no inner break — to keep
            // byte-equal parity on result counts.
        }
    }

    result
}

/// scan_syntactic_ast_facts_multi is the production worker
/// path for mixed-language repositories. It walks the repo tree
/// once, dispatching each matching file to the selected
/// language extractor. The single-language scanner above stays
/// available for parity tests and narrow callers, but the worker
/// should prefer this function to avoid 9x directory traversal
/// on polyglot repos.
pub fn scan_syntactic_ast_facts_multi(
    languages: &[&str],
    options: SyntacticAstScanOptions<'_>,
) -> Vec<(String, SyntacticAstScanResult)> {
    let mut results: Vec<(String, SyntacticAstScanResult)> = Vec::new();
    let mut language_indexes: HashMap<String, usize> = HashMap::new();
    let mut extension_to_languages: HashMap<&'static str, Vec<usize>> = HashMap::new();
    let mut active_indexes: Vec<usize> = Vec::new();

    for language in languages {
        let language = language.trim();
        if language.is_empty() || language_indexes.contains_key(language) {
            continue;
        }
        let index = results.len();
        language_indexes.insert(language.to_string(), index);
        results.push((
            language.to_string(),
            SyntacticAstScanResult {
                facts: Vec::new(),
                scanned_file_count: 0,
                skipped_file_count: 0,
                warnings: Vec::new(),
            },
        ));
        match language_extensions(language) {
            Some(exts) => {
                active_indexes.push(index);
                for ext in exts {
                    extension_to_languages.entry(ext).or_default().push(index);
                }
            }
            None => {
                results[index]
                    .1
                    .warnings
                    .push(format!("No syntactic extractor for language {}", language));
            }
        }
    }

    if active_indexes.is_empty() {
        return results;
    }

    let max_files = options.max_files.unwrap_or(SCAN_DEFAULT_MAX_FILES);
    let max_bytes = options
        .max_file_bytes
        .unwrap_or(SCAN_DEFAULT_MAX_FILE_BYTES);
    let max_dirs = options
        .max_directories
        .unwrap_or(SCAN_DEFAULT_MAX_DIRECTORIES);

    let repo_root = match options.repo_root.canonicalize() {
        Ok(p) => p,
        Err(e) => {
            let warning = format!(
                "canonicalize repo_root {}: {}",
                options.repo_root.display(),
                e
            );
            for index in active_indexes {
                results[index].1.warnings.push(warning.clone());
            }
            return results;
        }
    };

    let mut visited_dirs: usize = 0;
    let mut queue: std::collections::VecDeque<std::path::PathBuf> =
        std::collections::VecDeque::new();
    queue.push_back(repo_root.clone());

    while let Some(current) = queue.pop_front() {
        if active_indexes
            .iter()
            .all(|index| (results[*index].1.scanned_file_count as usize) >= max_files)
        {
            break;
        }
        if visited_dirs >= max_dirs {
            for index in &active_indexes {
                let language = results[*index].0.clone();
                results[*index].1.warnings.push(format!(
                    "Syntactic {} walk hit maxDirectories={}; remaining tree skipped.",
                    language, max_dirs
                ));
            }
            break;
        }
        visited_dirs += 1;

        if current != repo_root && !current.starts_with(&repo_root) {
            continue;
        }

        let entries = match std::fs::read_dir(&current) {
            Ok(e) => e,
            Err(_) => continue,
        };

        for entry_result in entries {
            let entry = match entry_result {
                Ok(e) => e,
                Err(_) => continue,
            };
            let file_type = match entry.file_type() {
                Ok(t) => t,
                Err(_) => continue,
            };
            if file_type.is_symlink() {
                for index in &active_indexes {
                    results[*index].1.skipped_file_count += 1;
                }
                continue;
            }
            let name = entry.file_name();
            let name_str = name.to_string_lossy();

            if file_type.is_dir() {
                if SCAN_IGNORED_DIRS.iter().any(|d| *d == name_str.as_ref())
                    || name_str.starts_with('.')
                {
                    continue;
                }
                queue.push_back(entry.path());
                continue;
            }

            if !file_type.is_file() {
                continue;
            }

            let ext = entry
                .path()
                .extension()
                .map(|e| format!(".{}", e.to_string_lossy()))
                .unwrap_or_default();
            let Some(language_indices) = extension_to_languages.get(ext.as_str()) else {
                continue;
            };

            let abs_file = entry.path();
            let resolved = match abs_file.canonicalize() {
                Ok(p) => p,
                Err(_) => {
                    for index in language_indices {
                        results[*index].1.skipped_file_count += 1;
                    }
                    continue;
                }
            };
            if resolved != repo_root && !resolved.starts_with(&repo_root) {
                for index in language_indices {
                    results[*index].1.skipped_file_count += 1;
                }
                continue;
            }

            let meta = match std::fs::symlink_metadata(&resolved) {
                Ok(m) => m,
                Err(_) => {
                    for index in language_indices {
                        results[*index].1.skipped_file_count += 1;
                    }
                    continue;
                }
            };
            if !meta.is_file() || meta.len() > max_bytes {
                for index in language_indices {
                    results[*index].1.skipped_file_count += 1;
                }
                continue;
            }

            let source = match std::fs::read_to_string(&resolved) {
                Ok(s) => s,
                Err(_) => {
                    for index in language_indices {
                        results[*index].1.skipped_file_count += 1;
                    }
                    continue;
                }
            };
            let relative = resolved
                .strip_prefix(&repo_root)
                .map(|p| p.to_string_lossy().to_string())
                .unwrap_or_else(|_| resolved.to_string_lossy().to_string());

            for index in language_indices {
                let language = results[*index].0.clone();
                match dispatch_extractor_result(&language, &relative, &source) {
                    Ok(extracted) => results[*index].1.facts.extend(extracted),
                    Err(err) => {
                        results[*index].1.warnings.push(format!(
                            "{} parse failed for {}: {}",
                            language, relative, err
                        ));
                        results[*index].1.skipped_file_count += 1;
                        continue;
                    }
                }
                results[*index].1.scanned_file_count += 1;
            }
        }
    }

    results
}

#[cfg(test)]
mod tests {
    //! These tests assert byte-equal facts against
    //! hand-evaluated legacy outputs. The legacy emits facts in
    //! source order via `facts.push(...)`, and the Rust port
    //! mirrors that ordering, so a Vec equality comparison is
    //! the right test surface.

    use super::*;

    #[test]
    fn sanitise_rejects_quoted_text() {
        assert_eq!(sanitise(r#"foo"bar"#), None);
        assert_eq!(sanitise(r#"foo'bar"#), None);
        assert_eq!(sanitise("foo`bar"), None);
    }

    #[test]
    fn sanitise_collapses_whitespace_and_trims() {
        assert_eq!(sanitise("  foo   bar  "), Some("foo bar".to_string()));
        assert_eq!(sanitise("foo\tbar"), Some("foo bar".to_string()));
        assert_eq!(sanitise("foo\n\nbar"), Some("foo bar".to_string()));
    }

    #[test]
    fn sanitise_truncates_over_max_length() {
        // Critic 2.2: pin the exact post-truncation length
        // for ASCII input. For pure ASCII the byte boundary
        // and char boundary agree at 120, so we expect
        // exactly MAX_CALLEE_TEXT_LENGTH + 3 = 123 bytes.
        let long = "a".repeat(150);
        let out = sanitise(&long).expect("sanitise long");
        assert!(out.ends_with("..."));
        assert_eq!(out.len(), MAX_CALLEE_TEXT_LENGTH + 3);
    }

    #[test]
    fn sanitise_handles_empty_and_whitespace_only() {
        assert_eq!(sanitise(""), None);
        assert_eq!(sanitise("   "), None);
    }

    #[test]
    fn count_char_counts_braces() {
        assert_eq!(count_char("func main() {", '{'), 1);
        assert_eq!(count_char("} }} }", '}'), 4);
        assert_eq!(count_char("no braces", '{'), 0);
    }

    #[test]
    fn is_language_keyword_recognises_go_keywords() {
        assert!(is_language_keyword("go", "if"));
        assert!(is_language_keyword("go", "func"));
        assert!(is_language_keyword("go", "return"));
        assert!(!is_language_keyword("go", "myFunc"));
        // Unknown language → no keywords matched.
        assert!(!is_language_keyword("klingon", "if"));
    }

    #[test]
    fn push_import_emits_external_for_non_dot_target() {
        let mut facts = Vec::new();
        push_import(&mut facts, "go", "module:a.go", "a.go", "fmt", 3);
        assert_eq!(facts.len(), 1);
        let f = &facts[0];
        assert_eq!(f.kind, FactKind::ImportsFrom);
        assert_eq!(f.target_symbol, "module:fmt");
        assert_eq!(f.target_display_name, "fmt");
        assert_eq!(f.target_kind, SymbolKind::External);
        assert_eq!(f.start_line, 3);
        assert_eq!(f.confidence, CONFIDENCE_INFERRED);
    }

    #[test]
    fn push_import_emits_module_for_dot_target() {
        let mut facts = Vec::new();
        push_import(&mut facts, "python", "module:b.py", "b.py", "./helpers", 7);
        let f = &facts[0];
        assert_eq!(f.target_kind, SymbolKind::Module);
    }

    #[test]
    fn extract_go_facts_emits_imports_block_form() {
        let src = r#"package main

import (
    "fmt"
    "context"
    "github.com/x/y"
)

func main() {
}
"#;
        let facts = extract_go_facts("main.go", src);
        // 3 imports + 1 defines (`main` function).
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 3);
        let targets: Vec<&str> = imports
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(targets.contains(&"fmt"));
        assert!(targets.contains(&"context"));
        assert!(targets.contains(&"github.com/x/y"));
    }

    #[test]
    fn extract_go_facts_emits_imports_single_form() {
        let src = r#"package main

import "fmt"

func main() {}
"#;
        let facts = extract_go_facts("main.go", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 1);
        assert_eq!(imports[0].target_display_name, "fmt");
    }

    #[test]
    fn extract_go_facts_defines_top_level_funcs() {
        let src = r#"package main

func Foo() {
    return
}

func Bar() {
    return
}
"#;
        let facts = extract_go_facts("main.go", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 2);
        assert!(defines.iter().any(|f| f.target_display_name == "Foo"));
        assert!(defines.iter().any(|f| f.target_display_name == "Bar"));
        let foo = defines
            .iter()
            .find(|f| f.target_display_name == "Foo")
            .unwrap();
        assert_eq!(foo.target_symbol, "function:main.go:Foo");
        assert_eq!(foo.target_kind, SymbolKind::Function);
    }

    #[test]
    fn extract_go_facts_emits_calls_inside_func_body_skipping_keywords() {
        let src = r#"package main

func Main() {
    handleRequest()
    if x {
        helper()
    }
    return
}
"#;
        let facts = extract_go_facts("main.go", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(callees.contains(&"handleRequest"), "{:?}", callees);
        assert!(callees.contains(&"helper"), "{:?}", callees);
        // `if` is a keyword — must NOT be emitted as a call.
        assert!(!callees.contains(&"if"), "{:?}", callees);
        // `return` is a keyword.
        assert!(!callees.contains(&"return"), "{:?}", callees);
    }

    #[test]
    fn extract_go_facts_skips_self_recursion_call_inside_same_func() {
        // Legacy line 122: `callee !== currentFunc.name` — a
        // function calling itself should NOT emit a `calls`
        // fact for its own name (avoids self-edges in the
        // graph).
        let src = r#"package main

func Recur() {
    Recur()
    other()
}
"#;
        let facts = extract_go_facts("main.go", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        // `other` is the only non-recursive non-keyword call.
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(
            !callees.contains(&"Recur"),
            "self-recursion leaked: {:?}",
            callees
        );
        assert!(callees.contains(&"other"), "{:?}", callees);
    }

    #[test]
    fn extract_go_facts_handles_method_receivers() {
        // Legacy fn regex: `func\s+(?:\([^)]+\)\s*)?([A-Za-z_][A-Za-z0-9_]*)`.
        // The optional receiver group `(?:\([^)]+\)\s*)?` lets
        // us match both `func F()` and `func (r *R) M()`.
        let src = r#"package main

func (r *Receiver) Method() {
    inner()
}
"#;
        let facts = extract_go_facts("main.go", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "Method");
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        assert!(calls.iter().any(|f| f.target_display_name == "inner"));
    }

    #[test]
    fn extract_go_facts_no_calls_emitted_outside_func_body() {
        // Calls at the top level (e.g. var x = pkg.Init()) are
        // legitimately skipped by the legacy because it only
        // collects calls when `currentFunc` is set. Mirror that.
        let src = r#"package main

var x = init()

func main() {}
"#;
        let facts = extract_go_facts("main.go", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(
            !callees.contains(&"init"),
            "top-level call leaked: {:?}",
            callees
        );
    }

    #[test]
    fn duplicate_callees_in_one_func_emit_one_fact_each() {
        // Critic 2.6: legacy emits one `calls` fact PER call
        // site, no dedup within a function. A function that
        // calls `helper()` three times should produce three
        // `calls` facts. Locks the no-dedup contract.
        let src = r#"package main

func Main() {
    helper()
    helper()
    helper()
}
"#;
        let facts = extract_go_facts("main.go", src);
        let helper_calls: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Calls && f.target_display_name == "helper")
            .collect();
        assert_eq!(
            helper_calls.len(),
            3,
            "expected 3 calls, got {:?}",
            helper_calls
        );
        // Each call should have a distinct startLine.
        let lines: std::collections::HashSet<u32> =
            helper_calls.iter().map(|f| f.start_line).collect();
        assert_eq!(lines.len(), 3);
    }

    #[test]
    fn extract_go_facts_full_fixture_byte_equal() {
        // Critic 2.1: full fixture parity test. Hand-evaluated
        // expected facts for a small Go file. The legacy emits
        // facts in source order via `facts.push(...)`; the
        // Rust port mirrors that. Any divergence in ordering,
        // symbol naming, or filter logic surfaces here.
        let src = "package main\n\
                   \n\
                   import \"fmt\"\n\
                   \n\
                   func Greet() {\n\
                   \tfmt.Println(\"hi\")\n\
                   }\n";
        let facts = extract_go_facts("greet.go", src);
        let expected = vec![
            SyntacticAstFact {
                kind: FactKind::ImportsFrom,
                language: "go".to_string(),
                source_symbol: "module:greet.go".to_string(),
                source_display_name: "greet.go".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "module:fmt".to_string(),
                target_display_name: "fmt".to_string(),
                target_kind: SymbolKind::External,
                file_path: "greet.go".to_string(),
                start_line: 1,
                end_line: 1,
                confidence: CONFIDENCE_INFERRED,
            },
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "go".to_string(),
                source_symbol: "module:greet.go".to_string(),
                source_display_name: "greet.go".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:greet.go:Greet".to_string(),
                target_display_name: "Greet".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "greet.go".to_string(),
                start_line: 5,
                end_line: 5,
                confidence: CONFIDENCE_INFERRED,
            },
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "go".to_string(),
                source_symbol: "function:greet.go:Greet".to_string(),
                source_display_name: "Greet".to_string(),
                source_kind: SymbolKind::Function,
                target_symbol: "function:?:fmt.Println".to_string(),
                target_display_name: "fmt.Println".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "greet.go".to_string(),
                start_line: 6,
                end_line: 6,
                confidence: CONFIDENCE_INFERRED,
            },
        ];
        assert_eq!(
            facts.len(),
            expected.len(),
            "fact count mismatch\nactual: {:#?}",
            facts
        );
        for (i, (a, e)) in facts.iter().zip(expected.iter()).enumerate() {
            assert_eq!(
                a, e,
                "fact #{} mismatch\nactual: {:#?}\nexpected: {:#?}",
                i, a, e
            );
        }
    }

    // ===== Python extractor tests =====

    #[test]
    fn extract_python_facts_emits_import_form() {
        let src = "import os\n";
        let facts = extract_python_facts("a.py", src);
        assert_eq!(facts.len(), 1);
        let f = &facts[0];
        assert_eq!(f.kind, FactKind::ImportsFrom);
        assert_eq!(f.language, "python");
        assert_eq!(f.target_display_name, "os");
        assert_eq!(f.target_symbol, "module:os");
        assert_eq!(f.target_kind, SymbolKind::External);
    }

    #[test]
    fn extract_python_facts_emits_from_import_module_plus_symbols() {
        let src = "from foo.bar import a, b, c\n";
        let facts = extract_python_facts("x.py", src);
        // 1 module-level imports_from + 3 symbol-level
        // imports_from.
        assert_eq!(facts.len(), 4);
        // First is the module import; subsequent are symbols.
        assert_eq!(facts[0].target_display_name, "foo.bar");
        assert_eq!(facts[0].target_symbol, "module:foo.bar");
        let symbol_facts: Vec<&SyntacticAstFact> = facts[1..]
            .iter()
            .filter(|f| f.target_kind == SymbolKind::Symbol)
            .collect();
        assert_eq!(symbol_facts.len(), 3);
        let displays: Vec<&str> = symbol_facts
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(displays.contains(&"foo.bar.a"));
        assert!(displays.contains(&"foo.bar.b"));
        assert!(displays.contains(&"foo.bar.c"));
        // Symbol target_symbol shape: `symbol:<module>:<name>`.
        assert!(symbol_facts
            .iter()
            .any(|f| f.target_symbol == "symbol:foo.bar:a"));
    }

    #[test]
    fn extract_python_facts_handles_as_alias() {
        // `from foo import bar as baz` — cleaned name is "bar".
        let src = "from foo import bar as baz\n";
        let facts = extract_python_facts("x.py", src);
        let sym = facts
            .iter()
            .find(|f| f.target_kind == SymbolKind::Symbol)
            .expect("symbol fact");
        assert_eq!(sym.target_display_name, "foo.bar");
        assert_eq!(sym.target_symbol, "symbol:foo:bar");
    }

    #[test]
    fn extract_python_facts_defines_class_with_extends() {
        let src = "class Child(Parent, Mixin):\n    pass\n";
        let facts = extract_python_facts("c.py", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "Child");
        assert_eq!(defines[0].target_kind, SymbolKind::Class);
        assert_eq!(defines[0].target_symbol, "class:c.py:Child");
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 2);
        let bases: Vec<&str> = extends
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(bases.contains(&"Parent"));
        assert!(bases.contains(&"Mixin"));
    }

    #[test]
    fn extract_python_facts_handles_async_def() {
        let src = "async def fetch():\n    pass\n";
        let facts = extract_python_facts("a.py", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "fetch");
    }

    #[test]
    fn extract_python_facts_emits_calls_inside_def_body_skipping_keywords() {
        let src = "def main():\n    handle()\n    if x:\n        helper()\n    return\n";
        let facts = extract_python_facts("m.py", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(callees.contains(&"handle"), "{:?}", callees);
        assert!(callees.contains(&"helper"), "{:?}", callees);
        // `if` is a Python keyword.
        assert!(!callees.contains(&"if"), "{:?}", callees);
        assert!(!callees.contains(&"return"), "{:?}", callees);
    }

    #[test]
    fn extract_python_facts_exits_scope_on_outdent() {
        // Function `main` defined at col 0. Body indented at
        // col 4. The next line at col 0 is non-empty -> exits
        // scope. Calls in that outdented line are NOT
        // attributed to `main`.
        let src = "def main():\n    inside()\noutside()\n";
        let facts = extract_python_facts("m.py", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(callees.contains(&"inside"), "{:?}", callees);
        assert!(
            !callees.contains(&"outside"),
            "outdented call leaked: {:?}",
            callees
        );
    }

    #[test]
    fn extract_python_facts_skips_self_recursion() {
        let src = "def recur():\n    recur()\n    other()\n";
        let facts = extract_python_facts("r.py", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(
            !callees.contains(&"recur"),
            "self-recursion leaked: {:?}",
            callees
        );
        assert!(callees.contains(&"other"), "{:?}", callees);
    }

    #[test]
    fn extract_python_facts_class_with_empty_bases() {
        // Critic 2.1: `class Foo():` parses as class name "Foo"
        // with empty bases. Defines should still emit; no
        // extends facts because the bases group is "".
        let src = "class Foo():\n    pass\n";
        let facts = extract_python_facts("x.py", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "Foo");
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 0);
    }

    #[test]
    fn extract_python_facts_class_without_parens() {
        // Critic 2.2: `class Foo:` (no parens at all). The
        // legacy regex makes the paren group optional, so a
        // bare-class is matched. Defines emits, no extends.
        let src = "class Foo:\n    pass\n";
        let facts = extract_python_facts("x.py", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "Foo");
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 0);
    }

    #[test]
    fn extract_python_facts_from_x_import_star_emits_zero_facts() {
        // Critic 2.3: legacy import regex requires the symbol
        // list to match `[A-Za-z_,\s]+`; `*` doesn't match, so
        // the entire import block fails to match (the `from`
        // arm needs both module + non-empty symbol-list match).
        // Net: zero imports_from facts emitted. This locks
        // that behavior as a regression guard.
        let src = "from foo import *\n";
        let facts = extract_python_facts("x.py", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 0, "import * leaked facts: {:?}", imports);
    }

    #[test]
    fn extract_python_facts_full_fixture_byte_equal() {
        // Hand-evaluated parity fixture.
        let src = "from collections import OrderedDict\n\
             \n\
             class Cache(OrderedDict):\n\
                 \x20\x20\x20\x20def lookup(self, key):\n\
                 \x20\x20\x20\x20\x20\x20\x20\x20return self.get(key)\n";
        let facts = extract_python_facts("cache.py", src);
        let expected = vec![
            // from-import module
            SyntacticAstFact {
                kind: FactKind::ImportsFrom,
                language: "python".to_string(),
                source_symbol: "module:cache.py".to_string(),
                source_display_name: "cache.py".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "module:collections".to_string(),
                target_display_name: "collections".to_string(),
                target_kind: SymbolKind::External,
                file_path: "cache.py".to_string(),
                start_line: 1,
                end_line: 1,
                confidence: CONFIDENCE_INFERRED,
            },
            // from-import symbol
            SyntacticAstFact {
                kind: FactKind::ImportsFrom,
                language: "python".to_string(),
                source_symbol: "module:cache.py".to_string(),
                source_display_name: "cache.py".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "symbol:collections:OrderedDict".to_string(),
                target_display_name: "collections.OrderedDict".to_string(),
                target_kind: SymbolKind::Symbol,
                file_path: "cache.py".to_string(),
                start_line: 1,
                end_line: 1,
                confidence: CONFIDENCE_INFERRED,
            },
            // class Cache defines
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "python".to_string(),
                source_symbol: "module:cache.py".to_string(),
                source_display_name: "cache.py".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "class:cache.py:Cache".to_string(),
                target_display_name: "Cache".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "cache.py".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // class Cache extends OrderedDict
            SyntacticAstFact {
                kind: FactKind::Extends,
                language: "python".to_string(),
                source_symbol: "class:cache.py:Cache".to_string(),
                source_display_name: "Cache".to_string(),
                source_kind: SymbolKind::Class,
                target_symbol: "class:?:OrderedDict".to_string(),
                target_display_name: "OrderedDict".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "cache.py".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // def lookup defines
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "python".to_string(),
                source_symbol: "module:cache.py".to_string(),
                source_display_name: "cache.py".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:cache.py:lookup".to_string(),
                target_display_name: "lookup".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "cache.py".to_string(),
                start_line: 4,
                end_line: 4,
                confidence: CONFIDENCE_INFERRED,
            },
            // call self.get inside lookup body
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "python".to_string(),
                source_symbol: "function:cache.py:lookup".to_string(),
                source_display_name: "lookup".to_string(),
                source_kind: SymbolKind::Function,
                target_symbol: "function:?:self.get".to_string(),
                target_display_name: "self.get".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "cache.py".to_string(),
                start_line: 5,
                end_line: 5,
                confidence: CONFIDENCE_INFERRED,
            },
        ];
        assert_eq!(
            facts.len(),
            expected.len(),
            "fact count mismatch\nactual: {:#?}",
            facts
        );
        for (i, (a, e)) in facts.iter().zip(expected.iter()).enumerate() {
            assert_eq!(
                a, e,
                "fact #{} mismatch\nactual: {:#?}\nexpected: {:#?}",
                i, a, e
            );
        }
    }

    // ===== Java extractor tests =====

    #[test]
    fn extract_java_facts_emits_imports_plain_and_static() {
        let src = "package com.acme;\n\
                   \n\
                   import java.util.List;\n\
                   import static java.lang.Math.PI;\n\
                   \n\
                   public class App {}\n";
        let facts = extract_java_facts("App.java", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 2);
        let targets: Vec<&str> = imports
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(targets.contains(&"java.util.List"), "{:?}", targets);
        assert!(targets.contains(&"java.lang.Math.PI"), "{:?}", targets);
    }

    #[test]
    fn extract_java_facts_class_with_extends_and_implements() {
        let src = "public class Service extends BaseService implements Runnable, Serializable {\n\
                   }\n";
        let facts = extract_java_facts("Service.java", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "Service");
        assert_eq!(defines[0].target_kind, SymbolKind::Class);
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 1);
        assert_eq!(extends[0].target_display_name, "BaseService");
        let implements: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Implements)
            .collect();
        assert_eq!(implements.len(), 2);
        let names: Vec<&str> = implements
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(names.contains(&"Runnable"));
        assert!(names.contains(&"Serializable"));
    }

    #[test]
    fn extract_java_facts_strips_generics_from_extends() {
        let src = "public class A extends List<String> implements Map<K, V> {\n}\n";
        let facts = extract_java_facts("A.java", src);
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 1);
        // Generic stripped → "List", not "List<String>".
        assert_eq!(extends[0].target_display_name, "List");
        // Implements: generic stripped per entry. Note legacy
        // regex captures everything up to `{`, so the comma
        // splits the implements list — `Map<K, V>` becomes
        // ["Map<K", " V>"], both stripped to "Map" and "V"
        // respectively. Mirror that quirk.
        let implements: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Implements)
            .collect();
        let names: Vec<&str> = implements
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        // "Map" comes from "Map<K" → split('<') → "Map".
        assert!(names.contains(&"Map"), "{:?}", names);
        // "V" comes from " V>" → split('<') → " V>" → split('<') doesn't split → trim → "V>"
        // — actually let's check: " V>".split('<').next() is " V>", trim is "V>", which
        // doesn't match the identifier regex (the `>` is rejected). So "V>" should NOT
        // appear in the facts.
        assert!(!names.contains(&"V>"), "{:?}", names);
        assert!(!names.contains(&"V"), "{:?}", names);
    }

    #[test]
    fn extract_java_facts_interface_treated_as_class() {
        let src = "public interface MyService {\n}\n";
        let facts = extract_java_facts("MyService.java", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "MyService");
        // Interface decls emit `defines` with target_kind=class
        // because the legacy uses the same `pushDefines(...,
        // "class", ...)` call site. Mirror.
        assert_eq!(defines[0].target_kind, SymbolKind::Class);
    }

    #[test]
    fn extract_java_facts_method_decl_inside_class() {
        let src = "public class Calc {\n\
                       public int add(int a, int b) {\n\
                           return a + b;\n\
                       }\n\
                   }\n";
        let facts = extract_java_facts("Calc.java", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        // 1 class + 1 method.
        assert_eq!(defines.len(), 2);
        let methods: Vec<&SyntacticAstFact> = defines
            .iter()
            .filter(|f| f.target_kind == SymbolKind::Method)
            .copied()
            .collect();
        assert_eq!(methods.len(), 1);
        assert_eq!(methods[0].target_display_name, "add");
        // Method symbol shape: class:Calc.java:Calc#add
        assert_eq!(methods[0].target_symbol, "class:Calc.java:Calc#add");
    }

    #[test]
    fn extract_java_facts_method_decl_outside_class_not_emitted() {
        // No class scope -> no method defines, even if regex
        // matches (top-level method-like line). The legacy
        // guard `currentScope?.kind === "class"` blocks this.
        let src = "public int globalThing(int x) { return x; }\n";
        let facts = extract_java_facts("x.java", src);
        let method_defs: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines && f.target_kind == SymbolKind::Method)
            .collect();
        assert_eq!(
            method_defs.len(),
            0,
            "top-level method leaked: {:?}",
            method_defs
        );
    }

    #[test]
    fn extract_java_facts_calls_attributed_to_module() {
        let src = "public class App {\n\
                       public static void main(String[] args) {\n\
                           handle();\n\
                           helper.doWork();\n\
                       }\n\
                   }\n";
        let facts = extract_java_facts("App.java", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        // All calls source_kind=module (the legacy
        // simplification — even though we know they're inside
        // the App.main method, the emission attributes to
        // module scope).
        for c in &calls {
            assert_eq!(c.source_kind, SymbolKind::Module, "{:?}", c);
        }
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(callees.contains(&"handle"), "{:?}", callees);
        assert!(callees.contains(&"helper.doWork"), "{:?}", callees);
    }

    #[test]
    fn extract_java_facts_call_first_segment_keyword_filter() {
        // `String.format("...")` — first dotted segment is
        // "String" which IS a Java keyword. Legacy filters via
        // `callee.split(".")[0]`, so this call is dropped.
        let src = "public class App {\n\
                       public void greet() {\n\
                           String.format(\"hi\");\n\
                           Logger.info(\"x\");\n\
                       }\n\
                   }\n";
        let facts = extract_java_facts("App.java", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(
            !callees.contains(&"String.format"),
            "String.format leaked: {:?}",
            callees
        );
        assert!(callees.contains(&"Logger.info"), "{:?}", callees);
    }

    #[test]
    fn extract_java_facts_full_fixture_byte_equal() {
        // Hand-evaluated parity fixture. A 7-line Java class
        // with import + extends + method.
        let src = "package x;\n\
                   \n\
                   import java.util.List;\n\
                   \n\
                   public class Foo extends Bar {\n\
                       public void greet() {\n\
                           handle();\n\
                       }\n\
                   }\n";
        let facts = extract_java_facts("Foo.java", src);

        // Expected ordering: pass 1 (imports), then pass 2
        // (class defines + extends + method defines), then
        // pass 3 (calls).
        let expected = vec![
            // 1. import java.util.List
            SyntacticAstFact {
                kind: FactKind::ImportsFrom,
                language: "java".to_string(),
                source_symbol: "module:Foo.java".to_string(),
                source_display_name: "Foo.java".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "module:java.util.List".to_string(),
                target_display_name: "java.util.List".to_string(),
                target_kind: SymbolKind::External,
                file_path: "Foo.java".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // 2. class Foo defines
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "java".to_string(),
                source_symbol: "module:Foo.java".to_string(),
                source_display_name: "Foo.java".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "class:Foo.java:Foo".to_string(),
                target_display_name: "Foo".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "Foo.java".to_string(),
                start_line: 5,
                end_line: 5,
                confidence: CONFIDENCE_INFERRED,
            },
            // 3. extends Bar
            SyntacticAstFact {
                kind: FactKind::Extends,
                language: "java".to_string(),
                source_symbol: "class:Foo.java:Foo".to_string(),
                source_display_name: "Foo".to_string(),
                source_kind: SymbolKind::Class,
                target_symbol: "class:?:Bar".to_string(),
                target_display_name: "Bar".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "Foo.java".to_string(),
                start_line: 5,
                end_line: 5,
                confidence: CONFIDENCE_INFERRED,
            },
            // 4. method greet defines. NOTE: `file_path: "Foo"`
            // (not "Foo.java") is intentional — push_defines
            // sets `file_path = from_display`, and the
            // method call site passes `scope.name` (the class
            // name) as from_display. Legacy quirk
            // (syntacticAstExtractor.ts:684 `filePath:
            // fromDisplay`), preserved for byte-equal parity.
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "java".to_string(),
                source_symbol: "class:Foo.java:Foo".to_string(),
                source_display_name: "Foo".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "class:Foo.java:Foo#greet".to_string(),
                target_display_name: "greet".to_string(),
                target_kind: SymbolKind::Method,
                file_path: "Foo".to_string(),
                start_line: 6,
                end_line: 6,
                confidence: CONFIDENCE_INFERRED,
            },
            // 5. call `greet(` on line 6 — the legacy call pass
            // is a separate top-to-bottom regex sweep with no
            // scope filter, so the method-declaration line
            // ALSO produces a call fact for the method's own
            // name. Quirk of the legacy; preserved for parity.
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "java".to_string(),
                source_symbol: "module:Foo.java".to_string(),
                source_display_name: "Foo.java".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:?:greet".to_string(),
                target_display_name: "greet".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "Foo.java".to_string(),
                start_line: 6,
                end_line: 6,
                confidence: CONFIDENCE_INFERRED,
            },
            // 6. call handle() on line 7. Attributed to module.
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "java".to_string(),
                source_symbol: "module:Foo.java".to_string(),
                source_display_name: "Foo.java".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:?:handle".to_string(),
                target_display_name: "handle".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "Foo.java".to_string(),
                start_line: 7,
                end_line: 7,
                confidence: CONFIDENCE_INFERRED,
            },
        ];
        assert_eq!(
            facts.len(),
            expected.len(),
            "fact count mismatch\nactual: {:#?}",
            facts
        );
        for (i, (a, e)) in facts.iter().zip(expected.iter()).enumerate() {
            assert_eq!(
                a, e,
                "fact #{} mismatch\nactual: {:#?}\nexpected: {:#?}",
                i, a, e
            );
        }
    }

    // ===== Ruby extractor tests =====

    #[test]
    fn extract_ruby_facts_emits_require_and_require_relative() {
        let src = "require \"json\"\n\
                   require_relative \"./helpers\"\n";
        let facts = extract_ruby_facts("a.rb", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 2);
        let targets: Vec<&str> = imports
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(targets.contains(&"json"));
        assert!(targets.contains(&"./helpers"));
        // The relative one starts with "." → target_kind=module.
        let relative = imports
            .iter()
            .find(|f| f.target_display_name == "./helpers")
            .unwrap();
        assert_eq!(relative.target_kind, SymbolKind::Module);
        // The gem one doesn't → target_kind=external.
        let json = imports
            .iter()
            .find(|f| f.target_display_name == "json")
            .unwrap();
        assert_eq!(json.target_kind, SymbolKind::External);
    }

    #[test]
    fn extract_ruby_facts_class_with_extends() {
        let src = "class User < ApplicationRecord\nend\n";
        let facts = extract_ruby_facts("u.rb", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "User");
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 1);
        assert_eq!(extends[0].target_display_name, "ApplicationRecord");
        assert_eq!(extends[0].target_symbol, "class:?:ApplicationRecord");
    }

    #[test]
    fn extract_ruby_facts_class_with_namespaced_extends() {
        // `class Foo < Bar::Baz` — the base class regex
        // `[A-Za-z_][A-Za-z0-9_:]*` permits `:` for Ruby's
        // `::` namespace separator.
        let src = "class Worker < ActiveJob::Base\nend\n";
        let facts = extract_ruby_facts("w.rb", src);
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 1);
        assert_eq!(extends[0].target_display_name, "ActiveJob::Base");
    }

    #[test]
    fn extract_ruby_facts_def_and_def_self_dot() {
        // Both `def foo` and `def self.bar` emit `defines`
        // with the captured name (the `self.` prefix is
        // dropped).
        let src = "def foo\nend\n\
                   def self.bar\nend\n";
        let facts = extract_ruby_facts("x.rb", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 2);
        let names: Vec<&str> = defines
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(names.contains(&"foo"));
        assert!(names.contains(&"bar"));
    }

    #[test]
    fn extract_ruby_facts_method_name_with_bang_question_equals() {
        // Ruby allows `!`/`?`/`=` suffixes on method names.
        // The regex `[A-Za-z_][A-Za-z0-9_!?=]*` captures them.
        let src = "def save!\nend\n\
                   def valid?\nend\n\
                   def name=\nend\n";
        let facts = extract_ruby_facts("m.rb", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        let names: Vec<&str> = defines
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(names.contains(&"save!"));
        assert!(names.contains(&"valid?"));
        assert!(names.contains(&"name="));
    }

    #[test]
    fn extract_ruby_facts_calls_inside_def_body() {
        let src = "def main\n  handle()\n  raise(\"err\")\nend\n";
        let facts = extract_ruby_facts("m.rb", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(callees.contains(&"handle"), "{:?}", callees);
        // `raise` is a Ruby keyword → filtered out.
        assert!(!callees.contains(&"raise"), "raise leaked: {:?}", callees);
    }

    #[test]
    fn extract_ruby_facts_end_pops_scope() {
        // After `end`, current_func resets — calls on
        // subsequent lines are not attributed.
        let src = "def main\n  inside()\nend\noutside()\n";
        let facts = extract_ruby_facts("m.rb", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(callees.contains(&"inside"));
        assert!(
            !callees.contains(&"outside"),
            "post-end call leaked: {:?}",
            callees
        );
    }

    #[test]
    fn extract_ruby_facts_inner_if_end_pops_def_scope_quirk() {
        // Critic 2 advisory: pin the documented quirk where an
        // inner block's `end` (e.g. `if ... end`) prematurely
        // exits the enclosing `def` scope, because the legacy
        // `/^\s*end\b/` regex doesn't track nesting. Without
        // this test, a later "improvement" that fixed the
        // quirk would silently break byte-equal parity.
        let src = "def main\n  if cond\n  end\n  after()\nend\n";
        let facts = extract_ruby_facts("q.rb", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        // `after` is on line 4, AFTER the inner `if`'s `end`
        // on line 3 prematurely popped current_func. So
        // `after` is NOT attributed to `main` and NOT emitted.
        assert!(
            !callees.contains(&"after"),
            "quirk regression: post-inner-end call should NOT emit (got {:?})",
            callees
        );
    }

    #[test]
    fn extract_ruby_facts_skips_self_recursion() {
        let src = "def recur\n  recur()\n  other()\nend\n";
        let facts = extract_ruby_facts("r.rb", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(
            !callees.contains(&"recur"),
            "self-recursion leaked: {:?}",
            callees
        );
        assert!(callees.contains(&"other"));
    }

    #[test]
    fn extract_ruby_facts_full_fixture_byte_equal() {
        let src = "require \"json\"\n\
                   \n\
                   class Worker < ApplicationJob\n\
                     def perform\n\
                       run()\n\
                     end\n\
                   end\n";
        let facts = extract_ruby_facts("worker.rb", src);
        let expected = vec![
            // 1. require json
            SyntacticAstFact {
                kind: FactKind::ImportsFrom,
                language: "ruby".to_string(),
                source_symbol: "module:worker.rb".to_string(),
                source_display_name: "worker.rb".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "module:json".to_string(),
                target_display_name: "json".to_string(),
                target_kind: SymbolKind::External,
                file_path: "worker.rb".to_string(),
                start_line: 1,
                end_line: 1,
                confidence: CONFIDENCE_INFERRED,
            },
            // 2. class Worker defines
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "ruby".to_string(),
                source_symbol: "module:worker.rb".to_string(),
                source_display_name: "worker.rb".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "class:worker.rb:Worker".to_string(),
                target_display_name: "Worker".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "worker.rb".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // 3. extends ApplicationJob
            SyntacticAstFact {
                kind: FactKind::Extends,
                language: "ruby".to_string(),
                source_symbol: "class:worker.rb:Worker".to_string(),
                source_display_name: "Worker".to_string(),
                source_kind: SymbolKind::Class,
                target_symbol: "class:?:ApplicationJob".to_string(),
                target_display_name: "ApplicationJob".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "worker.rb".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // 4. def perform
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "ruby".to_string(),
                source_symbol: "module:worker.rb".to_string(),
                source_display_name: "worker.rb".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:worker.rb:perform".to_string(),
                target_display_name: "perform".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "worker.rb".to_string(),
                start_line: 4,
                end_line: 4,
                confidence: CONFIDENCE_INFERRED,
            },
            // 5. call run() inside perform body
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "ruby".to_string(),
                source_symbol: "function:worker.rb:perform".to_string(),
                source_display_name: "perform".to_string(),
                source_kind: SymbolKind::Function,
                target_symbol: "function:?:run".to_string(),
                target_display_name: "run".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "worker.rb".to_string(),
                start_line: 5,
                end_line: 5,
                confidence: CONFIDENCE_INFERRED,
            },
        ];
        assert_eq!(
            facts.len(),
            expected.len(),
            "fact count mismatch\nactual: {:#?}",
            facts
        );
        for (i, (a, e)) in facts.iter().zip(expected.iter()).enumerate() {
            assert_eq!(
                a, e,
                "fact #{} mismatch\nactual: {:#?}\nexpected: {:#?}",
                i, a, e
            );
        }
    }

    // ===== C# extractor tests =====

    #[test]
    fn extract_csharp_facts_emits_using_plain_and_static() {
        let src = "using System;\n\
                   using static System.Math;\n";
        let facts = extract_csharp_facts("a.cs", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 2);
        let targets: Vec<&str> = imports
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(targets.contains(&"System"));
        assert!(targets.contains(&"System.Math"));
    }

    #[test]
    fn extract_csharp_facts_class_with_base_list() {
        let src = "public class Service : BaseService, IRunnable, IDisposable\n{\n}\n";
        let facts = extract_csharp_facts("Service.cs", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "Service");
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 3);
        let names: Vec<&str> = extends
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(names.contains(&"BaseService"));
        assert!(names.contains(&"IRunnable"));
        assert!(names.contains(&"IDisposable"));
    }

    #[test]
    fn extract_csharp_facts_struct_and_record_treated_as_class() {
        let src = "public struct Point : IEquatable<Point> { }\n\
                   public record User(string Name) : Entity;\n";
        let facts = extract_csharp_facts("x.cs", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 2);
        // All defines have target_kind=Class regardless of
        // struct/record/interface keyword.
        for d in &defines {
            assert_eq!(d.target_kind, SymbolKind::Class, "{:?}", d);
        }
        let names: Vec<&str> = defines
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(names.contains(&"Point"));
        assert!(names.contains(&"User"));
    }

    #[test]
    fn extract_csharp_facts_strips_generics_from_base_list() {
        let src = "public class A : List<string>, IEnumerable<int> { }\n";
        let facts = extract_csharp_facts("A.cs", src);
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        // First base: "List<string>" → split('<')[0] = "List".
        // Second base: " IEnumerable<int>" → " IEnumerable" →
        // trim → "IEnumerable".
        let names: Vec<&str> = extends
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(names.contains(&"List"), "{:?}", names);
        assert!(names.contains(&"IEnumerable"), "{:?}", names);
    }

    #[test]
    fn extract_csharp_facts_bare_class_without_modifier_not_emitted() {
        // Legacy quirk: the class regex REQUIRES a visibility
        // modifier. `class Foo {}` (no `public`/`private`/etc.)
        // does NOT match. Pinned for parity.
        let src = "class Foo {}\n";
        let facts = extract_csharp_facts("foo.cs", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 0, "bare class leaked: {:?}", defines);
    }

    #[test]
    fn extract_csharp_facts_call_first_segment_keyword_filter() {
        let src = "public class App\n{\n  public void Greet() { string.Format(\"hi\"); Logger.Info(\"x\"); }\n}\n";
        let facts = extract_csharp_facts("App.cs", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        // `string` is a C# keyword → string.Format rejected.
        assert!(!callees.contains(&"string.Format"), "{:?}", callees);
        // `Logger` is not a keyword → Logger.Info accepted.
        assert!(callees.contains(&"Logger.Info"), "{:?}", callees);
    }

    #[test]
    fn extract_csharp_facts_calls_attributed_to_module() {
        let src = "public class A { public void f() { Helper(); } }\n";
        let facts = extract_csharp_facts("A.cs", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        for c in &calls {
            assert_eq!(c.source_kind, SymbolKind::Module, "{:?}", c);
        }
    }

    #[test]
    fn extract_csharp_facts_full_fixture_byte_equal() {
        let src = "using System;\n\
                   \n\
                   public class Foo : Bar\n\
                   {\n\
                       public void Greet() { Hello(); }\n\
                   }\n";
        let facts = extract_csharp_facts("Foo.cs", src);
        let expected = vec![
            // 1. using System;
            SyntacticAstFact {
                kind: FactKind::ImportsFrom,
                language: "csharp".to_string(),
                source_symbol: "module:Foo.cs".to_string(),
                source_display_name: "Foo.cs".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "module:System".to_string(),
                target_display_name: "System".to_string(),
                target_kind: SymbolKind::External,
                file_path: "Foo.cs".to_string(),
                start_line: 1,
                end_line: 1,
                confidence: CONFIDENCE_INFERRED,
            },
            // 2. class Foo defines (line 3)
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "csharp".to_string(),
                source_symbol: "module:Foo.cs".to_string(),
                source_display_name: "Foo.cs".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "class:Foo.cs:Foo".to_string(),
                target_display_name: "Foo".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "Foo.cs".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // 3. extends Bar (line 3)
            SyntacticAstFact {
                kind: FactKind::Extends,
                language: "csharp".to_string(),
                source_symbol: "class:Foo.cs:Foo".to_string(),
                source_display_name: "Foo".to_string(),
                source_kind: SymbolKind::Class,
                target_symbol: "class:?:Bar".to_string(),
                target_display_name: "Bar".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "Foo.cs".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // 4. call Greet() on line 5 — method-decl line
            //    produces a self-call fact since the call sweep
            //    is inline with no scope filter (same quirk as
            //    Java pass 3).
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "csharp".to_string(),
                source_symbol: "module:Foo.cs".to_string(),
                source_display_name: "Foo.cs".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:?:Greet".to_string(),
                target_display_name: "Greet".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "Foo.cs".to_string(),
                start_line: 5,
                end_line: 5,
                confidence: CONFIDENCE_INFERRED,
            },
            // 5. call Hello() on line 5
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "csharp".to_string(),
                source_symbol: "module:Foo.cs".to_string(),
                source_display_name: "Foo.cs".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:?:Hello".to_string(),
                target_display_name: "Hello".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "Foo.cs".to_string(),
                start_line: 5,
                end_line: 5,
                confidence: CONFIDENCE_INFERRED,
            },
        ];
        assert_eq!(
            facts.len(),
            expected.len(),
            "fact count mismatch\nactual: {:#?}",
            facts
        );
        for (i, (a, e)) in facts.iter().zip(expected.iter()).enumerate() {
            assert_eq!(
                a, e,
                "fact #{} mismatch\nactual: {:#?}\nexpected: {:#?}",
                i, a, e
            );
        }
    }

    // ===== Rust extractor tests =====

    #[test]
    fn extract_rust_facts_emits_use_simple_path() {
        let src = "use std::collections::HashMap;\n";
        let facts = extract_rust_facts("a.rs", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 1);
        assert_eq!(imports[0].target_display_name, "std::collections::HashMap");
    }

    #[test]
    fn extract_rust_facts_emits_use_brace_list_head_only() {
        // `use foo::{a, b, c};` captures only "foo" (the
        // brace contents are consumed but not captured). Pin
        // legacy behavior.
        let src = "use foo::bar::{baz, qux};\n";
        let facts = extract_rust_facts("a.rs", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 1);
        assert_eq!(imports[0].target_display_name, "foo::bar");
    }

    #[test]
    fn extract_rust_facts_fn_variants() {
        // All 4 fn variants: plain / pub / async / pub async.
        let src = "fn a() {}\n\
                   pub fn b() {}\n\
                   async fn c() {}\n\
                   pub async fn d() {}\n";
        let facts = extract_rust_facts("x.rs", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 4);
        let names: Vec<&str> = defines
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        for name in &["a", "b", "c", "d"] {
            assert!(names.contains(name), "missing {} in {:?}", name, names);
        }
    }

    #[test]
    fn extract_rust_facts_struct_enum_trait_all_emit_class() {
        let src = "pub struct Point;\n\
                   pub enum Color { Red, Green }\n\
                   pub trait Draw { fn draw(&self); }\n";
        let facts = extract_rust_facts("x.rs", src);
        // Filter out the inner `fn draw(&self);` line —
        // that's a fn defines, not a struct/enum/trait one.
        let class_defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines && f.target_kind == SymbolKind::Class)
            .collect();
        assert_eq!(class_defines.len(), 3);
        let names: Vec<&str> = class_defines
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(names.contains(&"Point"));
        assert!(names.contains(&"Color"));
        assert!(names.contains(&"Draw"));
    }

    #[test]
    fn extract_rust_facts_calls_use_colon_colon_separator() {
        // The Rust call regex captures `name::path::call`
        // shapes. The first segment is checked against the
        // keyword set — "Vec" IS a Rust keyword (per the
        // legacy keyword list at syntacticAstExtractor.ts:649)
        // so `Vec::new()` is REJECTED. Pin this quirk.
        let src = "fn main() {\n  Vec::new();\n  other_helper();\n}\n";
        let facts = extract_rust_facts("m.rs", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        // Vec::new rejected via first-segment keyword filter.
        assert!(
            !callees.contains(&"Vec::new"),
            "Vec::new leaked: {:?}",
            callees
        );
        // other_helper accepted (not a keyword).
        assert!(callees.contains(&"other_helper"), "{:?}", callees);
    }

    #[test]
    fn extract_rust_facts_skips_self_recursion() {
        let src = "fn recur() {\n  recur();\n  other();\n}\n";
        let facts = extract_rust_facts("r.rs", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(
            !callees.contains(&"recur"),
            "self-recursion leaked: {:?}",
            callees
        );
        assert!(callees.contains(&"other"));
    }

    #[test]
    fn extract_rust_facts_brace_tracking_exits_scope() {
        let src = "fn outer() {\n  inside();\n}\n\
                   outside();\n";
        let facts = extract_rust_facts("m.rs", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(callees.contains(&"inside"));
        // `outside()` is top-level, after brace closed — not
        // attributed to outer.
        assert!(
            !callees.contains(&"outside"),
            "post-brace call leaked: {:?}",
            callees
        );
    }

    #[test]
    fn extract_rust_facts_full_fixture_byte_equal() {
        let src = "use std::fmt;\n\
                   \n\
                   pub struct Point;\n\
                   \n\
                   pub fn greet() {\n\
                       say_hi();\n\
                   }\n";
        let facts = extract_rust_facts("lib.rs", src);
        let expected = vec![
            // 1. use std::fmt
            SyntacticAstFact {
                kind: FactKind::ImportsFrom,
                language: "rust".to_string(),
                source_symbol: "module:lib.rs".to_string(),
                source_display_name: "lib.rs".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "module:std::fmt".to_string(),
                target_display_name: "std::fmt".to_string(),
                target_kind: SymbolKind::External,
                file_path: "lib.rs".to_string(),
                start_line: 1,
                end_line: 1,
                confidence: CONFIDENCE_INFERRED,
            },
            // 2. struct Point defines
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "rust".to_string(),
                source_symbol: "module:lib.rs".to_string(),
                source_display_name: "lib.rs".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "class:lib.rs:Point".to_string(),
                target_display_name: "Point".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "lib.rs".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // 3. pub fn greet defines
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "rust".to_string(),
                source_symbol: "module:lib.rs".to_string(),
                source_display_name: "lib.rs".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:lib.rs:greet".to_string(),
                target_display_name: "greet".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "lib.rs".to_string(),
                start_line: 5,
                end_line: 5,
                confidence: CONFIDENCE_INFERRED,
            },
            // 4. say_hi() inside greet body
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "rust".to_string(),
                source_symbol: "function:lib.rs:greet".to_string(),
                source_display_name: "greet".to_string(),
                source_kind: SymbolKind::Function,
                target_symbol: "function:?:say_hi".to_string(),
                target_display_name: "say_hi".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "lib.rs".to_string(),
                start_line: 6,
                end_line: 6,
                confidence: CONFIDENCE_INFERRED,
            },
        ];
        assert_eq!(
            facts.len(),
            expected.len(),
            "fact count mismatch\nactual: {:#?}",
            facts
        );
        for (i, (a, e)) in facts.iter().zip(expected.iter()).enumerate() {
            assert_eq!(
                a, e,
                "fact #{} mismatch\nactual: {:#?}\nexpected: {:#?}",
                i, a, e
            );
        }
    }

    // ===== Dart extractor tests =====

    #[test]
    fn extract_dart_facts_emits_import_both_quote_styles() {
        let src = "import 'package:flutter/material.dart';\n\
                   import \"./helpers.dart\";\n";
        let facts = extract_dart_facts("a.dart", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert_eq!(imports.len(), 2);
        let targets: Vec<&str> = imports
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(targets.contains(&"package:flutter/material.dart"));
        assert!(targets.contains(&"./helpers.dart"));
        // The relative import (starts with `.`) maps to
        // target_kind=module; the package URL doesn't.
        let relative = imports
            .iter()
            .find(|f| f.target_display_name == "./helpers.dart")
            .unwrap();
        assert_eq!(relative.target_kind, SymbolKind::Module);
        let pkg = imports
            .iter()
            .find(|f| f.target_display_name == "package:flutter/material.dart")
            .unwrap();
        assert_eq!(pkg.target_kind, SymbolKind::External);
    }

    #[test]
    fn extract_dart_facts_class_with_extends() {
        let src = "class Foo extends Bar {}\n";
        let facts = extract_dart_facts("x.dart", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "Foo");
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 1);
        assert_eq!(extends[0].target_display_name, "Bar");
    }

    #[test]
    fn extract_dart_facts_abstract_class_with_extends() {
        let src = "abstract class Animal extends LivingThing {}\n";
        let facts = extract_dart_facts("a.dart", src);
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        assert_eq!(defines[0].target_display_name, "Animal");
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 1);
        assert_eq!(extends[0].target_display_name, "LivingThing");
    }

    #[test]
    fn extract_dart_facts_strips_generic_from_extends() {
        let src = "class Repo extends State<MyWidget> {}\n";
        let facts = extract_dart_facts("r.dart", src);
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 1);
        // Generic stripped → "State", not "State<MyWidget>".
        assert_eq!(extends[0].target_display_name, "State");
    }

    #[test]
    fn extract_dart_facts_implements_clause_intentionally_not_emitted_quirk() {
        // Legacy quirk: Dart's class regex captures the
        // implements list as group 3 but the emission block
        // only references group 2 (extends). Pin this so a
        // future "fix" doesn't silently break parity.
        let src = "class Service implements Foo, Bar {}\n";
        let facts = extract_dart_facts("s.dart", src);
        let implements: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Implements)
            .collect();
        assert_eq!(
            implements.len(),
            0,
            "Dart implements should NOT emit (legacy quirk): {:?}",
            implements
        );
        // Defines + zero extends (no extends clause here).
        let defines: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .collect();
        assert_eq!(defines.len(), 1);
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 0);
    }

    #[test]
    fn extract_dart_facts_extends_then_implements_emits_only_extends() {
        // Combined `extends X implements Y, Z` — legacy
        // captures both groups but only emits the extends.
        let src = "class Widget extends StatelessWidget implements Comparable, Disposable {}\n";
        let facts = extract_dart_facts("w.dart", src);
        let extends: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Extends)
            .collect();
        assert_eq!(extends.len(), 1);
        assert_eq!(extends[0].target_display_name, "StatelessWidget");
        let implements: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Implements)
            .collect();
        assert_eq!(implements.len(), 0);
    }

    #[test]
    fn extract_dart_facts_calls_attributed_to_module_with_keyword_filter() {
        let src = "void main() {\n  runApp(MyApp());\n  if (debug) print(\"x\");\n}\n";
        let facts = extract_dart_facts("m.dart", src);
        let calls: Vec<&SyntacticAstFact> =
            facts.iter().filter(|f| f.kind == FactKind::Calls).collect();
        // All calls attributed to module scope.
        for c in &calls {
            assert_eq!(c.source_kind, SymbolKind::Module, "{:?}", c);
        }
        let callees: Vec<&str> = calls
            .iter()
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(callees.contains(&"runApp"), "{:?}", callees);
        assert!(callees.contains(&"MyApp"), "{:?}", callees);
        // `if` is a Dart keyword — rejected by first-segment
        // filter.
        assert!(!callees.contains(&"if"), "if leaked: {:?}", callees);
    }

    #[test]
    fn extract_dart_facts_full_fixture_byte_equal() {
        let src = "import 'package:flutter/widgets.dart';\n\
                   \n\
                   class Counter extends StatefulWidget {\n\
                       Widget build() { return Text(\"hi\"); }\n\
                   }\n";
        let facts = extract_dart_facts("counter.dart", src);
        let expected = vec![
            // 1. import package
            SyntacticAstFact {
                kind: FactKind::ImportsFrom,
                language: "dart".to_string(),
                source_symbol: "module:counter.dart".to_string(),
                source_display_name: "counter.dart".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "module:package:flutter/widgets.dart".to_string(),
                target_display_name: "package:flutter/widgets.dart".to_string(),
                target_kind: SymbolKind::External,
                file_path: "counter.dart".to_string(),
                start_line: 1,
                end_line: 1,
                confidence: CONFIDENCE_INFERRED,
            },
            // 2. class Counter defines (line 3)
            SyntacticAstFact {
                kind: FactKind::Defines,
                language: "dart".to_string(),
                source_symbol: "module:counter.dart".to_string(),
                source_display_name: "counter.dart".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "class:counter.dart:Counter".to_string(),
                target_display_name: "Counter".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "counter.dart".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // 3. extends StatefulWidget (line 3)
            SyntacticAstFact {
                kind: FactKind::Extends,
                language: "dart".to_string(),
                source_symbol: "class:counter.dart:Counter".to_string(),
                source_display_name: "Counter".to_string(),
                source_kind: SymbolKind::Class,
                target_symbol: "class:?:StatefulWidget".to_string(),
                target_display_name: "StatefulWidget".to_string(),
                target_kind: SymbolKind::Class,
                file_path: "counter.dart".to_string(),
                start_line: 3,
                end_line: 3,
                confidence: CONFIDENCE_INFERRED,
            },
            // 4. call build() on line 4 (method-decl line)
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "dart".to_string(),
                source_symbol: "module:counter.dart".to_string(),
                source_display_name: "counter.dart".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:?:build".to_string(),
                target_display_name: "build".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "counter.dart".to_string(),
                start_line: 4,
                end_line: 4,
                confidence: CONFIDENCE_INFERRED,
            },
            // 5. call Text() on line 4
            SyntacticAstFact {
                kind: FactKind::Calls,
                language: "dart".to_string(),
                source_symbol: "module:counter.dart".to_string(),
                source_display_name: "counter.dart".to_string(),
                source_kind: SymbolKind::Module,
                target_symbol: "function:?:Text".to_string(),
                target_display_name: "Text".to_string(),
                target_kind: SymbolKind::Function,
                file_path: "counter.dart".to_string(),
                start_line: 4,
                end_line: 4,
                confidence: CONFIDENCE_INFERRED,
            },
        ];
        assert_eq!(
            facts.len(),
            expected.len(),
            "fact count mismatch\nactual: {:#?}",
            facts
        );
        for (i, (a, e)) in facts.iter().zip(expected.iter()).enumerate() {
            assert_eq!(
                a, e,
                "fact #{} mismatch\nactual: {:#?}\nexpected: {:#?}",
                i, a, e
            );
        }
    }

    // ===== TypeScript / JavaScript tree-sitter tests =====

    #[test]
    fn extract_typescript_facts_tree_sitter_defines_imports_extends_and_calls() {
        let src = "import { authorizePayment } from \"@acme/payments\";\n\
                   export { createOrder } from \"./orders\";\n\
                   export interface PaymentPort extends BasePort { charge(): void; }\n\
                   export type CheckoutCommand = { id: string };\n\
                   export enum CheckoutState { Open }\n\
                   \n\
                   export class CheckoutController extends BaseController implements PaymentPort {\n\
                     handle = () => this.submit({ id: \"1\" });\n\
                     async submit(command: CheckoutCommand) {\n\
                       const order = await createOrder(command);\n\
                       return authorizePayment(order.id);\n\
                     }\n\
                   }\n\
                   \n\
                   export const registerCheckout = () => wireRoute(CheckoutController);\n";
        let facts = extract_typescript_facts("src/checkout/controller.ts", src);
        assert!(
            facts.iter().all(|f| f.confidence == CONFIDENCE_PARSED),
            "{:#?}",
            facts
        );
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::ImportsFrom && f.target_display_name == "@acme/payments"
        }));
        assert!(facts
            .iter()
            .any(|f| { f.kind == FactKind::ImportsFrom && f.target_display_name == "./orders" }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines
                && f.target_kind == SymbolKind::Interface
                && f.target_display_name == "PaymentPort"
        }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Extends
                && f.source_symbol == "interface:src/checkout/controller.ts:PaymentPort"
                && f.target_display_name == "BasePort"
        }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines && f.target_display_name == "CheckoutCommand"
        }));
        assert!(facts
            .iter()
            .any(|f| { f.kind == FactKind::Defines && f.target_display_name == "CheckoutState" }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines
                && f.target_kind == SymbolKind::Class
                && f.target_display_name == "CheckoutController"
        }));
        assert!(facts
            .iter()
            .any(|f| { f.kind == FactKind::Extends && f.target_display_name == "BaseController" }));
        assert!(facts
            .iter()
            .any(|f| { f.kind == FactKind::Implements && f.target_display_name == "PaymentPort" }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines
                && f.target_kind == SymbolKind::Method
                && f.source_symbol == "class:src/checkout/controller.ts:CheckoutController"
                && f.target_display_name == "submit"
        }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines
                && f.target_kind == SymbolKind::Method
                && f.source_symbol == "class:src/checkout/controller.ts:CheckoutController"
                && f.target_display_name == "handle"
        }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines
                && f.target_kind == SymbolKind::Function
                && f.target_display_name == "registerCheckout"
        }));
        let calls: Vec<&str> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Calls)
            .map(|f| f.target_display_name.as_str())
            .collect();
        for want in [
            "this.submit",
            "createOrder",
            "authorizePayment",
            "wireRoute",
        ] {
            assert!(calls.contains(&want), "missing {} in {:#?}", want, facts);
        }
    }

    #[test]
    fn extract_typescript_facts_exported_string_const_is_symbol_not_import() {
        let src = "export const tenantMarker = \"tenant_one_secret_symbol\";\n\
                   export function orga_owned_function() {\n\
                     return tenantMarker;\n\
                   }\n\
                   export const branchMarker = \"tenant_one_secret_symbol_release-a\";\n";
        let facts = extract_typescript_facts("src/tenant.ts", src);
        let imports: Vec<&SyntacticAstFact> = facts
            .iter()
            .filter(|f| f.kind == FactKind::ImportsFrom)
            .collect();
        assert!(
            imports.is_empty(),
            "string constants emitted imports: {imports:#?}"
        );
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines
                && f.target_kind == SymbolKind::Symbol
                && f.target_display_name == "tenantMarker"
        }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines
                && f.target_kind == SymbolKind::Symbol
                && f.target_display_name == "branchMarker"
        }));
        assert!(facts.iter().any(|f| {
            f.kind == FactKind::Defines
                && f.target_kind == SymbolKind::Function
                && f.target_display_name == "orga_owned_function"
        }));
    }

    #[test]
    fn extract_javascript_facts_tree_sitter_handles_commonjs_and_jsx_calls() {
        let src = "import React from 'react';\n\
                   const view = require('./view');\n\
                   const Button = () => render(<span />);\n\
                   function mount() {\n\
                     React.createElement(Button);\n\
                   }\n";
        let facts = extract_javascript_facts("src/Button.jsx", src);
        assert!(facts
            .iter()
            .any(|f| { f.kind == FactKind::ImportsFrom && f.target_display_name == "react" }));
        assert!(facts
            .iter()
            .any(|f| { f.kind == FactKind::ImportsFrom && f.target_display_name == "./view" }));
        assert!(facts
            .iter()
            .any(|f| { f.kind == FactKind::Defines && f.target_display_name == "Button" }));
        assert!(facts
            .iter()
            .any(|f| { f.kind == FactKind::Defines && f.target_display_name == "mount" }));
        let calls: Vec<&str> = facts
            .iter()
            .filter(|f| f.kind == FactKind::Calls)
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(calls.contains(&"render"), "{:#?}", facts);
        assert!(calls.contains(&"React.createElement"), "{:#?}", facts);
    }

    // ===== Walker tests =====

    use std::fs;
    use std::path::PathBuf;

    fn make_temp_repo(test_name: &str) -> PathBuf {
        let tmp = std::env::temp_dir().join(format!(
            "codeintel-r9i-{}-{}",
            test_name,
            std::process::id()
        ));
        let _ = fs::remove_dir_all(&tmp);
        fs::create_dir_all(&tmp).expect("mkdir");
        tmp
    }

    fn write_file(root: &std::path::Path, rel: &str, body: &str) {
        let p = root.join(rel);
        if let Some(parent) = p.parent() {
            fs::create_dir_all(parent).expect("mkdir parent");
        }
        fs::write(p, body).expect("write");
    }

    #[test]
    fn scan_typescript_and_javascript_repo_collects_tree_sitter_facts() {
        let root = make_temp_repo("ts-js-tree-sitter");
        write_file(
            &root,
            "src/index.ts",
            "import { startWorker } from './worker';\n\
             export function bootstrap() {\n\
               return startWorker();\n\
             }\n",
        );
        write_file(
            &root,
            "src/view.jsx",
            "import React from 'react';\n\
             const View = () => React.createElement('div');\n",
        );

        let ts = scan_syntactic_ast_facts(
            "typescript",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );
        assert_eq!(ts.scanned_file_count, 1);
        assert!(ts.facts.iter().any(|f| {
            f.language == "typescript"
                && f.kind == FactKind::Calls
                && f.target_display_name == "startWorker"
        }));

        let js = scan_syntactic_ast_facts(
            "javascript",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );
        assert_eq!(js.scanned_file_count, 1);
        assert!(js.facts.iter().any(|f| {
            f.language == "javascript"
                && f.kind == FactKind::Calls
                && f.target_display_name == "React.createElement"
        }));
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_multi_language_repo_walks_once_and_dispatches_by_extension() {
        let root = make_temp_repo("multi-language-shared-walk");
        write_file(
            &root,
            "src/index.ts",
            "export function bootstrap() { return startWorker(); }\n",
        );
        write_file(
            &root,
            "src/server.go",
            "package main\nfunc main() { handleOrder() }\nfunc handleOrder() {}\n",
        );
        write_file(
            &root,
            "src/jobs.py",
            "def run_job():\n    publish_event()\n",
        );
        write_file(
            &root,
            "node_modules/skip.ts",
            "export function shouldNotAppear() {}\n",
        );

        let results = scan_syntactic_ast_facts_multi(
            &["typescript", "go", "python"],
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );
        let by_language: std::collections::HashMap<String, SyntacticAstScanResult> =
            results.into_iter().collect();

        let ts = by_language.get("typescript").expect("typescript result");
        assert_eq!(ts.scanned_file_count, 1);
        assert!(ts
            .facts
            .iter()
            .any(|f| f.target_display_name == "bootstrap"));
        assert!(!ts
            .facts
            .iter()
            .any(|f| f.target_display_name == "shouldNotAppear"));

        let go = by_language.get("go").expect("go result");
        assert_eq!(go.scanned_file_count, 1);
        assert!(go
            .facts
            .iter()
            .any(|f| f.target_display_name == "handleOrder"));

        let python = by_language.get("python").expect("python result");
        assert_eq!(python.scanned_file_count, 1);
        assert!(python
            .facts
            .iter()
            .any(|f| f.target_display_name == "publish_event"));
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_unknown_language_emits_warning_no_facts() {
        let root = make_temp_repo("unknown-lang");
        let result = scan_syntactic_ast_facts(
            "klingon",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );
        assert_eq!(result.facts.len(), 0);
        assert_eq!(result.scanned_file_count, 0);
        assert!(result
            .warnings
            .iter()
            .any(|w| w.contains("No syntactic extractor for language klingon")));
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_go_repo_collects_facts_across_files() {
        let root = make_temp_repo("go-multi-file");
        write_file(
            &root,
            "main.go",
            "package main\n\nimport \"fmt\"\n\nfunc main() {\n  fmt.Println()\n}\n",
        );
        write_file(
            &root,
            "internal/helper.go",
            "package internal\n\nfunc Helper() {\n  doWork()\n}\n",
        );
        // README.md is NOT in the .go extension whitelist, so
        // it should not be opened.
        write_file(&root, "README.md", "# repo\n");

        let result = scan_syntactic_ast_facts(
            "go",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );
        // 2 .go files scanned; .md not counted.
        assert_eq!(result.scanned_file_count, 2);
        assert!(result
            .facts
            .iter()
            .any(|f| f.target_display_name == "fmt" && f.kind == FactKind::ImportsFrom));
        assert!(result
            .facts
            .iter()
            .any(|f| f.target_display_name == "main" && f.kind == FactKind::Defines));
        assert!(result
            .facts
            .iter()
            .any(|f| f.target_display_name == "Helper" && f.kind == FactKind::Defines));
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_skips_ignored_directories() {
        let root = make_temp_repo("ignored-dirs");
        write_file(&root, "real/file.py", "import os\n");
        // Each of these should NOT be visited.
        write_file(&root, "node_modules/leaked.py", "import secret\n");
        write_file(&root, "dist/leaked.py", "import secret\n");
        write_file(&root, "vendor/leaked.py", "import secret\n");
        write_file(&root, "__pycache__/leaked.py", "import secret\n");
        // Dotted dir skipped too (matches legacy `name.startsWith(".")` guard).
        write_file(&root, ".git/leaked.py", "import secret\n");
        write_file(&root, ".venv/leaked.py", "import secret\n");

        let result = scan_syntactic_ast_facts(
            "python",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );
        // Only real/file.py scanned.
        assert_eq!(
            result.scanned_file_count, 1,
            "expected exactly the non-ignored .py file. Facts: {:?}",
            result.facts
        );
        // None of the imports should be `secret` (which only
        // appears in ignored dirs).
        for f in &result.facts {
            assert_ne!(f.target_display_name, "secret", "ignored-dir leak: {:?}", f);
        }
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_honors_per_file_size_budget() {
        let root = make_temp_repo("size-budget");
        // Small file — should be scanned.
        write_file(&root, "small.rs", "fn small() {}\n");
        // Large file — should be skipped (counted as
        // skipped, not scanned).
        let large_body = "fn x() {}\n".repeat(50_000); // ~500KB
        write_file(&root, "huge.rs", &large_body);

        let result = scan_syntactic_ast_facts(
            "rust",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: Some(1024), // 1KB cap — both should be size-checked
                max_directories: None,
            },
        );
        // `small.rs` is ~14 bytes → passes. `huge.rs` exceeds
        // 1KB → skipped.
        assert_eq!(result.scanned_file_count, 1, "{:?}", result.facts);
        assert_eq!(result.skipped_file_count, 1);
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_honors_max_files_budget() {
        // Parity: legacy gates max_files only in the outer
        // while-header — once a directory begins iteration, it
        // is scanned to completion. To test the cap precisely
        // we put files in SEPARATE directories so each subdir
        // = one while-iteration that re-checks the cap.
        let root = make_temp_repo("max-files");
        for i in 0..10 {
            write_file(
                &root,
                &format!("d{}/file.go", i),
                "package x\nfunc Foo() {}\n",
            );
        }
        let result = scan_syntactic_ast_facts(
            "go",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: Some(3),
                max_file_bytes: None,
                max_directories: None,
            },
        );
        // After the while-header sees scanned_file_count >= 3
        // at iteration boundary, the loop exits. The exact
        // boundary value matches legacy (one extra file may
        // sneak in for the directory that was already being
        // iterated when the threshold was crossed, but each
        // dir here only has 1 file, so the count lands at 3).
        assert_eq!(result.scanned_file_count, 3);
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_legacy_max_files_overshoot_within_directory() {
        // Parity-tightening test: in legacy semantics, a single
        // directory with N files exceeding max_files is scanned
        // to completion. The while-header re-check happens only
        // after the directory's full for-loop. Mirror exactly.
        let root = make_temp_repo("max-files-overshoot");
        for i in 0..10 {
            write_file(
                &root,
                &format!("file_{}.go", i),
                "package x\nfunc Foo() {}\n",
            );
        }
        let result = scan_syntactic_ast_facts(
            "go",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: Some(3),
                max_file_bytes: None,
                max_directories: None,
            },
        );
        // Legacy overshoots: all 10 files in the root dir are
        // scanned before the while-header re-checks.
        assert_eq!(result.scanned_file_count, 10);
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_honors_max_directories_budget_emits_warning() {
        // Build a small subtree so visited_dirs exceeds the
        // cap. With max_directories = 1, the very first dir
        // we'd dequeue beyond the root triggers the ceiling
        // warning.
        let root = make_temp_repo("max-dirs");
        write_file(&root, "a/x.go", "package a\nfunc X() {}\n");
        write_file(&root, "b/y.go", "package b\nfunc Y() {}\n");
        write_file(&root, "c/z.go", "package c\nfunc Z() {}\n");

        let result = scan_syntactic_ast_facts(
            "go",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: Some(1),
            },
        );
        // The ceiling warning must be emitted.
        assert!(
            result
                .warnings
                .iter()
                .any(|w| w.contains("hit maxDirectories=1")),
            "warnings: {:?}",
            result.warnings
        );
        // And the walk must have bailed before scanning every
        // sub-dir's .go file (root itself counted as visit 1,
        // so children are NOT visited).
        assert!(
            result.scanned_file_count < 3,
            "expected early bail; got {} scanned",
            result.scanned_file_count
        );
        let _ = fs::remove_dir_all(&root);
    }

    #[cfg(unix)]
    #[test]
    fn scan_silently_skips_symlinks() {
        // Symlinks (both file and directory targets) must be
        // skipped, regardless of where they point, and counted
        // in skipped_file_count. Unix-only because Windows
        // symlinks require elevated privileges in CI.
        use std::os::unix::fs::symlink;

        let root = make_temp_repo("symlink-skip");
        write_file(&root, "real.go", "package r\nfunc Real() {}\n");
        // File symlink → "real.go"
        let link_file = root.join("link.go");
        symlink(root.join("real.go"), &link_file).expect("file symlink");
        // Directory symlink → external dir we never want to
        // descend into.
        let external =
            std::env::temp_dir().join(format!("codeintel-r9i-external-{}", std::process::id()));
        let _ = fs::remove_dir_all(&external);
        fs::create_dir_all(&external).expect("mkdir external");
        write_file(&external, "leaked.go", "package l\nfunc Leaked() {}\n");
        let link_dir = root.join("linkdir");
        symlink(&external, &link_dir).expect("dir symlink");

        let result = scan_syntactic_ast_facts(
            "go",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );

        // Only the one real file scanned.
        assert_eq!(result.scanned_file_count, 1);
        // Both symlinks (file + dir) are counted as skipped.
        assert!(
            result.skipped_file_count >= 2,
            "expected >=2 symlink skips; got {}",
            result.skipped_file_count
        );
        // No "Leaked" fact made it through.
        assert!(
            !result
                .facts
                .iter()
                .any(|f| f.target_display_name == "Leaked"),
            "symlink-dir traversal leak: {:?}",
            result.facts
        );

        let _ = fs::remove_dir_all(&root);
        let _ = fs::remove_dir_all(&external);
    }

    #[test]
    fn scan_facts_byte_equal_against_hand_evaluated() {
        // Byte-equal parity test. Seeds a single .go file
        // exercising imports + defines + calls, then asserts
        // the full Vec equals a hand-computed expected Vec
        // (the same shape extract_go_facts produces directly).
        let root = make_temp_repo("byte-eq");
        let source = "package main\n\nimport \"fmt\"\n\nfunc main() {\n  fmt.Println()\n}\n";
        write_file(&root, "main.go", source);

        let result = scan_syntactic_ast_facts(
            "go",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );

        // Compute the expected facts the same way the walker
        // would (single-file dispatch with the same relative
        // path it emits — "main.go").
        let expected = extract_go_facts("main.go", source);
        assert_eq!(
            result.facts, expected,
            "walker output diverged from direct dispatch"
        );
        assert_eq!(result.scanned_file_count, 1);
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn scan_extension_filter_excludes_wrong_lang_files() {
        // Repo has both .rs and .go files. Scanning "rust"
        // language should only open .rs files.
        let root = make_temp_repo("ext-filter");
        write_file(&root, "lib.rs", "fn foo() {}\n");
        write_file(&root, "main.go", "package main\nfunc Bar() {}\n");

        let rust_result = scan_syntactic_ast_facts(
            "rust",
            SyntacticAstScanOptions {
                repo_root: &root,
                max_files: None,
                max_file_bytes: None,
                max_directories: None,
            },
        );
        assert_eq!(rust_result.scanned_file_count, 1);
        // The single rust-defines should be "foo", not "Bar".
        let names: Vec<&str> = rust_result
            .facts
            .iter()
            .filter(|f| f.kind == FactKind::Defines)
            .map(|f| f.target_display_name.as_str())
            .collect();
        assert!(names.contains(&"foo"));
        assert!(!names.contains(&"Bar"), "leaked go fact: {:?}", names);
        let _ = fs::remove_dir_all(&root);
    }

    #[test]
    fn fact_serializes_to_legacy_json_shape() {
        // Spot-check the wire format mirrors the legacy
        // camelCase + snake_case-for-kind shape exactly.
        let f = SyntacticAstFact {
            kind: FactKind::ImportsFrom,
            language: "go".to_string(),
            source_symbol: "module:main.go".to_string(),
            source_display_name: "main.go".to_string(),
            source_kind: SymbolKind::Module,
            target_symbol: "module:fmt".to_string(),
            target_display_name: "fmt".to_string(),
            target_kind: SymbolKind::External,
            file_path: "main.go".to_string(),
            start_line: 3,
            end_line: 3,
            confidence: CONFIDENCE_INFERRED,
        };
        let json = serde_json::to_string(&f).expect("serialize");
        assert!(json.contains("\"kind\":\"imports_from\""), "{}", json);
        assert!(
            json.contains("\"sourceSymbol\":\"module:main.go\""),
            "{}",
            json
        );
        assert!(json.contains("\"targetKind\":\"external\""), "{}", json);
        assert!(json.contains("\"startLine\":3"), "{}", json);
        assert!(json.contains("\"confidence\":0.6"), "{}", json);
    }
}
