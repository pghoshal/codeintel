package server

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/sourcegraph/zoekt/grpc/chunk"
	proto "github.com/sourcegraph/zoekt/grpc/protos/zoekt/webserver/v1"
	"github.com/sourcegraph/zoekt/shards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/sourcegraph/zoekt"
	"github.com/sourcegraph/zoekt/query"
)

func NewServer(s zoekt.Streamer) *Server {
	return &Server{
		streamer:        s,
		tenantIndexRoot: os.Getenv("CODEINTEL_ZOEKT_EFS_ROOT"),
		tenantSearchers: map[string]zoekt.Streamer{},
	}
}

type Server struct {
	proto.UnimplementedWebserverServiceServer
	streamer        zoekt.Streamer
	tenantIndexRoot string
	mu              sync.Mutex
	tenantSearchers map[string]zoekt.Streamer
}

var tenantIDPattern = regexp.MustCompile(`^[0-9]+$`)

func resolveTenantIndexPath(tenantIndexRoot string, tenantID string, md metadata.MD) (string, error) {
	indexPaths := md.Get("codeintel-index-path")
	if len(indexPaths) == 0 {
		return filepath.Join(tenantIndexRoot, tenantID, "index"), nil
	}
	if len(indexPaths) != 1 || strings.TrimSpace(indexPaths[0]) == "" {
		return "", status.Error(codes.InvalidArgument, "codeintel-index-path metadata must contain one index path")
	}

	orgRoot := filepath.Join(tenantIndexRoot, tenantID)
	cleanOrgRoot, err := filepath.Abs(filepath.Clean(orgRoot))
	if err != nil {
		return "", status.Errorf(codes.Internal, "failed to resolve tenant root: %v", err)
	}
	cleanIndexPath, err := filepath.Abs(filepath.Clean(indexPaths[0]))
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "failed to resolve codeintel-index-path metadata: %v", err)
	}

	relativePath, err := filepath.Rel(cleanOrgRoot, cleanIndexPath)
	if err != nil {
		return "", status.Errorf(codes.InvalidArgument, "codeintel-index-path metadata must be under the tenant root: %v", err)
	}
	if relativePath == ".." || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) || filepath.IsAbs(relativePath) {
		return "", status.Error(codes.PermissionDenied, "codeintel-index-path metadata must be under the requested tenant root")
	}

	return cleanIndexPath, nil
}

func (s *Server) getStreamer(ctx context.Context) (zoekt.Streamer, error) {
	if os.Getenv("CODEINTEL_ZOEKT_STORAGE_LAYOUT") != "org-directory" {
		return s.streamer, nil
	}

	if s.tenantIndexRoot == "" {
		return nil, status.Error(codes.FailedPrecondition, "CODEINTEL_ZOEKT_EFS_ROOT is required for org-directory zoekt routing")
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "codeintel-org-id metadata is required")
	}

	tenantIDs := md.Get("codeintel-org-id")
	if len(tenantIDs) != 1 || !tenantIDPattern.MatchString(tenantIDs[0]) {
		return nil, status.Error(codes.InvalidArgument, "codeintel-org-id metadata must contain one numeric org id")
	}
	tenantID := tenantIDs[0]
	indexPath, err := resolveTenantIndexPath(s.tenantIndexRoot, tenantID, md)
	if err != nil {
		return nil, err
	}
	cacheKey := tenantID + "|" + indexPath

	s.mu.Lock()
	defer s.mu.Unlock()

	if searcher, ok := s.tenantSearchers[cacheKey]; ok {
		return searcher, nil
	}

	if err := os.MkdirAll(indexPath, 0o755); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create tenant index directory: %v", err)
	}

	searcher, err := shards.NewDirectorySearcher(indexPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to open tenant index directory: %v", err)
	}

	s.tenantSearchers[cacheKey] = searcher
	return searcher, nil
}

func (s *Server) Search(ctx context.Context, req *proto.SearchRequest) (*proto.SearchResponse, error) {
	if req.GetQuery() == nil {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}

	q, err := query.QFromProto(req.GetQuery())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	streamer, err := s.getStreamer(ctx)
	if err != nil {
		return nil, err
	}

	res, err := streamer.Search(ctx, q, zoekt.SearchOptionsFromProto(req.GetOpts()))
	if err != nil {
		return nil, err
	}

	return res.ToProto(), nil
}

func (s *Server) StreamSearch(req *proto.StreamSearchRequest, ss proto.WebserverService_StreamSearchServer) error {
	request := req.GetRequest()
	if request == nil || request.GetQuery() == nil {
		return status.Error(codes.InvalidArgument, "query is required")
	}

	q, err := query.QFromProto(request.GetQuery())
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	sender := gRPCChunkSender(ss)
	sampler := newSamplingSender(sender)

	streamer, err := s.getStreamer(ss.Context())
	if err != nil {
		return err
	}

	err = streamer.StreamSearch(ss.Context(), q, zoekt.SearchOptionsFromProto(request.GetOpts()), sampler)
	if err == nil {
		sampler.Flush()
	}
	return err
}

func (s *Server) List(ctx context.Context, req *proto.ListRequest) (*proto.ListResponse, error) {
	if req.GetQuery() == nil {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}

	q, err := query.QFromProto(req.GetQuery())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	streamer, err := s.getStreamer(ctx)
	if err != nil {
		return nil, err
	}

	repoList, err := streamer.List(ctx, q, zoekt.ListOptionsFromProto(req.GetOpts()))
	if err != nil {
		return nil, err
	}

	return repoList.ToProto(), nil
}

// gRPCChunkSender is a zoekt.Sender that sends small chunks of FileMatches to the provided gRPC stream.
func gRPCChunkSender(ss proto.WebserverService_StreamSearchServer) zoekt.Sender {
	f := func(r *zoekt.SearchResult) {
		result := r.ToStreamProto().GetResponseChunk()

		if len(result.GetFiles()) == 0 { // stats-only result, send it immediately
			_ = ss.Send(&proto.StreamSearchResponse{
				ResponseChunk: result,
			})
			return
		}

		// Otherwise, chunk the file matches into multiple responses

		statsSent := false
		numFilesSent := 0

		sendFunc := func(filesChunk []*proto.FileMatch) error {
			numFilesSent += len(filesChunk)

			var stats *proto.Stats
			if !statsSent { // We only send stats back on the first chunk
				statsSent = true
				stats = result.GetStats()
			}

			progress := result.GetProgress()

			if numFilesSent < len(result.GetFiles()) { // more chunks to come
				progress = &proto.Progress{
					Priority: result.GetProgress().GetPriority(),

					// We want the client to consume the entire set of chunks - so we manually
					// patch the MaxPendingPriority to be >= overall priority.
					MaxPendingPriority: math.Max(
						result.GetProgress().GetPriority(),
						result.GetProgress().GetMaxPendingPriority(),
					),
				}
			}

			return ss.Send(&proto.StreamSearchResponse{
				ResponseChunk: &proto.SearchResponse{
					Files: filesChunk,

					Stats:    stats,
					Progress: progress,
				},
			})
		}

		_ = chunk.SendAll(sendFunc, result.GetFiles()...)
	}

	return zoekt.SenderFunc(f)
}
