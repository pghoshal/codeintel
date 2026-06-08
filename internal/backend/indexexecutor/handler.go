// Package indexexecutor consumes class-specific indexing subjob
// queues. It owns the durable claim/heartbeat/complete state machine;
// CPU-heavy execution is delegated to a configured executor runner.
package indexexecutor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/indexsubjobtask"

	"github.com/hibiken/asynq"
)

const (
	defaultLeaseDuration     = 2 * time.Minute
	defaultHeartbeatInterval = 30 * time.Second
)

type Store interface {
	ClaimScoped(context.Context, indexsubjobs.ClaimScope, string, string, time.Time) (bool, error)
	Heartbeat(context.Context, string, string, string, time.Time) (bool, error)
	MarkArtifactWritten(context.Context, string, string, string, string, string, string) (bool, error)
	MarkSucceeded(context.Context, string, string, string) (bool, error)
	MarkFailed(context.Context, string, string, string, string, string) (bool, error)
	MarkRetryableInfrastructureFailure(context.Context, string, string, string, string, string) (bool, error)
}

type Runner interface {
	Execute(context.Context, Job) (Result, error)
}

type ArtifactIngestor interface {
	Ingest(context.Context, indexsubjobtask.Payload, Result, string, string) error
}

type Job struct {
	Payload indexsubjobtask.Payload
}

type Result struct {
	ArtifactTempPath string
	ArtifactPath     string
	ArtifactSHA256   string
	Metadata         map[string]string
}

type Config struct {
	LeaseDuration     time.Duration
	HeartbeatInterval time.Duration
	LeaseOwner        string
	ArtifactValidator ArtifactValidator
	ArtifactIngestor  ArtifactIngestor
	Now               func() time.Time
}

type Handler struct {
	store             Store
	runner            Runner
	artifactValidator ArtifactValidator
	artifactIngestor  ArtifactIngestor
	logger            *slog.Logger
	leaseDuration     time.Duration
	heartbeatInterval time.Duration
	leaseOwner        string
	now               func() time.Time
}

func NewHandler(store Store, runner Runner, logger *slog.Logger, cfg Config) (*Handler, error) {
	if store == nil {
		return nil, errors.New("indexexecutor: store is required")
	}
	if runner == nil {
		return nil, errors.New("indexexecutor: runner is required")
	}
	if cfg.ArtifactValidator == nil {
		return nil, errors.New("indexexecutor: artifact validator is required")
	}
	if cfg.ArtifactIngestor == nil {
		return nil, errors.New("indexexecutor: artifact ingestor is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	leaseDuration := cfg.LeaseDuration
	if leaseDuration <= 0 {
		leaseDuration = defaultLeaseDuration
	}
	heartbeatInterval := cfg.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultHeartbeatInterval
	}
	if heartbeatInterval >= leaseDuration {
		return nil, errors.New("indexexecutor: heartbeat interval must be shorter than lease duration")
	}
	leaseOwner := cfg.LeaseOwner
	if leaseOwner == "" {
		leaseOwner = generatedLeaseOwner()
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Handler{
		store:             store,
		runner:            runner,
		artifactValidator: cfg.ArtifactValidator,
		artifactIngestor:  cfg.ArtifactIngestor,
		logger:            logger.With("component", "index-executor"),
		leaseDuration:     leaseDuration,
		heartbeatInterval: heartbeatInterval,
		leaseOwner:        leaseOwner,
		now:               now,
	}, nil
}

func (h *Handler) AsynqHandlerFunc() func(context.Context, *asynq.Task) error {
	return h.Handle
}

func (h *Handler) Handle(ctx context.Context, task *asynq.Task) error {
	if h == nil || h.store == nil || h.runner == nil {
		return errors.New("indexexecutor: handler is not configured")
	}
	if task == nil {
		return errors.New("indexexecutor: nil task")
	}
	payload, err := indexsubjobtask.Unmarshal(task.Payload())
	if err != nil {
		h.logger.Warn("dropping malformed index subjob task", "taskType", task.Type(), "err", err)
		return nil
	}
	if task.Type() != payload.QueueName {
		h.logger.Warn("dropping wrong-queue index subjob task",
			"taskType", task.Type(),
			"payloadQueue", payload.QueueName,
			"subjobId", payload.SubjobID,
		)
		return nil
	}
	attemptID := generatedAttemptID(payload.SubjobID, payload.Attempt)
	leaseUntil := h.now().Add(h.leaseDuration)
	ok, err := h.store.ClaimScoped(ctx, toClaimScope(payload), h.leaseOwner, attemptID, leaseUntil)
	if err != nil {
		return fmt.Errorf("claim subjob %s: %w", payload.SubjobID, err)
	}
	if !ok {
		h.logger.Info("index subjob claim skipped",
			"subjobId", payload.SubjobID,
			"repoIndexingJobId", payload.RepoIndexingJobID,
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"layer", payload.Layer,
			"attempt", payload.Attempt,
		)
		return nil
	}

	result, runErr := h.runWithHeartbeat(ctx, payload, attemptID)
	if runErr != nil {
		if errors.Is(runErr, ErrExecutorUnavailable) {
			if markErr := h.markRetryableInfrastructureFailure(ctx, payload.SubjobID, attemptID, "EXECUTOR_UNAVAILABLE", runErr.Error()); markErr != nil {
				return markErr
			}
			h.logger.Warn("index subjob executor unavailable; attempt preserved for retry",
				"subjobId", payload.SubjobID,
				"repoIndexingJobId", payload.RepoIndexingJobID,
				"orgId", payload.OrgID,
				"repoId", payload.RepoID,
				"layer", payload.Layer,
				"attempt", payload.Attempt,
				"err", runErr,
			)
			return nil
		}
		if markErr := h.markFailed(ctx, payload.SubjobID, attemptID, "EXECUTION_FAILED", runErr.Error()); markErr != nil {
			return markErr
		}
		h.logger.Warn("index subjob failed; durable retry state updated",
			"subjobId", payload.SubjobID,
			"repoIndexingJobId", payload.RepoIndexingJobID,
			"orgId", payload.OrgID,
			"repoId", payload.RepoID,
			"layer", payload.Layer,
			"attempt", payload.Attempt,
			"err", runErr,
		)
		return nil
	}

	postFailureCode := ""
	if err := h.withHeartbeat(ctx, payload, attemptID, func(postCtx context.Context) error {
		validated, err := h.artifactValidator.ValidateAndPublish(postCtx, payload, result)
		if err != nil {
			postFailureCode = "INVALID_ARTIFACT"
			return err
		}
		ok, err = h.store.MarkArtifactWritten(postCtx, payload.SubjobID, h.leaseOwner, attemptID, validated.ArtifactTempPath, validated.ArtifactPath, validated.ArtifactSHA256)
		if err != nil {
			return fmt.Errorf("mark artifact written for subjob %s: %w", payload.SubjobID, err)
		}
		if !ok {
			return fmt.Errorf("mark artifact written for subjob %s: lease lost", payload.SubjobID)
		}

		if err := h.artifactIngestor.Ingest(postCtx, payload, validated, h.leaseOwner, attemptID); err != nil {
			postFailureCode = "ARTIFACT_INGEST_FAILED"
			return err
		}
		return nil
	}); err != nil {
		if postFailureCode != "" {
			if markErr := h.markFailed(ctx, payload.SubjobID, attemptID, postFailureCode, err.Error()); markErr != nil {
				return markErr
			}
			return nil
		}
		return err
	}

	ok, err = h.store.MarkSucceeded(ctx, payload.SubjobID, h.leaseOwner, attemptID)
	if err != nil {
		return fmt.Errorf("mark subjob %s succeeded: %w", payload.SubjobID, err)
	}
	if !ok {
		return fmt.Errorf("mark subjob %s succeeded: lease lost", payload.SubjobID)
	}
	h.logger.Info("index subjob completed",
		"subjobId", payload.SubjobID,
		"repoIndexingJobId", payload.RepoIndexingJobID,
		"orgId", payload.OrgID,
		"repoId", payload.RepoID,
		"layer", payload.Layer,
		"attempt", payload.Attempt,
	)
	return nil
}

func (h *Handler) runWithHeartbeat(ctx context.Context, payload indexsubjobtask.Payload, attemptID string) (Result, error) {
	var result Result
	err := h.withHeartbeat(ctx, payload, attemptID, func(runCtx context.Context) error {
		out, err := h.runner.Execute(runCtx, Job{Payload: payload})
		result = out
		return err
	})
	return result, err
}

func (h *Handler) withHeartbeat(ctx context.Context, payload indexsubjobtask.Payload, attemptID string, fn func(context.Context) error) error {
	firstLease := h.now().Add(h.leaseDuration)
	ok, err := h.store.Heartbeat(ctx, payload.SubjobID, h.leaseOwner, attemptID, firstLease)
	if err != nil {
		return fmt.Errorf("initial heartbeat: %w", err)
	}
	if !ok {
		return errors.New("initial heartbeat: lease lost")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(h.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				leaseUntil := h.now().Add(h.leaseDuration)
				ok, err := h.store.Heartbeat(runCtx, payload.SubjobID, h.leaseOwner, attemptID, leaseUntil)
				if err != nil {
					select {
					case heartbeatErr <- fmt.Errorf("heartbeat: %w", err):
					default:
					}
					cancel()
					return
				}
				if !ok {
					select {
					case heartbeatErr <- errors.New("heartbeat: lease lost"):
					default:
					}
					cancel()
					return
				}
			case <-runCtx.Done():
				return
			}
		}
	}()

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- fn(runCtx)
	}()

	select {
	case hbErr := <-heartbeatErr:
		cancel()
		select {
		case <-runnerDone:
		case <-time.After(5 * time.Second):
			h.logger.Warn("index subjob function did not stop after heartbeat loss",
				"subjobId", payload.SubjobID,
				"repoIndexingJobId", payload.RepoIndexingJobID,
				"orgId", payload.OrgID,
				"repoId", payload.RepoID,
				"layer", payload.Layer,
			)
		}
		<-heartbeatDone
		return hbErr
	case err := <-runnerDone:
		cancel()
		<-heartbeatDone
		if err != nil {
			return err
		}
		return nil
	}
}

func (h *Handler) markFailed(ctx context.Context, subjobID, attemptID, code, message string) error {
	ok, err := h.store.MarkFailed(ctx, subjobID, h.leaseOwner, attemptID, code, message)
	if err != nil {
		return fmt.Errorf("mark subjob %s failed: %w", subjobID, err)
	}
	if !ok {
		return fmt.Errorf("mark subjob %s failed: lease lost", subjobID)
	}
	return nil
}

func (h *Handler) markRetryableInfrastructureFailure(ctx context.Context, subjobID, attemptID, code, message string) error {
	ok, err := h.store.MarkRetryableInfrastructureFailure(ctx, subjobID, h.leaseOwner, attemptID, code, message)
	if err != nil {
		return fmt.Errorf("mark subjob %s retryable infrastructure failure: %w", subjobID, err)
	}
	if !ok {
		return fmt.Errorf("mark subjob %s retryable infrastructure failure: lease lost", subjobID)
	}
	return nil
}

func toClaimScope(p indexsubjobtask.Payload) indexsubjobs.ClaimScope {
	return indexsubjobs.ClaimScope{
		ID:                p.SubjobID,
		RepoIndexingJobID: p.RepoIndexingJobID,
		OrgID:             p.OrgID,
		WorkspaceID:       p.WorkspaceID,
		RepoID:            p.RepoID,
		Branch:            p.Branch,
		Revision:          p.Revision,
		CommitHash:        p.CommitHash,
		Layer:             indexsubjobs.Layer(p.Layer),
		Language:          p.Language,
		ProjectRoot:       p.ProjectRoot,
		Indexer:           p.Indexer,
		WorkerClass:       p.WorkerClass,
		QueueName:         p.QueueName,
		Attempt:           p.Attempt,
	}
}

func hasCompleteArtifact(result Result) bool {
	return result.ArtifactTempPath != "" && result.ArtifactPath != "" && result.ArtifactSHA256 != ""
}

func generatedLeaseOwner() string {
	hostname, _ := os.Hostname()
	return "index-executor-" + hostname + "-" + nonceHex(12)
}

func generatedAttemptID(subjobID string, attempt int32) string {
	return fmt.Sprintf("%s:%d:%s", subjobID, attempt, nonceHex(12))
}

func nonceHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
