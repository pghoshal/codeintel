package gitclone

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// buildFakeRemote creates a real local bare git repository with
// 3 commits on a "main" branch + a feature branch with 1 extra
// commit. Used by the test as a real "remote" the production
// Clone codepath can pull from over file://. No shell-out, no
// network, fully hermetic.
//
// Returns the file:// URL of the bare repo + the SHA of the
// main-branch HEAD so the test can assert Clone observed the
// same SHA the remote advertises.
func buildFakeRemote(t *testing.T) (cloneURL, mainHead string) {
	t.Helper()
	// Step 1: build a working repo we'll commit into.
	workTree := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(workTree, 0o755); err != nil {
		t.Fatalf("mkdir workTree: %v", err)
	}
	repo, err := git.PlainInit(workTree, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}

	// 3 commits on the default branch.
	author := &object.Signature{Name: "test", Email: "test@example.com", When: time.Now()}
	for i, content := range []string{"v1\n", "v2\n", "v3\n"} {
		path := filepath.Join(workTree, "README.md")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write README v%d: %v", i+1, err)
		}
		if _, err := wt.Add("README.md"); err != nil {
			t.Fatalf("Add: %v", err)
		}
		commitMsg := "v" + string(rune('1'+i))
		_, err := wt.Commit(commitMsg, &git.CommitOptions{Author: author})
		if err != nil {
			t.Fatalf("Commit v%d: %v", i+1, err)
		}
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	mainHead = head.Hash().String()

	// Step 2: re-pack the working repo as a bare repo via clone.
	// PlainClone (file:// source) into a bare directory gives us
	// a real "remote" the production Clone() codepath can talk
	// to without any custom transport.
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainClone(bareDir, true, &git.CloneOptions{URL: workTree}); err != nil {
		t.Fatalf("PlainClone (bare): %v", err)
	}
	cloneURL = "file://" + bareDir
	return cloneURL, mainHead
}

// TestClone_RealHTTPNot — full happy path. Real git clone of a
// real local bare repo. Asserts: (a) the working tree appears,
// (b) the README.md is on disk with the v3 content, (c) HEAD
// SHA matches what the remote advertises.
func TestClone_HappyPath_FullClone(t *testing.T) {
	cloneURL, expectedHead := buildFakeRemote(t)
	dest := filepath.Join(t.TempDir(), "clone")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := Clone(ctx, Request{
		CloneURL:    cloneURL,
		Destination: dest,
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if res.CommitHash != expectedHead {
		t.Errorf("CommitHash: got %s want %s", res.CommitHash, expectedHead)
	}
	if res.WorkTree != dest {
		t.Errorf("WorkTree: got %s want %s", res.WorkTree, dest)
	}
	// README content should be the latest "v3".
	content, err := os.ReadFile(filepath.Join(dest, "README.md"))
	if err != nil {
		t.Fatalf("read cloned README: %v", err)
	}
	if string(content) != "v3\n" {
		t.Errorf("README content: got %q want %q", content, "v3\n")
	}
	// .git directory must exist (proves it's a real working repo,
	// not a shell-out artefact).
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		t.Errorf(".git missing: %v", err)
	}
}

func TestClone_ShallowDepth1_ProducesShallowMarker(t *testing.T) {
	// --depth 1 clones produce a .git/shallow marker file
	// containing the SHA of the truncation boundary. Checking
	// that the file exists is a more reliable assertion than
	// walking the log (go-git's log iterator on a shallow
	// file:// clone hits "object not found" because the parent
	// objects are intentionally absent).
	cloneURL, _ := buildFakeRemote(t)
	dest := filepath.Join(t.TempDir(), "shallow")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir dest: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := Clone(ctx, Request{
		CloneURL:    cloneURL,
		Destination: dest,
		Depth:       1,
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if res.CommitHash == "" {
		t.Errorf("shallow clone HEAD: got empty")
	}
	if _, err := os.Stat(filepath.Join(dest, ".git", "shallow")); err != nil {
		t.Errorf(".git/shallow marker missing: %v", err)
	}
}

func TestClone_DestinationMustBeEmpty(t *testing.T) {
	cloneURL, _ := buildFakeRemote(t)
	dest := t.TempDir() // TempDir is empty but the path exists.
	if err := os.WriteFile(filepath.Join(dest, "leftover.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed leftover: %v", err)
	}
	_, err := Clone(context.Background(), Request{CloneURL: cloneURL, Destination: dest})
	if !errors.Is(err, ErrDestinationNotEmpty) {
		t.Errorf("err: got %v, want ErrDestinationNotEmpty", err)
	}
}

func TestClone_InvalidCloneURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"unsupported_scheme", "ftp://example.com/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Clone(context.Background(), Request{
				CloneURL:    tc.url,
				Destination: t.TempDir(),
			})
			if !errors.Is(err, ErrInvalidCloneURL) {
				t.Errorf("err: got %v want ErrInvalidCloneURL", err)
			}
		})
	}
}

func TestClone_EmptyDestination(t *testing.T) {
	_, err := Clone(context.Background(), Request{
		CloneURL:    "file:///tmp/x",
		Destination: "",
	})
	if err == nil {
		t.Fatalf("expected error for empty destination")
	}
}

func TestClone_ContextCancelled(t *testing.T) {
	cloneURL, _ := buildFakeRemote(t)
	dest := filepath.Join(t.TempDir(), "cancelled")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE clone
	_, err := Clone(ctx, Request{
		CloneURL:    cloneURL,
		Destination: dest,
	})
	if err == nil {
		t.Errorf("expected error for cancelled context")
	}
}

func TestCloneRetryClassifier(t *testing.T) {
	if !isRetryableCloneError(errors.New(`git fallback failed: unexpected EOF`)) {
		t.Fatal("unexpected EOF should be retryable")
	}
	if !isRetryableCloneError(errors.New("transport: TLS handshake timeout")) {
		t.Fatal("TLS handshake timeout should be retryable")
	}
	if isRetryableCloneError(context.Canceled) {
		t.Fatal("context cancellation should not be retried")
	}
	if isRetryableCloneError(ErrInvalidCloneURL) {
		t.Fatal("invalid clone URL should not be retried")
	}
}

func TestGitCloneArgsDropsDepthForFullFallback(t *testing.T) {
	req := Request{
		CloneURL:    "http://git-fixtures/repo.git",
		Destination: "/tmp/repo",
		Branch:      "refs/heads/release-a",
		Depth:       1,
	}
	shallow := gitCloneArgs(req, req.Depth)
	if !stringSliceContains(shallow, "--depth") {
		t.Fatalf("shallow fallback args missing --depth: %v", shallow)
	}
	full := gitCloneArgs(req, 0)
	if stringSliceContains(full, "--depth") {
		t.Fatalf("full fallback args must drop --depth for dumb HTTP remotes: %v", full)
	}
	if !stringSliceContains(full, "--branch") || !stringSliceContains(full, "release-a") {
		t.Fatalf("full fallback must preserve branch selection: %v", full)
	}
}

func TestDumbHTTPShallowUnsupportedClassifier(t *testing.T) {
	var stderr bytes.Buffer
	stderr.WriteString("fatal: dumb http transport does not support shallow capabilities")
	if !isDumbHTTPShallowUnsupported(stderr) {
		t.Fatalf("expected dumb HTTP shallow stderr to trigger full clone fallback")
	}
	var other bytes.Buffer
	other.WriteString("fatal: repository not found")
	if isDumbHTTPShallowUnsupported(other) {
		t.Fatalf("unrelated git failure must not trigger full clone fallback")
	}
}

// TestClone_CredentialsForwarded_NoCrashOnEmpty verifies the
// auth-wiring branch: when credentials are non-empty go-git
// receives a BasicAuth; when empty (anonymous) no auth header
// is sent. We can't easily assert the network header from
// go-git's internals, but we can confirm both code paths
// complete without error against the local bare remote.
func TestClone_AnonymousAndCredentialed_BothWork(t *testing.T) {
	cloneURL, _ := buildFakeRemote(t)

	t.Run("anonymous", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "anon")
		_ = os.MkdirAll(dest, 0o755)
		if _, err := Clone(context.Background(), Request{
			CloneURL:    cloneURL,
			Destination: dest,
			Credentials: Credentials{}, // empty
		}); err != nil {
			t.Errorf("anonymous clone: %v", err)
		}
	})

	t.Run("with_credentials", func(t *testing.T) {
		// file:// transport ignores the BasicAuth header (no
		// auth handshake on a local repo), but the code path
		// that sets opts.Auth still runs. This is the smallest
		// hermetic gate that exercises the auth-wiring branch.
		dest := filepath.Join(t.TempDir(), "auth")
		_ = os.MkdirAll(dest, 0o755)
		if _, err := Clone(context.Background(), Request{
			CloneURL:    cloneURL,
			Destination: dest,
			Credentials: Credentials{Username: "x-access-token", Password: "ghp_fake"},
		}); err != nil {
			t.Errorf("authenticated clone: %v", err)
		}
	})
}

func TestCredentials_Empty(t *testing.T) {
	if !(Credentials{}).Empty() {
		t.Error("zero Credentials should be Empty()")
	}
	if (Credentials{Username: "x"}).Empty() {
		t.Error("Credentials with Username should not be Empty()")
	}
	if (Credentials{Password: "y"}).Empty() {
		t.Error("Credentials with Password should not be Empty()")
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestClone_BranchTargeting(t *testing.T) {
	// Build a remote with a non-default branch + commit on it.
	workTree := filepath.Join(t.TempDir(), "src")
	_ = os.MkdirAll(workTree, 0o755)
	repo, err := git.PlainInit(workTree, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, _ := repo.Worktree()
	author := &object.Signature{Name: "t", Email: "t@e", When: time.Now()}
	_ = os.WriteFile(filepath.Join(workTree, "f.txt"), []byte("on-main"), 0o644)
	_, _ = wt.Add("f.txt")
	_, _ = wt.Commit("main", &git.CommitOptions{Author: author})
	// Create + switch to branch "feature".
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("feature"),
		Create: true,
	}); err != nil {
		t.Fatalf("checkout feature: %v", err)
	}
	_ = os.WriteFile(filepath.Join(workTree, "f.txt"), []byte("on-feature"), 0o644)
	_, _ = wt.Add("f.txt")
	featureCommit, _ := wt.Commit("feature", &git.CommitOptions{Author: author})

	bareDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainClone(bareDir, true, &git.CloneOptions{URL: workTree}); err != nil {
		t.Fatalf("PlainClone bare: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "clone-feature")
	_ = os.MkdirAll(dest, 0o755)
	res, err := Clone(context.Background(), Request{
		CloneURL:    "file://" + bareDir,
		Destination: dest,
		Branch:      "feature",
	})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if res.CommitHash != featureCommit.String() {
		t.Errorf("CommitHash: got %s want %s (feature tip)", res.CommitHash, featureCommit.String())
	}
	if res.Branch != "feature" {
		t.Errorf("Branch: got %s want feature", res.Branch)
	}
	content, _ := os.ReadFile(filepath.Join(dest, "f.txt"))
	if string(content) != "on-feature" {
		t.Errorf("content: got %q want on-feature", content)
	}
}
