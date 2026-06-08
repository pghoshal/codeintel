package analytics

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestNoopEmitter_CaptureAndShutdownBothNil locks the contract
// that the default no-op emitter never returns an error and never
// observes the event payload. The zero-value Event{} branch is
// included so the contract that consumers may pass a partially-
// populated event (no Time, nil Properties) stays exercised.
func TestNoopEmitter_CaptureAndShutdownBothNil(t *testing.T) {
	var e Emitter = NoopEmitter{}
	if err := e.Capture(context.Background(), Event{Name: "x"}); err != nil {
		t.Fatalf("NoopEmitter.Capture: got %v, want nil", err)
	}
	if err := e.Capture(context.Background(), Event{}); err != nil {
		t.Fatalf("NoopEmitter.Capture(zero-value): got %v, want nil", err)
	}
	if err := e.Shutdown(context.Background()); err != nil {
		t.Fatalf("NoopEmitter.Shutdown: got %v, want nil", err)
	}
}

// spyEmitter is a thread-safe in-memory Emitter for tests that
// need to assert what handlers fired. It also doubles as a
// reference implementation for the documented contract:
// Capture must be safe under concurrent invocation.
type spyEmitter struct {
	mu       sync.Mutex
	captured []Event
	shutdown int
}

func (s *spyEmitter) Capture(_ context.Context, e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = append(s.captured, e)
	return nil
}

func (s *spyEmitter) Shutdown(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdown++
	return nil
}

// TestSpyEmitter_CapturesInOrder confirms a concrete Emitter
// implementation can record events in the order they arrive.
// The test exists primarily to lock the interface signature so a
// future refactor that changes Capture's shape surfaces here.
func TestSpyEmitter_CapturesInOrder(t *testing.T) {
	spy := &spyEmitter{}
	now := time.Date(2025, 5, 23, 12, 0, 0, 0, time.UTC)
	_ = spy.Capture(context.Background(), Event{Name: "a.first", DistinctID: "i-1", Time: now})
	_ = spy.Capture(context.Background(), Event{Name: "b.second", DistinctID: "i-1", Time: now, Properties: map[string]any{"k": "v"}})

	if len(spy.captured) != 2 {
		t.Fatalf("captured count: got %d, want 2", len(spy.captured))
	}
	if spy.captured[0].Name != "a.first" || spy.captured[1].Name != "b.second" {
		t.Errorf("order: got %q, %q", spy.captured[0].Name, spy.captured[1].Name)
	}
	if got := spy.captured[1].Properties["k"]; got != "v" {
		t.Errorf("Properties[k]: got %v, want \"v\"", got)
	}
	if err := spy.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: got %v, want nil", err)
	}
	if spy.shutdown != 1 {
		t.Errorf("shutdown counter: got %d, want 1", spy.shutdown)
	}
}

// TestEmitterErrorPropagates confirms Capture's error return is
// not swallowed by the interface.
func TestEmitterErrorPropagates(t *testing.T) {
	wantErr := errors.New("telemetry backend rejected event")
	failing := failingEmitter{err: wantErr}
	if got := failing.Capture(context.Background(), Event{}); !errors.Is(got, wantErr) {
		t.Errorf("error propagation: got %v, want %v", got, wantErr)
	}
}

type failingEmitter struct {
	err error
}

func (f failingEmitter) Capture(_ context.Context, _ Event) error { return f.err }
func (f failingEmitter) Shutdown(_ context.Context) error         { return nil }

// TestSpyEmitter_ConcurrentCaptureSafe locks the documented
// contract that Emitter must be safe for concurrent use. A bug
// here would surface under go test -race.
func TestSpyEmitter_ConcurrentCaptureSafe(t *testing.T) {
	spy := &spyEmitter{}
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_ = spy.Capture(context.Background(), Event{Name: "concurrent", DistinctID: "i", Properties: map[string]any{"i": i}})
		}(i)
	}
	wg.Wait()
	if len(spy.captured) != n {
		t.Fatalf("captured count under concurrency: got %d, want %d", len(spy.captured), n)
	}
}
