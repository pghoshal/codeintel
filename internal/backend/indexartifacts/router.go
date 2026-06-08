// Package indexartifacts routes split-executor artifacts to the
// layer-specific backend persistence path.
package indexartifacts

import (
	"context"
	"errors"
	"fmt"

	"codeintel/internal/backend/indexexecutor"
	"codeintel/internal/backend/indexsubjobtask"
)

type Ingestor interface {
	Ingest(context.Context, indexsubjobtask.Payload, indexexecutor.Result, string, string) error
}

type Router struct {
	byLayer map[indexsubjobtask.Layer]Ingestor
}

func NewRouter(routes map[indexsubjobtask.Layer]Ingestor) *Router {
	copied := make(map[indexsubjobtask.Layer]Ingestor, len(routes))
	for layer, ingestor := range routes {
		if ingestor != nil {
			copied[layer] = ingestor
		}
	}
	return &Router{byLayer: copied}
}

func (r *Router) Ingest(ctx context.Context, payload indexsubjobtask.Payload, result indexexecutor.Result, leaseOwner, attemptID string) error {
	if r == nil || len(r.byLayer) == 0 {
		return errors.New("indexartifacts: no artifact routes configured")
	}
	ingestor, ok := r.byLayer[payload.Layer]
	if !ok || ingestor == nil {
		return fmt.Errorf("indexartifacts: unsupported artifact layer %s", payload.Layer)
	}
	return ingestor.Ingest(ctx, payload, result, leaseOwner, attemptID)
}
