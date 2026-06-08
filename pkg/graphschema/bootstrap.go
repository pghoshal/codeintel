package graphschema

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"codeintel/pkg/nebulaclient"
)

// SpaceName is the single graph space the codeintel deployment
// writes / reads. Production deployments often configure this
// out-of-band via the operator's Helm bootstrap; codeintel-graph-init
// creates it explicitly so the dev compose-up flow is fully
// automated and the production Helm Job is a single deployment
// concern.
const SpaceName = "codeintel"

// Default space sizing. VidType FIXED_STRING(128) holds the scoped
// code graph VID shape:
// cg:o<org>:w<workspaceHash>:r<repo>:c<commit>:s<schema>:b<builderHash>:<kind>:<keyHash>.
// Partition count of 10 matches
// the Nebula sizing guidance for small graphs (re-balance cheap
// below ~1B vertices); the single-node dev compose uses
// replica_factor=1.
const (
	DefaultVidLength      = 128
	DefaultPartitionNum   = 10
	DefaultReplicaFactor  = 1
)

// renderCreateSpaceStatement assembles the CREATE SPACE statement.
// codeintel-specific (no port equivalent — the TS source assumes
// a pre-existing space); the function lives in this file rather
// than nebula_ngql.go so the line-by-line port test in
// nebula_ngql_test.go stays focused on the ported surface.
//
// Each parameter has a non-zero floor: zero or negative values
// fall back to the package defaults so the codeintel-graph-init
// binary can pass a partially-filled struct.
func renderCreateSpaceStatement(partitions, replicaFactor, vidLen int) string {
	if partitions <= 0 {
		partitions = DefaultPartitionNum
	}
	if replicaFactor <= 0 {
		replicaFactor = DefaultReplicaFactor
	}
	if vidLen <= 0 {
		vidLen = DefaultVidLength
	}
	return fmt.Sprintf(
		"CREATE SPACE IF NOT EXISTS %s (partition_num = %d, replica_factor = %d, vid_type = FIXED_STRING(%d));",
		quoteIdentifier(SpaceName), partitions, replicaFactor, vidLen,
	)
}

// BootstrapOptions is the argument bag for Bootstrap. Each field
// is optional; zero falls back to the package default. The
// codeintel-graph-init binary populates this from operator env
// vars; tests construct it directly.
type BootstrapOptions struct {
	// PartitionNum bounds the Nebula partition count for the
	// space. Operator-tunable per cluster size.
	PartitionNum int

	// ReplicaFactor is the storaged replication factor. Single-
	// node dev cluster runs 1; production HA cluster runs 3 or 5.
	ReplicaFactor int

	// VidLength is the FIXED_STRING width for vertex ids. 64 fits
	// every id shape the indexer emits — do not lower without
	// auditing every writer.
	VidLength int

	// ReadyTimeout bounds the post-CREATE-SPACE poll for storaged
	// to pick up the new space metadata. Nebula propagates schema
	// asynchronously; a too-eager USE returns "SpaceNotFound".
	ReadyTimeout time.Duration

	// ReadyPoll is the per-attempt sleep between SHOW SPACES
	// probes during readiness wait.
	ReadyPoll time.Duration
}

// applyDefaults fills zero-valued fields without overwriting
// operator-supplied values.
func (o BootstrapOptions) applyDefaults() BootstrapOptions {
	if o.PartitionNum <= 0 {
		o.PartitionNum = DefaultPartitionNum
	}
	if o.ReplicaFactor <= 0 {
		o.ReplicaFactor = DefaultReplicaFactor
	}
	if o.VidLength <= 0 {
		o.VidLength = DefaultVidLength
	}
	if o.ReadyTimeout <= 0 {
		o.ReadyTimeout = 30 * time.Second
	}
	if o.ReadyPoll <= 0 {
		o.ReadyPoll = 1 * time.Second
	}
	return o
}

// ErrClientNil is returned when Bootstrap receives a nil client.
// Surfaced as a sentinel so the codeintel-graph-init binary can
// pin the diagnostic before reaching for cluster credentials.
var ErrClientNil = errors.New("graphschema: client is nil")

// Bootstrap creates (idempotently) the codeintel SPACE plus every
// tag, edge, and tag-index under it. Sequence:
//
//  1. CREATE SPACE IF NOT EXISTS (codeintel-specific; not in the
//     port surface of nebulaNgql.ts).
//  2. Poll SHOW SPACES until SpaceName appears (Nebula propagates
//     schema async; subsequent USE / CREATE TAG calls fail until
//     storaged has picked it up).
//  3. USE the space.
//  4. Walk RenderSchemaStatements() — the parity-locked set of
//     CREATE TAG / CREATE EDGE / CREATE TAG INDEX statements —
//     and issue one Execute per.
//
// On re-run every CREATE is a no-op (IF NOT EXISTS) and Bootstrap
// returns nil. The supplied client's per-call Timeout governs
// each individual statement; the ctx deadline bounds the overall
// sequence including the readiness poll.
func Bootstrap(ctx context.Context, client *nebulaclient.Client, opts BootstrapOptions) error {
	if client == nil {
		return ErrClientNil
	}
	opts = opts.applyDefaults()

	if _, err := client.Execute(ctx, renderCreateSpaceStatement(opts.PartitionNum, opts.ReplicaFactor, opts.VidLength)); err != nil {
		return fmt.Errorf("graphschema: CREATE SPACE: %w", err)
	}

	if err := waitForSpaceReady(ctx, client, opts.ReadyTimeout, opts.ReadyPoll); err != nil {
		return err
	}

	// Each schema-create statement is dispatched as its own
	// Execute call. The nebulaclient pool checks out a fresh
	// session per call, so the USE applied during the readiness
	// probe doesn't persist; we prepend USE to every statement so
	// the batch runs inside the codeintel space irrespective of
	// which pooled session services the call.
	useStmt := "USE " + quoteIdentifier(SpaceName) + "; "
	for _, stmt := range RenderSchemaStatements() {
		if _, err := client.Execute(ctx, useStmt+stmt); err != nil {
			return fmt.Errorf("graphschema: %s: %w", statementKind(stmt), err)
		}
	}
	return nil
}

// waitForSpaceReady polls by attempting USE <space> until it
// succeeds or the timeout / context deadline expires. Nebula
// propagates schema creates asynchronously from the metad write
// to the storaged caches AND across the metad raft group — a
// SHOW SPACES on the metad leader returns the name before every
// follower has caught up, so polling SHOW SPACES can return
// "ready" while a subsequent USE still hits SpaceNotFound.
// Probing with the operation we actually intend to perform
// (USE) is the operational gate.
//
// USE succeeds → session retains the space context, so the
// CREATE TAG / CREATE EDGE statements that follow run inside
// the right namespace without a second USE.
func waitForSpaceReady(ctx context.Context, client *nebulaclient.Client, timeout, poll time.Duration) error {
	useStmt := "USE " + quoteIdentifier(SpaceName) + ";"
	deadline := time.Now().Add(timeout)
	for {
		_, err := client.Execute(ctx, useStmt)
		if err == nil {
			return nil
		}
		// USE on an unpropagated space returns
		// "SpaceNotFound: SpaceName `<name>`". That string is the
		// expected retry case; anything else is a real failure and
		// surfaces immediately.
		if !isSpaceNotFound(err) {
			return fmt.Errorf("graphschema: USE %s probe: %w", SpaceName, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("graphschema: space %q did not become USE-able within %s: %w", SpaceName, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// isSpaceNotFound matches the nGQL error surface for "the space
// hasn't propagated to this node yet". Nebula returns error code
// -1005 with a "SpaceNotFound" prefix; the wrapper string-matches
// on the prefix rather than the code so a Nebula minor-version
// change that re-orders error codes doesn't silently break the
// readiness loop.
func isSpaceNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SpaceNotFound")
}

// statementKind extracts a short diagnostic label from a CREATE
// statement so errors can say "CREATE TAG INDEX" rather than the
// full 446-char statement. Used only for error wrapping.
func statementKind(stmt string) string {
	fields := strings.Fields(stmt)
	if len(fields) >= 3 && strings.EqualFold(fields[0], "CREATE") {
		// "CREATE TAG INDEX ..." → "CREATE TAG INDEX"
		// "CREATE TAG ..."       → "CREATE TAG"
		// "CREATE EDGE ..."      → "CREATE EDGE"
		if strings.EqualFold(fields[2], "INDEX") {
			return strings.Join(fields[0:3], " ")
		}
		return strings.Join(fields[0:2], " ")
	}
	return "statement"
}
