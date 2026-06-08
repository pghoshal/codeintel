// Package zoektartifact validates split-executor Zoekt shard
// artifacts before a ZOEKT subjob can be marked successful.
package zoektartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"codeintel/internal/backend/indexexecutor"
	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/pkg/repopaths"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const maxZoektArtifactBytes = 32 << 20

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type Store struct {
	db           querier
	pathsCfg     repopaths.Config
	artifactRoot string
	readFile     func(string) ([]byte, error)
	hashFile     func(context.Context, string) (string, error)
}

func NewStore(db querier, pathsCfg repopaths.Config, artifactRoot string) (*Store, error) {
	if db == nil {
		return nil, errors.New("zoektartifact: database is required")
	}
	resolvedRoot, err := resolveArtifactRoot(artifactRoot)
	if err != nil {
		return nil, err
	}
	return &Store{
		db:           db,
		pathsCfg:     pathsCfg,
		artifactRoot: resolvedRoot,
	}, nil
}

func (s *Store) Ingest(ctx context.Context, payload indexsubjobtask.Payload, result indexexecutor.Result, leaseOwner, attemptID string) error {
	if s == nil || s.db == nil {
		return errors.New("zoektartifact: store is not configured")
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("zoektartifact: payload: %w", err)
	}
	if payload.Layer != indexsubjobtask.LayerZoekt {
		return fmt.Errorf("zoektartifact: unsupported artifact layer %s", payload.Layer)
	}
	if payload.Language != nil || payload.ProjectRoot != nil || payload.Indexer != nil {
		return errors.New("zoektartifact: ZOEKT payload must not carry SCIP language/project/indexer scope")
	}
	if strings.TrimSpace(leaseOwner) == "" || strings.TrimSpace(attemptID) == "" {
		return errors.New("zoektartifact: leaseOwner and attemptID are required")
	}
	if strings.TrimSpace(result.ArtifactPath) == "" {
		return errors.New("zoektartifact: artifactPath is required")
	}
	if err := validatePublishedArtifactScope(s.artifactRoot, payload, result.ArtifactPath); err != nil {
		return err
	}
	if err := s.validateLiveSubjobLease(ctx, payload, result, leaseOwner, attemptID); err != nil {
		return err
	}

	readFile := s.readFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	raw, err := readFile(result.ArtifactPath)
	if err != nil {
		return fmt.Errorf("zoektartifact: read artifact: %w", err)
	}
	if err := validateArtifactSHA(raw, result.ArtifactSHA256); err != nil {
		return err
	}
	var artifact zoektArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return fmt.Errorf("zoektartifact: decode artifact: %w", err)
	}
	if err := validateArtifactScope(payload, artifact); err != nil {
		return err
	}
	if err := s.validateShardSet(ctx, payload, artifact); err != nil {
		return err
	}
	if err := s.reconcileManifestStrategy(ctx, payload, artifact); err != nil {
		return err
	}
	return nil
}

type zoektArtifact struct {
	OrgID       int32                `json:"orgId"`
	WorkspaceID string               `json:"workspaceId"`
	RepoID      int32                `json:"repoId"`
	Branch      string               `json:"branch"`
	Revision    string               `json:"revision"`
	CommitHash  string               `json:"commitHash"`
	IndexJobID  string               `json:"indexJobId"`
	IndexDir    string               `json:"indexDir"`
	ShardPrefix string               `json:"shardPrefix"`
	Shards      []zoektShardArtifact `json:"shards"`
	Stdout      string               `json:"stdout"`
	Stderr      string               `json:"stderr"`
}

type zoektShardArtifact struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

func validateArtifactScope(payload indexsubjobtask.Payload, artifact zoektArtifact) error {
	if payload.WorkspaceID == nil {
		return errors.New("zoektartifact: workspaceId is required")
	}
	if artifact.OrgID != payload.OrgID ||
		artifact.WorkspaceID != *payload.WorkspaceID ||
		artifact.RepoID != payload.RepoID ||
		artifact.Branch != payload.Branch ||
		artifact.Revision != payload.Revision ||
		artifact.CommitHash != payload.CommitHash ||
		artifact.IndexJobID != payload.RepoIndexingJobID {
		return fmt.Errorf("zoektartifact: artifact scope mismatch for subjob %s", payload.SubjobID)
	}
	wantPrefix := repopaths.ShardPrefix(payload.OrgID, payload.RepoID)
	if artifact.ShardPrefix != wantPrefix && !strings.HasPrefix(artifact.ShardPrefix, wantPrefix+"_") {
		return fmt.Errorf("zoektartifact: shardPrefix mismatch: got %q want %q or %q-prefixed branch shard", artifact.ShardPrefix, wantPrefix, wantPrefix+"_")
	}
	if len(artifact.Shards) == 0 {
		return errors.New("zoektartifact: no Zoekt shard files were recorded")
	}
	return nil
}

func (s *Store) validateShardSet(ctx context.Context, payload indexsubjobtask.Payload, artifact zoektArtifact) error {
	expectedDir := s.pathsCfg.ZoektIndexPath(repopaths.Repo{OrgID: payload.OrgID, RepoID: payload.RepoID})
	expectedDirAbs, err := filepath.Abs(expectedDir)
	if err != nil {
		return fmt.Errorf("zoektartifact: resolve expected index dir: %w", err)
	}
	expectedDirAbs = filepath.Clean(expectedDirAbs)
	artifactDirAbs, err := filepath.Abs(artifact.IndexDir)
	if err != nil {
		return fmt.Errorf("zoektartifact: resolve artifact index dir: %w", err)
	}
	if filepath.Clean(artifactDirAbs) != expectedDirAbs {
		return fmt.Errorf("zoektartifact: indexDir mismatch: got %s want %s", filepath.Clean(artifactDirAbs), expectedDirAbs)
	}
	hashFile := s.hashFile
	if hashFile == nil {
		hashFile = sha256File
	}
	for _, shard := range artifact.Shards {
		if strings.TrimSpace(shard.Path) == "" {
			return errors.New("zoektartifact: shard path is required")
		}
		pathAbs, err := filepath.Abs(shard.Path)
		if err != nil {
			return fmt.Errorf("zoektartifact: resolve shard path: %w", err)
		}
		pathAbs = filepath.Clean(pathAbs)
		rel, err := filepath.Rel(expectedDirAbs, pathAbs)
		if err != nil {
			return fmt.Errorf("zoektartifact: shard relative path: %w", err)
		}
		if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || strings.Contains(rel, string(filepath.Separator)) {
			return fmt.Errorf("zoektartifact: shard %s escapes expected index dir %s", pathAbs, expectedDirAbs)
		}
		name := filepath.Base(pathAbs)
		if !strings.HasPrefix(name, artifact.ShardPrefix) || !strings.HasSuffix(name, ".zoekt") {
			return fmt.Errorf("zoektartifact: shard %s does not match prefix %q and .zoekt suffix", name, artifact.ShardPrefix)
		}
		info, err := os.Lstat(pathAbs)
		if err != nil {
			return fmt.Errorf("zoektartifact: stat shard %s: %w", pathAbs, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("zoektartifact: shard %s is not a regular file", pathAbs)
		}
		if shard.SizeBytes > 0 && shard.SizeBytes != info.Size() {
			return fmt.Errorf("zoektartifact: shard %s size mismatch: got %d want %d", pathAbs, info.Size(), shard.SizeBytes)
		}
		gotSHA, err := hashFile(ctx, pathAbs)
		if err != nil {
			return err
		}
		if gotSHA != normalizeSHA256(shard.SHA256) {
			return fmt.Errorf("zoektartifact: shard %s SHA-256 mismatch", pathAbs)
		}
	}
	return nil
}

func (s *Store) validateLiveSubjobLease(ctx context.Context, payload indexsubjobtask.Payload, result indexexecutor.Result, leaseOwner, attemptID string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE "CodeIntelIndexSubjob" s
		SET status = 'VALIDATING',
		    "leaseExpiresAt" = GREATEST(s."leaseExpiresAt", NOW() + INTERVAL '15 minutes'),
		    "heartbeatAt" = NOW(),
		    "updatedAt" = NOW()
		WHERE s.id = $1
		  AND s."repoIndexingJobId" = $2
		  AND s."orgId" = $3
		  AND s."workspaceId" = $4
		  AND s."repoId" = $5
		  AND s.branch = $6
		  AND s.revision = $7
		  AND s."commitHash" = $8
		  AND s.layer = 'ZOEKT'
		  AND s.language IS NULL
		  AND s."projectRoot" IS NULL
		  AND s.indexer IS NULL
		  AND s."workerClass" = $9
		  AND s."queueName" = $10
		  AND s."leaseOwner" = $11
		  AND s."attemptId" = $12
		  AND s."artifactPath" = $13
		  AND s."artifactSha256" = $14
		  AND s.status IN ('ARTIFACT_WRITTEN', 'VALIDATING')
		  AND s."leaseExpiresAt" IS NOT NULL
		  AND s."leaseExpiresAt" > NOW()
		  AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexingJob" j
		    JOIN "Repo" r
		      ON r.id = j."repoId"
		     AND r."orgId" = s."orgId"
		    WHERE j.id = s."repoIndexingJobId"
		      AND j."repoId" = s."repoId"
		      AND j.type = 'INDEX'::"RepoIndexingJobType"
		      AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
		  )
		  AND EXISTS (
		    SELECT 1
		    FROM "RepoIndexManifest" m
		    WHERE m."indexJobId" = s."repoIndexingJobId"
		      AND m."orgId" = s."orgId"
		      AND m."repoId" = s."repoId"
		      AND m."workspaceId" = s."workspaceId"
		      AND m.branch = s.branch
		      AND m."commitHash" = s."commitHash"
		      AND m.status = 'PENDING'::"RepoIndexManifestStatus"
		  )
	`, payload.SubjobID, payload.RepoIndexingJobID, payload.OrgID, payload.WorkspaceID,
		payload.RepoID, payload.Branch, payload.Revision, payload.CommitHash,
		payload.WorkerClass, payload.QueueName, leaseOwner, attemptID,
		result.ArtifactPath, result.ArtifactSHA256)
	if err != nil {
		return fmt.Errorf("zoektartifact: validate live subjob lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("zoektartifact: subjob lease is no longer valid")
	}
	return nil
}

func resolveArtifactRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("zoektartifact: artifact root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("zoektartifact: artifact root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("zoektartifact: resolve artifact root: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func validatePublishedArtifactScope(root string, payload indexsubjobtask.Payload, raw string) error {
	if payload.WorkspaceID == nil || strings.TrimSpace(*payload.WorkspaceID) == "" || strings.TrimSpace(payload.Branch) == "" {
		return errors.New("zoektartifact: payload workspaceId and branch are required for artifact scope")
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = filepath.Clean(resolved)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("zoektartifact: artifact %s escapes root %s", abs, root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 5 {
		return fmt.Errorf("zoektartifact: artifact %s does not include org/repo/workspace/branch/commit scope", abs)
	}
	if parts[0] != strconv.FormatInt(int64(payload.OrgID), 10) ||
		parts[1] != strconv.FormatInt(int64(payload.RepoID), 10) ||
		parts[2] != artifactScopeSegment(*payload.WorkspaceID) ||
		parts[3] != artifactScopeSegment(payload.Branch) ||
		parts[4] != payload.CommitHash {
		return fmt.Errorf("zoektartifact: artifact %s is outside payload scope", abs)
	}
	return nil
}

func artifactScopeSegment(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "s-" + hex.EncodeToString(sum[:])[:16]
}

func validateArtifactSHA(raw []byte, want string) error {
	gotBytes := sha256.Sum256(raw)
	got := hex.EncodeToString(gotBytes[:])
	want = normalizeSHA256(want)
	if want == "" {
		return errors.New("zoektartifact: artifact SHA-256 is required")
	}
	if got != want {
		return fmt.Errorf("zoektartifact: artifact SHA-256 mismatch: got %s want %s", got, want)
	}
	return nil
}

func sha256File(ctx context.Context, path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("zoektartifact: lstat shard: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("zoektartifact: shard %s is a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("zoektartifact: shard %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("zoektartifact: open shard: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx: ctx, r: f}); err != nil {
		return "", fmt.Errorf("zoektartifact: hash shard: %w", err)
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

func (s *Store) reconcileManifestStrategy(ctx context.Context, payload indexsubjobtask.Payload, artifact zoektArtifact) error {
	if !zoektDeltaFallbackLogged(artifact.Stdout + "\n" + artifact.Stderr) {
		return nil
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE "RepoIndexManifest"
		SET "zoektStrategy" = 'FULL_REPO_REWRITE',
		    plan = jsonb_set(
		        COALESCE(plan, '{}'::jsonb),
		        '{zoekt}',
		        COALESCE(plan->'zoekt', '{}'::jsonb)
		          || jsonb_build_object(
		               'strategy', 'FULL_REPO_REWRITE',
		               'reason', 'Zoekt requested native file-level delta but upstream fell back to a normal shard rewrite.'
		             ),
		        true
		    ),
		    "updatedAt" = NOW()
		WHERE "indexJobId" = $1
		  AND "orgId" = $2
		  AND "repoId" = $3
		  AND "workspaceId" = $4
		  AND branch = $5
		  AND "commitHash" = $6
		  AND status = 'PENDING'::"RepoIndexManifestStatus"
		  AND "zoektStrategy" = 'DELTA_FILES'
	`, payload.RepoIndexingJobID, payload.OrgID, payload.RepoID, payload.WorkspaceID, payload.Branch, payload.CommitHash)
	if err != nil {
		return fmt.Errorf("zoektartifact: reconcile delta fallback: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	return nil
}

func zoektDeltaFallbackLogged(output string) bool {
	return strings.Contains(output, "delta build: falling back to normal build")
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
