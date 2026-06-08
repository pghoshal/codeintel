package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codeintel/internal/auth"
	"codeintel/internal/db"
)

type chatSpy struct {
	fakeAuthQuerier
	chat           db.ChatRow
	repoNames      map[string]bool
	reposet        []string
	created        db.CreateChatParams
	updatedChatID  string
	updatedPayload json.RawMessage
	updatedName    string
}

func (c *chatSpy) CreateChat(ctx context.Context, p db.CreateChatParams) (db.ChatRow, error) {
	c.created = p
	row := db.ChatRow{
		ID:          p.ID,
		CreatedByID: p.CreatedByID,
		OrgID:       p.OrgID,
		Visibility:  p.Visibility,
		Messages:    p.Messages,
		CreatedAt:   time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
		UpdatedAt:   time.Date(2026, 5, 25, 0, 0, 0, 0, time.UTC),
	}
	c.chat = row
	return row, nil
}

func (c *chatSpy) GetChatForOrg(ctx context.Context, orgID int32, chatID string) (db.ChatRow, error) {
	if c.chat.ID == chatID && c.chat.OrgID == orgID {
		return c.chat, nil
	}
	return db.ChatRow{}, db.ErrChatNotFound
}

func (c *chatSpy) UpdateChatMessages(ctx context.Context, orgID int32, chatID string, messages json.RawMessage) error {
	c.updatedChatID = chatID
	c.updatedPayload = messages
	return nil
}

func (c *chatSpy) UpdateChatName(ctx context.Context, orgID int32, chatID string, name string) error {
	c.updatedName = name
	return nil
}

func (c *chatSpy) ListSearchContextRepoNames(ctx context.Context, orgID int32, name string) ([]string, error) {
	if name != "backend" {
		return nil, db.ErrSearchContextNotFound
	}
	return c.reposet, nil
}

func (c *chatSpy) GetOrgRepoForRead(ctx context.Context, orgID int32, repoName string) (db.RepoReadRow, error) {
	if c.repoNames[repoName] {
		return db.RepoReadRow{ID: 1, OrgID: orgID, Name: repoName}, nil
	}
	return db.RepoReadRow{}, db.ErrRepoNotFound
}

type chatBackendSpy struct {
	last    ChatRequest
	lastGet ChatResultRequest
	resp    ChatResponse
	getResp ChatResponse
	err     error
}

func (c *chatBackendSpy) Ask(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	c.last = req
	if c.err != nil {
		return ChatResponse{}, c.err
	}
	resp := c.resp
	resp.ChatID = req.ChatID
	return resp, nil
}

func (c *chatBackendSpy) Get(ctx context.Context, req ChatResultRequest) (ChatResponse, error) {
	c.lastGet = req
	if c.err != nil {
		return ChatResponse{}, c.err
	}
	resp := c.getResp
	if resp.Answer == "" && resp.Status == "" {
		resp = c.resp
	}
	resp.ChatID = req.ChatID
	resp.SessionID = req.SessionID
	return resp, nil
}

func newChatTestServer(q *chatSpy, backend ChatBackend) *Server {
	return NewServer(Config{
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Queries:       q,
		EncryptionKey: "0123456789abcdef0123456789abcdef",
		ChatBackend:   backend,
	})
}

func chatReq(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	const secret = "ownersec"
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(auth.ApiKeyHeader, auth.ApiKeyPrefix+secret)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestBlockingChatCreatesPersistsAndCallsSharedBackend(t *testing.T) {
	_, hash := ownerHash(t)
	q := &chatSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		repoNames:       map[string]bool{"github.com/acme/api": true},
	}
	backend := &chatBackendSpy{resp: ChatResponse{
		Answer:        "<!--answer-->\n**Answer / Decision**\nUse the api flow.",
		LanguageModel: ChatLanguageModel{Provider: "openai-compatible", Model: "glm-4.6"},
		SessionID:     "mcp-session",
		ToolTrace:     []string{"grep"},
	}}
	srv := newChatTestServer(q, backend)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, chatReq(t, http.MethodPost, "/api/chat/blocking", `{
		"query":"Trace order flow",
		"repos":["github.com/acme/api"],
		"languageModel":{"provider":"openai-compatible","model":"glm-4.6"},
		"visibility":"PRIVATE"
	}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if q.created.OrgID != 7 || q.created.CreatedByID == nil || *q.created.CreatedByID != "u-1" || q.created.Visibility != "PRIVATE" {
		t.Fatalf("chat create wrong: %+v", q.created)
	}
	if backend.last.Query != "Trace order flow" || len(backend.last.Repos) != 1 || backend.last.Repos[0] != "github.com/acme/api" {
		t.Fatalf("backend request wrong: %+v", backend.last)
	}
	if q.updatedChatID == "" || !strings.Contains(string(q.updatedPayload), "Use the api flow") {
		t.Fatalf("messages not persisted: chat=%q payload=%s", q.updatedChatID, q.updatedPayload)
	}
	if q.updatedName != "Trace order flow" {
		t.Fatalf("fallback name=%q", q.updatedName)
	}
	if !strings.Contains(rec.Body.String(), `"sessionId":"mcp-session"`) {
		t.Fatalf("response missing session/tool data: %s", rec.Body.String())
	}
}

func TestChatExistingSessionExpandsRepoSetAndPersistsAssistant(t *testing.T) {
	_, hash := ownerHash(t)
	owner := "u-1"
	q := &chatSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		chat: db.ChatRow{
			ID:          "chat-1",
			OrgID:       7,
			CreatedByID: &owner,
			Visibility:  "PRIVATE",
			Messages:    json.RawMessage(`[]`),
		},
		repoNames: map[string]bool{"github.com/acme/api": true, "github.com/acme/worker": true},
		reposet:   []string{"github.com/acme/api", "github.com/acme/worker"},
	}
	backend := &chatBackendSpy{resp: ChatResponse{
		Answer:        "<!--answer-->\nImplementation map.",
		LanguageModel: ChatLanguageModel{Provider: "openai-compatible", Model: "glm-4.6"},
	}}
	srv := newChatTestServer(q, backend)
	body := `{
		"id":"chat-1",
		"messages":[
			{"id":"m1","role":"user","parts":[{"type":"text","text":"Earlier context"}]},
			{"id":"m2","role":"assistant","parts":[{"type":"text","text":"Earlier answer"}]},
			{"id":"m3","role":"user","parts":[{"type":"text","text":"Compare branches"}]}
		],
		"selectedSearchScopes":[{"type":"reposet","value":"backend"}],
		"languageModel":{"provider":"openai-compatible","model":"glm-4.6"}
	}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, chatReq(t, http.MethodPost, "/api/chat", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backend.last.Query != "Compare branches" || len(backend.last.PriorMessages) != 2 {
		t.Fatalf("chat turn not extracted: %+v", backend.last)
	}
	if strings.Join(backend.last.Repos, ",") != "github.com/acme/api,github.com/acme/worker" {
		t.Fatalf("reposet not expanded: %+v", backend.last.Repos)
	}
	if !strings.Contains(string(q.updatedPayload), "Implementation map.") {
		t.Fatalf("assistant response not persisted: %s", q.updatedPayload)
	}
}

func TestChatExistingSessionQueryOnlyLoadsPersistedContext(t *testing.T) {
	_, hash := ownerHash(t)
	owner := "u-1"
	q := &chatSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		chat: db.ChatRow{
			ID:          "chat-query-only",
			OrgID:       7,
			CreatedByID: &owner,
			Visibility:  "PRIVATE",
			Messages: json.RawMessage(`[
				{"id":"m1","role":"user","parts":[{"type":"text","text":"Trace tenant marker first."}]},
				{"id":"m2","role":"assistant","parts":[{"type":"text","text":"The marker is in src/tenant.ts."}]}
			]`),
		},
		repoNames: map[string]bool{"github.com/acme/api": true},
	}
	backend := &chatBackendSpy{resp: ChatResponse{
		Answer:        "<!--answer-->\nThe follow-up still uses src/tenant.ts.",
		LanguageModel: ChatLanguageModel{Provider: "openai-compatible", Model: "glm-4.6"},
	}}
	srv := newChatTestServer(q, backend)
	body := `{
		"id":"chat-query-only",
		"query":"Which file should I modify next?",
		"selectedSearchScopes":[{"type":"repo","value":"github.com/acme/api"}],
		"languageModel":{"provider":"openai-compatible","model":"glm-4.6"}
	}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, chatReq(t, http.MethodPost, "/api/chat", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backend.last.Query != "Which file should I modify next?" {
		t.Fatalf("query-only turn not used: %+v", backend.last)
	}
	if len(backend.last.PriorMessages) != 2 || backend.last.PriorMessages[1].Content != "The marker is in src/tenant.ts." {
		t.Fatalf("persisted prior context not loaded: %+v", backend.last.PriorMessages)
	}
	persisted := string(q.updatedPayload)
	for _, want := range []string{"Trace tenant marker first.", "Which file should I modify next?", "The follow-up still uses src/tenant.ts."} {
		if !strings.Contains(persisted, want) {
			t.Fatalf("persisted messages missing %q: %s", want, persisted)
		}
	}
}

func TestChatExistingSessionReturnsAcceptedForDurableAsyncWithoutFakeAssistant(t *testing.T) {
	_, hash := ownerHash(t)
	owner := "u-1"
	q := &chatSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		chat: db.ChatRow{
			ID:          "chat-async",
			OrgID:       7,
			CreatedByID: &owner,
			Visibility:  "PRIVATE",
			Messages:    json.RawMessage(`[]`),
		},
		repoNames: map[string]bool{"github.com/acme/api": true},
	}
	backend := &chatBackendSpy{resp: ChatResponse{
		Answer:        "ask_codebase synthesis is IN_PROGRESS.",
		Status:        "IN_PROGRESS",
		SessionID:     "mcp-async",
		LanguageModel: ChatLanguageModel{Provider: "openai-compatible", Model: "glm-4.6"},
	}}
	srv := newChatTestServer(q, backend)
	body := `{
		"id":"chat-async",
		"messages":[{"id":"m1","role":"user","parts":[{"type":"text","text":"Trace async flow"}]}],
		"selectedSearchScopes":[{"type":"repo","value":"github.com/acme/api"}],
		"languageModel":{"provider":"openai-compatible","model":"glm-4.6"}
	}`
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, chatReq(t, http.MethodPost, "/api/chat", body))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !backend.last.Async {
		t.Fatalf("backend request should be async: %+v", backend.last)
	}
	if strings.Contains(string(q.updatedPayload), "ask_codebase synthesis is IN_PROGRESS") {
		t.Fatalf("pending assistant placeholder should not be persisted: %s", q.updatedPayload)
	}
	if !strings.Contains(string(q.updatedPayload), "Trace async flow") {
		t.Fatalf("user message should still be persisted: %s", q.updatedPayload)
	}
	if !strings.Contains(rec.Body.String(), `"status":"IN_PROGRESS"`) || !strings.Contains(rec.Body.String(), `"sessionId":"mcp-async"`) {
		t.Fatalf("response missing durable state: %s", rec.Body.String())
	}
}

func TestChatResultPersistsFinalAnswerOnce(t *testing.T) {
	_, hash := ownerHash(t)
	owner := "u-1"
	q := &chatSpy{
		fakeAuthQuerier: fakeAuthQuerier{lookupResult: validOwnerLookup(hash)},
		chat: db.ChatRow{
			ID:          "chat-result",
			OrgID:       7,
			CreatedByID: &owner,
			Visibility:  "PRIVATE",
			Messages:    json.RawMessage(`[{"id":"m1","role":"user","parts":[{"type":"text","text":"Trace final"}]}]`),
		},
		repoNames: map[string]bool{"github.com/acme/api": true},
	}
	backend := &chatBackendSpy{getResp: ChatResponse{
		Answer:        "<!--answer-->\nFinal architecture answer.",
		Status:        "SUCCEEDED",
		LanguageModel: ChatLanguageModel{Provider: "openai-compatible", Model: "glm-4.6"},
	}}
	srv := newChatTestServer(q, backend)

	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, chatReq(t, http.MethodGet, "/api/chat/chat-result/result?sessionId=mcp-final", ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if backend.lastGet.SessionID != "mcp-final" || backend.lastGet.ChatID != "chat-result" {
		t.Fatalf("get request wrong: %+v", backend.lastGet)
	}
	if !strings.Contains(string(q.updatedPayload), "Final architecture answer.") || !strings.Contains(string(q.updatedPayload), `"sessionId":"mcp-final"`) {
		t.Fatalf("final assistant not persisted with session metadata: %s", q.updatedPayload)
	}

	q.chat.Messages = q.updatedPayload
	q.updatedPayload = nil
	rec = httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, chatReq(t, http.MethodGet, "/api/chat/chat-result/result?sessionId=mcp-final", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", rec.Code, rec.Body.String())
	}
	if q.updatedPayload != nil {
		t.Fatalf("final answer should not be appended twice: %s", q.updatedPayload)
	}
}
