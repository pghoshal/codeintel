// Package api hosts the HTTP gateway. Each route is declared in
// its own file and registered through Server.routes(). The Server
// applies a configurable middleware chain (recover -> CORS ->
// request-id -> access-log -> metrics -> rate-limit -> handler) to
// every /api/* route; probes and the /metrics endpoint bypass the
// chain by design.
package api

import (
	"context"
	"log/slog"
	"net/http"

	"codeintel/internal/analytics"
	"codeintel/internal/db"
	"codeintel/internal/obs"
	"codeintel/pkg/audit"
)

// DBPinger is the minimal surface the /readyz probe needs to check
// the database is reachable. *db.Pool satisfies this directly.
type DBPinger interface {
	Ping(ctx context.Context) error
}

// SecretsQuerier is the db surface the /api/secrets handlers need
// in addition to the auth surface. Defined as an interface so tests
// can drop in a fake without booting Postgres or pgxmock.
type SecretsQuerier interface {
	ListOrgSecrets(ctx context.Context, orgID int32) ([]db.OrgSecret, error)
	UpsertOrgSecret(ctx context.Context, p db.UpsertOrgSecretParams) (db.OrgSecret, error)
	ListOrgConnectionsForRefcheck(ctx context.Context, orgID int32) ([]db.ConfigOwner, error)
	ListOrgLanguageModelsForRefcheck(ctx context.Context, orgID int32) ([]db.ConfigOwner, error)
	DeleteOrgSecret(ctx context.Context, orgID int32, key string) error
}

// ConnectionsQuerier is the db surface the /api/connections
// handlers need.
type ConnectionsQuerier interface {
	ListOrgConnectionsForRead(ctx context.Context, orgID int32) ([]db.ConnectionListRow, error)
	DeleteOrgConnection(ctx context.Context, orgID int32, connectionID int32) error
	UpsertOrgConnection(ctx context.Context, p db.UpsertOrgConnectionParams) (db.ConnectionListRow, error)
	GetOrgConnectionForUpdate(ctx context.Context, orgID, connectionID int32) (db.ConnectionListRow, error)
	CheckOrgConnectionNameAvailable(ctx context.Context, orgID int32, name string, excludeID int32) error
	ConnectionExistsInOrg(ctx context.Context, orgID, connectionID int32) (bool, error)
	GetOrgConnectionMeta(ctx context.Context, orgID, connectionID int32) (db.ConnectionMetaRow, error)
	ListConnectionSyncJobs(ctx context.Context, orgID, connectionID, limit int32) ([]db.ConnectionSyncJobRow, error)
	CountConnectionRepos(ctx context.Context, orgID, connectionID int32) (int32, error)
}

// StatusQuerier is the db surface the /api/status handler needs.
type StatusQuerier interface {
	GetOrgStatusRollup(ctx context.Context, orgID int32) (db.OrgStatusRollup, error)
	ListRecentFailedConnectionSyncJobs(ctx context.Context, orgID, limit int32) ([]db.RecentFailedConnectionSyncJobRow, error)
	ListRecentFailedRepoIndexingJobs(ctx context.Context, orgID, limit int32) ([]db.RecentFailedRepoIndexingJobRow, error)
}

// ReposQuerier is the db surface the /api/repos handler needs.
// ListOrgRepos pages the active-repo set; CountOrgRepos backs the
// X-Total-Count response header.
type ReposQuerier interface {
	ListOrgRepos(ctx context.Context, p db.ListOrgReposParams) ([]db.RepoListRow, error)
	CountOrgRepos(ctx context.Context, p db.CountOrgReposParams) (int32, error)
}

// ModelsQuerier is the db surface the /api/models handlers need.
// GET reads via ListEnabledOrgLanguageModels; PUT writes via
// ReplaceOrgLanguageModels and checks missing secret refs via
// SelectMissingOrgSecretKeys.
type ModelsQuerier interface {
	GetOrgWithMetadata(ctx context.Context, id int32) (db.Org, error)
	ListEnabledOrgLanguageModels(ctx context.Context, orgID int32) ([]db.OrgLanguageModelRow, error)
	SelectMissingOrgSecretKeys(ctx context.Context, orgID int32, candidateKeys []string) ([]string, error)
	ReplaceOrgLanguageModels(ctx context.Context, orgID int32, models []db.OrgLanguageModelInsert) error
}

// AuthQuerier is the union surface the api package needs from db:
// auth resolution + secrets listing + models listing. *db.Queries
// satisfies it directly. Future handlers extend the interface (not
// the impl).
type AuthQuerier interface {
	GetApiKeyAuth(ctx context.Context, hash string) (db.AuthLookup, error)
	UpdateApiKeyLastUsedAt(ctx context.Context, hash string) error
	SecretsQuerier
	ModelsQuerier
	ConnectionsQuerier
	StatusQuerier
	ReposQuerier
}

// Config is the explicit dependency surface for the server. Every new
// route slice adds the dependencies it needs here; nothing is wired via
// package-global state so tests can construct a Server with mocked
// dependencies in isolation.
type Config struct {
	Logger        *slog.Logger
	Queries       AuthQuerier
	EncryptionKey string // CODEINTEL_ENCRYPTION_KEY; HMAC key for ApiKey hashing.

	// Metrics is the Prometheus metrics collector. Optional — when
	// nil (e.g. in tests that don't care about observability),
	// routes are not wrapped and /metrics is not exposed.
	Metrics *obs.Metrics

	// DBPinger backs the /readyz probe. Optional — when nil /readyz
	// short-circuits to 503 unavailable (correct conservative default).
	DBPinger DBPinger

	// SingleTenantOrgID is the org id used for anonymous-access
	// resolution by optional-auth middleware-equivalent handlers.
	// Default is 1 (single-tenant deployments). Zero / negative
	// disables anonymous access entirely.
	SingleTenantOrgID int32

	// RateLimiter, when set, wraps every /api/* route. nil disables
	// rate-limiting entirely (the pass-through middleware path).
	RateLimiter *obs.RateLimiter

	// CORS, when set, applies the CORS middleware to every /api/*
	// route. nil disables CORS (same-origin only).
	CORS *obs.CORSMiddleware

	// ConnectionSyncer, when set, schedules a sync after a successful
	// POST /api/connections (when the request body asks for one).
	// nil falls back to NoopConnectionSyncer so the route stays
	// usable in dev / test deployments without a sync backend.
	ConnectionSyncer ConnectionSyncer

	// RepoIndexer, when set, schedules a repo-index job from the
	// DELETE /api/repos/{id}/index (REMOVE_INDEX) and
	// POST /api/repos/{id}/index (INDEX) routes. nil falls back
	// to NoopRepoIndexer, which fails closed with 503 so callers
	// never see success without a durable job id.
	RepoIndexer RepoIndexer

	// RepoStatusFetcher, when set, serves
	// GET /api/repos/{id}/status. nil falls back to
	// NoopRepoStatusFetcher (every request 404s) so dev / test
	// servers without a Postgres-backed status fetcher don't
	// pretend every repo has an empty status payload.
	RepoStatusFetcher RepoStatusFetcher

	// AuditEmitter, when set, receives every audit event the
	// mutating handlers fire (connection create / update / delete
	// / sync, etc.). nil falls back to audit.NoopEmitter so dev /
	// test deployments stay quiet.
	AuditEmitter audit.Emitter

	// AnalyticsEmitter, when set, receives product-telemetry
	// events handlers fire (search executed, model invoked, sync
	// finished). nil falls back to analytics.NoopEmitter so dev /
	// test deployments emit nothing.
	AnalyticsEmitter analytics.Emitter

	// ZoektWebserverUrls lists the static Zoekt webserver endpoints
	// for the /api/status rollup's `zoekt.mode` resolution: 0 or 1
	// url → "single", 2+ → "fanout". Empty when no Zoekt backend is
	// wired (the common dev / single-tenant default).
	ZoektWebserverUrls []string
	ZoektStorageLayout string
	ZoektEFSRoot       string
	ZoektDataCacheDir  string

	// SearchBackend serves POST /api/search. nil returns a typed
	// 503 so headless deployments never pretend search is available
	// before the real layered retrieval path is wired.
	SearchBackend SearchBackend

	// MCPBackend serves /api/{domain}/mcp. nil returns a typed 503.
	// The route still performs authentication and domain checks so
	// clients get tenant-safe failures even before the real MCP
	// transport is wired.
	MCPBackend MCPBackend

	// ChatBackend serves the headless /api/chat and
	// /api/chat/blocking surfaces. Production wires the same
	// retrieval/agent engine as MCP; tests can inject a small fake.
	ChatBackend ChatBackend

	// AtomControlPlaneToken gates Atom-native workspace provisioning.
	// It is separate from per-org API keys because Atom creates the
	// org tenant before such a key exists.
	AtomControlPlaneToken string
}

// Server holds the HTTP mux and the cross-route dependencies. The mux is
// the stdlib *http.ServeMux which gained method-pattern routing in
// Go 1.22 (`GET /api/...`) lets us avoid a third-party router.
// A switch to chi/echo is a no-op signature change later if we
// need richer middleware ordering.
//
// Per-route loggers are bound once at construction (e.g.
// `healthLogger`) and reused on every request — no per-request
// `.With` allocation on the hot path.
type Server struct {
	cfg               Config
	mux               *http.ServeMux
	healthLogger      *slog.Logger
	secretsLogger     *slog.Logger
	modelsLogger      *slog.Logger
	connectionsLogger *slog.Logger
	statusLogger      *slog.Logger
	reposLogger       *slog.Logger
	searchLogger      *slog.Logger
	mcpLogger         *slog.Logger
	chatLogger        *slog.Logger
}

// NewServer constructs the Server, registers all routes, and
// returns it ready to bind via Router(). Constructor failure is
// intentionally not possible — routes register unconditionally and
// config errors surface at request time.
func NewServer(cfg Config) *Server {
	s := &Server{
		cfg:               cfg,
		mux:               http.NewServeMux(),
		healthLogger:      cfg.Logger.With("logger", "health-check"),
		secretsLogger:     cfg.Logger.With("logger", "secrets"),
		modelsLogger:      cfg.Logger.With("logger", "models"),
		connectionsLogger: cfg.Logger.With("logger", "connections"),
		statusLogger:      cfg.Logger.With("logger", "status"),
		reposLogger:       cfg.Logger.With("logger", "repos"),
		searchLogger:      cfg.Logger.With("logger", "search"),
		mcpLogger:         cfg.Logger.With("logger", "mcp"),
		chatLogger:        cfg.Logger.With("logger", "chat"),
	}
	s.routes()
	return s
}

// Router returns the underlying http.Handler. Cmd-level main.go wires
// this into a *http.Server with read/write timeouts.
func (s *Server) Router() http.Handler {
	return s.mux
}

// routes wires every HTTP route into the mux. Each method uses
// the canonical `/api/<route>` path. The method patterns require
// Go 1.22+ mux syntax (`GET /api/...`).
func (s *Server) routes() {
	s.handle("GET /api/health", "/api/health", s.handleHealth)
	s.handle("GET /api/version", "/api/version", s.handleVersion)
	s.handle("POST /api/atom/workspaces", "/api/atom/workspaces", s.handleProvisionAtomWorkspace)
	s.handle("GET /api/tenants/{domain}/metadata", "/api/tenants/{domain}/metadata", s.handleGetTenant)
	s.handle("GET /api/secrets", "/api/secrets", s.handleListOrgSecrets)
	s.handle("PUT /api/secrets", "/api/secrets", s.handlePutOrgSecret)
	s.handle("DELETE /api/secrets/{key}", "/api/secrets/{key}", s.handleDeleteOrgSecret)
	s.handle("GET /api/models", "/api/models", s.handleListOrgLanguageModels)
	s.handle("PUT /api/models", "/api/models", s.handlePutOrgLanguageModels)
	s.handle("GET /api/connections", "/api/connections", s.handleListOrgConnections)
	s.handle("POST /api/connections", "/api/connections", s.handleUpsertOrgConnection)
	s.handle("PATCH /api/connections/{id}", "/api/connections/{id}", s.handlePatchOrgConnection)
	s.handle("POST /api/connections/{id}/sync", "/api/connections/{id}/sync", s.handleSyncOrgConnection)
	s.handle("GET /api/connections/{id}/branches", "/api/connections/{id}/branches", s.handleGetOrgConnectionBranches)
	s.handle("PUT /api/connections/{id}/branches", "/api/connections/{id}/branches", s.handlePutOrgConnectionBranches)
	s.handle("GET /api/connections/{id}/status", "/api/connections/{id}/status", s.handleGetOrgConnectionStatus)
	s.handle("DELETE /api/connections/{id}", "/api/connections/{id}", s.handleDeleteOrgConnection)
	s.handle("GET /api/status", "/api/status", s.handleStatus)
	s.handle("GET /api/repos", "/api/repos", s.handleListOrgRepos)
	s.handle("GET /api/search-contexts", "/api/search-contexts", s.handleListOrgSearchContexts)
	s.handle("PUT /api/search-contexts", "/api/search-contexts", s.handlePutOrgSearchContexts)
	s.handle("GET /api/repos/{id}/status", "/api/repos/{id}/status", s.handleGetOrgRepoStatus)
	s.handle("GET /api/repos/{id}/branches", "/api/repos/{id}/branches", s.handleGetOrgRepoBranches)
	s.handle("PUT /api/repos/{id}/branches", "/api/repos/{id}/branches", s.handlePutOrgRepoBranches)
	s.handle("POST /api/repos/{id}/index", "/api/repos/{id}/index", s.handlePostOrgRepoIndex)
	s.handle("DELETE /api/repos/{id}/index", "/api/repos/{id}/index", s.handleDeleteOrgRepoIndex)
	s.handle("POST /api/search", "/api/search", s.handleSearch)
	s.handle("POST /api/chat", "/api/chat", s.handleChat)
	s.handle("POST /api/chat/blocking", "/api/chat/blocking", s.handleBlockingChat)
	s.handle("GET /api/chat/{id}/result", "/api/chat/{id}/result", s.handleGetChatResult)
	s.handle("POST /api/{domain}/mcp", "/api/{domain}/mcp", s.handleMCP)
	s.handle("GET /api/{domain}/mcp", "/api/{domain}/mcp", s.handleMCP)

	// Kubernetes probes (no auth, no metrics — probes themselves
	// must not appear as application traffic in the histogram).
	s.mux.HandleFunc("GET /healthz", s.handleLiveness)
	s.mux.HandleFunc("GET /readyz", s.handleReadiness)

	// /metrics is mounted only when a Metrics collector is configured;
	// otherwise the endpoint is omitted entirely.
	if s.cfg.Metrics != nil {
		s.mux.Handle("GET /metrics", s.cfg.Metrics.Handler())
	}
}

// handle is the unified route-registration helper. It wires the
// handler into the mux and applies the observability chain:
//
//	WithPanicRecovery -> WithRequestID -> WithAccessLog ->
//	  WithMetrics -> WithRateLimit -> handler
//
// Rate-limit sits closest to the inner handler so admitted requests
// are counted by metrics + access log but THROTTLED requests still
// show up as 429s in both (operators want visibility into who got
// throttled). Recovery sits outermost so any panic anywhere in the
// chain is caught with a stable 500 envelope.
//
// `pattern` is the mux pattern (with method + braces); `route` is
// the metric/access-log label (without method, no braces — to avoid
// label-cardinality explosion).
func (s *Server) handle(pattern, route string, h http.HandlerFunc) {
	if s.cfg.RateLimiter != nil {
		h = s.cfg.RateLimiter.WithRateLimit(h)
	}
	if s.cfg.Metrics != nil {
		h = s.cfg.Metrics.WithMetrics(route, h)
	}
	if s.cfg.Logger != nil {
		h = obs.WithAccessLog(s.cfg.Logger.With("logger", "access"), route, h)
	}
	h = obs.WithRequestID(h)
	if s.cfg.CORS != nil {
		// CORS sits outside RequestID so preflight responses also
		// carry the X-Request-Id header for client correlation but
		// before metrics — preflight is not application traffic.
		h = s.cfg.CORS.Wrap(h)
	}
	if s.cfg.Logger != nil {
		h = obs.WithPanicRecovery(s.cfg.Logger.With("logger", "recover"), h)
	}
	s.mux.HandleFunc(pattern, h)
}
