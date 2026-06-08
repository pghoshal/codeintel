package repoindexmanager

import (
	"strings"
	"testing"
	"time"

	"codeintel/internal/backend/indexplanner"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestBuildManifestFilesForCommitClassifiesFiles(t *testing.T) {
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	writeFile(t, root, "package.json", "{}\n")
	writeFile(t, root, "src/index.ts", "export const value = 1\n")
	writeFile(t, root, "src/index.test.ts", "test('x', () => {})\n")
	writeFile(t, root, "node_modules/pkg/index.js", "module.exports = {}\n")
	writeFile(t, root, "dist/app.min.js", "minified\n")
	for _, name := range []string{"package.json", "src/index.ts", "src/index.test.ts", "node_modules/pkg/index.js", "dist/app.min.js"} {
		if _, err := tree.Add(name); err != nil {
			t.Fatalf("add %s: %v", name, err)
		}
	}
	hash, err := tree.Commit("manifest", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.local", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	files, err := buildManifestFilesForCommit(root, hash.String())
	if err != nil {
		t.Fatalf("buildManifestFilesForCommit: %v", err)
	}
	byPath := map[string]manifestFileRow{}
	for _, file := range files {
		byPath[file.Path] = file
	}
	if len(byPath) != 5 {
		t.Fatalf("file count = %d want 5: %#v", len(byPath), byPath)
	}
	ts := byPath["src/index.ts"]
	if ts.Language == nil || *ts.Language != "typescript" {
		t.Fatalf("typescript language = %#v", ts.Language)
	}
	if ts.ProjectRoot == nil || *ts.ProjectRoot != "." {
		t.Fatalf("project root = %#v want .", ts.ProjectRoot)
	}
	if byPath["src/index.test.ts"].Test != true {
		t.Fatalf("test file not classified")
	}
	if byPath["node_modules/pkg/index.js"].Vendor != true {
		t.Fatalf("vendor file not classified")
	}
	if byPath["dist/app.min.js"].Generated != true {
		t.Fatalf("generated file not classified")
	}
	if byPath["src/index.ts"].ContentHash == "" {
		t.Fatalf("content hash must be git blob hash")
	}
}

func TestBuildManifestDeltaPlanNarrowsSCIPAndGraph(t *testing.T) {
	root := "."
	previous := []manifestFileRow{
		{Path: "go.mod", ContentHash: "a", ProjectRoot: &root},
		{Path: "cmd/main.go", ContentHash: "old", ProjectRoot: &root},
		{Path: "README.md", ContentHash: "same", ProjectRoot: &root},
		{Path: "deleted.go", ContentHash: "gone", ProjectRoot: &root},
	}
	current := []manifestFileRow{
		{Path: "go.mod", ContentHash: "a", ProjectRoot: &root},
		{Path: "cmd/main.go", ContentHash: "new", ProjectRoot: &root},
		{Path: "README.md", ContentHash: "same", ProjectRoot: &root},
		{Path: "added.go", ContentHash: "add", ProjectRoot: &root},
	}
	plan := buildManifestDeltaPlan(previous, current, false)
	if plan.Mode != "DELTA" {
		t.Fatalf("mode = %s want DELTA", plan.Mode)
	}
	if plan.Zoekt.Strategy != zoektStrategyFullRepoRewrite {
		t.Fatalf("zoekt strategy = %s want %s", plan.Zoekt.Strategy, zoektStrategyFullRepoRewrite)
	}
	if plan.Zoekt.Reason == nil || !strings.Contains(*plan.Zoekt.Reason, "rewrites repository shard") {
		t.Fatalf("zoekt rewrite reason = %#v", plan.Zoekt.Reason)
	}
	if plan.SCIP.Strategy != "PROJECT_ROOTS" || len(plan.SCIP.ProjectRoots) != 1 || plan.SCIP.ProjectRoots[0] != "." {
		t.Fatalf("scip plan = %#v", plan.SCIP)
	}
	if plan.Graph.Strategy != "DELTA_FILES" || len(plan.Graph.DeletedFiles) != 1 || plan.Graph.DeletedFiles[0] != "deleted.go" {
		t.Fatalf("graph plan = %#v", plan.Graph)
	}
	if len(plan.AddedFiles) != 1 || plan.AddedFiles[0] != "added.go" || len(plan.ChangedFiles) != 1 || plan.ChangedFiles[0] != "cmd/main.go" {
		t.Fatalf("touched files wrong: added=%v changed=%v", plan.AddedFiles, plan.ChangedFiles)
	}
}

func TestBuildManifestDeltaPlanUsesZoektFileDeltaWhenSupported(t *testing.T) {
	root := "."
	previous := []manifestFileRow{
		{Path: "cmd/main.go", ContentHash: "old", ProjectRoot: &root},
		{Path: "deleted.go", ContentHash: "gone", ProjectRoot: &root},
	}
	current := []manifestFileRow{
		{Path: "cmd/main.go", ContentHash: "new", ProjectRoot: &root},
		{Path: "added.go", ContentHash: "add", ProjectRoot: &root},
	}
	plan := buildManifestDeltaPlan(previous, current, true)
	if plan.Zoekt.Strategy != zoektStrategyDeltaFiles {
		t.Fatalf("zoekt strategy = %s want %s", plan.Zoekt.Strategy, zoektStrategyDeltaFiles)
	}
	if plan.Zoekt.Reason != nil {
		t.Fatalf("delta-capable zoekt plan should not carry rewrite reason: %#v", plan.Zoekt.Reason)
	}
	if len(plan.Zoekt.Files) != 2 || plan.Zoekt.Files[0] != "added.go" || plan.Zoekt.Files[1] != "cmd/main.go" {
		t.Fatalf("zoekt delta files = %#v", plan.Zoekt.Files)
	}
	if len(plan.DeletedFiles) != 1 || plan.DeletedFiles[0] != "deleted.go" {
		t.Fatalf("deleted files = %#v", plan.DeletedFiles)
	}
}

func TestSupportsZoektFileDeltaDefaultsOnWithExplicitOptOut(t *testing.T) {
	t.Setenv("CODEINTEL_ZOEKT_FILE_DELTA", "")
	if !supportsZoektFileDelta() {
		t.Fatalf("unset CODEINTEL_ZOEKT_FILE_DELTA should enable native Zoekt delta")
	}
	t.Setenv("CODEINTEL_ZOEKT_FILE_DELTA", "false")
	if supportsZoektFileDelta() {
		t.Fatalf("CODEINTEL_ZOEKT_FILE_DELTA=false should disable native Zoekt delta")
	}
	t.Setenv("CODEINTEL_ZOEKT_FILE_DELTA", "true")
	if !supportsZoektFileDelta() {
		t.Fatalf("CODEINTEL_ZOEKT_FILE_DELTA=true should enable native Zoekt delta")
	}
}

func TestForceSemanticRepairPlanRepairsUnchangedSemanticLayersOnly(t *testing.T) {
	root := "."
	previous := []manifestFileRow{
		{Path: "go.mod", ContentHash: "same", ProjectRoot: &root},
		{Path: "cmd/main.go", ContentHash: "same", ProjectRoot: &root},
	}
	current := []manifestFileRow{
		{Path: "go.mod", ContentHash: "same", ProjectRoot: &root},
		{Path: "cmd/main.go", ContentHash: "same", ProjectRoot: &root},
	}
	plan := buildManifestDeltaPlan(previous, current, false)
	if plan.Mode != "NOOP" {
		t.Fatalf("mode = %s want NOOP", plan.Mode)
	}
	repair := forceSemanticRepairPlan(plan, current, "hollow semantic index")
	if repair.Mode != "SEMANTIC_REPAIR" {
		t.Fatalf("mode = %s want SEMANTIC_REPAIR", repair.Mode)
	}
	if repair.Zoekt.Strategy != "NOOP" {
		t.Fatalf("zoekt strategy = %s want NOOP", repair.Zoekt.Strategy)
	}
	if repair.SCIP.Strategy != "FULL_REPO" || len(repair.SCIP.Files) != 2 || repair.SCIP.ProjectRoots[0] != "." {
		t.Fatalf("scip repair plan = %#v", repair.SCIP)
	}
	if repair.Graph.Strategy != "DELTA_FILES" || len(repair.Graph.Files) != 2 {
		t.Fatalf("graph repair plan = %#v", repair.Graph)
	}
	if repair.SCIP.Reason == nil || *repair.SCIP.Reason != "hollow semantic index" {
		t.Fatalf("repair reason = %#v", repair.SCIP.Reason)
	}
}

func TestManifestHasSemanticCandidatesUsesLanguageAndExtension(t *testing.T) {
	goLang := "go"
	if !manifestHasSemanticCandidates([]manifestFileRow{{Path: "README.md"}, {Path: "cmd/main", Language: &goLang}}) {
		t.Fatal("expected explicit go language to require semantic health")
	}
	if !manifestHasSemanticCandidates([]manifestFileRow{{Path: "src/main.rs"}}) {
		t.Fatal("expected rust extension to require semantic health")
	}
	if manifestHasSemanticCandidates([]manifestFileRow{{Path: "README.md"}, {Path: "docs/guide.md"}}) {
		t.Fatal("markdown-only manifest should not require semantic health")
	}
}

func TestFilterSCIPProjectsForDeltaKeepsTouchedRoots(t *testing.T) {
	plan := &deltaReindexPlan{SCIP: deltaSCIPPlan{Strategy: "PROJECT_ROOTS", ProjectRoots: []string{"services/api"}}}
	projects := []indexplanner.SCIPProject{
		{Language: "go", ProjectRoot: "services/api", Indexer: "scip-go", SCIPWorkerClass: "go"},
		{Language: "typescript", ProjectRoot: "web", Indexer: "scip-typescript", SCIPWorkerClass: "ts-js"},
	}
	got := filterSCIPProjectsForDelta(projects, plan)
	if len(got) != 1 || got[0].ProjectRoot != "services/api" {
		t.Fatalf("filtered projects = %#v", got)
	}
}
