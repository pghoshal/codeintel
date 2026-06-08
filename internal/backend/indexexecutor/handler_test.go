package indexexecutor

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/indexsubjobtask"

	"github.com/hibiken/asynq"
)

func TestHandlerSuccessClaimsHeartbeatsWritesArtifactAndSucceeds(t *testing.T) {
	store := &fakeStore{claimOK: true, heartbeatOK: true, artifactOK: true, successOK: true}
	artifactRoot, tempPath, finalPath, sha := writeAttemptArtifact(t, scipPayload(), []byte("semantic-index\n"))
	runner := &fakeRunner{result: Result{
		ArtifactTempPath: tempPath,
		ArtifactPath:     finalPath,
		ArtifactSHA256:   sha,
	}}
	ingestor := &fakeIngestor{}
	handler := newTestHandler(t, store, runner, ingestor, artifactRoot)

	if err := handler.Handle(context.Background(), asynq.NewTask("codeintel-index-scip-go", mustPayload(t, scipPayload()))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !store.claimed || store.heartbeatCount == 0 || !store.artifactWritten || ingestor.calls != 1 || !store.succeeded {
		t.Fatalf("store transitions claim=%v heartbeats=%d artifact=%v ingestCalls=%d success=%v",
			store.claimed, store.heartbeatCount, store.artifactWritten, ingestor.calls, store.succeeded)
	}
	if store.heartbeatCount < 2 {
		t.Fatalf("heartbeatCount=%d want at least 2 so execution and post-publish ingestion are both lease-protected", store.heartbeatCount)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d want 1", runner.calls)
	}
	if got := store.scope; got.ID != "subjob-1" || got.OrgID != 7 || got.RepoID != 42 || got.WorkerClass != "scip-go" {
		t.Fatalf("claim scope = %+v", got)
	}
	if _, err := os.Stat(finalPath); err != nil {
		t.Fatalf("final artifact not published: %v", err)
	}
}

func TestHandlerRunnerFailureMarksFailedAndDoesNotReturnAsynqError(t *testing.T) {
	store := &fakeStore{claimOK: true, heartbeatOK: true, failOK: true}
	runner := &fakeRunner{err: errors.New("scip-go failed")}
	handler := newTestHandler(t, store, runner, &fakeIngestor{}, t.TempDir())

	err := handler.Handle(context.Background(), asynq.NewTask("codeintel-index-scip-go", mustPayload(t, scipPayload())))
	if err != nil {
		t.Fatalf("Handle returned queue error after durable MarkFailed: %v", err)
	}
	if !store.failed {
		t.Fatal("runner failure did not mark subjob failed")
	}
	if store.succeeded {
		t.Fatal("runner failure marked subjob succeeded")
	}
}

func TestHandlerClaimFalseSkipsStaleTask(t *testing.T) {
	store := &fakeStore{claimOK: false}
	runner := &fakeRunner{}
	handler := newTestHandler(t, store, runner, &fakeIngestor{}, t.TempDir())

	if err := handler.Handle(context.Background(), asynq.NewTask("codeintel-index-core", mustPayload(t, corePayload()))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("runner calls = %d want 0", runner.calls)
	}
	if store.heartbeatCount != 0 || store.failed || store.succeeded {
		t.Fatalf("stale task changed state: heartbeats=%d failed=%v success=%v",
			store.heartbeatCount, store.failed, store.succeeded)
	}
}

func TestHandlerDropsWrongQueueTaskBeforeClaim(t *testing.T) {
	store := &fakeStore{claimOK: true}
	runner := &fakeRunner{}
	handler := newTestHandler(t, store, runner, &fakeIngestor{}, t.TempDir())

	if err := handler.Handle(context.Background(), asynq.NewTask("codeintel-index-scip-jvm", mustPayload(t, scipPayload()))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if store.claimed || runner.calls != 0 {
		t.Fatalf("wrong queue claimed=%v runnerCalls=%d", store.claimed, runner.calls)
	}
}

func TestHandlerExecutorUnavailablePreservesAttemptForDBRetry(t *testing.T) {
	store := &fakeStore{claimOK: true, heartbeatOK: true, retryableOK: true}
	runner := &fakeRunner{err: ErrExecutorUnavailable}
	handler := newTestHandler(t, store, runner, &fakeIngestor{}, t.TempDir())

	if err := handler.Handle(context.Background(), asynq.NewTask("codeintel-index-scip-go", mustPayload(t, scipPayload()))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !store.retryable {
		t.Fatal("executor unavailable did not mark retryable infrastructure failure")
	}
	if store.failed || store.succeeded {
		t.Fatalf("executor unavailable progressed failed=%v succeeded=%v", store.failed, store.succeeded)
	}
}

func TestHandlerRejectsIncompleteArtifactMetadata(t *testing.T) {
	store := &fakeStore{claimOK: true, heartbeatOK: true, failOK: true}
	runner := &fakeRunner{result: Result{ArtifactPath: "/efs/artifacts/final.scip"}}
	handler := newTestHandler(t, store, runner, &fakeIngestor{}, t.TempDir())

	if err := handler.Handle(context.Background(), asynq.NewTask("codeintel-index-scip-go", mustPayload(t, scipPayload()))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !store.failed {
		t.Fatal("incomplete artifact metadata did not mark failure")
	}
	if store.artifactWritten || store.succeeded {
		t.Fatalf("incomplete artifact progressed artifact=%v success=%v", store.artifactWritten, store.succeeded)
	}
}

func TestHandlerArtifactIngestFailureMarksFailed(t *testing.T) {
	store := &fakeStore{claimOK: true, heartbeatOK: true, artifactOK: true, failOK: true}
	artifactRoot, tempPath, finalPath, sha := writeAttemptArtifact(t, scipPayload(), []byte("semantic-index\n"))
	runner := &fakeRunner{result: Result{
		ArtifactTempPath: tempPath,
		ArtifactPath:     finalPath,
		ArtifactSHA256:   sha,
	}}
	ingestor := &fakeIngestor{err: errors.New("decode failed")}
	handler := newTestHandler(t, store, runner, ingestor, artifactRoot)

	if err := handler.Handle(context.Background(), asynq.NewTask("codeintel-index-scip-go", mustPayload(t, scipPayload()))); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !store.artifactWritten || !store.failed || store.succeeded {
		t.Fatalf("ingest failure transitions artifact=%v failed=%v succeeded=%v", store.artifactWritten, store.failed, store.succeeded)
	}
}

func newTestHandler(t *testing.T, store Store, runner Runner, ingestor ArtifactIngestor, artifactRoot string) *Handler {
	t.Helper()
	validator, err := NewFilesystemArtifactValidator(artifactRoot)
	if err != nil {
		t.Fatalf("NewFilesystemArtifactValidator: %v", err)
	}
	handler, err := NewHandler(store, runner, nil, Config{
		LeaseDuration:     time.Minute,
		HeartbeatInterval: 30 * time.Second,
		LeaseOwner:        "worker-a",
		ArtifactValidator: validator,
		ArtifactIngestor:  ingestor,
		Now:               func() time.Time { return time.Unix(1000, 0).UTC() },
	})
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	return handler
}

func mustPayload(t *testing.T, payload indexsubjobtask.Payload) []byte {
	t.Helper()
	raw, err := indexsubjobtask.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return raw
}

func scipPayload() indexsubjobtask.Payload {
	workspaceID := "atom-ws-1"
	language := "go"
	projectRoot := ""
	indexer := "scip-go"
	return indexsubjobtask.Payload{
		SubjobID:          "subjob-1",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        "0123456789abcdef0123456789abcdef01234567",
		Layer:             indexsubjobtask.LayerSCIP,
		Language:          &language,
		ProjectRoot:       &projectRoot,
		Indexer:           &indexer,
		WorkerClass:       "scip-go",
		QueueName:         "codeintel-index-scip-go",
		Attempt:           2,
	}
}

func corePayload() indexsubjobtask.Payload {
	payload := scipPayload()
	payload.Layer = indexsubjobtask.LayerASTTreeSitter
	payload.Language = nil
	payload.ProjectRoot = nil
	payload.Indexer = nil
	payload.WorkerClass = "core"
	payload.QueueName = "codeintel-index-core"
	return payload
}

type fakeStore struct {
	claimOK         bool
	heartbeatOK     bool
	artifactOK      bool
	successOK       bool
	failOK          bool
	retryableOK     bool
	claimed         bool
	scope           indexsubjobs.ClaimScope
	heartbeatCount  int
	artifactWritten bool
	succeeded       bool
	failed          bool
	retryable       bool
}

func (s *fakeStore) ClaimScoped(_ context.Context, scope indexsubjobs.ClaimScope, _, _ string, _ time.Time) (bool, error) {
	s.claimed = true
	s.scope = scope
	return s.claimOK, nil
}

func (s *fakeStore) Heartbeat(context.Context, string, string, string, time.Time) (bool, error) {
	s.heartbeatCount++
	return s.heartbeatOK, nil
}

func (s *fakeStore) MarkArtifactWritten(context.Context, string, string, string, string, string, string) (bool, error) {
	s.artifactWritten = true
	return s.artifactOK, nil
}

func (s *fakeStore) MarkSucceeded(context.Context, string, string, string) (bool, error) {
	s.succeeded = true
	return s.successOK, nil
}

func (s *fakeStore) MarkFailed(context.Context, string, string, string, string, string) (bool, error) {
	s.failed = true
	return s.failOK, nil
}

func (s *fakeStore) MarkRetryableInfrastructureFailure(context.Context, string, string, string, string, string) (bool, error) {
	s.retryable = true
	return s.retryableOK, nil
}

type fakeRunner struct {
	result Result
	err    error
	calls  int
}

func (r *fakeRunner) Execute(context.Context, Job) (Result, error) {
	r.calls++
	return r.result, r.err
}

type fakeIngestor struct {
	err   error
	calls int
}

func (i *fakeIngestor) Ingest(context.Context, indexsubjobtask.Payload, Result, string, string) error {
	i.calls++
	return i.err
}

func writeAttemptArtifact(t *testing.T, payload indexsubjobtask.Payload, content []byte) (root, tempPath, finalPath, sha string) {
	t.Helper()
	root = t.TempDir()
	base := filepath.Join(root, fmt.Sprint(payload.OrgID), fmt.Sprint(payload.RepoID), artifactScopeSegment(*payload.WorkspaceID), artifactScopeSegment(payload.Branch), payload.CommitHash)
	tempPath = filepath.Join(base, "tmp", payload.SubjobID+".scip.tmp")
	finalPath = filepath.Join(base, "scip", payload.SubjobID+".scip")
	if err := os.MkdirAll(filepath.Dir(tempPath), 0o755); err != nil {
		t.Fatalf("mkdir temp artifact: %v", err)
	}
	if err := os.WriteFile(tempPath, content, 0o644); err != nil {
		t.Fatalf("write temp artifact: %v", err)
	}
	sum := sha256.Sum256(content)
	return root, tempPath, finalPath, "sha256:" + fmt.Sprintf("%x", sum[:])
}
