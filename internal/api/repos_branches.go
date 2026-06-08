package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/pkg/audit"
	"codeintel/pkg/repoindexstatus"
)

type repoBranchesQuerier interface {
	GetOrgRepoPrimaryConnectionMeta(ctx context.Context, orgID, repoID int32) (db.ConnectionMetaRow, error)
	GetOrgRepoPrimaryConnectionForUpdate(ctx context.Context, orgID, repoID int32) (db.ConnectionListRow, error)
	UpsertOrgConnection(ctx context.Context, p db.UpsertOrgConnectionParams) (db.ConnectionListRow, error)
	UpdateOrgRepoBranchPolicyMetadata(ctx context.Context, orgID, repoID int32, branches []string) error
}

type repoBranchesResponse struct {
	RepoID         int32                               `json:"repoId"`
	RepoName       string                              `json:"repoName"`
	ConnectionID   int32                               `json:"connectionId"`
	ConnectionName string                              `json:"connectionName"`
	ConnectionType string                              `json:"connectionType"`
	SyncedAt       *iso8601MilliTime                   `json:"syncedAt"`
	UpdatedAt      iso8601MilliTime                    `json:"updatedAt"`
	BranchPolicy   branchPolicy                        `json:"branchPolicy"`
	BranchStatuses []repoindexstatus.BranchIndexStatus `json:"branchStatuses"`
	BranchStatus   *repoindexstatus.BranchIndexStatus  `json:"branchStatus,omitempty"`
}

func parseRepoIDFromPath(r *http.Request) (int32, *ServiceError) {
	idStr := r.PathValue("id")
	idParsed, parseErr := strconv.ParseInt(idStr, 10, 32)
	if parseErr != nil {
		return 0, &ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidQueryParams,
			Message:    "Repo id must be an integer.",
		}
	}
	return int32(idParsed), nil
}

func (s *Server) repoBranchesQueries() (repoBranchesQuerier, bool) {
	q, ok := s.cfg.Queries.(repoBranchesQuerier)
	return q, ok
}

func buildRepoBranchesResponse(status RepoStatusResponse, conn db.ConnectionMetaRow) repoBranchesResponse {
	resp := repoBranchesResponse{
		RepoID:         status.ID,
		RepoName:       status.Name,
		ConnectionID:   conn.ID,
		ConnectionName: conn.Name,
		ConnectionType: conn.ConnectionType,
		SyncedAt:       syncedAtFromRow(conn.SyncedAt),
		UpdatedAt:      iso8601MilliTime(conn.UpdatedAt),
		BranchPolicy:   computeBranchPolicy(conn.Config),
		BranchStatuses: status.BranchStatuses,
		BranchStatus:   status.BranchStatus,
	}
	if resp.BranchStatuses == nil {
		resp.BranchStatuses = []repoindexstatus.BranchIndexStatus{}
	}
	return resp
}

func (s *Server) handleGetOrgRepoBranches(w http.ResponseWriter, r *http.Request) {
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
			Message:    "Only organization owners can view repo branch policy.",
		}, s.reposLogger)
		return
	}
	repoID, svcErr := parseRepoIDFromPath(r)
	if svcErr != nil {
		writeServiceError(w, *svcErr, s.reposLogger)
		return
	}
	q, ok := s.repoBranchesQueries()
	if !ok {
		s.reposLogger.Error("repo branches queries are not configured")
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	conn, err := q.GetOrgRepoPrimaryConnectionMeta(r.Context(), authCtx.Org.ID, repoID)
	if err != nil {
		if errors.Is(err, db.ErrConnectionNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Repo not found.",
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("GetOrgRepoPrimaryConnectionMeta failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	status, err := s.repoStatusFetcher().Fetch(r.Context(), authCtx.Org.ID, repoID, r.URL.Query().Get("branch"))
	if err != nil {
		if errors.Is(err, ErrRepoStatusNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Repo not found.",
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("repo branch status fetch failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	resp := buildRepoBranchesResponse(status, conn)
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.reposLogger.Error("encode repo branches response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (s *Server) handlePutOrgRepoBranches(w http.ResponseWriter, r *http.Request) {
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
			Message:    "Only organization owners can update repo branch policy.",
		}, s.reposLogger)
		return
	}
	repoID, svcErr := parseRepoIDFromPath(r)
	if svcErr != nil {
		writeServiceError(w, *svcErr, s.reposLogger)
		return
	}
	q, ok := s.repoBranchesQueries()
	if !ok {
		s.reposLogger.Error("repo branches queries are not configured")
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPostConnectionBodyBytes)
	var body putBranchesBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusRequestEntityTooLarge,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body exceeds the maximum allowed size.",
			}, s.reposLogger)
		case errors.Is(err, io.EOF):
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is empty.",
			}, s.reposLogger)
		default:
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is not valid JSON.",
			}, s.reposLogger)
		}
		return
	}
	switch body.Mode {
	case branchSyncModeDefault, branchSyncModeAll, branchSyncModePatterns:
	default:
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Branch mode must be one of: default, all, patterns.",
		}, s.reposLogger)
		return
	}

	existing, err := q.GetOrgRepoPrimaryConnectionForUpdate(r.Context(), authCtx.Org.ID, repoID)
	if err != nil {
		if errors.Is(err, db.ErrConnectionNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Repo not found.",
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("GetOrgRepoPrimaryConnectionForUpdate failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	cfgMap, ok := existing.Config.(map[string]any)
	if !ok {
		s.reposLogger.Error("existing repo connection config is not a JSON object", "orgId", authCtx.Org.ID, "repoId", repoID, "connectionId", existing.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if _, err := applyBranchPolicy(cfgMap, body.Mode, body.Branches); err != nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    err.Error(),
		}, s.reposLogger)
		return
	}
	shouldSync := body.Sync == nil || *body.Sync
	row, err := q.UpsertOrgConnection(r.Context(), db.UpsertOrgConnectionParams{
		OrgID:                            authCtx.Org.ID,
		Name:                             existing.Name,
		ConnectionType:                   existing.ConnectionType,
		Config:                           cfgMap,
		EnforcePermissions:               existing.EnforcePermissions,
		EnforcePermissionsForPublicRepos: existing.EnforcePermissionsForPublicRepos,
		ResetSync:                        shouldSync,
	})
	if err != nil {
		s.reposLogger.Error("UpsertOrgConnection (repo branches put) failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID, "connectionId", existing.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if err := q.UpdateOrgRepoBranchPolicyMetadata(r.Context(), authCtx.Org.ID, repoID, branchesFromConfig(row.Config)); err != nil {
		if errors.Is(err, db.ErrRepoNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Repo not found.",
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("UpdateOrgRepoBranchPolicyMetadata failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID, "connectionId", row.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if shouldSync {
		res, schedErr := s.connectionSyncer().Schedule(r.Context(), SyncRequest{
			OrgID:        authCtx.Org.ID,
			ConnectionID: row.ID,
		})
		if schedErr != nil {
			s.reposLogger.Error("schedule sync failed (repo branches put)", "err", schedErr, "orgId", authCtx.Org.ID, "repoId", repoID, "connectionId", row.ID)
			writeStaticServiceError(w, http.StatusBadGateway, unexpectedErrorBody)
			return
		}
		if res.AlreadyAtCapacity {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusTooManyRequests,
				ErrorCode:  "CONNECTION_SYNC_ALREADY_SCHEDULED",
				Message:    "Connection sync was not scheduled because the tenant is at active sync capacity. Retry after the current sync jobs finish.",
			}, s.reposLogger)
			return
		}
	}

	status, err := s.repoStatusFetcher().Fetch(r.Context(), authCtx.Org.ID, repoID, "")
	if err != nil {
		if errors.Is(err, ErrRepoStatusNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Repo not found.",
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("repo branch status fetch after put failed", "err", err, "orgId", authCtx.Org.ID, "repoId", repoID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	resp := buildRepoBranchesResponse(status, db.ConnectionMetaRow{
		ID:             row.ID,
		Name:           row.Name,
		ConnectionType: row.ConnectionType,
		Config:         row.Config,
		SyncedAt:       row.SyncedAt,
		UpdatedAt:      row.UpdatedAt,
	})
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.reposLogger.Error("encode repo branches put response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	actorID, actorType := auditActor(authCtx)
	s.emitAudit(r.Context(), audit.Event{
		Action:     "repo.branches_updated",
		ActorID:    actorID,
		ActorType:  actorType,
		TargetID:   targetIDFromInt(repoID),
		TargetType: audit.TargetRepo,
		OrgID:      authCtx.Org.ID,
		Metadata: map[string]any{
			"connectionId": row.ID,
			"mode":         string(body.Mode),
			"sync":         shouldSync,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
