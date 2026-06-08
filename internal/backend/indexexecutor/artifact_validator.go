package indexexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"codeintel/internal/backend/indexsubjobtask"
)

const maxArtifactBytes = 256 << 20

type ArtifactValidator interface {
	ValidateAndPublish(context.Context, indexsubjobtask.Payload, Result) (Result, error)
}

type FilesystemArtifactValidator struct {
	root string
}

func NewFilesystemArtifactValidator(root string) (*FilesystemArtifactValidator, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("indexexecutor: artifact root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("indexexecutor: artifact root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("indexexecutor: create artifact root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("indexexecutor: resolve artifact root: %w", err)
	}
	return &FilesystemArtifactValidator{root: filepath.Clean(resolved)}, nil
}

func (v *FilesystemArtifactValidator) ValidateAndPublish(ctx context.Context, payload indexsubjobtask.Payload, result Result) (Result, error) {
	if v == nil || v.root == "" {
		return Result{}, errors.New("artifact validator is not configured")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !hasCompleteArtifact(result) {
		return Result{}, errors.New("executor returned incomplete artifact metadata")
	}
	tempPath, err := v.validateScopedExistingPath(payload, result.ArtifactTempPath)
	if err != nil {
		return Result{}, fmt.Errorf("temp artifact path: %w", err)
	}
	finalPath, err := v.validateScopedTargetPath(payload, result.ArtifactPath)
	if err != nil {
		return Result{}, fmt.Errorf("final artifact path: %w", err)
	}
	if tempPath == finalPath {
		return Result{}, errors.New("temp and final artifact paths must be different")
	}
	gotSHA, err := sha256File(ctx, tempPath)
	if err != nil {
		return Result{}, err
	}
	wantSHA := normalizeSHA256(result.ArtifactSHA256)
	if wantSHA == "" {
		return Result{}, errors.New("artifact SHA-256 is required")
	}
	if gotSHA != wantSHA {
		return Result{}, fmt.Errorf("artifact SHA-256 mismatch: got %s want %s", gotSHA, wantSHA)
	}
	if err := v.ensureParentSafe(filepath.Dir(finalPath)); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("mkdir final artifact dir: %w", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return Result{}, fmt.Errorf("publish artifact: %w", err)
	}
	gotFinalSHA, err := sha256File(ctx, finalPath)
	if err != nil {
		return Result{}, err
	}
	if gotFinalSHA != gotSHA {
		return Result{}, fmt.Errorf("published artifact SHA-256 changed: got %s want %s", gotFinalSHA, gotSHA)
	}
	result.ArtifactTempPath = tempPath
	result.ArtifactPath = finalPath
	result.ArtifactSHA256 = "sha256:" + gotSHA
	return result, nil
}

func (v *FilesystemArtifactValidator) validateScopedExistingPath(payload indexsubjobtask.Payload, raw string) (string, error) {
	abs, err := canonicalExistingPath(raw)
	if err != nil {
		return "", err
	}
	if _, err := v.validateScopedPathLexical(payload, abs); err != nil {
		return "", err
	}
	return abs, nil
}

func (v *FilesystemArtifactValidator) validateScopedTargetPath(payload indexsubjobtask.Payload, raw string) (string, error) {
	abs, err := canonicalTargetPath(raw)
	if err != nil {
		return "", err
	}
	if _, err := v.validateScopedPathLexical(payload, abs); err != nil {
		return "", err
	}
	return abs, nil
}

func canonicalExistingPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink", abs)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", abs)
	}
	if info.Size() > maxArtifactBytes {
		return "", fmt.Errorf("%s is %d bytes, exceeds %d byte artifact limit", abs, info.Size(), maxArtifactBytes)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func canonicalTargetPath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symlink", abs)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s is a directory", abs)
		}
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s is not a regular file", abs)
		}
		if info.Size() > maxArtifactBytes {
			return "", fmt.Errorf("%s is %d bytes, exceeds %d byte artifact limit", abs, info.Size(), maxArtifactBytes)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return filepath.Clean(resolved), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	var missing []string
	current := abs
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("%s is a symlink", current)
			}
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", current)
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("no existing parent for %s", abs)
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (v *FilesystemArtifactValidator) validateScopedPathLexical(payload indexsubjobtask.Payload, raw string) (string, error) {
	if payload.WorkspaceID == nil || strings.TrimSpace(*payload.WorkspaceID) == "" || strings.TrimSpace(payload.Branch) == "" {
		return "", errors.New("payload workspaceId and branch are required for artifact scope")
	}
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("path is required")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(v.root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes artifact root %s", abs, v.root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 5 {
		return "", fmt.Errorf("%s does not include org/repo/workspace/branch/commit scope", abs)
	}
	if parts[0] != strconv.FormatInt(int64(payload.OrgID), 10) ||
		parts[1] != strconv.FormatInt(int64(payload.RepoID), 10) ||
		parts[2] != artifactScopeSegment(*payload.WorkspaceID) ||
		parts[3] != artifactScopeSegment(payload.Branch) ||
		parts[4] != payload.CommitHash {
		return "", fmt.Errorf("%s is outside payload scope org=%d repo=%d workspace=%s branch=%s commit=%s", abs, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Branch, payload.CommitHash)
	}
	return abs, nil
}

func artifactScopeSegment(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "s-" + hex.EncodeToString(sum[:])[:16]
}

func (v *FilesystemArtifactValidator) ensureParentSafe(dir string) error {
	dir = filepath.Clean(dir)
	rel, err := filepath.Rel(v.root, dir)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s escapes artifact root %s", dir, v.root)
	}
	current := v.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", current)
		}
	}
	return nil
}

func sha256File(ctx context.Context, path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("lstat artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("artifact %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("artifact %s is not a regular file", path)
	}
	if info.Size() > maxArtifactBytes {
		return "", fmt.Errorf("artifact %s is %d bytes, exceeds %d byte limit", path, info.Size(), maxArtifactBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open artifact: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx: ctx, r: f}); err != nil {
		return "", fmt.Errorf("hash artifact: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if r.ctx != nil {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
	}
	return r.r.Read(p)
}

func normalizeSHA256(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != 64 {
		return ""
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ""
		}
	}
	return value
}
