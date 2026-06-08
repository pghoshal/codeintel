package indexexecutor

import (
	"context"
	"net"
	"testing"
	"time"

	codeintelv1 "codeintel/proto/codeintel/v1"

	"google.golang.org/grpc"
)

func TestGRPCRunnerMapsPayloadAndResult(t *testing.T) {
	srv := grpc.NewServer()
	fake := &fakeExecutorServer{}
	codeintelv1.RegisterIndexExecutorServiceServer(srv, fake)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()

	runner, err := NewGRPCRunner(lis.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("NewGRPCRunner: %v", err)
	}
	defer runner.Close()

	result, err := runner.Execute(context.Background(), Job{Payload: scipPayload()})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.ArtifactPath != "/efs/final.scip" || result.ArtifactSHA256 != "sha256:abc" {
		t.Fatalf("result = %+v", result)
	}
	req := fake.req
	if req.GetSubjobId() != "subjob-1" || req.GetOrgId() != 7 || req.GetRepoId() != 42 ||
		req.GetWorkspaceId() != "atom-ws-1" || req.GetWorkerClass() != "scip-go" ||
		req.GetLanguage() != "go" || req.GetProjectRoot() != "" || req.GetIndexer() != "scip-go" {
		t.Fatalf("request = %+v", req)
	}
}

type fakeExecutorServer struct {
	codeintelv1.UnimplementedIndexExecutorServiceServer
	req *codeintelv1.ExecuteIndexSubjobRequest
}

func (s *fakeExecutorServer) ExecuteSubjob(_ context.Context, req *codeintelv1.ExecuteIndexSubjobRequest) (*codeintelv1.ExecuteIndexSubjobResponse, error) {
	s.req = req
	return &codeintelv1.ExecuteIndexSubjobResponse{
		ArtifactTempPath: "/efs/tmp.scip",
		ArtifactPath:     "/efs/final.scip",
		ArtifactSha256:   "sha256:abc",
		Metadata:         map[string]string{"symbols": "10"},
	}, nil
}
