package schedulerapi

type RepoIndexRequest struct {
	OrgID  int32  `json:"orgId"`
	RepoID int32  `json:"repoId"`
	Kind   string `json:"kind"`
	Ref    string `json:"ref,omitempty"`
}

type RepoIndexResponse struct {
	JobID             string `json:"jobId,omitempty"`
	AlreadyAtCapacity bool   `json:"alreadyAtCapacity,omitempty"`
}

type ConnectionSyncRequest struct {
	OrgID        int32 `json:"orgId"`
	ConnectionID int32 `json:"connectionId"`
}

type ConnectionSyncResponse struct {
	JobID             string `json:"jobId,omitempty"`
	AlreadyAtCapacity bool   `json:"alreadyAtCapacity,omitempty"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}
