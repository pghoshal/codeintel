// codeintel-app is the binary serving the codeintel HTTP API.
// Configuration is environment-driven; every CODEINTEL_* variable
// maps to one struct field on api.Config.
//
// SLOs the binary commits to:
//
//   - p50 latency < 5 ms, p99 < 50 ms for stateless routes
//   - steady memory < 50 MiB
//   - idle CPU < 10 m core
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"codeintel/internal/api"
	"codeintel/internal/db"
	"codeintel/internal/graphreader"
	"codeintel/internal/mcp"
	"codeintel/internal/migrate"
	"codeintel/internal/obs"
	"codeintel/internal/search"
	"codeintel/pkg/audit"
	"codeintel/pkg/llmproxy"
	"codeintel/pkg/nebulaclient"
	"codeintel/pkg/redisclient"
	"codeintel/pkg/repopaths"
)

func main() {
	logger := obs.NewLogger()

	listenAddr := os.Getenv("CODEINTEL_LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = "0.0.0.0:3000"
	}

	dsn := os.Getenv("CODEINTEL_DATABASE_URL")
	if dsn == "" {
		logger.Error("CODEINTEL_DATABASE_URL is required")
		os.Exit(1)
	}
	encryptionKey := os.Getenv("CODEINTEL_ENCRYPTION_KEY")
	if encryptionKey == "" {
		logger.Error("CODEINTEL_ENCRYPTION_KEY is required")
		os.Exit(1)
	}

	// Construct the pgx pool up-front so a misconfigured DSN fails
	// boot instead of failing at request time.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.NewPool(bootCtx, db.Config{DSN: dsn})
	bootCancel()
	if err != nil {
		logger.Error("db pool init failed", "err", err.Error())
		os.Exit(1)
	}
	defer pool.Close()

	// Optional auto-migrate. Enable for first-boot deployments
	// against a fresh Postgres; production deployments that run
	// migrations via a separate pipeline leave this off.
	if os.Getenv("CODEINTEL_AUTO_MIGRATE") == "true" {
		migCtx, migCancel := context.WithTimeout(context.Background(), 30*time.Second)
		applied, err := migrate.Apply(migCtx, pool)
		migCancel()
		if err != nil {
			logger.Error("auto-migrate failed", "err", err.Error())
			os.Exit(1)
		}
		if len(applied) > 0 {
			logger.Info("auto-migrate applied versions", "versions", applied)
		} else {
			logger.Info("auto-migrate: schema already current")
		}
	}

	metrics := obs.NewMetrics()

	// Rate limiter — optional. Read tuning from env; missing /
	// non-positive disables enforcement (useful in dev and tests).
	var rateLimiter *obs.RateLimiter
	if rps := envFloat("CODEINTEL_RATE_LIMIT_RPS", 0); rps > 0 {
		burst := int(envFloat("CODEINTEL_RATE_LIMIT_BURST", 0))
		if burst <= 0 {
			burst = int(rps) * 2
			if burst < 1 {
				burst = 1
			}
		}
		rateLimiter = obs.NewRateLimiter(obs.RateLimitConfig{
			RequestsPerSecond: rps,
			Burst:             burst,
		})
		defer rateLimiter.Stop()
		logger.Info("rate limiter enabled", "rps", rps, "burst", burst)
	}

	// SingleTenantOrgID — the org id used for anonymous-access
	// resolution. Defaults to 1; multi-tenant single-host
	// deployments override via env.
	singleTenantOrgID := int32(1)
	if v := os.Getenv("CODEINTEL_SINGLE_TENANT_ORG_ID"); v != "" {
		if parsed, err := strconv.ParseInt(v, 10, 32); err == nil {
			singleTenantOrgID = int32(parsed)
		} else {
			logger.Warn("CODEINTEL_SINGLE_TENANT_ORG_ID invalid, falling back to 1", "value", v, "err", err.Error())
		}
	}

	// CORS — optional. CODEINTEL_CORS_ALLOWED_ORIGINS is a
	// comma-separated allow-list. Wildcard "*" enables permissive
	// mode (dev only — production should enumerate exact origins).
	var corsMW *obs.CORSMiddleware
	if v := os.Getenv("CODEINTEL_CORS_ALLOWED_ORIGINS"); v != "" {
		origins := strings.Split(v, ",")
		for i := range origins {
			origins[i] = strings.TrimSpace(origins[i])
		}
		corsMW = obs.NewCORSMiddleware(obs.CORSConfig{
			AllowedOrigins:   origins,
			AllowCredentials: os.Getenv("CODEINTEL_CORS_ALLOW_CREDENTIALS") == "true",
		})
		logger.Info("CORS enabled", "origins", origins)
	}

	// AuditEmitter: when CODEINTEL_BACKEND_GRPC_ADDR is set, fire
	// every audit event over gRPC to codeintel-backend for durable
	// persistence. Unset / empty falls back to NoopEmitter so dev
	// deployments without a backend stay quiet.
	var auditEmitter audit.Emitter = audit.NoopEmitter{}
	if addr := os.Getenv("CODEINTEL_BACKEND_GRPC_ADDR"); addr != "" {
		grpcAudit, err := audit.NewGRPCEmitter(addr)
		if err != nil {
			logger.Error("audit GRPC emitter init failed; falling back to Noop", "err", err, "addr", addr)
		} else {
			auditEmitter = grpcAudit
			defer func() { _ = grpcAudit.Shutdown(context.Background()) }()
			logger.Info("audit emitter dialing backend", "addr", addr)
		}
	}

	// ConnectionSyncer + RepoIndexer: app never owns queue
	// producers. When CODEINTEL_BACKEND_CONTROL_URL is set, public
	// mutation routes call codeintel-backend's internal scheduler
	// surface. Unset / empty leaves connection sync best-effort and
	// makes repo index/remove-index fail closed with 503 instead of
	// reporting fake success.
	var connSyncer api.ConnectionSyncer
	var repoIndexer api.RepoIndexer
	var graphCacheClient *redisclient.Client
	graphCacheTTL := envDurationSeconds("CODEINTEL_GRAPH_READER_CACHE_TTL_SECONDS", 2*time.Minute)
	graphEvidenceTTL := envDurationSeconds("CODEINTEL_GRAPH_EVIDENCE_CACHE_TTL_SECONDS", 2*time.Minute)
	graphCacheMaxBytes := envInt("CODEINTEL_GRAPH_READER_CACHE_MAX_BYTES", 256*1024)
	graphInspectionCacheMaxBytes := envInt("CODEINTEL_GRAPH_INSPECTION_CACHE_MAX_BYTES", 256*1024)
	codegraphContextCacheMaxBytes := envInt("CODEINTEL_CODEGRAPH_CONTEXT_CACHE_MAX_BYTES", 256*1024)
	if controlURL := strings.TrimSpace(os.Getenv("CODEINTEL_BACKEND_CONTROL_URL")); controlURL != "" {
		controlToken := strings.TrimSpace(os.Getenv("CODEINTEL_BACKEND_CONTROL_TOKEN"))
		if controlToken == "" {
			logger.Error("backend scheduler client requires CODEINTEL_BACKEND_CONTROL_TOKEN", "url", controlURL)
			os.Exit(1)
		}
		client, err := api.NewBackendSchedulerClient(controlURL, nil, controlToken)
		if err != nil {
			logger.Error("backend scheduler client init failed", "err", err.Error(), "url", controlURL)
			os.Exit(1)
		}
		repoIndexer = client
		connSyncer = api.NewBackendConnectionSyncer(client)
		logger.Info("backend scheduler client wired", "url", controlURL)
	}
	if redisURL := os.Getenv("CODEINTEL_REDIS_URL"); redisURL != "" {
		if graphCacheTTL > 0 || graphEvidenceTTL > 0 {
			cacheCtx, cacheCancel := context.WithTimeout(context.Background(), 5*time.Second)
			client, err := redisclient.New(cacheCtx, redisclient.Config{URL: redisURL}.WithDefaults(), logger)
			if err != nil {
				cacheCancel()
				logger.Warn("graph reader Redis cache disabled", "err", err.Error())
			} else {
				graphCacheClient = client
				defer func() { _ = graphCacheClient.Close() }()
				if snapshot, err := graphCacheClient.EnforceOperationalPolicy(cacheCtx, redisclient.LoadOperationalPolicyFromEnv()); err != nil {
					logger.Warn("redis operational policy check failed; cache remains best-effort", "err", err.Error())
				} else {
					logger.Info("redis operational policy checked",
						"maxmemory_bytes", snapshot.MaxMemoryBytes,
						"used_memory_bytes", snapshot.UsedMemoryBytes,
						"eviction_policy", snapshot.EvictionPolicy,
						"key_count", snapshot.KeyCount,
					)
				}
				cacheCancel()
			}
		}
	}

	queries := db.NewQueries(pool)
	repoPathsCfg := repopaths.LoadConfigFromEnv()

	var searchBackend api.SearchBackend
	zoektEndpoints := splitEnvList(os.Getenv("CODEINTEL_ZOEKT_GRPC_ENDPOINTS"))
	zoektReplicated := envBool("CODEINTEL_ZOEKT_REPLICATED")
	if len(zoektEndpoints) > 0 {
		backend, err := search.NewBackend(context.Background(), search.Config{
			Endpoints:  zoektEndpoints,
			Replicated: zoektReplicated,
			RepoLookup: queries,
		})
		if err != nil {
			logger.Error("search backend init failed", "err", err.Error())
			os.Exit(1)
		}
		searchBackend = backend
		logger.Info("search backend wired",
			"zoekt_endpoints", zoektEndpoints,
			"replicated", zoektReplicated,
		)
	}

	var graphReader graphreader.Inspector
	if os.Getenv(nebulaclient.EnvAddr) != "" {
		nebulaCfg, err := nebulaclient.LoadConfigFromEnv()
		if err != nil {
			logger.Error("graph reader config failed", "err", err.Error())
			os.Exit(1)
		}
		nebulaCtx, nebulaCancel := context.WithTimeout(context.Background(), 5*time.Second)
		nebulaClient, err := nebulaclient.New(nebulaCtx, nebulaCfg, logger)
		nebulaCancel()
		if err != nil {
			logger.Error("graph reader init failed", "err", err.Error())
			os.Exit(1)
		}
		defer nebulaClient.Close()
		graphReader = graphreader.New(nebulaClient, logger)
		if graphCacheClient != nil && graphCacheTTL > 0 {
			graphReader = graphreader.NewCachedWithMaxBytes(graphReader, graphCacheClient.Underlying(), graphCacheTTL, graphCacheMaxBytes, logger)
			logger.Info("graph reader cache wired", "ttl", graphCacheTTL.String(), "maxBytes", graphCacheMaxBytes)
		}
		logger.Info("graph reader wired", "space", nebulaCfg.Space)
	}

	var languageModelClient mcp.LanguageModelClient
	if gatewayURL := strings.TrimSpace(os.Getenv("CODEINTEL_LLM_GATEWAY_URL")); gatewayURL != "" {
		llmToken := strings.TrimSpace(os.Getenv("CODEINTEL_BACKEND_LLM_TOKEN"))
		if llmToken == "" {
			logger.Error("llm gateway client requires CODEINTEL_BACKEND_LLM_TOKEN", "url", gatewayURL)
			os.Exit(1)
		}
		client, err := llmproxy.NewClient(gatewayURL, nil, llmToken)
		if err != nil {
			logger.Error("llm gateway client init failed", "err", err.Error())
			os.Exit(1)
		}
		languageModelClient = client
		logger.Info("llm gateway client wired", "url", gatewayURL)
	}

	var graphEvidenceCache mcp.GraphEvidenceCache
	if graphCacheClient != nil && graphEvidenceTTL > 0 {
		graphEvidenceCache = mcp.NewRedisGraphEvidenceCacheWithLimits(
			graphCacheClient.Underlying(),
			logger,
			graphInspectionCacheMaxBytes,
			codegraphContextCacheMaxBytes,
		)
		logger.Info(
			"graph evidence cache wired",
			"ttl", graphEvidenceTTL.String(),
			"graphInspectionMaxBytes", graphInspectionCacheMaxBytes,
			"codegraphContextMaxBytes", codegraphContextCacheMaxBytes,
		)
	}

	mcpBackend := mcp.NewBackend(mcp.Config{
		Queries:              queries,
		SearchBackend:        searchBackend,
		GraphReader:          graphReader,
		Paths:                repoPathsCfg,
		Logger:               logger,
		EncryptionKey:        encryptionKey,
		AskTimeout:           envDurationSeconds("CODEINTEL_ASK_TIMEOUT_SECONDS", 5*time.Minute),
		AskMaxAttempts:       envInt("CODEINTEL_ASK_MAX_ATTEMPTS", 2),
		GraphEvidenceCache:   graphEvidenceCache,
		GraphEvidenceTTL:     graphEvidenceTTL,
		CompactGraphTimeout:  envDurationSeconds("CODEINTEL_COMPACT_GRAPH_TIMEOUT_SECONDS", 5*time.Second),
		LanguageModelClient:  languageModelClient,
		AllowedModelBaseURLs: splitEnvList(os.Getenv("CODEINTEL_ALLOWED_MODEL_BASE_URLS")),
	})
	mcpTools := []string{"compare_branches", "list_language_models", "list_repos", "read_file"}
	if searchBackend != nil {
		mcpTools = append(mcpTools, "find_symbol_definitions", "find_symbol_references", "grep")
	}
	if searchBackend != nil && graphReader != nil {
		mcpTools = append(mcpTools, "ask_codebase")
		mcpTools = append(mcpTools, "graph_callers", "graph_callees", "graph_impact", "graph_minimal_context", "graph_path", "graph_status", "inspect_code_graph")
	}
	logger.Info("mcp backend wired", "tools", mcpTools)

	server := api.NewServer(api.Config{
		Logger:                logger,
		Queries:               queries,
		EncryptionKey:         encryptionKey,
		Metrics:               metrics,
		DBPinger:              pool,
		SingleTenantOrgID:     singleTenantOrgID,
		RateLimiter:           rateLimiter,
		CORS:                  corsMW,
		AuditEmitter:          auditEmitter,
		ConnectionSyncer:      connSyncer,
		RepoIndexer:           repoIndexer,
		RepoStatusFetcher:     api.NewPgxRepoStatusFetcher(pool.Pool),
		ZoektWebserverUrls:    zoektEndpoints,
		ZoektStorageLayout:    repoPathsCfg.ZoektStorageLayout,
		ZoektEFSRoot:          repoPathsCfg.ZoektEFSRoot,
		ZoektDataCacheDir:     repoPathsCfg.DataCacheDir,
		SearchBackend:         searchBackend,
		MCPBackend:            mcpBackend,
		ChatBackend:           mcpBackend,
		AtomControlPlaneToken: os.Getenv("CODEINTEL_ATOM_CONTROL_PLANE_TOKEN"),
	})

	// HTTP server timeouts catch slow-loris clients while allowing
	// long-running MCP/chat research requests to finish. Operators
	// can lower WriteTimeout for pure stateless deployments.
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           server.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      envDurationSeconds("CODEINTEL_WRITE_TIMEOUT_SECONDS", 6*time.Minute),
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("codeintel-app listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		logger.Error("server fatal", "err", err.Error())
		os.Exit(1)
	case <-ctx.Done():
		logger.Info("codeintel-app shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown error", "err", err.Error())
		os.Exit(1)
	}
}

// envFloat returns the float64 value of the named env var, or the
// supplied default when unset / unparseable. Quiet about parse
// failures by design — operators see misconfigurations via the
// "rate limiter enabled / not enabled" log on boot.
func envFloat(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	parsed, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return parsed
}

func envBool(name string) bool {
	return strings.EqualFold(os.Getenv(name), "true")
}

func envInt(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed <= 0 {
		return def
	}
	return parsed
}

func envDurationSeconds(name string, def time.Duration) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	parsed, err := strconv.Atoi(v)
	if err != nil || parsed <= 0 {
		return def
	}
	return time.Duration(parsed) * time.Second
}

func splitEnvList(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}
