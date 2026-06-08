package gitindex

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sourcegraph/zoekt/query"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/build"
	"github.com/sourcegraph/zoekt/shards"
)

func createSourcegraphignoreRepo(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	script := `mkdir repo
cd repo
git init -b master
mkdir subdir
echo acont > afile
echo sub-cont > subdir/sub-file
git add afile subdir/sub-file
git config user.email "you@example.com"
git config user.name "Your Name"
git commit -am amsg

git branch branchdir/abranch

mkdir .sourcegraph
echo subdir/ > .sourcegraph/ignore
git add .sourcegraph/ignore
git commit -am "ignore subdir/"

git update-ref refs/meta/config HEAD
`
	cmd := exec.Command("/bin/sh", "-euxc", script)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("execution error: %v, output %s", err, out)
	}
	return nil
}

func TestIgnore(t *testing.T) {
	dir := t.TempDir()

	if err := createSourcegraphignoreRepo(dir); err != nil {
		t.Fatalf("createSourcegraphignoreRepo: %v", err)
	}

	indexDir := t.TempDir()

	buildOpts := build.Options{
		IndexDir: indexDir,
		RepositoryDescription: zoekt.Repository{
			Name: "repo",
		},
	}
	buildOpts.SetDefaults()

	opts := Options{
		RepoDir:      filepath.Join(dir + "/repo"),
		BuildOptions: buildOpts,
		BranchPrefix: "refs/heads",
		Branches:     []string{"master", "branchdir/*"},
		Submodules:   true,
		Incremental:  true,
	}
	if _, err := IndexGitRepo(opts); err != nil {
		t.Fatalf("IndexGitRepo: %v", err)
	}

	searcher, err := shards.NewDirectorySearcher(indexDir)
	if err != nil {
		t.Fatal("NewDirectorySearcher", err)
	}
	defer searcher.Close()

	res, err := searcher.Search(context.Background(), &query.Substring{}, &zoekt.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Files) != 3 {
		t.Fatalf("expected 3 file matches")
	}
	for _, match := range res.Files {
		switch match.FileName {
		case "afile":
			if !reflect.DeepEqual(match.Branches, []string{"master", "branchdir/abranch"}) {
				t.Fatalf("expected afile to be present on both branches")
			}
		case "subdir/sub-file":
			if len(match.Branches) != 1 || match.Branches[0] != "branchdir/abranch" {
				t.Fatalf("expected sub-file to be present only on branchdir/abranch")
			}
		case ".sourcegraph/ignore":
			if len(match.Branches) != 1 || match.Branches[0] != "master" {
				t.Fatalf("expected sourcegraphignore to be present only on master")
			}
		default:
			t.Fatalf("match %+v not handled", match)
		}
	}
}

func TestIgnoreDirsOption(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "src", "vendor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, ".next", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}

	script := `git init -b main
git config user.email "you@example.com"
git config user.name "Your Name"
echo keep > app.go
echo nested > src/vendor/generated.go
echo module > node_modules/pkg/index.js
echo cache > .next/cache/page.js
git add .
git commit -m initial
`
	cmd := exec.Command("/bin/sh", "-euxc", script)
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create repo: %v, output %s", err, out)
	}

	indexDir := t.TempDir()
	buildOpts := build.Options{
		IndexDir: indexDir,
		RepositoryDescription: zoekt.Repository{
			Name: "repo",
		},
	}
	buildOpts.SetDefaults()

	opts := Options{
		RepoDir:      repoDir,
		BuildOptions: buildOpts,
		BranchPrefix: "refs/heads",
		Branches:     []string{"main"},
		Submodules:   true,
		Incremental:  true,
		IgnoreDirs:   []string{"node_modules", "vendor", ".next"},
	}
	if _, err := IndexGitRepo(opts); err != nil {
		t.Fatalf("IndexGitRepo: %v", err)
	}

	searcher, err := shards.NewDirectorySearcher(indexDir)
	if err != nil {
		t.Fatal("NewDirectorySearcher", err)
	}
	defer searcher.Close()

	res, err := searcher.Search(context.Background(), &query.Substring{}, &zoekt.SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("expected only app.go to be indexed, got %+v", res.Files)
	}
	if res.Files[0].FileName != "app.go" {
		t.Fatalf("expected app.go, got %s", res.Files[0].FileName)
	}
}
