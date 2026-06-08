package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"codeintel/pkg/schedulerapi"
)

var activeJobMessagePattern = regexp.MustCompile(`^Repo already has active ([A-Z_]+) job ([^.]+)\.$`)

type BackendSchedulerClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func NewBackendSchedulerClient(baseURL string, httpClient *http.Client, token string) (*BackendSchedulerClient, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("backend scheduler url is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("backend scheduler url must be an absolute URL")
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &BackendSchedulerClient{baseURL: baseURL, token: token, http: httpClient}, nil
}

func (c *BackendSchedulerClient) Schedule(ctx context.Context, req RepoIndexRequest) (RepoIndexResult, error) {
	payload := schedulerapi.RepoIndexRequest{
		OrgID:  req.OrgID,
		RepoID: req.RepoID,
		Kind:   string(req.Kind),
		Ref:    req.Ref,
	}
	var resp schedulerapi.RepoIndexResponse
	if err := c.post(ctx, "/internal/scheduler/repo-index", payload, &resp); err != nil {
		return RepoIndexResult{}, mapSchedulerError(err)
	}
	return RepoIndexResult{JobID: resp.JobID, AlreadyAtCapacity: resp.AlreadyAtCapacity}, nil
}

func (c *BackendSchedulerClient) ScheduleConnectionSync(ctx context.Context, req SyncRequest) (SyncResult, error) {
	payload := schedulerapi.ConnectionSyncRequest{OrgID: req.OrgID, ConnectionID: req.ConnectionID}
	var resp schedulerapi.ConnectionSyncResponse
	if err := c.post(ctx, "/internal/scheduler/connection-sync", payload, &resp); err != nil {
		return SyncResult{}, err
	}
	return SyncResult{JobID: resp.JobID, AlreadyAtCapacity: resp.AlreadyAtCapacity}, nil
}

func (c *BackendSchedulerClient) post(ctx context.Context, path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("backend scheduler request failed: %w", err)
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return fmt.Errorf("read backend scheduler response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var decoded schedulerapi.ErrorResponse
		_ = json.Unmarshal(data, &decoded)
		return schedulerHTTPError{status: resp.StatusCode, message: strings.TrimSpace(decoded.Error)}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode backend scheduler response: %w", err)
	}
	return nil
}

type schedulerHTTPError struct {
	status  int
	message string
}

func (e schedulerHTTPError) Error() string {
	if e.message != "" {
		return e.message
	}
	return fmt.Sprintf("backend scheduler failed with status %d", e.status)
}

func mapSchedulerError(err error) error {
	var httpErr schedulerHTTPError
	if !errors.As(err, &httpErr) {
		return err
	}
	if httpErr.status == http.StatusNotFound {
		return ErrRepoNotFound
	}
	if httpErr.status == http.StatusConflict {
		return parseActiveJobError(httpErr.message)
	}
	if httpErr.status == http.StatusServiceUnavailable {
		return ErrRepoIndexerUnavailable
	}
	return err
}

func parseActiveJobError(message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		return &JobAlreadyActiveError{}
	}
	matches := activeJobMessagePattern.FindStringSubmatch(message)
	if len(matches) == 3 {
		return &JobAlreadyActiveError{JobID: matches[2], Type: matches[1], Status: "IN_PROGRESS"}
	}
	return &JobAlreadyActiveError{JobID: message, Type: "INDEX", Status: "IN_PROGRESS"}
}

type backendConnectionSyncer struct {
	client *BackendSchedulerClient
}

func NewBackendConnectionSyncer(client *BackendSchedulerClient) ConnectionSyncer {
	return backendConnectionSyncer{client: client}
}

func (s backendConnectionSyncer) Schedule(ctx context.Context, req SyncRequest) (SyncResult, error) {
	if s.client == nil {
		return SyncResult{}, ErrRepoIndexerUnavailable
	}
	return s.client.ScheduleConnectionSync(ctx, req)
}
