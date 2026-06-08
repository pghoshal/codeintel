package secretrefs

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// decodeJSON is a test helper: JSON-unmarshal into `any` so the
// nested map/slice shape matches what a Postgres JSONB column scan
// would produce in production.
func decodeJSON(t *testing.T, s string) any {
	t.Helper()
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return out
}

// TestCollect_FlatObject locks the simplest case — a single
// {secretRef} record at the top level.
func TestCollect_FlatObject(t *testing.T) {
	got := Collect(decodeJSON(t, `{"secretRef":"GH_TOKEN"}`))
	want := []string{"GH_TOKEN"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestCollect_NestedRecursion confirms the walker descends into
// nested objects and surfaces every secretRef along the way.
func TestCollect_NestedRecursion(t *testing.T) {
	cfg := decodeJSON(t, `{
		"auth": {"token": {"secretRef": "GH"}},
		"branches": ["main", "dev"],
		"nested": {"deep": {"deeper": {"secretRef": "DEEP_KEY"}}}
	}`)
	got := Collect(cfg)
	sort.Strings(got)
	want := []string{"DEEP_KEY", "GH"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestCollect_ArrayDedup locks the contract that arrays dedup
// across items via a set, while object-sibling duplicates are
// preserved. The asymmetry is intentional.
func TestCollect_ArrayDedup(t *testing.T) {
	cfg := decodeJSON(t, `[
		{"secretRef":"X"},
		{"secretRef":"X"},
		{"secretRef":"Y"}
	]`)
	got := Collect(cfg)
	sort.Strings(got)
	want := []string{"X", "Y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestCollect_NonRefShapesIgnored confirms the walker only matches
// the exact shape `{secretRef: <string>}`. A key named
// `secretRef` carrying a non-string value (number, object) is
// ignored — only string-valued secretRefs are emitted.
func TestCollect_NonRefShapesIgnored(t *testing.T) {
	cases := []string{
		`{"secretRef":123}`,
		`{"secretRef":null}`,
		`{"secretRef":{"nested":"X"}}`,
		`{"secretRef":["arr"]}`,
		`{"otherKey":"X"}`,
		`"just a string"`,
		`42`,
		`null`,
	}
	for _, c := range cases {
		got := Collect(decodeJSON(t, c))
		if len(got) != 0 {
			t.Errorf("Collect(%s): got %v, want []", c, got)
		}
	}
}

// TestCollect_SiblingDuplicatesKept locks the documented quirk
// that sibling-of-object duplicates are NOT deduped (only array items
// are). Two keys in the same parent object both carrying the same
// secretRef value yield two entries.
func TestCollect_SiblingDuplicatesKept(t *testing.T) {
	cfg := decodeJSON(t, `{
		"a": {"secretRef":"DUP"},
		"b": {"secretRef":"DUP"}
	}`)
	got := Collect(cfg)
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 entries (object-sibling dedup is array-only)", got)
	}
	for _, v := range got {
		if v != "DUP" {
			t.Errorf("entry: got %q, want DUP", v)
		}
	}
}

// TestCollect_EmptyAndZero defends against panics on edge inputs.
func TestCollect_EmptyAndZero(t *testing.T) {
	for _, c := range []any{nil, map[string]any{}, []any{}} {
		if got := Collect(c); len(got) != 0 {
			t.Errorf("Collect(%v): got %v, want []", c, got)
		}
	}
}

// TestBindToOrg_AttachesOrgIdToEveryRef locks the canonical rewrite
// shape: every nested {secretRef:"X"} becomes
// {secretRef:"X", orgId:<n>}. Other fields are preserved.
func TestBindToOrg_AttachesOrgIdToEveryRef(t *testing.T) {
	in := decodeJSON(t, `{
		"auth": {"token": {"secretRef":"GH"}},
		"branches": ["main", "dev"],
		"nested": {"deep": {"secretRef":"DEEP"}}
	}`)
	out := BindToOrg(in, 42)
	want := decodeJSON(t, `{
		"auth": {"token": {"secretRef":"GH","orgId":42}},
		"branches": ["main", "dev"],
		"nested": {"deep": {"secretRef":"DEEP","orgId":42}}
	}`)
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("BindToOrg:\n  got  %#v\n  want %#v", out, want)
	}
}

// TestBindToOrg_DoesNotMutateInput is the immutability contract:
// the input tree must be untouched after BindToOrg returns.
func TestBindToOrg_DoesNotMutateInput(t *testing.T) {
	in := decodeJSON(t, `{"a":{"secretRef":"K"}}`)
	snapshotBytes, _ := json.Marshal(in)
	_ = BindToOrg(in, 7)
	afterBytes, _ := json.Marshal(in)
	if string(snapshotBytes) != string(afterBytes) {
		t.Fatalf("BindToOrg mutated input:\n  before %s\n  after  %s", string(snapshotBytes), string(afterBytes))
	}
}

// TestBindToOrg_DeeplyNestedDoesNotPanic asserts the depth-limit
// guard: a config exceeding MaxWalkDepth must NOT exhaust the Go
// goroutine stack — instead the walker stops descending at the
// cutoff and returns the original sub-tree there.
func TestBindToOrg_DeeplyNestedDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("BindToOrg panicked on deep tree: %v", r)
		}
	}()
	const depth = 200
	root := map[string]any{"v": "leaf"}
	cur := root
	for i := 0; i < depth; i++ {
		next := map[string]any{"v": "level"}
		cur["nested"] = next
		cur = next
	}
	if out := BindToOrg(root, 1); out == nil {
		t.Fatalf("BindToOrg returned nil for deep input")
	}
}

// TestRedact_DeeplyNestedDoesNotPanic locks the depth-limit guard
// on the Redact walker.
func TestRedact_DeeplyNestedDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Redact panicked on deep tree: %v", r)
		}
	}()
	root := map[string]any{}
	cur := root
	for i := 0; i < 200; i++ {
		next := map[string]any{}
		cur["nested"] = next
		cur = next
	}
	if got := Redact(root); got == nil {
		t.Fatalf("Redact returned nil for deep input")
	}
}

// TestBindToOrg_ScalarsPassThrough confirms non-record inputs are
// returned verbatim (no panic, no rewrap).
func TestBindToOrg_ScalarsPassThrough(t *testing.T) {
	for _, in := range []any{nil, "string", 42, 3.14, true} {
		if got := BindToOrg(in, 1); !reflect.DeepEqual(got, in) {
			t.Errorf("BindToOrg(%v) = %v, want %v", in, got, in)
		}
	}
}

// TestCollectUnique_DedupsAcrossSiblings extends the array-dedup
// quirk of Collect: CollectUnique applies dedup at the top level, so
// sibling-object duplicates also collapse to one entry.
func TestCollectUnique_DedupsAcrossSiblings(t *testing.T) {
	cfg := decodeJSON(t, `{
		"a": {"secretRef":"DUP"},
		"b": {"secretRef":"DUP"},
		"c": {"secretRef":"OTHER"}
	}`)
	got := CollectUnique(cfg)
	sort.Strings(got)
	want := []string{"DUP", "OTHER"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestRedact_PreservesKindIdentifiers locks the contract that the
// three known secret-kind shapes survive the walk verbatim with
// every sibling stripped.
func TestRedact_PreservesKindIdentifiers(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"secretRef", `{"secretRef":"K","orgId":7,"extra":"leak"}`, `{"secretRef":"K"}`},
		{"env", `{"env":"VAR","fallback":"x"}`, `{"env":"VAR"}`},
		{"googleCloudSecret", `{"googleCloudSecret":"projects/x/secrets/y","version":42}`, `{"googleCloudSecret":"projects/x/secrets/y"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(decodeJSON(t, c.in))
			gotBytes, _ := json.Marshal(got)
			wantBytes, _ := json.Marshal(decodeJSON(t, c.want))
			if string(gotBytes) != string(wantBytes) {
				t.Fatalf("Redact:\n  got  %s\n  want %s", string(gotBytes), string(wantBytes))
			}
		})
	}
}

// TestRedact_RecursesIntoNonSecretMaps covers deep traversal:
// non-secret containers are recursed into so nested secret records
// also get stripped of sibling leaks.
func TestRedact_RecursesIntoNonSecretMaps(t *testing.T) {
	in := decodeJSON(t, `{
		"connection": {
			"auth": {"token": {"secretRef":"X","leak":"v"}},
			"branches": ["main", "dev"]
		}
	}`)
	got := Redact(in)
	want := decodeJSON(t, `{
		"connection": {
			"auth": {"token": {"secretRef":"X"}},
			"branches": ["main", "dev"]
		}
	}`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Redact deep:\n  got  %#v\n  want %#v", got, want)
	}
}

// TestRedact_MasksScalarSecretKeys locks the enterprise safety
// rule: connection configs may contain legacy/raw credential
// fields. They must never be echoed back as scalar values.
func TestRedact_MasksScalarSecretKeys(t *testing.T) {
	in := decodeJSON(t, `{
		"type": "github",
		"token": "ghp_raw",
		"apiKey": "sk_raw",
		"client_secret": "oauth_raw",
		"private-key": "pem_raw",
		"auth": {
			"password": "pw_raw",
			"token": {"secretRef":"GH_TOKEN","orgId":7}
		},
		"branches": ["main"]
	}`)
	got := Redact(in)
	want := decodeJSON(t, `{
		"type": "github",
		"token": "[REDACTED]",
		"apiKey": "[REDACTED]",
		"client_secret": "[REDACTED]",
		"private-key": "[REDACTED]",
		"auth": {
			"password": "[REDACTED]",
			"token": {"secretRef":"GH_TOKEN"}
		},
		"branches": ["main"]
	}`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Redact scalar secrets:\n  got  %#v\n  want %#v", got, want)
	}
}

// TestRedact_DoesNotMutateInput locks the immutability contract
// (same as BindToOrg).
func TestRedact_DoesNotMutateInput(t *testing.T) {
	in := decodeJSON(t, `{"a":{"secretRef":"K","leak":"v"}}`)
	before, _ := json.Marshal(in)
	_ = Redact(in)
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatalf("Redact mutated input:\n  before %s\n  after  %s", string(before), string(after))
	}
}

// TestRedact_ScalarsAndArraysPassThrough confirms scalar values
// and arrays of scalars survive unchanged.
func TestRedact_ScalarsAndArraysPassThrough(t *testing.T) {
	for _, in := range []any{nil, "hi", 42, 3.14, true, false} {
		if got := Redact(in); !reflect.DeepEqual(got, in) {
			t.Errorf("Redact(%v) = %v, want %v", in, got, in)
		}
	}
	arr := decodeJSON(t, `[1, "two", null, {"k": 3}]`)
	if got := Redact(arr); !reflect.DeepEqual(got, arr) {
		t.Errorf("Redact array passthrough: got %v, want %v", got, arr)
	}
}

// TestContains is a sanity check for the convenience wrapper used by
// the DELETE handler.
func TestContains(t *testing.T) {
	cfg := decodeJSON(t, `{"a":{"secretRef":"K1"},"b":{"secretRef":"K2"}}`)
	if !Contains(cfg, "K1") {
		t.Error("Contains: missed K1")
	}
	if Contains(cfg, "K3") {
		t.Error("Contains: false positive on K3")
	}
}
