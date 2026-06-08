package llmproxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCompleteChatPollsAcceptedRequest(t *testing.T) {
	var getCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/internal/llm/chat":
			writeTestJSON(t, w, http.StatusAccepted, ChatResponse{
				RequestID: "llm-test",
				Status:    "IN_PROGRESS",
				Model:     LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/internal/llm/chat/llm-test":
			if got := r.URL.Query().Get("orgId"); got != "7" {
				t.Fatalf("orgId query = %q, want 7", got)
			}
			getCalls.Add(1)
			writeTestJSON(t, w, http.StatusOK, ChatResponse{
				RequestID: "llm-test",
				Status:    "SUCCEEDED",
				Content:   "durable answer",
				Model:     LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
			})
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, server.Client(), "")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.CompleteChat(context.Background(), ChatRequest{
		RequestID: "llm-test",
		OrgID:     7,
		Provider:  "openai-compatible",
		Model:     LanguageModelInfo{Provider: "openai-compatible", Model: "glm-test"},
		OpenAI:    OpenAICompatibleConfig{Model: "glm-test", BaseURL: "http://unused"},
		Messages:  []ChatMessage{{Role: "user", Content: "question"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != "SUCCEEDED" || resp.Content != "durable answer" {
		t.Fatalf("response = %#v", resp)
	}
	if getCalls.Load() != 1 {
		t.Fatalf("GET calls = %d, want 1", getCalls.Load())
	}
}

func TestStreamOpenAICompatibleChatCollectsDeltasAndSendsBudget(t *testing.T) {
	var sawStream atomic.Bool
	var sawMaxTokens atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] == true {
			sawStream.Store(true)
		}
		if body["max_tokens"].(float64) == 1234 {
			sawMaxTokens.Store(true)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var partial string
	completion, err := StreamOpenAICompatibleChat(context.Background(), server.Client(), OpenAICompatibleConfig{
		Model:     "glm-test",
		BaseURL:   server.URL + "/v1",
		MaxTokens: 1234,
	}, []ChatMessage{{Role: "user", Content: "question"}}, nil, func(delta StreamDelta) error {
		partial += delta.Content
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if completion.Content != "hello world" || partial != "hello world" {
		t.Fatalf("completion=%q partial=%q", completion.Content, partial)
	}
	if !sawStream.Load() || !sawMaxTokens.Load() {
		t.Fatalf("stream=%v maxTokens=%v", sawStream.Load(), sawMaxTokens.Load())
	}
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
