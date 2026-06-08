package graphschema

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math"
	"strconv"
)

// This file ports three helpers from
// packages/backend/src/codeGraph/nebulaNgql.ts:
//
//   ngqlValue   — TS lines 154-165 — formats a CodeGraphPrimitive
//                 into the literal nGQL VALUES syntax expected by
//                 INSERT VERTEX / INSERT EDGE.
//   edgeRank    — TS lines 105-107 — derives a deterministic int
//                 from the first 8 hex chars of sha256(input). Used
//                 to disambiguate parallel edges in nGQL's
//                 INSERT EDGE `from->to@rank: (...)` syntax.
//   chunkArray  — TS lines 167-173 — slice generic chunker (250 is
//                 the writer's default batch size).
//
// Parity is verified at the byte level by nebula_ngql_test.go:
// the test runs ngqlValue + edgeRank + chunkArray against
// captured-golden inputs and asserts the Go output matches the
// JS algorithm exactly.

// ngqlValue formats a CodeGraphPrimitive into the literal nGQL
// VALUES syntax. Direct port of the JS:
//
//	const ngqlValue = (value: CodeGraphPrimitive) => {
//	    if (value === null) {
//	        return "NULL";
//	    }
//	    if (typeof value === "number") {
//	        return Number.isFinite(value) ? String(value) : "NULL";
//	    }
//	    if (typeof value === "boolean") {
//	        return value ? "true" : "false";
//	    }
//	    return JSON.stringify(value);
//	};
//
// The string branch routes through encoding/json with HTML
// escaping disabled — JS's JSON.stringify does not html-escape
// `<` / `>` / `&`, and the byte-equal contract requires Go to
// match. Numbers use strconv.FormatFloat with the JS String(x)
// shape (no trailing zeros, decimal form when small, etc.) —
// strconv's 'f'/-1 form matches JS's default ToString for finite
// floats. Integer types route through strconv.FormatInt to avoid
// the gratuitous ".0" suffix Go's FormatFloat would add.
func ngqlValue(value CodeGraphPrimitive) string {
	if value == nil {
		return "NULL"
	}
	switch v := value.(type) {
	case bool:
		if v {
			return "true"
		}
		return "false"
	case string:
		return jsonStringify(v)
	case int:
		return strconv.FormatInt(int64(v), 10)
	case int32:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case float32:
		return formatFloat(float64(v))
	case float64:
		return formatFloat(v)
	default:
		// A property map carrying a value the TS source's union
		// rejected (CodeGraphPrimitive = string|number|boolean|null)
		// is a writer bug. The TS implementation would also fail
		// here — JSON.stringify of an object returns "{}" without
		// surrounding quotes which would corrupt the surrounding
		// VALUES clause. Defensive choice: emit NULL so the row
		// still inserts (with the offending column nulled) and the
		// writer surfaces a downstream parity-test failure.
		return "NULL"
	}
}

// jsonStringify mirrors JS's JSON.stringify(string) — wraps in
// double quotes, escapes ", \, control chars, and unicode
// codepoints above U+FFFF as surrogate pairs. Critically does NOT
// html-escape < > & — Go's encoding/json defaults to escaping
// those, so SetEscapeHTML(false) is mandatory for byte-equal
// output against the JS golden.
func jsonStringify(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		// json.Marshal of a string cannot fail; surface a
		// recognisable diagnostic if invariants change in a future
		// Go release.
		return strconv.Quote(s)
	}
	// json.Encoder appends a trailing newline; strip it to match
	// the JS JSON.stringify(s) output.
	out := buf.Bytes()
	if len(out) > 0 && out[len(out)-1] == '\n' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// formatFloat mirrors JS's Number.toString for a finite double.
// Non-finite (NaN, ±Inf) values render NULL — matching the TS
// source's `Number.isFinite(value) ? String(value) : "NULL"`
// branch.
//
// strconv.FormatFloat with the 'g' verb and precision -1 produces
// the shortest decimal that round-trips to the same float64 —
// the same invariant JS's ToString relies on. JS prints integer-
// valued floats without a decimal (`String(3.0) === "3"`); Go's
// FormatFloat with 'g'/-1 does the same.
func formatFloat(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "NULL"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// edgeRank produces the deterministic int64 rank that
// disambiguates parallel edges in INSERT EDGE VALUES. Direct
// port of the JS:
//
//	export const edgeRank = (input: string) => {
//	    return parseInt(
//	        createHash("sha256").update(input).digest("hex").slice(0, 8),
//	        16,
//	    );
//	};
//
// "First 8 hex characters" === "first 4 bytes" in network byte
// order — Go reads the four bytes directly via
// binary.BigEndian.Uint32, which saves the 64-char hex-encode +
// re-parse round trip the literal JS algorithm requires. The
// numerical result is byte-identical (locked by parity test).
//
// The output range is [0, 0xFFFFFFFF] (32 bits). int64 holds it
// trivially; callers route through CodeGraphEdge.Rank.
func edgeRank(input string) int64 {
	sum := sha256.Sum256([]byte(input))
	return int64(binary.BigEndian.Uint32(sum[:4]))
}

// EdgeRank is the exported entry point — see edgeRank for the
// port docstring. The exported wrapper is the production
// surface; the unexported helper is the one parity-tested at
// the byte level.
func EdgeRank(input string) int64 {
	return edgeRank(input)
}

// chunkArray slices `values` into successive runs of length
// `chunkSize`. Direct port of the JS:
//
//	const chunkArray = <T>(values: T[], chunkSize: number) => {
//	    const chunks: T[][] = [];
//	    for (let index = 0; index < values.length; index += chunkSize) {
//	        chunks.push(values.slice(index, index + chunkSize));
//	    }
//	    return chunks;
//	};
//
// Used by RenderSnapshotStatements to bound the per-INSERT batch
// at 250 elements — matches the TS writer's batch ceiling.
// Generic over T so the same helper batches vertices and edges
// without two near-duplicates.
func chunkArray[T any](values []T, chunkSize int) [][]T {
	if chunkSize <= 0 {
		// JS doesn't guard against this — an infinite loop would
		// result. The Go port treats a non-positive chunkSize as
		// "everything in one chunk" so a writer typo doesn't hang
		// the process.
		if len(values) == 0 {
			return nil
		}
		return [][]T{values}
	}
	chunks := make([][]T, 0, (len(values)+chunkSize-1)/chunkSize)
	for i := 0; i < len(values); i += chunkSize {
		end := i + chunkSize
		if end > len(values) {
			end = len(values)
		}
		chunks = append(chunks, values[i:end])
	}
	return chunks
}
