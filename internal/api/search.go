// POST /api/search — headless code search entrypoint.
//
// This slice ports only the HTTP boundary and dependency contract.
// The actual layered retrieval implementation is injected through
// SearchBackend so API replicas do not carry indexer/search-engine
// binaries. A nil backend returns a typed 503; it never fabricates
// empty search results.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"codeintel/internal/auth"
)

const maxSearchBodyBytes = 1 << 20

var ErrSearchBackendNotConfigured = errors.New("api: search backend is not configured")
var ErrSearchBackendUnavailable = errors.New("api: search backend is unavailable")
var ErrSearchInvalidQuery = errors.New("api: search query is invalid")

type SearchBackend interface {
	Search(ctx context.Context, req SearchRequest) (json.RawMessage, error)
}

type SearchRequest struct {
	OrgID     int32
	OrgDomain string
	Query     string
	Options   map[string]any
}

type NoopSearchBackend struct{}

func (NoopSearchBackend) Search(context.Context, SearchRequest) (json.RawMessage, error) {
	return nil, ErrSearchBackendNotConfigured
}

func (s *Server) searchBackend() SearchBackend {
	if s.cfg.SearchBackend == nil {
		return NoopSearchBackend{}
	}
	return s.cfg.SearchBackend
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.searchLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSearchBodyBytes)
	var body map[string]any
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusRequestEntityTooLarge,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body exceeds the maximum allowed size.",
			}, s.searchLogger)
		case errors.Is(err, io.EOF):
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is empty.",
			}, s.searchLogger)
		default:
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body is not valid JSON.",
			}, s.searchLogger)
		}
		return
	}

	query, _ := body["query"].(string)
	if query == "" {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Search query is required.",
		}, s.searchLogger)
		return
	}
	delete(body, "query")

	resp, err := s.searchBackend().Search(r.Context(), SearchRequest{
		OrgID:     authCtx.Org.ID,
		OrgDomain: authCtx.Org.Domain,
		Query:     query,
		Options:   body,
	})
	if err != nil {
		if errors.Is(err, ErrSearchBackendNotConfigured) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusServiceUnavailable,
				ErrorCode:  "SEARCH_BACKEND_NOT_CONFIGURED",
				Message:    "Search backend is not configured.",
			}, s.searchLogger)
			return
		}
		if errors.Is(err, ErrSearchBackendUnavailable) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusServiceUnavailable,
				ErrorCode:  "SEARCH_BACKEND_UNAVAILABLE",
				Message:    "Search backend is unavailable.",
			}, s.searchLogger)
			return
		}
		if errors.Is(err, ErrSearchInvalidQuery) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  "SEARCH_INVALID_QUERY",
				Message:    "Search query is invalid.",
			}, s.searchLogger)
			return
		}
		s.searchLogger.Error("search backend failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusBadGateway, unexpectedErrorBody)
		return
	}
	if !json.Valid(resp) {
		s.searchLogger.Error("search backend returned invalid JSON", "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusBadGateway, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp)
}
