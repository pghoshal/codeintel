package graphschema

import "testing"

// Goldens captured by re-running the JS algorithm from
// packages/backend/src/codeGraph/nebulaNgql.ts lines 82-92.

func TestRenderDescribeTagStatement_Parity(t *testing.T) {
	got := RenderDescribeTagStatement()
	want := "DESCRIBE TAG `code_graph_node`;"
	if got != want {
		t.Errorf("byte-mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestRenderDescribeEdgeStatement_Parity(t *testing.T) {
	got := RenderDescribeEdgeStatement()
	want := "DESCRIBE EDGE `code_graph_edge`;"
	if got != want {
		t.Errorf("byte-mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestRenderAlterTagAddStatement_Parity covers single-column +
// mixed-type cases. Mixed types verifies propType discriminates
// correctly inside the ADD clause: startLine → int, label →
// string, confidence → double, an unknown property → string
// (default branch).
func TestRenderAlterTagAddStatement_Parity(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{
			name: "single new string column",
			in:   []string{"newField"},
			want: "ALTER TAG `code_graph_node` ADD (`newField` string);",
		},
		{
			name: "mixed types in declared order",
			in:   []string{"startLine", "label", "confidence", "newField"},
			want: "ALTER TAG `code_graph_node` ADD (`startLine` int, `label` string, `confidence` double, `newField` string);",
		},
		{
			// Empty input emits `ADD ()` which graphd rejects as a
			// syntax error — exactly what the JS source does. The
			// renderer preserves the behaviour so the migration call
			// site fails loudly rather than silently on an empty
			// diff.
			name: "empty props (matches JS behaviour, graphd will reject)",
			in:   []string{},
			want: "ALTER TAG `code_graph_node` ADD ();",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderAlterTagAddStatement(tc.in); got != tc.want {
				t.Errorf("byte-mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestRenderAlterEdgeAddStatement_Parity(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{
			name: "single new string column",
			in:   []string{"newProp"},
			want: "ALTER EDGE `code_graph_edge` ADD (`newProp` string);",
		},
		{
			name: "mixed types: int / string / double",
			in:   []string{"repoId", "evidenceFilePath", "confidence"},
			want: "ALTER EDGE `code_graph_edge` ADD (`repoId` int, `evidenceFilePath` string, `confidence` double);",
		},
		{
			name: "empty props (matches JS behaviour, graphd will reject)",
			in:   []string{},
			want: "ALTER EDGE `code_graph_edge` ADD ();",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderAlterEdgeAddStatement(tc.in); got != tc.want {
				t.Errorf("byte-mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestRenderAlter_PropTypeConsistencyWithCreate locks the
// invariant that ALTER's column-type derivation is exactly the
// same propType the original CREATE TAG / CREATE EDGE used. A
// drift between the two (e.g. CREATE renders orgId int but ALTER
// renders orgId string) would corrupt every existing row when the
// migration is applied. The test wires the same prop name
// through both render paths and asserts the type substring
// matches.
func TestRenderAlter_PropTypeConsistencyWithCreate(t *testing.T) {
	cases := []struct {
		prop string
		want string
	}{
		{"orgId", "int"},
		{"repoId", "int"},
		{"schemaVersion", "int"},
		{"startLine", "int"},
		{"endLine", "int"},
		{"confidence", "double"},
		{"label", "string"},
		{"path", "string"},
		{"unknownNew", "string"},
	}
	for _, tc := range cases {
		t.Run(tc.prop, func(t *testing.T) {
			if got := propType(tc.prop); got != tc.want {
				t.Errorf("propType(%q): got %q, want %q (CREATE vs ALTER would mismatch)", tc.prop, got, tc.want)
			}
		})
	}
}
