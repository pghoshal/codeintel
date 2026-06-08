package graphschema

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestNgqlValue_Parity locks the byte-for-byte equivalence
// between the Go ngqlValue port and the captured JS golden. The
// fixture table was produced by re-running the JS algorithm from
// packages/backend/src/codeGraph/nebulaNgql.ts (lines 154-165)
// against the listed inputs and snapshotting the output.
//
// Critical edge cases pinned here:
//   - null → "NULL" (uppercase, no quotes)
//   - NaN / ±Inf → "NULL" (TS branch on !Number.isFinite)
//   - booleans → "true" / "false" (lowercase, no quotes)
//   - strings route through JSON.stringify (HTML-special chars
//     `<` `>` `&` MUST NOT escape — Go's encoding/json defaults
//     to escaping; the port disables that with SetEscapeHTML)
//   - integer floats render without a trailing ".0" (TS:
//     String(3.0) === "3"; Go: strconv FormatFloat 'g'/-1)
func TestNgqlValue_Parity(t *testing.T) {
	cases := []struct {
		name string
		in   CodeGraphPrimitive
		want string
	}{
		{"null", nil, "NULL"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"int small", 42, "42"},
		{"int zero", 0, "0"},
		{"int negative", -7, "-7"},
		{"int large", int64(2147483647), "2147483647"},
		{"float", 3.14, "3.14"},
		{"float integer", 3.0, "3"},
		{"float negative", -2.5, "-2.5"},
		{"NaN", math.NaN(), "NULL"},
		{"positive infinity", math.Inf(1), "NULL"},
		{"negative infinity", math.Inf(-1), "NULL"},
		{"string empty", "", `""`},
		{"string plain", "hello", `"hello"`},
		{"string with quote", `a"b`, `"a\"b"`},
		{"string with backslash", `a\b`, `"a\\b"`},
		{"string with newline", "a\nb", `"a\nb"`},
		{"string with tab", "a\tb", `"a\tb"`},
		{"string with html", "<script>&", `"<script>&"`},
		{"string with unicode", "héllo", `"héllo"`},
		// Surrogate-pair codepoint — JS JSON.stringify and Go's
		// encoding/json both emit raw UTF-8 bytes (not a `🎉`
		// escape). Lock both implementations agree.
		{"string with emoji", "🎉", `"🎉"`},
		{"string with emoji inline", "a🎉b", `"a🎉b"`},
		// NUL byte: both implementations emit the JSON Unicode escape
		// rather than the raw byte.
		{"string with NUL byte", "a\x00b", "\"a\\u0000b\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ngqlValue(tc.in)
			if got != tc.want {
				t.Errorf("ngqlValue(%v): got %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEdgeRank_Parity locks the deterministic
// sha256-prefix-as-int output. Captured from the JS algorithm:
// parseInt(sha256(input).hex.slice(0,8), 16).
//
// Range invariant: every result fits in uint32 (0..2^32-1). The
// table covers empty input, single-char, short string, the
// schema's NODE_TAG literal, and a multi-word phrase to lock
// behaviour across input shapes.
func TestEdgeRank_Parity(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 3820012610},
		{"hello", 754077114},
		{"a", 3398926610},
		{"abc", 3128432319},
		{"code_graph_node", 1629667449},
		{"the quick brown fox", 2664117846},
		// Long input (200 bytes) — exceeds the sha256 single-block
		// boundary (64 bytes). Proves the hash and the 8-hex prefix
		// slice both honour inputs that span multiple blocks.
		{strings.Repeat("a", 200), 3265857753},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := edgeRank(tc.in); got != tc.want {
				t.Errorf("edgeRank(%q): got %d, want %d", tc.in, got, tc.want)
			}
			// EdgeRank is the exported wrapper; lock it returns the
			// same value so a future refactor that splits the bodies
			// surfaces a regression here.
			if got := EdgeRank(tc.in); got != tc.want {
				t.Errorf("EdgeRank(%q): got %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestEdgeRank_Range confirms every output lands inside the
// 32-bit unsigned range — a regression that took 16 hex chars
// instead of 8 would surface here.
func TestEdgeRank_Range(t *testing.T) {
	inputs := []string{"", "x", "abcdef", "long string with many words and characters in it"}
	for _, in := range inputs {
		r := edgeRank(in)
		if r < 0 || r > 0xFFFFFFFF {
			t.Errorf("edgeRank(%q) = %d outside [0, 2^32-1]", in, r)
		}
	}
}

// TestChunkArray_Parity locks the JS chunker output against
// captured fixtures. The 250 batch-size constant doesn't appear
// here — it's the responsibility of the renderer call site.
func TestChunkArray_Parity(t *testing.T) {
	cases := []struct {
		name string
		in   []int
		size int
		want [][]int
	}{
		{"odd remainder", []int{1, 2, 3, 4, 5}, 2, [][]int{{1, 2}, {3, 4}, {5}}},
		{"exact multiples", []int{1, 2, 3, 4, 5, 6}, 3, [][]int{{1, 2, 3}, {4, 5, 6}}},
		{"empty input", []int{}, 250, [][]int{}},
		{"size larger than input", []int{1, 2, 3}, 5, [][]int{{1, 2, 3}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chunkArray(tc.in, tc.size)
			// nil vs empty distinction: JS returns [] (length 0)
			// for empty input; Go's make([][]int, 0, 0) is also a
			// non-nil zero-length slice. Treat the two as
			// equivalent via reflect.DeepEqual after normalising
			// nil → zero-length.
			if got == nil {
				got = [][]int{}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("chunkArray(%v, %d): got %v, want %v", tc.in, tc.size, got, tc.want)
			}
		})
	}
}

// TestChunkArray_NonPositiveSize confirms the Go-port-specific
// defensive guard against a non-positive chunkSize (which the JS
// source would treat as an infinite loop).
func TestChunkArray_NonPositiveSize(t *testing.T) {
	if got := chunkArray([]int{1, 2, 3}, 0); !reflect.DeepEqual(got, [][]int{{1, 2, 3}}) {
		t.Errorf("chunkArray with size=0: got %v", got)
	}
	if got := chunkArray([]int{}, 0); got != nil {
		t.Errorf("chunkArray with empty input + size=0: got %v, want nil", got)
	}
	if got := chunkArray([]int{1}, -1); !reflect.DeepEqual(got, [][]int{{1}}) {
		t.Errorf("chunkArray with size=-1: got %v", got)
	}
}
