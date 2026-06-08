// DELETE /api/secrets/{key}
//
// Steps:
//
//  1. auth middleware — resolve API-key auth.
//  2. OWNER role guard.
//  3. URL-decode + validate the path-segment key (regex:
//     min1/max128/[A-Za-z0-9_.:-]).
//  4. Parallel refcheck: ListOrgConnectionsForRefcheck +
//     ListOrgLanguageModelsForRefcheck via errgroup. If any
//     config references the target key, return 400 with the
//     "connection:<name>, model:<name>" diagnostic.
//  5. DeleteOrgSecret (idempotent — zero rows affected is
//     success).
//  6. Respond with `{"key":"<key>","deleted":true}` byte-equal.
package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/secretrefs"
	"golang.org/x/sync/errgroup"
)

// deleteOrgSecretResponse is the wire shape: `{ key, deleted:
// true }`. JSON field ordering pinned by Go declaration order.
type deleteOrgSecretResponse struct {
	Key     string `json:"key"`
	Deleted bool   `json:"deleted"`
}

// handleDeleteOrgSecret runs the full DELETE pipeline. Path-segment
// `{key}` is read via r.PathValue (Go 1.22+).
func (s *Server) handleDeleteOrgSecret(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.secretsLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	if authCtx.Role != auth.OrgRoleOwner {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "Only organization owners can delete secrets.",
		}, s.secretsLogger)
		return
	}

	rawKey := r.PathValue("key")
	// URL-decode the raw path segment. Go's ServeMux returns the
	// segment without further decoding so we must do it explicitly.
	decodedKey, err := url.PathUnescape(rawKey)
	if err != nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidQueryParams,
			Message:    "Secret key is invalid.",
		}, s.secretsLogger)
		return
	}
	if !isValidSecretKey(decodedKey) {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidQueryParams,
			Message:    "Secret key is invalid.",
		}, s.secretsLogger)
		return
	}

	// Parallel refcheck via errgroup.WithContext — cancels the
	// slower call if the faster one errors. Small p99 win on cold
	// connections.
	g, gctx := errgroup.WithContext(r.Context())
	var (
		conns  []db.ConfigOwner
		models []db.ConfigOwner
	)
	g.Go(func() error {
		rows, err := s.cfg.Queries.ListOrgConnectionsForRefcheck(gctx, authCtx.Org.ID)
		if err != nil {
			return err
		}
		conns = rows
		return nil
	})
	g.Go(func() error {
		rows, err := s.cfg.Queries.ListOrgLanguageModelsForRefcheck(gctx, authCtx.Org.ID)
		if err != nil {
			return err
		}
		models = rows
		return nil
	})
	if err := g.Wait(); err != nil {
		s.secretsLogger.Error("refcheck queries failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	uses := make([]string, 0)
	for _, c := range conns {
		if secretrefs.Contains(c.Config, decodedKey) {
			uses = append(uses, "connection:"+c.Name)
		}
	}
	for _, m := range models {
		if secretrefs.Contains(m.Config, decodedKey) {
			uses = append(uses, "model:"+m.Name)
		}
	}
	if len(uses) > 0 {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Secret '" + decodedKey + "' is still referenced by " + strings.Join(uses, ", ") + ".",
		}, s.secretsLogger)
		return
	}

	if err := s.cfg.Queries.DeleteOrgSecret(r.Context(), authCtx.Org.ID, decodedKey); err != nil {
		s.secretsLogger.Error("DeleteOrgSecret failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	resp := deleteOrgSecretResponse{Key: decodedKey, Deleted: true}
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.secretsLogger.Error("encode delete response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// isValidSecretKey is the URL-path-segment counterpart to the PUT
// body's secret-key validator. The constraint is
// min(1).max(128).regex(/^[A-Za-z0-9_.:-]+$/). Inlined into a
// small helper because both the body schema and the path-segment
// schema share the same regex constant.
func isValidSecretKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	return secretKeyRegex.MatchString(key)
}

