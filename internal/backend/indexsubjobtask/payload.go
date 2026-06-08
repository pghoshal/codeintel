// Package indexsubjobtask defines the backend-owned queue payload
// for hot/cold indexing executor subjobs.
package indexsubjobtask

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"codeintel/internal/backend/workerclasses"
)

type Layer string

const (
	LayerClone         Layer = "CLONE"
	LayerZoekt         Layer = "ZOEKT"
	LayerASTTreeSitter Layer = "AST_TREE_SITTER"
	LayerSCIP          Layer = "SCIP"
	LayerGraphMerge    Layer = "GRAPH_MERGE"
	LayerActivate      Layer = "ACTIVATE"
	LayerRemove        Layer = "REMOVE"
)

func (l Layer) Valid() bool {
	switch l {
	case LayerClone, LayerZoekt, LayerASTTreeSitter, LayerSCIP, LayerGraphMerge, LayerActivate, LayerRemove:
		return true
	default:
		return false
	}
}

// Payload is the JSON body placed on the class-specific asynq
// queues. The row in Postgres remains the source of truth; this
// payload carries the immutable scope so stateless pods can log,
// validate, and reject tenant drift before claiming work.
type Payload struct {
	SubjobID          string  `json:"subjobId"`
	RepoIndexingJobID string  `json:"repoIndexingJobId"`
	OrgID             int32   `json:"orgId"`
	WorkspaceID       *string `json:"workspaceId,omitempty"`
	RepoID            int32   `json:"repoId"`
	Branch            string  `json:"branch"`
	Revision          string  `json:"revision"`
	CommitHash        string  `json:"commitHash"`
	Layer             Layer   `json:"layer"`
	Language          *string `json:"language,omitempty"`
	ProjectRoot       *string `json:"projectRoot,omitempty"`
	Indexer           *string `json:"indexer,omitempty"`
	WorkerClass       string  `json:"workerClass"`
	QueueName         string  `json:"queueName"`
	Attempt           int32   `json:"attempt"`
}

var ErrInvalidPayload = errors.New("indexsubjobtask: invalid payload")

var commitHashPattern = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func (p Payload) Validate() error {
	if p.SubjobID == "" || p.RepoIndexingJobID == "" || p.OrgID <= 0 || p.RepoID <= 0 ||
		p.WorkspaceID == nil || *p.WorkspaceID == "" ||
		p.Branch == "" || p.Revision == "" || p.CommitHash == "" ||
		!p.Layer.Valid() || p.WorkerClass == "" || p.QueueName == "" {
		return ErrInvalidPayload
	}
	if !commitHashPattern.MatchString(p.CommitHash) {
		return ErrInvalidPayload
	}
	class, ok := workerclasses.ByName(p.WorkerClass)
	if !ok || class.QueueName != p.QueueName {
		return ErrInvalidPayload
	}
	if p.Layer == LayerSCIP {
		if p.Language == nil || *p.Language == "" ||
			p.ProjectRoot == nil || p.Indexer == nil || *p.Indexer == "" {
			return ErrInvalidPayload
		}
	} else if p.Language != nil || p.ProjectRoot != nil || p.Indexer != nil {
		return ErrInvalidPayload
	}
	return nil
}

func Marshal(p Payload) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

func Unmarshal(raw []byte) (Payload, error) {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return Payload{}, err
	}
	if err := p.Validate(); err != nil {
		return Payload{}, fmt.Errorf("%w after unmarshal", err)
	}
	return p, nil
}
