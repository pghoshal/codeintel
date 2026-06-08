// Package astartifact persists split-executor AST/tree-sitter graph
// artifacts through the Go-owned Nebula/Postgres graph writer path.
package astartifact

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"codeintel/internal/backend/codegraphwriter"
	"codeintel/internal/backend/graphstore"
	"codeintel/internal/backend/indexexecutor"
	"codeintel/internal/backend/indexsubjobtask"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrUnsupportedLayer = errors.New("astartifact: unsupported artifact layer")

const maxASTGraphArtifactBytes = 256 << 20

type database interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type Store struct {
	db           database
	artifactRoot string
	writer       *codegraphwriter.Handler
	readFile     func(string) ([]byte, error)
}

func NewStore(db database, graph graphstore.Store, logger *slog.Logger, artifactRoot string) (*Store, error) {
	resolvedRoot, err := resolveArtifactRoot(artifactRoot)
	if err != nil {
		return nil, err
	}
	return &Store{
		db:           db,
		artifactRoot: resolvedRoot,
		writer:       codegraphwriter.NewHandler(db, graph, logger),
	}, nil
}

func (s *Store) Ingest(ctx context.Context, payload indexsubjobtask.Payload, result indexexecutor.Result, leaseOwner, attemptID string) error {
	if s == nil || s.db == nil || s.writer == nil {
		return errors.New("astartifact: store is not configured")
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("astartifact: payload: %w", err)
	}
	if strings.TrimSpace(leaseOwner) == "" || strings.TrimSpace(attemptID) == "" {
		return errors.New("astartifact: leaseOwner and attemptID are required")
	}
	if payload.Layer != indexsubjobtask.LayerASTTreeSitter {
		return ErrUnsupportedLayer
	}
	if payload.Language != nil || payload.ProjectRoot != nil || payload.Indexer != nil {
		return errors.New("astartifact: AST/tree-sitter payload must not carry SCIP language/project/indexer scope")
	}
	if strings.TrimSpace(result.ArtifactPath) == "" {
		return errors.New("astartifact: artifactPath is required")
	}
	if err := s.validateLiveSubjobLeasePrecheck(ctx, payload, result, leaseOwner, attemptID); err != nil {
		return err
	}
	if err := validatePublishedArtifactScope(s.artifactRoot, payload, result.ArtifactPath); err != nil {
		return err
	}

	readFile := s.readFile
	if readFile == nil {
		readFile = readArtifactFile
	}
	raw, err := readFile(result.ArtifactPath)
	if err != nil {
		return fmt.Errorf("read AST graph artifact: %w", err)
	}
	if err := validateArtifactSHA(raw, result.ArtifactSHA256); err != nil {
		return err
	}
	graphPayload, err := decodeGraphPayload(raw, payload)
	if err != nil {
		return err
	}
	manifest, err := resolvePendingManifest(ctx, s.db, payload)
	if err != nil {
		return err
	}
	if graphPayload.ManifestID != "" && graphPayload.ManifestID != manifest.ID {
		return fmt.Errorf("astartifact: artifact manifestId %q does not match pending manifest %q", graphPayload.ManifestID, manifest.ID)
	}
	graphPayload.ManifestID = manifest.ID
	graphPayload.ProviderConnectionID = manifest.ProviderConnectionID

	if err := s.validateLiveSubjobLeasePrecheck(ctx, payload, result, leaseOwner, attemptID); err != nil {
		return err
	}
	if err := s.writer.WritePendingPayload(ctx, graphPayload); err != nil {
		return fmt.Errorf("write AST graph artifact: %w", err)
	}
	return nil
}

func decodeGraphPayload(raw []byte, payload indexsubjobtask.Payload) (codegraphwriter.Payload, error) {
	var graphPayload codegraphwriter.Payload
	if err := json.Unmarshal(raw, &graphPayload); err != nil {
		return codegraphwriter.Payload{}, fmt.Errorf("astartifact: decode graph payload: %w", err)
	}
	if graphPayload.OrgID != int64(payload.OrgID) {
		return codegraphwriter.Payload{}, fmt.Errorf("astartifact: orgId mismatch: artifact=%d payload=%d", graphPayload.OrgID, payload.OrgID)
	}
	if graphPayload.RepoID != int64(payload.RepoID) {
		return codegraphwriter.Payload{}, fmt.Errorf("astartifact: repoId mismatch: artifact=%d payload=%d", graphPayload.RepoID, payload.RepoID)
	}
	if payload.WorkspaceID == nil || graphPayload.WorkspaceID != *payload.WorkspaceID {
		return codegraphwriter.Payload{}, fmt.Errorf("astartifact: workspaceId mismatch: artifact=%q", graphPayload.WorkspaceID)
	}
	if graphPayload.Branch != "" && graphPayload.Branch != payload.Branch {
		return codegraphwriter.Payload{}, fmt.Errorf("astartifact: branch mismatch: artifact=%q payload=%q", graphPayload.Branch, payload.Branch)
	}
	graphPayload.Branch = payload.Branch
	if graphPayload.Revision != payload.Revision {
		return codegraphwriter.Payload{}, fmt.Errorf("astartifact: revision mismatch: artifact=%q payload=%q", graphPayload.Revision, payload.Revision)
	}
	if graphPayload.CommitHash != payload.CommitHash {
		return codegraphwriter.Payload{}, fmt.Errorf("astartifact: commitHash mismatch: artifact=%q payload=%q", graphPayload.CommitHash, payload.CommitHash)
	}
	if graphPayload.IndexJobID != "" && graphPayload.IndexJobID != payload.RepoIndexingJobID {
		return codegraphwriter.Payload{}, fmt.Errorf("astartifact: indexJobId mismatch: artifact=%q payload=%q", graphPayload.IndexJobID, payload.RepoIndexingJobID)
	}
	graphPayload.IndexJobID = payload.RepoIndexingJobID
	if strings.TrimSpace(graphPayload.Source) == "" {
		graphPayload.Source = "syntactic-ast"
	}
	return graphPayload, nil
}

type pendingManifest struct {
	ID                   string
	ProviderConnectionID *string
}

func resolvePendingManifest(ctx context.Context, q querier, payload indexsubjobtask.Payload) (pendingManifest, error) {
	var manifest pendingManifest
	var provider sql.NullString
	err := q.QueryRow(ctx, `
		SELECT id, "providerConnectionId"
		FROM "RepoIndexManifest"
		WHERE "indexJobId" = $1
		  AND "orgId" = $2
		  AND "repoId" = $3
		  AND "workspaceId" = $4
		  AND branch = $5
		  AND "commitHash" = $6
		  AND status = 'PENDING'::"RepoIndexManifestStatus"
		ORDER BY "createdAt" DESC, id DESC
		LIMIT 1
	`, payload.RepoIndexingJobID, payload.OrgID, payload.RepoID, payload.WorkspaceID, payload.Branch, payload.CommitHash).Scan(&manifest.ID, &provider)
	if err != nil {
		return pendingManifest{}, fmt.Errorf("astartifact: resolve pending manifest: %w", err)
	}
	if provider.Valid {
		manifest.ProviderConnectionID = &provider.String
	}
	return manifest, nil
}

func (s *Store) validateLiveSubjobLeasePrecheck(ctx context.Context, payload indexsubjobtask.Payload, result indexexecutor.Result, leaseOwner, attemptID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin AST artifact ingest precheck: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := validateLiveSubjobLease(ctx, tx, payload, result, leaseOwner, attemptID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit AST artifact ingest precheck: %w", err)
	}
	return nil
}

func validateLiveSubjobLease(ctx context.Context, q querier, payload indexsubjobtask.Payload, result indexexecutor.Result, leaseOwner, attemptID string) error {
	tag, err := q.Exec(ctx, `
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
		  AND s.layer = 'AST_TREE_SITTER'
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
	`,
		payload.SubjobID,
		payload.RepoIndexingJobID,
		payload.OrgID,
		payload.WorkspaceID,
		payload.RepoID,
		payload.Branch,
		payload.Revision,
		payload.CommitHash,
		payload.WorkerClass,
		payload.QueueName,
		leaseOwner,
		attemptID,
		result.ArtifactPath,
		result.ArtifactSHA256,
	)
	if err != nil {
		return fmt.Errorf("validate AST subjob lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("astartifact: live subjob lease not found or no longer valid")
	}
	return nil
}

func resolveArtifactRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("astartifact: artifact root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("astartifact: artifact root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("astartifact: resolve artifact root: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func validatePublishedArtifactScope(root string, payload indexsubjobtask.Payload, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("astartifact: artifactPath is required")
	}
	root, err := resolveArtifactRoot(root)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("astartifact: artifactPath: %w", err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("astartifact: lstat artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("astartifact: artifact %s is a symlink", abs)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("astartifact: artifact %s is not a regular file", abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("astartifact: resolve artifact path: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if err := validateRootRelativeScope(root, resolved, payload); err != nil {
		return err
	}
	return nil
}

func validateRootRelativeScope(root, path string, payload indexsubjobtask.Payload) error {
	if payload.WorkspaceID == nil {
		return errors.New("astartifact: payload workspaceId is required for artifact scope")
	}
	org := strconv.FormatInt(int64(payload.OrgID), 10)
	repo := strconv.FormatInt(int64(payload.RepoID), 10)
	workspace := artifactScopeSegment(*payload.WorkspaceID)
	branch := artifactScopeSegment(payload.Branch)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("astartifact: artifact scope: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("astartifact: artifact %s escapes artifact root %s", path, root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 5 {
		return fmt.Errorf("astartifact: artifact %s does not include org/repo/workspace/branch/commit scope", path)
	}
	if parts[0] != org || parts[1] != repo || parts[2] != workspace || parts[3] != branch || parts[4] != payload.CommitHash {
		return fmt.Errorf("astartifact: artifact %s is outside payload scope org=%d repo=%d workspace=%s branch=%s commit=%s", path, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Branch, payload.CommitHash)
	}
	return nil
}

func artifactScopeSegment(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "s-" + hex.EncodeToString(sum[:])[:16]
}

func readArtifactFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxASTGraphArtifactBytes {
		return nil, fmt.Errorf("%s is %d bytes, exceeds %d byte AST graph artifact limit", path, info.Size(), maxASTGraphArtifactBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxASTGraphArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxASTGraphArtifactBytes {
		return nil, fmt.Errorf("%s exceeds %d byte AST graph artifact limit while reading", path, maxASTGraphArtifactBytes)
	}
	return raw, nil
}

func validateArtifactSHA(raw []byte, expected string) error {
	expected = normalizeSHA256(expected)
	if expected == "" {
		return errors.New("astartifact: artifact SHA-256 is required")
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if got != expected {
		return fmt.Errorf("astartifact: artifact SHA-256 mismatch: got %s want %s", got, expected)
	}
	return nil
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
