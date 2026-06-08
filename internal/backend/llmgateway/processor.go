package llmgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"codeintel/pkg/llmproxy"

	"github.com/hibiken/asynq"
)

const (
	partialFlushBytes    = 2048
	partialFlushInterval = 2 * time.Second
)

type ProcessorConfig struct {
	Store   RequestStore
	HTTP    *http.Client
	Logger  *slog.Logger
	Timeout time.Duration
}

type Processor struct {
	cfg    ProcessorConfig
	logger *slog.Logger
}

func NewProcessor(cfg ProcessorConfig) *Processor {
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
	return &Processor{cfg: cfg, logger: logger.With("component", "llm-completion-worker")}
}

func (p *Processor) Handle(ctx context.Context, task *asynq.Task) error {
	payload, err := unmarshalTaskPayload(task.Payload())
	if err != nil {
		return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
	}
	if p.cfg.Store == nil {
		return fmt.Errorf("durable llm request store is not configured")
	}
	row, err := p.cfg.Store.Get(ctx, payload.OrgID, payload.RequestID)
	if err != nil {
		return fmt.Errorf("read llm request state: %w", err)
	}
	if row.Status == "SUCCEEDED" {
		return nil
	}
	if row.Status != "IN_PROGRESS" {
		return fmt.Errorf("llm request %s is %s: %w", row.ID, row.Status, asynq.SkipRetry)
	}
	var req llmproxy.ChatRequest
	if err := json.Unmarshal(row.Request, &req); err != nil {
		_ = p.cfg.Store.MarkFailed(context.Background(), payload.OrgID, payload.RequestID, "stored llm request payload is invalid")
		return fmt.Errorf("decode stored llm request: %w: %w", err, asynq.SkipRetry)
	}
	if err := validateChatRequest(req); err != nil {
		_ = p.cfg.Store.MarkFailed(context.Background(), payload.OrgID, payload.RequestID, err.Error())
		return fmt.Errorf("stored llm request invalid: %w: %w", err, asynq.SkipRetry)
	}
	started := time.Now()
	workCtx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	completion, err := p.complete(workCtx, req, started)
	duration := time.Since(started).Milliseconds()
	if err != nil {
		message := err.Error()
		if shouldMarkFinalFailure(ctx) {
			_ = p.cfg.Store.MarkFailed(context.Background(), req.OrgID, req.RequestID, message)
			p.logger.Warn("llm request failed permanently", "requestId", req.RequestID, "orgId", req.OrgID, "model", req.Model.Model, "duration_ms", duration, "err", message)
		} else {
			_ = p.cfg.Store.MarkRetryableError(context.Background(), req.OrgID, req.RequestID, message)
			p.logger.Warn("llm request failed; retry scheduled", "requestId", req.RequestID, "orgId", req.OrgID, "model", req.Model.Model, "duration_ms", duration, "err", message)
		}
		return err
	}
	resp := llmproxy.ChatResponse{
		RequestID:  req.RequestID,
		Status:     "SUCCEEDED",
		Content:    completion.Content,
		ToolCalls:  completion.ToolCalls,
		DurationMs: duration,
		Model:      req.Model,
		Budget:     req.Budget,
		Metadata:   req.Metadata,
	}
	if err := p.cfg.Store.MarkSucceeded(context.Background(), req.OrgID, req.RequestID, resp); err != nil {
		if errors.Is(err, context.Canceled) {
			return err
		}
		return fmt.Errorf("mark llm request succeeded: %w", err)
	}
	p.logger.Info("llm request completed", "requestId", req.RequestID, "orgId", req.OrgID, "model", req.Model.Model, "duration_ms", duration)
	return nil
}

func (p *Processor) complete(ctx context.Context, req llmproxy.ChatRequest, started time.Time) (llmproxy.Completion, error) {
	if !req.Stream {
		return llmproxy.CompleteOpenAICompatibleChatWithRetry(ctx, p.cfg.HTTP, req.OpenAI, req.Messages, req.Tools, req.MaxAttempts)
	}
	var content string
	lastFlush := time.Now()
	lastBytes := 0
	flush := func(force bool) error {
		if content == "" {
			return nil
		}
		now := time.Now()
		if !force && len(content)-lastBytes < partialFlushBytes && now.Sub(lastFlush) < partialFlushInterval {
			return nil
		}
		resp := llmproxy.ChatResponse{
			RequestID:  req.RequestID,
			Status:     "IN_PROGRESS",
			Content:    content,
			DurationMs: time.Since(started).Milliseconds(),
			Model:      req.Model,
			Partial:    true,
			UpdatedAt:  now.UTC().Format(time.RFC3339Nano),
			Budget:     req.Budget,
			Metadata:   req.Metadata,
		}
		if err := p.cfg.Store.MarkPartial(context.Background(), req.OrgID, req.RequestID, resp); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		lastFlush = now
		lastBytes = len(content)
		return nil
	}
	completion, err := llmproxy.StreamOpenAICompatibleChatWithRetry(ctx, p.cfg.HTTP, req.OpenAI, req.Messages, req.Tools, req.MaxAttempts, func(delta llmproxy.StreamDelta) error {
		content += delta.Content
		return flush(false)
	})
	if err != nil {
		_ = flush(true)
		return llmproxy.Completion{}, err
	}
	content = completion.Content
	if err := flush(true); err != nil {
		return llmproxy.Completion{}, err
	}
	return completion, nil
}

func shouldMarkFinalFailure(ctx context.Context) bool {
	retryCount, retryOK := asynq.GetRetryCount(ctx)
	maxRetry, maxOK := asynq.GetMaxRetry(ctx)
	if !retryOK || !maxOK {
		return true
	}
	return retryCount >= maxRetry
}
