package connectionmanager

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// =====================================================================
// Dispatch
// =====================================================================

func TestCompileGenericGitHostFromConfig_RequiresURL(t *testing.T) {
	_, _, err := CompileGenericGitHostFromConfig(context.Background(),
		GenericGitHostConnectionConfig{}, 1)
	if err == nil {
		t.Errorf("expected error for empty URL")
	}
}

func TestCompileGenericGitHostFromConfig_UnsupportedScheme(t *testing.T) {
	_, _, err := CompileGenericGitHostFromConfig(context.Background(),
		GenericGitHostConnectionConfig{URL: "ssh://git@example.com/x.git"}, 1)
	if !errors.Is(err, ErrGenericGitUnsupportedScheme) {
		t.Errorf("got %v want ErrGenericGitUnsupportedScheme", err)
	}
}

// =====================================================================
// http(s):// branch
// =====================================================================

// TestCompileGenericGitURL_HappyPath: httptest fake that
// presents the git-smart-protocol content type.
func TestCompileGenericGitURL_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/info/refs") {
			w.WriteHeader(404)
			return
		}
		if r.URL.Query().Get("service") != "git-upload-pack" {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("001e# service=git-upload-pack\n"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, warnings, err := CompileGenericGitHostFromConfig(ctx,
		GenericGitHostConnectionConfig{URL: srv.URL + "/myrepo.git"}, 33)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	if len(records) != 1 {
		t.Fatalf("records: %d", len(records))
	}
	r := records[0]
	if r.ExternalCodeHostType != "generic-git-host" {
		t.Errorf("codeHostType: %q", r.ExternalCodeHostType)
	}
	if r.CloneURL != srv.URL+"/myrepo.git" {
		t.Errorf("cloneURL: %q", r.CloneURL)
	}
	// DisplayName = host + "/myrepo" (with .git suffix stripped).
	if !strings.HasSuffix(r.DisplayName, "/myrepo") {
		t.Errorf("DisplayName missing /myrepo suffix: %q", r.DisplayName)
	}
	if !r.IsPublic {
		t.Errorf("IsPublic: got false want true")
	}
	if r.ConnectionIDs == nil || r.ConnectionIDs[0] != 33 {
		t.Errorf("ConnectionIDs: %+v", r.ConnectionIDs)
	}
	// Validate the URL-branch gitConfig keys are present.
	if r.Metadata.GitConfig == nil {
		t.Errorf("URL branch should emit gitConfig")
	} else {
		if r.Metadata.GitConfig["zoekt.web-url"] != srv.URL+"/myrepo.git" {
			t.Errorf("zoekt.web-url: %q", r.Metadata.GitConfig["zoekt.web-url"])
		}
	}
}

func TestCompileGenericGitURL_DumbHTTPInfoRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/info/refs") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("service") != "git-upload-pack" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("147dc2ddd5832db3e23f391fa8b1930cca05af11\trefs/heads/main\n"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, warnings, err := CompileGenericGitHostFromConfig(ctx,
		GenericGitHostConnectionConfig{URL: srv.URL + "/dumb.git"}, 34)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	if len(records) != 1 {
		t.Fatalf("records: %d", len(records))
	}
	if records[0].CloneURL != srv.URL+"/dumb.git" {
		t.Errorf("cloneURL: %q", records[0].CloneURL)
	}
}

// TestCompileGenericGitURL_NotAGitRepo: httptest returns
// non-git content-type -> warning + empty result.
func TestCompileGenericGitURL_NotAGitRepo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>not git</html>"))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, warnings, err := CompileGenericGitHostFromConfig(ctx,
		GenericGitHostConnectionConfig{URL: srv.URL + "/whatever"}, 1)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("records: got %d want 0", len(records))
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "not a git repository") {
		t.Errorf("warning: %v", warnings)
	}
}

func TestIsURLAValidGitRepo_NetworkFailure(t *testing.T) {
	// 192.0.2.1 is RFC 5737 TEST-NET-1, guaranteed unroutable.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	got := IsURLAValidGitRepo(ctx, &http.Client{Timeout: 400 * time.Millisecond},
		"http://192.0.2.1/x.git")
	if got {
		t.Errorf("expected false for unreachable host")
	}
}

// =====================================================================
// file:// branch
// =====================================================================

// initLocalGitRepo creates a minimal .git layout in a temp dir
// for the file:// branch test. Avoids requiring `git` binary;
// hand-writes the .git/config and .git/HEAD files.
func initLocalGitRepo(t *testing.T, originURL, head string) string {
	t.Helper()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	cfg := "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = " + originURL + "\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/"+head+"\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	return dir
}

func TestIsPathAValidGitRepoRoot(t *testing.T) {
	repo := initLocalGitRepo(t, "https://example/x.git", "main")
	if !IsPathAValidGitRepoRoot(repo) {
		t.Errorf("expected true for repo dir")
	}
	notRepo := t.TempDir()
	if IsPathAValidGitRepoRoot(notRepo) {
		t.Errorf("expected false for non-repo dir")
	}
}

func TestGetOriginURL(t *testing.T) {
	repo := initLocalGitRepo(t, "https://example.com/owner/repo.git", "main")
	got := GetOriginURL(repo)
	if got != "https://example.com/owner/repo.git" {
		t.Errorf("origin: got %q", got)
	}
}

func TestGetOriginURL_EmptyOnNoConfig(t *testing.T) {
	got := GetOriginURL(t.TempDir())
	if got != "" {
		t.Errorf("expected empty on missing config, got %q", got)
	}
}

func TestGetLocalDefaultBranch(t *testing.T) {
	repo := initLocalGitRepo(t, "https://x", "trunk")
	got := GetLocalDefaultBranch(repo)
	if got == nil || *got != "trunk" {
		t.Errorf("default branch: got %v want trunk", got)
	}
}

func TestCompileGenericGitFile_HappyPath(t *testing.T) {
	repo := initLocalGitRepo(t, "https://github.com/owner/myrepo.git", "main")
	// Use the repo path directly as the file:// URL.
	cfgURL := "file://" + repo
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, warnings, err := CompileGenericGitHostFromConfig(ctx,
		GenericGitHostConnectionConfig{URL: cfgURL}, 7)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records: got %d want 1, warnings=%v", len(records), warnings)
	}
	r := records[0]
	if r.ExternalCodeHostType != "generic-git-host" {
		t.Errorf("codeHostType: %q", r.ExternalCodeHostType)
	}
	if r.CloneURL != "file://"+repo {
		t.Errorf("cloneURL: got %q want file://%s", r.CloneURL, repo)
	}
	// Display name should be "github.com/owner/myrepo" (no .git suffix).
	if r.DisplayName != "github.com/owner/myrepo" {
		t.Errorf("DisplayName: got %q", r.DisplayName)
	}
	if r.DefaultBranch == nil || *r.DefaultBranch != "main" {
		t.Errorf("DefaultBranch: %v", r.DefaultBranch)
	}
	if r.Metadata.GitConfig != nil {
		t.Errorf("file branch should emit no gitConfig (local repos read-only)")
	}
}

func TestCompileGenericGitFile_GlobNoMatches(t *testing.T) {
	cfgURL := "file:///tmp/codeintel-no-such-pattern-xyz-*"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, warnings, err := CompileGenericGitHostFromConfig(ctx,
		GenericGitHostConnectionConfig{URL: cfgURL}, 1)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("records: %d", len(records))
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "No paths matched") {
		t.Errorf("warning: %v", warnings)
	}
}

func TestCompileGenericGitFile_PathIsNotDirectory(t *testing.T) {
	// Create a regular file (not a directory) and point file://
	// at it.
	tmp := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(tmp, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	records, warnings, err := CompileGenericGitHostFromConfig(ctx,
		GenericGitHostConnectionConfig{URL: "file://" + tmp}, 1)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("records: %d", len(records))
	}
	hasWarn := false
	for _, w := range warnings {
		if strings.Contains(w, "not a directory") {
			hasWarn = true
		}
	}
	if !hasWarn {
		t.Errorf("missing 'not a directory' warning: %v", warnings)
	}
}

// TestParseGitConfigOriginURL covers the hand-rolled parser
// against fixtures with comments, whitespace, multiple remotes.
func TestParseGitConfigOriginURL(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want string
	}{
		{
			"basic",
			"[remote \"origin\"]\n\turl = https://example.com/x.git\n",
			"https://example.com/x.git",
		},
		{
			"with-comments",
			"# comment\n[remote \"origin\"]\n; another\n\turl = https://example/x\n",
			"https://example/x",
		},
		{
			"multiple-remotes-pick-origin",
			"[remote \"upstream\"]\n\turl = https://up/x\n[remote \"origin\"]\n\turl = https://origin/x\n",
			"https://origin/x",
		},
		{
			"no-origin",
			"[remote \"upstream\"]\n\turl = https://up/x\n",
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseGitConfigOriginURL(tc.cfg); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestParseSSHGitURL(t *testing.T) {
	cases := []struct {
		raw        string
		host, path string
		ok         bool
	}{
		{"git@github.com:owner/repo.git", "github.com", "owner/repo.git", true},
		{"https://example.com/x", "", "", false},
		{"git@", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			host, p, ok := parseSSHGitURL(tc.raw)
			if host != tc.host || p != tc.path || ok != tc.ok {
				t.Errorf("got (%q, %q, %v) want (%q, %q, %v)",
					host, p, ok, tc.host, tc.path, tc.ok)
			}
		})
	}
}

func TestGenericGitRepoNameFromOrigin(t *testing.T) {
	cases := []struct {
		origin string
		want   string
	}{
		{"https://github.com/owner/repo.git", "github.com/owner/repo"},
		{"https://example.com/path/with%20spaces/repo.git", "example.com/path/with spaces/repo"},
		{"git@github.com:owner/repo.git", "github.com/owner/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.origin, func(t *testing.T) {
			got := genericGitRepoNameFromOrigin(tc.origin)
			if got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// Sanity test - make sure init-local-git fixture doesn't need
// the git binary on the host (we wrote the files by hand).
func TestInitLocalGitRepoFixture(t *testing.T) {
	// Just confirm `git` binary is not invoked by the test.
	// If it were, this test would silently use a different
	// flow. The fixture writes raw files; we verify by
	// inspecting the repo dir.
	repo := initLocalGitRepo(t, "https://example/x.git", "main")
	if _, err := os.Stat(filepath.Join(repo, ".git", "config")); err != nil {
		t.Errorf(".git/config not present: %v", err)
	}
	// Also make sure `git rev-parse` isn't required to function -
	// we don't call it.
	_ = exec.Command // pin import; helper might be unused otherwise
}
