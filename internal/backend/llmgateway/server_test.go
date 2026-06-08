package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"codeintel/pkg/asynqueues"
	"codeintel/pkg/llmproxy"

	"github.com/hibiken/asynq"
)

func TestServerPostPersistsAndEnqueuesDurableCompletion(t *testing.T) {
	store := &fakeRequestStore{}
	enq := &fakeEnqueuer{}
	server := httptest.NewServer(NewServer(Config{
		Store:    store,
		Enqueuer: enq,
		Token:    "backend-token",
		Timeout:  2 * time.Minute,
	}))
	defer server.Close()

	body := llmproxy.ChatRequest{
		RequestID: "ask-123",
		OrgID:     42,
		Provider:  "openai-compatible",
		Model:     llmproxy.LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
		OpenAI:    llmproxy.OpenAICompatibleConfig{Model: "glm-test", BaseURL: "https://example.invalid/v1"},
		Messages:  []llmproxy.ChatMessage{{Role: "user", Content: "question"}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+chatPath, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer backend-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d want 202", resp.StatusCode)
	}
	if store.claimed.RequestID != "ask-123" || store.claimed.OrgID != 42 {
		t.Fatalf("claimed request = %#v", store.claimed)
	}
	if len(enq.tasks) != 1 {
		t.Fatalf("enqueued tasks = %d want 1", len(enq.tasks))
	}
	if enq.tasks[0].Type() != asynqueues.QueueLLMCompletion {
		t.Fatalf("task type = %q want %q", enq.tasks[0].Type(), asynqueues.QueueLLMCompletion)
	}
	payload, err := unmarshalTaskPayload(enq.tasks[0].Payload())
	if err != nil {
		t.Fatal(err)
	}
	if payload.OrgID != 42 || payload.RequestID != "ask-123" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestServerRejectsRequestsWhenBackendTokenUnset(t *testing.T) {
	server := httptest.NewServer(NewServer(Config{
		Store:    &fakeRequestStore{},
		Enqueuer: &fakeEnqueuer{},
	}))
	defer server.Close()

	body := llmproxy.ChatRequest{
		RequestID: "ask-no-token",
		OrgID:     42,
		Provider:  "openai-compatible",
		Model:     llmproxy.LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
		OpenAI:    llmproxy.OpenAICompatibleConfig{Model: "glm-test", BaseURL: "https://example.invalid/v1"},
		Messages:  []llmproxy.ChatMessage{{Role: "user", Content: "question"}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Post(server.URL+chatPath, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401", resp.StatusCode)
	}
}

func TestServerGetRestoresMetadataFromStoredRequest(t *testing.T) {
	request := llmproxy.ChatRequest{
		RequestID: "ask-meta",
		OrgID:     42,
		Provider:  "openai-compatible",
		Model:     llmproxy.LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test", DisplayName: "GLM"},
		OpenAI:    llmproxy.OpenAICompatibleConfig{Model: "glm-test", BaseURL: "https://example.invalid/v1"},
		Messages:  []llmproxy.ChatMessage{{Role: "user", Content: "question"}},
		Budget:    llmproxy.AnswerBudget{Mode: "compact", MaxOutputTokens: 6000, MaxAnswerBytes: 96000},
		Metadata: map[string]any{
			"repos":            []string{"github.com/acme/orders"},
			"scopeFingerprint": "snapshot-a",
			"toolTrace": []map[string]any{{
				"step":     0,
				"toolName": "codegraph_context",
				"output":   "ok",
			}},
		},
	}
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rawResponse, err := json.Marshal(llmproxy.ChatResponse{
		RequestID: "ask-meta",
		Status:    "SUCCEEDED",
		Content:   "answer without old metadata",
		Model:     llmproxy.LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeRequestStore{row: RequestRow{
		ID:       "ask-meta",
		OrgID:    42,
		Status:   "SUCCEEDED",
		Provider: "openai-compatible",
		Model:    "glm-test",
		Request:  rawRequest,
		Response: rawResponse,
	}}
	server := httptest.NewServer(NewServer(Config{
		Store: store,
		Token: "backend-token",
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+chatPath+"/ask-meta?orgId=42", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer backend-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d want 200", resp.StatusCode)
	}
	var got llmproxy.ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Budget.Mode != "compact" {
		t.Fatalf("budget = %#v", got.Budget)
	}
	repos, ok := got.Metadata["repos"].([]any)
	if !ok || len(repos) != 1 || repos[0] != "github.com/acme/orders" {
		t.Fatalf("metadata repos = %#v", got.Metadata["repos"])
	}
	if got.Metadata["scopeFingerprint"] != "snapshot-a" {
		t.Fatalf("scope fingerprint = %#v", got.Metadata["scopeFingerprint"])
	}
}

func TestServerGetRequeuesOrphanedInProgressRequest(t *testing.T) {
	request := llmproxy.ChatRequest{
		RequestID: "ask-orphan",
		OrgID:     42,
		Provider:  "openai-compatible",
		Model:     llmproxy.LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
		OpenAI:    llmproxy.OpenAICompatibleConfig{Model: "glm-test", BaseURL: "https://example.invalid/v1"},
		Messages:  []llmproxy.ChatMessage{{Role: "user", Content: "question"}},
	}
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeRequestStore{row: RequestRow{
		ID:        "ask-orphan",
		OrgID:     42,
		Status:    "IN_PROGRESS",
		Provider:  "openai-compatible",
		Model:     "glm-test",
		Request:   rawRequest,
		Attempts:  0,
		UpdatedAt: time.Now().Add(-orphanedRequestGrace - time.Second),
	}}
	enq := &fakeEnqueuer{}
	server := httptest.NewServer(NewServer(Config{
		Store:    store,
		Enqueuer: enq,
		Token:    "backend-token",
		Timeout:  2 * time.Minute,
	}))
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+chatPath+"/ask-orphan?orgId=42", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer backend-token")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d want 200", resp.StatusCode)
	}
	if len(enq.tasks) != 1 {
		t.Fatalf("enqueued tasks = %d want 1", len(enq.tasks))
	}
	payload, err := unmarshalTaskPayload(enq.tasks[0].Payload())
	if err != nil {
		t.Fatal(err)
	}
	if payload.OrgID != 42 || payload.RequestID != "ask-orphan" {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestLLMTaskIDIsDeterministicPerOrgRequest(t *testing.T) {
	first := llmTaskID(42, "ask-123", time.Time{})
	second := llmTaskID(42, "ask-123", time.Time{})
	if first == "" || first != second {
		t.Fatalf("task id must be deterministic, first=%q second=%q", first, second)
	}
	if first == llmTaskID(43, "ask-123", time.Time{}) || first == llmTaskID(42, "ask-456", time.Time{}) {
		t.Fatalf("task id must still isolate org and request id")
	}
	generation := time.Unix(100, 200)
	versioned := llmTaskID(42, "ask-123", generation)
	if versioned == first || versioned != llmTaskID(42, "ask-123", generation) {
		t.Fatalf("versioned task id should be stable for one claimed generation and distinct from base id: base=%q versioned=%q", first, versioned)
	}
	if versioned == llmTaskID(42, "ask-123", generation.Add(time.Nanosecond)) {
		t.Fatalf("different claimed generations must enqueue distinct worker task ids")
	}
}

func TestProcessorCompletesStoredRequestAndPersistsResponse(t *testing.T) {
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected model call %s %s", r.Method, r.URL.Path)
		}
		writeTestJSON(t, w, http.StatusOK, map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "durable backend answer"}},
			},
		})
	}))
	defer modelServer.Close()

	request := llmproxy.ChatRequest{
		RequestID: "ask-456",
		OrgID:     77,
		Provider:  "openai-compatible",
		Model:     llmproxy.LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
		OpenAI:    llmproxy.OpenAICompatibleConfig{Model: "glm-test", BaseURL: modelServer.URL + "/v1"},
		Messages:  []llmproxy.ChatMessage{{Role: "user", Content: "question"}},
		Metadata: map[string]any{
			"repos":            []string{"github.com/acme/collector"},
			"scopeFingerprint": "snapshot-a",
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeRequestStore{
		row: RequestRow{
			ID:       "ask-456",
			OrgID:    77,
			Status:   "IN_PROGRESS",
			Provider: "openai-compatible",
			Model:    "glm-test",
			Request:  raw,
		},
	}
	payload, err := marshalTaskPayload(TaskPayload{OrgID: 77, RequestID: "ask-456"})
	if err != nil {
		t.Fatal(err)
	}
	processor := NewProcessor(ProcessorConfig{Store: store, HTTP: modelServer.Client(), Timeout: 30 * time.Second})
	if err := processor.Handle(context.Background(), asynq.NewTask(asynqueues.QueueLLMCompletion, payload)); err != nil {
		t.Fatal(err)
	}
	if store.succeeded.RequestID != "ask-456" || store.succeeded.Content != "durable backend answer" {
		t.Fatalf("persisted response = %#v", store.succeeded)
	}
	if store.succeeded.Metadata["scopeFingerprint"] != "snapshot-a" {
		t.Fatalf("persisted response metadata = %#v", store.succeeded.Metadata)
	}
}

func TestProcessorStreamsAndPersistsPartialResponse(t *testing.T) {
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected model call %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != true {
			t.Fatalf("stream flag = %#v, want true", body["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"first \"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"second\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer modelServer.Close()

	request := llmproxy.ChatRequest{
		RequestID: "ask-stream",
		OrgID:     77,
		Provider:  "openai-compatible",
		Model:     llmproxy.LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
		OpenAI:    llmproxy.OpenAICompatibleConfig{Model: "glm-test", BaseURL: modelServer.URL + "/v1", MaxTokens: 4000},
		Messages:  []llmproxy.ChatMessage{{Role: "user", Content: "question"}},
		Stream:    true,
		Budget:    llmproxy.AnswerBudget{Mode: "compact", MaxOutputTokens: 4000, MaxAnswerBytes: 96000},
		Metadata: map[string]any{
			"repos":            []string{"github.com/acme/collector"},
			"scopeFingerprint": "snapshot-stream",
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeRequestStore{
		row: RequestRow{
			ID:       "ask-stream",
			OrgID:    77,
			Status:   "IN_PROGRESS",
			Provider: "openai-compatible",
			Model:    "glm-test",
			Request:  raw,
		},
	}
	payload, err := marshalTaskPayload(TaskPayload{OrgID: 77, RequestID: "ask-stream"})
	if err != nil {
		t.Fatal(err)
	}
	processor := NewProcessor(ProcessorConfig{Store: store, HTTP: modelServer.Client(), Timeout: 30 * time.Second})
	if err := processor.Handle(context.Background(), asynq.NewTask(asynqueues.QueueLLMCompletion, payload)); err != nil {
		t.Fatal(err)
	}
	if store.partial.Content == "" || !store.partial.Partial || store.partial.Status != "IN_PROGRESS" {
		t.Fatalf("partial response not persisted: %#v", store.partial)
	}
	if store.partial.Metadata["scopeFingerprint"] != "snapshot-stream" {
		t.Fatalf("partial response metadata = %#v", store.partial.Metadata)
	}
	if store.succeeded.Content != "first second" || store.succeeded.Budget.Mode != "compact" {
		t.Fatalf("final response = %#v", store.succeeded)
	}
	if store.succeeded.Metadata["scopeFingerprint"] != "snapshot-stream" {
		t.Fatalf("final response metadata = %#v", store.succeeded.Metadata)
	}
}

type fakeRequestStore struct {
	mu        sync.Mutex
	row       RequestRow
	claimed   llmproxy.ChatRequest
	succeeded llmproxy.ChatResponse
	partial   llmproxy.ChatResponse
	failed    string
	retryErr  string
}

func (s *fakeRequestStore) ClaimStarted(_ context.Context, req llmproxy.ChatRequest, _ time.Duration) (RequestRow, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimed = req
	raw, err := json.Marshal(req)
	if err != nil {
		return RequestRow{}, false, err
	}
	row := RequestRow{ID: req.RequestID, OrgID: req.OrgID, Status: "IN_PROGRESS", Provider: req.Provider, Model: req.Model.Model, Request: raw}
	s.row = row
	return row, true, nil
}

func (s *fakeRequestStore) MarkSucceeded(_ context.Context, _ int32, _ string, resp llmproxy.ChatResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.succeeded = resp
	s.row.Status = "SUCCEEDED"
	return nil
}

func (s *fakeRequestStore) MarkPartial(_ context.Context, _ int32, _ string, resp llmproxy.ChatResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = resp
	raw, _ := json.Marshal(resp)
	s.row.Response = raw
	return nil
}

func (s *fakeRequestStore) MarkRetryableError(_ context.Context, _ int32, _ string, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryErr = message
	return nil
}

func (s *fakeRequestStore) MarkFailed(_ context.Context, _ int32, _ string, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed = message
	s.row.Status = "FAILED"
	return nil
}

func (s *fakeRequestStore) Get(_ context.Context, _ int32, _ string) (RequestRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.row, nil
}

type fakeEnqueuer struct {
	tasks []*asynq.Task
	opts  [][]asynq.Option
	err   error
}

func (e *fakeEnqueuer) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	e.tasks = append(e.tasks, task)
	e.opts = append(e.opts, opts)
	return &asynq.TaskInfo{}, e.err
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
