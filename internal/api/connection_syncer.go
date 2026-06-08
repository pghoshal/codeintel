package api

import "context"

// ConnectionSyncer is the extension point for scheduling a sync
// when a connection is created or updated. The default in-process
// implementation is a no-op (writes succeed, no sync scheduled);
// deployments wire a real syncer (e.g. an HTTP client to an
// indexer service, a Kafka producer, a Temporal worker) by
// implementing this interface and passing it via api.Config.
//
// Implementations must be safe for concurrent use; the handler
// invokes Schedule from the request goroutine.
type ConnectionSyncer interface {
	Schedule(ctx context.Context, req SyncRequest) (SyncResult, error)
}

// SyncRequest describes the work the handler asks the syncer to
// schedule. ConnectionID is the just-upserted row's id;
// OrgID identifies the tenant. The syncer is free to enrich
// the request with additional context loaded out of band.
type SyncRequest struct {
	OrgID        int32
	ConnectionID int32
}

// SyncResult is what the syncer reports back so the handler can
// surface the outcome to the client. JobID is the (opaque)
// identifier of the scheduled work; AlreadyAtCapacity = true is a
// soft-reject signalling the caller should retry once existing
// work drains.
type SyncResult struct {
	JobID             string
	AlreadyAtCapacity bool
}

// NoopConnectionSyncer is the default implementation when the
// server isn't wired with a real syncer. It accepts every request
// without doing any work and returns an empty SyncResult.
//
// Use case: dev / test environments that don't run an indexer.
// Production wires a real syncer at startup.
type NoopConnectionSyncer struct{}

func (NoopConnectionSyncer) Schedule(_ context.Context, _ SyncRequest) (SyncResult, error) {
	return SyncResult{}, nil
}
