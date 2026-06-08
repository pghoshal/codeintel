// Package audit hosts codeintel-backend's AuditService
// implementation. The server side of the wire contract defined in
// proto/codeintel/v1/audit_service.proto. Each Emit RPC inserts
// one row into the Audit Postgres table.
//
// Backend-private: this package is unimportable from
// codeintel-app, which is intentional. App fires events through
// pkg/audit.GRPCEmitter; backend persists them here.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	codeintelv1 "codeintel/proto/codeintel/v1"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// codeintelVersion is the build-time identifier that lands in
// every Audit row's codeintelVersion column. Carries a build
// SHA / semver string. The legacy schema had this column under
// a brand-prefixed name; the codeintel port renamed it to
// `codeintelVersion` per the brand-sweep policy. The constant is
// overridable via -ldflags at build time:
//
//	go build -ldflags="-X 'codeintel/internal/backend/audit.codeintelVersion=v0.1.0+abcd123'" ./cmd/codeintel-backend
var codeintelVersion = "0.0.0-dev"

// dbQuerier is the narrow subset of *pgxpool.Pool the audit server
// needs. Defined as an interface so unit tests can drop in a
// pgxmock without booting Postgres.
type dbQuerier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Server is the AuditService gRPC handler. Embeds the proto's
// UnimplementedAuditServiceServer for forward-compatibility — when
// the wire contract gains a new RPC, the proto-generated default
// returns Unimplemented until the impl catches up.
type Server struct {
	codeintelv1.UnimplementedAuditServiceServer
	db dbQuerier

	// newID is the per-row id generator. Wrapped as a field so
	// tests can substitute a deterministic generator. Defaults to
	// uuid.NewString.
	newID func() string
}

// NewServer constructs the audit-service handler bound to the
// supplied Postgres pool.
func NewServer(pool *pgxpool.Pool) *Server {
	return &Server{db: pool, newID: uuid.NewString}
}

// newServerWithDB is the test-only constructor that accepts the
// narrow dbQuerier interface so pgxmock can satisfy it without
// embedding a real *pgxpool.Pool.
func newServerWithDB(db dbQuerier) *Server {
	return &Server{db: db, newID: uuid.NewString}
}

// insertAuditSQL writes one row into the legacy-shaped Audit
// table. Column order matches the placeholder positions below.
// id/timestamp/action/actorId/actorType/targetId/targetType/
// codeintelVersion/metadata/orgId.
const insertAuditSQL = `INSERT INTO "Audit" ` +
	`("id", "timestamp", "action", "actorId", "actorType", "targetId", "targetType", "codeintelVersion", "metadata", "orgId") ` +
	`VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`

// Emit persists the supplied event. Validation rejects malformed
// requests with codes.InvalidArgument; persistence failures bubble
// up as codes.Internal via the standard gRPC error wrap.
func (s *Server) Emit(ctx context.Context, req *codeintelv1.EmitRequest) (*codeintelv1.EmitResponse, error) {
	if req == nil || req.Event == nil {
		return nil, fmt.Errorf("audit: Emit: event is required")
	}
	ev := req.Event
	if ev.Action == "" {
		return nil, fmt.Errorf("audit: Emit: action is required")
	}
	if ev.OrgId <= 0 {
		return nil, fmt.Errorf("audit: Emit: orgId must be positive")
	}

	// Build the metadata payload. The legacy Audit table does not
	// carry a separate requestId column; non-empty RequestId is
	// merged into the metadata JSONB as a top-level "requestId"
	// key so downstream consumers can still trace events back to
	// the originating request.
	metadataJSON, err := buildMetadataJSON(ev.Metadata, ev.RequestId)
	if err != nil {
		return nil, fmt.Errorf("audit: Emit: marshal metadata: %w", err)
	}

	// Event time defaults to server-now when the caller omits it,
	// matching the documented contract that emitter implementations
	// MAY stamp the time at transport. The Audit.timestamp column
	// is timestamp(3) without time zone; pgx writes UTC values
	// into this column as the wall-clock without an offset.
	eventTime := time.Now().UTC()
	if ev.Time != nil {
		eventTime = ev.Time.AsTime().UTC()
	}

	if _, err := s.db.Exec(ctx,
		insertAuditSQL,
		s.newID(),
		eventTime,
		ev.Action,
		ev.ActorId,
		actorTypeString(ev.ActorType),
		ev.TargetId,
		targetTypeString(ev.TargetType),
		codeintelVersion,
		metadataJSON,
		ev.OrgId,
	); err != nil {
		return nil, fmt.Errorf("audit: Emit: insert: %w", err)
	}
	return &codeintelv1.EmitResponse{}, nil
}

// buildMetadataJSON marshals the caller-supplied metadata map +
// optional requestId into a single JSONB payload. Returns nil
// when both inputs are empty so the column lands as SQL NULL —
// avoids storing a literal `{}` when no metadata is present.
func buildMetadataJSON(metadata map[string]string, requestID string) ([]byte, error) {
	if len(metadata) == 0 && requestID == "" {
		return nil, nil
	}
	merged := make(map[string]string, len(metadata)+1)
	for k, v := range metadata {
		merged[k] = v
	}
	if requestID != "" {
		merged["requestId"] = requestID
	}
	return json.Marshal(merged)
}

// actorTypeString flattens the proto enum to the canonical string
// stored in the Audit.actorType TEXT column.
func actorTypeString(a codeintelv1.ActorType) string {
	switch a {
	case codeintelv1.ActorType_ACTOR_TYPE_USER:
		return "user"
	case codeintelv1.ActorType_ACTOR_TYPE_API_KEY:
		return "api_key"
	case codeintelv1.ActorType_ACTOR_TYPE_SYSTEM:
		return "system"
	default:
		return "unspecified"
	}
}

// targetTypeString flattens the proto enum to the canonical string
// stored in the Audit.targetType TEXT column.
func targetTypeString(t codeintelv1.TargetType) string {
	switch t {
	case codeintelv1.TargetType_TARGET_TYPE_ORG:
		return "org"
	case codeintelv1.TargetType_TARGET_TYPE_CONNECTION:
		return "connection"
	case codeintelv1.TargetType_TARGET_TYPE_SECRET:
		return "secret"
	case codeintelv1.TargetType_TARGET_TYPE_MODEL:
		return "model"
	default:
		return "unspecified"
	}
}
