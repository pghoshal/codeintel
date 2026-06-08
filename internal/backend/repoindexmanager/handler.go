// asynq handler for the repo-index-queue. Mirrors the
// connection-sync handler shape:
//   - Decode payload.
//   - markInProgress with status guard.
//   - Dispatch on payload.Type.
//   - On success, MarkCompleted + RefreshRepoLatestIndexingJobStatus.
//   - On error, MarkFailed + bubble the error so asynq's retry
//     policy can decide whether to requeue.
//
// Supported job types:
//   - CLEANUP / REMOVE_INDEX: full DB cascade + filesystem scrub
//     plus graph snapshot retirement.
//   - INDEX: real git clone into the EFS/shared working tree,
//     branch-policy resolution from Repo.metadata, immutable
//     per-commit snapshots, and split subjobs for Zoekt,
//     AST/tree-sitter, SCIP where workers are available,
//     graph merge, and activation.
package repoindexmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"codeintel/internal/backend/gitclone"
	"codeintel/internal/backend/indexplanner"
	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/scipprojectdetect"
	"codeintel/pkg/graphschema"
	"codeintel/pkg/repoindex"
	"codeintel/pkg/repopaths"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/gobwas/glob"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// ErrIndexReadOnlyRepo is returned when an INDEX job targets a
// repo whose RepoPath resolves to a read-only location (legacy:
// generic-git file:// repos pointing at a local checkout the
// operator owns). The legacy treats this as success-without-
// clone; the codeintel port surfaces a typed sentinel so the
// dispatch path can short-circuit.
var ErrIndexReadOnlyRepo = errors.New("repoindexmanager: INDEX target is a read-only local repo; nothing to clone")

var errIndexContinuesAsSubjobs = errors.New("repoindexmanager: INDEX continues via split subjobs")

const maxIndexedRevisionsPerRepo = 64

// Handler is the asynq worker handler. Holds the typed store,
// the path-resolution config (loaded from env at boot), and a
// logger.
type Handler struct {
	store        *Store
	pathsCfg     repopaths.Config
	graphRetirer graphSnapshotRetirer
	logger       *slog.Logger
}

type graphSnapshotRetirer interface {
	MarkSnapshotForDeletion(ctx context.Context, input graphschema.CodeGraphDeleteInput) error
}

type repoIndexMetadata struct {
	Branches []string `json:"branches,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

type resolvedIndexRevision struct {
	Branch     string
	Revision   string
	CommitHash string
}

// NewHandler constructs a Handler. The supplied Store wraps the
// codeintel-backend's pgx pool. The pathsCfg is loaded from env
// (LoadConfigFromEnv) at process boot — passing it in here keeps
// the worker testable with synthetic tmpdirs.
func NewHandler(store *Store, pathsCfg repopaths.Config, logger *slog.Logger) *Handler {
	return NewHandlerWithGraphRetirer(store, pathsCfg, nil, logger)
}

func NewHandlerWithGraphRetirer(store *Store, pathsCfg repopaths.Config, graphRetirer graphSnapshotRetirer, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		store:        store,
		pathsCfg:     pathsCfg,
		graphRetirer: graphRetirer,
		logger:       logger.With("component", "repo-index"),
	}
}

// AsynqHandlerFunc returns the asynq.HandlerFunc-shaped callback
// to register on asynqueues.QueueRepoIndex.
func (h *Handler) AsynqHandlerFunc() func(context.Context, *asynq.Task) error {
	return h.Handle
}

// Handle is the per-task entrypoint.
func (h *Handler) Handle(ctx context.Context, t *asynq.Task) error {
	if h == nil || h.store == nil {
		return errors.New("repoindexmanager: handler not configured")
	}
	payload, err := repoindex.UnmarshalLegacyForBackfill(t.Payload())
	if err != nil {
		return fmt.Errorf("payload decode: %w", err)
	}
	scope, err := h.store.FetchJobScope(ctx, payload.JobID)
	if err != nil {
		if errors.Is(err, ErrJobInTerminalState) {
			h.logger.Info("repo-index job missing or terminal before scope validation; skipping", "jobId", payload.JobID)
			return nil
		}
		return fmt.Errorf("FetchJobScope: %w", err)
	}
	if payload.OrgID == 0 {
		payload.OrgID = scope.OrgID
	}
	if payload.RepoID == 0 {
		payload.RepoID = scope.RepoID
	}
	if payload.OrgID != scope.OrgID || payload.RepoID != scope.RepoID || string(payload.Type) != scope.Type {
		errMsg := fmt.Sprintf("repo-index payload scope mismatch jobId=%s payload=(org:%d repo:%d type:%s) db=(org:%d repo:%d type:%s)",
			payload.JobID,
			payload.OrgID,
			payload.RepoID,
			payload.Type,
			scope.OrgID,
			scope.RepoID,
			scope.Type,
		)
		if mfErr := h.store.MarkFailedScoped(ctx, payload.JobID, scope.OrgID, scope.RepoID, scope.Type, errMsg); mfErr != nil {
			h.logger.Error("MarkFailedScoped after scope mismatch failed", "err", mfErr, "jobId", payload.JobID)
		}
		_ = h.store.RefreshRepoLatestIndexingJobStatus(ctx, scope.RepoID)
		return fmt.Errorf("%w: repo-index payload scope mismatch jobId=%s payload=(org:%d repo:%d type:%s) db=(org:%d repo:%d type:%s)",
			asynq.SkipRetry,
			payload.JobID,
			payload.OrgID,
			payload.RepoID,
			payload.Type,
			scope.OrgID,
			scope.RepoID,
			scope.Type,
		)
	}
	h.logger.Info("repo-index task received",
		"jobId", payload.JobID,
		"type", payload.Type,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"ref", payload.Ref,
	)

	// Status guard + transition to IN_PROGRESS. The legacy fails
	// the whole job if the status guard rejects; we propagate
	// the typed sentinel so asynq can choose between retry vs.
	// discard.
	if err := h.store.MarkInProgressScoped(ctx, payload.JobID, payload.OrgID, payload.RepoID, string(payload.Type)); err != nil {
		if errors.Is(err, ErrJobInTerminalState) {
			h.logger.Info("repo-index job already terminal; skipping", "jobId", payload.JobID)
			return nil
		}
		return fmt.Errorf("MarkInProgressScoped: %w", err)
	}

	// Dispatch on payload.Type.
	var dispatchErr error
	switch payload.Type {
	case repoindex.JobTypeCleanup, repoindex.JobTypeRemoveIndex:
		dispatchErr = h.dispatchCleanupScoped(ctx, payload.OrgID, payload.RepoID, payload.Ref)
	case repoindex.JobTypeIndex:
		dispatchErr = h.dispatchIndex(ctx, payload)
	default:
		// repoindex.Unmarshal already validates the enum; this
		// is defence-in-depth.
		dispatchErr = fmt.Errorf("repoindexmanager: unknown job type %q", payload.Type)
	}

	if dispatchErr != nil {
		if errors.Is(dispatchErr, errIndexContinuesAsSubjobs) {
			if err := h.store.RefreshRepoLatestIndexingJobStatus(ctx, payload.RepoID); err != nil {
				h.logger.Warn("RefreshRepoLatestIndexingJobStatus failed", "err", err)
			}
			h.logger.Info("repo-index task planned split subjobs",
				"jobId", payload.JobID,
				"type", payload.Type,
				"orgId", payload.OrgID,
				"repoId", payload.RepoID,
			)
			return nil
		}
		if mfErr := h.store.MarkFailedScoped(ctx, payload.JobID, payload.OrgID, payload.RepoID, string(payload.Type), dispatchErr.Error()); mfErr != nil {
			h.logger.Error("MarkFailedScoped failed", "err", mfErr)
		}
		// Refresh denormalised state so dashboards see FAILED.
		_ = h.store.RefreshRepoLatestIndexingJobStatus(ctx, payload.RepoID)
		return fmt.Errorf("dispatch %s: %w", payload.Type, dispatchErr)
	}

	// Success path.
	if err := h.store.MarkCompletedScoped(ctx, payload.JobID, payload.OrgID, payload.RepoID, string(payload.Type)); err != nil {
		return fmt.Errorf("MarkCompletedScoped: %w", err)
	}
	if err := h.store.RefreshRepoLatestIndexingJobStatus(ctx, payload.RepoID); err != nil {
		// Non-fatal: the row is already COMPLETED. Log + move on.
		h.logger.Warn("RefreshRepoLatestIndexingJobStatus failed", "err", err)
	}
	h.logger.Info("repo-index task completed",
		"jobId", payload.JobID,
		"type", payload.Type,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
	)
	return nil
}

// dispatchCleanup runs the legacy cleanupRepository sequence:
// DB-side row deletes (CodeGraphIndex / CodeIntelIndex /
// RepoIndexManifest + clear Repo indexed-state) followed by the
// filesystem cleanup (clone dir + per-repo Zoekt shards). The
// per-step error from CleanupRepoFilesystem is treated as fatal
// for the job — operators rely on a single FAILED row to spot a
// stuck cleanup; silently swallowing FS errors hides shard
// leaks.
func (h *Handler) dispatchCleanup(ctx context.Context, orgID int32, repoID int32) error {
	return h.dispatchCleanupScoped(ctx, orgID, repoID, "")
}

func (h *Handler) dispatchCleanupScoped(ctx context.Context, orgID int32, repoID int32, ref string) error {
	if orgID <= 0 {
		return fmt.Errorf("cleanup requires org scope")
	}
	repo, err := h.store.FetchRepoForCleanup(ctx, orgID, repoID)
	if errors.Is(err, ErrRepoNotFound) {
		// Repo row already gone — DB cleanup is a no-op + we
		// can't resolve a clone path without the metadata.
		// Treat as success so REMOVE_INDEX is idempotent.
		h.logger.Info("repo-index cleanup: repo row missing; treating as success",
			"repo_id", repoID,
		)
		return nil
	}
	if err != nil {
		return fmt.Errorf("FetchRepoForCleanup: %w", err)
	}

	ref = strings.TrimSpace(ref)
	snapshots, err := h.store.ListGraphSnapshotsForCleanupRef(ctx, orgID, repoID, ref)
	if err != nil {
		return err
	}
	if len(snapshots) > 0 && h.graphRetirer == nil {
		return fmt.Errorf("repo-index cleanup: graph snapshot retirer is not configured for %d active snapshot(s)", len(snapshots))
	}
	if h.graphRetirer != nil {
		for _, snapshot := range snapshots {
			if err := h.graphRetirer.MarkSnapshotForDeletion(ctx, snapshot); err != nil {
				return fmt.Errorf("delete graph snapshot %s/%d@%s: %w", snapshot.WorkspaceID, snapshot.RepoID, shortCleanupCommit(snapshot.CommitHash), err)
			}
			h.logger.Info("repo-index cleanup: deleted graph snapshot",
				"repo_id", repoID,
				"org_id", snapshot.OrgID,
				"workspace_id", snapshot.WorkspaceID,
				"commit", shortCleanupCommit(snapshot.CommitHash),
			)
		}
	}

	var snapshotCommits []string
	if ref != "" {
		var err error
		snapshotCommits, err = h.store.ListSnapshotCommitsForCleanupRef(ctx, orgID, repoID, ref)
		if err != nil {
			return err
		}
	}

	if err := h.store.CleanupRepoDBStateForRef(ctx, orgID, repoID, ref); err != nil {
		return err
	}
	if err := CleanupRepoFilesystemForRef(ctx, h.pathsCfg, repo, ref, h.logger); err != nil {
		return err
	}
	if ref == "" {
		if err := CleanupRepoSnapshots(ctx, h.pathsCfg, repo, h.logger); err != nil {
			return err
		}
	} else if err := CleanupRepoSnapshotsForCommits(ctx, h.pathsCfg, repo, snapshotCommits, h.logger); err != nil {
		return err
	}
	return nil
}

func shortCleanupCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// dispatchIndex runs the INDEX job: clone the repo into the
// resolved working directory and record the observed HEAD SHA on
// the Repo row.
//
// Honest scope boundary (C.4a): this produces a real on-disk
// working tree + a real indexedCommitHash that operators can
// verify via GET /api/repos/{id}/status. It does NOT yet
// produce a Zoekt shard (so /api/search will not return hits)
// nor a SCIP code-intel index (so xref queries return nothing).
// Both land in follow-on slices (C.4b Zoekt, C.4c SCIP).
//
// On a re-INDEX of an already-cloned repo: the existing clone
// directory is removed first (re-clone semantics). The fetch-
// vs-clone branch + incremental update lives in a follow-on
// slice once the indexer needs it; for the foundation slice the
// always-fresh-clone path is simpler and still parity-correct
// (every observable post-INDEX state is the same as legacy's
// "clone followed by index" path).
func (h *Handler) dispatchIndex(ctx context.Context, payload repoindex.TaskPayload) error {
	repoID := payload.RepoID
	repo, err := h.store.FetchRepoForCleanup(ctx, payload.OrgID, repoID)
	if errors.Is(err, ErrRepoNotFound) {
		// INDEX on a missing Repo row is a hard error (the row
		// must exist for the producer to have enqueued the job
		// in the first place). Surface a typed message rather
		// than success.
		return fmt.Errorf("INDEX: repo %d not found", repoID)
	}
	if err != nil {
		return fmt.Errorf("FetchRepoForCleanup: %w", err)
	}

	dest, isReadOnly, err := h.pathsCfg.RepoPath(repo)
	if err != nil {
		return fmt.Errorf("INDEX: resolve RepoPath: %w", err)
	}
	if isReadOnly {
		// A genericGitHost file:// repo points at a local
		// checkout the operator owns. Legacy treats this as a
		// no-op clone (the working tree already exists at the
		// path). We mirror that semantically but still record
		// HEAD on the Repo row so /status reflects the latest
		// commit the operator's local repo advertises.
		commit, branch, headErr := readLocalHead(dest)
		if headErr != nil {
			return fmt.Errorf("INDEX read-only HEAD resolve: %w", headErr)
		}
		h.logger.Info("repo-index INDEX: read-only repo, recording HEAD without clone",
			"repo_id", repoID,
			"path", dest,
			"commit", commit,
			"branch", branch,
		)
		return h.planSplitIndex(ctx, payload, repo, dest, commit, branch)
	}

	// Fresh-clone semantics: nuke any stale working tree from a
	// previous run. The legacy does the same when
	// indexedCommitHash is unset OR a re-INDEX is requested.
	if _, statErr := os.Stat(dest); statErr == nil {
		if err := os.RemoveAll(dest); err != nil {
			return fmt.Errorf("INDEX: clear stale working tree %s: %w", dest, err)
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("INDEX: stat %s: %w", dest, statErr)
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("INDEX: mkdir %s: %w", dest, err)
	}

	cloneTimeout := gitCloneTimeout()
	cloneDepth := gitCloneDepth()
	h.logger.Info("repo-index INDEX: cloning",
		"repo_id", repoID,
		"clone_url", repo.CloneURL,
		"dest", dest,
		"depth", cloneDepth,
		"timeout", cloneTimeout.String(),
	)
	cloneCtx, cancelClone := context.WithTimeout(ctx, cloneTimeout)
	defer cancelClone()
	res, err := gitclone.Clone(cloneCtx, gitclone.Request{
		CloneURL:    repo.CloneURL,
		Destination: dest,
		// Indexing consumes the working tree at HEAD, not remote
		// history. Default to depth=1 so reindexing a large public
		// repo cannot pin a backend worker in full-history clone.
		Depth: cloneDepth,
		// No branch restriction; defaults to the remote's HEAD.
		// No credentials yet; anonymous clones work for public
		// repos and file:// transports. Cred resolution from
		// the secret-refs layer is the next slice (C.4-creds).
	})
	if err != nil {
		// Clone failed: leave the destination directory empty
		// so the next INDEX attempt re-clones cleanly. asynq
		// retry policy decides whether to re-enqueue.
		_ = os.RemoveAll(dest)
		return fmt.Errorf("INDEX clone: %w", err)
	}

	h.logger.Info("repo-index INDEX: clone successful",
		"repo_id", repoID,
		"commit_hash", res.CommitHash,
		"branch", res.Branch,
		"depth", cloneDepth,
		"timeout", cloneTimeout.String(),
	)
	return h.planSplitIndex(ctx, payload, repo, res.WorkTree, res.CommitHash, res.Branch)
}

func gitCloneDepth() int {
	raw := strings.TrimSpace(os.Getenv("CODEINTEL_GIT_CLONE_DEPTH"))
	if raw == "" {
		return 1
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 0 {
		return 1
	}
	if parsed > 10_000 {
		return 10_000
	}
	return parsed
}

func gitCloneTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CODEINTEL_GIT_CLONE_TIMEOUT_SECONDS"))
	if raw == "" {
		return 10 * time.Minute
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return 10 * time.Minute
	}
	if parsed < 30 {
		parsed = 30
	}
	if parsed > 7200 {
		parsed = 7200
	}
	return time.Duration(parsed) * time.Second
}

func supportsZoektFileDelta() bool {
	value := strings.TrimSpace(os.Getenv("CODEINTEL_ZOEKT_FILE_DELTA"))
	if value == "" {
		return true
	}
	return strings.EqualFold(value, "true")
}

func filterSCIPProjectsForDelta(projects []indexplanner.SCIPProject, plan *deltaReindexPlan) []indexplanner.SCIPProject {
	if plan == nil || plan.SCIP.Strategy == "FULL_REPO" {
		return projects
	}
	if plan.SCIP.Strategy == "NOOP" {
		return nil
	}
	roots := map[string]bool{}
	for _, root := range plan.SCIP.ProjectRoots {
		roots[indexplanner.TrimProjectRoot(root)] = true
	}
	out := make([]indexplanner.SCIPProject, 0, len(projects))
	for _, project := range projects {
		if roots[indexplanner.TrimProjectRoot(project.ProjectRoot)] {
			out = append(out, project)
		}
	}
	return out
}

func (h *Handler) planSplitIndex(ctx context.Context, payload repoindex.TaskPayload, repo repopaths.Repo, worktree, commit, branch string) error {
	scope, err := h.store.FetchRepoIndexScope(ctx, payload.RepoID)
	if err != nil {
		return err
	}
	if scope.OrgID != repo.OrgID {
		return fmt.Errorf("INDEX: repo org drift: repo=%d scope=%d", repo.OrgID, scope.OrgID)
	}
	revisions, err := resolveIndexRevisions(worktree, scope, commit, branch)
	if err != nil {
		return fmt.Errorf("INDEX: resolve selected revisions: %w", err)
	}
	workspaceID := scope.WorkspaceID
	planned := make([]indexplanner.Revision, 0, len(revisions))
	type skippedForRevision struct {
		branch string
		commit string
		items  []indexplanner.SCIPProject
	}
	var skipped []skippedForRevision
	var totalFiles int32
	for _, selected := range revisions {
		snapshotPath := h.pathsCfg.RevisionSnapshotPath(repo.OrgID, repo.RepoID, selected.CommitHash)
		fileCount, err := materializeCommitSnapshot(worktree, selected.CommitHash, snapshotPath)
		if err != nil {
			return fmt.Errorf("INDEX: materialize revision snapshot %s: %w", selected.Branch, err)
		}
		files, err := buildManifestFilesForCommit(worktree, selected.CommitHash)
		if err != nil {
			return fmt.Errorf("INDEX: build manifest files %s: %w", selected.Branch, err)
		}
		if len(files) != int(fileCount) {
			h.logger.Warn("manifest file count differs from materialized snapshot count",
				"repo_id", repo.RepoID,
				"org_id", repo.OrgID,
				"branch", selected.Branch,
				"commit", selected.CommitHash,
				"manifest_file_count", len(files),
				"snapshot_file_count", fileCount,
			)
		}
		totalFiles += int32(len(files))
		previousFiles, err := h.store.FetchPreviousReadyManifestFiles(ctx, repo.OrgID, repo.RepoID, scope.WorkspaceID, selected.Branch)
		if err != nil {
			return fmt.Errorf("INDEX: fetch previous manifest files %s: %w", selected.Branch, err)
		}
		deltaPlan := buildManifestDeltaPlan(previousFiles, files, supportsZoektFileDelta())
		if err := deltaPlan.validate(); err != nil {
			return fmt.Errorf("INDEX: build delta plan %s: %w", selected.Branch, err)
		}
		if deltaPlan.Mode == "NOOP" && manifestHasSemanticCandidates(files) {
			health, err := h.store.FetchSemanticIndexHealth(ctx, repo.OrgID, repo.RepoID, scope.WorkspaceID, selected.Branch, selected.CommitHash)
			if err != nil {
				return fmt.Errorf("INDEX: fetch semantic health %s: %w", selected.Branch, err)
			}
			if health.NeedsRepair() {
				deltaPlan = forceSemanticRepairPlan(deltaPlan, files, "Active SCIP or graph semantic index is missing or hollow for an unchanged revision.")
				h.logger.Warn("repo-index INDEX: revision unchanged but semantic index needs repair; planning SCIP/graph repair",
					"repo_id", repo.RepoID,
					"org_id", repo.OrgID,
					"workspace_id", scope.WorkspaceID,
					"branch", selected.Branch,
					"commit", selected.CommitHash,
					"scip_found", health.SCIPFound,
					"scip_symbol_count", health.SCIPSymbolCount,
					"scip_occurrence_count", health.SCIPOccurrenceCount,
					"scip_relationship_count", health.SCIPRelationshipCount,
					"graph_found", health.GraphFound,
					"graph_anchor_count", health.GraphAnchorCount,
					"graph_linked_edge_count", health.GraphLinkedEdgeCount,
				)
			}
		}
		if deltaPlan.Mode == "NOOP" {
			if err := h.store.RecordUsableIndexedRevision(ctx, repo.OrgID, repo.RepoID, scope.WorkspaceID, selected.Branch, selected.CommitHash); err != nil {
				return fmt.Errorf("INDEX: refresh unchanged indexed revision %s: %w", selected.Branch, err)
			}
			h.logger.Info("repo-index INDEX: revision unchanged; skipping producer subjobs",
				"repo_id", repo.RepoID,
				"org_id", repo.OrgID,
				"workspace_id", scope.WorkspaceID,
				"branch", selected.Branch,
				"commit", selected.CommitHash,
				"file_count", len(files),
			)
			continue
		}
		if err := h.store.InsertPendingManifest(ctx, pendingManifestInput{
			ID:          uuid.NewString(),
			JobID:       payload.JobID,
			OrgID:       repo.OrgID,
			RepoID:      repo.RepoID,
			WorkspaceID: scope.WorkspaceID,
			Branch:      selected.Branch,
			CommitHash:  selected.CommitHash,
			FileCount:   int32(len(files)),
			Plan:        deltaPlan,
			Files:       files,
		}); err != nil {
			return err
		}
		scipPlan, err := scipprojectdetect.DetectSnapshotPlan(snapshotPath, scipprojectdetect.ConfigFromEnv())
		if err != nil {
			return fmt.Errorf("INDEX: detect SCIP projects %s: %w", selected.Branch, err)
		}
		planned = append(planned, indexplanner.Revision{
			WorkspaceID:      &workspaceID,
			Branch:           selected.Branch,
			Revision:         selected.Revision,
			CommitHash:       selected.CommitHash,
			RunZoekt:         deltaPlan.Zoekt.Strategy != zoektStrategyNoop,
			RunASTTreeSitter: deltaPlan.Graph.Strategy != "NOOP",
			RunGraphMerge:    true,
			RunActivate:      true,
			SCIPProjects:     filterSCIPProjectsForDelta(scipPlan.Runnable, deltaPlan),
		})
		if len(scipPlan.Skipped) > 0 {
			skipped = append(skipped, skippedForRevision{
				branch: selected.Branch,
				commit: selected.CommitHash,
				items:  scipPlan.Skipped,
			})
		}
	}
	if len(planned) == 0 {
		h.logger.Info("repo-index INDEX: all selected revisions unchanged; no split subjobs planned",
			"repo_id", repo.RepoID,
			"org_id", repo.OrgID,
			"workspace_id", scope.WorkspaceID,
			"revision_count", len(revisions),
			"selected_revisions", revisionNames(revisions),
			"file_count_total", totalFiles,
		)
		return nil
	}
	subjobStore := indexsubjobs.NewStore(h.store.db)
	if _, err := indexplanner.PlanAndPersist(ctx, subjobStore, indexplanner.Input{
		RepoIndexingJobID: payload.JobID,
		OrgID:             repo.OrgID,
		RepoID:            repo.RepoID,
		MaxAttempts:       3,
		Revisions:         planned,
	}); err != nil {
		return fmt.Errorf("INDEX: persist split plan: %w", err)
	}
	for _, entry := range skipped {
		if err := h.persistSkippedSCIPProjects(ctx, subjobStore, payload.JobID, repo, workspaceID, entry.branch, entry.commit, entry.items); err != nil {
			return fmt.Errorf("INDEX: persist skipped SCIP projects %s: %w", entry.branch, err)
		}
	}
	h.logger.Info("repo-index INDEX: split subjobs planned",
		"repo_id", repo.RepoID,
		"org_id", repo.OrgID,
		"workspace_id", scope.WorkspaceID,
		"revision_count", len(revisions),
		"selected_revisions", revisionNames(revisions),
		"file_count_total", totalFiles,
	)
	return errIndexContinuesAsSubjobs
}

func (h *Handler) persistSkippedSCIPProjects(ctx context.Context, store *indexsubjobs.Store, jobID string, repo repopaths.Repo, workspaceID, branchRef, commit string, projects []indexplanner.SCIPProject) error {
	if len(projects) == 0 {
		return nil
	}
	subjobs, err := indexplanner.Build(indexplanner.Input{
		RepoIndexingJobID: jobID,
		OrgID:             repo.OrgID,
		RepoID:            repo.RepoID,
		MaxAttempts:       1,
		Revisions: []indexplanner.Revision{{
			WorkspaceID:  &workspaceID,
			Branch:       branchRef,
			Revision:     branchRef,
			CommitHash:   commit,
			SCIPProjects: projects,
		}},
	})
	if err != nil {
		return err
	}
	for _, subjob := range subjobs {
		if subjob.Layer != indexsubjobs.LayerSCIP {
			return fmt.Errorf("unexpected skipped layer %s", subjob.Layer)
		}
		if err := store.UpsertSkipped(ctx, subjob, "WORKER_CLASS_UNAVAILABLE", "SCIP worker class is not enabled for this backend/indexer deployment"); err != nil {
			return err
		}
	}
	return nil
}

// readLocalHead resolves HEAD on a working tree that already
// exists on disk (the read-only file:// case). Direct go-git
// PlainOpen + Head() — no shell-out.
func readLocalHead(path string) (commit, branch string, err error) {
	// Local import-free: keep the gitclone package as the only
	// go-git boundary. The function lives here because it's a
	// dispatchIndex-only path; if it grows we'll move it.
	res, err := gitclone.OpenHead(path)
	if err != nil {
		return "", "", err
	}
	return res.CommitHash, res.Branch, nil
}

func normalizeBranchRef(branch, fallback string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		branch = strings.TrimSpace(fallback)
	}
	if branch == "" {
		branch = "main"
	}
	if strings.HasPrefix(branch, "refs/heads/") {
		return branch
	}
	return "refs/heads/" + strings.TrimPrefix(branch, "/")
}

func resolveIndexRevisions(worktree string, scope RepoIndexScope, headCommit, headBranch string) ([]resolvedIndexRevision, error) {
	var metadata repoIndexMetadata
	if len(scope.Metadata) > 0 {
		if err := json.Unmarshal(scope.Metadata, &metadata); err != nil {
			return nil, fmt.Errorf("decode repo metadata: %w", err)
		}
	}
	repo, err := git.PlainOpen(worktree)
	if err != nil {
		return nil, fmt.Errorf("open worktree refs: %w", err)
	}
	branches, tags, err := collectGitRefs(repo)
	if err != nil {
		return nil, err
	}
	out := map[string]resolvedIndexRevision{}
	if len(metadata.Branches) > 0 {
		compiled, err := compileGlobs(metadata.Branches)
		if err != nil {
			return nil, fmt.Errorf("compile branch globs: %w", err)
		}
		for _, name := range sortedKeys(branches) {
			if matchesAny(compiled, name) {
				ref := normalizeBranchRef(name, scope.DefaultBranch)
				out[ref] = resolvedIndexRevision{Branch: ref, Revision: ref, CommitHash: branches[name]}
			}
		}
	} else {
		name := strings.TrimPrefix(normalizeBranchRef(headBranch, scope.DefaultBranch), "refs/heads/")
		hash := branches[name]
		if hash == "" {
			hash = headCommit
		}
		ref := normalizeBranchRef(name, scope.DefaultBranch)
		out[ref] = resolvedIndexRevision{Branch: ref, Revision: ref, CommitHash: hash}
	}
	if len(metadata.Tags) > 0 {
		compiled, err := compileGlobs(metadata.Tags)
		if err != nil {
			return nil, fmt.Errorf("compile tag globs: %w", err)
		}
		for _, name := range sortedKeys(tags) {
			if matchesAny(compiled, name) {
				ref := "refs/tags/" + strings.TrimPrefix(name, "refs/tags/")
				out[ref] = resolvedIndexRevision{Branch: ref, Revision: ref, CommitHash: tags[name]}
			}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no branches/tags matched the repository index policy")
	}
	keys := make([]string, 0, len(out))
	for key := range out {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > maxIndexedRevisionsPerRepo {
		keys = keys[:maxIndexedRevisionsPerRepo]
	}
	revisions := make([]resolvedIndexRevision, 0, len(keys))
	for _, key := range keys {
		revisions = append(revisions, out[key])
	}
	return revisions, nil
}

func collectGitRefs(repo *git.Repository) (map[string]string, map[string]string, error) {
	branches := map[string]string{}
	tags := map[string]string{}
	iter, err := repo.References()
	if err != nil {
		return nil, nil, fmt.Errorf("list git refs: %w", err)
	}
	defer iter.Close()
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if ref == nil || ref.Type() != plumbing.HashReference {
			return nil
		}
		name := ref.Name()
		switch {
		case name.IsBranch():
			short := name.Short()
			if short != "" {
				branches[short] = ref.Hash().String()
			}
		case name.IsRemote():
			short := name.Short()
			if short == "" || strings.HasSuffix(short, "/HEAD") {
				return nil
			}
			if idx := strings.IndexByte(short, '/'); idx >= 0 {
				short = short[idx+1:]
			}
			if short != "" {
				branches[short] = ref.Hash().String()
			}
		case name.IsTag():
			short := name.Short()
			if short != "" {
				hash := ref.Hash()
				if tag, err := repo.TagObject(hash); err == nil {
					if target, err := tag.Commit(); err == nil {
						hash = target.Hash
					}
				}
				tags[short] = hash.String()
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("iterate git refs: %w", err)
	}
	return branches, tags, nil
}

func compileGlobs(patterns []string) ([]glob.Glob, error) {
	out := make([]glob.Glob, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		g, err := glob.Compile(pattern, '/')
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func matchesAny(patterns []glob.Glob, value string) bool {
	for _, pattern := range patterns {
		if pattern.Match(value) {
			return true
		}
	}
	return false
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func revisionNames(revisions []resolvedIndexRevision) []string {
	out := make([]string, 0, len(revisions))
	for _, rev := range revisions {
		out = append(out, rev.Revision)
	}
	return out
}

func materializeCommitSnapshot(src, commitHash, dst string) (int32, error) {
	if strings.TrimSpace(src) == "" || strings.TrimSpace(dst) == "" {
		return 0, errors.New("source and destination are required")
	}
	if err := verifyCommitExists(src, commitHash); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	_ = runGitWorktree(src, "worktree", "remove", "--force", dst)
	if err := os.RemoveAll(dst); err != nil {
		return 0, err
	}
	_ = runGitWorktree(src, "worktree", "prune")
	if err := runGitWorktree(src, "worktree", "add", "--detach", "--force", dst, commitHash); err != nil {
		_ = os.RemoveAll(dst)
		return 0, fmt.Errorf("create git-backed revision snapshot: %w", err)
	}
	count, err := countSnapshotFiles(dst)
	if err != nil {
		_ = os.RemoveAll(dst)
		return 0, err
	}
	return count, nil
}

func verifyCommitExists(src, commitHash string) error {
	repo, err := git.PlainOpen(src)
	if err != nil {
		return fmt.Errorf("open git repo: %w", err)
	}
	if _, err := repo.CommitObject(plumbing.NewHash(commitHash)); err != nil {
		return fmt.Errorf("resolve commit %s: %w", commitHash, err)
	}
	return nil
}

func runGitWorktree(src string, args ...string) error {
	cmd := exec.Command("git", append([]string{"-C", src}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func countSnapshotFiles(root string) (int32, error) {
	var count int32
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			count++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func isSnapshotFileMode(mode filemode.FileMode) bool {
	return mode == filemode.Regular || mode == filemode.Executable || mode == filemode.Symlink
}

func copyRegularFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	return copyReaderToFile(in, dst, mode)
}

func copyReaderToFile(in io.Reader, dst string, mode os.FileMode) error {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
