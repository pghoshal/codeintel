package repopaths

import (
	"path/filepath"
	"testing"
)

func TestRepoPath_FlatLayout(t *testing.T) {
	cfg := Config{DataCacheDir: "/data"}
	repo := Repo{OrgID: 7, RepoID: 42, CodeHostType: "github", CloneURL: "https://github.com/o/r.git"}
	path, ro, err := cfg.RepoPath(repo)
	if err != nil {
		t.Fatalf("RepoPath: %v", err)
	}
	want := filepath.Join("/data", "repos", "42")
	if path != want {
		t.Errorf("path: got %q want %q", path, want)
	}
	if ro {
		t.Errorf("isReadOnly: got true want false")
	}
}

func TestRepoPath_OrgDirectoryLayout(t *testing.T) {
	cfg := Config{
		DataCacheDir:       "/data",
		ZoektStorageLayout: StorageLayoutOrgDirectory,
	}
	repo := Repo{OrgID: 7, RepoID: 42, CodeHostType: "gitlab"}
	path, ro, _ := cfg.RepoPath(repo)
	want := filepath.Join("/data", "zoekt-orgs", "7", "repos", "42")
	if path != want {
		t.Errorf("path: got %q want %q", path, want)
	}
	if ro {
		t.Errorf("isReadOnly: got true want false")
	}
}

func TestRepoPath_OrgDirectoryLayout_WithEFSRoot(t *testing.T) {
	cfg := Config{
		DataCacheDir:       "/data",
		ZoektEFSRoot:       "/mnt/efs/zoekt",
		ZoektStorageLayout: StorageLayoutOrgDirectory,
	}
	repo := Repo{OrgID: 7, RepoID: 42, CodeHostType: "github"}
	path, _, _ := cfg.RepoPath(repo)
	want := filepath.Join("/mnt/efs/zoekt", "7", "repos", "42")
	if path != want {
		t.Errorf("path: got %q want %q", path, want)
	}
}

func TestRepoPath_FileScheme_ReadOnly(t *testing.T) {
	cfg := Config{DataCacheDir: "/data"}
	for _, codeHostType := range []string{"genericGitHost", "generic-git-host"} {
		repo := Repo{
			OrgID:        7,
			RepoID:       42,
			CodeHostType: codeHostType,
			CloneURL:     "file:///local/repos/myproject",
		}
		path, ro, err := cfg.RepoPath(repo)
		if err != nil {
			t.Fatalf("RepoPath(%s): %v", codeHostType, err)
		}
		if path != "/local/repos/myproject" {
			t.Errorf("RepoPath(%s) path: got %q want /local/repos/myproject", codeHostType, path)
		}
		if !ro {
			t.Errorf("RepoPath(%s) isReadOnly: got false want true", codeHostType)
		}
	}
}

func TestRepoPath_GenericGitHttp_NotReadOnly(t *testing.T) {
	// genericGitHost with http:// CloneURL is NOT read-only — it's
	// a remote git host the indexer clones into the local cache.
	cfg := Config{DataCacheDir: "/data"}
	repo := Repo{
		OrgID:        7,
		RepoID:       42,
		CodeHostType: "genericGitHost",
		CloneURL:     "https://git.internal/o/r.git",
	}
	path, ro, _ := cfg.RepoPath(repo)
	want := filepath.Join("/data", "repos", "42")
	if path != want || ro {
		t.Errorf("got (%q, %v) want (%q, false)", path, ro, want)
	}
}

func TestZoektIndexPath_FlatLayout(t *testing.T) {
	cfg := Config{DataCacheDir: "/data"}
	got := cfg.ZoektIndexPath(Repo{OrgID: 7})
	want := filepath.Join("/data", "index")
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestZoektIndexPath_OrgDirectoryLayout(t *testing.T) {
	cfg := Config{
		DataCacheDir:       "/data",
		ZoektStorageLayout: StorageLayoutOrgDirectory,
	}
	got := cfg.ZoektIndexPath(Repo{OrgID: 7})
	want := filepath.Join("/data", "zoekt-orgs", "7", "index")
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestZoektIndexPath_OrgDirectoryLayout_WithEFSRoot(t *testing.T) {
	cfg := Config{
		DataCacheDir:       "/data",
		ZoektEFSRoot:       "/mnt/efs/zoekt",
		ZoektStorageLayout: StorageLayoutOrgDirectory,
	}
	got := cfg.ZoektIndexPath(Repo{OrgID: 7})
	want := filepath.Join("/mnt/efs/zoekt", "7", "index")
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestShardPrefix(t *testing.T) {
	got := ShardPrefix(7, 42)
	if got != "7_42" {
		t.Errorf("got %q want %q", got, "7_42")
	}
	filePrefix := ShardFilePrefix(7, 42)
	if filePrefix != "7_42_" {
		t.Errorf("file prefix got %q want %q", filePrefix, "7_42_")
	}
}

func TestRevisionSnapshotPath(t *testing.T) {
	cfg := Config{DataCacheDir: "/data"}
	got := cfg.RevisionSnapshotPath(7, 42, "abc")
	want := filepath.Join("/data", "codeintel", "revision-snapshots", "42", "abc")
	if got != want {
		t.Fatalf("flat path: got %q want %q", got, want)
	}

	cfg = Config{
		DataCacheDir:       "/data",
		ZoektEFSRoot:       "/mnt/efs/zoekt",
		ZoektStorageLayout: StorageLayoutOrgDirectory,
	}
	got = cfg.RevisionSnapshotPath(7, 42, "abc")
	want = filepath.Join("/mnt/efs/zoekt", "7", "codeintel", "revision-snapshots", "42", "abc")
	if got != want {
		t.Fatalf("org path: got %q want %q", got, want)
	}
}

func TestLoadConfigFromEnv_Defaults(t *testing.T) {
	t.Setenv("CODEINTEL_DATA_CACHE_DIR", "")
	t.Setenv("CODEINTEL_ZOEKT_EFS_ROOT", "")
	t.Setenv("CODEINTEL_ZOEKT_STORAGE_LAYOUT", "")
	cfg := LoadConfigFromEnv()
	if cfg.DataCacheDir != "./data" {
		t.Errorf("DataCacheDir: got %q want ./data", cfg.DataCacheDir)
	}
	if cfg.ZoektEFSRoot != "" {
		t.Errorf("ZoektEFSRoot: got %q want empty", cfg.ZoektEFSRoot)
	}
	if cfg.ZoektStorageLayout != "" {
		t.Errorf("ZoektStorageLayout: got %q want empty", cfg.ZoektStorageLayout)
	}
}

func TestLoadConfigFromEnv_AllSet(t *testing.T) {
	t.Setenv("CODEINTEL_DATA_CACHE_DIR", "/var/cache/codeintel")
	t.Setenv("CODEINTEL_ZOEKT_EFS_ROOT", "/mnt/efs/zoekt")
	t.Setenv("CODEINTEL_ZOEKT_STORAGE_LAYOUT", StorageLayoutOrgDirectory)
	cfg := LoadConfigFromEnv()
	if cfg.DataCacheDir != "/var/cache/codeintel" {
		t.Errorf("DataCacheDir: %q", cfg.DataCacheDir)
	}
	if cfg.ZoektEFSRoot != "/mnt/efs/zoekt" {
		t.Errorf("ZoektEFSRoot: %q", cfg.ZoektEFSRoot)
	}
	if cfg.ZoektStorageLayout != StorageLayoutOrgDirectory {
		t.Errorf("ZoektStorageLayout: %q", cfg.ZoektStorageLayout)
	}
}
