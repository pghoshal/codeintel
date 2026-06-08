package graphstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"codeintel/pkg/graphschema"
)

// TestUnconfiguredCodeGraphStore_WriteSnapshot_SKIPPED locks
// the no-op return shape: status=SKIPPED, all counts zero,
// ErrorMessage carries the operator reason + the offending
// snapshot's identity (repoId + 12-char commit prefix).
func TestUnconfiguredCodeGraphStore_WriteSnapshot_SKIPPED(t *testing.T) {
	store := NewUnconfiguredStore("CODEINTEL_NEBULA_ADDR is not configured")
	result, err := store.WriteSnapshot(context.Background(), graphschema.CodeGraphSnapshot{
		RepoID:     42,
		CommitHash: "deadbeefcafe1234",
		Vertices:   []graphschema.CodeGraphVertex{{VID: "ignored"}},
		Edges:      []graphschema.CodeGraphEdge{{FromVID: "ignored"}},
	})
	if err != nil {
		t.Fatalf("WriteSnapshot returned error: %v", err)
	}
	if result.Status != graphschema.WriteStatusSkipped {
		t.Errorf("Status: got %q, want SKIPPED", result.Status)
	}
	if result.VertexCount != 0 || result.EdgeCount != 0 ||
		result.AnchorCount != 0 || result.LinkedEdgeCount != 0 {
		t.Errorf("all counts should be zero on SKIPPED: %+v", result)
	}
	if !strings.Contains(result.ErrorMessage, "CODEINTEL_NEBULA_ADDR is not configured") {
		t.Errorf("ErrorMessage missing reason: %s", result.ErrorMessage)
	}
	if !strings.Contains(result.ErrorMessage, "42@deadbeefcafe") {
		t.Errorf("ErrorMessage missing snapshot identity: %s", result.ErrorMessage)
	}
}

// TestUnconfiguredCodeGraphStore_MarkSnapshotForDeletion_Noop
// locks the silent-success contract: deletion against an
// unconfigured store is a no-op, no error.
func TestUnconfiguredCodeGraphStore_MarkSnapshotForDeletion_Noop(t *testing.T) {
	store := NewUnconfiguredStore("any reason")
	if err := store.MarkSnapshotForDeletion(context.Background(), graphschema.CodeGraphDeleteInput{
		OrgID: 1, WorkspaceID: "ws", RepoID: 1, CommitHash: "x", SchemaVersion: 1, BuilderVersion: "v",
	}); err != nil {
		t.Errorf("MarkSnapshotForDeletion: got %v, want nil", err)
	}
}

// TestCreateFromEnv_UnsetAddrReturnsUnconfigured locks the
// "graph backend not configured" path. The factory MUST NOT
// fail the caller — it returns a working no-op store + no-op
// closer.
func TestCreateFromEnv_UnsetAddrReturnsUnconfigured(t *testing.T) {
	// Clear every CODEINTEL_NEBULA_* env so LoadConfigFromEnv
	// won't accidentally pick up a value from the parent shell.
	t.Setenv("CODEINTEL_NEBULA_ADDR", "")
	t.Setenv("CODEINTEL_NEBULA_USER", "")
	t.Setenv("CODEINTEL_NEBULA_PASSWORD", "")

	store, closer := CreateFromEnv(context.Background(), nil)
	if closer == nil {
		t.Fatalf("Closer should always be non-nil")
	}
	defer closer.Close()
	if _, ok := store.(*UnconfiguredCodeGraphStore); !ok {
		t.Errorf("expected *UnconfiguredCodeGraphStore when addr unset, got %T", store)
	}
}

// TestCreateFromEnv_InvalidConfigReturnsUnavailable locks the
// "addr set but malformed" recovery path. A bad env value
// surfaces as an UnavailableCodeGraphStore wrapping the typed
// error, so graph writes retry instead of being terminally skipped.
func TestCreateFromEnv_InvalidConfigReturnsUnavailable(t *testing.T) {
	t.Setenv("CODEINTEL_NEBULA_ADDR", "not-a-valid-host-port-pair")
	t.Setenv("CODEINTEL_NEBULA_USER", "root")
	t.Setenv("CODEINTEL_NEBULA_PASSWORD", "nebula")

	store, closer := CreateFromEnv(context.Background(), nil)
	defer closer.Close()
	un, ok := store.(*UnavailableCodeGraphStore)
	if !ok {
		t.Fatalf("expected *UnavailableCodeGraphStore, got %T", store)
	}
	if !strings.Contains(un.Reason, "env validation failed") {
		t.Errorf("Reason should identify env-validation failure: %s", un.Reason)
	}
}

func TestUnavailableCodeGraphStore_WriteSnapshot_RetryableFailure(t *testing.T) {
	store := NewUnavailableStore("nebula client init failed")
	result, err := store.WriteSnapshot(context.Background(), graphschema.CodeGraphSnapshot{})
	if !errors.Is(err, ErrGraphStoreUnavailable) {
		t.Fatalf("got %v, want ErrGraphStoreUnavailable", err)
	}
	if result.Status != graphschema.WriteStatusFailed {
		t.Errorf("Status: got %q, want FAILED", result.Status)
	}
}

// TestNoopCloser_Idempotent confirms the noop close can be
// called multiple times without error — defer + extra close
// stays safe.
func TestNoopCloser_Idempotent(t *testing.T) {
	c := noopCloser{}
	for i := 0; i < 3; i++ {
		if err := c.Close(); err != nil {
			t.Errorf("Close call %d: got %v, want nil", i, err)
		}
	}
}
