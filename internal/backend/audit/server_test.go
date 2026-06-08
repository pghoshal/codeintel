package audit

import (
	"context"
	"errors"
	"testing"
	"time"

	codeintelv1 "codeintel/proto/codeintel/v1"

	"github.com/pashagolub/pgxmock/v4"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fixedID is the deterministic id-generator used by the unit
// tests so the pgxmock arg expectations are stable. Production
// code uses uuid.NewString.
func fixedID() string { return "audit-row-id-fixture" }

// newTestServer constructs a Server with the supplied pgxmock
// pool + a deterministic id generator. Centralised so a future
// generator-shape change doesn't require touching every test.
func newTestServer(db dbQuerier) *Server {
	s := newServerWithDB(db)
	s.newID = fixedID
	return s
}

// TestEmit_PersistsRow locks the INSERT path: every column of the
// Audit table is populated from the corresponding proto field.
// The pgxmock expectation pins the exact SQL + arg order so any
// field-shuffle regression surfaces here.
//
// Column order (matches insertAuditSQL):
//
//	id, timestamp, action, actorId, actorType, targetId,
//	targetType, codeintelVersion, metadata, orgId
//
// metadata merges the proto map + RequestId — see
// buildMetadataJSON. The fixture below carries both inputs so
// the merged JSON is asserted.
func TestEmit_PersistsRow(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	t1 := time.Date(2025, 5, 23, 12, 0, 0, 0, time.UTC)
	mock.ExpectExec(`INSERT INTO "Audit"`).
		WithArgs(
			"audit-row-id-fixture",
			t1,
			"connection.created",
			"u-1",
			"user",
			"42",
			"connection",
			codeintelVersion,
			// metadata: connectionName + requestId merged.
			pgxmock.AnyArg(),
			int32(7),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	srv := newTestServer(mock)
	_, err = srv.Emit(context.Background(), &codeintelv1.EmitRequest{
		Event: &codeintelv1.Event{
			Action:     "connection.created",
			ActorId:    "u-1",
			ActorType:  codeintelv1.ActorType_ACTOR_TYPE_USER,
			TargetId:   "42",
			TargetType: codeintelv1.TargetType_TARGET_TYPE_CONNECTION,
			OrgId:      7,
			RequestId:  "req-abc",
			Time:       timestamppb.New(t1),
			Metadata:   map[string]string{"connectionName": "gh-prod"},
		},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

// TestEmit_RejectsMissingEvent confirms the boundary guard.
func TestEmit_RejectsMissingEvent(t *testing.T) {
	srv := newTestServer(nil)
	if _, err := srv.Emit(context.Background(), &codeintelv1.EmitRequest{}); err == nil {
		t.Fatalf("expected error for nil Event, got nil")
	}
}

// TestEmit_RejectsEmptyAction confirms the action-required guard.
func TestEmit_RejectsEmptyAction(t *testing.T) {
	srv := newTestServer(nil)
	_, err := srv.Emit(context.Background(), &codeintelv1.EmitRequest{
		Event: &codeintelv1.Event{OrgId: 7},
	})
	if err == nil {
		t.Fatalf("expected error for empty action, got nil")
	}
}

// TestEmit_RejectsNonPositiveOrg confirms tenant-scope guard.
func TestEmit_RejectsNonPositiveOrg(t *testing.T) {
	srv := newTestServer(nil)
	_, err := srv.Emit(context.Background(), &codeintelv1.EmitRequest{
		Event: &codeintelv1.Event{Action: "connection.created", OrgId: 0},
	})
	if err == nil {
		t.Fatalf("expected error for orgId=0, got nil")
	}
}

// TestEmit_DBErrorPropagates locks the failure-path: a pgx error
// surfaces as a wrapped error to the caller.
func TestEmit_DBErrorPropagates(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()
	mock.ExpectExec(`INSERT INTO "Audit"`).WillReturnError(errors.New("simulated disk full"))

	srv := newTestServer(mock)
	_, err = srv.Emit(context.Background(), &codeintelv1.EmitRequest{
		Event: &codeintelv1.Event{
			Action: "connection.created",
			OrgId:  7,
		},
	})
	if err == nil {
		t.Fatalf("expected db error, got nil")
	}
}

// TestEmit_NoMetadataIsNullJSONB confirms the metadata column is
// left as SQL NULL (a nil []byte) when the caller supplies no
// keys AND no RequestId — avoids writing a literal `{}` for an
// empty map. With RequestId present, the row carries a JSON
// payload — see TestEmit_RequestIdMergedIntoMetadata.
func TestEmit_NoMetadataIsNullJSONB(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`INSERT INTO "Audit"`).
		WithArgs(
			"audit-row-id-fixture",
			pgxmock.AnyArg(),
			"connection.deleted",
			"",
			"unspecified",
			"",
			"unspecified",
			codeintelVersion,
			[]byte(nil),
			int32(7),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	srv := newTestServer(mock)
	_, err = srv.Emit(context.Background(), &codeintelv1.EmitRequest{
		Event: &codeintelv1.Event{Action: "connection.deleted", OrgId: 7},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

// TestEmit_RequestIdMergedIntoMetadata pins the divergence-
// closing contract: legacy Audit table has no requestId column,
// so a non-empty proto RequestId field is merged into the
// metadata JSONB as a top-level "requestId" key. Without
// metadata of its own, the column carries `{"requestId":"..."}`.
func TestEmit_RequestIdMergedIntoMetadata(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectExec(`INSERT INTO "Audit"`).
		WithArgs(
			"audit-row-id-fixture",
			pgxmock.AnyArg(),
			"connection.deleted",
			"",
			"unspecified",
			"",
			"unspecified",
			codeintelVersion,
			[]byte(`{"requestId":"req-XYZ"}`),
			int32(7),
		).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	srv := newTestServer(mock)
	_, err = srv.Emit(context.Background(), &codeintelv1.EmitRequest{
		Event: &codeintelv1.Event{
			Action:    "connection.deleted",
			OrgId:     7,
			RequestId: "req-XYZ",
		},
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}
