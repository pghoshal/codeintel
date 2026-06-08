package indexexecutor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemArtifactValidatorRejectsEscapedPath(t *testing.T) {
	payload := scipPayload()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "artifact.scip")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	validator, err := NewFilesystemArtifactValidator(root)
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	_, err = validator.ValidateAndPublish(context.Background(), payload, Result{
		ArtifactTempPath: outside,
		ArtifactPath:     filepath.Join(root, "7", "42", artifactScopeSegment(*payload.WorkspaceID), artifactScopeSegment(payload.Branch), payload.CommitHash, "scip", "artifact.scip"),
		ArtifactSHA256:   strings.Repeat("0", 64),
	})
	if err == nil {
		t.Fatal("ValidateAndPublish accepted path outside artifact root")
	}
}

func TestFilesystemArtifactValidatorRejectsWrongScopeAndChecksum(t *testing.T) {
	payload := scipPayload()
	root, tempPath, finalPath, _ := writeAttemptArtifact(t, payload, []byte("payload"))
	validator, err := NewFilesystemArtifactValidator(root)
	if err != nil {
		t.Fatalf("validator: %v", err)
	}
	_, err = validator.ValidateAndPublish(context.Background(), payload, Result{
		ArtifactTempPath: tempPath,
		ArtifactPath:     filepath.Join(root, "8", "42", artifactScopeSegment(*payload.WorkspaceID), artifactScopeSegment(payload.Branch), payload.CommitHash, "scip", "artifact.scip"),
		ArtifactSHA256:   strings.Repeat("0", 64),
	})
	if err == nil {
		t.Fatal("ValidateAndPublish accepted wrong org scope")
	}
	_, err = validator.ValidateAndPublish(context.Background(), payload, Result{
		ArtifactTempPath: tempPath,
		ArtifactPath:     finalPath,
		ArtifactSHA256:   strings.Repeat("0", 64),
	})
	if err == nil {
		t.Fatal("ValidateAndPublish accepted checksum mismatch")
	}
}
