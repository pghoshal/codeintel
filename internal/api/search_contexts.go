package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

const maxPutSearchContextsBodyBytes = 2 << 20

type searchContextsQuerier interface {
	ListOrgSearchContexts(ctx context.Context, orgID int32) ([]db.SearchContextRow, error)
	ReplaceOrgSearchContexts(ctx context.Context, orgID int32, contexts []db.SearchContextInput) error
}

type searchContextResponse struct {
	ID            int32           `json:"id"`
	Name          string          `json:"name"`
	Description   *string         `json:"description"`
	Config        json.RawMessage `json:"config"`
	IsDeclarative bool            `json:"isDeclarative"`
	RepoNames     []string        `json:"repoNames"`
}

type putSearchContextsBody struct {
	Contexts json.RawMessage `json:"contexts"`
}

func (s *Server) searchContextQueries() (searchContextsQuerier, bool) {
	q, ok := s.cfg.Queries.(searchContextsQuerier)
	return q, ok
}

func (s *Server) handleListOrgSearchContexts(w http.ResponseWriter, r *http.Request) {
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
		s.reposLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	q, ok := s.searchContextQueries()
	if !ok {
		s.reposLogger.Error("search context queries are not configured")
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	rows, err := q.ListOrgSearchContexts(r.Context(), authCtx.Org.ID)
	if err != nil {
		s.reposLogger.Error("ListOrgSearchContexts failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	resp := make([]searchContextResponse, 0, len(rows))
	for _, row := range rows {
		repoNames := row.RepoNames
		if repoNames == nil {
			repoNames = []string{}
		}
		resp = append(resp, searchContextResponse{
			ID:            row.ID,
			Name:          row.Name,
			Description:   row.Description,
			Config:        row.Config,
			IsDeclarative: row.IsDeclarative,
			RepoNames:     repoNames,
		})
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		s.reposLogger.Error("encode search contexts", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

func (s *Server) handlePutOrgSearchContexts(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.reposLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if authCtx.Role != auth.OrgRoleOwner {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "Only organization owners can configure search contexts.",
		}, s.reposLogger)
		return
	}
	q, ok := s.searchContextQueries()
	if !ok {
		s.reposLogger.Error("search context queries are not configured")
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPutSearchContextsBodyBytes)
	var body putSearchContextsBody
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
			}, s.reposLogger)
		case errors.Is(err, io.EOF):
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is empty.",
			}, s.reposLogger)
		default:
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is not valid JSON.",
			}, s.reposLogger)
		}
		return
	}
	if len(body.Contexts) == 0 || string(body.Contexts) == "null" {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Request body is missing the required `contexts` field.",
		}, s.reposLogger)
		return
	}
	contexts, svcErr := parseSearchContextsBody(body.Contexts)
	if svcErr != nil {
		writeServiceError(w, *svcErr, s.reposLogger)
		return
	}
	if err := q.ReplaceOrgSearchContexts(r.Context(), authCtx.Org.ID, contexts); err != nil {
		if errors.Is(err, db.ErrSearchContextRepoNotFound) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Search context references repositories not found in this tenant.",
			}, s.reposLogger)
			return
		}
		s.reposLogger.Error("ReplaceOrgSearchContexts failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"success":true}`))
}

func parseSearchContextsBody(raw json.RawMessage) ([]db.SearchContextInput, *ServiceError) {
	var array []map[string]any
	if err := json.Unmarshal(raw, &array); err == nil {
		return normalizeSearchContextArray(array)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err == nil {
		return normalizeSearchContextMap(object)
	}
	return nil, &ServiceError{
		StatusCode: http.StatusBadRequest,
		ErrorCode:  errorCodeInvalidRequestBody,
		Message:    "`contexts` must be an object or array.",
	}
}

func normalizeSearchContextArray(array []map[string]any) ([]db.SearchContextInput, *ServiceError) {
	seen := map[string]struct{}{}
	out := make([]db.SearchContextInput, 0, len(array))
	for _, item := range array {
		name, _ := item["name"].(string)
		if name == "" {
			return nil, &ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Search context name is required."}
		}
		if _, ok := seen[name]; ok {
			return nil, &ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Duplicate search context '" + name + "'."}
		}
		seen[name] = struct{}{}
		description, _ := item["description"].(string)
		var descriptionPtr *string
		if description != "" {
			descriptionPtr = &description
		}
		repos, svcErr := stringArrayField(item, "repos")
		if svcErr != nil {
			return nil, svcErr
		}
		include, svcErr := stringArrayField(item, "include")
		if svcErr != nil {
			return nil, svcErr
		}
		include = uniqueStrings(append(include, repos...))
		config := map[string]any{}
		if descriptionPtr != nil {
			config["description"] = description
		}
		if len(include) > 0 {
			config["include"] = include
		}
		for _, key := range []string{"includeConnections", "exclude", "excludeConnections", "includeTopics", "excludeTopics"} {
			values, svcErr := stringArrayField(item, key)
			if svcErr != nil {
				return nil, svcErr
			}
			if len(values) > 0 {
				config[key] = values
			}
		}
		out = append(out, db.SearchContextInput{Name: name, Description: descriptionPtr, Config: config, RepoNames: include})
	}
	return out, nil
}

func normalizeSearchContextMap(object map[string]any) ([]db.SearchContextInput, *ServiceError) {
	names := make([]string, 0, len(object))
	for name := range object {
		if name == "" {
			return nil, &ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Search context name is required."}
		}
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]db.SearchContextInput, 0, len(names))
	for _, name := range names {
		cfg, ok := object[name].(map[string]any)
		if !ok {
			return nil, &ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Search context '" + name + "' is invalid."}
		}
		description, _ := cfg["description"].(string)
		var descriptionPtr *string
		if description != "" {
			descriptionPtr = &description
		}
		include, svcErr := stringArrayField(cfg, "include")
		if svcErr != nil {
			return nil, svcErr
		}
		out = append(out, db.SearchContextInput{Name: name, Description: descriptionPtr, Config: cfg, RepoNames: include})
	}
	return out, nil
}

func stringArrayField(obj map[string]any, key string) ([]string, *ServiceError) {
	raw, ok := obj[key]
	if !ok || raw == nil {
		return []string{}, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, &ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Search context field `" + key + "` must be an array of strings."}
	}
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, ok := v.(string)
		if !ok {
			return nil, &ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Search context field `" + key + "` must be an array of strings."}
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
