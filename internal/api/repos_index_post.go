// POST /api/repos/{id}/index — request an INDEX job for a repo
// in the caller's org. Triggers a real git clone of the repo
// into the indexer's working tree + records the observed HEAD
// SHA on the Repo row (Phase C.4a behaviour). Mirrors the legacy
// POST /api/repos/[id]/index route
// (packages/web/src/app/api/(server)/repos/[id]/index/route.ts).
//
// Honest scope at this slice: the resulting "indexed" repo is
// cloned to disk but is NOT yet searchable (Zoekt shard writes
// land in C.4b) nor queryable via code intel (SCIP extraction
// lands in C.4c). The job still returns COMPLETED so operators
// can confirm the clone succeeded and the commit hash is what
// they expect.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"codeintel/internal/auth"
)

func (s *Server) handlePostOrgRepoIndex(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.reposLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if authCtx.Role != auth.OrgRoleOwner {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "Only organization owners can index repositories.",
		}, s.reposLogger)
		return
	}

	idStr := r.PathValue("id")
	idParsed, parseErr := strconv.ParseInt(idStr, 10, 32)
	if parseErr != nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidQueryParams,
			Message:    "Repo id must be an integer.",
		}, s.reposLogger)
		return
	}
	repoID := int32(idParsed)
	ref := parseRepoIndexRef(r)

	indexer := s.repoIndexer()
	res, err := indexer.Schedule(r.Context(), RepoIndexRequest{
		OrgID:  authCtx.Org.ID,
		RepoID: repoID,
		Kind:   RepoIndexJobKindIndex,
		Ref:    ref,
	})
	if err != nil {
		if errors.Is(err, ErrRepoIndexerUnavailable) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusServiceUnavailable,
				ErrorCode:  "REPO_INDEXER_NOT_CONFIGURED",
				Message:    "Repository indexing backend is not configured.",
			}, s.reposLogger)
			return
		}
		if errors.Is(err, ErrRepoNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Repo not found.",
			}, s.reposLogger)
			return
		}
		// P.8 parity: a repo with a PENDING / IN_PROGRESS
		// RepoIndexingJob returns 409 with the legacy
		// REPO_INDEXING_JOB_ALREADY_ACTIVE code + the active
		// job's id / type / status surfaced to the caller.
		var activeErr *JobAlreadyActiveError
		if errors.As(err, &activeErr) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusConflict,
				ErrorCode:  "REPO_INDEXING_JOB_ALREADY_ACTIVE",
				Message:    activeErr.Error(),
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("index schedule failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID, "ref", ref)
		writeStaticServiceError(w, http.StatusBadGateway, unexpectedErrorBody)
		return
	}
	if res.AlreadyAtCapacity {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusTooManyRequests,
			ErrorCode:  "REPO_INDEXING_CAPACITY_REACHED",
			Message:    "Repository indexing capacity has been reached for this organization. Retry after the current jobs finish.",
		}, s.reposLogger)
		return
	}
	if strings.TrimSpace(res.JobID) == "" {
		s.reposLogger.Error("index schedule returned empty job id", "orgId", authCtx.Org.ID, "repoId", repoID, "ref", ref)
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusServiceUnavailable,
			ErrorCode:  "REPO_INDEXER_EMPTY_JOB_ID",
			Message:    "Repository indexing backend did not return a durable job id.",
		}, s.reposLogger)
		return
	}

	encoded, err := json.Marshal(repoIndexResponse{JobID: res.JobID})
	if err != nil {
		s.reposLogger.Error("encode index response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	// P.2 parity: legacy
	// packages/web/src/app/api/(server)/repos/[id]/index/route.ts
	// does NOT call audit.createAudit on POST. Removed to match.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
