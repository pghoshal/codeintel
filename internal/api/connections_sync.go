// POST /api/connections/{id}/sync — manually trigger a sync for
// an existing connection. Useful for operators (re-run a failed
// import) and for clients that defer sync at create-time via
// `"sync": false`.
//
// Flow:
//
//  1. Required auth + OWNER role.
//  2. Parse {id} path segment as int32.
//  3. Verify the connection exists in the caller's org
//     (404 if missing or owned by another org).
//  4. Schedule via the configured ConnectionSyncer.
//  5. Respond with the SyncResult JSON
//     (`{"jobId":"<id>"}` on success, 429 on at-capacity, 502
//     on syncer outage).
package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"codeintel/internal/auth"
)

// syncResponse is the wire shape for a successful sync schedule.
// JobID is omitempty so the no-op syncer (which returns an empty
// SyncResult) produces `{}` rather than `{"jobId":""}`.
type syncResponse struct {
	JobID string `json:"jobId,omitempty"`
}

func (s *Server) handleSyncOrgConnection(w http.ResponseWriter, r *http.Request) {
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
	if authCtx.Role != auth.OrgRoleOwner {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "Only organization owners can sync connections.",
		}, s.connectionsLogger)
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

	// Verify the row exists in the caller's org BEFORE scheduling
	// the sync. Without this check an attacker could probe for
	// connection ids in other orgs by observing the syncer-
	// rejection latency. The narrow `SELECT 1` avoids the cost of
	// reading and decoding the full config JSONB on a code path
	// that only needs the existence boolean.
	exists, err := s.cfg.Queries.ConnectionExistsInOrg(r.Context(), authCtx.Org.ID, connectionID)
	if err != nil {
		s.connectionsLogger.Error("ConnectionExistsInOrg failed (sync)", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if !exists {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusNotFound,
			ErrorCode:  "NOT_FOUND",
			Message:    "Connection not found.",
		}, s.connectionsLogger)
		return
	}

	syncer := s.connectionSyncer()
	res, err := syncer.Schedule(r.Context(), SyncRequest{
		OrgID:        authCtx.Org.ID,
		ConnectionID: connectionID,
	})
	if err != nil {
		s.connectionsLogger.Error("manual sync schedule failed", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusBadGateway, unexpectedErrorBody)
		return
	}
	if res.AlreadyAtCapacity {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusTooManyRequests,
			ErrorCode:  "CONNECTION_SYNC_ALREADY_SCHEDULED",
			Message:    "Connection sync was not scheduled because the tenant is at active sync capacity. Retry after the current sync jobs finish.",
		}, s.connectionsLogger)
		return
	}

	encoded, err := json.Marshal(syncResponse{JobID: res.JobID})
	if err != nil {
		s.connectionsLogger.Error("encode sync response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	// P.2 parity: legacy
	// packages/web/src/app/api/(server)/connections/[id]/sync/route.ts
	// does NOT call audit.createAudit. Removed to match.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
