package repoindexstatus

import (
	"encoding/json"
	"testing"
	"time"
)

// ---- FormatScipCodeIntelIndex ----

func TestFormatScip_NilReturnsNil(t *testing.T) {
	if got := FormatScipCodeIntelIndex(nil, FormatScipCodeIntelOptions{}); got != nil {
		t.Errorf("got %+v want nil", got)
	}
}

func TestFormatScip_HappyPath_FullProjection(t *testing.T) {
	indexedAt := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	in := &ScipCodeIntelIndexInput{
		ID:                "scip-1",
		Kind:              "SCIP",
		Status:            "READY",
		Revision:          "main",
		CommitHash:        "abc123",
		LanguageCount:     3,
		SymbolCount:       100,
		OccurrenceCount:   500,
		RelationshipCount: 200,
		IndexedAt:         &indexedAt,
		LanguageIndexes: []CodeIntelLanguageIndexInput{
			{Language: "go", ProjectRoot: "/", Indexer: "scip-go", Status: "READY"},
			{Language: "ts", ProjectRoot: "/", Indexer: "scip-typescript", Status: "READY"},
			{Language: "py", ProjectRoot: "/", Indexer: "scip-python", Status: "FAILED"},
			{Language: "rb", ProjectRoot: "/", Indexer: "scip-ruby", Status: "SKIPPED"},
		},
	}
	out := FormatScipCodeIntelIndex(in, FormatScipCodeIntelOptions{})
	if out == nil {
		t.Fatalf("got nil")
	}
	if out.ProjectCount != 4 {
		t.Errorf("ProjectCount: got %d want 4", out.ProjectCount)
	}
	want := []string{"go", "py", "rb", "ts"}
	if !stringSlicesEqual(out.DetectedLanguages, want) {
		t.Errorf("DetectedLanguages: got %v want %v", out.DetectedLanguages, want)
	}
	if !stringSlicesEqual(out.ReadyLanguages, []string{"go", "ts"}) {
		t.Errorf("ReadyLanguages: got %v want [go ts]", out.ReadyLanguages)
	}
	if !stringSlicesEqual(out.SkippedLanguages, []string{"rb"}) {
		t.Errorf("SkippedLanguages: got %v want [rb]", out.SkippedLanguages)
	}
	if !stringSlicesEqual(out.FailedLanguages, []string{"py"}) {
		t.Errorf("FailedLanguages: got %v want [py]", out.FailedLanguages)
	}
}

func TestFormatScip_LanguageRowOrdering(t *testing.T) {
	// Sort key: (projectRoot, language, indexer). Mixed input ->
	// deterministic output.
	in := &ScipCodeIntelIndexInput{
		LanguageIndexes: []CodeIntelLanguageIndexInput{
			{Language: "ts", ProjectRoot: "/svc/a", Indexer: "x"},
			{Language: "go", ProjectRoot: "/svc/a", Indexer: "y"},
			{Language: "go", ProjectRoot: "/svc/a", Indexer: "x"},
			{Language: "py", ProjectRoot: "/", Indexer: "z"},
		},
	}
	out := FormatScipCodeIntelIndex(in, FormatScipCodeIntelOptions{})
	want := []struct {
		root, lang, idx string
	}{
		{"/", "py", "z"},
		{"/svc/a", "go", "x"},
		{"/svc/a", "go", "y"},
		{"/svc/a", "ts", "x"},
	}
	if len(out.LanguageIndexes) != len(want) {
		t.Fatalf("len: %d", len(out.LanguageIndexes))
	}
	for i, w := range want {
		got := out.LanguageIndexes[i]
		if got.ProjectRoot != w.root || got.Language != w.lang || got.Indexer != w.idx {
			t.Errorf("[%d]: got (%s,%s,%s) want (%s,%s,%s)", i,
				got.ProjectRoot, got.Language, got.Indexer, w.root, w.lang, w.idx)
		}
	}
}

func TestFormatScip_IncludeArtifactPaths_TogglesWireFields(t *testing.T) {
	toolPath := "/opt/scip-go"
	artPath := "s3://bucket/shard"
	in := &ScipCodeIntelIndexInput{
		LanguageIndexes: []CodeIntelLanguageIndexInput{
			{
				Language: "go", ProjectRoot: "/", Indexer: "scip-go",
				Status: "READY", ToolchainPath: &toolPath, ArtifactPath: &artPath,
			},
		},
	}

	t.Run("includeArtifactPaths=false: keys absent on the wire", func(t *testing.T) {
		out := FormatScipCodeIntelIndex(in, FormatScipCodeIntelOptions{})
		row := out.LanguageIndexes[0]
		if row.ToolchainPath != nil {
			t.Errorf("ToolchainPath: got %v want nil", *row.ToolchainPath)
		}
		if row.ArtifactPath != nil {
			t.Errorf("ArtifactPath: got %v want nil", *row.ArtifactPath)
		}
		// And critically: the JSON tags use omitempty so the
		// keys are dropped entirely from the wire.
		raw, _ := json.Marshal(row)
		body := string(raw)
		if contains2(body, "toolchainPath") {
			t.Errorf("body still has toolchainPath: %s", body)
		}
		if contains2(body, "artifactPath") {
			t.Errorf("body still has artifactPath: %s", body)
		}
	})

	t.Run("includeArtifactPaths=true: keys present", func(t *testing.T) {
		out := FormatScipCodeIntelIndex(in, FormatScipCodeIntelOptions{IncludeArtifactPaths: true})
		row := out.LanguageIndexes[0]
		if row.ToolchainPath == nil || *row.ToolchainPath != "/opt/scip-go" {
			t.Errorf("ToolchainPath: %v", row.ToolchainPath)
		}
		if row.ArtifactPath == nil || *row.ArtifactPath != "s3://bucket/shard" {
			t.Errorf("ArtifactPath: %v", row.ArtifactPath)
		}
		raw, _ := json.Marshal(row)
		body := string(raw)
		if !contains2(body, "toolchainPath") || !contains2(body, "artifactPath") {
			t.Errorf("body missing fields: %s", body)
		}
	})
}

func TestFormatScip_DropsEmptyLanguages(t *testing.T) {
	in := &ScipCodeIntelIndexInput{
		LanguageIndexes: []CodeIntelLanguageIndexInput{
			{Language: "", ProjectRoot: "/", Indexer: "x", Status: "READY"},
			{Language: "go", ProjectRoot: "/", Indexer: "y", Status: "READY"},
		},
	}
	out := FormatScipCodeIntelIndex(in, FormatScipCodeIntelOptions{})
	// detectedLanguages uses uniqueSorted which drops empty
	// strings. So detected should be ["go"].
	if !stringSlicesEqual(out.DetectedLanguages, []string{"go"}) {
		t.Errorf("DetectedLanguages: got %v want [go]", out.DetectedLanguages)
	}
}

// ---- FormatCodeGraphIndex ----

func TestFormatCodeGraph_NilReturnsNil(t *testing.T) {
	if got := FormatCodeGraphIndex(nil); got != nil {
		t.Errorf("got %+v want nil", got)
	}
}

func TestFormatCodeGraph_HappyPath(t *testing.T) {
	activatedAt := time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC)
	in := &CodeGraphIndexInput{
		ID:              "cg-1",
		Provider:        "NEBULA",
		Status:          "READY",
		CommitHash:      "abc",
		WorkspaceID:     "ws",
		SchemaVersion:   1,
		BuilderVersion:  "v1",
		VertexCount:     1000,
		EdgeCount:       2000,
		AnchorCount:     500,
		LinkedEdgeCount: 750,
		Revisions: []CodeGraphRevisionInput{
			{Revision: "refs/heads/main", CommitHash: "abc", ActivatedAt: &activatedAt},
		},
	}
	out := FormatCodeGraphIndex(in)
	if out == nil {
		t.Fatalf("got nil")
	}
	if !out.IsActive {
		t.Errorf("IsActive: got false (revisions non-empty -> want true)")
	}
	if out.ActiveRevisionCount != 1 {
		t.Errorf("ActiveRevisionCount: %d want 1", out.ActiveRevisionCount)
	}
	if len(out.ActiveRevisions) != 1 || out.ActiveRevisions[0].Revision != "refs/heads/main" {
		t.Errorf("ActiveRevisions: %+v", out.ActiveRevisions)
	}
}

func TestFormatCodeGraph_RevisionSortStable(t *testing.T) {
	in := &CodeGraphIndexInput{
		Status: "READY",
		Revisions: []CodeGraphRevisionInput{
			{Revision: "refs/heads/zeta"},
			{Revision: "refs/heads/alpha"},
			{Revision: "refs/heads/beta"},
		},
	}
	out := FormatCodeGraphIndex(in)
	want := []string{"refs/heads/alpha", "refs/heads/beta", "refs/heads/zeta"}
	got := make([]string, len(out.ActiveRevisions))
	for i, r := range out.ActiveRevisions {
		got[i] = r.Revision
	}
	if !stringSlicesEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}

func TestFormatCodeGraph_SemanticCountFallbacks(t *testing.T) {
	// Three semantic-count axes:
	//   direct value -> used.
	//   direct nil + _count present -> _count.
	//   both nil -> 0.
	explicit := int32(7)
	aggFacts := int32(11)
	in := &CodeGraphIndexInput{
		Status:                    "READY",
		CommitHash:                "x",
		WorkspaceID:               "ws",
		SemanticFactCount:         &explicit, // direct
		Counts:                    &CodeGraphCounts{SemanticFacts: &aggFacts, SemanticEdges: nil, SemanticHyperedges: nil},
		// SemanticEdgeCount nil, Counts.SemanticEdges nil -> 0
		// SemanticHyperedgeCount nil, Counts.SemanticHyperedges nil -> 0
		// SemanticRejectedFactCount nil -> 0
		// SemanticBatchCount nil -> 0
	}
	out := FormatCodeGraphIndex(in)
	if out.SemanticFactCount != 7 {
		t.Errorf("SemanticFactCount: got %d want 7 (direct wins)", out.SemanticFactCount)
	}
	if out.SemanticEdgeCount != 0 {
		t.Errorf("SemanticEdgeCount: got %d want 0", out.SemanticEdgeCount)
	}
	if out.SemanticHyperedgeCount != 0 {
		t.Errorf("SemanticHyperedgeCount: got %d", out.SemanticHyperedgeCount)
	}
}

func TestFormatCodeGraph_CountAggFallback(t *testing.T) {
	aggFacts := int32(11)
	in := &CodeGraphIndexInput{
		Status:      "READY",
		CommitHash:  "x",
		WorkspaceID: "ws",
		Counts:      &CodeGraphCounts{SemanticFacts: &aggFacts},
	}
	out := FormatCodeGraphIndex(in)
	if out.SemanticFactCount != 11 {
		t.Errorf("SemanticFactCount: got %d want 11 (agg fallback)", out.SemanticFactCount)
	}
}

func TestFormatCodeGraph_NoRevisions_IsActiveFalse(t *testing.T) {
	in := &CodeGraphIndexInput{Status: "READY", CommitHash: "x", WorkspaceID: "ws"}
	out := FormatCodeGraphIndex(in)
	if out.IsActive {
		t.Errorf("IsActive: got true with no revisions")
	}
	if out.ActiveRevisionCount != 0 {
		t.Errorf("ActiveRevisionCount: %d", out.ActiveRevisionCount)
	}
}

// ---- compareCodeGraphIndexForCurrent + selection ----

func TestSelectCurrentCodeGraphIndex_PreferReadyWithRevisions(t *testing.T) {
	// Two READY indexes; only one has revisions. The one with
	// revisions should win.
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idxA := CodeGraphIndexInput{ID: "a", Status: "READY", IndexedAt: &ts}
	idxB := CodeGraphIndexInput{ID: "b", Status: "READY", IndexedAt: &ts,
		Revisions: []CodeGraphRevisionInput{{Revision: "refs/heads/main"}}}
	got := SelectCurrentCodeGraphIndex([]CodeGraphIndexInput{idxA, idxB})
	if got == nil || got.ID != "b" {
		t.Errorf("got %v want b", got)
	}
}

func TestSelectCurrentCodeGraphIndex_PreferBetterStatus(t *testing.T) {
	idxFailed := CodeGraphIndexInput{ID: "failed", Status: "FAILED"}
	idxReady := CodeGraphIndexInput{ID: "ready", Status: "READY"}
	idxBuilding := CodeGraphIndexInput{ID: "building", Status: "BUILDING"}
	got := SelectCurrentCodeGraphIndex([]CodeGraphIndexInput{idxFailed, idxBuilding, idxReady})
	if got == nil || got.ID != "ready" {
		t.Errorf("got %v want ready", got)
	}
}

func TestSelectCurrentCodeGraphIndex_PreferNonSuperseded(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	older := CodeGraphIndexInput{ID: "older", Status: "READY"} // SupersededAt nil
	newer := CodeGraphIndexInput{ID: "newer", Status: "READY", SupersededAt: &ts}
	// Both READY without revisions -> not "current snapshots". Same
	// statusRank. SupersededAt-nil should win.
	got := SelectCurrentCodeGraphIndex([]CodeGraphIndexInput{newer, older})
	if got == nil || got.ID != "older" {
		t.Errorf("got %v want older (non-superseded preferred)", got)
	}
}

func TestSelectCurrentCodeGraphIndex_FallsBackToIndexedAtDESC(t *testing.T) {
	older := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	idxOld := CodeGraphIndexInput{ID: "old", Status: "READY", IndexedAt: &older}
	idxNew := CodeGraphIndexInput{ID: "new", Status: "READY", IndexedAt: &newer}
	got := SelectCurrentCodeGraphIndex([]CodeGraphIndexInput{idxOld, idxNew})
	if got == nil || got.ID != "new" {
		t.Errorf("got %v want new (DESC indexedAt)", got)
	}
}

func TestSelectCurrentCodeGraphIndex_EmptySlice(t *testing.T) {
	if got := SelectCurrentCodeGraphIndex(nil); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

func TestSortCodeGraphIndexesForStatus_DoesNotMutateInput(t *testing.T) {
	in := []CodeGraphIndexInput{
		{ID: "a", Status: "FAILED"},
		{ID: "b", Status: "READY"},
	}
	original := []CodeGraphIndexInput{in[0], in[1]}
	got := SortCodeGraphIndexesForStatus(in)
	if got[0].ID != "b" || got[1].ID != "a" {
		t.Errorf("sort order wrong: %+v", got)
	}
	if in[0].ID != original[0].ID || in[1].ID != original[1].ID {
		t.Errorf("input mutated: %+v", in)
	}
}

// ---- helpers ----

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains2(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
