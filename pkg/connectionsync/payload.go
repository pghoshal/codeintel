// Package connectionsync holds the cross-binary wire types for
// the connection-sync queue. Producer (codeintel-app's
// POST /api/connections/{id}/sync handler) marshals these;
// consumer (codeintel-backend's connectionmanager.Handler)
// unmarshals.
//
// Lives under pkg/ rather than internal/ because both binaries
// share the type. Drift between producer + consumer would be a
// silent serialisation break; centralising here makes the
// wire contract a single source of truth.
package connectionsync

import "encoding/json"

// TaskPayload is the asynq.Task.Payload shape for every job on
// the connection-sync-queue. Field names mirror the legacy
// JobPayload (connectionManager.ts:47-52) byte-for-byte so a
// logger that grepped legacy JSON payloads finds the same keys
// in codeintel.
type TaskPayload struct {
	// JobID is the uuid of the ConnectionSyncJob row tracking
	// this job's lifecycle. The producer MUST have inserted
	// the row in PENDING status before enqueueing.
	JobID string `json:"jobId"`

	// ConnectionID is the Connection row id the worker should
	// sync.
	ConnectionID int32 `json:"connectionId"`

	// ConnectionName is the human-readable Connection.name
	// (legacy uses this for posthog events + logs; kept for
	// log-fingerprint parity).
	ConnectionName string `json:"connectionName"`

	// OrgID is the Connection.orgId. Carried in the payload so
	// the worker can scope every DB write without an extra
	// lookup + so the tenant-scoping invariant is enforceable
	// at the handler-entry boundary.
	OrgID int32 `json:"orgId"`
}

// Marshal returns the bytes that go into asynq.NewTask's
// payload arg. Errors surface only on a programmer bug — the
// payload struct is plain data with no marshaler-blocking
// types.
func Marshal(p TaskPayload) ([]byte, error) {
	return json.Marshal(p)
}

// Unmarshal decodes the asynq task payload bytes. Used by the
// worker handler.
func Unmarshal(b []byte) (TaskPayload, error) {
	var p TaskPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return TaskPayload{}, err
	}
	return p, nil
}
