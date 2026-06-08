// Generic git host compile + fetcher. Direct port of
// compileGenericGitHostConfig (repoCompileUtils.ts:574-706).
//
// Two branches share the entrypoint:
//   - file://  -> glob local paths, validate each as a git
//     repo, read remote.origin.url + derive
//     repoName from origin's host + path.
//   - http(s)://-> single remote URL validated via an
//     ls-remote-style HTTP probe; one Repo per
//     config.url.
//
// Generic git's wire shape (legacy):
//   - external_codeHostType = "generic-git-host" in Postgres
//     (`genericGitHost` is the legacy Prisma enum member name).
//   - file:// emits NO gitConfig block (legacy comment: local
//     repos are read-only).
//   - http(s):// emits a 6-key gitConfig with the URL itself
//     as zoekt.web-url + an isArchived/isFork/isPublic=true
//     defaults block.
//   - defaultBranch is nil for http(s):// (the indexer fills it
//     on clone) but populated for file:// (read from .git/HEAD
//     or HEAD config).
//   - Schemes outside {file, http, https} return an error.
package connectionmanager

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// GenericGitHostConnectionConfig mirrors the legacy
// GenericGitHostConnectionConfig (@schemas/v3/genericGitHost.type).
type GenericGitHostConnectionConfig struct {
	URL       string           `json:"url"` // REQUIRED; file:// or http(s)://
	Revisions *GitHubRevisions `json:"revisions,omitempty"`
}

const genericGitHostCodeHostType = "generic-git-host"

// ErrGenericGitUnsupportedScheme is returned for schemes
// outside {file, http, https}.
var ErrGenericGitUnsupportedScheme = errors.New("connectionmanager: generic git host unsupported scheme")

// CompileGenericGitHostFromConfig is the dispatch entrypoint
// the worker handler calls when conn.ConnectionType == "git".
// Routes on URL scheme to one of the two branches.
func CompileGenericGitHostFromConfig(ctx context.Context, cfg GenericGitHostConnectionConfig, connectionID int32) ([]RepoData, []string, error) {
	if cfg.URL == "" {
		return nil, nil, errors.New("connectionmanager: generic git host config.url is required")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("parse config.url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "file":
		return compileGenericGitFile(ctx, cfg, u, connectionID)
	case "http", "https":
		return compileGenericGitURL(ctx, cfg, u, connectionID)
	default:
		return nil, nil, fmt.Errorf("%w: %q", ErrGenericGitUnsupportedScheme, u.Scheme)
	}
}

// =====================================================================
// http(s):// branch
// =====================================================================

// compileGenericGitURL handles the single-remote-URL branch.
// Validates the URL is a git repo via an ls-info HTTP probe,
// then emits one RepoData. Legacy lines 708-775.
func compileGenericGitURL(ctx context.Context, cfg GenericGitHostConnectionConfig, u *url.URL, connectionID int32) ([]RepoData, []string, error) {
	if !IsURLAValidGitRepo(ctx, http.DefaultClient, u.String()) {
		return nil, []string{fmt.Sprintf("Skipping %s - not a git repository.", u.String())}, nil
	}

	// repoName = host + pathname (with .git suffix stripped).
	repoName := path.Join(u.Host, strings.TrimSuffix(u.Path, ".git"))

	rec := RepoData{
		ExternalID:           u.String(),
		ExternalCodeHostType: genericGitHostCodeHostType,
		// origin = scheme + "://" + host (legacy uses URL.origin
		// which is exactly that).
		ExternalCodeHostURL: u.Scheme + "://" + u.Host,
		CloneURL:            u.String(),
		Name:                repoName,
		DisplayName:         repoName,
		// defaultBranch nil by design.
		IsFork:     false,
		IsArchived: false,
		IsPublic:   true,
		OrgID:      SingleTenantOrgID,
		Metadata: RepoMetadata{
			GitConfig: map[string]string{
				"zoekt.name":         repoName,
				"zoekt.web-url":      u.String(),
				"zoekt.archived":     marshalBoolValue(false),
				"zoekt.fork":         marshalBoolValue(false),
				"zoekt.public":       marshalBoolValue(true),
				"zoekt.display-name": repoName,
			},
		},
	}
	if cfg.Revisions != nil {
		rec.Metadata.Branches = cfg.Revisions.Branches
		rec.Metadata.Tags = cfg.Revisions.Tags
	}
	rec.ConnectionIDs = []int32{connectionID}
	return []RepoData{rec}, nil, nil
}

// IsURLAValidGitRepo probes a remote URL with the
// `?service=git-upload-pack` info refs endpoint. A real git
// HTTP server responds 200 + a body starting with the git-smart
// protocol prefix. Static/dumb HTTP repos generated with
// `git update-server-info` respond 200 with a plain `info/refs`
// table, so the port accepts that deterministic shape too.
// Other URLs respond 404 / 200-with-HTML and are rejected.
//
// Direct port of git.ts:isUrlAValidGitRepo. Returns false on
// transport error / non-2xx / wrong content-type.
func IsURLAValidGitRepo(ctx context.Context, httpClient *http.Client, rawURL string) bool {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	probeURL := strings.TrimRight(rawURL, "/") + "/info/refs?service=git-upload-pack"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/x-git-upload-pack-advertisement")
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "git-upload-pack-advertisement") ||
		strings.Contains(ct, "x-git-upload-pack") {
		return true
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8192))
	if err != nil {
		return false
	}
	return looksLikeDumbHTTPInfoRefs(string(data))
}

func looksLikeDumbHTTPInfoRefs(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		oid, ref := fields[0], fields[1]
		if !isHexObjectID(oid) {
			continue
		}
		if ref == "HEAD" || strings.HasPrefix(ref, "refs/") {
			return true
		}
	}
	return false
}

func isHexObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

// =====================================================================
// file:// branch
// =====================================================================

// compileGenericGitFile handles the glob-local-paths branch.
// Each matched path is validated as a git repo root, its
// remote.origin.url is read for the canonical repoName + cloneURL
// gets file:// pointing at the local path.
func compileGenericGitFile(ctx context.Context, cfg GenericGitHostConnectionConfig, u *url.URL, connectionID int32) ([]RepoData, []string, error) {
	pattern := u.Path
	if pattern == "" {
		return nil, []string{"file:// URL has empty path"}, nil
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, nil, fmt.Errorf("glob %q: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, []string{fmt.Sprintf("No paths matched the pattern '%s'. Please verify the path exists and is accessible.", pattern)}, nil
	}

	var (
		repos    []RepoData
		warnings []string
	)
	for _, p := range matches {
		if err := ctx.Err(); err != nil {
			return nil, warnings, fmt.Errorf("%w: %v", ErrFetchAborted, err)
		}
		stat, statErr := os.Stat(p)
		if statErr != nil || !stat.IsDir() {
			warnings = append(warnings, fmt.Sprintf("Skipping %s - path is not a directory.", p))
			continue
		}
		if !IsPathAValidGitRepoRoot(p) {
			warnings = append(warnings, fmt.Sprintf("Skipping %s - not a git repository.", p))
			continue
		}
		origin := GetOriginURL(p)
		if origin == "" {
			warnings = append(warnings, fmt.Sprintf("Skipping %s - remote.origin.url not found in git config.", p))
			continue
		}
		repoName := genericGitRepoNameFromOrigin(origin)
		defaultBranch := GetLocalDefaultBranch(p)

		rec := RepoData{
			ExternalID:           origin,
			ExternalCodeHostType: genericGitHostCodeHostType,
			ExternalCodeHostURL:  genericGitResource(origin),
			CloneURL:             "file://" + p,
			Name:                 repoName,
			DisplayName:          repoName,
			DefaultBranch:        defaultBranch,
			IsFork:               false,
			IsArchived:           false,
			OrgID:                SingleTenantOrgID,
			// IsPublic stays false for local repos - the legacy
			// comment says local is read-only; we surface that
			// as "not public" so the wire metadata stays
			// internally consistent. (Legacy didn't set isPublic
			// for the file branch either - the field defaults
			// to false on the Prisma side.)
			Metadata: RepoMetadata{
				// gitConfig intentionally nil - local repos read-only.
			},
		}
		if cfg.Revisions != nil {
			rec.Metadata.Branches = cfg.Revisions.Branches
			rec.Metadata.Tags = cfg.Revisions.Tags
		}
		rec.ConnectionIDs = []int32{connectionID}
		repos = append(repos, rec)
	}

	if len(repos) == 0 {
		warnings = append(warnings, fmt.Sprintf(
			"No valid git repositories found from %d matched path(s). Check the warnings for details on individual paths.",
			len(matches)))
	}
	return repos, warnings, nil
}

// IsPathAValidGitRepoRoot returns true if the supplied path
// contains a .git directory OR a .git file (worktree). Direct
// port of git.ts:isPathAValidGitRepoRoot.
func IsPathAValidGitRepoRoot(repoPath string) bool {
	dotGit := filepath.Join(repoPath, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return false
	}
	// Either a regular .git dir OR a .git file pointing at a
	// worktree counts as a valid git repo root.
	return info.IsDir() || info.Mode().IsRegular()
}

// GetOriginURL reads .git/config and extracts the
// remote.origin.url value. Returns "" on any I/O or parse
// error. Hand-rolled parser since git config is a small
// INI-like format and we only need one specific value.
func GetOriginURL(repoPath string) string {
	cfgPath := filepath.Join(repoPath, ".git", "config")
	// If .git is a file (worktree pointer), follow it. Simpler:
	// also accept a direct .git directory.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		// Try gitdir indirection for worktree pointers.
		alt, altErr := readWorktreeGitConfig(filepath.Join(repoPath, ".git"))
		if altErr != nil {
			return ""
		}
		data = alt
	}
	return parseGitConfigOriginURL(string(data))
}

// parseGitConfigOriginURL parses git config text and returns the
// value of remote."origin".url. Tolerant of leading whitespace,
// blank lines, comments, and case in the section header.
func parseGitConfigOriginURL(cfg string) string {
	inSection := false
	for _, line := range strings.Split(cfg, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, ";") || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasPrefix(trim, "[") {
			inSection = strings.EqualFold(trim, `[remote "origin"]`)
			continue
		}
		if !inSection {
			continue
		}
		// Look for key = value where key is "url".
		eq := strings.Index(trim, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(trim[:eq])
		if !strings.EqualFold(key, "url") {
			continue
		}
		val := strings.TrimSpace(trim[eq+1:])
		return val
	}
	return ""
}

// readWorktreeGitConfig handles the case where .git is a file
// containing "gitdir: /actual/path". Returns the config at the
// referenced path.
func readWorktreeGitConfig(dotGitPath string) ([]byte, error) {
	info, err := os.Stat(dotGitPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a worktree pointer")
	}
	contents, err := os.ReadFile(dotGitPath)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(contents), "\n") {
		trim := strings.TrimSpace(line)
		const prefix = "gitdir:"
		if strings.HasPrefix(trim, prefix) {
			target := strings.TrimSpace(strings.TrimPrefix(trim, prefix))
			return os.ReadFile(filepath.Join(target, "config"))
		}
	}
	return nil, errors.New("worktree pointer missing gitdir:")
}

// GetLocalDefaultBranch reads HEAD to derive the default branch
// name. Returns nil pointer on error / detached HEAD. Format:
// `ref: refs/heads/<branch>` or a raw 40-char SHA for detached.
func GetLocalDefaultBranch(repoPath string) *string {
	headPath := filepath.Join(repoPath, ".git", "HEAD")
	data, err := os.ReadFile(headPath)
	if err != nil {
		// Worktree indirection.
		alt, altErr := readWorktreeHEAD(filepath.Join(repoPath, ".git"))
		if altErr != nil {
			return nil
		}
		data = alt
	}
	line := strings.TrimSpace(string(data))
	const refPrefix = "ref:"
	if !strings.HasPrefix(line, refPrefix) {
		// Detached HEAD - no symbolic name available.
		return nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, refPrefix))
	// Strip the "refs/heads/" prefix if present.
	branch := strings.TrimPrefix(rest, "refs/heads/")
	return &branch
}

func readWorktreeHEAD(dotGitPath string) ([]byte, error) {
	info, err := os.Stat(dotGitPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("not a worktree pointer")
	}
	contents, err := os.ReadFile(dotGitPath)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(contents), "\n") {
		trim := strings.TrimSpace(line)
		const prefix = "gitdir:"
		if strings.HasPrefix(trim, prefix) {
			target := strings.TrimSpace(strings.TrimPrefix(trim, prefix))
			return os.ReadFile(filepath.Join(target, "HEAD"))
		}
	}
	return nil, errors.New("worktree pointer missing gitdir:")
}

// genericGitRepoNameFromOrigin derives a normalised repoName
// from a remote origin URL. The legacy uses GitUrlParse(origin)
// + decodeURIComponent(pathname) + strip-.git-suffix +
// path.join(host, pathname). The Go port mirrors via url.Parse +
// PathUnescape + path.Join.
//
// Falls back to the raw URL if parsing fails (better than
// throwing - the row still gets emitted with a less-clean
// name).
func genericGitRepoNameFromOrigin(origin string) string {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		// SSH-style git@host:path/repo.git URLs aren't valid
		// url.URL inputs. Try the SSH form parse.
		if host, p, ok := parseSSHGitURL(origin); ok {
			return path.Join(host, strings.TrimSuffix(p, ".git"))
		}
		return origin
	}
	decoded, decErr := url.PathUnescape(u.Path)
	if decErr != nil {
		decoded = u.Path
	}
	return path.Join(u.Host, strings.TrimSuffix(decoded, ".git"))
}

// parseSSHGitURL handles `git@host:path/repo.git` style URLs
// (most local-clones-of-github use this form). Returns (host,
// path, ok).
func parseSSHGitURL(raw string) (string, string, bool) {
	at := strings.Index(raw, "@")
	colon := strings.Index(raw, ":")
	if at < 0 || colon < at {
		return "", "", false
	}
	host := raw[at+1 : colon]
	p := raw[colon+1:]
	if host == "" || p == "" {
		return "", "", false
	}
	return host, p, true
}

// genericGitResource derives the "resource" host string from
// the origin URL - legacy uses GitUrlParse(origin).resource
// which for HTTP URLs is the hostname.
func genericGitResource(origin string) string {
	u, err := url.Parse(origin)
	if err == nil && u.Host != "" {
		return u.Host
	}
	if host, _, ok := parseSSHGitURL(origin); ok {
		return host
	}
	return origin
}
