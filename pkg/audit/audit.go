// Package audit defines the compliance-reporting extension point.
// Every mutating handler fires a structured Event through the
// configured Emitter; deployments wire a real backend (Postgres
// audit table, SIEM forwarder, event bus) by implementing the
// Emitter interface, while dev / test deployments keep the
// NoopEmitter default.
//
// The package is deliberately tiny — types and an interface, no
// I/O. The serialisation / persistence concerns belong to the
// implementations.
package audit

import (
	"context"
	"time"
)

// ActorType discriminates the subject performing an audited
// action. Adding kinds is a backwards-compatible change: emitters
// that switch on the value treat unknown kinds as the literal
// string.
type ActorType string

const (
	ActorUser   ActorType = "user"
	ActorApiKey ActorType = "api_key"
	ActorSystem ActorType = "system"
)

// TargetType discriminates the subject being acted upon.
type TargetType string

const (
	TargetOrg        TargetType = "org"
	TargetConnection TargetType = "connection"
	TargetSecret     TargetType = "secret"
	TargetModel      TargetType = "model"
	TargetRepo       TargetType = "repo"
)

// Event is the structured payload an Emitter persists. Time
// stamping is left to the Emitter so a batched implementation can
// pick its own clock (e.g. ingestion timestamp vs. event-emit
// timestamp). Action is a free-form verb-noun string like
// "connection.created", "secret.deleted", "model.replaced".
type Event struct {
	Action     string
	ActorID    string
	ActorType  ActorType
	TargetID   string
	TargetType TargetType
	OrgID      int32
	RequestID  string
	Time       time.Time
	Metadata   map[string]any
}

// Emitter persists an Event. Implementations MUST be safe for
// concurrent use; the handler calls Emit from the request
// goroutine. Implementations SHOULD NOT block the request beyond
// what an in-memory enqueue requires — a slow audit backend can
// be wrapped in a buffered Emitter that returns quickly and
// drains asynchronously.
//
// An error from Emit propagates to the handler; whether the
// handler treats that as a 500 or swallows it (and logs) is an
// implementation choice per route.
type Emitter interface {
	Emit(ctx context.Context, event Event) error
}

// NoopEmitter is the safe default when no real emitter is wired.
// Every Emit call is a no-op and returns nil. The fallback keeps
// handlers free from nil-checks.
type NoopEmitter struct{}

func (NoopEmitter) Emit(_ context.Context, _ Event) error { return nil }
