// GET /api/connections/{id}/status — sync-job rollup for a single
// connection. Used by dashboards to surface whether the most
// recent sync attempts succeeded, are in progress, or failed with
// diagnostics.
//
// Response shape:
//
//	{
//	  "id":              <int>,
//	  "name":            "<string>",
//	  "connectionType":  "<string>",
//	  "syncedAt":        "<iso>" | null,
//	  "updatedAt":       "<iso>",
//	  "syncJobs": [
//	    {
//	      "id":              "<cuid>",
//	      "status":          "PENDING|IN_PROGRESS|COMPLETED|FAILED",
//	      "createdAt":       "<iso>",
//	      "updatedAt":       "<iso>",
//	      "completedAt":     "<iso>" | null,
//	      "warningMessages": [<string>...],
//	      "errorMessage":    "<string>" | null
//	    },
//	    ...
//	  ],
//	  "repoCount":     <int>,
//	  "branchPolicy":  { "mode": "...", "branches": [...], "defaultBranchAlwaysIncluded": <bool>, "maxIndexedRevisions": 64 },
//	  "latestJob":     { ...same as syncJobs[i] } | null
//	}
//
// Up to 20 most-recent jobs are returned, newest first. `latestJob`
// is `syncJobs[0]` (or null when there are no jobs).
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// syncJobLimit caps how many job rows the status response carries.
const syncJobLimit = 20

// syncJobItem field order matches the documented wire shape for a
// sync-job row: id, status, createdAt, updatedAt, completedAt,
// warningMessages, errorMessage.
type syncJobItem struct {
	ID              string          `json:"id"`
	Status          string          `json:"status"`
	CreatedAt       iso8601MilliTime  `json:"createdAt"`
	UpdatedAt       iso8601MilliTime  `json:"updatedAt"`
	CompletedAt     *iso8601MilliTime `json:"completedAt"`
	WarningMessages []string        `json:"warningMessages"`
	ErrorMessage    *string         `json:"errorMessage"`
}

// connectionStatusResponse pins the JSON wire shape. Field order
// is the connection projection (id, name, connectionType, syncedAt,
// updatedAt, syncJobs) followed by the three computed fields
// (repoCount, branchPolicy, latestJob).
type connectionStatusResponse struct {
	ID             int32           `json:"id"`
	Name           string          `json:"name"`
	ConnectionType string          `json:"connectionType"`
	SyncedAt       *iso8601MilliTime `json:"syncedAt"`
	UpdatedAt      iso8601MilliTime  `json:"updatedAt"`
	SyncJobs       []syncJobItem   `json:"syncJobs"`
	RepoCount      int32           `json:"repoCount"`
	BranchPolicy   branchPolicy    `json:"branchPolicy"`
	LatestJob      *syncJobItem    `json:"latestJob"`
}

func (s *Server) handleGetOrgConnectionStatus(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.connectionsLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	idStr := r.PathValue("id")
	idParsed, parseErr := strconv.ParseInt(idStr, 10, 32)
	if parseErr != nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidQueryParams,
			Message:    "Connection id must be an integer.",
		}, s.connectionsLogger)
		return
	}
	connectionID := int32(idParsed)

	meta, err := s.cfg.Queries.GetOrgConnectionMeta(r.Context(), authCtx.Org.ID, connectionID)
	if err != nil {
		if errors.Is(err, db.ErrConnectionNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Connection not found.",
			}, s.connectionsLogger)
			return
		}
		s.connectionsLogger.Error("GetOrgConnectionMeta failed (status)", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	repoCount, err := s.cfg.Queries.CountConnectionRepos(r.Context(), authCtx.Org.ID, connectionID)
	if err != nil {
		s.connectionsLogger.Error("CountConnectionRepos failed", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	rawJobs, err := s.cfg.Queries.ListConnectionSyncJobs(r.Context(), authCtx.Org.ID, connectionID, syncJobLimit)
	if err != nil {
		s.connectionsLogger.Error("ListConnectionSyncJobs failed", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	jobs := make([]syncJobItem, 0, len(rawJobs))
	for _, j := range rawJobs {
		item := syncJobItem{
			ID:              j.ID,
			Status:          j.Status,
			CreatedAt:       iso8601MilliTime(j.CreatedAt),
			UpdatedAt:       iso8601MilliTime(j.UpdatedAt),
			WarningMessages: j.WarningMessages,
			ErrorMessage:    j.ErrorMessage,
		}
		if j.CompletedAt != nil {
			cp := iso8601MilliTime(*j.CompletedAt)
			item.CompletedAt = &cp
		}
		jobs = append(jobs, item)
	}

	var latest *syncJobItem
	if len(jobs) > 0 {
		j := jobs[0]
		latest = &j
	}

	resp := connectionStatusResponse{
		ID:             meta.ID,
		Name:           meta.Name,
		ConnectionType: meta.ConnectionType,
		SyncedAt:       syncedAtFromRow(meta.SyncedAt),
		UpdatedAt:      iso8601MilliTime(meta.UpdatedAt),
		SyncJobs:       jobs,
		RepoCount:      repoCount,
		BranchPolicy:   computeBranchPolicy(meta.Config),
		LatestJob:      latest,
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.connectionsLogger.Error("encode status response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
