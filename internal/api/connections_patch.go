// PATCH /api/connections/{id} — partial update of a connection.
//
// Body schema (all fields optional, at least one required):
//
//	{
//	  "name":    "<non-empty string>",
//	  "config":  <object with at least { "type": "<provider>" }>,
//	  "sync":    <bool — when true, syncedAt is nulled and a sync
//	             is scheduled through the ConnectionSyncer>
//	}
//
// Flow:
//
//  1. Required auth + OWNER role.
//  2. Parse {id} path segment as int32.
//  3. Decode + validate body — at least one mutable field present.
//  4. Load the existing row (404 if missing or owned by another
//     org).
//  5. If `name` is changing, verify no other row in the org owns
//     it (400 on conflict).
//  6. If `config` is supplied, BindToOrg + missing-secret-ref
//     check.
//  7. Compute the merged row and write through UpsertOrgConnection.
//  8. Optionally schedule a sync.
//  9. Respond with the redacted-config projection.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"codeintel/pkg/audit"
	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/secretrefs"

	"github.com/jackc/pgx/v5/pgconn"
)

// patchConnectionBody mirrors the partial-update schema. All
// fields are pointers so the handler can distinguish "absent"
// from "explicitly null/false".
type patchConnectionBody struct {
	Name   *string `json:"name,omitempty"`
	Config any     `json:"config,omitempty"`
	Sync   *bool   `json:"sync,omitempty"`
}

func (s *Server) handlePatchOrgConnection(w http.ResponseWriter, r *http.Request) {
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

	r.Body = http.MaxBytesReader(w, r.Body, maxPostConnectionBodyBytes)
	var body patchConnectionBody
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

	// At least one mutable field must be present — empty PATCH is
	// a programming error worth surfacing instead of silently
	// returning the unchanged row.
	if body.Name == nil && body.Config == nil && body.Sync == nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "PATCH body must include at least one of `name`, `config`, or `sync`.",
		}, s.connectionsLogger)
		return
	}

	// Read the merge-base row. 404 surfaces here when the id is
	// unknown OR when it belongs to another org.
	existing, err := s.cfg.Queries.GetOrgConnectionForUpdate(r.Context(), authCtx.Org.ID, connectionID)
	if err != nil {
		if errors.Is(err, db.ErrConnectionNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusNotFound,
				ErrorCode:  "NOT_FOUND",
				Message:    "Connection not found.",
			}, s.connectionsLogger)
			return
		}
		s.connectionsLogger.Error("GetOrgConnectionForUpdate failed", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	// Name change: confirm no conflict in the org.
	newName := existing.Name
	if body.Name != nil {
		if *body.Name == "" {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Connection name must not be empty.",
			}, s.connectionsLogger)
			return
		}
		if *body.Name != existing.Name {
			if err := s.cfg.Queries.CheckOrgConnectionNameAvailable(r.Context(), authCtx.Org.ID, *body.Name, connectionID); err != nil {
				if errors.Is(err, db.ErrConnectionNameConflict) {
					writeServiceError(w, ServiceError{
						StatusCode: http.StatusBadRequest,
						ErrorCode:  "CONNECTION_ALREADY_EXISTS",
						Message:    "Connection '" + *body.Name + "' already exists.",
					}, s.connectionsLogger)
					return
				}
				s.connectionsLogger.Error("CheckOrgConnectionNameAvailable failed", "err", err, "orgId", authCtx.Org.ID)
				writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
				return
			}
			newName = *body.Name
		}
	}

	// Config change: re-bind orgIds and re-check missing refs.
	newConnectionType := existing.ConnectionType
	newConfig := existing.Config
	newEnforcePermissions := existing.EnforcePermissions
	newEnforcePermissionsForPublicRepos := existing.EnforcePermissionsForPublicRepos
	if body.Config != nil {
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
		typ, _ := cfgMap["type"].(string)
		if typ == "" {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Connection config must specify a `type` string.",
			}, s.connectionsLogger)
			return
		}

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

		newConfig = bound
		newConnectionType = typ
		// Permission-enforcement flags ride along on the config blob
		// when supplied. Absence preserves the existing value.
		if v, ok := cfgMap["enforcePermissions"].(bool); ok {
			newEnforcePermissions = v
		}
		if v, ok := cfgMap["enforcePermissionsForPublicRepos"].(bool); ok {
			newEnforcePermissionsForPublicRepos = v
		}
	}

	shouldSync := body.Sync != nil && *body.Sync
	row, err := s.cfg.Queries.UpsertOrgConnection(r.Context(), db.UpsertOrgConnectionParams{
		OrgID:                            authCtx.Org.ID,
		Name:                             newName,
		ConnectionType:                   newConnectionType,
		Config:                           newConfig,
		EnforcePermissions:               newEnforcePermissions,
		EnforcePermissionsForPublicRepos: newEnforcePermissionsForPublicRepos,
		ResetSync:                        shouldSync,
	})
	if err != nil {
		// Race window: a concurrent INSERT could have taken the
		// requested name between our availability check and this
		// write. Postgres surfaces the unique-constraint violation
		// as SQLSTATE 23505; remap to a 400 so the client knows it
		// was a soft-conflict, not a hard failure.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  "CONNECTION_ALREADY_EXISTS",
				Message:    "Connection name already in use (concurrent update detected).",
			}, s.connectionsLogger)
			return
		}
		s.connectionsLogger.Error("UpsertOrgConnection (PATCH) failed", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
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
			s.connectionsLogger.Error("schedule sync failed (PATCH)", "err", err, "orgId", authCtx.Org.ID, "connectionId", row.ID)
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
		s.connectionsLogger.Error("encode patch response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	actorID, actorType := auditActor(authCtx)
	patched := map[string]any{}
	if body.Name != nil {
		patched["name"] = true
	}
	if body.Config != nil {
		patched["config"] = true
	}
	if body.Sync != nil {
		patched["sync"] = *body.Sync
	}
	s.emitAudit(r.Context(), audit.Event{
		Action:     "connection.patched",
		ActorID:    actorID,
		ActorType:  actorType,
		TargetID:   targetIDFromInt(row.ID),
		TargetType: audit.TargetConnection,
		OrgID:      authCtx.Org.ID,
		Metadata: map[string]any{
			"fields": patched,
			"newName": row.Name,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
