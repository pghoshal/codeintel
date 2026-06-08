// /api/{domain}/mcp — headless MCP transport entrypoint.
//
// This slice ports the HTTP boundary and tenant checks. The actual
// MCP JSON-RPC transport/tool execution is injected through
// MCPBackend so the API package stays independent of UI/runtime
// code and can be wired to the production MCP engine later.
package api

import (
	"context"
	"errors"
	"io"
	"net/http"

	"codeintel/internal/auth"
)

const maxMCPBodyBytes = 4 << 20

var ErrMCPBackendNotConfigured = errors.New("api: mcp backend is not configured")

type MCPBackend interface {
	Handle(ctx context.Context, req MCPRequest) (MCPResponse, error)
}

type MCPRequest struct {
	OrgID     int32
	OrgDomain string
	Method    string
	Headers   http.Header
	Body      []byte
}

type MCPResponse struct {
	StatusCode  int
	ContentType string
	Headers     http.Header
	Body        []byte
}

type NoopMCPBackend struct{}

func (NoopMCPBackend) Handle(context.Context, MCPRequest) (MCPResponse, error) {
	return MCPResponse{}, ErrMCPBackendNotConfigured
}

func (s *Server) mcpBackend() MCPBackend {
	if s.cfg.MCPBackend == nil {
		return NoopMCPBackend{}
	}
	return s.cfg.MCPBackend
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.mcpLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	domain := r.PathValue("domain")
	if domain == "" || domain != authCtx.Org.Domain {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "MCP endpoint domain does not belong to the authenticated organization.",
		}, s.mcpLogger)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxMCPBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusRequestEntityTooLarge,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Request body exceeds the maximum allowed size.",
			}, s.mcpLogger)
			return
		}
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Request body could not be read.",
		}, s.mcpLogger)
		return
	}

	resp, err := s.mcpBackend().Handle(r.Context(), MCPRequest{
		OrgID:     authCtx.Org.ID,
		OrgDomain: authCtx.Org.Domain,
		Method:    r.Method,
		Headers:   r.Header.Clone(),
		Body:      body,
	})
	if err != nil {
		if errors.Is(err, ErrMCPBackendNotConfigured) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusServiceUnavailable,
				ErrorCode:  "MCP_BACKEND_NOT_CONFIGURED",
				Message:    "MCP backend is not configured.",
			}, s.mcpLogger)
			return
		}
		s.mcpLogger.Error("mcp backend failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusBadGateway, unexpectedErrorBody)
		return
	}

	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	for key, values := range resp.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	} else if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}
