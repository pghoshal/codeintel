// Package analytics defines the product-telemetry extension point.
// Handlers fire a structured Event through the configured Emitter;
// deployments wire a real backend (hosted telemetry SaaS, in-house
// data pipeline) by implementing the Emitter interface, while dev
// and test deployments keep the NoopEmitter default.
//
// The package is intentionally minimal — types and an interface,
// no I/O. Serialisation, batching, and transport are
// implementation concerns. Emitter implementations also own:
//
//   - DistinctID rewriting: an Emitter MAY override the caller-
//     supplied DistinctID with a server-resolved install id so a
//     misuse at the call site cannot leak per-user identifiers.
//   - Property augmentation: an Emitter MAY inject standard
//     properties (codeintel version, install id, build metadata)
//     into every Event's Properties map. This belongs in the
//     transport, not at call sites — callers focus on the event-
//     specific payload.
package analytics

import (
	"context"
	"time"
)

// Event is the structured payload an Emitter forwards. Time
// stamping is left to the Emitter so a batched implementation can
// pick its own clock (e.g. ingestion vs. event-emit timestamps).
//
// Name is a free-form verb-noun string like "search.executed",
// "connection.synced", "model.replaced". DistinctID identifies the
// emitter — typically a per-install UUID for self-hosted
// deployments, never user PII unless the deployment explicitly
// opts in. Properties carries event-specific context; the map MUST
// be safe to share (callers SHOULD NOT mutate it after Capture).
type Event struct {
	Name       string
	DistinctID string
	Properties map[string]any
	Time       time.Time
}

// Emitter forwards an Event to a telemetry backend. Implementations
// MUST be safe for concurrent use. They SHOULD NOT block the
// request beyond what an in-memory enqueue requires — slow
// backends belong behind a buffered Emitter that returns quickly
// and drains asynchronously.
//
// Shutdown flushes any buffered events. Callers MUST invoke it
// from a graceful-shutdown hook so in-flight events are not
// dropped at process exit.
type Emitter interface {
	Capture(ctx context.Context, event Event) error
	Shutdown(ctx context.Context) error
}

// NoopEmitter is the safe default when no real emitter is wired.
// Every Capture call is a no-op; Shutdown is a no-op. The
// fallback keeps handlers free from nil-checks.
type NoopEmitter struct{}

func (NoopEmitter) Capture(_ context.Context, _ Event) error { return nil }
func (NoopEmitter) Shutdown(_ context.Context) error         { return nil }
