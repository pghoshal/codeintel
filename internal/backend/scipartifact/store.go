package scipartifact

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

	"codeintel/internal/backend/indexexecutor"
	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/pkg/repopaths"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrUnsupportedLayer = errors.New("scipartifact: unsupported artifact layer")

const maxSCIPArtifactBytes = 256 << 20

type beginner interface {
	Begin(context.Context) (pgx.Tx, error)
}

type querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

type Store struct {
	db           beginner
	paths        repopaths.Config
	artifactRoot string
	newID        func() string
	readFile     func(string) ([]byte, error)
}

func NewStore(db beginner, paths repopaths.Config, artifactRoot string) (*Store, error) {
	resolvedRoot, err := resolveArtifactRoot(artifactRoot)
	if err != nil {
		return nil, err
	}
	return &Store{
		db:           db,
		paths:        paths,
		artifactRoot: resolvedRoot,
		newID:        uuid.NewString,
	}, nil
}

func (s *Store) Ingest(ctx context.Context, payload indexsubjobtask.Payload, result indexexecutor.Result, leaseOwner, attemptID string) error {
	if s == nil || s.db == nil {
		return errors.New("scipartifact: store is not configured")
	}
	if err := payload.Validate(); err != nil {
		return fmt.Errorf("scipartifact: payload: %w", err)
	}
	if strings.TrimSpace(leaseOwner) == "" || strings.TrimSpace(attemptID) == "" {
		return errors.New("scipartifact: leaseOwner and attemptID are required")
	}
	if payload.Layer != indexsubjobtask.LayerSCIP {
		return ErrUnsupportedLayer
	}
	if payload.Language == nil || strings.TrimSpace(*payload.Language) == "" {
		return errors.New("scipartifact: SCIP language is required")
	}
	if payload.ProjectRoot == nil {
		return errors.New("scipartifact: SCIP projectRoot is required")
	}
	if payload.Indexer == nil || strings.TrimSpace(*payload.Indexer) == "" {
		return errors.New("scipartifact: SCIP indexer is required")
	}
	if strings.TrimSpace(result.ArtifactPath) == "" {
		return errors.New("scipartifact: artifactPath is required")
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
		return fmt.Errorf("read SCIP artifact: %w", err)
	}
	if err := validateArtifactSHA(raw, result.ArtifactSHA256); err != nil {
		return err
	}
	worktree := s.paths.RevisionSnapshotPath(payload.OrgID, payload.RepoID, payload.CommitHash)
	rows, err := RowsFromArtifactBytes(raw, *payload.Language, *payload.ProjectRoot, worktree)
	if err != nil {
		return fmt.Errorf("parse SCIP artifact: %w", err)
	}
	if err := validateSemanticRows(rows, result.ArtifactPath); err != nil {
		return err
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin SCIP artifact ingest: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := validateLiveSubjobLease(ctx, tx, payload, result, leaseOwner, attemptID); err != nil {
		return err
	}
	indexID, err := s.upsertCodeIntelIndex(ctx, tx, payload, result)
	if err != nil {
		return err
	}
	if err := deleteStaleLanguageIndexes(ctx, tx, indexID, payload); err != nil {
		return err
	}
	languageIndexID, err := s.upsertLanguageIndex(ctx, tx, indexID, payload, result)
	if err != nil {
		return err
	}
	if err := deleteLanguageRows(ctx, tx, languageIndexID); err != nil {
		return err
	}
	if err := s.bulkInsertSymbols(ctx, tx, payload, indexID, languageIndexID, rows.Symbols); err != nil {
		return err
	}
	if err := s.bulkInsertOccurrences(ctx, tx, payload, indexID, languageIndexID, rows.Occurrences); err != nil {
		return err
	}
	if err := s.bulkInsertRelationships(ctx, tx, payload, indexID, languageIndexID, rows.Relationships); err != nil {
		return err
	}
	if err := markLanguageReady(ctx, tx, languageIndexID); err != nil {
		return err
	}
	if err := refreshCodeIntelIndexCounts(ctx, tx, indexID, payload); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit SCIP artifact ingest: %w", err)
	}
	return nil
}

func validateSemanticRows(rows Rows, artifactPath string) error {
	if len(rows.Symbols) == 0 && len(rows.Occurrences) == 0 && len(rows.Relationships) == 0 {
		return fmt.Errorf("scipartifact: artifact %s has no semantic rows: 0 symbols, 0 occurrences, 0 relationships", artifactPath)
	}
	return nil
}

func (s *Store) validateLiveSubjobLeasePrecheck(ctx context.Context, payload indexsubjobtask.Payload, result indexexecutor.Result, leaseOwner, attemptID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin SCIP artifact ingest precheck: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := validateLiveSubjobLease(ctx, tx, payload, result, leaseOwner, attemptID); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit SCIP artifact ingest precheck: %w", err)
	}
	return nil
}

func resolveArtifactRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("scipartifact: artifact root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("scipartifact: artifact root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("scipartifact: resolve artifact root: %w", err)
	}
	return filepath.Clean(resolved), nil
}

func validatePublishedArtifactScope(root string, payload indexsubjobtask.Payload, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("scipartifact: artifactPath is required")
	}
	root, err := resolveArtifactRoot(root)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("scipartifact: artifactPath: %w", err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Lstat(abs)
	if err != nil {
		return fmt.Errorf("scipartifact: lstat artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("scipartifact: artifact %s is a symlink", abs)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("scipartifact: artifact %s is not a regular file", abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("scipartifact: resolve artifact path: %w", err)
	}
	resolved = filepath.Clean(resolved)
	if err := validateRootRelativeScope(root, resolved, payload); err != nil {
		return err
	}
	return nil
}

func validateRootRelativeScope(root, path string, payload indexsubjobtask.Payload) error {
	if payload.WorkspaceID == nil {
		return errors.New("scipartifact: payload workspaceId is required for artifact scope")
	}
	org := strconv.FormatInt(int64(payload.OrgID), 10)
	repo := strconv.FormatInt(int64(payload.RepoID), 10)
	workspace := artifactScopeSegment(*payload.WorkspaceID)
	branch := artifactScopeSegment(payload.Branch)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return fmt.Errorf("scipartifact: artifact scope: %w", err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("scipartifact: artifact %s escapes artifact root %s", path, root)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) < 5 {
		return fmt.Errorf("scipartifact: artifact %s does not include org/repo/workspace/branch/commit scope", path)
	}
	if parts[0] != org || parts[1] != repo || parts[2] != workspace || parts[3] != branch || parts[4] != payload.CommitHash {
		return fmt.Errorf("scipartifact: artifact %s is outside payload scope org=%d repo=%d workspace=%s branch=%s commit=%s", path, payload.OrgID, payload.RepoID, *payload.WorkspaceID, payload.Branch, payload.CommitHash)
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
	if info.Size() > maxSCIPArtifactBytes {
		return nil, fmt.Errorf("%s is %d bytes, exceeds %d byte SCIP artifact limit", path, info.Size(), maxSCIPArtifactBytes)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxSCIPArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxSCIPArtifactBytes {
		return nil, fmt.Errorf("%s exceeds %d byte SCIP artifact limit while reading", path, maxSCIPArtifactBytes)
	}
	return raw, nil
}

func validateArtifactSHA(raw []byte, expected string) error {
	expected = normalizeSHA256(expected)
	if expected == "" {
		return errors.New("scipartifact: artifact SHA-256 is required")
	}
	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if got != expected {
		return fmt.Errorf("scipartifact: artifact SHA-256 mismatch: got %s want %s", got, expected)
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
		  AND s.layer = 'SCIP'
		  AND COALESCE(s.language, '') = $9
		  AND COALESCE(s."projectRoot", '') = $10
		  AND COALESCE(s.indexer, '') = $11
		  AND s."workerClass" = $12
		  AND s."queueName" = $13
		  AND s."leaseOwner" = $14
		  AND s."attemptId" = $15
		  AND s."artifactPath" = $16
		  AND s."artifactSha256" = $17
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
		*payload.Language,
		*payload.ProjectRoot,
		*payload.Indexer,
		payload.WorkerClass,
		payload.QueueName,
		leaseOwner,
		attemptID,
		result.ArtifactPath,
		result.ArtifactSHA256,
	)
	if err != nil {
		return fmt.Errorf("validate SCIP subjob lease: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("scipartifact: live subjob lease not found or no longer valid")
	}
	return nil
}

func (s *Store) upsertCodeIntelIndex(ctx context.Context, q querier, payload indexsubjobtask.Payload, result indexexecutor.Result) (string, error) {
	id := s.newID()
	artifactRoot := s.artifactRoot
	var indexID string
	err := q.QueryRow(ctx, `
		INSERT INTO "CodeIntelIndex" (
		    id, kind, status, "workspaceId", branch, revision, "commitHash", "artifactRoot",
		    "languageCount", "symbolCount", "occurrenceCount",
		    "relationshipCount", "errorMessage", "indexedAt",
		    "createdAt", "updatedAt", "orgId", "repoId"
		)
		VALUES (
		    $1, 'SCIP'::"CodeIntelIndexKind", 'INDEXING'::"CodeIntelIndexStatus",
		    $2, $3, $4, $5, $6, 0, 0, 0, 0, NULL, NULL, NOW(), NOW(), $7, $8
		)
		ON CONFLICT ("orgId", "repoId", "workspaceId", branch, revision, "commitHash", kind)
		DO UPDATE SET
		    status = CASE
		        WHEN "CodeIntelIndex".status = 'READY'::"CodeIntelIndexStatus" THEN 'INDEXING'::"CodeIntelIndexStatus"
		        ELSE "CodeIntelIndex".status
		    END,
		    "artifactRoot" = EXCLUDED."artifactRoot",
		    "errorMessage" = NULL,
		    "indexedAt" = CASE
		        WHEN "CodeIntelIndex".status = 'READY'::"CodeIntelIndexStatus" THEN NULL
		        ELSE "CodeIntelIndex"."indexedAt"
		    END,
		    "updatedAt" = NOW()
		WHERE "CodeIntelIndex"."orgId" = EXCLUDED."orgId"
		  AND "CodeIntelIndex"."workspaceId" = EXCLUDED."workspaceId"
		  AND "CodeIntelIndex".branch = EXCLUDED.branch
		RETURNING id
	`, id, *payload.WorkspaceID, payload.Branch, payload.Revision, payload.CommitHash, artifactRoot, payload.OrgID, payload.RepoID).Scan(&indexID)
	if err != nil {
		return "", fmt.Errorf("upsert CodeIntelIndex: %w", err)
	}
	return indexID, nil
}

func (s *Store) upsertLanguageIndex(ctx context.Context, q querier, indexID string, payload indexsubjobtask.Payload, result indexexecutor.Result) (string, error) {
	id := s.newID()
	language := *payload.Language
	projectRoot := *payload.ProjectRoot
	indexer := *payload.Indexer
	durationMS := metadataInt32(result.Metadata, "durationMs")
	var languageIndexID string
	err := q.QueryRow(ctx, `
		INSERT INTO "CodeIntelLanguageIndex" (
		    id, language, "projectRoot", indexer, "workerClass",
		    status, "artifactPath", command, "durationMs",
		    "errorMessage", "createdAt", "updatedAt", "codeIntelIndexId"
		)
		VALUES (
		    $1, $2, $3, $4, $5,
		    'INDEXING'::"CodeIntelIndexStatus", $6, $7, $8,
		    NULL, NOW(), NOW(), $9
		)
		ON CONFLICT ("codeIntelIndexId", language, "projectRoot", indexer)
		DO UPDATE SET
		    "workerClass" = EXCLUDED."workerClass",
		    status = 'INDEXING'::"CodeIntelIndexStatus",
		    "artifactPath" = EXCLUDED."artifactPath",
		    command = EXCLUDED.command,
		    "durationMs" = EXCLUDED."durationMs",
		    "errorMessage" = NULL,
		    "toolchainId" = NULL,
		    "toolchainFingerprint" = NULL,
		    "toolchainVersion" = NULL,
		    "toolchainPath" = NULL,
		    "toolchainSha256" = NULL,
		    "updatedAt" = NOW()
		RETURNING id
	`, id, language, projectRoot, indexer, payload.WorkerClass, result.ArtifactPath, commandFromPayload(payload), durationMS, indexID).Scan(&languageIndexID)
	if err != nil {
		return "", fmt.Errorf("upsert CodeIntelLanguageIndex: %w", err)
	}
	return languageIndexID, nil
}

func deleteLanguageRows(ctx context.Context, q querier, languageIndexID string) error {
	for _, stmt := range []string{
		`DELETE FROM "CodeIntelRelationship" WHERE "codeIntelLanguageIndexId" = $1`,
		`DELETE FROM "CodeIntelOccurrence" WHERE "codeIntelLanguageIndexId" = $1`,
		`DELETE FROM "CodeIntelSymbol" WHERE "codeIntelLanguageIndexId" = $1`,
	} {
		if _, err := q.Exec(ctx, stmt, languageIndexID); err != nil {
			return fmt.Errorf("delete previous language rows: %w", err)
		}
	}
	return nil
}

func deleteStaleLanguageIndexes(ctx context.Context, q querier, indexID string, payload indexsubjobtask.Payload) error {
	for _, stmt := range []string{
		`WITH stale AS (
		    SELECT li.id
		    FROM "CodeIntelLanguageIndex" li
		    WHERE li."codeIntelIndexId" = $1
		      AND NOT EXISTS (
		        SELECT 1
		        FROM "CodeIntelIndexSubjob" s
		        WHERE s."repoIndexingJobId" = $2
		          AND s."orgId" = $3
		          AND s."workspaceId" = $4
		          AND s."repoId" = $5
		          AND s.branch = $6
		          AND s.revision = $7
		          AND s."commitHash" = $8
		          AND s.layer = 'SCIP'
		          AND COALESCE(s.language, '') = COALESCE(li.language, '')
		          AND COALESCE(s."projectRoot", '') = COALESCE(li."projectRoot", '')
		          AND COALESCE(s.indexer, '') = COALESCE(li.indexer, '')
		      )
		)
		DELETE FROM "CodeIntelRelationship" r
		USING stale
		WHERE r."codeIntelLanguageIndexId" = stale.id`,
		`WITH stale AS (
		    SELECT li.id
		    FROM "CodeIntelLanguageIndex" li
		    WHERE li."codeIntelIndexId" = $1
		      AND NOT EXISTS (
		        SELECT 1
		        FROM "CodeIntelIndexSubjob" s
		        WHERE s."repoIndexingJobId" = $2
		          AND s."orgId" = $3
		          AND s."workspaceId" = $4
		          AND s."repoId" = $5
		          AND s.branch = $6
		          AND s.revision = $7
		          AND s."commitHash" = $8
		          AND s.layer = 'SCIP'
		          AND COALESCE(s.language, '') = COALESCE(li.language, '')
		          AND COALESCE(s."projectRoot", '') = COALESCE(li."projectRoot", '')
		          AND COALESCE(s.indexer, '') = COALESCE(li.indexer, '')
		      )
		)
		DELETE FROM "CodeIntelOccurrence" o
		USING stale
		WHERE o."codeIntelLanguageIndexId" = stale.id`,
		`WITH stale AS (
		    SELECT li.id
		    FROM "CodeIntelLanguageIndex" li
		    WHERE li."codeIntelIndexId" = $1
		      AND NOT EXISTS (
		        SELECT 1
		        FROM "CodeIntelIndexSubjob" s
		        WHERE s."repoIndexingJobId" = $2
		          AND s."orgId" = $3
		          AND s."workspaceId" = $4
		          AND s."repoId" = $5
		          AND s.branch = $6
		          AND s.revision = $7
		          AND s."commitHash" = $8
		          AND s.layer = 'SCIP'
		          AND COALESCE(s.language, '') = COALESCE(li.language, '')
		          AND COALESCE(s."projectRoot", '') = COALESCE(li."projectRoot", '')
		          AND COALESCE(s.indexer, '') = COALESCE(li.indexer, '')
		      )
		)
		DELETE FROM "CodeIntelSymbol" s
		USING stale
		WHERE s."codeIntelLanguageIndexId" = stale.id`,
		`DELETE FROM "CodeIntelLanguageIndex" li
		WHERE li."codeIntelIndexId" = $1
		  AND NOT EXISTS (
		    SELECT 1
		    FROM "CodeIntelIndexSubjob" s
		    WHERE s."repoIndexingJobId" = $2
		      AND s."orgId" = $3
		      AND s."workspaceId" = $4
		      AND s."repoId" = $5
		      AND s.branch = $6
		      AND s.revision = $7
		      AND s."commitHash" = $8
		      AND s.layer = 'SCIP'
		      AND COALESCE(s.language, '') = COALESCE(li.language, '')
		      AND COALESCE(s."projectRoot", '') = COALESCE(li."projectRoot", '')
		      AND COALESCE(s.indexer, '') = COALESCE(li.indexer, '')
		  )`,
	} {
		if _, err := q.Exec(ctx, stmt,
			indexID,
			payload.RepoIndexingJobID,
			payload.OrgID,
			payload.WorkspaceID,
			payload.RepoID,
			payload.Branch,
			payload.Revision,
			payload.CommitHash,
		); err != nil {
			return fmt.Errorf("delete stale language indexes: %w", err)
		}
	}
	return nil
}

func (s *Store) bulkInsertSymbols(ctx context.Context, q querier, payload indexsubjobtask.Payload, indexID, languageIndexID string, rows []SymbolRow) error {
	const paramsPerRow = 17
	for _, chunk := range chunkSymbols(rows, 60000/paramsPerRow) {
		sql := strings.Builder{}
		sql.WriteString(`INSERT INTO "CodeIntelSymbol" (
			id, symbol, "displayName", kind, language, documentation,
			signature, "filePath", "startLine", "startCharacter",
			"endLine", "endCharacter", "enclosingSymbol", "createdAt",
			"orgId", "repoId", "codeIntelIndexId", "codeIntelLanguageIndexId"
		) VALUES `)
		args := make([]any, 0, len(chunk)*paramsPerRow)
		for i, row := range chunk {
			if i > 0 {
				sql.WriteString(", ")
			}
			base := i * paramsPerRow
			sql.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,NOW(),$%d,$%d,$%d,$%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9,
				base+10, base+11, base+12, base+13, base+14, base+15, base+16, base+17))
			args = append(args,
				s.newID(),
				row.Symbol,
				row.DisplayName,
				row.Kind,
				row.Language,
				row.Documentation,
				row.Signature,
				row.FilePath,
				row.StartLine,
				row.StartCharacter,
				row.EndLine,
				row.EndCharacter,
				row.EnclosingSymbol,
				payload.OrgID,
				payload.RepoID,
				indexID,
				languageIndexID,
			)
		}
		sql.WriteString(` ON CONFLICT ("codeIntelIndexId", symbol) DO NOTHING`)
		if _, err := q.Exec(ctx, sql.String(), args...); err != nil {
			return fmt.Errorf("insert CodeIntelSymbol: %w", err)
		}
	}
	return nil
}

func (s *Store) bulkInsertOccurrences(ctx context.Context, q querier, payload indexsubjobtask.Payload, indexID, languageIndexID string, rows []OccurrenceRow) error {
	const paramsPerRow = 16
	for _, chunk := range chunkOccurrences(rows, 60000/paramsPerRow) {
		sql := strings.Builder{}
		sql.WriteString(`INSERT INTO "CodeIntelOccurrence" (
			id, symbol, "filePath", "startLine", "startCharacter",
			"endLine", "endCharacter", role, language, "syntaxKind",
			"lineContent", "enclosingSymbol", "createdAt",
			"orgId", "repoId", "codeIntelIndexId", "codeIntelLanguageIndexId"
		) VALUES `)
		args := make([]any, 0, len(chunk)*paramsPerRow)
		for i, row := range chunk {
			if i > 0 {
				sql.WriteString(", ")
			}
			base := i * paramsPerRow
			sql.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d::\"CodeIntelOccurrenceRole\",$%d,$%d,$%d,$%d,NOW(),$%d,$%d,$%d,$%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8,
				base+9, base+10, base+11, base+12, base+13, base+14, base+15, base+16))
			args = append(args,
				s.newID(),
				row.Symbol,
				row.FilePath,
				row.StartLine,
				row.StartCharacter,
				row.EndLine,
				row.EndCharacter,
				row.Role,
				row.Language,
				row.SyntaxKind,
				row.LineContent,
				row.EnclosingSymbol,
				payload.OrgID,
				payload.RepoID,
				indexID,
				languageIndexID,
			)
		}
		if _, err := q.Exec(ctx, sql.String(), args...); err != nil {
			return fmt.Errorf("insert CodeIntelOccurrence: %w", err)
		}
	}
	return nil
}

func (s *Store) bulkInsertRelationships(ctx context.Context, q querier, payload indexsubjobtask.Payload, indexID, languageIndexID string, rows []RelationshipRow) error {
	const paramsPerRow = 11
	for _, chunk := range chunkRelationships(rows, 60000/paramsPerRow) {
		sql := strings.Builder{}
		sql.WriteString(`INSERT INTO "CodeIntelRelationship" (
			id, "sourceSymbol", "targetSymbol", "isReference",
			"isImplementation", "isTypeDefinition", "isDefinition",
			"createdAt", "orgId", "repoId", "codeIntelIndexId", "codeIntelLanguageIndexId"
		) VALUES `)
		args := make([]any, 0, len(chunk)*paramsPerRow)
		for i, row := range chunk {
			if i > 0 {
				sql.WriteString(", ")
			}
			base := i * paramsPerRow
			sql.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d,NOW(),$%d,$%d,$%d,$%d)",
				base+1, base+2, base+3, base+4, base+5, base+6, base+7,
				base+8, base+9, base+10, base+11))
			args = append(args,
				s.newID(),
				row.SourceSymbol,
				row.TargetSymbol,
				row.IsReference,
				row.IsImplementation,
				row.IsTypeDefinition,
				row.IsDefinition,
				payload.OrgID,
				payload.RepoID,
				indexID,
				languageIndexID,
			)
		}
		sql.WriteString(` ON CONFLICT ("codeIntelIndexId", "sourceSymbol", "targetSymbol", "isReference", "isImplementation", "isTypeDefinition", "isDefinition") DO NOTHING`)
		if _, err := q.Exec(ctx, sql.String(), args...); err != nil {
			return fmt.Errorf("insert CodeIntelRelationship: %w", err)
		}
	}
	return nil
}

func markLanguageReady(ctx context.Context, q querier, languageIndexID string) error {
	_, err := q.Exec(ctx, `
		UPDATE "CodeIntelLanguageIndex"
		SET status = 'READY'::"CodeIntelIndexStatus",
		    "errorMessage" = NULL,
		    "updatedAt" = NOW()
		WHERE id = $1
	`, languageIndexID)
	if err != nil {
		return fmt.Errorf("mark CodeIntelLanguageIndex READY: %w", err)
	}
	return nil
}

func refreshCodeIntelIndexCounts(ctx context.Context, q querier, indexID string, payload indexsubjobtask.Payload) error {
	_, err := q.Exec(ctx, `
		WITH counts AS (
		  SELECT
		    (SELECT COUNT(DISTINCT language)::int FROM "CodeIntelLanguageIndex" WHERE "codeIntelIndexId" = $1 AND status = 'READY'::"CodeIntelIndexStatus") AS language_count,
		    (SELECT COUNT(*)::int FROM "CodeIntelSymbol" WHERE "codeIntelIndexId" = $1) AS symbol_count,
		    (SELECT COUNT(*)::int FROM "CodeIntelOccurrence" WHERE "codeIntelIndexId" = $1) AS occurrence_count,
		    (SELECT COUNT(*)::int FROM "CodeIntelRelationship" WHERE "codeIntelIndexId" = $1) AS relationship_count,
		    EXISTS (
		      SELECT 1
		      FROM "CodeIntelIndexSubjob" s
		      WHERE s."repoIndexingJobId" = $2
		        AND s."orgId" = $3
		        AND s."workspaceId" = $4
		        AND s."repoId" = $5
		        AND s.branch = $6
		        AND s.revision = $7
		        AND s."commitHash" = $8
		        AND s.layer = 'SCIP'
		        AND s.status = 'FAILED'
		    ) AS has_failed_subjob,
		    EXISTS (
		      SELECT 1
		      FROM "CodeIntelIndexSubjob" s
		      WHERE s."repoIndexingJobId" = $2
		        AND s."orgId" = $3
		        AND s."workspaceId" = $4
		        AND s."repoId" = $5
		        AND s.branch = $6
		        AND s.revision = $7
		        AND s."commitHash" = $8
		        AND s.layer = 'SCIP'
		        AND s.status = 'SKIPPED'
		    ) AS has_skipped_subjob,
		    EXISTS (
		      SELECT 1
		      FROM "CodeIntelIndexSubjob" s
		      WHERE s."repoIndexingJobId" = $2
		        AND s."orgId" = $3
		        AND s."workspaceId" = $4
		        AND s."repoId" = $5
		        AND s.branch = $6
		        AND s.revision = $7
		        AND s."commitHash" = $8
		        AND s.layer = 'SCIP'
		        AND s.status <> 'SKIPPED'
		        AND NOT EXISTS (
		          SELECT 1
		          FROM "CodeIntelLanguageIndex" li
		          WHERE li."codeIntelIndexId" = $1
		            AND li.status = 'READY'::"CodeIntelIndexStatus"
		            AND COALESCE(li.language, '') = COALESCE(s.language, '')
		            AND COALESCE(li."projectRoot", '') = COALESCE(s."projectRoot", '')
		            AND COALESCE(li.indexer, '') = COALESCE(s.indexer, '')
		        )
		    ) AS has_unready,
		    EXISTS (
		      SELECT 1 FROM "CodeIntelLanguageIndex"
		      WHERE "codeIntelIndexId" = $1
		        AND status = 'READY'::"CodeIntelIndexStatus"
		    ) AS has_ready,
		    EXISTS (
		      SELECT 1 FROM "CodeIntelLanguageIndex"
		      WHERE "codeIntelIndexId" = $1
		        AND status = 'FAILED'::"CodeIntelIndexStatus"
		    ) AS has_failed
		)
		UPDATE "CodeIntelIndex" ci
		SET "languageCount" = counts.language_count,
		    "symbolCount" = counts.symbol_count,
		    "occurrenceCount" = counts.occurrence_count,
		    "relationshipCount" = counts.relationship_count,
		    status = CASE
		      WHEN (counts.has_failed OR counts.has_failed_subjob OR counts.has_skipped_subjob) AND counts.has_ready THEN 'PARTIAL'::"CodeIntelIndexStatus"
		      WHEN counts.has_unready THEN 'INDEXING'::"CodeIntelIndexStatus"
		      WHEN counts.has_ready THEN 'READY'::"CodeIntelIndexStatus"
		      ELSE ci.status
		    END,
		    "indexedAt" = CASE
		      WHEN NOT counts.has_unready AND counts.has_ready THEN COALESCE(ci."indexedAt", NOW())
		      ELSE ci."indexedAt"
		    END,
		    "errorMessage" = CASE
		      WHEN NOT counts.has_unready AND counts.has_ready THEN NULL
		      ELSE ci."errorMessage"
		    END,
		    "updatedAt" = NOW()
		FROM counts
		WHERE ci.id = $1
	`,
		indexID,
		payload.RepoIndexingJobID,
		payload.OrgID,
		payload.WorkspaceID,
		payload.RepoID,
		payload.Branch,
		payload.Revision,
		payload.CommitHash,
	)
	if err != nil {
		return fmt.Errorf("refresh CodeIntelIndex counts: %w", err)
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

func commandFromPayload(payload indexsubjobtask.Payload) *string {
	if payload.Indexer == nil || payload.ProjectRoot == nil {
		return nil
	}
	command := strings.TrimSpace(*payload.Indexer)
	if root := strings.TrimSpace(*payload.ProjectRoot); root != "" {
		command += " --project-root " + root
	}
	return &command
}

func metadataInt32(values map[string]string, key string) *int32 {
	if len(values) == 0 {
		return nil
	}
	raw := strings.TrimSpace(values[key])
	if raw == "" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return nil
	}
	out := int32(n)
	return &out
}

func chunkSymbols(rows []SymbolRow, size int) [][]SymbolRow {
	if size <= 0 {
		size = 1
	}
	var out [][]SymbolRow
	for len(rows) > 0 {
		n := size
		if len(rows) < n {
			n = len(rows)
		}
		out = append(out, rows[:n])
		rows = rows[n:]
	}
	return out
}

func chunkOccurrences(rows []OccurrenceRow, size int) [][]OccurrenceRow {
	if size <= 0 {
		size = 1
	}
	var out [][]OccurrenceRow
	for len(rows) > 0 {
		n := size
		if len(rows) < n {
			n = len(rows)
		}
		out = append(out, rows[:n])
		rows = rows[n:]
	}
	return out
}

func chunkRelationships(rows []RelationshipRow, size int) [][]RelationshipRow {
	if size <= 0 {
		size = 1
	}
	var out [][]RelationshipRow
	for len(rows) > 0 {
		n := size
		if len(rows) < n {
			n = len(rows)
		}
		out = append(out, rows[:n])
		rows = rows[n:]
	}
	return out
}
