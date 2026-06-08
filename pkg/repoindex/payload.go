// Package repoindex holds the cross-binary wire types for the
// repo-index queue. Producer (codeintel-app's connection-sync
// completion handler, or future POST /api/connections/{id}/index)
// marshals; consumer (codeintel-backend's
// repoindexmanager.Handler) unmarshals.
//
// Lives under pkg/ so producer + consumer share the same wire
// contract. Same shape as pkg/connectionsync — no functional
// difference, just a different queue.
package repoindex

import (
	"encoding/json"
	"fmt"
)

// JobType mirrors the legacy RepoIndexingJobType enum:
// INDEX / CLEANUP / REMOVE_INDEX.
type JobType string

const (
	JobTypeIndex       JobType = "INDEX"
	JobTypeCleanup     JobType = "CLEANUP"
	JobTypeRemoveIndex JobType = "REMOVE_INDEX"
)

// Valid reports whether t is one of the three legacy enum
// values. Used at the payload-decode boundary so a corrupted
// task surfaces a typed error before the handler runs.
func (t JobType) Valid() bool {
	switch t {
	case JobTypeIndex, JobTypeCleanup, JobTypeRemoveIndex:
		return true
	}
	return false
}

// TaskPayload is the asynq.Task.Payload shape for every job on
// the repo-index-queue. Field names mirror the legacy JobPayload
// (repoIndexManager.ts:26-31) byte-for-byte so log fingerprints
// stay aligned with the legacy pipeline.
type TaskPayload struct {
	// Type is the job kind. The handler dispatches on this.
	Type JobType `json:"type"`

	// JobID is the uuid of the RepoIndexingJob row tracking the
	// lifecycle. Caller MUST have inserted the row in PENDING
	// status before enqueueing.
	JobID string `json:"jobId"`

	// RepoID is the Repo row id the worker should operate on.
	RepoID int32 `json:"repoId"`

	// OrgID is the tenant boundary for the job. Backend workers
	// must carry it through every cleanup/index write.
	OrgID int32 `json:"orgId"`

	// RepoName is the human-readable Repo.name (legacy uses it
	// in posthog events + logs; preserved for log-fingerprint
	// parity).
	RepoName string `json:"repoName"`

	// Ref is the optional branch/tag/ref scope for the lifecycle
	// action. Empty preserves legacy repo-wide behavior. REMOVE_INDEX
	// callers use this to remove one indexed branch without deleting
	// sibling branch indexes for the same repo.
	Ref string `json:"ref,omitempty"`
}

// Marshal serialises the payload for asynq.NewTask.
func Marshal(p TaskPayload) ([]byte, error) {
	if !p.Type.Valid() {
		return nil, fmt.Errorf("repoindex: invalid JobType %q", p.Type)
	}
	if p.OrgID <= 0 {
		return nil, fmt.Errorf("repoindex: orgId is required")
	}
	return json.Marshal(p)
}

// Unmarshal decodes an asynq task payload. Validates Type and
// orgId at the boundary so new producers cannot emit unscoped
// work.
func Unmarshal(b []byte) (TaskPayload, error) {
	return unmarshal(b, false)
}

// UnmarshalLegacyForBackfill decodes old Redis/asynq tasks that
// were already queued before org-scoped payloads shipped. The
// backend handler must immediately backfill orgId from the
// scoped RepoIndexingJob row before doing any state transition or
// cleanup.
func UnmarshalLegacyForBackfill(b []byte) (TaskPayload, error) {
	return unmarshal(b, true)
}

func unmarshal(b []byte, allowMissingOrgID bool) (TaskPayload, error) {
	var p TaskPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return TaskPayload{}, err
	}
	if !p.Type.Valid() {
		return TaskPayload{}, fmt.Errorf("repoindex: invalid JobType %q after unmarshal", p.Type)
	}
	if p.OrgID <= 0 && !allowMissingOrgID {
		return TaskPayload{}, fmt.Errorf("repoindex: orgId is required after unmarshal")
	}
	return p, nil
}
