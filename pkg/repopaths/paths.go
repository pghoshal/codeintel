// Package repopaths resolves the on-disk paths used by the
// repo-index worker: the per-repo clone directory and the Zoekt
// shard index directory. Direct port of the legacy shared utils
// helpers (getRepoPath, getZoektIndexPathForRepo,
// getZoektOrgRootPath, getZoektOrgIndexPath, getZoektOrgReposPath)
// from packages/shared/src/utils.ts.
//
// Tenant isolation note: when the legacy storage-layout env was
// set to "org-directory", every per-tenant repo lived under
// .../zoekt-orgs/<orgId>/repos/<repoId>. The Go port keeps the
// same shape under CODEINTEL_ZOEKT_STORAGE_LAYOUT=org-directory,
// so existing on-disk layouts survive the rename.
//
// Env vars (codeintel rename of the legacy brand-prefixed keys):
//
//	CODEINTEL_DATA_CACHE_DIR      — base cache root. Required when
//	                                ZOEKT_STORAGE_LAYOUT is unset
//	                                or != "org-directory".
//	CODEINTEL_ZOEKT_EFS_ROOT      — explicit zoekt-orgs root. When
//	                                set, used in place of
//	                                <DATA_CACHE_DIR>/zoekt-orgs.
//	CODEINTEL_ZOEKT_STORAGE_LAYOUT — "org-directory" enables the
//	                                per-org layout; any other
//	                                value (or unset) selects the
//	                                flat <DATA_CACHE_DIR>/repos +
//	                                <DATA_CACHE_DIR>/index layout.
package repopaths

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

// StorageLayoutOrgDirectory is the legacy magic string that
// selects the per-org layout. Anything else (including the empty
// string) means the flat layout.
const StorageLayoutOrgDirectory = "org-directory"

// Config holds the env-derived storage roots. Build it once at
// process boot and reuse for every cleanup call.
type Config struct {
	DataCacheDir       string
	ZoektEFSRoot       string
	ZoektStorageLayout string
}

// LoadConfigFromEnv reads the three CODEINTEL_* env vars listed in
// the package doc. DataCacheDir defaults to "./data" when unset
// (matches legacy behaviour). The other two are optional.
func LoadConfigFromEnv() Config {
	dataCacheDir := os.Getenv("CODEINTEL_DATA_CACHE_DIR")
	if dataCacheDir == "" {
		dataCacheDir = "./data"
	}
	return Config{
		DataCacheDir:       dataCacheDir,
		ZoektEFSRoot:       os.Getenv("CODEINTEL_ZOEKT_EFS_ROOT"),
		ZoektStorageLayout: os.Getenv("CODEINTEL_ZOEKT_STORAGE_LAYOUT"),
	}
}

// Repo is the minimum repo shape required to resolve paths.
// Sourced from the Repo Postgres row at the caller.
type Repo struct {
	OrgID        int32
	RepoID       int32
	CloneURL     string
	CodeHostType string
}

// ErrInvalidCloneURL is returned by RepoPath when the Repo's
// CloneURL is set but does not parse. Cleanup callers can choose
// to skip filesystem work in this case rather than fail the job
// (the DB cleanup is still safe to run).
var ErrInvalidCloneURL = errors.New("repopaths: invalid CloneURL")

// RepoPath resolves the on-disk clone directory for the repo and
// reports whether the path is read-only.
//
// Mirror of getRepoPath (packages/shared/src/utils.ts:125):
//
//   - genericGitHost/generic-git-host + file:// CloneURL -> CloneURL.pathname, read-only
//   - ZoektStorageLayout=="org-directory" ->
//     <ZoektEFSRoot|DataCacheDir/zoekt-orgs>/<orgId>/repos/<repoId>
//   - default -> <DataCacheDir>/repos/<repoId>
func (c Config) RepoPath(r Repo) (string, bool, error) {
	if (r.CodeHostType == "genericGitHost" || r.CodeHostType == "generic-git-host") && r.CloneURL != "" {
		u, err := url.Parse(r.CloneURL)
		if err != nil {
			return "", false, ErrInvalidCloneURL
		}
		if u.Scheme == "file" {
			// url.URL.Path is the pathname after file:// (e.g.
			// file:///repos/foo -> "/repos/foo"). On Windows it
			// strips the leading slash; codeintel doesn't ship a
			// Windows build, so the POSIX semantics match
			// legacy 1:1.
			return u.Path, true, nil
		}
	}

	var reposRoot string
	if c.ZoektStorageLayout == StorageLayoutOrgDirectory {
		reposRoot = c.zoektOrgReposPath(r.OrgID)
	} else {
		reposRoot = filepath.Join(c.DataCacheDir, "repos")
	}
	return filepath.Join(reposRoot, strconv.Itoa(int(r.RepoID))), false, nil
}

// ZoektIndexPath returns the directory holding Zoekt shard
// files for the repo's org. Direct port of
// getZoektIndexPathForRepo (utils.ts:117).
func (c Config) ZoektIndexPath(r Repo) string {
	if c.ZoektStorageLayout == StorageLayoutOrgDirectory {
		return c.zoektOrgIndexPath(r.OrgID)
	}
	return filepath.Join(c.DataCacheDir, "index")
}

// RevisionSnapshotPath returns the immutable materialized commit tree used
// by split SCIP/AST executor pods. The Rust executor uses the same shape so
// backend-side artifact ingestion can read source lines without guessing.
func (c Config) RevisionSnapshotPath(orgID, repoID int32, commitHash string) string {
	return filepath.Join(c.RevisionSnapshotRepoRoot(orgID, repoID), commitHash)
}

func (c Config) RevisionSnapshotRepoRoot(orgID, repoID int32) string {
	var base string
	if c.ZoektStorageLayout == StorageLayoutOrgDirectory {
		base = c.zoektOrgRootPath(orgID)
	} else {
		base = c.DataCacheDir
	}
	return filepath.Join(base, "codeintel", "revision-snapshots", strconv.Itoa(int(repoID)))
}

// ShardPrefix mirrors the legacy getShardPrefix (utils.ts:81):
//
//	"<orgId>_<repoId>"
//
// Used to filter files inside ZoektIndexPath to those that
// belong to a single repo, since Zoekt drops every org's shards
// in the same dir under the flat layout. The actual shard file
// names take the form "<orgId>_<repoId>_<branch>_<seq>.zoekt".
func ShardPrefix(orgID, repoID int32) string {
	return strconv.Itoa(int(orgID)) + "_" + strconv.Itoa(int(repoID))
}

// ShardFilePrefix returns the delimiter-safe prefix for actual
// shard filenames. Use this for deletion/filtering so repo 2
// never matches repo 23 in the same org directory.
func ShardFilePrefix(orgID, repoID int32) string {
	return ShardPrefix(orgID, repoID) + "_"
}

func (c Config) zoektOrgRootPath(orgID int32) string {
	if c.ZoektEFSRoot != "" {
		return filepath.Join(c.ZoektEFSRoot, strconv.Itoa(int(orgID)))
	}
	return filepath.Join(c.DataCacheDir, "zoekt-orgs", strconv.Itoa(int(orgID)))
}

func (c Config) zoektOrgIndexPath(orgID int32) string {
	return filepath.Join(c.zoektOrgRootPath(orgID), "index")
}

func (c Config) zoektOrgReposPath(orgID int32) string {
	return filepath.Join(c.zoektOrgRootPath(orgID), "repos")
}
