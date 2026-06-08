package scipprojectdetect

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"codeintel/internal/backend/indexplanner"
)

func TestDetectSnapshotFindsPolyglotProjectsAndSkipsIgnored(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"package.json",
		"src/index.ts",
		"services/api/go.mod",
		"python/requirements.txt",
		"java/pom.xml",
		"java/src/main/java/App.java",
		"native/compile_commands.json",
		"dotnet/app.csproj",
		"ruby/foo.gemspec",
		"ruby/Gemfile",
		"rust/Cargo.toml",
		"dart/pubspec.yaml",
		"node_modules/pkg/package.json",
		"dist/generated/package.json",
		"vendor/lib/go.mod",
	} {
		writeFile(t, root, rel)
	}

	got, err := DetectSnapshot(root, Config{WorkerClasses: "all", MaxProjects: 50})
	if err != nil {
		t.Fatalf("DetectSnapshot: %v", err)
	}
	var compact []string
	for _, p := range got {
		compact = append(compact, p.ProjectRoot+":"+p.Language+":"+p.Indexer+":"+p.SCIPWorkerClass)
	}
	want := []string{
		":typescript:scip-typescript:ts-js",
		"dart:dart:dart:rust-dart",
		"dotnet:dotnet:scip-dotnet:dotnet",
		"java:java:scip-java:jvm",
		"native:cpp:scip-clang:cpp",
		"python:python:scip-python:python",
		"ruby:ruby:scip-ruby:ruby",
		"rust:rust:rust-analyzer:rust-dart",
		"services/api:go:scip-go:go",
	}
	if !reflect.DeepEqual(compact, want) {
		t.Fatalf("projects:\n got %#v\nwant %#v", compact, want)
	}
}

func TestDetectSnapshotInfersTypeScriptRoots(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "apps/web/src/app.ts")
	writeFile(t, root, "apps/admin/src/app.jsx")
	writeFile(t, root, "coverage/ignored.ts")
	writeFile(t, root, "src/global.d.ts")
	writeFile(t, root, "src/vendor.min.js")

	got, err := DetectSnapshot(root, Config{WorkerClasses: "ts-js", MaxProjects: 10})
	if err != nil {
		t.Fatalf("DetectSnapshot: %v", err)
	}
	var roots []string
	for _, p := range got {
		roots = append(roots, p.ProjectRoot)
	}
	want := []string{"apps/admin", "apps/web"}
	if !reflect.DeepEqual(roots, want) {
		t.Fatalf("roots = %#v want %#v", roots, want)
	}
}

func TestDetectSnapshotWorkerClassFilterAndCap(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{
		"one/go.mod",
		"two/go.mod",
		"three/requirements.txt",
		"four/package.json",
		"four/src/app.ts",
	} {
		writeFile(t, root, rel)
	}

	got, err := DetectSnapshot(root, Config{WorkerClasses: "go,python", MaxProjects: 2})
	if err != nil {
		t.Fatalf("DetectSnapshot: %v", err)
	}
	var compact []string
	for _, p := range got {
		compact = append(compact, p.ProjectRoot+":"+p.Language)
	}
	want := []string{"one:go", "three:python"}
	if !reflect.DeepEqual(compact, want) {
		t.Fatalf("filtered/capped projects = %#v want %#v", compact, want)
	}

	disabled, err := DetectSnapshot(root, Config{WorkerClasses: "none", MaxProjects: 10})
	if err != nil {
		t.Fatalf("DetectSnapshot none: %v", err)
	}
	if len(disabled) != 0 {
		t.Fatalf("none filter returned projects: %#v", disabled)
	}
}

func TestDetectSnapshotPlanSplitsUnavailableWorkerClasses(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "go.mod")
	writeFile(t, root, "package.json")
	writeFile(t, root, "src/index.ts")
	writeFile(t, root, "service/requirements.txt")

	plan, err := DetectSnapshotPlan(root, Config{WorkerClasses: "go", MaxProjects: 10})
	if err != nil {
		t.Fatalf("DetectSnapshotPlan: %v", err)
	}
	if got := compactProjects(plan.Runnable); !reflect.DeepEqual(got, []string{":go:go"}) {
		t.Fatalf("runnable = %#v", got)
	}
	if got := compactProjects(plan.Skipped); !reflect.DeepEqual(got, []string{":typescript:ts-js", "service:python:python"}) {
		t.Fatalf("skipped = %#v", got)
	}

	t.Setenv(envWorkerClasses, "")
	cfg := ConfigFromEnv()
	if cfg.WorkerClasses != "universal" {
		t.Fatalf("ConfigFromEnv WorkerClasses=%q want universal", cfg.WorkerClasses)
	}
}

func TestDetectSnapshotSkipsToolingOnlyTypeScriptMarkers(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "package.json")
	writeFile(t, root, "autoinstrumentation/nodejs/package.json")
	writeFile(t, root, "autoinstrumentation/nodejs/tsconfig.json")
	writeFile(t, root, "autoinstrumentation/nodejs/Dockerfile")
	writeFile(t, root, ".github/scripts/triage-helper/requirements.txt")
	writeFile(t, root, ".github/scripts/triage-helper/triage.py")

	got, err := DetectSnapshot(root, Config{WorkerClasses: "all", MaxProjects: 10})
	if err != nil {
		t.Fatalf("DetectSnapshot: %v", err)
	}
	if got := compactProjects(got); !reflect.DeepEqual(got, []string{".github/scripts/triage-helper:python:python"}) {
		t.Fatalf("projects = %#v, want only the Python SCIP project", got)
	}
}

func TestDetectSnapshotDoesNotLetParentTypeScriptMarkerClaimChildProjectInput(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "e2e-tests/package.json")
	writeFile(t, root, "e2e-tests/web/package.json")
	writeFile(t, root, "e2e-tests/web/src/app.ts")
	writeFile(t, root, "e2e-tests/api/tsconfig.json")
	writeFile(t, root, "e2e-tests/api/src/server.ts")

	got, err := DetectSnapshot(root, Config{WorkerClasses: "ts-js", MaxProjects: 10})
	if err != nil {
		t.Fatalf("DetectSnapshot: %v", err)
	}
	if got := compactProjects(got); !reflect.DeepEqual(got, []string{"e2e-tests/api:typescript:ts-js", "e2e-tests/web:typescript:ts-js"}) {
		t.Fatalf("projects = %#v, want only child TypeScript projects", got)
	}
}

func TestDetectSnapshotSkipsJavaMarkerWithoutBuildSourceSetInput(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "tests/test-e2e-apps/java/build.gradle")
	writeFile(t, root, "tests/test-e2e-apps/java/DemoApplication.java")
	writeFile(t, root, "services/real-java/build.gradle")
	writeFile(t, root, "services/real-java/src/main/java/com/example/App.java")

	got, err := DetectSnapshot(root, Config{WorkerClasses: "jvm", MaxProjects: 10})
	if err != nil {
		t.Fatalf("DetectSnapshot: %v", err)
	}
	if got := compactProjects(got); !reflect.DeepEqual(got, []string{"services/real-java:java:jvm"}) {
		t.Fatalf("projects = %#v, want only build-source-set Java project", got)
	}
}

func compactProjects(projects []indexplanner.SCIPProject) []string {
	var out []string
	for _, p := range projects {
		out = append(out, p.ProjectRoot+":"+p.Language+":"+p.SCIPWorkerClass)
	}
	return out
}

func writeFile(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
