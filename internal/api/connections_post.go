// POST /api/connections — create or update a connection.
//
// Body schema (Zod-equivalent):
//
//	{
//	  "name": "<non-empty string>",
//	  "config": <object with at least { "type": "<provider>" }>,
//	  "sync": <bool, default true>
//	}
//
// Flow:
//
//  1. Required auth + OWNER role.
//  2. Decode + structural body validation.
//  3. BindToOrg over the config so every nested secretRef carries
//     the org scope.
//  4. SelectMissingOrgSecretKeys: refuse the write if any
//     referenced secret does not exist.
//  5. UpsertOrgConnection (single INSERT...ON CONFLICT).
//  6. Optionally schedule a sync through the configured
//     ConnectionSyncer. Sync scheduling failures surface as a
//     soft 429 (the connection row is already persisted —
//     callers retry the sync step out of band).
//  7. Respond with the same projection GET /api/connections
//     emits for the new row (with the config redacted).
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"

	"codeintel/pkg/audit"
	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/secretrefs"
)

// maxPostConnectionBodyBytes caps the body size at 256 KiB.
// Connection configs are typically a few KB; 256 KiB leaves headroom
// for declarative tenant imports while still bounding memory under
// a hostile client.
const maxPostConnectionBodyBytes = 256 << 10

// postConnectionBody is the request shape. Sync is *bool so the
// handler can distinguish "field absent" (default to true) from
// "explicitly false".
type postConnectionBody struct {
	Name   string `json:"name"`
	Config any    `json:"config"`
	Sync   *bool  `json:"sync,omitempty"`
}

// handleUpsertOrgConnection performs the create-or-update flow.
func (s *Server) handleUpsertOrgConnection(w http.ResponseWriter, r *http.Request) {
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
			Message:    "Only organization owners can configure code host connections.",
		}, s.connectionsLogger)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPostConnectionBodyBytes)
	var body postConnectionBody
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
			}, s.connectionsLogger)
		case errors.Is(err, io.EOF):
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is empty.",
			}, s.connectionsLogger)
		default:
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is not valid JSON.",
			}, s.connectionsLogger)
		}
		return
	}
	if body.Name == "" {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Connection name is required.",
		}, s.connectionsLogger)
		return
	}
	if body.Config == nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Connection config is required.",
		}, s.connectionsLogger)
		return
	}

	// Bind orgId onto every secret reference inside the config so
	// downstream resolvers don't have to re-derive the org scope.
	bound := secretrefs.BindToOrg(body.Config, authCtx.Org.ID)

	cfgMap, ok := bound.(map[string]any)
	if !ok {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Connection config must be a JSON object.",
		}, s.connectionsLogger)
		return
	}
	connectionType, _ := cfgMap["type"].(string)
	if connectionType == "" {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Connection config must specify a `type` string.",
		}, s.connectionsLogger)
		return
	}
	// Permission-enforcement flags ride along on the config blob.
	// The handler intentionally trusts the client value here — the
	// sync backend is the policy authority and is expected to
	// reject configurations that violate deployment policy. A
	// future deployment-level gate (e.g. "force enforcement on") is
	// queued as a separate slice; today's design surface is "the
	// client says what they want, the sync backend approves or
	// denies".
	enforcePermissions, _ := cfgMap["enforcePermissions"].(bool)
	enforcePermissionsForPublicRepos, _ := cfgMap["enforcePermissionsForPublicRepos"].(bool)

	// Verify every secret reference inside the config resolves to
	// a known OrgSecret row BEFORE writing the connection.
	candidateKeys := secretrefs.CollectUnique(bound)
	if len(candidateKeys) > 0 {
		missing, err := s.cfg.Queries.SelectMissingOrgSecretKeys(r.Context(), authCtx.Org.ID, candidateKeys)
		if err != nil {
			s.connectionsLogger.Error("SelectMissingOrgSecretKeys failed", "err", err, "orgId", authCtx.Org.ID)
			writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
			return
		}
		if len(missing) > 0 {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Connection config references missing secrets: " + joinSorted(missing),
			}, s.connectionsLogger)
			return
		}
	}

	shouldSync := body.Sync == nil || *body.Sync
	row, err := s.cfg.Queries.UpsertOrgConnection(r.Context(), db.UpsertOrgConnectionParams{
		OrgID:                            authCtx.Org.ID,
		Name:                             body.Name,
		ConnectionType:                   connectionType,
		Config:                           bound,
		EnforcePermissions:               enforcePermissions,
		EnforcePermissionsForPublicRepos: enforcePermissionsForPublicRepos,
		ResetSync:                        shouldSync,
	})
	if err != nil {
		s.connectionsLogger.Error("UpsertOrgConnection failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	if shouldSync {
		syncer := s.connectionSyncer()
		res, err := syncer.Schedule(r.Context(), SyncRequest{
			OrgID:        authCtx.Org.ID,
			ConnectionID: row.ID,
		})
		if err != nil {
			s.connectionsLogger.Error("schedule sync failed", "err", err, "orgId", authCtx.Org.ID, "connectionId", row.ID)
			// The connection row is already persisted — surface a
			// 502 rather than a 500 so operators can distinguish the
			// sync-backend failure from a data-layer failure.
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
	}

	resp := connectionListItem{
		ID:                               row.ID,
		Name:                             row.Name,
		ConnectionType:                   row.ConnectionType,
		Config:                           secretrefs.Redact(row.Config),
		IsDeclarative:                    row.IsDeclarative,
		SyncedAt:                         syncedAtFromRow(row.SyncedAt),
		EnforcePermissions:               row.EnforcePermissions,
		EnforcePermissionsForPublicRepos: row.EnforcePermissionsForPublicRepos,
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.connectionsLogger.Error("encode post-connection response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	actorID, actorType := auditActor(authCtx)
	s.emitAudit(r.Context(), audit.Event{
		Action:     "connection.upserted",
		ActorID:    actorID,
		ActorType:  actorType,
		TargetID:   targetIDFromInt(row.ID),
		TargetType: audit.TargetConnection,
		OrgID:      authCtx.Org.ID,
		Metadata: map[string]any{
			"name":           row.Name,
			"connectionType": row.ConnectionType,
			"sync":           shouldSync,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// connectionSyncer returns the configured syncer or the no-op
// default. Centralised so every call site sees a non-nil value
// without each branch having to nil-check.
func (s *Server) connectionSyncer() ConnectionSyncer {
	if s.cfg.ConnectionSyncer != nil {
		return s.cfg.ConnectionSyncer
	}
	return NoopConnectionSyncer{}
}

// joinSorted sorts and comma-joins the keys for the missing-secret
// diagnostic body — produces a deterministic error message so
// client idempotent-retry logic can build on it.
//
// strings.Builder gives O(n) string assembly even when the slice
// grows (e.g. a config that references dozens of secrets).
func joinSorted(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	sorted := make([]string, len(keys))
	copy(sorted, keys)
	sort.Strings(sorted)
	var buf strings.Builder
	buf.WriteString(sorted[0])
	for _, k := range sorted[1:] {
		buf.WriteString(", ")
		buf.WriteString(k)
	}
	return buf.String()
}
