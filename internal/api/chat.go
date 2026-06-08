// POST /api/chat and POST /api/chat/blocking — headless chat entrypoints.
//
// The legacy Next route owned HTTP/session/persistence and called the
// chat agent directly; it did not tunnel through MCP. The Go split keeps
// that architecture: API handlers own auth, chat rows, repo-scope
// expansion, and persistence, while ChatBackend owns the shared
// retrieval/agent loop also used by MCP ask_codebase.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"codeintel/internal/auth"
	"codeintel/internal/db"

	"github.com/google/uuid"
)

const maxChatBodyBytes = 4 << 20

var ErrChatBackendNotConfigured = errors.New("api: chat backend is not configured")

type ChatBackend interface {
	Ask(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Get(ctx context.Context, req ChatResultRequest) (ChatResponse, error)
}

type ChatRequest struct {
	OrgID         int32
	OrgDomain     string
	UserID        string
	ChatID        string
	Query         string
	Repos         []string
	LanguageModel *ChatLanguageModel
	PriorMessages []ChatMessage
	Async         bool
	AnswerBudget  string
}

type ChatResultRequest struct {
	OrgID     int32
	OrgDomain string
	UserID    string
	ChatID    string
	SessionID string
}

type ChatLanguageModel struct {
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	DisplayName string `json:"displayName,omitempty"`
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	Answer        string            `json:"answer"`
	ChatID        string            `json:"chatId"`
	LanguageModel ChatLanguageModel `json:"languageModel"`
	ToolTrace     any               `json:"toolTrace,omitempty"`
	SessionID     string            `json:"sessionId,omitempty"`
	Status        string            `json:"status,omitempty"`
	Error         string            `json:"error,omitempty"`
}

type NoopChatBackend struct{}

func (NoopChatBackend) Ask(context.Context, ChatRequest) (ChatResponse, error) {
	return ChatResponse{}, ErrChatBackendNotConfigured
}

func (NoopChatBackend) Get(context.Context, ChatResultRequest) (ChatResponse, error) {
	return ChatResponse{}, ErrChatBackendNotConfigured
}

type chatQuerier interface {
	CreateChat(ctx context.Context, p db.CreateChatParams) (db.ChatRow, error)
	GetChatForOrg(ctx context.Context, orgID int32, chatID string) (db.ChatRow, error)
	UpdateChatMessages(ctx context.Context, orgID int32, chatID string, messages json.RawMessage) error
	UpdateChatName(ctx context.Context, orgID int32, chatID string, name string) error
	ListSearchContextRepoNames(ctx context.Context, orgID int32, name string) ([]string, error)
	GetOrgRepoForRead(ctx context.Context, orgID int32, repoName string) (db.RepoReadRow, error)
}

func (s *Server) chatBackend() ChatBackend {
	if s.cfg.ChatBackend != nil {
		return s.cfg.ChatBackend
	}
	return NoopChatBackend{}
}

func (s *Server) chatQueries() (chatQuerier, bool) {
	q, ok := s.cfg.Queries.(chatQuerier)
	return q, ok
}

type searchScopeBody struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type chatBody struct {
	ID                   string            `json:"id"`
	Query                string            `json:"query,omitempty"`
	Messages             []json.RawMessage `json:"messages"`
	SelectedSearchScopes []searchScopeBody `json:"selectedSearchScopes"`
	LanguageModel        ChatLanguageModel `json:"languageModel"`
	AnswerBudget         string            `json:"answerBudget,omitempty"`
	AnswerMode           string            `json:"answerMode,omitempty"`
}

type blockingChatBody struct {
	Query         string             `json:"query"`
	Repos         []string           `json:"repos"`
	LanguageModel *ChatLanguageModel `json:"languageModel"`
	Visibility    string             `json:"visibility"`
	AnswerBudget  string             `json:"answerBudget,omitempty"`
	AnswerMode    string             `json:"answerMode,omitempty"`
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	authCtx, ok := s.resolveChatAuth(w, r)
	if !ok {
		return
	}
	q, ok := s.requireChatQueries(w)
	if !ok {
		return
	}

	var body chatBody
	if !decodeChatJSON(w, r, &body) {
		return
	}
	if body.ID == "" || (len(body.Messages) == 0 && strings.TrimSpace(body.Query) == "") || body.LanguageModel.Provider == "" || body.LanguageModel.Model == "" {
		writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Request body is missing required chat fields."}, s.chatLogger)
		return
	}
	chat, err := q.GetChatForOrg(r.Context(), authCtx.Org.ID, body.ID)
	if err != nil {
		if errors.Is(err, db.ErrChatNotFound) {
			writeServiceError(w, ServiceError{StatusCode: http.StatusNotFound, ErrorCode: "NOT_FOUND", Message: "Chat not found."}, s.chatLogger)
			return
		}
		s.chatLogger.Error("GetChatForOrg failed", "err", err, "orgId", authCtx.Org.ID, "chatId", body.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if chat.CreatedByID == nil || *chat.CreatedByID != authCtx.UserID {
		writeServiceError(w, ServiceError{StatusCode: http.StatusForbidden, ErrorCode: errorCodeInsufficientPermission, Message: "Only the owner of a chat can send messages."}, s.chatLogger)
		return
	}

	messagesForTurn := body.Messages
	if strings.TrimSpace(body.Query) != "" {
		messagesForTurn = append(decodeRawMessages(chat.Messages), newTextMessage("user", body.Query))
	}
	query, prior, err := extractChatTurn(messagesForTurn)
	if err != nil {
		writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: err.Error()}, s.chatLogger)
		return
	}
	repos, err := expandSearchScopes(r.Context(), q, authCtx.Org.ID, body.SelectedSearchScopes)
	if err != nil {
		writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: err.Error()}, s.chatLogger)
		return
	}

	resp, err := s.chatBackend().Ask(r.Context(), ChatRequest{
		OrgID:         authCtx.Org.ID,
		OrgDomain:     authCtx.Org.Domain,
		UserID:        authCtx.UserID,
		ChatID:        body.ID,
		Query:         query,
		Repos:         repos,
		LanguageModel: &body.LanguageModel,
		PriorMessages: prior,
		Async:         true,
		AnswerBudget:  firstNonEmptyString(body.AnswerBudget, body.AnswerMode),
	})
	if err != nil {
		s.writeChatBackendError(w, err)
		return
	}
	normalizeChatResponse(&resp)
	var persisted json.RawMessage
	if chatResponseSucceeded(resp) {
		persisted = appendAssistantMessage(messagesForTurn, resp)
	} else {
		persisted = marshalRawMessages(messagesForTurn)
	}
	if err := q.UpdateChatMessages(r.Context(), authCtx.Org.ID, body.ID, persisted); err != nil {
		s.chatLogger.Error("UpdateChatMessages failed", "err", err, "orgId", authCtx.Org.ID, "chatId", body.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	writeChatJSON(w, resp, s.chatLogger)
}

func (s *Server) handleBlockingChat(w http.ResponseWriter, r *http.Request) {
	authCtx, ok := s.resolveChatAuth(w, r)
	if !ok {
		return
	}
	q, ok := s.requireChatQueries(w)
	if !ok {
		return
	}
	var body blockingChatBody
	if !decodeChatJSON(w, r, &body) {
		return
	}
	body.Query = strings.TrimSpace(body.Query)
	if body.Query == "" {
		writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "query is required."}, s.chatLogger)
		return
	}
	visibility := strings.ToUpper(strings.TrimSpace(body.Visibility))
	if visibility == "" {
		visibility = "PRIVATE"
	}
	if visibility != "PRIVATE" && visibility != "PUBLIC" {
		writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "visibility must be PRIVATE or PUBLIC."}, s.chatLogger)
		return
	}
	if body.LanguageModel != nil && (body.LanguageModel.Provider == "" || body.LanguageModel.Model == "") {
		writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "languageModel requires provider and model."}, s.chatLogger)
		return
	}
	repos := cleanUnique(body.Repos)
	for _, repo := range repos {
		if _, err := q.GetOrgRepoForRead(r.Context(), authCtx.Org.ID, repo); err != nil {
			if errors.Is(err, db.ErrRepoNotFound) {
				writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: fmt.Sprintf("Repository %q not found.", repo)}, s.chatLogger)
				return
			}
			s.chatLogger.Error("GetOrgRepoForRead failed", "err", err, "orgId", authCtx.Org.ID, "repo", repo)
			writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
			return
		}
	}

	chatID := "chat-" + uuid.NewString()
	userID := authCtx.UserID
	userMessage := newTextMessage("user", body.Query)
	initialMessages, _ := json.Marshal([]json.RawMessage{userMessage})
	chat, err := q.CreateChat(r.Context(), db.CreateChatParams{
		ID:          chatID,
		OrgID:       authCtx.Org.ID,
		CreatedByID: &userID,
		Visibility:  visibility,
		Messages:    initialMessages,
	})
	if err != nil {
		s.chatLogger.Error("CreateChat failed", "err", err, "orgId", authCtx.Org.ID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}

	resp, err := s.chatBackend().Ask(r.Context(), ChatRequest{
		OrgID:         authCtx.Org.ID,
		OrgDomain:     authCtx.Org.Domain,
		UserID:        authCtx.UserID,
		ChatID:        chat.ID,
		Query:         body.Query,
		Repos:         repos,
		LanguageModel: body.LanguageModel,
		Async:         true,
		AnswerBudget:  firstNonEmptyString(body.AnswerBudget, body.AnswerMode),
	})
	if err != nil {
		s.writeChatBackendError(w, err)
		return
	}
	normalizeChatResponse(&resp)
	if chatResponseSucceeded(resp) {
		persisted := appendAssistantMessage([]json.RawMessage{userMessage}, resp)
		if err := q.UpdateChatMessages(r.Context(), authCtx.Org.ID, chat.ID, persisted); err != nil {
			s.chatLogger.Error("UpdateChatMessages failed", "err", err, "orgId", authCtx.Org.ID, "chatId", chat.ID)
			writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
			return
		}
	}
	if err := q.UpdateChatName(r.Context(), authCtx.Org.ID, chat.ID, fallbackChatName(body.Query)); err != nil {
		s.chatLogger.Warn("UpdateChatName failed", "err", err, "orgId", authCtx.Org.ID, "chatId", chat.ID)
	}
	resp.ChatID = chat.ID
	writeChatJSON(w, resp, s.chatLogger)
}

func (s *Server) handleGetChatResult(w http.ResponseWriter, r *http.Request) {
	authCtx, ok := s.resolveChatAuth(w, r)
	if !ok {
		return
	}
	q, ok := s.requireChatQueries(w)
	if !ok {
		return
	}
	chatID := strings.TrimSpace(r.PathValue("id"))
	if chatID == "" {
		writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "chat id is required."}, s.chatLogger)
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("sessionId"))
	if sessionID == "" {
		sessionID = strings.TrimSpace(r.URL.Query().Get("requestId"))
	}
	if sessionID == "" {
		writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "sessionId query parameter is required."}, s.chatLogger)
		return
	}
	chat, err := q.GetChatForOrg(r.Context(), authCtx.Org.ID, chatID)
	if err != nil {
		if errors.Is(err, db.ErrChatNotFound) {
			writeServiceError(w, ServiceError{StatusCode: http.StatusNotFound, ErrorCode: "NOT_FOUND", Message: "Chat not found."}, s.chatLogger)
			return
		}
		s.chatLogger.Error("GetChatForOrg failed", "err", err, "orgId", authCtx.Org.ID, "chatId", chatID)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	if chat.CreatedByID == nil || *chat.CreatedByID != authCtx.UserID {
		writeServiceError(w, ServiceError{StatusCode: http.StatusForbidden, ErrorCode: errorCodeInsufficientPermission, Message: "Only the owner of a chat can read results."}, s.chatLogger)
		return
	}
	resp, err := s.chatBackend().Get(r.Context(), ChatResultRequest{
		OrgID:     authCtx.Org.ID,
		OrgDomain: authCtx.Org.Domain,
		UserID:    authCtx.UserID,
		ChatID:    chat.ID,
		SessionID: sessionID,
	})
	if err != nil {
		s.writeChatBackendError(w, err)
		return
	}
	normalizeChatResponse(&resp)
	resp.ChatID = chat.ID
	if chatResponseSucceeded(resp) && !chatMessagesContainSession(chat.Messages, resp.SessionID) {
		persisted := appendAssistantMessage(decodeRawMessages(chat.Messages), resp)
		if err := q.UpdateChatMessages(r.Context(), authCtx.Org.ID, chat.ID, persisted); err != nil {
			s.chatLogger.Error("UpdateChatMessages failed", "err", err, "orgId", authCtx.Org.ID, "chatId", chat.ID)
			writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
			return
		}
	}
	writeChatJSON(w, resp, s.chatLogger)
}

func (s *Server) resolveChatAuth(w http.ResponseWriter, r *http.Request) (auth.AuthContext, bool) {
	authCtx, err := auth.ResolveFromHeaders(r.Context(), r.Header, s.cfg.EncryptionKey, s.cfg.Queries)
	if err != nil {
		if isAuthFailure(err) {
			writeStaticServiceError(w, http.StatusUnauthorized, notAuthenticatedBody)
			return auth.AuthContext{}, false
		}
		s.chatLogger.Error("auth resolution failed", "err", err)
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return auth.AuthContext{}, false
	}
	return authCtx, true
}

func (s *Server) requireChatQueries(w http.ResponseWriter) (chatQuerier, bool) {
	q, ok := s.chatQueries()
	if !ok {
		writeServiceError(w, ServiceError{StatusCode: http.StatusServiceUnavailable, ErrorCode: "CHAT_BACKEND_NOT_CONFIGURED", Message: "Chat persistence is not configured."}, s.chatLogger)
		return nil, false
	}
	return q, true
}

func decodeChatJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxChatBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.As(err, &maxErr):
			writeServiceError(w, ServiceError{StatusCode: http.StatusRequestEntityTooLarge, ErrorCode: errorCodeInvalidRequestBody, Message: "Request body exceeds the maximum allowed size."}, nil)
		case errors.Is(err, io.EOF):
			writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Request body is empty."}, nil)
		default:
			writeServiceError(w, ServiceError{StatusCode: http.StatusBadRequest, ErrorCode: errorCodeInvalidRequestBody, Message: "Request body is not valid JSON."}, nil)
		}
		return false
	}
	return true
}

func (s *Server) writeChatBackendError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrChatBackendNotConfigured) {
		writeServiceError(w, ServiceError{StatusCode: http.StatusServiceUnavailable, ErrorCode: "CHAT_BACKEND_NOT_CONFIGURED", Message: "Chat backend is not configured."}, s.chatLogger)
		return
	}
	msg := err.Error()
	status := http.StatusBadGateway
	if strings.Contains(msg, "not configured") || strings.Contains(msg, "not found") || strings.Contains(msg, "required") || strings.Contains(msg, "not supported") {
		status = http.StatusBadRequest
	}
	writeServiceError(w, ServiceError{StatusCode: status, ErrorCode: errorCodeUnexpectedError, Message: msg}, s.chatLogger)
}

func writeJSON(w http.ResponseWriter, status int, value any, logger interface{ Error(string, ...any) }) {
	body, err := json.Marshal(value)
	if err != nil {
		if logger != nil {
			logger.Error("encode JSON response", "err", err)
		}
		writeStaticServiceError(w, http.StatusInternalServerError, unexpectedErrorBody)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeChatJSON(w http.ResponseWriter, resp ChatResponse, logger interface{ Error(string, ...any) }) {
	writeJSON(w, chatHTTPStatus(resp), resp, logger)
}

func normalizeChatResponse(resp *ChatResponse) {
	if resp == nil {
		return
	}
	resp.Status = strings.ToUpper(strings.TrimSpace(resp.Status))
	if resp.Status == "" {
		if strings.TrimSpace(resp.Error) != "" {
			resp.Status = "FAILED"
		} else if strings.TrimSpace(resp.Answer) != "" {
			resp.Status = "SUCCEEDED"
		} else {
			resp.Status = "IN_PROGRESS"
		}
	}
}

func chatResponseSucceeded(resp ChatResponse) bool {
	return strings.EqualFold(resp.Status, "SUCCEEDED")
}

func chatHTTPStatus(resp ChatResponse) int {
	switch strings.ToUpper(strings.TrimSpace(resp.Status)) {
	case "IN_PROGRESS", "QUEUED", "PENDING":
		return http.StatusAccepted
	case "FAILED":
		return http.StatusBadGateway
	default:
		return http.StatusOK
	}
}

func extractChatTurn(messages []json.RawMessage) (string, []ChatMessage, error) {
	turns := make([]ChatMessage, 0, len(messages))
	for _, raw := range messages {
		msg, ok := parseChatMessage(raw)
		if ok && msg.Content != "" {
			turns = append(turns, msg)
		}
	}
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "user" {
			return turns[i].Content, turns[:i], nil
		}
	}
	return "", nil, errors.New("messages must contain a user text message.")
}

func parseChatMessage(raw json.RawMessage) (ChatMessage, bool) {
	var msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
		Parts   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"parts"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ChatMessage{}, false
	}
	content := strings.TrimSpace(msg.Content)
	if content == "" {
		parts := make([]string, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
				parts = append(parts, strings.TrimSpace(part.Text))
			}
		}
		content = strings.Join(parts, "\n")
	}
	role := strings.TrimSpace(msg.Role)
	if role == "" || content == "" {
		return ChatMessage{}, false
	}
	return ChatMessage{Role: role, Content: content}, true
}

func expandSearchScopes(ctx context.Context, q chatQuerier, orgID int32, scopes []searchScopeBody) ([]string, error) {
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		switch scope.Type {
		case "repo":
			if strings.TrimSpace(scope.Value) == "" {
				return nil, errors.New("repo search scope is missing value.")
			}
			if _, err := q.GetOrgRepoForRead(ctx, orgID, scope.Value); err != nil {
				if errors.Is(err, db.ErrRepoNotFound) {
					return nil, fmt.Errorf("Repository %q not found.", scope.Value)
				}
				return nil, err
			}
			out = append(out, scope.Value)
		case "reposet":
			repos, err := q.ListSearchContextRepoNames(ctx, orgID, scope.Value)
			if err != nil {
				if errors.Is(err, db.ErrSearchContextNotFound) {
					return nil, fmt.Errorf("Repository set %q not found.", scope.Value)
				}
				return nil, err
			}
			out = append(out, repos...)
		case "":
			continue
		default:
			return nil, fmt.Errorf("Unsupported search scope type %q.", scope.Type)
		}
	}
	return cleanUnique(out), nil
}

func appendAssistantMessage(messages []json.RawMessage, resp ChatResponse) json.RawMessage {
	out := make([]json.RawMessage, 0, len(messages)+1)
	out = append(out, messages...)
	out = append(out, newAssistantMessage(resp))
	return marshalRawMessages(out)
}

func newTextMessage(role, text string) json.RawMessage {
	body, _ := json.Marshal(map[string]any{
		"id":    "msg-" + uuid.NewString(),
		"role":  role,
		"parts": []map[string]string{{"type": "text", "text": text}},
	})
	return body
}

func newAssistantMessage(resp ChatResponse) json.RawMessage {
	body := map[string]any{
		"id":    "msg-" + uuid.NewString(),
		"role":  "assistant",
		"parts": []map[string]string{{"type": "text", "text": resp.Answer}},
	}
	metadata := map[string]string{}
	if strings.TrimSpace(resp.SessionID) != "" {
		metadata["sessionId"] = strings.TrimSpace(resp.SessionID)
	}
	if strings.TrimSpace(resp.Status) != "" {
		metadata["status"] = strings.TrimSpace(resp.Status)
	}
	if len(metadata) > 0 {
		body["metadata"] = metadata
	}
	raw, _ := json.Marshal(body)
	return raw
}

func marshalRawMessages(messages []json.RawMessage) json.RawMessage {
	body, _ := json.Marshal(messages)
	return body
}

func decodeRawMessages(raw json.RawMessage) []json.RawMessage {
	var messages []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &messages) != nil {
		return nil
	}
	return messages
}

func chatMessagesContainSession(raw json.RawMessage, sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return false
	}
	for _, msg := range decodeRawMessages(raw) {
		var envelope struct {
			Metadata map[string]string `json:"metadata"`
		}
		if json.Unmarshal(msg, &envelope) == nil && envelope.Metadata["sessionId"] == sessionID {
			return true
		}
	}
	return false
}

func cleanUnique(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

func fallbackChatName(message string) string {
	normalized := strings.Join(strings.Fields(message), " ")
	if len(normalized) > 50 {
		return normalized[:47] + "..."
	}
	if normalized == "" {
		return "Ask Codebase"
	}
	return normalized
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
