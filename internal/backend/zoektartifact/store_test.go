package zoektartifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"codeintel/internal/backend/indexexecutor"
	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/pkg/repopaths"

	"github.com/pashagolub/pgxmock/v4"
)

func TestIngestValidatesShardManifestAndLease(t *testing.T) {
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	dataRoot := t.TempDir()
	artifactRoot := t.TempDir()
	payload := indexsubjobtask.Payload{
		SubjobID:          "isj-zoekt",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       strPtr("atom-ws"),
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Layer:             indexsubjobtask.LayerZoekt,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
		Attempt:           1,
	}

	indexDir := filepath.Join(dataRoot, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("mkdir indexDir: %v", err)
	}
	shardPath := filepath.Join(indexDir, repopaths.ShardPrefix(payload.OrgID, payload.RepoID)+"_main_0.zoekt")
	if err := os.WriteFile(shardPath, []byte("zoekt shard bytes"), 0o644); err != nil {
		t.Fatalf("write shard: %v", err)
	}
	shardSHA := shaFileForTest(t, shardPath)
	artifact := zoektArtifact{
		OrgID:       payload.OrgID,
		WorkspaceID: *payload.WorkspaceID,
		RepoID:      payload.RepoID,
		Branch:      payload.Branch,
		Revision:    payload.Revision,
		CommitHash:  payload.CommitHash,
		IndexJobID:  payload.RepoIndexingJobID,
		IndexDir:    indexDir,
		ShardPrefix: repopaths.ShardPrefix(payload.OrgID, payload.RepoID),
		Shards: []zoektShardArtifact{{
			Path:      shardPath,
			SHA256:    "sha256:" + shardSHA,
			SizeBytes: int64(len("zoekt shard bytes")),
		}},
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	artifactPath := filepath.Join(
		artifactRoot,
		"7",
		"42",
		artifactScopeSegment(*payload.WorkspaceID),
		artifactScopeSegment(payload.Branch),
		payload.CommitHash,
		"zoekt",
		"isj-zoekt_attempt_1.zoekt.json",
	)
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.WriteFile(artifactPath, raw, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	artifactSHABytes := sha256.Sum256(raw)
	artifactSHA := "sha256:" + hex.EncodeToString(artifactSHABytes[:])

	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs(payload.SubjobID, payload.RepoIndexingJobID, payload.OrgID, payload.WorkspaceID,
			payload.RepoID, payload.Branch, payload.Revision, payload.CommitHash,
			payload.WorkerClass, payload.QueueName, "lease-owner", "attempt-1",
			artifactPath, artifactSHA).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	store, err := NewStore(mock, repopaths.Config{DataCacheDir: dataRoot}, artifactRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	err = store.Ingest(ctx, payload, indexexecutor.Result{
		ArtifactPath:   artifactPath,
		ArtifactSHA256: artifactSHA,
	}, "lease-owner", "attempt-1")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestIngestMarksManifestRewriteWhenZoektDeltaFallsBack(t *testing.T) {
	ctx := context.Background()
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	dataRoot := t.TempDir()
	artifactRoot := t.TempDir()
	payload := indexsubjobtask.Payload{
		SubjobID:          "isj-zoekt",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       strPtr("atom-ws"),
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Layer:             indexsubjobtask.LayerZoekt,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
		Attempt:           1,
	}
	indexDir := filepath.Join(dataRoot, "index")
	if err := os.MkdirAll(indexDir, 0o755); err != nil {
		t.Fatalf("mkdir indexDir: %v", err)
	}
	shardPath := filepath.Join(indexDir, repopaths.ShardPrefix(payload.OrgID, payload.RepoID)+"_main_0.zoekt")
	if err := os.WriteFile(shardPath, []byte("zoekt shard bytes"), 0o644); err != nil {
		t.Fatalf("write shard: %v", err)
	}
	shardSHA := shaFileForTest(t, shardPath)
	artifact := zoektArtifact{
		OrgID:       payload.OrgID,
		WorkspaceID: *payload.WorkspaceID,
		RepoID:      payload.RepoID,
		Branch:      payload.Branch,
		Revision:    payload.Revision,
		CommitHash:  payload.CommitHash,
		IndexJobID:  payload.RepoIndexingJobID,
		IndexDir:    indexDir,
		ShardPrefix: repopaths.ShardPrefix(payload.OrgID, payload.RepoID),
		Shards: []zoektShardArtifact{{
			Path:      shardPath,
			SHA256:    "sha256:" + shardSHA,
			SizeBytes: int64(len("zoekt shard bytes")),
		}},
		Stderr: `2026/06/06 12:00:00 delta build: falling back to normal build since delta build failed, repository="repo", err=branch set changed`,
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	artifactPath := filepath.Join(
		artifactRoot,
		"7",
		"42",
		artifactScopeSegment(*payload.WorkspaceID),
		artifactScopeSegment(payload.Branch),
		payload.CommitHash,
		"zoekt",
		"isj-zoekt_attempt_1.zoekt.json",
	)
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o755); err != nil {
		t.Fatalf("mkdir artifact dir: %v", err)
	}
	if err := os.WriteFile(artifactPath, raw, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	artifactSHABytes := sha256.Sum256(raw)
	artifactSHA := "sha256:" + hex.EncodeToString(artifactSHABytes[:])

	mock.ExpectExec(`UPDATE "CodeIntelIndexSubjob"`).
		WithArgs(payload.SubjobID, payload.RepoIndexingJobID, payload.OrgID, payload.WorkspaceID,
			payload.RepoID, payload.Branch, payload.Revision, payload.CommitHash,
			payload.WorkerClass, payload.QueueName, "lease-owner", "attempt-1",
			artifactPath, artifactSHA).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectExec(`UPDATE "RepoIndexManifest"`).
		WithArgs(payload.RepoIndexingJobID, payload.OrgID, payload.RepoID, payload.WorkspaceID, payload.Branch, payload.CommitHash).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	store, err := NewStore(mock, repopaths.Config{DataCacheDir: dataRoot}, artifactRoot)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	err = store.Ingest(ctx, payload, indexexecutor.Result{
		ArtifactPath:   artifactPath,
		ArtifactSHA256: artifactSHA,
	}, "lease-owner", "attempt-1")
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestValidateArtifactScopeRejectsOtherOrgShardPrefix(t *testing.T) {
	payload := indexsubjobtask.Payload{
		SubjobID:          "isj-zoekt",
		RepoIndexingJobID: "job-1",
		OrgID:             7,
		WorkspaceID:       strPtr("atom-ws"),
		RepoID:            42,
		Branch:            "refs/heads/main",
		Revision:          "refs/heads/main",
		CommitHash:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Layer:             indexsubjobtask.LayerZoekt,
		WorkerClass:       "core",
		QueueName:         "codeintel-index-core",
	}
	err := validateArtifactScope(payload, zoektArtifact{
		OrgID:       7,
		WorkspaceID: "atom-ws",
		RepoID:      42,
		Branch:      "refs/heads/main",
		Revision:    "refs/heads/main",
		CommitHash:  payload.CommitHash,
		IndexJobID:  "job-1",
		ShardPrefix: "8_42",
		Shards:      []zoektShardArtifact{{Path: "/tmp/8_42_main_0.zoekt", SHA256: "sha256:" + "a"}},
	})
	if err == nil {
		t.Fatal("validateArtifactScope accepted other-org shard prefix")
	}
}

func shaFileForTest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func strPtr(value string) *string {
	return &value
}
