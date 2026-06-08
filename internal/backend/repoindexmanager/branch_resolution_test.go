package repoindexmanager

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestResolveIndexRevisionsHonorsBranchPolicyAndSnapshotsCommitTree(t *testing.T) {
	root := t.TempDir()
	repo, err := git.PlainInit(root, false)
	if err != nil {
		t.Fatalf("git init: %v", err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	author := &object.Signature{Name: "test", Email: "test@example.local", When: time.Now()}

	writeFile(t, root, "README.md", "main only\n")
	if _, err := tree.Add("README.md"); err != nil {
		t.Fatalf("add README: %v", err)
	}
	mainHash, err := tree.Commit("main", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("commit main: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), mainHash)); err != nil {
		t.Fatalf("set main ref: %v", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}

	if err := tree.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("release-a"), Create: true}); err != nil {
		t.Fatalf("checkout release-a: %v", err)
	}
	writeFile(t, root, "release.txt", "release branch only\n")
	if _, err := tree.Add("release.txt"); err != nil {
		t.Fatalf("add release: %v", err)
	}
	releaseHash, err := tree.Commit("release-a", &git.CommitOptions{Author: author})
	if err != nil {
		t.Fatalf("commit release: %v", err)
	}

	revisions, err := resolveIndexRevisions(root, RepoIndexScope{
		OrgID:         7,
		WorkspaceID:   "atom-ws",
		DefaultBranch: "main",
		Metadata:      []byte(`{"branches":["main","release-*"]}`),
	}, mainHash.String(), "main")
	if err != nil {
		t.Fatalf("resolveIndexRevisions: %v", err)
	}
	got := map[string]string{}
	for _, rev := range revisions {
		got[rev.Branch] = rev.CommitHash
	}
	if got["refs/heads/main"] != mainHash.String() {
		t.Fatalf("main revision hash = %q want %q; all=%v", got["refs/heads/main"], mainHash.String(), got)
	}
	if got["refs/heads/release-a"] != releaseHash.String() {
		t.Fatalf("release revision hash = %q want %q; all=%v", got["refs/heads/release-a"], releaseHash.String(), got)
	}

	mainSnapshot := filepath.Join(t.TempDir(), "main")
	if _, err := materializeCommitSnapshot(root, mainHash.String(), mainSnapshot); err != nil {
		t.Fatalf("materialize main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mainSnapshot, "release.txt")); !os.IsNotExist(err) {
		t.Fatalf("main snapshot leaked release branch file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mainSnapshot, ".git")); err != nil {
		t.Fatalf("main snapshot must be git-backed for SCIP tools: %v", err)
	}
	releaseSnapshot := filepath.Join(t.TempDir(), "release")
	if _, err := materializeCommitSnapshot(root, releaseHash.String(), releaseSnapshot); err != nil {
		t.Fatalf("materialize release: %v", err)
	}
	if _, err := os.Stat(filepath.Join(releaseSnapshot, "release.txt")); err != nil {
		t.Fatalf("release snapshot missing release.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(releaseSnapshot, ".git")); err != nil {
		t.Fatalf("release snapshot must be git-backed for SCIP tools: %v", err)
	}
}

func writeFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
