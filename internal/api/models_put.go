// PUT /api/models — replaces the org's full set of language-model
// configs in one transaction.
//
// Steps:
//
//  1. auth middleware — required auth, OWNER only.
//  2. Decode + validate body shape (`{models: [...]}`).
//  3. For each model: structural validation (provider/model
//     non-empty strings).
//  4. Duplicate-name detection by `<provider>:<model>` key.
//  5. Missing-secret-ref check: any secretRef referenced by
//     the new configs MUST exist in OrgSecret for the org.
//  6. ReplaceOrgLanguageModels: DELETE-all + INSERT-all in one tx.
//  7. Re-query enabled models for the response (same shape as GET).
//
// Step 3 currently checks only that `provider` and `model` are
// non-empty. Per-provider required-field validation lands in a
// later change — see progress.md.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"codeintel/internal/auth"
	"codeintel/internal/db"
	"codeintel/internal/secretrefs"
)

// maxPutOrgModelsBodyBytes caps the body size at 4 MiB. Larger
// than secrets (1 MiB) because each model's config can carry many
// per-provider fields and operators routinely configure 5-20
// models in one request.
const maxPutOrgModelsBodyBytes = 4 << 20

// putOrgModelsBody is the documented body schema:
// `{models: array(unknown)}`. The Models field is a pointer so
// the handler can distinguish "field absent" (request-shape
// error → 400) from "field present, empty array" (clear-the-set,
// valid request).
type putOrgModelsBody struct {
	Models *[]any `json:"models"`
}

// handlePutOrgLanguageModels runs the replace-all pipeline.
func (s *Server) handlePutOrgLanguageModels(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.modelsLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if authCtx.Role != auth.OrgRoleOwner {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "Only organization owners can configure language models.",
		}, s.modelsLogger)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPutOrgModelsBodyBytes)
	var body putOrgModelsBody
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
			}, s.modelsLogger)
		case errors.Is(err, io.EOF):
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is empty.",
			}, s.modelsLogger)
		default:
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is not valid JSON.",
			}, s.modelsLogger)
		}
		return
	}

	if body.Models == nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Request body is missing the required `models` field.",
		}, s.modelsLogger)
		return
	}

	parsed, svcErr := s.parseAndBindModels(*body.Models, authCtx.Org.ID)
	if svcErr != nil {
		writeServiceError(w, *svcErr, s.modelsLogger)
		return
	}

	// Aggregate every referenced secret across all models, then ask
	// the DB which ones don't exist. One round-trip vs N per-row
	// lookups.
	allRefs := make([]any, 0, len(parsed))
	for _, p := range parsed {
		allRefs = append(allRefs, p.boundConfig)
	}
	candidateKeys := secretrefs.CollectUnique(allRefs)
	if len(candidateKeys) > 0 {
		missing, err := s.cfg.Queries.SelectMissingOrgSecretKeys(r.Context(), authCtx.Org.ID, candidateKeys)
		if err != nil {
			s.modelsLogger.Error("SelectMissingOrgSecretKeys failed", "err", err, "orgId", authCtx.Org.ID)
			writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
			return
		}
		if len(missing) > 0 {
			// Sort the missing keys for a deterministic error
			// diagnostic — clients can build idempotent retry logic
			// around the message.
			sort.Strings(missing)
			joined := ""
			for i, k := range missing {
				if i > 0 {
					joined += ", "
				}
				joined += k
			}
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Language model config references missing secrets: " + joined,
			}, s.modelsLogger)
			return
		}
	}

	inserts := make([]db.OrgLanguageModelInsert, 0, len(parsed))
	for i, p := range parsed {
		inserts = append(inserts, db.OrgLanguageModelInsert{
			Name:   p.name,
			Config: p.boundConfig,
			Order:  int32(i),
		})
	}
	if err := s.cfg.Queries.ReplaceOrgLanguageModels(r.Context(), authCtx.Org.ID, inserts); err != nil {
		s.modelsLogger.Error("ReplaceOrgLanguageModels failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	// Re-fetch the canonical projection (same shape as GET) so the
	// response always reflects what the DB will return on the next
	// GET — a tiny extra round-trip in exchange for read-after-
	// write consistency.
	rows, err := s.cfg.Queries.ListEnabledOrgLanguageModels(r.Context(), authCtx.Org.ID)
	if err != nil {
		s.modelsLogger.Error("post-replace list failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	infos := make([]languageModelInfo, 0, len(rows))
	for i, row := range rows {
		var cfg languageModelConfig
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			s.modelsLogger.Error("decode model after write", "err", err, "rowIndex", i)
			writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
			return
		}
		infos = append(infos, languageModelInfo{Provider: cfg.Provider, Model: cfg.Model, DisplayName: cfg.DisplayName})
	}
	encoded, _ := json.Marshal(infos)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// parsedModel carries the validated (provider, model) identity, its
// derived <provider>:<model> name, and the secretrefs-bound config
// blob ready for persistence.
type parsedModel struct {
	provider    string
	model       string
	name        string
	boundConfig any
}

// parseAndBindModels validates the incoming models slice and
// rewrites each entry with BindToOrg. Returns a ServiceError on
// the first failure — surface the user's first mistake; don't
// aggregate.
func (s *Server) parseAndBindModels(models []any, orgID int32) ([]parsedModel, *ServiceError) {
	out := make([]parsedModel, 0, len(models))
	seenNames := make(map[string]struct{})
	for i, m := range models {
		// Each model entry must be a JSON object — not a scalar /
		// array / null.
		obj, ok := m.(map[string]any)
		if !ok {
			return nil, &ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    fmt.Sprintf("Language model at index %d is not an object.", i),
			}
		}
		provider, _ := obj["provider"].(string)
		model, _ := obj["model"].(string)
		if provider == "" || model == "" {
			return nil, &ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    fmt.Sprintf("Language model at index %d is missing required `provider` / `model` fields.", i),
			}
		}
		name := provider + ":" + model
		if _, dup := seenNames[name]; dup {
			return nil, &ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    fmt.Sprintf("Duplicate language model config: %s", name),
			}
		}
		seenNames[name] = struct{}{}
		bound := secretrefs.BindToOrg(obj, orgID)
		out = append(out, parsedModel{
			provider:    provider,
			model:       model,
			name:        name,
			boundConfig: bound,
		})
	}
	return out, nil
}
