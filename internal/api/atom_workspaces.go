package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

const (
	defaultAtomAPIKeyName     = "atom-control-plane"
	maxAtomWorkspaceBodyBytes = 256 << 10
)

var (
	tenantDomainRegex     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	reservedTenantDomains = map[string]struct{}{
		"api": {}, "login": {}, "signup": {}, "onboard": {}, "redeem": {},
		"account": {}, "settings": {}, "staging": {}, "support": {},
		"docs": {}, "blog": {}, "contact": {}, "status": {},
	}
)

type atomWorkspaceQuerier interface {
	UpsertAtomWorkspaceTenant(ctx context.Context, p db.AtomWorkspaceTenantParams) (db.AtomWorkspaceTenant, error)
}

type provisionAtomWorkspaceBody struct {
	WorkspaceID   string `json:"workspaceId"`
	WorkspaceName string `json:"workspaceName"`
	Domain        string `json:"domain,omitempty"`
	CreateAPIKey  *bool  `json:"createApiKey,omitempty"`
	APIKeyName    string `json:"apiKeyName,omitempty"`
}

type atomWorkspaceTenantResponse struct {
	ID                int32  `json:"id"`
	Name              string `json:"name"`
	Domain            string `json:"domain"`
	AtomWorkspaceID   string `json:"atomWorkspaceId"`
	AtomWorkspaceName string `json:"atomWorkspaceName"`
}

type provisionAtomWorkspaceResponse struct {
	Tenant atomWorkspaceTenantResponse `json:"tenant"`
	APIKey string                      `json:"apiKey,omitempty"`
}

func (s *Server) handleProvisionAtomWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAtomControlPlane(w, r) {
		return
	}
	q, ok := s.cfg.Queries.(atomWorkspaceQuerier)
	if !ok || q == nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusServiceUnavailable,
			ErrorCode:  errorCodeUnexpectedError,
			Message:    "Atom workspace provisioning is not configured.",
		}, s.statusLogger)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxAtomWorkspaceBodyBytes)
	var body provisionAtomWorkspaceBody
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Request body is not valid JSON.",
		}, s.statusLogger)
		return
	}

	body.WorkspaceID = strings.TrimSpace(body.WorkspaceID)
	body.WorkspaceName = strings.TrimSpace(body.WorkspaceName)
	body.Domain = strings.TrimSpace(body.Domain)
	body.APIKeyName = strings.TrimSpace(body.APIKeyName)
	if body.WorkspaceID == "" {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "workspaceId is required.",
		}, s.statusLogger)
		return
	}
	if body.WorkspaceName == "" {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "workspaceName is required.",
		}, s.statusLogger)
		return
	}
	domain, svcErr := resolveTenantDomain(body.WorkspaceID, body.Domain)
	if svcErr != nil {
		writeServiceError(w, *svcErr, s.statusLogger)
		return
	}
	createAPIKey := true
	if body.CreateAPIKey != nil {
		createAPIKey = *body.CreateAPIKey
	}
	if body.APIKeyName == "" {
		body.APIKeyName = defaultAtomAPIKeyName
	}

	var rawAPIKey, apiKeyHash string
	if createAPIKey {
		var err error
		rawAPIKey, apiKeyHash, err = generateTenantAPIKey(s.cfg.EncryptionKey)
		if err != nil {
			s.statusLogger.Error("generate tenant api key failed", "err", err)
			writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
			return
		}
	}

	tenant, err := q.UpsertAtomWorkspaceTenant(r.Context(), db.AtomWorkspaceTenantParams{
		WorkspaceID:   body.WorkspaceID,
		WorkspaceName: body.WorkspaceName,
		Domain:        domain,
		APIKeyName:    body.APIKeyName,
		APIKeyHash:    apiKeyHash,
	})
	if err != nil {
		if errors.Is(err, db.ErrOrgDomainExists) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusBadRequest,
				ErrorCode:  "ORG_DOMAIN_ALREADY_EXISTS",
				Message:    "Organization domain is already used by another Atom workspace.",
			}, s.statusLogger)
			return
		}
		if errors.Is(err, db.ErrAtomServiceUserConflict) {
			writeServiceError(w, ServiceError{
				StatusCode: http.StatusConflict,
				ErrorCode:  errorCodeInvalidRequestBody,
				Message:    "Atom service principal email is already used by another user.",
			}, s.statusLogger)
			return
		}
		s.statusLogger.Error("UpsertAtomWorkspaceTenant failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	writeJSON(w, http.StatusOK, provisionAtomWorkspaceResponse{
		Tenant: atomWorkspaceTenantResponse{
			ID:                tenant.ID,
			Name:              tenant.Name,
			Domain:            tenant.Domain,
			AtomWorkspaceID:   tenant.AtomWorkspaceID,
			AtomWorkspaceName: tenant.AtomWorkspaceName,
		},
		APIKey: rawAPIKey,
	}, s.statusLogger)
}

func (s *Server) handleGetTenant(w http.ResponseWriter, r *http.Request) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return
		}
		s.statusLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	domain := r.PathValue("domain")
	if domain != authCtx.Org.Domain {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusForbidden,
			ErrorCode:  errorCodeInsufficientPermission,
			Message:    "API key does not belong to the requested organization.",
		}, s.statusLogger)
		return
	}
	workspaceID := ""
	if authCtx.Org.AtomWorkspaceID != nil {
		workspaceID = *authCtx.Org.AtomWorkspaceID
	}
	writeJSON(w, http.StatusOK, atomWorkspaceTenantResponse{
		ID:                authCtx.Org.ID,
		Name:              authCtx.Org.Name,
		Domain:            authCtx.Org.Domain,
		AtomWorkspaceID:   workspaceID,
		AtomWorkspaceName: authCtx.Org.Name,
	}, s.statusLogger)
}

func (s *Server) authorizeAtomControlPlane(w http.ResponseWriter, r *http.Request) bool {
	expected := s.cfg.AtomControlPlaneToken
	if expected == "" {
		expected = os.Getenv("CODEINTEL_ATOM_CONTROL_PLANE_TOKEN")
	}
	if expected == "" {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusServiceUnavailable,
			ErrorCode:  errorCodeUnexpectedError,
			Message:    "Atom control-plane token is not configured.",
		}, s.statusLogger)
		return false
	}
	got := r.Header.Get("X-Codeintel-Atom-Token")
	if got == "" {
		authz := r.Header.Get("Authorization")
		if strings.HasPrefix(authz, "Bearer ") {
			got = strings.TrimPrefix(authz, "Bearer ")
		}
	}
	if got != expected {
		writeServiceError(w, ServiceError{
			StatusCode: http.StatusUnauthorized,
			ErrorCode:  errorCodeNotAuthenticated,
			Message:    "Atom control-plane authentication failed.",
		}, s.statusLogger)
		return false
	}
	return true
}

func resolveTenantDomain(workspaceID, explicit string) (string, *ServiceError) {
	domain := strings.ToLower(strings.TrimSpace(explicit))
	if domain == "" {
		domain = strings.ToLower(workspaceID)
		domain = regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(domain, "-")
		domain = regexp.MustCompile(`-+`).ReplaceAllString(domain, "-")
		domain = strings.Trim(domain, "-")
		if len(domain) > 63 {
			domain = strings.TrimRight(domain[:63], "-")
		}
		if domain == "" {
			domain = "atom-workspace"
		}
	}
	if len(domain) < 2 || len(domain) > 63 || !tenantDomainRegex.MatchString(domain) {
		return "", &ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Domain must be 2-63 characters, lowercase alphanumeric or hyphen, and cannot start or end with a hyphen.",
		}
	}
	if _, reserved := reservedTenantDomains[domain]; reserved {
		return "", &ServiceError{
			StatusCode: http.StatusBadRequest,
			ErrorCode:  errorCodeInvalidRequestBody,
			Message:    "Domain is reserved.",
		}
	}
	return domain, nil
}

func generateTenantAPIKey(encryptionKey string) (string, string, error) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", err
	}
	secret := hex.EncodeToString(secretBytes)
	return auth.ApiKeyPrefix + secret, auth.HashSecret(encryptionKey, secret), nil
}
