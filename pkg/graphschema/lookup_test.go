package graphschema

import (
	"strings"
	"testing"
)

// Golden outputs captured by re-running the JS algorithm from
// packages/backend/src/codeGraph/nebulaNgql.ts (lines 109-126)
// against the fixtures below. Re-capture only when the underlying
// TS source actually changes.
const (
	goldenLookupSnapshot = "LOOKUP ON `code_graph_node` WHERE `code_graph_node`.`orgId` == 7 AND `code_graph_node`.`workspaceId` == \"ws-1\" AND `code_graph_node`.`repoId` == 42 AND `code_graph_node`.`commitHash` == \"deadbeef\" AND `code_graph_node`.`schemaVersion` == 1 AND `code_graph_node`.`builderVersion` == \"b-1\" YIELD id(vertex) AS vid;"

	goldenDeleteOneVid    = "DELETE VERTEX \"v-1\" WITH EDGE;"
	goldenDeleteThreeVids = "DELETE VERTEX \"v-1\", \"v-2\", \"v-3\" WITH EDGE;"
)

// TestRenderLookupSnapshotVerticesStatement_Parity locks the
// byte-for-byte output of the LOOKUP renderer against the JS
// golden. Every quote, every space, every comparison operator
// must match.
func TestRenderLookupSnapshotVerticesStatement_Parity(t *testing.T) {
	got := RenderLookupSnapshotVerticesStatement(CodeGraphDeleteInput{
		OrgID:          7,
		WorkspaceID:    "ws-1",
		RepoID:         42,
		CommitHash:     "deadbeef",
		SchemaVersion:  1,
		BuilderVersion: "b-1",
	})
	if got != goldenLookupSnapshot {
		t.Errorf("byte-mismatch:\n got: %s\nwant: %s", got, goldenLookupSnapshot)
	}
}

// TestRenderLookupSnapshotVerticesStatement_InjectionSafety locks
// the defence-in-depth on string-typed input fields. A hostile
// WorkspaceID / CommitHash / BuilderVersion containing quote or
// backslash characters must produce a JSON-escaped value that
// keeps the surrounding statement well-formed.
func TestRenderLookupSnapshotVerticesStatement_InjectionSafety(t *testing.T) {
	got := RenderLookupSnapshotVerticesStatement(CodeGraphDeleteInput{
		OrgID:          1,
		WorkspaceID:    `evil"; DROP SPACE codeintel; --`,
		RepoID:         1,
		CommitHash:     "ok",
		SchemaVersion:  1,
		BuilderVersion: "ok",
	})
	// The hostile workspaceId must be JSON-escaped inside the
	// quoted literal — the embedded `"` becomes `\"`, the
	// surrounding double quotes still close the literal cleanly,
	// and the rest of the statement parses normally.
	if !strings.Contains(got, `\"; DROP SPACE codeintel; --`) {
		t.Errorf("hostile workspaceId not JSON-escaped: %s", got)
	}
	// Confirm the YIELD clause still terminates the statement —
	// the literal didn't escape its scope.
	if !strings.HasSuffix(got, " YIELD id(vertex) AS vid;") {
		t.Errorf("hostile input broke statement structure: %s", got)
	}
}

// TestRenderDeleteVerticesStatements_Parity covers the four
// chunk-count cases: 0 (empty input → empty output), 1, 3
// (multiple-in-same-chunk), and the batch boundary at 250
// (251 vids → 2 statements).
func TestRenderDeleteVerticesStatements_Parity(t *testing.T) {
	if got := RenderDeleteVerticesStatements([]string{}); len(got) != 0 {
		t.Errorf("empty input: got %d statements, want 0", len(got))
	}

	one := RenderDeleteVerticesStatements([]string{"v-1"})
	if len(one) != 1 || one[0] != goldenDeleteOneVid {
		t.Errorf("single vid: got %v, want [%q]", one, goldenDeleteOneVid)
	}

	three := RenderDeleteVerticesStatements([]string{"v-1", "v-2", "v-3"})
	if len(three) != 1 || three[0] != goldenDeleteThreeVids {
		t.Errorf("three vids: got %v, want [%q]", three, goldenDeleteThreeVids)
	}
}

// TestRenderDeleteVerticesStatements_BatchBoundary confirms a
// 251-vid input produces 2 statements (250 + 1) AND that the
// first chunk has exactly 250 vid-comma-separated entries in
// order. A regression that reordered or dropped vids inside the
// large chunk would still pass a count-only assertion; the
// comma count + endpoint vids pin the chunk contents.
func TestRenderDeleteVerticesStatements_BatchBoundary(t *testing.T) {
	vids := make([]string, 251)
	for i := range vids {
		vids[i] = "v-" + iToStr(i)
	}
	got := RenderDeleteVerticesStatements(vids)
	if len(got) != 2 {
		t.Fatalf("251 vids: got %d statements, want 2", len(got))
	}
	// First chunk: 250 entries → 249 commas separating them.
	if c := strings.Count(got[0], ","); c != 249 {
		t.Errorf("first chunk comma count: got %d, want 249", c)
	}
	// Endpoint vids appear in order: chunk 0 must start with v-0
	// and end with v-249; chunk 1 must contain only v-250.
	if !strings.HasPrefix(got[0], `DELETE VERTEX "v-0", "v-1",`) {
		t.Errorf("first chunk does not start with v-0, v-1: %q", got[0][:60])
	}
	if !strings.Contains(got[0], `"v-249" WITH EDGE;`) {
		t.Errorf("first chunk does not end with v-249: %q", got[0][len(got[0])-40:])
	}
	if got[1] != `DELETE VERTEX "v-250" WITH EDGE;` {
		t.Errorf("second-chunk content: got %q", got[1])
	}
}

// TestRenderDeleteVerticesStatements_InjectionSafety locks the
// quote-escape path for hostile vid values. A vid containing
// `"` must JSON-escape inside the surrounding literal.
func TestRenderDeleteVerticesStatements_InjectionSafety(t *testing.T) {
	got := RenderDeleteVerticesStatements([]string{`v"); DROP TAG bad;`})
	if len(got) != 1 {
		t.Fatalf("got %d statements", len(got))
	}
	// The vid value renders inside JSON-quoted literal — the
	// embedded `"` becomes `\"`, keeping the statement structure
	// intact.
	if !strings.Contains(got[0], `\")`) {
		t.Errorf("hostile vid not escaped: %s", got[0])
	}
	if !strings.HasSuffix(got[0], " WITH EDGE;") {
		t.Errorf("hostile vid broke statement structure: %s", got[0])
	}
}

// TestPropRef locks the internal helper used by the LOOKUP
// builder. Direct equivalent of the TS propRef: produces
// `<tag>`.`<prop>` with both identifiers backtick-quoted.
func TestPropRef(t *testing.T) {
	if got := propRef("orgId"); got != "`code_graph_node`.`orgId`" {
		t.Errorf("propRef(orgId): got %q", got)
	}
	// Backtick-embedded prop name still escapes correctly.
	if got := propRef("weird`name"); got != "`code_graph_node`.`weird``name`" {
		t.Errorf("propRef(weird`name): got %q", got)
	}
}
