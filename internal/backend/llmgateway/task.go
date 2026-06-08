package llmgateway

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"codeintel/pkg/asynqueues"

	"github.com/hibiken/asynq"
)

const defaultTaskMaxRetry = 3

type Enqueuer interface {
	Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

type TaskPayload struct {
	OrgID     int32  `json:"orgId"`
	RequestID string `json:"requestId"`
}

func enqueueChatCompletion(enq Enqueuer, orgID int32, requestID string, generation time.Time, timeout time.Duration, maxRetry int) error {
	if enq == nil {
		return errors.New("llm completion queue client is not configured")
	}
	payload, err := marshalTaskPayload(TaskPayload{OrgID: orgID, RequestID: requestID})
	if err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	if maxRetry <= 0 {
		maxRetry = defaultTaskMaxRetry
	}
	taskID := llmTaskID(orgID, requestID, generation)
	task := asynq.NewTask(asynqueues.QueueLLMCompletion, payload)
	_, err = enq.Enqueue(task,
		asynq.Queue(asynqueues.QueueLLMCompletion),
		asynq.TaskID(taskID),
		asynq.MaxRetry(maxRetry),
		asynq.Timeout(timeout+staleRequestGrace),
	)
	if err != nil {
		if errors.Is(err, asynq.ErrTaskIDConflict) || errors.Is(err, asynq.ErrDuplicateTask) {
			return nil
		}
		return fmt.Errorf("enqueue llm completion: %w", err)
	}
	return nil
}

func marshalTaskPayload(payload TaskPayload) ([]byte, error) {
	if payload.OrgID <= 0 {
		return nil, errors.New("llm task orgId is required")
	}
	if strings.TrimSpace(payload.RequestID) == "" {
		return nil, errors.New("llm task requestId is required")
	}
	return json.Marshal(payload)
}

func unmarshalTaskPayload(raw []byte) (TaskPayload, error) {
	var payload TaskPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return TaskPayload{}, fmt.Errorf("decode llm task payload: %w", err)
	}
	if payload.OrgID <= 0 {
		return TaskPayload{}, errors.New("llm task orgId is required")
	}
	payload.RequestID = strings.TrimSpace(payload.RequestID)
	if payload.RequestID == "" {
		return TaskPayload{}, errors.New("llm task requestId is required")
	}
	return payload, nil
}

func llmTaskID(orgID int32, requestID string, generation time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", orgID, requestID)))
	if generation.IsZero() {
		return fmt.Sprintf("llm:%d:%s", orgID, hex.EncodeToString(sum[:12]))
	}
	return fmt.Sprintf("llm:%d:%s:%x", orgID, hex.EncodeToString(sum[:12]), generation.UTC().UnixNano())
}
