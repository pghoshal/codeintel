// GET /api/connections
//
// Returns a list of org connections with the config blob piped
// through secretrefs.Redact so sensitive fields (tokens, base
// URLs, etc.) never leak. OWNER-only — the entire endpoint is
// admin-grade.
//
// This handler defers the latest-syncJob nested projection
// (`take:1 orderBy:createdAt-desc`) to the dedicated
// /api/connections/{id}/status endpoint, which carries the full
// per-connection sync-job rollup.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/secretrefs"
)

// connectionListItem is the wire shape for one row of
// GET /api/connections. Field order: id, name, connectionType,
// config (redacted), isDeclarative, syncedAt,
// enforcePermissions, enforcePermissionsForPublicRepos.
//
// syncedAt is *iso8601MilliTime so a NULL column emits JSON null
// rather than being omitted.
type connectionListItem struct {
	ID                                int32              `json:"id"`
	Name                              string             `json:"name"`
	ConnectionType                    string             `json:"connectionType"`
	Config                            any                `json:"config"`
	IsDeclarative                     bool               `json:"isDeclarative"`
	SyncedAt                          *iso8601MilliTime    `json:"syncedAt"`
	EnforcePermissions                bool               `json:"enforcePermissions"`
	EnforcePermissionsForPublicRepos  bool               `json:"enforcePermissionsForPublicRepos"`
}

// handleListOrgConnections lists the org's connections with each
// config blob redacted.
func (s *Server) handleListOrgConnections(w http.ResponseWriter, r *http.Request) {
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
			Message:    "Only organization owners can list connection configuration.",
		}, s.connectionsLogger)
		return
	}

	rows, err := s.cfg.Queries.ListOrgConnectionsForRead(r.Context(), authCtx.Org.ID)
	if err != nil {
		s.connectionsLogger.Error("ListOrgConnectionsForRead failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	out := make([]connectionListItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, connectionListItem{
			ID:                                row.ID,
			Name:                              row.Name,
			ConnectionType:                    row.ConnectionType,
			Config:                            secretrefs.Redact(row.Config),
			IsDeclarative:                     row.IsDeclarative,
			SyncedAt:                          syncedAtFromRow(row.SyncedAt),
			EnforcePermissions:                row.EnforcePermissions,
			EnforcePermissionsForPublicRepos:  row.EnforcePermissionsForPublicRepos,
		})
	}

	encoded, err := json.Marshal(out)
	if err != nil {
		s.connectionsLogger.Error("encode connections response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// syncedAtFromRow wraps a nullable time.Time into a *iso8601MilliTime
// so a NULL column emits JSON null and a populated column emits the
// canonical 3-digit-millis UTC ISO-8601 string.
func syncedAtFromRow(t *time.Time) *iso8601MilliTime {
	if t == nil {
		return nil
	}
	p := iso8601MilliTime(*t)
	return &p
}

