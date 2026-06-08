package llmgateway

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codeintel/pkg/llmproxy"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	defaultRequestTimeout = 6 * time.Minute
	staleRequestGrace     = 30 * time.Second
	orphanedRequestGrace  = 15 * time.Second
	chatPath              = "/internal/llm/chat"
)

type Config struct {
	Store        RequestStore
	HTTP         *http.Client
	Logger       *slog.Logger
	Token        string
	Timeout      time.Duration
	Enqueuer     Enqueuer
	MaxTaskRetry int
}

type Server struct {
	cfg    Config
	logger *slog.Logger
}

func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.HTTP == nil {
		cfg.HTTP = http.DefaultClient
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultRequestTimeout
	}
	return &Server{cfg: cfg, logger: logger.With("component", "llm-gateway")}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, llmproxy.ChatResponse{Status: "FAILED", Error: "unauthorized"})
		return
	}
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, chatPath+"/") {
		s.handleGet(w, r)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != chatPath {
		http.NotFound(w, r)
		return
	}
	s.handlePost(w, r)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	var req llmproxy.ChatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, llmproxy.ChatResponse{Status: "FAILED", Error: "invalid llm request"})
		return
	}
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.RequestID == "" {
		req.RequestID = "llm-" + uuid.NewString()
	}
	if err := validateChatRequest(req); err != nil {
		writeJSON(w, http.StatusBadRequest, llmproxy.ChatResponse{RequestID: req.RequestID, Status: "FAILED", Error: err.Error(), Model: req.Model})
		return
	}
	if s.cfg.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, llmproxy.ChatResponse{RequestID: req.RequestID, Status: "FAILED", Error: "durable llm request store is not configured", Model: req.Model})
		return
	}
	claimedRow, claimed, err := s.cfg.Store.ClaimStarted(r.Context(), req, s.cfg.Timeout+staleRequestGrace)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.logger.Error("claim llm request failed", "err", err, "requestId", req.RequestID, "orgId", req.OrgID)
			writeJSON(w, http.StatusInternalServerError, llmproxy.ChatResponse{RequestID: req.RequestID, Status: "FAILED", Error: "persist llm request state failed", Model: req.Model})
			return
		}
		writeJSON(w, http.StatusNotFound, llmproxy.ChatResponse{RequestID: req.RequestID, Status: "FAILED", Error: "llm request state not found after claim", Model: req.Model})
		return
	}
	if !claimed && claimedRow.Status == "SUCCEEDED" && len(claimedRow.Response) > 2 {
		writeJSON(w, http.StatusOK, chatResponseFromRow(claimedRow))
		return
	}
	if !claimed {
		status := claimedRow.Status
		code := http.StatusAccepted
		if status == "FAILED" {
			code = http.StatusBadGateway
		}
		writeJSON(w, code, llmproxy.ChatResponse{
			RequestID: req.RequestID,
			Status:    status,
			Error:     deref(claimedRow.Error),
			Model:     req.Model,
			Budget:    req.Budget,
			Metadata:  req.Metadata,
		})
		return
	}

	if err := enqueueChatCompletion(s.cfg.Enqueuer, req.OrgID, req.RequestID, claimedRow.UpdatedAt, s.cfg.Timeout, s.cfg.MaxTaskRetry); err != nil {
		_ = s.cfg.Store.MarkFailed(r.Context(), req.OrgID, req.RequestID, err.Error())
		s.logger.Error("enqueue llm completion failed", "err", err, "requestId", req.RequestID, "orgId", req.OrgID)
		writeJSON(w, http.StatusServiceUnavailable, llmproxy.ChatResponse{RequestID: req.RequestID, Status: "FAILED", Error: "enqueue llm completion failed", Model: req.Model})
		return
	}
	writeJSON(w, http.StatusAccepted, llmproxy.ChatResponse{RequestID: req.RequestID, Status: "IN_PROGRESS", Model: req.Model, Budget: req.Budget, Metadata: req.Metadata})
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, llmproxy.ChatResponse{Status: "FAILED", Error: "durable llm request store is not configured"})
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, chatPath+"/"), "/")
	orgID64, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("orgId")), 10, 32)
	if err != nil || orgID64 <= 0 {
		writeJSON(w, http.StatusBadRequest, llmproxy.ChatResponse{RequestID: id, Status: "FAILED", Error: "orgId query parameter is required"})
		return
	}
	row, err := s.cfg.Store.Get(r.Context(), int32(orgID64), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, llmproxy.ChatResponse{RequestID: id, Status: "FAILED", Error: "llm request not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, llmproxy.ChatResponse{RequestID: id, Status: "FAILED", Error: "read llm request state failed"})
		return
	}
	if len(row.Response) > 2 {
		writeJSON(w, http.StatusOK, chatResponseFromRow(row))
		return
	}
	if row.Status == "IN_PROGRESS" && row.Attempts == 0 && time.Since(row.UpdatedAt) > orphanedRequestGrace {
		if err := enqueueChatCompletion(s.cfg.Enqueuer, row.OrgID, row.ID, row.UpdatedAt, s.cfg.Timeout, s.cfg.MaxTaskRetry); err != nil {
			_ = s.cfg.Store.MarkFailed(r.Context(), row.OrgID, row.ID, err.Error())
			s.logger.Error("rescue orphaned llm request failed", "err", err, "requestId", row.ID, "orgId", row.OrgID)
			writeJSON(w, http.StatusServiceUnavailable, llmproxy.ChatResponse{RequestID: id, Status: "FAILED", Error: "rescue orphaned llm request failed"})
			return
		}
		s.logger.Warn("rescued orphaned llm request with zero attempts", "requestId", row.ID, "orgId", row.OrgID, "updatedAt", row.UpdatedAt)
	}
	request := chatRequestFromRow(row)
	resp := llmproxy.ChatResponse{
		RequestID: row.ID,
		Status:    row.Status,
		Error:     deref(row.Error),
		Model:     firstModel(request.Model, llmproxy.LanguageModelInfo{Provider: row.Provider, Model: row.Model}),
		Budget:    request.Budget,
		Metadata:  request.Metadata,
	}
	writeJSON(w, http.StatusOK, resp)
}

func chatResponseFromRow(row RequestRow) llmproxy.ChatResponse {
	request := chatRequestFromRow(row)
	var resp llmproxy.ChatResponse
	if len(row.Response) > 2 {
		_ = json.Unmarshal(row.Response, &resp)
	}
	if resp.RequestID == "" {
		resp.RequestID = row.ID
	}
	if resp.Status == "" {
		resp.Status = row.Status
	}
	if resp.Error == "" {
		resp.Error = deref(row.Error)
	}
	resp.Model = firstModel(resp.Model, firstModel(request.Model, llmproxy.LanguageModelInfo{Provider: row.Provider, Model: row.Model}))
	if resp.Budget.Mode == "" {
		resp.Budget = request.Budget
	}
	if len(resp.Metadata) == 0 {
		resp.Metadata = request.Metadata
	}
	return resp
}

func chatRequestFromRow(row RequestRow) llmproxy.ChatRequest {
	var req llmproxy.ChatRequest
	if len(row.Request) > 2 {
		_ = json.Unmarshal(row.Request, &req)
	}
	return req
}

func firstModel(primary, fallback llmproxy.LanguageModelInfo) llmproxy.LanguageModelInfo {
	if strings.TrimSpace(primary.Model) != "" || strings.TrimSpace(primary.Provider) != "" || strings.TrimSpace(primary.DisplayName) != "" {
		return primary
	}
	return fallback
}

func (s *Server) authorized(r *http.Request) bool {
	token := strings.TrimSpace(s.cfg.Token)
	if token == "" {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func validateChatRequest(req llmproxy.ChatRequest) error {
	if req.OrgID <= 0 {
		return fmt.Errorf("orgId is required")
	}
	if req.Provider != "openai-compatible" {
		return fmt.Errorf("provider %q is not supported by the LLM gateway", req.Provider)
	}
	if strings.TrimSpace(req.OpenAI.Model) == "" || strings.TrimSpace(req.OpenAI.BaseURL) == "" {
		return fmt.Errorf("openai-compatible model and baseUrl are required")
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("messages are required")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
