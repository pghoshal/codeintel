//go:build integration

// Phase R.1 cross-binary parity guard: the Rust
// codeintel-indexer-rs CLI binary clones a real local bare git
// repository and prints the observed HEAD SHA. This test runs
// THE EXACT SAME clone via the Go gitclone module and asserts
// both implementations produce byte-equal HEAD SHAs + byte-
// equal README content on disk.
//
// This is the binding contract for the R.2 hand-off: when the
// Go worker stops doing inline clones and the Rust binary
// takes over, the user-observable outcome (Repo.indexedCommitHash)
// must not change.
//
// Skipped unless CODEINTEL_RUST_INDEXER_BIN env var is set OR
// the release binary is found at the conventional path
// codeintel/indexer-rs/target/release/codeintel-indexer-rs.
package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codeintel/internal/backend/gitclone"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// locateRustIndexerBinary returns the path to the Rust binary
// under test. Order: env var > conventional release path > skip.
func locateRustIndexerBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CODEINTEL_RUST_INDEXER_BIN"); p != "" {
		return p
	}
	wd, _ := os.Getwd()
	// integration tests live at codeintel/tests/integration; the
	// Rust binary builds to codeintel/indexer-rs/target/release/.
	candidate := filepath.Clean(filepath.Join(wd, "..", "..", "indexer-rs", "target", "release", "codeintel-indexer-rs"))
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	t.Skipf("Rust indexer binary not found (set CODEINTEL_RUST_INDEXER_BIN or run `cargo build --release` in codeintel/indexer-rs/); candidate=%s", candidate)
	return ""
}

// buildFakeRemoteForRustParity constructs a local bare git repo
// with deterministic content + returns the file:// URL + expected
// HEAD SHA. Same shape as the Go-side helper but in a fresh
// per-test tmpdir.
func buildFakeRemoteForRustParity(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	wt := filepath.Join(root, "src")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	repo, err := git.PlainInit(wt, false)
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	tree, _ := repo.Worktree()
	author := &object.Signature{Name: "t", Email: "t@e", When: time.Now()}
	var lastBody string
	for i, body := range []string{"first\n", "second\n", "third\n"} {
		if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		_, _ = tree.Add("README.md")
		if _, err := tree.Commit("c"+string(rune('1'+i)), &git.CommitOptions{Author: author}); err != nil {
			t.Fatalf("commit: %v", err)
		}
		lastBody = body
	}
	h, _ := repo.Head()
	expectedHead := h.Hash().String()

	bare := filepath.Join(root, "remote.git")
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: wt}); err != nil {
		t.Fatalf("PlainClone bare: %v", err)
	}
	return "file://" + bare, expectedHead, lastBody
}

func TestParity_GoVsRustClone_SameHeadAndContent(t *testing.T) {
	binary := locateRustIndexerBinary(t)

	cloneURL, expectedHead, expectedContent := buildFakeRemoteForRustParity(t)

	// --- Go side: gitclone.Clone ---
	goDest := filepath.Join(t.TempDir(), "go-clone")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	goRes, err := gitclone.Clone(ctx, gitclone.Request{
		CloneURL:    cloneURL,
		Destination: goDest,
	})
	if err != nil {
		t.Fatalf("Go gitclone: %v", err)
	}

	// --- Rust side: codeintel-indexer-rs clone ---
	rustDest := filepath.Join(t.TempDir(), "rust-clone")
	cmd := exec.CommandContext(ctx, binary,
		"clone",
		"--clone-url", cloneURL,
		"--dest", rustDest,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Rust clone exit=%v output=%s", err, out)
	}
	rustHead := strings.TrimSpace(string(out))

	t.Run("HEAD SHAs match", func(t *testing.T) {
		if goRes.CommitHash != expectedHead {
			t.Errorf("Go HEAD: got %s want %s", goRes.CommitHash, expectedHead)
		}
		if rustHead != expectedHead {
			t.Errorf("Rust HEAD: got %s want %s", rustHead, expectedHead)
		}
		if goRes.CommitHash != rustHead {
			t.Errorf("Go vs Rust diverge: go=%s rust=%s", goRes.CommitHash, rustHead)
		}
	})

	t.Run("README content matches on both sides", func(t *testing.T) {
		goBytes, err := os.ReadFile(filepath.Join(goDest, "README.md"))
		if err != nil {
			t.Fatalf("read Go README: %v", err)
		}
		rustBytes, err := os.ReadFile(filepath.Join(rustDest, "README.md"))
		if err != nil {
			t.Fatalf("read Rust README: %v", err)
		}
		if string(goBytes) != expectedContent {
			t.Errorf("Go README: got %q want %q", goBytes, expectedContent)
		}
		if string(rustBytes) != expectedContent {
			t.Errorf("Rust README: got %q want %q", rustBytes, expectedContent)
		}
		if string(goBytes) != string(rustBytes) {
			t.Errorf("Go vs Rust README content diverge")
		}
	})

	t.Run("both produce a real .git directory", func(t *testing.T) {
		if _, err := os.Stat(filepath.Join(goDest, ".git")); err != nil {
			t.Errorf("Go .git missing: %v", err)
		}
		if _, err := os.Stat(filepath.Join(rustDest, ".git")); err != nil {
			t.Errorf("Rust .git missing: %v", err)
		}
	})
}
