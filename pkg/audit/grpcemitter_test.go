package audit

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	codeintelv1 "codeintel/proto/codeintel/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeAuditServer records every Emit RPC so the test can lock the
// wire-format mapping from audit.Event to codeintelv1.Event.
type fakeAuditServer struct {
	codeintelv1.UnimplementedAuditServiceServer
	mu       sync.Mutex
	received []*codeintelv1.EmitRequest
}

func (f *fakeAuditServer) Emit(_ context.Context, req *codeintelv1.EmitRequest) (*codeintelv1.EmitResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.received = append(f.received, req)
	return &codeintelv1.EmitResponse{}, nil
}

// startFakeBackend spins up an in-process gRPC server on a random
// port and returns its address plus a cleanup func.
func startFakeBackend(t *testing.T) (addr string, fake *fakeAuditServer, cleanup func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake = &fakeAuditServer{}
	codeintelv1.RegisterAuditServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	return lis.Addr().String(), fake, func() { srv.GracefulStop() }
}

// TestGRPCEmitter_EmitMapsAllFields locks the field-by-field
// mapping from audit.Event → proto Event. A regression that
// shuffles fields, drops the time, or boxes metadata wrong fails
// this test.
func TestGRPCEmitter_EmitMapsAllFields(t *testing.T) {
	addr, fake, cleanup := startFakeBackend(t)
	defer cleanup()

	emitter, err := NewGRPCEmitter(addr)
	if err != nil {
		t.Fatalf("NewGRPCEmitter: %v", err)
	}
	defer func() { _ = emitter.Shutdown(context.Background()) }()

	now := time.Date(2025, 5, 23, 12, 0, 0, 0, time.UTC)
	err = emitter.Emit(context.Background(), Event{
		Action:     "connection.created",
		ActorID:    "u-1",
		ActorType:  ActorUser,
		TargetID:   "42",
		TargetType: TargetConnection,
		OrgID:      7,
		RequestID:  "req-abc",
		Time:       now,
		Metadata:   map[string]any{"name": "gh-prod", "count": 3},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.received) != 1 {
		t.Fatalf("received %d events, want 1", len(fake.received))
	}
	ev := fake.received[0].Event
	if ev.Action != "connection.created" || ev.ActorId != "u-1" || ev.TargetId != "42" {
		t.Errorf("scalar fields: %+v", ev)
	}
	if ev.ActorType != codeintelv1.ActorType_ACTOR_TYPE_USER {
		t.Errorf("ActorType: got %v, want USER", ev.ActorType)
	}
	if ev.TargetType != codeintelv1.TargetType_TARGET_TYPE_CONNECTION {
		t.Errorf("TargetType: got %v, want CONNECTION", ev.TargetType)
	}
	if ev.OrgId != 7 {
		t.Errorf("OrgId: got %d, want 7", ev.OrgId)
	}
	if ev.RequestId != "req-abc" {
		t.Errorf("RequestId: got %q, want req-abc", ev.RequestId)
	}
	if ev.Time == nil || !ev.Time.AsTime().Equal(now) {
		t.Errorf("Time: got %v, want %v", ev.Time, now)
	}
	if got := ev.Metadata["name"]; got != "gh-prod" {
		t.Errorf("Metadata[name]: got %q, want gh-prod (string passthrough)", got)
	}
	if got := ev.Metadata["count"]; got != "3" {
		t.Errorf("Metadata[count]: got %q, want \"3\" (JSON-encoded non-string)", got)
	}
}

// TestGRPCEmitter_ZeroTimeOmits confirms the Time field is left
// nil on the wire when the caller doesn't set it — backend
// stamps server-now in that case.
func TestGRPCEmitter_ZeroTimeOmits(t *testing.T) {
	addr, fake, cleanup := startFakeBackend(t)
	defer cleanup()
	emitter, _ := NewGRPCEmitter(addr)
	defer func() { _ = emitter.Shutdown(context.Background()) }()

	_ = emitter.Emit(context.Background(), Event{Action: "x", OrgID: 1})
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.received) != 1 {
		t.Fatalf("received %d", len(fake.received))
	}
	if fake.received[0].Event.Time != nil {
		t.Errorf("Time: got non-nil, want nil for zero-value caller-side Time")
	}
}

// TestNewGRPCEmitter_RejectsEmptyAddr.
func TestNewGRPCEmitter_RejectsEmptyAddr(t *testing.T) {
	if _, err := NewGRPCEmitter(""); err == nil {
		t.Fatalf("expected error on empty addr")
	}
}

// keep imports stable
var _ = insecure.NewCredentials
