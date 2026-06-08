// Package scipprojectdetect detects language SCIP project roots from an
// immutable revision snapshot.
package scipprojectdetect

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"codeintel/internal/backend/indexplanner"
)

const (
	envWorkerClasses = "CODEINTEL_INDEX_PLAN_SCIP_WORKER_CLASSES"
	envMaxProjects   = "CODEINTEL_SCIP_MAX_PROJECT_ROOTS_PER_REVISION"
	defaultMax       = 24
)

type Config struct {
	WorkerClasses string
	MaxProjects   int
}

func ConfigFromEnv() Config {
	maxProjects := defaultMax
	if raw := strings.TrimSpace(os.Getenv(envMaxProjects)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			maxProjects = parsed
		}
	}
	workerClasses := strings.TrimSpace(os.Getenv(envWorkerClasses))
	if workerClasses == "" {
		workerClasses = "universal"
	}
	return Config{
		WorkerClasses: workerClasses,
		MaxProjects:   maxProjects,
	}
}

type Plan struct {
	Runnable []indexplanner.SCIPProject
	Skipped  []indexplanner.SCIPProject
}

type definition struct {
	language    string
	indexer     string
	workerClass string
	markers     []string
}

var supported = []definition{
	{language: "typescript", indexer: "scip-typescript", workerClass: "ts-js", markers: []string{"tsconfig.json", "jsconfig.json", "package.json"}},
	{language: "go", indexer: "scip-go", workerClass: "go", markers: []string{"go.mod"}},
	{language: "python", indexer: "scip-python", workerClass: "python", markers: []string{"pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile"}},
	{language: "java", indexer: "scip-java", workerClass: "jvm", markers: []string{"pom.xml", "build.gradle", "build.gradle.kts", "settings.gradle", "settings.gradle.kts", "build.sbt"}},
	{language: "cpp", indexer: "scip-clang", workerClass: "cpp", markers: []string{"compile_commands.json"}},
	{language: "dotnet", indexer: "scip-dotnet", workerClass: "dotnet"},
	{language: "ruby", indexer: "scip-ruby", workerClass: "ruby", markers: []string{"Gemfile"}},
	{language: "rust", indexer: "rust-analyzer", workerClass: "rust-dart", markers: []string{"Cargo.toml"}},
	{language: "dart", indexer: "dart", workerClass: "rust-dart", markers: []string{"pubspec.yaml"}},
}

var ignoredSegments = map[string]struct{}{
	".git":         {},
	"node_modules": {},
	".next":        {},
	"dist":         {},
	"build":        {},
	"target":       {},
	".venv":        {},
	"venv":         {},
	"__pycache__":  {},
	"vendor":       {},
}

var (
	dotnetSuffixes    = []string{".sln", ".csproj", ".vbproj", ".fsproj"}
	rubySuffixes      = []string{".gemspec"}
	typescriptSuffix  = []string{".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs"}
	jvmSourceSuffixes = []string{".java", ".scala", ".kt"}
	defaultClassAllow = "all"
)

type project struct {
	language    string
	indexer     string
	workerClass string
	root        string
}

func DetectSnapshot(snapshotRoot string, cfg Config) ([]indexplanner.SCIPProject, error) {
	plan, err := DetectSnapshotPlan(snapshotRoot, cfg)
	if err != nil {
		return nil, err
	}
	return plan.Runnable, nil
}

func DetectSnapshotPlan(snapshotRoot string, cfg Config) (Plan, error) {
	files, err := snapshotFiles(snapshotRoot)
	if err != nil {
		return Plan{}, err
	}
	projects := detect(files)
	addInferredTypeScriptProjects(files, &projects)
	runnable, skipped := splitWorkerClasses(projects, cfg.WorkerClasses)
	maxProjects := cfg.MaxProjects
	if maxProjects <= 0 {
		maxProjects = defaultMax
	}
	if len(runnable) > maxProjects {
		runnable = runnable[:maxProjects]
	}
	if len(skipped) > maxProjects {
		skipped = skipped[:maxProjects]
	}
	return Plan{
		Runnable: toPlannerProjects(runnable),
		Skipped:  toPlannerProjects(skipped),
	}, nil
}

func toPlannerProjects(projects []project) []indexplanner.SCIPProject {
	out := make([]indexplanner.SCIPProject, 0, len(projects))
	for _, p := range projects {
		out = append(out, indexplanner.SCIPProject{
			Language:        p.language,
			ProjectRoot:     p.root,
			Indexer:         p.indexer,
			SCIPWorkerClass: p.workerClass,
		})
	}
	return out
}

func snapshotFiles(snapshotRoot string) ([]string, error) {
	var files []string
	root, err := filepath.Abs(snapshotRoot)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if pathIsIgnored(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if pathIsIgnored(rel) {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func detect(files []string) []project {
	var out []project
	seen := map[string]struct{}{}
	tsMarkerRoots := typeScriptMarkerRoots(files)
	for _, file := range files {
		base := path.Base(file)
		root := path.Dir(file)
		if root == "." {
			root = ""
		}
		for _, def := range supported {
			if !matches(def, base) {
				continue
			}
			if def.language == "typescript" && !hasTypeScriptInputInRoot(files, root, tsMarkerRoots) {
				continue
			}
			if def.language == "java" && !hasJVMSourceSetInputInRoot(files, root) {
				continue
			}
			key := def.language + "\x00" + root
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, project{
				language:    def.language,
				indexer:     def.indexer,
				workerClass: def.workerClass,
				root:        root,
			})
		}
	}
	sortProjects(out)
	return out
}

func typeScriptMarkerRoots(files []string) []string {
	seen := map[string]struct{}{}
	for _, file := range files {
		base := path.Base(file)
		if base != "package.json" && base != "tsconfig.json" && base != "jsconfig.json" {
			continue
		}
		root := path.Dir(file)
		if root == "." {
			root = ""
		}
		seen[root] = struct{}{}
	}
	var roots []string
	for root := range seen {
		roots = append(roots, root)
	}
	sort.Strings(roots)
	return roots
}

func matches(def definition, base string) bool {
	for _, marker := range def.markers {
		if base == marker {
			return true
		}
	}
	if def.language == "dotnet" {
		return hasAnySuffix(base, dotnetSuffixes)
	}
	if def.language == "ruby" {
		return hasAnySuffix(base, rubySuffixes)
	}
	return false
}

func addInferredTypeScriptProjects(files []string, projects *[]project) {
	var roots []string
	for _, p := range *projects {
		if p.language == "typescript" {
			roots = append(roots, p.root)
		}
	}
	inferred := map[string]struct{}{}
	for _, file := range files {
		lower := strings.ToLower(file)
		if !hasAnySuffix(lower, typescriptSuffix) ||
			strings.HasSuffix(lower, ".d.ts") ||
			strings.HasSuffix(lower, ".min.js") ||
			file == "coverage" ||
			strings.HasPrefix(file, "coverage/") ||
			strings.Contains(file, "/coverage/") {
			continue
		}
		if coveredByRoot(file, roots) {
			continue
		}
		inferred[inferTypeScriptRoot(file)] = struct{}{}
	}
	seen := map[string]struct{}{}
	for _, p := range *projects {
		seen[p.language+"\x00"+p.root] = struct{}{}
	}
	var inferredRoots []string
	for root := range inferred {
		inferredRoots = append(inferredRoots, root)
	}
	sort.Strings(inferredRoots)
	for _, root := range inferredRoots {
		key := "typescript\x00" + root
		if _, ok := seen[key]; ok {
			continue
		}
		*projects = append(*projects, project{
			language:    "typescript",
			indexer:     "scip-typescript",
			workerClass: "ts-js",
			root:        root,
		})
	}
	sortProjects(*projects)
}

func splitWorkerClasses(projects []project, raw string) ([]project, []project) {
	allowed := parseWorkerClasses(raw)
	if _, ok := allowed["none"]; ok {
		return nil, append([]project(nil), projects...)
	}
	if _, ok := allowed["all"]; ok {
		return projects, nil
	}
	if _, ok := allowed["universal"]; ok {
		return projects, nil
	}
	var runnable []project
	var skipped []project
	for _, p := range projects {
		if _, ok := allowed[p.workerClass]; ok {
			runnable = append(runnable, p)
		} else {
			skipped = append(skipped, p)
		}
	}
	return runnable, skipped
}

func parseWorkerClasses(raw string) map[string]struct{} {
	if strings.TrimSpace(raw) == "" {
		raw = defaultClassAllow
	}
	out := map[string]struct{}{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			out[item] = struct{}{}
		}
	}
	if len(out) == 0 {
		out[defaultClassAllow] = struct{}{}
	}
	return out
}

func pathIsIgnored(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		if _, ok := ignoredSegments[part]; ok {
			return true
		}
	}
	return false
}

func hasTypeScriptInputInRoot(files []string, root string, markerRoots []string) bool {
	for _, file := range files {
		if !fileInRoot(file, root) {
			continue
		}
		if containedByNestedRoot(file, root, markerRoots) {
			continue
		}
		lower := strings.ToLower(file)
		if !hasAnySuffix(lower, typescriptSuffix) ||
			strings.HasSuffix(lower, ".d.ts") ||
			strings.HasSuffix(lower, ".min.js") ||
			file == "coverage" ||
			strings.HasPrefix(file, "coverage/") ||
			strings.Contains(file, "/coverage/") {
			continue
		}
		return true
	}
	return false
}

func containedByNestedRoot(file, root string, markerRoots []string) bool {
	for _, child := range markerRoots {
		if child == root || child == "" {
			continue
		}
		if root != "" && !strings.HasPrefix(child, root+"/") {
			continue
		}
		if root == "" || strings.HasPrefix(child, root+"/") {
			if fileInRoot(file, child) {
				return true
			}
		}
	}
	return false
}

func hasJVMSourceSetInputInRoot(files []string, root string) bool {
	for _, file := range files {
		if !fileInRoot(file, root) {
			continue
		}
		rel := strings.TrimPrefix(file, root+"/")
		if root == "" || root == "." {
			rel = file
		}
		lower := strings.ToLower(rel)
		if !hasAnySuffix(lower, jvmSourceSuffixes) {
			continue
		}
		if strings.HasPrefix(lower, "src/") || strings.Contains(lower, "/src/") {
			return true
		}
	}
	return false
}

func fileInRoot(file, root string) bool {
	if root == "" || root == "." {
		return true
	}
	return file == root || strings.HasPrefix(file, root+"/")
}

func coveredByRoot(file string, roots []string) bool {
	for _, root := range roots {
		if fileInRoot(file, root) {
			return true
		}
	}
	return false
}

func inferTypeScriptRoot(file string) string {
	parts := strings.Split(file, "/")
	for i, part := range parts {
		if part == "src" && i > 0 {
			return strings.Join(parts[:i], "/")
		}
	}
	return ""
}

func sortProjects(projects []project) {
	sort.Slice(projects, func(i, j int) bool {
		if projects[i].root == projects[j].root {
			return projects[i].language < projects[j].language
		}
		return projects[i].root < projects[j].root
	})
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}
