// Q.B: natural-language → edge `context` filter inference.
//
// Port of the `_CONTEXT_HINTS` heuristic from
// `graphify reference implementation`.
// The Python reference scans the user's NL query for known
// hint substrings and accumulates the corresponding edge
// `context` values. The Go port follows the same shape but
// uses the codeintel edge-context vocabulary (call /
// definition / reference / import / inherits / type /
// containment).
//
// **Why this exists**: NebulaGraph BFS without a context
// filter expands through every neighbouring edge regardless
// of whether it answers the question. A query like "what calls
// processOrder" matched against an unfiltered graph returns
// the call edges, the import edges of the file, the contains
// edges from the repo, the type-defines of nearby symbols —
// the LLM consumer drowns in irrelevant relationships. With
// the context filter, only edges with `context == "call"`
// survive — the precision of the response goes up an order of
// magnitude and the latency drops too because we're traversing
// far fewer edges.
package graphreader

import "strings"

// contextHints is the lookup table that maps NL-hint
// substrings to edge `context` values. Substring matching is
// deliberate: we want "what calls X" and "find callers of X"
// and "who invokes X" to all infer ["call"] even though they
// use different surface phrasings.
//
// Entries with multiple values infer multiple contexts (e.g.
// "uses" maps to both "reference" and "call" because the
// English term is ambiguous between the two). The traversal
// query then filters edges where context IN [...].
//
// Ordering does not matter — the inferContextFilters function
// dedupes the accumulated values into a stable sorted slice
// before returning.
var contextHints = map[string][]string{
	// Call-graph hints.
	"who calls":  {"call"},
	"caller":     {"call"},
	"calls":      {"call"},
	"called by":  {"call"},
	"call site":  {"call"},
	"invokes":    {"call"},
	"invocation": {"call"},
	"callee":     {"call"},
	"call from":  {"call"},

	// Inheritance / interface implementation hints.
	"implements":       {"inherits"},
	"implementation":   {"inherits"},
	"implementations":  {"inherits"},
	"subclass":         {"inherits"},
	"subclasses":       {"inherits"},
	"superclass":       {"inherits"},
	"super class":      {"inherits"},
	"inherit":          {"inherits"},
	"inherits":         {"inherits"},
	"inheritance":      {"inherits"},
	"extends":          {"inherits"},
	"derived from":     {"inherits"},
	"base class":       {"inherits"},
	"parent class":     {"inherits"},
	"interface of":     {"inherits"},
	"protocol":         {"inherits"},
	"trait":            {"inherits"},

	// Type hints.
	"type of":     {"type"},
	"return type": {"type"},
	"returns":     {"type"},
	"typed":       {"type"},
	"typedef":     {"type"},

	// Import hints.
	"imports":         {"import"},
	"imported by":     {"import"},
	"importing":       {"import"},
	"depend on":       {"import"},
	"depends on":      {"import"},
	"dependencies of": {"import"},
	"dependency":      {"import"},

	// Reference / usage hints.
	"references":  {"reference"},
	"referenced":  {"reference"},
	"refers to":   {"reference"},
	"refer to":    {"reference"},
	"usages":      {"reference"},
	"uses":        {"reference", "call"},
	"used by":     {"reference"},
	"where is":    {"reference"},
	"where used":  {"reference"},

	// Definition hints.
	"defined":         {"definition"},
	"definition":      {"definition"},
	"declared":        {"definition"},
	"declaration":     {"definition"},
	"where defined":   {"definition"},

	// File-containment hints.
	"contains":      {"containment"},
	"files":         {"containment"},
	"file contains": {"containment"},
	"in file":       {"containment"},
}

// inferContextFilters scans `lowerQuery` (assumed already
// lowercased by the caller) for known NL hints and returns the
// deduped, sorted set of edge `context` values that match.
// Returns nil/empty when no hint fires — the caller should then
// treat ContextFilters as a no-op (NULL context edges are
// allowed through alongside everything else).
//
// Heuristic, not parsed: substring match is intentional so the
// LUT survives word-order variations. False positives are
// possible but contained — when the user types something the
// LUT misses they can supply an explicit context filter via the
// API. Worst case the filter is empty and we degrade to no
// context restriction (current behaviour).
func inferContextFilters(lowerQuery string) []string {
	if lowerQuery == "" {
		return nil
	}
	seen := make(map[string]struct{}, 4)
	for hint, ctxs := range contextHints {
		if strings.Contains(lowerQuery, hint) {
			for _, c := range ctxs {
				seen[c] = struct{}{}
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	// Stable sort so identical queries produce identical
	// QueryPlan output (tests + caching downstream).
	sortStrings(out)
	return out
}

// sortStrings is a tiny stable sort kept local so the package
// has zero new external dependencies. Insertion sort is fine
// here — the LUT can match at most ~10 distinct contexts.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
