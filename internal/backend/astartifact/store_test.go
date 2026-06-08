package astartifact

import (
	"os"
	"path/filepath"
	"testing"

	"codeintel/internal/backend/indexsubjobtask"
)

func TestValidatePublishedArtifactScopeRejectsPathOutsideRoot(t *testing.T) {
	workspaceID := "atom-ws-root-guard"
	payload := indexsubjobtask.Payload{
		OrgID:       101,
		WorkspaceID: &workspaceID,
		RepoID:      202,
		Branch:      "refs/heads/main",
		CommitHash:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "101", "202", artifactScopeSegment(workspaceID), artifactScopeSegment(payload.Branch), payload.CommitHash, "ast", "graph.json")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.WriteFile(outside, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := validatePublishedArtifactScope(root, payload, outside); err == nil {
		t.Fatal("validatePublishedArtifactScope accepted artifact outside root")
	}
}

func TestValidatePublishedArtifactScopeAcceptsRootScopedPath(t *testing.T) {
	workspaceID := "atom-ws-root-guard"
	payload := indexsubjobtask.Payload{
		OrgID:       101,
		WorkspaceID: &workspaceID,
		RepoID:      202,
		Branch:      "refs/heads/main",
		CommitHash:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	root := t.TempDir()
	inside := filepath.Join(root, "101", "202", artifactScopeSegment(workspaceID), artifactScopeSegment(payload.Branch), payload.CommitHash, "ast", "graph.json")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	if err := os.WriteFile(inside, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write inside: %v", err)
	}
	if err := validatePublishedArtifactScope(root, payload, inside); err != nil {
		t.Fatalf("validatePublishedArtifactScope rejected scoped artifact: %v", err)
	}
}
