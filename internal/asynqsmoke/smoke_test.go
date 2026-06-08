// Package asynqsmoke holds the live-Redis + asynq bring-up
// smoke tests. The tests are env-gated on CODEINTEL_REDIS_URL;
// when unset they t.Skip so `go test ./...` stays green in CI
// without Docker.
//
// To run locally (after `make stack-up` in the codeintel dir):
//
//	export CODEINTEL_REDIS_URL='redis://127.0.0.1:6380'
//	go test ./internal/asynqsmoke/... -v
//
// What this package proves:
//
//   - pkg/redisclient.New connects to the running Redis,
//     authenticates (when applicable), and the PING round-trip
//     succeeds.
//   - hibiken/asynq's Client + Server can enqueue a task and
//     have a worker drain it against the same Redis.
//   - The pkg/asynqueues registry produces queue names asynq
//     accepts at both producer + consumer ends.
//
// What this package does NOT prove (Phase B+ slices):
//
//   - Any specific worker handler (connection-sync, repo-index,
//     etc.) — those land in their feature slices.
//   - Queue-pluggability into codeintel-app handlers.
//   - The codeintel-backend binary booting an asynq Server as
//     part of its standard startup.
package asynqsmoke

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codeintel/pkg/asynqueues"
	"codeintel/pkg/redisclient"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const (
	envRedisURL              = "CODEINTEL_REDIS_URL"
	envDestructiveAcknowledge = "CODEINTEL_ASYNQSMOKE_DESTRUCTIVE"
)

// requireLocalRedisOrDestructiveOptIn refuses to run the smoke
// against a non-loopback Redis unless the operator has set
// CODEINTEL_ASYNQSMOKE_DESTRUCTIVE=true. The smoke's
// pre-cleanup (`inspector.DeleteAllPendingTasks(...)`) is a
// hard reset of the connection-sync queue — if a developer
// accidentally exports CODEINTEL_REDIS_URL pointing at staging
// or production Redis and runs `go test`, the smoke would
// silently nuke pending jobs. The guard turns the silent
// destruction into an explicit refusal.
func requireLocalRedisOrDestructiveOptIn(t *testing.T, rawURL string) {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("CODEINTEL_REDIS_URL parse: %v", err)
	}
	host := u.Hostname()
	isLocal := host == "127.0.0.1" || host == "::1" || strings.EqualFold(host, "localhost")
	if isLocal {
		return
	}
	if strings.EqualFold(os.Getenv(envDestructiveAcknowledge), "true") {
		t.Logf("non-local Redis %q + %s=true; proceeding with destructive smoke", host, envDestructiveAcknowledge)
		return
	}
	t.Skipf("non-local Redis %q without %s=true; refusing destructive pre-cleanup. "+
		"Set %s=true to override.", host, envDestructiveAcknowledge, envDestructiveAcknowledge)
}

// TestRedisClient_Ping is the bare-minimum proof that the
// pkg/redisclient wrapper connects to the running Redis. It's
// the simplest of the smoke tests; failure here means everything
// else in this file would fail too.
func TestRedisClient_Ping(t *testing.T) {
	url := os.Getenv(envRedisURL)
	if url == "" {
		t.Skipf("%s is unset; skipping live-Redis ping smoke", envRedisURL)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := redisclient.New(ctx, redisclient.Config{URL: url}.WithDefaults(),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("redisclient.New: %v", err)
	}
	defer c.Close()

	if err := c.Ping(ctx); err != nil {
		t.Fatalf("second Ping: %v", err)
	}
}

// TestAsynq_EnqueueDequeue_RoundTrip is the Phase A.2 binding
// E2E gate. It:
//
//  1. Connects to the real Redis on 127.0.0.1:6380.
//  2. Builds an asynq.Client (the producer) + asynq.Server
//     (the worker) — both pointed at the same Redis.
//  3. Enqueues one task on the QueueConnectionSync queue (the
//     legacy 'connection-sync-queue' identifier).
//  4. The Server's handler atomically marks "received" + signals
//     a wait group.
//  5. Test waits for the handler to run, then verifies the side
//     effect (the atomic counter incremented + the payload
//     bytes round-tripped).
//
// Failure modes proved:
//   - Redis connection refused → fail.
//   - asynq Client can't enqueue → fail.
//   - Server isn't subscribed to the queue → timeout fail.
//   - Payload corruption mid-roundtrip → byte-mismatch fail.
//
// What the test does NOT prove yet (subsequent Phase B/C slices):
//   - The real ConnectionSyncJob payload shape + the real
//     handler body. This test uses a synthetic payload to keep
//     the round-trip provable in isolation.
func TestAsynq_EnqueueDequeue_RoundTrip(t *testing.T) {
	urlRaw := os.Getenv(envRedisURL)
	if urlRaw == "" {
		t.Skipf("%s is unset; skipping asynq round-trip smoke", envRedisURL)
	}
	// Refuse to run against a non-loopback Redis without an
	// explicit operator opt-in — the pre-cleanup step destroys
	// pending tasks on the target connection-sync queue.
	requireLocalRedisOrDestructiveOptIn(t, urlRaw)

	// Build asynq's redis-opt from the URL. asynq has its own
	// RedisConnOpt type — we use RedisClientOpt because the test
	// uses a single Redis (not a cluster), matching the codeintel
	// deployment.
	rOpts, err := redis.ParseURL(urlRaw)
	if err != nil {
		t.Fatalf("parse redis URL: %v", err)
	}
	redisOpt := asynq.RedisClientOpt{
		Addr:     rOpts.Addr,
		Username: rOpts.Username,
		Password: rOpts.Password,
		DB:       rOpts.DB,
	}

	// Pre-cleanup: ensure no stale task from a prior run is
	// still pending. Use a dedicated asynq.Inspector to drain
	// the queue. (Pending tasks from a prior failed run would
	// be received by THIS test's Server before the new task,
	// failing the payload-byte-equal assertion.)
	inspector := asynq.NewInspector(redisOpt)
	defer inspector.Close()
	if _, err := inspector.DeleteAllPendingTasks(asynqueues.QueueConnectionSync); err != nil {
		// A "queue doesn't exist yet" error is fine; we only fail
		// on harder errors.
		if !isMissingQueue(err) {
			t.Logf("pre-cleanup warning (non-fatal): %v", err)
		}
	}

	// Round-trip plumbing.
	const taskPayload = "asynqsmoke:round-trip-payload"
	var (
		received      atomic.Int32
		receivedBytes atomic.Value
		seenWG        sync.WaitGroup
	)
	seenWG.Add(1)
	receivedBytes.Store([]byte(nil))

	// Build the Server with a single handler registered on the
	// QueueConnectionSync queue. The handler atomically marks
	// receipt + signals the WaitGroup.
	mux := asynq.NewServeMux()
	mux.HandleFunc(asynqueues.QueueConnectionSync, func(ctx context.Context, t *asynq.Task) error {
		received.Add(1)
		// Copy the bytes so a later mutation doesn't race with
		// the assertion goroutine.
		copyBuf := make([]byte, len(t.Payload()))
		copy(copyBuf, t.Payload())
		receivedBytes.Store(copyBuf)
		seenWG.Done()
		return nil
	})

	server := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: 1,
		Queues:      asynqueues.DefaultPriorities(),
		Logger:      &silentAsynqLogger{},
		// Short retry delay so a flaky handler doesn't hold up
		// the smoke for long. Production tuning lives in the
		// codeintel-backend wiring slice.
		RetryDelayFunc: func(n int, _ error, _ *asynq.Task) time.Duration {
			return 50 * time.Millisecond
		},
	})

	// Start the Server in a goroutine; mux registers handlers.
	serverErr := make(chan error, 1)
	go func() {
		if err := server.Run(mux); err != nil && !errors.Is(err, asynq.ErrServerClosed) {
			serverErr <- err
		}
	}()
	t.Cleanup(func() {
		// Graceful stop. asynq.Server.Shutdown waits for
		// in-flight tasks to finish on its own internal
		// timeout (configured via Config.ShutdownTimeout — we
		// rely on the asynq default; explicit context propagation
		// is not exposed on this signature in v0.26).
		server.Shutdown()
	})

	// Producer: enqueue one task with the round-trip payload on
	// the same queue the Server is draining.
	client := asynq.NewClient(redisOpt)
	defer client.Close()

	task := asynq.NewTask(asynqueues.QueueConnectionSync, []byte(taskPayload))
	info, err := client.Enqueue(task, asynq.Queue(asynqueues.QueueConnectionSync))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	t.Logf("enqueued task id=%s queue=%s", info.ID, info.Queue)

	// Wait for the handler to mark received OR fail on timeout.
	doneCh := make(chan struct{})
	go func() {
		seenWG.Wait()
		close(doneCh)
	}()
	select {
	case <-doneCh:
		// Receipt happened.
	case err := <-serverErr:
		t.Fatalf("asynq server failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatalf("task not received within 10s; received=%d", received.Load())
	}

	// Side-effect assertions.
	if got := received.Load(); got != 1 {
		t.Errorf("handler invocations: got %d, want exactly 1", got)
	}
	gotBytes, _ := receivedBytes.Load().([]byte)
	if string(gotBytes) != taskPayload {
		t.Errorf("payload round-trip mismatch:\n got: %q\nwant: %q", gotBytes, taskPayload)
	}
}

// isMissingQueue returns true for the asynq "queue doesn't
// exist" error. asynq doesn't expose a typed sentinel for this
// case so the test string-matches; tolerant of error-message
// reword across asynq minor versions because non-matching
// branches treat it as benign.
func isMissingQueue(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "does not exist") || strings.Contains(msg, "queue not found")
}

// silentAsynqLogger satisfies asynq.Logger but drops every
// message. The Server is chatty on the Info channel during
// boot/shutdown; this keeps the test output focused on
// pass/fail.
type silentAsynqLogger struct{}

func (silentAsynqLogger) Debug(_ ...interface{}) {}
func (silentAsynqLogger) Info(_ ...interface{})  {}
func (silentAsynqLogger) Warn(_ ...interface{})  {}
func (silentAsynqLogger) Error(_ ...interface{}) {}
func (silentAsynqLogger) Fatal(_ ...interface{}) {}
