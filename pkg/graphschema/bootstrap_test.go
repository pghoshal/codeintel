package graphschema

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestRenderCreateSpaceStatement_Defaults locks the default
// CREATE SPACE shape. Operators eyeballing the codeintel-graph-init
// log line need this to be a stable diagnostic string.
func TestRenderCreateSpaceStatement_Defaults(t *testing.T) {
	got := renderCreateSpaceStatement(0, 0, 0) // all-zero → defaults
	want := "CREATE SPACE IF NOT EXISTS `codeintel` (partition_num = 10, replica_factor = 1, vid_type = FIXED_STRING(128));"
	if got != want {
		t.Errorf("default CREATE SPACE byte-mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestRenderCreateSpaceStatement_Overrides confirms the operator
// can dial partition / replica / vid_type values. The production
// Helm chart sets replica_factor=3 against an HA cluster.
func TestRenderCreateSpaceStatement_Overrides(t *testing.T) {
	got := renderCreateSpaceStatement(50, 3, 128)
	want := "CREATE SPACE IF NOT EXISTS `codeintel` (partition_num = 50, replica_factor = 3, vid_type = FIXED_STRING(128));"
	if got != want {
		t.Errorf("override CREATE SPACE byte-mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestBootstrap_NilClient locks the sentinel boundary so a
// codeintel-graph-init bug that forgets to wire the client
// surfaces as a clean diagnostic.
func TestBootstrap_NilClient(t *testing.T) {
	err := Bootstrap(context.Background(), nil, BootstrapOptions{})
	if !errors.Is(err, ErrClientNil) {
		t.Errorf("Bootstrap(nil): want ErrClientNil, got %v", err)
	}
}

// TestApplyDefaults locks the zero-overwrite contract: caller
// non-zero values pass through; zero falls to the package
// default.
func TestApplyDefaults(t *testing.T) {
	zero := BootstrapOptions{}.applyDefaults()
	if zero.PartitionNum != DefaultPartitionNum ||
		zero.ReplicaFactor != DefaultReplicaFactor ||
		zero.VidLength != DefaultVidLength ||
		zero.ReadyTimeout != 30*time.Second ||
		zero.ReadyPoll != 1*time.Second {
		t.Errorf("zero defaults: got %+v", zero)
	}

	custom := BootstrapOptions{
		PartitionNum:  100,
		ReplicaFactor: 5,
		VidLength:     128,
		ReadyTimeout:  2 * time.Minute,
		ReadyPoll:     500 * time.Millisecond,
	}.applyDefaults()
	if custom.PartitionNum != 100 ||
		custom.ReplicaFactor != 5 ||
		custom.VidLength != 128 ||
		custom.ReadyTimeout != 2*time.Minute ||
		custom.ReadyPoll != 500*time.Millisecond {
		t.Errorf("custom defaults overridden: got %+v", custom)
	}
}

// TestIsSpaceNotFound locks the retry-gate predicate that
// waitForSpaceReady uses to distinguish "schema still propagating"
// from a real error. A regression here either turns transient
// propagation lag into a hard failure (retry too aggressive) or
// turns a permanent error into an infinite retry (retry too
// lenient).
func TestIsSpaceNotFound(t *testing.T) {
	if isSpaceNotFound(nil) {
		t.Errorf("isSpaceNotFound(nil): want false")
	}
	if !isSpaceNotFound(errors.New("nebulaclient: USE: code=-1005 msg=\"SpaceNotFound: SpaceName `codeintel`\"")) {
		t.Errorf("isSpaceNotFound(real propagation error): want true")
	}
	if isSpaceNotFound(errors.New("connection refused")) {
		t.Errorf("isSpaceNotFound(transport error): want false")
	}
	if isSpaceNotFound(errors.New("PermissionDenied")) {
		t.Errorf("isSpaceNotFound(auth error): want false")
	}
}

// TestStatementKind_Discriminates locks the diagnostic-label
// extractor against each of the three CREATE shapes the
// schema-bootstrap loop emits.
func TestStatementKind_Discriminates(t *testing.T) {
	cases := []struct {
		stmt string
		want string
	}{
		{"CREATE TAG IF NOT EXISTS `foo`();", "CREATE TAG"},
		{"CREATE EDGE IF NOT EXISTS `foo`();", "CREATE EDGE"},
		{"CREATE TAG INDEX IF NOT EXISTS `foo_idx` ON `foo`(`a`);", "CREATE TAG INDEX"},
		{"USE `bar`;", "statement"},
		{"", "statement"},
	}
	for _, tc := range cases {
		t.Run(strings.SplitN(tc.stmt, " ", 4)[0]+"_"+tc.want, func(t *testing.T) {
			if got := statementKind(tc.stmt); got != tc.want {
				t.Errorf("statementKind(%q): got %q, want %q", tc.stmt, got, tc.want)
			}
		})
	}
}
