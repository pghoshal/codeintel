// PUT /api/connections/{id}/branches — update the branch sync
// policy. Mutates config.revisions.branches according to the
// requested mode then writes the row via UpsertOrgConnection so
// the existing race-loss handling + audit trail are reused.
//
// Body schema:
//
//	{
//	  "mode":     "default" | "all" | "patterns",
//	  "branches": [<string>...]  // required when mode == patterns
//	  "sync":     <bool, default true>
//	}
//
// Flow:
//   1. Required auth + OWNER role.
//   2. Parse {id}; validate body schema.
//   3. Load merge-base row.
//   4. Apply the requested mode to config.revisions.branches.
//   5. Upsert (single-statement INSERT...ON CONFLICT).
//   6. Optionally schedule a sync.
//   7. Emit a connection.branches_updated audit event.
//   8. Respond with the GET /branches response shape.
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
)

// putBranchesBody mirrors the request schema. Sync is a pointer
// so absent / explicit-false / explicit-true are all
// distinguishable.
type putBranchesBody struct {
	Mode     branchSyncMode `json:"mode"`
	Branches []string       `json:"branches,omitempty"`
	Sync     *bool          `json:"sync,omitempty"`
}

// applyBranchPolicy mutates `config` so its revisions.branches
// field reflects the requested mode. Returns a non-nil error
// when the requested mode is "patterns" but no branches were
// supplied — at least one entry is required for that mode.
//
// The input map is mutated in place AND returned for chaining;
// the handler owns the merge-base map (deep-copied from the
// freshly-decoded JSON) so in-place mutation is safe.
func applyBranchPolicy(config map[string]any, mode branchSyncMode, branches []string) (map[string]any, error) {
	revisions, ok := config["revisions"].(map[string]any)
	if !ok {
		revisions = map[string]any{}
	}
	switch mode {
	case branchSyncModeDefault:
		delete(revisions, "branches")
	case branchSyncModeAll:
		revisions["branches"] = []any{allBranchesGlob}
	case branchSyncModePatterns:
		normalised := normaliseBranches(branches)
		if len(normalised) == 0 {
			return nil, errors.New("branch patterns are required when mode is 'patterns'")
		}
		out := make([]any, 0, len(normalised))
		for _, b := range normalised {
			out = append(out, b)
		}
		revisions["branches"] = out
	default:
		return nil, errors.New("unknown branch mode")
	}
	if len(revisions) == 0 {
		delete(config, "revisions")
	} else {
		config["revisions"] = revisions
	}
	return config, nil
}

func (s *Server) handlePutOrgConnectionBranches(w http.ResponseWriter, r *http.Request) {
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
			Message:    "Only organization owners can update branch sync policy.",
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
	// Validate mode: an empty string is the JSON zero-value, which
	// signals the client did not supply the required field.
	switch body.Mode {
	case branchSyncModeDefault, branchSyncModeAll, branchSyncModePatterns:
	default:
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Branch mode must be one of: default, all, patterns.",
		}, s.connectionsLogger)
		return
	}

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
		s.connectionsLogger.Error("GetOrgConnectionForUpdate failed (branches put)", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	cfgMap, ok := existing.Config.(map[string]any)
	if !ok {
		// Existing config has a shape we cannot safely mutate; fail
		// loud so an operator can repair the row out of band rather
		// than silently overwriting an opaque value.
		s.connectionsLogger.Error("existing connection config is not a JSON object", "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if _, err := applyBranchPolicy(cfgMap, body.Mode, body.Branches); err != nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    err.Error(),
		}, s.connectionsLogger)
		return
	}

	shouldSync := body.Sync == nil || *body.Sync
	row, err := s.cfg.Queries.UpsertOrgConnection(r.Context(), db.UpsertOrgConnectionParams{
		OrgID:                            authCtx.Org.ID,
		Name:                             existing.Name,
		ConnectionType:                   existing.ConnectionType,
		Config:                           cfgMap,
		EnforcePermissions:               existing.EnforcePermissions,
		EnforcePermissionsForPublicRepos: existing.EnforcePermissionsForPublicRepos,
		ResetSync:                        shouldSync,
	})
	if err != nil {
		s.connectionsLogger.Error("UpsertOrgConnection (branches put) failed", "err", err, "orgId", authCtx.Org.ID, "connectionId", connectionID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	if shouldSync {
		syncer := s.connectionSyncer()
		res, schedErr := syncer.Schedule(r.Context(), SyncRequest{
			OrgID:        authCtx.Org.ID,
			ConnectionID: row.ID,
		})
		if schedErr != nil {
			s.connectionsLogger.Error("schedule sync failed (branches put)", "err", schedErr, "orgId", authCtx.Org.ID, "connectionId", row.ID)
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

	// UpsertOrgConnection now returns the canonical UpdatedAt via
	// its RETURNING clause so the response timestamp comes from
	// the database clock without a follow-up SELECT.
	resp := branchesResponse{
		ConnectionID:   row.ID,
		ConnectionName: row.Name,
		ConnectionType: row.ConnectionType,
		SyncedAt:       syncedAtFromRow(row.SyncedAt),
		UpdatedAt:      iso8601MilliTime(row.UpdatedAt),
		BranchPolicy:   computeBranchPolicy(row.Config),
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.connectionsLogger.Error("encode branches put response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	actorID, actorType := auditActor(authCtx)
	s.emitAudit(r.Context(), audit.Event{
		Action:     "connection.branches_updated",
		ActorID:    actorID,
		ActorType:  actorType,
		TargetID:   targetIDFromInt(row.ID),
		TargetType: audit.TargetConnection,
		OrgID:      authCtx.Org.ID,
		Metadata: map[string]any{
			"mode": string(body.Mode),
			"sync": shouldSync,
		},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}
