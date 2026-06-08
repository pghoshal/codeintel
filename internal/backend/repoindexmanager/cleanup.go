// DB-side + filesystem cleanup helpers for CLEANUP /
// REMOVE_INDEX jobs. Direct port of cleanupRepository
// (repoIndexManager.ts:866-887).
//
// The cascade approach delegates row-level cleanup to the FKs
// landed in the schema-parity recovery. Deleting a parent row
// (CodeGraphIndex / CodeIntelIndex / RepoIndexManifest) drops
// every subordinate row via ON DELETE CASCADE.
//
// Filesystem cleanup (C.3): rm -rf the clone directory + remove
// every Zoekt shard file whose filename starts with the
// "<orgId>_<repoId>" prefix. Read-only clone dirs (file://
// generic-git repos pointing at a local checkout) are skipped to
// match the legacy isReadOnly branch.
package repoindexmanager

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"codeintel/pkg/graphschema"
	"codeintel/pkg/repopaths"

	"github.com/jackc/pgx/v5"
)

// CleanupRepoDBState runs the per-table DELETE statements for a
// CLEANUP / REMOVE_INDEX job. Each table is its own transaction-
// less DELETE; the caller's job-lifecycle bookkeeping
// (MarkInProgress / MarkCompleted) handles the surrounding
// transitions.
//
// Order matters: tables with FKs to the parents must be removed
// first OR rely on ON DELETE CASCADE. The codeintel schema
// (S.5 + S.8 + S.9 + S.10) has CASCADE on every dependent FK
// so deleting the parent removes all dependents.
func (s *Store) CleanupRepoDBState(ctx context.Context, orgID int32, repoID int32) error {
	if orgID <= 0 {
		return fmt.Errorf("CleanupRepoDBState: orgID is required")
	}
	// CodeGraphIndex (parent of Anchor, Revision,
	// SemanticEdge, SemanticFact, SemanticHyperedge). ON DELETE
	// CASCADE on each child FK landed in S.8.
	if _, err := s.db.Exec(ctx, `DELETE FROM "CodeGraphIndex" WHERE "repoId" = $1 AND "orgId" = $2`, repoID, orgID); err != nil {
		return fmt.Errorf("CleanupRepoDBState: delete CodeGraphIndex: %w", err)
	}

	// CodeIntelIndex (parent of LanguageIndex, Symbol,
	// Occurrence, Relationship). ON DELETE CASCADE on each child
	// FK landed in S.9.
	if _, err := s.db.Exec(ctx, `DELETE FROM "CodeIntelIndex" WHERE "repoId" = $1 AND "orgId" = $2`, repoID, orgID); err != nil {
		return fmt.Errorf("CleanupRepoDBState: delete CodeIntelIndex: %w", err)
	}

	// RepoIndexManifest (parent of RepoIndexManifestFile +
	// RepoSemanticChunkManifest). ON DELETE CASCADE on each
	// child FK landed in S.10.
	if _, err := s.db.Exec(ctx, `DELETE FROM "RepoIndexManifest" WHERE "repoId" = $1 AND "orgId" = $2`, repoID, orgID); err != nil {
		return fmt.Errorf("CleanupRepoDBState: delete RepoIndexManifest: %w", err)
	}

	// Clear the per-repo denormalised state.
	if _, err := s.db.Exec(ctx, `
		UPDATE "Repo"
		SET "indexedAt"               = NULL,
		    "indexedCommitHash"       = NULL,
		    "latestIndexingJobStatus" = NULL,
		    metadata                  = COALESCE(metadata, '{}'::jsonb) - 'indexedRevisions',
		    "updatedAt"               = NOW()
		WHERE id = $1 AND "orgId" = $2
	`, repoID, orgID); err != nil {
		return fmt.Errorf("CleanupRepoDBState: clear Repo indexed-state: %w", err)
	}

	return nil
}

func (s *Store) CleanupRepoDBStateForRef(ctx context.Context, orgID int32, repoID int32, ref string) error {
	if orgID <= 0 {
		return fmt.Errorf("CleanupRepoDBStateForRef: orgID is required")
	}
	refs := cleanupRefCandidates(ref)
	if len(refs) == 0 {
		return s.CleanupRepoDBState(ctx, orgID, repoID)
	}
	if _, err := s.db.Exec(ctx, `
		WITH removed AS (
		    DELETE FROM "CodeGraphRevision"
		    WHERE "repoId" = $1
		      AND "orgId" = $2
		      AND revision = ANY($3::text[])
		    RETURNING "codeGraphIndexId"
		)
		DELETE FROM "CodeGraphIndex" g
		WHERE g."repoId" = $1
		  AND g."orgId" = $2
		  AND g.id IN (SELECT "codeGraphIndexId" FROM removed)
		  AND NOT EXISTS (
		    SELECT 1
		    FROM "CodeGraphRevision" cgr
		    WHERE cgr."codeGraphIndexId" = g.id
		      AND cgr."orgId" = g."orgId"
		      AND cgr."repoId" = g."repoId"
		  )
	`, repoID, orgID, refs); err != nil {
		return fmt.Errorf("CleanupRepoDBStateForRef: delete CodeGraphRevision/orphan CodeGraphIndex: %w", err)
	}
	if _, err := s.db.Exec(ctx, `
		DELETE FROM "CodeIntelIndex"
		WHERE "repoId" = $1
		  AND "orgId" = $2
		  AND (branch = ANY($3::text[]) OR revision = ANY($3::text[]))
	`, repoID, orgID, refs); err != nil {
		return fmt.Errorf("CleanupRepoDBStateForRef: delete CodeIntelIndex: %w", err)
	}
	if _, err := s.db.Exec(ctx, `
		DELETE FROM "RepoIndexManifest"
		WHERE "repoId" = $1
		  AND "orgId" = $2
		  AND branch = ANY($3::text[])
	`, repoID, orgID, refs); err != nil {
		return fmt.Errorf("CleanupRepoDBStateForRef: delete RepoIndexManifest: %w", err)
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE "Repo"
		SET metadata = jsonb_set(
		        COALESCE(metadata, '{}'::jsonb),
		        '{indexedRevisions}',
		        COALESCE((
		          SELECT jsonb_agg(value)
		          FROM jsonb_array_elements_text(COALESCE(metadata->'indexedRevisions', '[]'::jsonb)) value
		          WHERE value <> ALL($3::text[])
		        ), '[]'::jsonb),
		        true
		    ),
		    "updatedAt" = NOW()
		WHERE id = $1 AND "orgId" = $2
	`, repoID, orgID, refs); err != nil {
		return fmt.Errorf("CleanupRepoDBStateForRef: remove indexed revision metadata: %w", err)
	}
	return nil
}

// ListGraphSnapshotsForCleanup returns every active Nebula snapshot
// tuple that must be physically retired before the DB rows are
// cascaded away. It intentionally runs before CleanupRepoDBState so a
// failed graph delete remains retryable with the full scope tuple.
func (s *Store) ListGraphSnapshotsForCleanup(ctx context.Context, orgID int32, repoID int32) ([]graphschema.CodeGraphDeleteInput, error) {
	if orgID <= 0 {
		return nil, fmt.Errorf("ListGraphSnapshotsForCleanup: orgID is required")
	}
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT "orgId", "repoId", "workspaceId", "commitHash",
		       "schemaVersion", "builderVersion"
		FROM "CodeGraphIndex"
		WHERE "repoId" = $1
		  AND "orgId" = $2
		  AND status = 'READY'
		  AND "workspaceId" IS NOT NULL
		  AND "commitHash" IS NOT NULL
		  AND "builderVersion" IS NOT NULL
	`, repoID, orgID)
	if err != nil {
		return nil, fmt.Errorf("ListGraphSnapshotsForCleanup: %w", err)
	}
	defer rows.Close()

	var out []graphschema.CodeGraphDeleteInput
	for rows.Next() {
		var item graphschema.CodeGraphDeleteInput
		if err := rows.Scan(&item.OrgID, &item.RepoID, &item.WorkspaceID, &item.CommitHash, &item.SchemaVersion, &item.BuilderVersion); err != nil {
			return nil, fmt.Errorf("ListGraphSnapshotsForCleanup: scan: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListGraphSnapshotsForCleanup: rows: %w", err)
	}
	return out, nil
}

func (s *Store) ListGraphSnapshotsForCleanupRef(ctx context.Context, orgID int32, repoID int32, ref string) ([]graphschema.CodeGraphDeleteInput, error) {
	refs := cleanupRefCandidates(ref)
	if len(refs) == 0 {
		return s.ListGraphSnapshotsForCleanup(ctx, orgID, repoID)
	}
	rows, err := s.db.Query(ctx, `
			SELECT DISTINCT g."orgId", g."repoId", g."workspaceId", g."commitHash",
			       g."schemaVersion", g."builderVersion"
			FROM "CodeGraphIndex" g
			JOIN "CodeGraphRevision" cgr
		  ON cgr."codeGraphIndexId" = g.id
		 AND cgr."orgId" = g."orgId"
		 AND cgr."repoId" = g."repoId"
		WHERE g."repoId" = $1
		  AND g."orgId" = $2
		  AND g.status = 'READY'
		  AND cgr.revision = ANY($3::text[])
			  AND g."workspaceId" IS NOT NULL
			  AND g."commitHash" IS NOT NULL
			  AND g."builderVersion" IS NOT NULL
			  AND NOT EXISTS (
			    SELECT 1
			    FROM "CodeGraphRevision" sibling
			    WHERE sibling."codeGraphIndexId" = g.id
			      AND sibling."orgId" = g."orgId"
			      AND sibling."repoId" = g."repoId"
			      AND sibling.revision <> ALL($3::text[])
			  )
		`, repoID, orgID, refs)
	if err != nil {
		return nil, fmt.Errorf("ListGraphSnapshotsForCleanupRef: %w", err)
	}
	defer rows.Close()
	var out []graphschema.CodeGraphDeleteInput
	for rows.Next() {
		var item graphschema.CodeGraphDeleteInput
		if err := rows.Scan(&item.OrgID, &item.RepoID, &item.WorkspaceID, &item.CommitHash, &item.SchemaVersion, &item.BuilderVersion); err != nil {
			return nil, fmt.Errorf("ListGraphSnapshotsForCleanupRef: scan: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListGraphSnapshotsForCleanupRef: rows: %w", err)
	}
	return out, nil
}

func (s *Store) ListSnapshotCommitsForCleanupRef(ctx context.Context, orgID int32, repoID int32, ref string) ([]string, error) {
	refs := cleanupRefCandidates(ref)
	if len(refs) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
			SELECT DISTINCT "commitHash"
			FROM "RepoIndexManifest"
			WHERE "repoId" = $1
			  AND "orgId" = $2
			  AND branch = ANY($3::text[])
			  AND "commitHash" IS NOT NULL
			  AND NOT EXISTS (
			    SELECT 1
			    FROM "RepoIndexManifest" sibling_manifest
			    WHERE sibling_manifest."repoId" = "RepoIndexManifest"."repoId"
			      AND sibling_manifest."orgId" = "RepoIndexManifest"."orgId"
			      AND sibling_manifest."commitHash" = "RepoIndexManifest"."commitHash"
			      AND sibling_manifest.branch <> ALL($3::text[])
			  )
			  AND NOT EXISTS (
			    SELECT 1
			    FROM "CodeIntelIndex" sibling_codeintel
			    WHERE sibling_codeintel."repoId" = "RepoIndexManifest"."repoId"
			      AND sibling_codeintel."orgId" = "RepoIndexManifest"."orgId"
			      AND sibling_codeintel."commitHash" = "RepoIndexManifest"."commitHash"
			      AND (
			        COALESCE(sibling_codeintel.branch, '') <> ALL($3::text[])
			        OR sibling_codeintel.revision <> ALL($3::text[])
			      )
			  )
			  AND NOT EXISTS (
			    SELECT 1
			    FROM "CodeGraphRevision" sibling_graph
			    WHERE sibling_graph."repoId" = "RepoIndexManifest"."repoId"
			      AND sibling_graph."orgId" = "RepoIndexManifest"."orgId"
			      AND sibling_graph."commitHash" = "RepoIndexManifest"."commitHash"
			      AND sibling_graph.revision <> ALL($3::text[])
			  )
		`, repoID, orgID, refs)
	if err != nil {
		return nil, fmt.Errorf("ListSnapshotCommitsForCleanupRef: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var commit string
		if err := rows.Scan(&commit); err != nil {
			return nil, fmt.Errorf("ListSnapshotCommitsForCleanupRef: scan: %w", err)
		}
		if strings.TrimSpace(commit) != "" {
			out = append(out, commit)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSnapshotCommitsForCleanupRef: rows: %w", err)
	}
	return out, nil
}

// ErrRepoNotFound is returned when FetchRepoForCleanup is called
// for a repoID that isn't in the Repo table. The handler treats
// this as a benign no-op (the repo was already torn down by a
// previous CLEANUP / DELETE).
var ErrRepoNotFound = errors.New("repoindexmanager: repo not found")

// FetchRepoForCleanup loads the minimum repo metadata required
// by the filesystem-cleanup path. Returns ErrRepoNotFound when
// no row matches.
func (s *Store) FetchRepoForCleanup(ctx context.Context, orgID int32, repoID int32) (repopaths.Repo, error) {
	if orgID <= 0 {
		return repopaths.Repo{}, fmt.Errorf("FetchRepoForCleanup: orgID is required")
	}
	var r repopaths.Repo
	err := s.db.QueryRow(ctx, `
		SELECT "orgId", id, COALESCE("cloneUrl", ''), COALESCE("external_codeHostType"::text, '')
		FROM "Repo" WHERE id = $1 AND "orgId" = $2
	`, repoID, orgID).Scan(&r.OrgID, &r.RepoID, &r.CloneURL, &r.CodeHostType)
	if errors.Is(err, pgx.ErrNoRows) {
		return repopaths.Repo{}, ErrRepoNotFound
	}
	if err != nil {
		return repopaths.Repo{}, fmt.Errorf("FetchRepoForCleanup: %w", err)
	}
	return r, nil
}

// CleanupRepoFilesystem mirrors the second half of legacy
// cleanupRepository (repoIndexManager.ts:872-886): rm -rf the
// clone path (unless read-only) + delete every shard file
// matching the orgId_repoId_ shard-file prefix from the Zoekt
// index dir.
//
// A missing repoPath is treated as success — the legacy uses
// existsSync to guard the rm. A missing index dir is also
// treated as success. Per-shard removal errors are accumulated
// and returned as a joined error so the caller can choose
// whether to MarkFailed or proceed.
//
// The logger receives info-level lines that mirror the legacy
// "Deleting repo directory <path>" / "Deleting shard file
// <path>" lines so operator log fingerprints survive the port.
func CleanupRepoFilesystem(
	ctx context.Context,
	cfg repopaths.Config,
	repo repopaths.Repo,
	logger *slog.Logger,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	repoPath, isReadOnly, err := cfg.RepoPath(repo)
	if err != nil {
		// Bad CloneURL is fatal for FS cleanup but harmless for
		// DB cleanup — the handler runs DB cleanup independently.
		return fmt.Errorf("CleanupRepoFilesystem: resolve RepoPath: %w", err)
	}

	if !isReadOnly && repoPath != "" {
		if _, statErr := os.Stat(repoPath); statErr == nil {
			logger.Info("repo-index: deleting repo directory",
				"path", repoPath,
				"repo_id", repo.RepoID,
				"org_id", repo.OrgID,
			)
			if rmErr := os.RemoveAll(repoPath); rmErr != nil {
				return fmt.Errorf("CleanupRepoFilesystem: rm -rf %s: %w", repoPath, rmErr)
			}
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("CleanupRepoFilesystem: stat %s: %w", repoPath, statErr)
		}
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	indexPath := cfg.ZoektIndexPath(repo)
	if indexPath == "" {
		return nil
	}
	prefix := repopaths.ShardFilePrefix(repo.OrgID, repo.RepoID)
	entries, err := os.ReadDir(indexPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("CleanupRepoFilesystem: readdir %s: %w", indexPath, err)
	}

	var rmErrs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		shardPath := filepath.Join(indexPath, entry.Name())
		logger.Info("repo-index: deleting shard file",
			"path", shardPath,
			"repo_id", repo.RepoID,
			"org_id", repo.OrgID,
		)
		if rmErr := os.Remove(shardPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			rmErrs = append(rmErrs, fmt.Errorf("rm %s: %w", shardPath, rmErr))
		}
	}
	if len(rmErrs) > 0 {
		return fmt.Errorf("CleanupRepoFilesystem: shard removal errors: %w", errors.Join(rmErrs...))
	}
	return nil
}

func CleanupRepoFilesystemForRef(
	ctx context.Context,
	cfg repopaths.Config,
	repo repopaths.Repo,
	ref string,
	logger *slog.Logger,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	branches := cleanupRefCandidates(ref)
	if len(branches) == 0 {
		return CleanupRepoFilesystem(ctx, cfg, repo, logger)
	}
	indexPath := cfg.ZoektIndexPath(repo)
	if indexPath == "" {
		return nil
	}
	entries, err := os.ReadDir(indexPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("CleanupRepoFilesystemForRef: readdir %s: %w", indexPath, err)
	}
	prefix := repopaths.ShardFilePrefix(repo.OrgID, repo.RepoID)
	branchTokens := cleanupShardBranchTokens(branches)
	var rmErrs []error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".zoekt") {
			continue
		}
		if !shardNameMatchesAnyBranch(name, prefix, branchTokens) {
			continue
		}
		shardPath := filepath.Join(indexPath, name)
		logger.Info("repo-index: deleting branch shard file",
			"path", shardPath,
			"repo_id", repo.RepoID,
			"org_id", repo.OrgID,
			"ref", ref,
		)
		if rmErr := os.Remove(shardPath); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			rmErrs = append(rmErrs, fmt.Errorf("rm %s: %w", shardPath, rmErr))
		}
	}
	if len(rmErrs) > 0 {
		return fmt.Errorf("CleanupRepoFilesystemForRef: shard removal errors: %w", errors.Join(rmErrs...))
	}
	return nil
}

// CleanupRepoSnapshots removes immutable split-index source
// snapshots for the repo. The exact commit list is already gone
// after DB cleanup, so this deliberately removes the repo-scoped
// snapshot directory. It is safe under the org-directory EFS
// layout because the path contains orgId and repoId.
func CleanupRepoSnapshots(
	ctx context.Context,
	cfg repopaths.Config,
	repo repopaths.Repo,
	logger *slog.Logger,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	root := cfg.RevisionSnapshotRepoRoot(repo.OrgID, repo.RepoID)
	if strings.TrimSpace(root) == "" {
		return nil
	}
	if _, statErr := os.Stat(root); statErr == nil {
		logger.Info("repo-index: deleting revision snapshot directory",
			"path", root,
			"repo_id", repo.RepoID,
			"org_id", repo.OrgID,
		)
		if rmErr := os.RemoveAll(root); rmErr != nil {
			return fmt.Errorf("CleanupRepoSnapshots: rm -rf %s: %w", root, rmErr)
		}
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("CleanupRepoSnapshots: stat %s: %w", root, statErr)
	}
	return nil
}

func CleanupRepoSnapshotsForCommits(
	ctx context.Context,
	cfg repopaths.Config,
	repo repopaths.Repo,
	commits []string,
	logger *slog.Logger,
) error {
	if len(commits) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, commit := range commits {
		commit = strings.TrimSpace(commit)
		if commit == "" || seen[commit] {
			continue
		}
		seen[commit] = true
		path := cfg.RevisionSnapshotPath(repo.OrgID, repo.RepoID, commit)
		if _, statErr := os.Stat(path); statErr == nil {
			logger.Info("repo-index: deleting revision snapshot",
				"path", path,
				"repo_id", repo.RepoID,
				"org_id", repo.OrgID,
				"commit", shortCleanupCommit(commit),
			)
			if rmErr := os.RemoveAll(path); rmErr != nil {
				return fmt.Errorf("CleanupRepoSnapshotsForCommits: rm -rf %s: %w", path, rmErr)
			}
		} else if !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("CleanupRepoSnapshotsForCommits: stat %s: %w", path, statErr)
		}
	}
	return nil
}

func cleanupRefCandidates(ref string) []string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 3)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	add(ref)
	add(strings.TrimPrefix(ref, "refs/heads/"))
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		add("refs/heads/" + strings.TrimPrefix(ref, "/"))
	}
	return out
}

func cleanupShardBranchTokens(branches []string) []string {
	seen := map[string]bool{}
	for _, branch := range branches {
		branch = strings.TrimSpace(branch)
		if branch == "" {
			continue
		}
		candidates := []string{
			branch,
			strings.TrimPrefix(branch, "refs/heads/"),
			strings.ReplaceAll(branch, "/", "_"),
			strings.ReplaceAll(strings.TrimPrefix(branch, "refs/heads/"), "/", "_"),
			strings.ReplaceAll(branch, "/", "-"),
			strings.ReplaceAll(strings.TrimPrefix(branch, "refs/heads/"), "/", "-"),
		}
		for _, candidate := range candidates {
			candidate = strings.Trim(candidate, "_- /")
			if candidate != "" {
				seen[candidate] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	return out
}

func shardNameMatchesAnyBranch(name, prefix string, tokens []string) bool {
	suffix := strings.TrimPrefix(name, prefix)
	for _, token := range tokens {
		if strings.HasPrefix(suffix, token+"_") ||
			strings.HasPrefix(suffix, token+"-") ||
			strings.Contains(suffix, "_"+token+"_") ||
			strings.Contains(suffix, "-"+token+"-") {
			return true
		}
	}
	return false
}
