// Package gitclone is the real git-clone module the INDEX
// pipeline (Phase C.4) builds on. Given a clone URL + optional
// credentials, it clones (or fetches) the repository into the
// caller-supplied destination directory and returns the actual
// HEAD commit SHA + branch name observed on disk.
//
// Direct port of the slice of legacy cloneRepo / fetchRepo
// (packages/backend/src/repoIndexManager.ts:707-787) that
// produces a working tree the indexer can read. Uses the go-git
// pure-Go library — no shell-out to the git binary so the
// codeintel-indexer can run in a slim container without a
// system git install.
//
// Wired into the INDEX dispatch path
// (internal/backend/repoindexmanager/handler.go:dispatchIndex)
// as of Phase C.4a — given a Repo row, the worker clones via
// this module and stamps the observed HEAD on
// Repo.indexedCommitHash. Zoekt + SCIP layers (C.4b / C.4c)
// compose on top.
package gitclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// Credentials holds the auth knobs go-git's HTTP transport
// understands. Caller is responsible for resolving these from
// the connection's secret refs (the secret-refs layer landed in
// an earlier slice).
//
// Either Username+Password (legacy git-over-https) or
// Username+OAuthToken (GitHub-style PAT in the password slot)
// works. An empty Credentials value performs an anonymous clone
// — fine for public repos.
type Credentials struct {
	Username string
	Password string
}

// Empty reports whether no credentials were supplied. Used by
// Clone to skip the AuthMethod wiring entirely so anonymous
// flows don't pay the cost (and don't accidentally send empty
// Basic-Auth headers that some servers reject).
func (c Credentials) Empty() bool {
	return c.Username == "" && c.Password == ""
}

// Request is the input to Clone. Embeds everything the caller
// needs to set so a future config-driven call site doesn't have
// to thread positional parameters.
type Request struct {
	// CloneURL is the remote URL. file:// is supported for
	// hermetic tests + local-only repos; https:// hits the wire.
	CloneURL string

	// Destination is the on-disk path the working tree lands at.
	// Caller is responsible for ensuring the parent dir exists
	// and is writable; Clone does NOT create it.
	Destination string

	// Branch, when non-empty, narrows the clone to a single ref
	// (the legacy behaviour for single-branch indexing). Empty
	// means "clone the remote default branch" — mirrors a
	// vanilla `git clone <url>` call.
	Branch string

	// Depth, when > 0, requests a shallow clone (--depth N). 0
	// performs a full clone. The indexer typically uses 1 for
	// "snapshot at HEAD" workflows.
	Depth int

	// Credentials carries the auth tokens. Optional — anonymous
	// clones use a zero value.
	Credentials Credentials
}

// Result is what Clone reports back. CommitHash is the actual
// SHA-1 the working tree resolved to (the value the caller
// stamps into Repo.indexedCommitHash). Branch is the ref name
// HEAD resolves to (so the caller can record the default-branch
// the remote currently advertises, useful when Request.Branch
// was empty).
type Result struct {
	CommitHash string
	Branch     string
	WorkTree   string
}

// ErrInvalidCloneURL is returned when the supplied CloneURL is
// not a parseable URL with a supported scheme (file, http,
// https). Callers should treat it as a permanent failure —
// retry won't help.
var ErrInvalidCloneURL = errors.New("gitclone: invalid CloneURL")

// ErrDestinationNotEmpty signals the caller passed a path that
// already contains files. Clone refuses to mix into an existing
// directory — the safe behaviour is to remove it first
// (REMOVE_INDEX handles that) and re-clone fresh.
var ErrDestinationNotEmpty = errors.New("gitclone: destination directory is not empty")

// Clone clones the repository identified by req into
// req.Destination and returns the observed HEAD SHA + branch.
// The implementation:
//
//  1. Validates inputs (URL scheme, destination empty).
//  2. Builds the go-git CloneOptions struct, applying
//     credentials only when non-empty.
//  3. Calls go-git's PlainCloneContext (cancellation-aware).
//  4. Resolves HEAD on the resulting repo to capture the
//     observed commit + branch name.
//
// This function is deterministic given the same remote state —
// a re-clone of the same remote at the same commit produces a
// byte-identical working tree (modulo file mtimes, which the
// indexer never reads). That matches the legacy parity rule.
func Clone(ctx context.Context, req Request) (Result, error) {
	if err := validateRequest(req); err != nil {
		return Result{}, err
	}

	attempts := cloneAttempts()
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		res, err := cloneOnce(ctx, req)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if attempt == attempts || !isRetryableCloneError(err) || ctx.Err() != nil {
			break
		}
		if removeErr := os.RemoveAll(req.Destination); removeErr != nil {
			return Result{}, fmt.Errorf("gitclone: clear partial destination before retry: %w", removeErr)
		}
		if mkdirErr := os.MkdirAll(filepath.Dir(req.Destination), 0o755); mkdirErr != nil {
			return Result{}, fmt.Errorf("gitclone: mkdir destination parent before retry: %w", mkdirErr)
		}
		timer := time.NewTimer(time.Duration(attempt) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Result{}, ctx.Err()
		case <-timer.C:
		}
	}
	return Result{}, lastErr
}

func cloneOnce(ctx context.Context, req Request) (Result, error) {
	opts := &git.CloneOptions{
		URL: req.CloneURL,
	}
	if req.Branch != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(req.Branch)
		opts.SingleBranch = true
	}
	if req.Depth > 0 {
		opts.Depth = req.Depth
	}
	if !req.Credentials.Empty() {
		opts.Auth = &http.BasicAuth{
			Username: req.Credentials.Username,
			Password: req.Credentials.Password,
		}
	}

	repo, err := git.PlainCloneContext(ctx, req.Destination, false, opts)
	if err != nil {
		if req.Credentials.Empty() {
			if fallback, fallbackErr := cloneWithGitCLI(ctx, req, err); fallbackErr == nil {
				return fallback, nil
			} else {
				return Result{}, fmt.Errorf("gitclone: PlainCloneContext: %w; git fallback: %v", err, fallbackErr)
			}
		}
		return Result{}, fmt.Errorf("gitclone: PlainCloneContext: %w", err)
	}

	head, err := repo.Head()
	if err != nil {
		return Result{}, fmt.Errorf("gitclone: resolve HEAD: %w", err)
	}

	return Result{
		CommitHash: head.Hash().String(),
		Branch:     headBranchName(head),
		WorkTree:   req.Destination,
	}, nil
}

func cloneAttempts() int {
	raw := strings.TrimSpace(os.Getenv("CODEINTEL_GIT_CLONE_ATTEMPTS"))
	if raw == "" {
		return 3
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		return 3
	}
	if parsed > 8 {
		return 8
	}
	return parsed
}

func isRetryableCloneError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"unexpected eof",
		"connection reset",
		"connection refused",
		"connection timed out",
		"tls handshake timeout",
		"temporary failure",
		"server misbehaving",
		"stream error",
		"early eof",
		"remote error",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func cloneWithGitCLI(ctx context.Context, req Request, goGitErr error) (Result, error) {
	if err := os.RemoveAll(req.Destination); err != nil {
		return Result{}, fmt.Errorf("gitclone: clear partial destination before git fallback: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(req.Destination), 0o755); err != nil {
		return Result{}, fmt.Errorf("gitclone: mkdir destination parent before git fallback: %w", err)
	}
	stderr, err := runGitClone(ctx, req, req.Depth)
	if err != nil && req.Depth > 0 && isDumbHTTPShallowUnsupported(stderr) {
		if removeErr := os.RemoveAll(req.Destination); removeErr != nil {
			return Result{}, fmt.Errorf("gitclone: clear partial destination before full git fallback: %w", removeErr)
		}
		if mkdirErr := os.MkdirAll(filepath.Dir(req.Destination), 0o755); mkdirErr != nil {
			return Result{}, fmt.Errorf("gitclone: mkdir destination parent before full git fallback: %w", mkdirErr)
		}
		stderr, err = runGitClone(ctx, req, 0)
	}
	if err != nil {
		_ = os.RemoveAll(req.Destination)
		return Result{}, fmt.Errorf("gitclone: git fallback after go-git error %q failed: %w: %s", goGitErr, err, strings.TrimSpace(stderr.String()))
	}
	res, err := OpenHead(req.Destination)
	if err != nil {
		return Result{}, fmt.Errorf("gitclone: git fallback resolve HEAD: %w", err)
	}
	return res, nil
}

func runGitClone(ctx context.Context, req Request, depth int) (bytes.Buffer, error) {
	args := gitCloneArgs(req, depth)
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return stderr, cmd.Run()
}

func gitCloneArgs(req Request, depth int) []string {
	args := []string{"clone"}
	if req.Branch != "" {
		args = append(args, "--branch", strings.TrimPrefix(req.Branch, "refs/heads/"), "--single-branch")
	}
	if depth > 0 {
		args = append(args, "--depth", fmt.Sprint(depth))
	}
	args = append(args, req.CloneURL, req.Destination)
	return args
}

func isDumbHTTPShallowUnsupported(stderr bytes.Buffer) bool {
	msg := strings.ToLower(stderr.String())
	return strings.Contains(msg, "dumb http") && strings.Contains(msg, "shallow")
}

func validateRequest(req Request) error {
	if req.CloneURL == "" {
		return fmt.Errorf("%w: empty CloneURL", ErrInvalidCloneURL)
	}
	u, err := url.Parse(req.CloneURL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCloneURL, err)
	}
	switch u.Scheme {
	case "file", "http", "https", "ssh":
		// ok
	default:
		return fmt.Errorf("%w: unsupported scheme %q", ErrInvalidCloneURL, u.Scheme)
	}
	if req.Destination == "" {
		return fmt.Errorf("gitclone: empty Destination")
	}
	// Empty-or-missing is acceptable; non-empty is not.
	entries, err := os.ReadDir(req.Destination)
	if err == nil && len(entries) > 0 {
		return fmt.Errorf("%w: %s", ErrDestinationNotEmpty, req.Destination)
	}
	return nil
}

// OpenHead reads HEAD on an existing working tree at path
// without re-cloning. Used by the INDEX dispatch path for the
// read-only file:// case where the working tree is owned by
// the operator and codeintel just observes its current state.
//
// Returns Result populated with CommitHash, Branch, and
// WorkTree (= path). Errors out if path is not a git repo or
// HEAD can't be resolved (e.g. the repo has no commits).
func OpenHead(path string) (Result, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return Result{}, fmt.Errorf("gitclone: PlainOpen %s: %w", path, err)
	}
	head, err := repo.Head()
	if err != nil {
		return Result{}, fmt.Errorf("gitclone: resolve HEAD in %s: %w", path, err)
	}
	return Result{
		CommitHash: head.Hash().String(),
		Branch:     headBranchName(head),
		WorkTree:   path,
	}, nil
}

func headBranchName(head *plumbing.Reference) string {
	name := head.Name()
	if name.IsBranch() {
		return name.Short()
	}
	// Detached HEAD case (post-clone with --depth 1 sometimes
	// lands in this state). Surface the symbolic name so the
	// caller has something to log; the indexer treats a non-
	// branch HEAD as "default branch" semantically.
	return strings.TrimPrefix(string(name), "refs/heads/")
}
