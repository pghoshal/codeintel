package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewClient(baseURL string, httpClient *http.Client, token string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("llm gateway url is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("llm gateway url must be an absolute URL")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{baseURL: baseURL, token: token, http: httpClient}, nil
}

func (c *Client) CompleteChat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	decoded, statusCode, err := c.startChat(ctx, req)
	if err != nil {
		if req.RequestID != "" {
			if recovered, pollErr := c.pollChat(ctx, req.OrgID, req.RequestID, req.Model); pollErr == nil {
				return recovered, nil
			}
		}
		return ChatResponse{}, err
	}
	if statusCode == http.StatusAccepted || decoded.Status == "IN_PROGRESS" {
		return c.pollChat(ctx, req.OrgID, decoded.RequestID, req.Model)
	}
	if statusCode < 200 || statusCode >= 300 {
		if decoded.Error != "" {
			return ChatResponse{}, fmt.Errorf("llm gateway failed: %s", decoded.Error)
		}
		return ChatResponse{}, fmt.Errorf("llm gateway failed with status %d", statusCode)
	}
	return decoded, nil
}

func (c *Client) StartChat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	decoded, statusCode, err := c.startChat(ctx, req)
	if err != nil {
		return ChatResponse{}, err
	}
	if statusCode < 200 || statusCode >= 300 {
		if decoded.Error != "" {
			return ChatResponse{}, fmt.Errorf("llm gateway failed: %s", decoded.Error)
		}
		return ChatResponse{}, fmt.Errorf("llm gateway failed with status %d", statusCode)
	}
	return decoded, nil
}

func (c *Client) startChat(ctx context.Context, req ChatRequest) (ChatResponse, int, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, 0, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/internal/llm/chat", bytes.NewReader(payload))
	if err != nil {
		return ChatResponse{}, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, 0, fmt.Errorf("llm gateway request failed: %w", err)
	}
	defer resp.Body.Close()
	decoded, err := decodeChatResponse(resp)
	if err != nil {
		return ChatResponse{}, resp.StatusCode, err
	}
	return decoded, resp.StatusCode, nil
}

func (c *Client) GetChat(ctx context.Context, orgID int32, requestID string) (ChatResponse, error) {
	if strings.TrimSpace(requestID) == "" {
		return ChatResponse{}, fmt.Errorf("llm request id is required")
	}
	u := fmt.Sprintf("%s/internal/llm/chat/%s?orgId=%d", c.baseURL, url.PathEscape(requestID), orgID)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return ChatResponse{}, err
	}
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("llm gateway status request failed: %w", err)
	}
	defer resp.Body.Close()
	decoded, err := decodeChatResponse(resp)
	if err != nil {
		return ChatResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if decoded.Error != "" {
			return ChatResponse{}, fmt.Errorf("llm gateway status failed: %s", decoded.Error)
		}
		return ChatResponse{}, fmt.Errorf("llm gateway status failed with status %d", resp.StatusCode)
	}
	return decoded, nil
}

func (c *Client) pollChat(ctx context.Context, orgID int32, requestID string, model LanguageModelInfo) (ChatResponse, error) {
	if strings.TrimSpace(requestID) == "" {
		return ChatResponse{}, fmt.Errorf("llm request id is required for polling")
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ChatResponse{}, ctx.Err()
		case <-timer.C:
			resp, err := c.GetChat(ctx, orgID, requestID)
			if err != nil {
				return ChatResponse{}, err
			}
			if resp.Status == "SUCCEEDED" {
				return resp, nil
			}
			if resp.Status == "FAILED" {
				if resp.Model.Model == "" {
					resp.Model = model
				}
				if resp.Error == "" {
					resp.Error = "llm request failed"
				}
				return ChatResponse{}, fmt.Errorf("llm gateway failed: %s", resp.Error)
			}
			timer.Reset(2 * time.Second)
		}
	}
}

func decodeChatResponse(resp *http.Response) (ChatResponse, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("read llm gateway response: %w", err)
	}
	var decoded ChatResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ChatResponse{}, fmt.Errorf("decode llm gateway response: %w", err)
	}
	return decoded, nil
}
