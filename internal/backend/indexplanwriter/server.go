// Package indexplanwriter hosts backend's IndexPlanService gRPC
// handler. Rust indexer workers send discovered branch/project facts
// here; backend validates scope and persists durable subjobs.
package indexplanwriter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"

	"codeintel/internal/backend/indexplanner"
	"codeintel/internal/backend/indexsubjobs"
	codeintelv1 "codeintel/proto/codeintel/v1"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxRevisions    = 128
	maxSCIPProjects = 2048
	defaultAttempts = 3
)

type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type planStore interface {
	UpsertQueued(context.Context, indexsubjobs.CreateInput) error
}

type Server struct {
	codeintelv1.UnimplementedIndexPlanServiceServer
	db     pgxQuerier
	store  planStore
	logger *slog.Logger
}

func NewServer(db pgxQuerier, store planStore, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		db:     db,
		store:  store,
		logger: logger.With("component", "index-plan-write"),
	}
}

func (s *Server) WritePlan(ctx context.Context, req *codeintelv1.WriteIndexPlanRequest) (*codeintelv1.WriteIndexPlanResponse, error) {
	if s == nil || s.db == nil || s.store == nil {
		return nil, status.Error(codes.FailedPrecondition, "index plan writer is not configured")
	}
	if err := validateRequest(req); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	scope, err := s.resolveScope(ctx, req)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.PermissionDenied, "index job scope does not belong to org/repo or is not an active INDEX job")
		}
		return nil, status.Errorf(codes.Internal, "scope validation: %v", err)
	}
	for _, rev := range req.GetRevisions() {
		if rev.GetWorkspaceId() != scope.WorkspaceID {
			return nil, status.Errorf(codes.InvalidArgument, "workspaceId %q does not match org-resolved workspaceId %q", rev.GetWorkspaceId(), scope.WorkspaceID)
		}
	}
	if err := s.validateRevisionManifests(ctx, req); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.PermissionDenied, "index plan revision is not part of the active prepared manifests")
		}
		return nil, status.Errorf(codes.Internal, "revision manifest validation: %v", err)
	}

	planned, err := indexplanner.PlanAndPersist(ctx, s.store, toPlannerInput(req))
	if err != nil {
		if errors.Is(err, indexplanner.ErrInvalidPlan) {
			return nil, status.Errorf(codes.InvalidArgument, "planner validation: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "persist index plan: %v", err)
	}
	s.logger.Info("index plan persisted",
		"jobId", req.GetIndexJobId(),
		"orgId", req.GetOrgId(),
		"repoId", req.GetRepoId(),
		"workspaceId", scope.WorkspaceID,
		"revisionCount", len(req.GetRevisions()),
		"subjobCount", len(planned),
	)
	return &codeintelv1.WriteIndexPlanResponse{
		RevisionCount: int32(len(req.GetRevisions())),
		SubjobCount:   int32(len(planned)),
	}, nil
}

func validateRequest(req *codeintelv1.WriteIndexPlanRequest) error {
	if req == nil {
		return errors.New("request is required")
	}
	if strings.TrimSpace(req.GetIndexJobId()) == "" {
		return errors.New("indexJobId is required")
	}
	if req.GetOrgId() <= 0 {
		return errors.New("orgId must be positive")
	}
	if req.GetRepoId() <= 0 {
		return errors.New("repoId must be positive")
	}
	if req.GetMaxAttempts() < 0 || req.GetMaxAttempts() > 25 {
		return errors.New("maxAttempts must be between 0 and 25")
	}
	if len(req.GetRevisions()) == 0 || len(req.GetRevisions()) > maxRevisions {
		return fmt.Errorf("revision count must be between 1 and %d", maxRevisions)
	}
	projectCount := 0
	for i, rev := range req.GetRevisions() {
		if err := validateRevision(rev); err != nil {
			return fmt.Errorf("revisions[%d]: %w", i, err)
		}
		projectCount += len(rev.GetScipProjects())
		if projectCount > maxSCIPProjects {
			return fmt.Errorf("scip project count exceeds %d", maxSCIPProjects)
		}
	}
	return nil
}

func validateRevision(rev *codeintelv1.IndexPlanRevision) error {
	if rev == nil {
		return errors.New("revision is required")
	}
	if strings.TrimSpace(rev.GetWorkspaceId()) == "" {
		return errors.New("workspaceId is required")
	}
	if strings.TrimSpace(rev.GetBranch()) == "" {
		return errors.New("branch is required")
	}
	if strings.TrimSpace(rev.GetRevision()) == "" {
		return errors.New("revision is required")
	}
	if rev.GetRevision() != rev.GetBranch() {
		return errors.New("revision must match branch until RepoIndexManifest stores a separate revision")
	}
	if !isHexCommit(rev.GetCommitHash()) {
		return errors.New("commitHash must be a 40-character SHA")
	}
	hasProducer := rev.GetRunAstTreeSitter() || len(rev.GetScipProjects()) > 0
	if !hasProducer && !rev.GetRunGraphMerge() && !rev.GetRunActivate() {
		return errors.New("at least one producer layer or SCIP project is required")
	}
	if rev.GetRunGraphMerge() && !hasProducer {
		return errors.New("runGraphMerge requires AST/tree-sitter or SCIP producer work")
	}
	if rev.GetRunActivate() && !hasProducer && !rev.GetRunGraphMerge() {
		return errors.New("runActivate requires at least one producer layer")
	}
	for i, project := range rev.GetScipProjects() {
		if err := validateSCIPProject(project); err != nil {
			return fmt.Errorf("scipProjects[%d]: %w", i, err)
		}
	}
	return nil
}

func validateSCIPProject(project *codeintelv1.SCIPProjectPlan) error {
	if project == nil {
		return errors.New("project is required")
	}
	if strings.TrimSpace(project.GetLanguage()) == "" {
		return errors.New("language is required")
	}
	if strings.TrimSpace(project.GetIndexer()) == "" {
		return errors.New("indexer is required")
	}
	if strings.TrimSpace(project.GetScipWorkerClass()) == "" {
		return errors.New("scipWorkerClass is required")
	}
	if !safeProjectRoot(project.GetProjectRoot()) {
		return fmt.Errorf("projectRoot %q is not repo-relative", project.GetProjectRoot())
	}
	for name, value := range map[string]*string{
		"toolchainDigest":  project.ToolchainDigest,
		"imageDigest":      project.ImageDigest,
		"projectInputHash": project.ProjectInputHash,
	} {
		if value != nil && strings.TrimSpace(*value) == "" {
			return fmt.Errorf("%s must be non-empty when present", name)
		}
	}
	return nil
}

type resolvedScope struct {
	WorkspaceID string
}

func (s *Server) resolveScope(ctx context.Context, req *codeintelv1.WriteIndexPlanRequest) (resolvedScope, error) {
	var workspaceID string
	err := s.db.QueryRow(ctx, `
		SELECT COALESCE(o."atomWorkspaceId", o.domain, 'org-' || o.id::text) AS "workspaceId"
		FROM "RepoIndexingJob" j
		JOIN "Repo" r ON r.id = j."repoId"
		JOIN "Org" o ON o.id = r."orgId"
		WHERE j.id = $1
		  AND j."repoId" = $2
		  AND r.id = $2
		  AND r."orgId" = $3
		  AND j.type = 'INDEX'::"RepoIndexingJobType"
		  AND j.status = 'IN_PROGRESS'::"RepoIndexingJobStatus"
	`, req.GetIndexJobId(), req.GetRepoId(), req.GetOrgId()).Scan(&workspaceID)
	if err != nil {
		return resolvedScope{}, err
	}
	return resolvedScope{WorkspaceID: workspaceID}, nil
}

func (s *Server) validateRevisionManifests(ctx context.Context, req *codeintelv1.WriteIndexPlanRequest) error {
	for _, rev := range req.GetRevisions() {
		var exists int
		if err := s.db.QueryRow(ctx, `
			SELECT 1
			FROM "RepoIndexManifest"
			WHERE "indexJobId" = $1
			  AND "orgId" = $2
			  AND "repoId" = $3
			  AND "workspaceId" = $4
			  AND branch = $5
			  AND "commitHash" = $6
			  AND status = 'PENDING'::"RepoIndexManifestStatus"
		`, req.GetIndexJobId(), req.GetOrgId(), req.GetRepoId(), rev.GetWorkspaceId(), rev.GetBranch(), rev.GetCommitHash()).Scan(&exists); err != nil {
			return err
		}
	}
	return nil
}

func toPlannerInput(req *codeintelv1.WriteIndexPlanRequest) indexplanner.Input {
	maxAttempts := req.GetMaxAttempts()
	if maxAttempts == 0 {
		maxAttempts = defaultAttempts
	}
	out := indexplanner.Input{
		RepoIndexingJobID: req.GetIndexJobId(),
		OrgID:             req.GetOrgId(),
		RepoID:            req.GetRepoId(),
		MaxAttempts:       maxAttempts,
		Revisions:         make([]indexplanner.Revision, 0, len(req.GetRevisions())),
	}
	for _, rev := range req.GetRevisions() {
		workspaceID := rev.GetWorkspaceId()
		next := indexplanner.Revision{
			WorkspaceID:      &workspaceID,
			Branch:           rev.GetBranch(),
			Revision:         rev.GetRevision(),
			CommitHash:       rev.GetCommitHash(),
			RunASTTreeSitter: rev.GetRunAstTreeSitter(),
			RunGraphMerge:    rev.GetRunGraphMerge(),
			RunActivate:      rev.GetRunActivate(),
			SCIPProjects:     make([]indexplanner.SCIPProject, 0, len(rev.GetScipProjects())),
		}
		for _, project := range rev.GetScipProjects() {
			next.SCIPProjects = append(next.SCIPProjects, indexplanner.SCIPProject{
				Language:         project.GetLanguage(),
				ProjectRoot:      project.GetProjectRoot(),
				Indexer:          project.GetIndexer(),
				SCIPWorkerClass:  project.GetScipWorkerClass(),
				ToolchainDigest:  project.ToolchainDigest,
				ImageDigest:      project.ImageDigest,
				ProjectInputHash: project.ProjectInputHash,
			})
		}
		out.Revisions = append(out.Revisions, next)
	}
	return out
}

func safeProjectRoot(root string) bool {
	_, ok := indexplanner.NormalizeProjectRoot(root)
	return ok
}

func isHexCommit(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !unicode.Is(unicode.ASCII_Hex_Digit, r) {
			return false
		}
	}
	return true
}
