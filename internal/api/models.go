// GET /api/models
//
// Steps:
//
//  1. optional-auth middleware — required-auth path if credentials
//     are present; anonymous-access fall-through if the
//     single-tenant org's metadata permits.
//  2. ListEnabledOrgLanguageModels (DB-only path; the config.json
//     fallback is deferred to a later change — see progress.md for
//     the queued follow-up).
//  3. Map each row's config JSON to the public {provider, model,
//     displayName} projection. config has many other fields (api
//     keys, base URLs, region, etc.) that MUST NOT leak to the
//     client — the projection is the security boundary.
//  4. Encode the array as JSON. Empty result encodes as `[]`.
package api

import (
	"encoding/json"
	"net/http"

	"codeintel/internal/auth"
)

// languageModelInfo is the safe-to-leak projection of an
// OrgLanguageModel row:
//
//	{ provider: string, model: string, displayName?: string }
//
// displayName uses `omitempty` so an absent value emits no JSON
// key — preserving the optional-field contract on the wire.
type languageModelInfo struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName,omitempty"`
}

// languageModelConfig is the minimal projection needed to build the
// languageModelInfo response. The full config has many additional
// fields (api keys, base URLs, region) that the handler MUST NOT
// echo — the typed projection is the safety boundary.
type languageModelConfig struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName,omitempty"`
}

// handleListOrgLanguageModels resolves auth (optional, anonymous-
// permitted), lists the enabled OrgLanguageModel rows, projects to
// the public info shape, and emits the array as JSON.
func (s *Server) handleListOrgLanguageModels(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveOptionalFromHeaders(r.Context(), r.Header, auth.OptionalAuthConfig{
		SingleTenantOrgID: s.cfg.SingleTenantOrgID,
		EncryptionKey:     s.cfg.EncryptionKey,
		Queries:           s.cfg.Queries,
	})
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.modelsLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	rows, err := s.cfg.Queries.ListEnabledOrgLanguageModels(r.Context(), authCtx.Org.ID)
	if err != nil {
		s.modelsLogger.Error("ListEnabledOrgLanguageModels failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	infos := make([]languageModelInfo, 0, len(rows))
	for i, row := range rows {
		var cfg languageModelConfig
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			// A malformed config row surfaces a 500 — never silently
			// drop the row, which could mask config corruption from
			// operators.
			s.modelsLogger.Error("decode language model config", "err", err, "orgId", authCtx.Org.ID, "rowIndex", i)
			writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
			return
		}
		infos = append(infos, languageModelInfo{
			Provider:    cfg.Provider,
			Model:       cfg.Model,
			DisplayName: cfg.DisplayName,
		})
	}

	encoded, err := json.Marshal(infos)
	if err != nil {
		s.modelsLogger.Error("encode models response", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// Compile-time check: the auth.OptionalQuerier surface is satisfied
// by AuthQuerier (so callers passing api.Config.Queries can hand it
// straight to optional-auth resolution).
var _ auth.OptionalQuerier = (AuthQuerier)(nil)
