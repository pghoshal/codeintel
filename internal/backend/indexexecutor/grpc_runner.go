package indexexecutor

import (
	"context"
	"errors"
	"fmt"
	"time"

	codeintelv1 "codeintel/proto/codeintel/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

const defaultExecutorRPCTimeout = 2 * time.Hour

var ErrExecutorUnavailable = errors.New("indexexecutor: executor unavailable")

type GRPCRunner struct {
	conn    *grpc.ClientConn
	client  codeintelv1.IndexExecutorServiceClient
	timeout time.Duration
}

func NewGRPCRunner(addr string, timeout time.Duration) (*GRPCRunner, error) {
	if addr == "" {
		return nil, errors.New("indexexecutor: gRPC runner addr is required")
	}
	if timeout <= 0 {
		timeout = defaultExecutorRPCTimeout
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("indexexecutor: dial %s: %w", addr, err)
	}
	return &GRPCRunner{
		conn:    conn,
		client:  codeintelv1.NewIndexExecutorServiceClient(conn),
		timeout: timeout,
	}, nil
}

func (r *GRPCRunner) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close()
}

func (r *GRPCRunner) Execute(ctx context.Context, job Job) (Result, error) {
	if r == nil || r.client == nil {
		return Result{}, errors.New("indexexecutor: gRPC runner is not configured")
	}
	if err := job.Payload.Validate(); err != nil {
		return Result{}, err
	}
	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	resp, err := r.client.ExecuteSubjob(callCtx, &codeintelv1.ExecuteIndexSubjobRequest{
		SubjobId:          job.Payload.SubjobID,
		RepoIndexingJobId: job.Payload.RepoIndexingJobID,
		OrgId:             job.Payload.OrgID,
		WorkspaceId:       stringValue(job.Payload.WorkspaceID),
		RepoId:            job.Payload.RepoID,
		Branch:            job.Payload.Branch,
		Revision:          job.Payload.Revision,
		CommitHash:        job.Payload.CommitHash,
		Layer:             string(job.Payload.Layer),
		Language:          job.Payload.Language,
		ProjectRoot:       job.Payload.ProjectRoot,
		Indexer:           job.Payload.Indexer,
		WorkerClass:       job.Payload.WorkerClass,
		QueueName:         job.Payload.QueueName,
		Attempt:           job.Payload.Attempt,
	})
	if err != nil {
		code := status.Code(err)
		if code == codes.Unavailable || code == codes.ResourceExhausted {
			return Result{}, fmt.Errorf("%w: %v", ErrExecutorUnavailable, err)
		}
		return Result{}, err
	}
	return Result{
		ArtifactTempPath: resp.GetArtifactTempPath(),
		ArtifactPath:     resp.GetArtifactPath(),
		ArtifactSHA256:   resp.GetArtifactSha256(),
		Metadata:         resp.GetMetadata(),
	}, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
