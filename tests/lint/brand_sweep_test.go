// Package lint hosts repo-level guard tests. The brand-sweep
// test walks every codeintel source file and fails on any
// reference to legacy brand identifiers.
//
// Rule: the project name is `codeintel`. Never reintroduce
// legacy product names or old short-key prefixes in code,
// identifiers, comments, log messages, env vars, Docker image
// names, Helm labels, metric names, gRPC service names, file
// paths, package paths, module names, or docs.
//
// The test is part of the regular `go test ./...` run so a
// regression fails the suite immediately.
package lint

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// codeintelRoot resolves the codeintel/ directory the test
// walks. The test file lives at codeintel/tests/lint/, so two
// `..` jumps reach codeintel/.
func codeintelRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// codeintel/tests/lint -> codeintel
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	return root
}

// scanExtensions are the file types the sweep covers. SQL,
// Go, Rust, proto, scripts, config, and Markdown are the
// source surfaces that ship with codeintel. go.sum is excluded
// (third-party hash dump; brand mentions there are inside
// dependency module paths we don't control).
var scanExtensions = map[string]bool{
	".go":    true,
	".rs":    true,
	".proto": true,
	".sql":   true,
	".md":    true,
	".yaml":  true,
	".yml":   true,
	".toml":  true,
	".sh":    true,
}

// excludedPaths are files / directories the walker skips.
// All paths are relative to the codeintel root.
var excludedPaths = map[string]bool{
	// The brand-sweep test itself necessarily mentions the
	// banned terms; exclude it explicitly.
	"tests/lint/brand_sweep_test.go": true,
	// go.sum entries are dependency-module path hashes; brand
	// mentions there are inside upstream module paths.
	"go.sum": true,
}

// excludedDirs are directories whose entire subtree is skipped.
var excludedDirs = map[string]bool{
	"vendor":       true,
	".git":         true,
	"dist":         true,
	"node_modules": true,
}

// brandPattern catches any of the banned identifier shapes.
// Case-insensitive matching catches the legacy product name in
// every common casing. The old short-key prefix patterns catch
// both underscore + dash forms.
//
// "sbk" or "sb" alone (without a separator) would be too
// aggressive — they can occur as substrings of legitimate words.
// The boundary forms below catch every real legacy-prefix use
// case without false-positives.
var brandPattern = regexp.MustCompile(`(?i)` + "source" + `bot|@` + "source" + `bot|\bsbk[_-]|\bsb[_-]`)

// TestNoBrandLeaks walks the codeintel tree and fails with a
// concrete file:line for every banned identifier it finds.
func TestNoBrandLeaks(t *testing.T) {
	root := codeintelRoot(t)

	type hit struct {
		path string
		line int
		text string
	}
	var hits []hit

	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if excludedDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if excludedPaths[rel] {
			return nil
		}
		if !scanExtensions[filepath.Ext(path)] {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		// Some legacy migrations have long lines; bump the
		// per-line cap so scanner doesn't truncate.
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if brandPattern.MatchString(line) {
				hits = append(hits, hit{
					path: rel,
					line: lineNo,
					text: strings.TrimSpace(line),
				})
			}
		}
		return scanner.Err()
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(hits) > 0 {
		t.Errorf("brand-leak guard: %d disallowed reference(s) found:", len(hits))
		for _, h := range hits {
			t.Errorf("  %s:%d  %s", h.path, h.line, h.text)
		}
		t.Logf(`
Rule: per architecture rules the codeintel codebase must not reintroduce
legacy brand identifiers. Replace each hit above with a neutral
name. The brand-sweep test itself + go.sum + vendor/ are
already excluded; anything else flagged here is a real leak.`)
	}
}
