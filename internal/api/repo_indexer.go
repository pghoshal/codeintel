package api

import (
	"context"
	"errors"
	"fmt"
)

// RepoIndexer is the extension point for scheduling a repo-index
// job (INDEX / CLEANUP / REMOVE_INDEX) when a user requests one
// via the public HTTP API. The default in-process implementation
// fails closed; deployments must wire a real indexer via
// api.Config.RepoIndexer before index/reindex/remove-index can
// return 2xx.
//
// Same shape + rationale as [ConnectionSyncer] — the handler
// stays implementation-agnostic; concrete enqueueing logic
// (Postgres row insert + asynq enqueue) lives in codeintel-backend.
type RepoIndexer interface {
	Schedule(ctx context.Context, req RepoIndexRequest) (RepoIndexResult, error)
}

// RepoIndexJobKind is the typed scheme for the three legacy
// RepoIndexingJobType values. Matches the Postgres enum landed
// in S.1.
type RepoIndexJobKind string

const (
	RepoIndexJobKindIndex       RepoIndexJobKind = "INDEX"
	RepoIndexJobKindCleanup     RepoIndexJobKind = "CLEANUP"
	RepoIndexJobKindRemoveIndex RepoIndexJobKind = "REMOVE_INDEX"
)

// ErrRepoNotFound is the typed sentinel returned when the requested
// repo does not belong to the supplied org. Surfaces as a 404 at the
// HTTP boundary.
var ErrRepoNotFound = errors.New("api: repo not found in org")

// JobAlreadyActiveError is returned when the target repo has a
// RepoIndexingJob row in PENDING or IN_PROGRESS status.
type JobAlreadyActiveError struct {
	JobID  string
	Type   string
	Status string
}

func (e *JobAlreadyActiveError) Error() string {
	return fmt.Sprintf("Repo already has active %s job %s.", e.Type, e.JobID)
}

// Valid reports whether k is one of the three enum values. Used
// at the HTTP boundary to reject malformed payloads.
func (k RepoIndexJobKind) Valid() bool {
	switch k {
	case RepoIndexJobKindIndex, RepoIndexJobKindCleanup, RepoIndexJobKindRemoveIndex:
		return true
	}
	return false
}

// RepoIndexRequest is what the handler hands the indexer.
// OrgID + RepoID identify the tenant + repo; Kind picks the job
// type so a single endpoint can multiplex INDEX / CLEANUP /
// REMOVE_INDEX (mirrors the POST/DELETE split in the legacy
// route).
type RepoIndexRequest struct {
	OrgID  int32
	RepoID int32
	Kind   RepoIndexJobKind
	Ref    string
}

// RepoIndexResult is what the indexer reports back. JobID is the
// row id the route returns to the caller so they can poll status.
// Production HTTP mutation routes require a non-empty JobID on
// success; returning an empty id would make Atom believe an index
// lifecycle action was scheduled when no durable backend work exists.
// AlreadyAtCapacity is reserved for the future per-tenant cap
// (parallels the legacy capacity check; deferred).
type RepoIndexResult struct {
	JobID             string
	AlreadyAtCapacity bool
}

// ErrRepoIndexerUnavailable is returned when the public API is not
// wired to a durable backend scheduler. It is intentionally loud:
// index/reindex/remove-index must never return 2xx without a real
// job id for Atom to poll.
var ErrRepoIndexerUnavailable = errors.New("api: repo indexer is not configured")

// NoopRepoIndexer is the default implementation when the server is
// not wired with a real indexer. It fails closed so production and
// product-flow tests cannot accidentally report a successful index
// lifecycle action without durable backend work.
type NoopRepoIndexer struct{}

// Schedule reports that no durable index scheduler is configured.
func (NoopRepoIndexer) Schedule(_ context.Context, _ RepoIndexRequest) (RepoIndexResult, error) {
	return RepoIndexResult{}, ErrRepoIndexerUnavailable
}
