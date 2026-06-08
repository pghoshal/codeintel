// GRPCEmitter is the production Emitter implementation that
// forwards every audit Event to codeintel-backend over the gRPC
// contract defined in proto/codeintel/v1/audit_service.proto.
//
// The emitter is constructed in cmd/codeintel-app/main.go when
// CODEINTEL_BACKEND_GRPC_ADDR is set; otherwise NoopEmitter is
// used and events go to /dev/null. The two-way fan-out (audit
// table + SIEM forward) is backend's concern; the app emitter
// just gets the event onto the wire.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	codeintelv1 "codeintel/proto/codeintel/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// GRPCEmitter dials a single backend address. The connection is
// long-lived: gRPC handles HTTP/2 multiplexing and re-connection
// transparently. The struct is safe for concurrent use because
// the underlying *grpc.ClientConn is.
type GRPCEmitter struct {
	conn   *grpc.ClientConn
	client codeintelv1.AuditServiceClient
}

// NewGRPCEmitter dials the supplied backend address and returns
// the emitter ready for use. The dial is non-blocking — gRPC
// performs the connect on first RPC. Callers MUST defer
// Shutdown to release the connection.
//
// For now the dial is plaintext (insecure credentials). TLS is
// configured via a follow-up Config knob when the backend is
// deployed outside the trust zone.
func NewGRPCEmitter(addr string) (*GRPCEmitter, error) {
	if addr == "" {
		return nil, fmt.Errorf("audit: GRPCEmitter: addr is required")
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("audit: GRPCEmitter: dial %s: %w", addr, err)
	}
	return &GRPCEmitter{
		conn:   conn,
		client: codeintelv1.NewAuditServiceClient(conn),
	}, nil
}

// Emit forwards the supplied Event to backend. The proto request
// shape mirrors the Go-side audit.Event 1:1 — actor / target enums
// are mapped through helpers below.
func (e *GRPCEmitter) Emit(ctx context.Context, ev Event) error {
	pbEvent := &codeintelv1.Event{
		Action:     ev.Action,
		ActorId:    ev.ActorID,
		ActorType:  actorTypeProto(ev.ActorType),
		TargetId:   ev.TargetID,
		TargetType: targetTypeProto(ev.TargetType),
		OrgId:      ev.OrgID,
		RequestId:  ev.RequestID,
		Metadata:   metadataAsStrings(ev.Metadata),
	}
	if !ev.Time.IsZero() {
		pbEvent.Time = timestamppb.New(ev.Time.UTC())
	}
	_, err := e.client.Emit(ctx, &codeintelv1.EmitRequest{Event: pbEvent})
	return err
}

// Shutdown closes the gRPC connection. Safe to call multiple times
// — grpc.ClientConn.Close is idempotent under the documented
// contract.
func (e *GRPCEmitter) Shutdown(_ context.Context) error {
	if e == nil || e.conn == nil {
		return nil
	}
	return e.conn.Close()
}

// actorTypeProto maps the Go-side ActorType string to the proto
// enum. Unknown values fall through to ACTOR_TYPE_UNSPECIFIED so
// a new ActorType constant added on the app side without a proto
// update degrades to "unspecified" rather than dropping the event.
func actorTypeProto(a ActorType) codeintelv1.ActorType {
	switch a {
	case ActorUser:
		return codeintelv1.ActorType_ACTOR_TYPE_USER
	case ActorApiKey:
		return codeintelv1.ActorType_ACTOR_TYPE_API_KEY
	case ActorSystem:
		return codeintelv1.ActorType_ACTOR_TYPE_SYSTEM
	default:
		return codeintelv1.ActorType_ACTOR_TYPE_UNSPECIFIED
	}
}

// targetTypeProto maps the Go-side TargetType string to the proto
// enum, with the same unknown-fallthrough behaviour as above.
func targetTypeProto(t TargetType) codeintelv1.TargetType {
	switch t {
	case TargetOrg:
		return codeintelv1.TargetType_TARGET_TYPE_ORG
	case TargetConnection:
		return codeintelv1.TargetType_TARGET_TYPE_CONNECTION
	case TargetSecret:
		return codeintelv1.TargetType_TARGET_TYPE_SECRET
	case TargetModel:
		return codeintelv1.TargetType_TARGET_TYPE_MODEL
	default:
		return codeintelv1.TargetType_TARGET_TYPE_UNSPECIFIED
	}
}

// metadataAsStrings flattens the Go-side map[string]any to the
// proto's map<string, string> by JSON-encoding each value. This
// keeps the wire shape language-agnostic — a Python or Rust
// receiver can decode the JSON values the same way.
func metadataAsStrings(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		// String values pass through unquoted; everything else is
		// JSON-encoded. This avoids producing `"value"` wrapped in
		// double-quotes for the common case.
		switch s := v.(type) {
		case string:
			out[k] = s
		default:
			b, err := json.Marshal(s)
			if err != nil {
				// Skip the key rather than fail the emit — a
				// metadata serialisation bug must not block the
				// underlying business event.
				continue
			}
			out[k] = string(b)
		}
	}
	return out
}
