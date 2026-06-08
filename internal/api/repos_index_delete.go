// DELETE /api/repos/{id}/index — request a REMOVE_INDEX job for
// a repo in the caller's org. Tears down every Zoekt shard,
// SCIP / CodeGraph row, and the local clone directory associated
// with the repo. Direct port of the legacy DELETE /api/repos/[id]/index
// route (packages/web/src/app/api/(server)/repos/[id]/index/route.ts).
//
// Flow:
//
//  1. Required auth + OWNER role.
//  2. Parse {id} as int32.
//  3. Verify the repo exists in the caller's org (404 otherwise).
//  4. Schedule via the configured RepoIndexer
//     (NoopRepoIndexer in dev; AsynqRepoIndexer in prod).
//  5. Respond with {"jobId":"<uuid>"} on 200, 409 on capacity,
//     404 on missing repo, 502 on indexer outage.
//
// Authorization: legacy required OrgRole.OWNER for both POST and
// DELETE on this route; the port preserves that.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"codeintel/internal/auth"
)

// repoIndexResponse is the wire shape returned on a successful
// schedule. omitempty so the no-op indexer (which returns an
// empty result) produces {} not {"jobId":""}.
type repoIndexResponse struct {
	JobID string `json:"jobId,omitempty"`
}

func (s *Server) handleDeleteOrgRepoIndex(w http.ResponseWriter, r *http.Request) {
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
			Message:    "Only organization owners can remove repository indexes.",
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
		Kind:   RepoIndexJobKindRemoveIndex,
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
		// P.8 parity: see repos_index_post.go for the legacy
		// 409 contract — `removeRepoIndex` in
		// packages/backend/src/api.ts:224 enforces the same
		// "no active job" guard.
		var activeErr *JobAlreadyActiveError
		if errors.As(err, &activeErr) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusConflict,
				ErrorCode:  "REPO_INDEXING_JOB_ALREADY_ACTIVE",
				Message:    activeErr.Error(),
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("remove-index schedule failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID, "ref", ref)
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
		s.reposLogger.Error("remove-index schedule returned empty job id", "orgId", authCtx.Org.ID, "repoId", repoID, "ref", ref)
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusServiceUnavailable,
			ErrorCode:  "REPO_INDEXER_EMPTY_JOB_ID",
			Message:    "Repository indexing backend did not return a durable job id.",
		}, s.reposLogger)
		return
	}

	encoded, err := json.Marshal(repoIndexResponse{JobID: res.JobID})
	if err != nil {
		s.reposLogger.Error("encode remove-index response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	// P.2 parity: legacy
	// packages/web/src/app/api/(server)/repos/[id]/index/route.ts
	// does NOT emit an audit event on DELETE. Emitting one here
	// would write rows operators have no equivalent for in their
	// legacy audit logs.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// repoIndexer returns the configured indexer or the no-op default.
// Same pattern as connectionSyncer() — centralised so every route
// sees a non-nil RepoIndexer.
func (s *Server) repoIndexer() RepoIndexer {
	if s.cfg.RepoIndexer != nil {
		return s.cfg.RepoIndexer
	}
	return NoopRepoIndexer{}
}
