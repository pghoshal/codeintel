// DELETE /api/connections/{id}
//
// Steps:
//   1. auth middleware — required auth, OWNER only.
//   2. Parse {id} path segment as an integer; reject non-integer.
//   3. Org-scoped delete: DELETE WHERE id = $1 AND orgId = $2 in
//      one scoped statement with a NOT-FOUND check on the
//      affected-rows count.
//   4. Respond `{"success":true}` byte-equal.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"codeintel/pkg/audit"
	"codeintel/internal/auth"
	"codeintel/internal/db"
)

// deleteConnectionResponse is the wire shape:
// `{"success": true}`. Field declaration order pins the JSON output.
type deleteConnectionResponse struct {
	Success bool `json:"success"`
}

func (s *Server) handleDeleteOrgConnection(w http.ResponseWriter, r *http.Request) {
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
			Message:    "Only organization owners can delete code host connections.",
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

	if err := s.cfg.Queries.DeleteOrgConnection(r.Context(), authCtx.Org.ID, int32(idParsed)); err != nil {
		if errors.Is(err, db.ErrConnectionNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Connection not found.",
			}, s.connectionsLogger)
			return
		}
		s.connectionsLogger.Error("DeleteOrgConnection failed", "err", err, "orgId", authCtx.Org.ID, "connectionId", idParsed)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	encoded, _ := json.Marshal(deleteConnectionResponse{Success: true})
	actorID, actorType := auditActor(authCtx)
	s.emitAudit(r.Context(), audit.Event{
		Action:     "connection.deleted",
		ActorID:    actorID,
		ActorType:  actorType,
		TargetID:   targetIDFromInt(int32(idParsed)),
		TargetType: audit.TargetConnection,
		OrgID:      authCtx.Org.ID,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
