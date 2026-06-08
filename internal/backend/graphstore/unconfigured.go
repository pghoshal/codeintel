package graphstore

import (
	"context"
	"errors"
	"fmt"

	"codeintel/pkg/graphschema"
)

// UnconfiguredCodeGraphStore is the no-op implementation the
// factory hands out when no Nebula address is configured. Direct
// port of packages/backend/src/codeGraph/store.ts:17-34.
//
// WriteSnapshot returns SKIPPED + an operator-facing reason on
// every call. MarkSnapshotForDeletion is a no-op. The construct
// exists so the codeintel-backend wiring code doesn't have to
// branch on "do we have a graph backend?" — it always has a
// store, the no-op variant just drops writes politely.
type UnconfiguredCodeGraphStore struct {
	// Reason is a free-form operator diagnostic embedded in
	// WriteSnapshot's ErrorMessage field. Examples: "CODEINTEL_NEBULA_ADDR
	// is not configured", "config validation failed during startup".
	Reason string
}

// NewUnconfiguredStore is the conventional constructor mirroring
// the TS class constructor at store.ts:18.
func NewUnconfiguredStore(reason string) *UnconfiguredCodeGraphStore {
	return &UnconfiguredCodeGraphStore{Reason: reason}
}

// WriteSnapshot returns a SKIPPED result identifying the
// snapshot that wasn't written. Mirrors the TS body at
// store.ts:20-29:
//
//	return {
//	  status: "SKIPPED",
//	  vertexCount: 0,
//	  edgeCount: 0,
//	  anchorCount: 0,
//	  linkedEdgeCount: 0,
//	  errorMessage: `${this.reason}; skipped graph snapshot
//	    ${snapshot.repoId}@${snapshot.commitHash.slice(0, 12)}.`,
//	};
//
// All counts are zero — no rows landed.
func (s *UnconfiguredCodeGraphStore) WriteSnapshot(_ context.Context, snapshot graphschema.CodeGraphSnapshot) (graphschema.CodeGraphWriteResult, error) {
	return graphschema.CodeGraphWriteResult{
		Status: graphschema.WriteStatusSkipped,
		ErrorMessage: fmt.Sprintf("%s; skipped graph snapshot %d@%s.",
			s.Reason, snapshot.RepoID, shortCommit(snapshot.CommitHash)),
	}, nil
}

// WriteRenderedStatements validates the Rust-rendered statement
// payload before returning SKIPPED. Even an unconfigured graph
// deployment should reject malformed queue data deterministically
// rather than hiding a poison-message producer bug.
func (s *UnconfiguredCodeGraphStore) WriteRenderedStatements(_ context.Context, input RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error) {
	if _, _, err := validateRenderedStatements(input); err != nil {
		return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
	}
	return graphschema.CodeGraphWriteResult{
		Status: graphschema.WriteStatusSkipped,
		ErrorMessage: fmt.Sprintf("%s; skipped rendered graph snapshot %d@%s.",
			s.Reason, input.RepoID, shortCommit(input.CommitHash)),
	}, nil
}

// MarkSnapshotForDeletion is a no-op. TS source returns
// `undefined`; the Go equivalent returns nil to satisfy the
// interface's error return.
func (s *UnconfiguredCodeGraphStore) MarkSnapshotForDeletion(_ context.Context, _ graphschema.CodeGraphDeleteInput) error {
	return nil
}

// Compile-time check: *UnconfiguredCodeGraphStore satisfies the
// graphstore.Store interface.
var _ graphschema.CodeGraphStore = (*UnconfiguredCodeGraphStore)(nil)
var _ Store = (*UnconfiguredCodeGraphStore)(nil)

// ErrGraphStoreUnavailable marks configured-but-currently-unusable
// graph backends. Queue handlers should retry these errors; they are
// different from the deliberate env-unset no-op store above.
var ErrGraphStoreUnavailable = errors.New("graphstore: unavailable")

// UnavailableCodeGraphStore is returned when graph storage is configured
// but startup could not validate or connect to it. It preserves backend
// process startup for mixed deployments, but graph write tasks fail
// retryably instead of being terminally marked SKIPPED by one unhealthy
// pod.
type UnavailableCodeGraphStore struct {
	Reason string
}

func NewUnavailableStore(reason string) *UnavailableCodeGraphStore {
	return &UnavailableCodeGraphStore{Reason: reason}
}

func (s *UnavailableCodeGraphStore) unavailableError() error {
	return fmt.Errorf("%w: %s", ErrGraphStoreUnavailable, s.Reason)
}

func (s *UnavailableCodeGraphStore) WriteSnapshot(_ context.Context, _ graphschema.CodeGraphSnapshot) (graphschema.CodeGraphWriteResult, error) {
	err := s.unavailableError()
	return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
}

func (s *UnavailableCodeGraphStore) WriteRenderedStatements(_ context.Context, input RenderedStatementWrite) (graphschema.CodeGraphWriteResult, error) {
	if _, _, err := validateRenderedStatements(input); err != nil {
		return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
	}
	err := s.unavailableError()
	return graphschema.CodeGraphWriteResult{Status: graphschema.WriteStatusFailed, ErrorMessage: err.Error()}, err
}

func (s *UnavailableCodeGraphStore) MarkSnapshotForDeletion(_ context.Context, _ graphschema.CodeGraphDeleteInput) error {
	return s.unavailableError()
}

var _ graphschema.CodeGraphStore = (*UnavailableCodeGraphStore)(nil)
var _ Store = (*UnavailableCodeGraphStore)(nil)
