package repoindexmanager

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type manifestFileRow struct {
	Path        string
	ContentHash string
	Language    *string
	ProjectRoot *string
	Generated   bool
	Vendor      bool
	Test        bool
}

func buildManifestFilesForCommit(repoPath, commitHash string) ([]manifestFileRow, error) {
	repo, err := git.PlainOpen(repoPath)
	if err != nil {
		return nil, fmt.Errorf("open git repo: %w", err)
	}
	commit, err := repo.CommitObject(plumbing.NewHash(commitHash))
	if err != nil {
		return nil, fmt.Errorf("resolve commit %s: %w", commitHash, err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("load commit tree: %w", err)
	}
	iter := tree.Files()
	var paths []string
	var raw []manifestFileRow
	err = iter.ForEach(func(file *object.File) error {
		if file == nil {
			return nil
		}
		if !isManifestFileMode(file.Mode) {
			return nil
		}
		p := normalizeManifestPath(file.Name)
		if p == "" {
			return nil
		}
		paths = append(paths, p)
		raw = append(raw, manifestFileRow{
			Path:        p,
			ContentHash: file.Hash.String(),
			Language:    stringPtr(inferManifestLanguage(p)),
			Generated:   isGeneratedManifestPath(p),
			Vendor:      isVendorManifestPath(p),
			Test:        isTestManifestPath(p),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk commit files: %w", err)
	}
	projectRoots := detectManifestProjectRoots(paths)
	for i := range raw {
		root := nearestManifestProjectRoot(raw[i].Path, projectRoots)
		raw[i].ProjectRoot = &root
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].Path < raw[j].Path })
	return raw, nil
}

func isManifestFileMode(mode filemode.FileMode) bool {
	return mode == filemode.Regular || mode == filemode.Executable || mode == filemode.Symlink
}

func normalizeManifestPath(value string) string {
	forward := strings.ReplaceAll(value, "\\", "/")
	return strings.TrimLeft(forward, "/")
}

var manifestRootMarkers = map[string]struct{}{
	"package.json":          {},
	"tsconfig.json":         {},
	"jsconfig.json":         {},
	"go.mod":                {},
	"pyproject.toml":        {},
	"setup.py":              {},
	"requirements.txt":      {},
	"pom.xml":               {},
	"build.gradle":          {},
	"build.sbt":             {},
	"compile_commands.json": {},
	"Cargo.toml":            {},
	"Gemfile":               {},
	"pubspec.yaml":          {},
}

var manifestRootMarkerSuffixes = []string{".csproj", ".vbproj", ".fsproj", ".sln", ".gemspec"}

func detectManifestProjectRoots(paths []string) []string {
	roots := []string{"."}
	seen := map[string]bool{".": true}
	for _, p := range paths {
		base := path.Base(p)
		_, marker := manifestRootMarkers[base]
		if !marker {
			for _, suffix := range manifestRootMarkerSuffixes {
				if strings.HasSuffix(base, suffix) {
					marker = true
					break
				}
			}
		}
		if !marker {
			continue
		}
		root := path.Dir(p)
		if root == "." || root == "/" {
			root = "."
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	return roots
}

func nearestManifestProjectRoot(filePath string, roots []string) string {
	for _, root := range roots {
		if root == "." || filePath == root || strings.HasPrefix(filePath, root+"/") {
			return root
		}
	}
	return "."
}

func inferManifestLanguage(filePath string) string {
	ext := strings.ToLower(path.Ext(filePath))
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return "typescript"
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".java", ".kt", ".scala":
		return "jvm"
	case ".c", ".cc", ".cpp", ".cxx", ".h", ".hpp", ".hh":
		return "cpp"
	case ".cs", ".vb", ".fs":
		return "dotnet"
	case ".rb":
		return "ruby"
	case ".rs":
		return "rust"
	case ".dart":
		return "dart"
	case ".md", ".mdx", ".rst", ".txt":
		return "docs"
	case ".json", ".yaml", ".yml", ".toml", ".xml", ".gradle", ".sbt":
		return "config"
	default:
		return ""
	}
}

func isGeneratedManifestPath(filePath string) bool {
	for _, segment := range []string{"dist", "build", "coverage", "target", "generated", "gen"} {
		if hasManifestDirSegment(filePath, segment) {
			return true
		}
	}
	return hasManifestDirPathPrefix(filePath, "vendor/bundle") ||
		strings.Contains(filePath, ".generated.") ||
		strings.HasSuffix(filePath, ".min.js")
}

func isVendorManifestPath(filePath string) bool {
	for _, segment := range []string{"node_modules", "vendor", "third_party", ".venv", "venv"} {
		if hasManifestDirSegment(filePath, segment) {
			return true
		}
	}
	return false
}

func isTestManifestPath(filePath string) bool {
	for _, segment := range []string{"__tests__", "test", "tests", "spec"} {
		if hasManifestDirSegment(filePath, segment) {
			return true
		}
	}
	base := path.Base(filePath)
	parts := strings.Split(base, ".")
	if len(parts) < 3 {
		return false
	}
	category := parts[len(parts)-2]
	return (category == "test" || category == "spec") && parts[len(parts)-1] != ""
}

func hasManifestDirSegment(filePath, segment string) bool {
	return strings.HasPrefix(filePath, segment+"/") || strings.Contains(filePath, "/"+segment+"/")
}

func hasManifestDirPathPrefix(filePath, prefix string) bool {
	return strings.HasPrefix(filePath, prefix+"/") || strings.Contains(filePath, "/"+prefix+"/")
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
