package indexplanner

import (
	"context"
	"errors"
	"testing"

	"codeintel/internal/backend/indexsubjobs"
)

func TestBuildCreatesBranchSpecificSubjobs(t *testing.T) {
	workspaceID := "atom-ws-1"
	in := Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		MaxAttempts:       5,
		Revisions: []Revision{
			{
				WorkspaceID:      &workspaceID,
				Branch:           "refs/heads/main",
				Revision:         "refs/heads/main",
				CommitHash:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				RunZoekt:         true,
				RunASTTreeSitter: true,
				RunGraphMerge:    true,
				RunActivate:      true,
				SCIPProjects:     []SCIPProject{{Language: "go", ProjectRoot: "", Indexer: "scip-go", SCIPWorkerClass: "go"}},
			},
			{
				WorkspaceID:      &workspaceID,
				Branch:           "refs/heads/release",
				Revision:         "refs/heads/release",
				CommitHash:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				RunZoekt:         true,
				RunASTTreeSitter: true,
				RunGraphMerge:    true,
				RunActivate:      true,
				SCIPProjects:     []SCIPProject{{Language: "go", ProjectRoot: "", Indexer: "scip-go", SCIPWorkerClass: "go"}},
			},
		},
	}

	got, err := Build(in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 10 {
		t.Fatalf("subjob count = %d want 10", len(got))
	}
	seenIDs := map[string]struct{}{}
	var mainSCIP, releaseSCIP *indexsubjobs.CreateInput
	for i := range got {
		if got[i].RepoIndexingJobID != "job-1" || got[i].OrgID != 7 || got[i].RepoID != 42 {
			t.Fatalf("scope drift in %+v", got[i])
		}
		if got[i].WorkspaceID == nil || *got[i].WorkspaceID != workspaceID {
			t.Fatalf("workspace drift in %+v", got[i])
		}
		if got[i].MaxAttempts != 5 {
			t.Fatalf("max attempts = %d want 5", got[i].MaxAttempts)
		}
		if _, ok := seenIDs[got[i].ID]; ok {
			t.Fatalf("duplicate deterministic subjob id %q", got[i].ID)
		}
		seenIDs[got[i].ID] = struct{}{}
		if got[i].Layer == indexsubjobs.LayerSCIP && got[i].Branch == "refs/heads/main" {
			mainSCIP = &got[i]
		}
		if got[i].Layer == indexsubjobs.LayerSCIP && got[i].Branch == "refs/heads/release" {
			releaseSCIP = &got[i]
		}
	}
	if mainSCIP == nil || releaseSCIP == nil {
		t.Fatalf("missing branch-specific SCIP rows: main=%v release=%v", mainSCIP, releaseSCIP)
	}
	if mainSCIP.ID == releaseSCIP.ID {
		t.Fatalf("branch-specific SCIP rows collapsed to the same id: %s", mainSCIP.ID)
	}
	if mainSCIP.WorkerClass != "scip-go" || mainSCIP.QueueName != "codeintel-index-scip-go" {
		t.Fatalf("go SCIP worker mapping wrong: %+v", *mainSCIP)
	}
}

func TestBuildRejectsUnknownSCIPWorkerClass(t *testing.T) {
	workspaceID := "atom-ws-1"
	_, err := Build(Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		Revisions: []Revision{{
			WorkspaceID:  &workspaceID,
			Branch:       "refs/heads/main",
			Revision:     "refs/heads/main",
			CommitHash:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SCIPProjects: []SCIPProject{{Language: "go", ProjectRoot: "", Indexer: "scip-go", SCIPWorkerClass: "missing"}},
		}},
	})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("err = %v want ErrInvalidPlan", err)
	}
}

func TestPlanAndPersistIsDeterministic(t *testing.T) {
	store := &recordingStore{}
	workspaceID := "atom-ws-1"
	in := Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		Revisions: []Revision{{
			WorkspaceID:  &workspaceID,
			Branch:       "refs/heads/main",
			Revision:     "refs/heads/main",
			CommitHash:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SCIPProjects: []SCIPProject{{Language: "python", ProjectRoot: "services/api", Indexer: "scip-python", SCIPWorkerClass: "python"}},
		}},
	}
	first, err := PlanAndPersist(context.Background(), store, in)
	if err != nil {
		t.Fatalf("PlanAndPersist first: %v", err)
	}
	second, err := Build(in)
	if err != nil {
		t.Fatalf("Build second: %v", err)
	}
	if len(first) != 1 || len(store.rows) != 1 {
		t.Fatalf("persisted counts: returned=%d stored=%d", len(first), len(store.rows))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("non-deterministic id at %d: %s vs %s", i, first[i].ID, second[i].ID)
		}
	}
}

func TestBuildNormalizesDotProjectRoot(t *testing.T) {
	workspaceID := "atom-ws-1"
	got, err := Build(Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		Revisions: []Revision{{
			WorkspaceID:  &workspaceID,
			Branch:       "refs/heads/main",
			Revision:     "refs/heads/main",
			CommitHash:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SCIPProjects: []SCIPProject{{Language: "go", ProjectRoot: ".", Indexer: "scip-go", SCIPWorkerClass: "go"}},
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 1 || got[0].ProjectRoot == nil || *got[0].ProjectRoot != "" {
		t.Fatalf("project root was not normalized: %+v", got)
	}
}

func TestBuildRejectsRepoWideCloneFlag(t *testing.T) {
	workspaceID := "atom-ws-1"
	_, err := Build(Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		Revisions: []Revision{{
			WorkspaceID: &workspaceID,
			Branch:      "refs/heads/main",
			Revision:    "refs/heads/main",
			CommitHash:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RunClone:    true,
		}},
	})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("err = %v want ErrInvalidPlan", err)
	}
}

func TestBuildCreatesZoektSubjob(t *testing.T) {
	workspaceID := "atom-ws-1"
	got, err := Build(Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		Revisions: []Revision{{
			WorkspaceID: &workspaceID,
			Branch:      "refs/heads/main",
			Revision:    "refs/heads/main",
			CommitHash:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RunZoekt:    true,
		}},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(got) != 1 || got[0].Layer != indexsubjobs.LayerZoekt || got[0].WorkerClass != "core" || got[0].QueueName != "codeintel-index-core" {
		t.Fatalf("unexpected Zoekt plan: %+v", got)
	}
}

func TestBuildRejectsActivateOnly(t *testing.T) {
	workspaceID := "atom-ws-1"
	_, err := Build(Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		Revisions: []Revision{{
			WorkspaceID: &workspaceID,
			Branch:      "refs/heads/main",
			Revision:    "refs/heads/main",
			CommitHash:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RunActivate: true,
		}},
	})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("err = %v want ErrInvalidPlan", err)
	}
}

func TestBuildRejectsUnsafeProjectRoot(t *testing.T) {
	workspaceID := "atom-ws-1"
	for _, root := range []string{"../outside", "services/../../outside", "/abs", `..\outside`, "services//api"} {
		_, err := Build(Input{
			RepoIndexingJobID: "job-1",
			OrgID:             7,
			RepoID:            42,
			Revisions: []Revision{{
				WorkspaceID:  &workspaceID,
				Branch:       "refs/heads/main",
				Revision:     "refs/heads/main",
				CommitHash:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				SCIPProjects: []SCIPProject{{Language: "go", ProjectRoot: root, Indexer: "scip-go", SCIPWorkerClass: "go"}},
			}},
		})
		if !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("root %q err = %v want ErrInvalidPlan", root, err)
		}
	}
}

func TestBuildWorkspaceParticipatesInIdentity(t *testing.T) {
	workspaceA := "atom-ws-a"
	workspaceB := "atom-ws-b"
	base := Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		Revisions: []Revision{{
			WorkspaceID:  &workspaceA,
			Branch:       "refs/heads/main",
			Revision:     "refs/heads/main",
			CommitHash:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SCIPProjects: []SCIPProject{{Language: "go", ProjectRoot: "", Indexer: "scip-go", SCIPWorkerClass: "go"}},
		}},
	}
	first, err := Build(base)
	if err != nil {
		t.Fatalf("Build first: %v", err)
	}
	base.Revisions[0].WorkspaceID = &workspaceB
	second, err := Build(base)
	if err != nil {
		t.Fatalf("Build second: %v", err)
	}
	if first[0].ID == second[0].ID {
		t.Fatalf("workspace-specific subjobs collapsed to %s", first[0].ID)
	}
}

func TestBuildRejectsMissingWorkspace(t *testing.T) {
	_, err := Build(Input{
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		RepoID:            42,
		Revisions: []Revision{{
			Branch:     "refs/heads/main",
			Revision:   "refs/heads/main",
			CommitHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RunZoekt:   true,
		}},
	})
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("err = %v want ErrInvalidPlan", err)
	}
}

type recordingStore struct {
	rows []indexsubjobs.CreateInput
}

func (s *recordingStore) UpsertQueued(_ context.Context, in indexsubjobs.CreateInput) error {
	s.rows = append(s.rows, in)
	return nil
}
