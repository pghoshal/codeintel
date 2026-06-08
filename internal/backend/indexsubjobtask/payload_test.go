package indexsubjobtask

import (
	"errors"
	"strings"
	"testing"
)

func TestPayloadRoundTrip(t *testing.T) {
	workspaceID := "atom-ws-1"
	language := "go"
	projectRoot := "services/api"
	indexer := "scip-go"
	payload := Payload{
		SubjobID:          "subjob-1",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             LayerSCIP,
		Language:          &language,
		ProjectRoot:       &projectRoot,
		Indexer:           &indexer,
		WorkerClass:       "scip-go",
		QueueName:         "codeintel-index-scip-go",
		Attempt:           1,
	}
	raw, err := Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"subjobId"`, `"repoIndexingJobId"`, `"orgId"`, `"commitHash"`, `"workerClass"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("payload JSON missing %s: %s", key, raw)
		}
	}
	got, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SubjobID != payload.SubjobID || got.Layer != LayerSCIP || got.Language == nil || *got.Language != language {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestPayloadRejectsInvalid(t *testing.T) {
	if _, err := Marshal(Payload{}); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Marshal empty err = %v want ErrInvalidPayload", err)
	}
	workspaceID := "atom-ws-1"
	language := "go"
	payload := Payload{
		SubjobID:          "subjob-1",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             LayerSCIP,
		Language:          &language,
		WorkerClass:       "scip-go",
		QueueName:         "codeintel-index-scip-go",
	}
	if _, err := Marshal(payload); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Marshal SCIP missing project/indexer err = %v want ErrInvalidPayload", err)
	}
	projectRoot := ""
	indexer := "scip-go"
	payload.ProjectRoot = &projectRoot
	payload.Indexer = &indexer
	payload.QueueName = "codeintel-index-scip-jvm"
	if _, err := Marshal(payload); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Marshal class/queue mismatch err = %v want ErrInvalidPayload", err)
	}
	payload.QueueName = "codeintel-index-scip-go"
	payload.CommitHash = "abc123"
	if _, err := Marshal(payload); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Marshal short commit err = %v want ErrInvalidPayload", err)
	}
	payload.CommitHash = strings.Repeat("a", 40)
	payload.Layer = LayerASTTreeSitter
	payload.WorkerClass = "core"
	payload.QueueName = "codeintel-index-core"
	if _, err := Marshal(payload); !errors.Is(err, ErrInvalidPayload) {
		t.Fatalf("Marshal non-SCIP with semantic fields err = %v want ErrInvalidPayload", err)
	}
}

func TestPayloadRejectsMissingScopeFields(t *testing.T) {
	workspaceID := "atom-ws-1"
	language := "go"
	projectRoot := ""
	indexer := "scip-go"
	valid := Payload{
		SubjobID:          "subjob-1",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       &workspaceID,
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        strings.Repeat("a", 40),
		Layer:             LayerSCIP,
		Language:          &language,
		ProjectRoot:       &projectRoot,
		Indexer:           &indexer,
		WorkerClass:       "scip-go",
		QueueName:         "codeintel-index-scip-go",
		Attempt:           1,
	}
	cases := []struct {
		name string
		edit func(*Payload)
	}{
		{"org", func(p *Payload) { p.OrgID = 0 }},
		{"workspace", func(p *Payload) { p.WorkspaceID = nil }},
		{"repo", func(p *Payload) { p.RepoID = 0 }},
		{"branch", func(p *Payload) { p.Branch = "" }},
		{"revision", func(p *Payload) { p.Revision = "" }},
		{"commit", func(p *Payload) { p.CommitHash = "" }},
		{"layer", func(p *Payload) { p.Layer = "BAD" }},
		{"worker", func(p *Payload) { p.WorkerClass = "" }},
		{"queue", func(p *Payload) { p.QueueName = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := valid
			tc.edit(&p)
			if _, err := Marshal(p); !errors.Is(err, ErrInvalidPayload) {
				t.Fatalf("Marshal err = %v want ErrInvalidPayload", err)
			}
		})
	}
}
