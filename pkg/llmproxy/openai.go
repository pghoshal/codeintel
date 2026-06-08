package llmproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Completion struct {
	Content   string
	ToolCalls []ToolCall
}

type StreamDelta struct {
	Content string
}

type RequestError struct {
	Err        error
	StatusCode int
	Retryable  bool
}

func (e *RequestError) Error() string {
	if e == nil || e.Err == nil {
		return "language model request failed"
	}
	return e.Err.Error()
}

func (e *RequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func CompleteOpenAICompatibleChatWithRetry(ctx context.Context, client *http.Client, cfg OpenAICompatibleConfig, messages []ChatMessage, tools []ToolInfo, attempts int) (Completion, error) {
	if attempts <= 0 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		completion, err := CompleteOpenAICompatibleChat(ctx, client, cfg, messages, tools)
		if err == nil {
			return completion, nil
		}
		last = err
		var reqErr *RequestError
		if !errors.As(err, &reqErr) || !reqErr.Retryable || attempt == attempts {
			return Completion{}, err
		}
		delay := time.Duration(attempt*250) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Completion{}, ctx.Err()
		case <-timer.C:
		}
	}
	return Completion{}, last
}

func StreamOpenAICompatibleChatWithRetry(ctx context.Context, client *http.Client, cfg OpenAICompatibleConfig, messages []ChatMessage, tools []ToolInfo, attempts int, onDelta func(StreamDelta) error) (Completion, error) {
	if attempts <= 0 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		completion, err := StreamOpenAICompatibleChat(ctx, client, cfg, messages, tools, onDelta)
		if err == nil {
			return completion, nil
		}
		last = err
		var reqErr *RequestError
		if !errors.As(err, &reqErr) || !reqErr.Retryable || attempt == attempts {
			return Completion{}, err
		}
		delay := time.Duration(attempt*250) * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Completion{}, ctx.Err()
		case <-timer.C:
		}
	}
	return Completion{}, last
}

func CompleteOpenAICompatibleChat(ctx context.Context, client *http.Client, cfg OpenAICompatibleConfig, messages []ChatMessage, tools []ToolInfo) (Completion, error) {
	if client == nil {
		client = http.DefaultClient
	}
	endpoint, err := ChatCompletionEndpoint(cfg)
	if err != nil {
		return Completion{}, err
	}
	body := map[string]any{
		"model":       cfg.Model,
		"messages":    messages,
		"temperature": 0,
	}
	if cfg.MaxTokens > 0 {
		body["max_tokens"] = cfg.MaxTokens
	}
	if len(tools) > 0 {
		body["tools"] = OpenAITools(tools)
		body["tool_choice"] = "auto"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Completion{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Completion{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	for key, value := range cfg.Headers {
		httpReq.Header.Set(key, value)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Completion{}, &RequestError{Err: fmt.Errorf("language model request failed: %w", err), Retryable: true}
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return Completion{}, fmt.Errorf("read language model response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Completion{}, &RequestError{
			Err:        fmt.Errorf("language model request failed with status %d: %s", resp.StatusCode, truncateForError(string(respBody), 1000)),
			StatusCode: resp.StatusCode,
			Retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
		}
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return Completion{}, fmt.Errorf("decode language model response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return Completion{}, fmt.Errorf("language model returned no choices")
	}
	return Completion{
		Content:   decoded.Choices[0].Message.Content,
		ToolCalls: decoded.Choices[0].Message.ToolCalls,
	}, nil
}

func StreamOpenAICompatibleChat(ctx context.Context, client *http.Client, cfg OpenAICompatibleConfig, messages []ChatMessage, tools []ToolInfo, onDelta func(StreamDelta) error) (Completion, error) {
	if client == nil {
		client = http.DefaultClient
	}
	endpoint, err := ChatCompletionEndpoint(cfg)
	if err != nil {
		return Completion{}, err
	}
	body := map[string]any{
		"model":       cfg.Model,
		"messages":    messages,
		"temperature": 0,
		"stream":      true,
	}
	if cfg.MaxTokens > 0 {
		body["max_tokens"] = cfg.MaxTokens
	}
	if len(tools) > 0 {
		body["tools"] = OpenAITools(tools)
		body["tool_choice"] = "auto"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Completion{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Completion{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	for key, value := range cfg.Headers {
		httpReq.Header.Set(key, value)
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return Completion{}, &RequestError{Err: fmt.Errorf("language model stream failed: %w", err), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		if readErr != nil {
			return Completion{}, fmt.Errorf("read language model stream error: %w", readErr)
		}
		return Completion{}, &RequestError{
			Err:        fmt.Errorf("language model stream failed with status %d: %s", resp.StatusCode, truncateForError(string(respBody), 1000)),
			StatusCode: resp.StatusCode,
			Retryable:  resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
		}
	}
	var content strings.Builder
	scanner := bufio.NewScanner(io.LimitReader(resp.Body, 16<<20))
	scanner.Buffer(make([]byte, 0, 64*1024), 2<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return Completion{Content: content.String()}, nil
		}
		delta, err := decodeOpenAIStreamDelta(data)
		if err != nil {
			return Completion{}, err
		}
		if delta.Content == "" {
			continue
		}
		content.WriteString(delta.Content)
		if onDelta != nil {
			if err := onDelta(delta); err != nil {
				return Completion{}, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Completion{}, &RequestError{Err: fmt.Errorf("read language model stream: %w", err), Retryable: true}
	}
	if content.Len() == 0 {
		return Completion{}, fmt.Errorf("language model stream returned no content")
	}
	return Completion{Content: content.String()}, nil
}

func decodeOpenAIStreamDelta(data string) (StreamDelta, error) {
	var decoded struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		return StreamDelta{}, fmt.Errorf("decode language model stream delta: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return StreamDelta{}, nil
	}
	return StreamDelta{Content: decoded.Choices[0].Delta.Content}, nil
}

func ChatCompletionEndpoint(cfg OpenAICompatibleConfig) (string, error) {
	parsed, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return "", fmt.Errorf("invalid model baseUrl: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("model baseUrl must be an absolute URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/chat/completions") {
		path += "/chat/completions"
	}
	parsed.Path = path
	q := parsed.Query()
	for key, value := range cfg.QueryParams {
		q.Set(key, value)
	}
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}

func OpenAITools(tools []ToolInfo) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.InputSchema,
			},
		})
	}
	return out
}

func truncateForError(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "... (truncated)"
}
