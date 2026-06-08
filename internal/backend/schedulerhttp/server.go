package schedulerhttp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"codeintel/internal/backend/scheduler"
	"codeintel/pkg/repoindex"
	"codeintel/pkg/schedulerapi"
)

const (
	repoIndexPath      = "/internal/scheduler/repo-index"
	connectionSyncPath = "/internal/scheduler/connection-sync"
)

type Config struct {
	Scheduler *scheduler.Service
	Logger    *slog.Logger
	Token     string
}

type Server struct {
	cfg    Config
	logger *slog.Logger
}

func NewServer(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, logger: logger.With("component", "scheduler-http")}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, schedulerapi.ErrorResponse{Error: "unauthorized"})
		return
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case repoIndexPath:
		s.handleRepoIndex(w, r)
	case connectionSyncPath:
		s.handleConnectionSync(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleRepoIndex(w http.ResponseWriter, r *http.Request) {
	var req schedulerapi.RepoIndexRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, schedulerapi.ErrorResponse{Error: "invalid repo index schedule request"})
		return
	}
	result, err := s.cfg.Scheduler.ScheduleRepoIndex(r.Context(), scheduler.RepoScheduleRequest{
		OrgID:  req.OrgID,
		RepoID: req.RepoID,
		Kind:   repoindex.JobType(req.Kind),
		Ref:    req.Ref,
	})
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, scheduler.ErrUnavailable):
			status = http.StatusServiceUnavailable
		case errors.Is(err, scheduler.ErrRepoNotFound):
			status = http.StatusNotFound
		default:
			var activeErr *scheduler.JobAlreadyActiveError
			if errors.As(err, &activeErr) {
				writeJSON(w, http.StatusConflict, schedulerapi.ErrorResponse{Error: activeErr.Error()})
				return
			}
		}
		s.logger.Error("repo index schedule failed", "err", err, "orgId", req.OrgID, "repoId", req.RepoID, "kind", req.Kind)
		writeJSON(w, status, schedulerapi.ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, schedulerapi.RepoIndexResponse{
		JobID:             result.JobID,
		AlreadyAtCapacity: result.AlreadyAtCapacity,
	})
}

func (s *Server) handleConnectionSync(w http.ResponseWriter, r *http.Request) {
	var req schedulerapi.ConnectionSyncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, schedulerapi.ErrorResponse{Error: "invalid connection sync schedule request"})
		return
	}
	result, err := s.cfg.Scheduler.ScheduleConnectionSync(r.Context(), scheduler.ConnectionSyncRequest{
		OrgID:        req.OrgID,
		ConnectionID: req.ConnectionID,
	})
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, scheduler.ErrUnavailable):
			status = http.StatusServiceUnavailable
		case errors.Is(err, scheduler.ErrConnectionNotFound):
			status = http.StatusNotFound
		}
		s.logger.Error("connection sync schedule failed", "err", err, "orgId", req.OrgID, "connectionId", req.ConnectionID)
		writeJSON(w, status, schedulerapi.ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, schedulerapi.ConnectionSyncResponse{
		JobID:             result.JobID,
		AlreadyAtCapacity: result.AlreadyAtCapacity,
	})
}

func (s *Server) authorized(r *http.Request) bool {
	token := strings.TrimSpace(s.cfg.Token)
	if token == "" {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
