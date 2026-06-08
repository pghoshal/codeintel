// codeintel-backend is the slow-HTTP + queue-worker binary in the
// 3-binary topology described in docs/codeintel-architecture-rules.md.
// Today it serves gRPC surfaces for audit-event forwarding and
// typed index-plan writes, plus an asynq Server consumer
// subscribed to the Go-owned queues
// (connection-sync, repo-index, code-graph-write, and the
// account/repo permission sync queues). Feature handlers are
// registered below as each port slice lands.
//
// The legacy backend (packages/backend/src/index.ts) branched its
// subsystem set on a BACKEND_MODE env var (control / indexer /
// all). The codeintel port collapses that axis into the binary
// topology: codeintel-backend ≡ legacy's control + non-indexer
// modes; the indexer mode lives in codeintel-indexer-rs (separate
// Rust binary, future slice). No runtime mode switch is needed.
//
// Subsystem startup order:
//
//  1. Postgres pool (required — CODEINTEL_DATABASE_URL).
//  2. Redis client (required — CODEINTEL_REDIS_URL).
//  3. Nebula graph writer (optional — CODEINTEL_NEBULA_ADDR;
//     unset → UnconfiguredCodeGraphStore + warning logged).
//  4. Audit gRPC server (always-on; depends on Postgres pool).
//  5. asynq Server (always-on; depends on Redis client) with an
//     ServeMux subscribed to every Go-owned queue in
//     asynqueues.DefaultPriorities().
//
// Each subsystem emits a "<name> ready" structured log line at
// boot so an operator running `make stack-up` then
// `go run ./cmd/codeintel-backend` sees a one-line confirmation
// per dependency.
//
// Future surfaces (analytics ingest, connection-sync coordinator,
// LLM gateway, semantic-extractor queue consumer) land in this
// same binary per the topology; see internal/backend/* for the
// per-feature packages.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"codeintel/internal/backend/astartifact"
	backendaudit "codeintel/internal/backend/audit"
	"codeintel/internal/backend/codegraphwriter"
	"codeintel/internal/backend/connectionmanager"
	"codeintel/internal/backend/graphstore"
	"codeintel/internal/backend/indexartifacts"
	"codeintel/internal/backend/indexcore"
	"codeintel/internal/backend/indexexecutor"
	"codeintel/internal/backend/indexplanwriter"
	"codeintel/internal/backend/indexsubjobs"
	"codeintel/internal/backend/indexsubjobtask"
	"codeintel/internal/backend/llmgateway"
	"codeintel/internal/backend/repoindexmanager"
	"codeintel/internal/backend/scheduler"
	"codeintel/internal/backend/schedulerhttp"
	"codeintel/internal/backend/scipartifact"
	"codeintel/internal/backend/workerclasses"
	"codeintel/internal/backend/zoektartifact"
	"codeintel/pkg/asynqbridge"
	"codeintel/pkg/asynqueues"
	"codeintel/pkg/dbpool"
	"codeintel/pkg/redisclient"
	"codeintel/pkg/repopaths"
	codeintelv1 "codeintel/proto/codeintel/v1"

	"github.com/hibiken/asynq"
	"google.golang.org/grpc"
)

const (
	defaultListenAddr       = "127.0.0.1:3101"
	defaultLLMGatewayAddr   = ""
	defaultControlAddr      = ""
	defaultShutdown         = 15 * time.Second
	defaultAsynqConcurrency = 8
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(logger); err != nil {
		logger.Error("backend exited with error", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	listenAddr := envOrDefault("CODEINTEL_BACKEND_GRPC_ADDR", defaultListenAddr)
	dsn := os.Getenv("CODEINTEL_DATABASE_URL")
	if dsn == "" {
		return errors.New("CODEINTEL_DATABASE_URL is required")
	}
	databaseMaxConns, _, err := envOptionalPositiveInt("CODEINTEL_DATABASE_MAX_CONNS")
	if err != nil {
		return err
	}
	databaseConnectTimeoutSeconds, connectTimeoutSet, err := envOptionalPositiveInt("CODEINTEL_DATABASE_CONNECT_TIMEOUT_SECONDS")
	if err != nil {
		return err
	}
	var connectTimeout time.Duration
	if connectTimeoutSet {
		connectTimeout = time.Duration(databaseConnectTimeoutSeconds) * time.Second
	}

	bootCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 1. Postgres pool. The pool's eager AcquireConn confirms the
	// DB is reachable; a startup error here is a hard fail.
	pool, err := dbpool.NewPool(bootCtx, dbpool.Config{
		DSN:            dsn,
		MaxConns:       int32(databaseMaxConns),
		ConnectTimeout: connectTimeout,
	})
	if err != nil {
		return fmt.Errorf("dbpool: %w", err)
	}
	defer pool.Close()
	logger.Info("postgres pool ready",
		"max_conns", pool.Pool.Config().MaxConns,
		"min_conns", pool.Pool.Config().MinConns,
	)

	// 2. Redis client. CODEINTEL_REDIS_URL is required because
	// the asynq Server can't subscribe without it. Eager Ping
	// inside redisclient.New surfaces a Redis-down state at
	// startup instead of at first task dispatch.
	redisCfg, err := redisclient.LoadConfigFromEnv()
	if err != nil {
		return fmt.Errorf("redis config: %w", err)
	}
	redisCli, err := redisclient.New(bootCtx, redisCfg, logger)
	if err != nil {
		return fmt.Errorf("redis client: %w", err)
	}
	defer redisCli.Close()
	logger.Info("redis client ready", "cfg", redisCli.Config())
	redisPolicy := redisclient.LoadOperationalPolicyFromEnv()
	redisSnapshot, err := redisCli.EnforceOperationalPolicy(bootCtx, redisPolicy)
	if err != nil {
		return fmt.Errorf("redis operational policy: %w", err)
	}
	logger.Info("redis operational policy checked",
		"require_bounded_memory", redisPolicy.RequireBoundedMemory,
		"maxmemory_bytes", redisSnapshot.MaxMemoryBytes,
		"used_memory_bytes", redisSnapshot.UsedMemoryBytes,
		"eviction_policy", redisSnapshot.EvictionPolicy,
		"key_count", redisSnapshot.KeyCount,
	)

	// 3. Nebula graph writer (optional). CreateFromEnv emits its
	// own ready/disabled log line — the env-unset path is a valid
	// deployment that returns an UnconfiguredCodeGraphStore. The
	// store is passed to the code-graph-write queue consumer
	// below; the unconfigured variant records SKIPPED metadata
	// without failing backend startup.
	graphStore, graphCloser := graphstore.CreateFromEnv(bootCtx, logger)
	defer graphCloser.Close()

	// 4. Audit gRPC server. Depends on Postgres pool.
	auditServer := backendaudit.NewServer(pool.Pool)
	grpcServer := grpc.NewServer()
	codeintelv1.RegisterAuditServiceServer(grpcServer, auditServer)
	indexSubjobStore := indexsubjobs.NewStore(pool.Pool)
	indexPlanServer := indexplanwriter.NewServer(pool.Pool, indexSubjobStore, logger)
	codeintelv1.RegisterIndexPlanServiceServer(grpcServer, indexPlanServer)

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", listenAddr, err)
	}
	logger.Info("backend gRPC server ready", "addr", listenAddr, "services", []string{"AuditService", "IndexPlanService"})

	// 5. asynq Server. The ServeMux below registers every handler
	// currently ported into the Go backend. Subscribed queues whose
	// handlers have not landed yet intentionally remain unregistered
	// so asynq records a clear "no handler" diagnostic instead of a
	// silent drop.
	asynqOpt, err := asynqbridge.RedisOptFromURL(redisCfg.URL)
	if err != nil {
		return fmt.Errorf("asynq redis opt: %w", err)
	}
	executorConsumersEnabled, err := envBoolOrDefault("CODEINTEL_INDEX_EXECUTOR_CONSUMERS_ENABLED", false)
	if err != nil {
		return err
	}
	queueMode, err := backendQueueMode(os.Getenv("CODEINTEL_BACKEND_QUEUE_MODE"), executorConsumersEnabled)
	if err != nil {
		return err
	}
	llmGatewayAddr := envOrDefault("CODEINTEL_BACKEND_LLM_ADDR", defaultLLMGatewayAddr)
	llmGatewayToken := strings.TrimSpace(os.Getenv("CODEINTEL_BACKEND_LLM_TOKEN"))
	controlAddr := envOrDefault("CODEINTEL_BACKEND_CONTROL_ADDR", defaultControlAddr)
	controlToken := strings.TrimSpace(os.Getenv("CODEINTEL_BACKEND_CONTROL_TOKEN"))
	llmTimeoutSeconds, err := envIntOrDefault("CODEINTEL_BACKEND_LLM_TIMEOUT_SECONDS", int((6*time.Minute)/time.Second))
	if err != nil {
		return err
	}
	llmTaskMaxRetry, err := envIntOrDefault("CODEINTEL_BACKEND_LLM_TASK_MAX_RETRY", 3)
	if err != nil {
		return err
	}
	if strings.TrimSpace(llmGatewayAddr) != "" && !queueModeConsumesControl(queueMode) {
		return errors.New("CODEINTEL_BACKEND_LLM_ADDR requires CODEINTEL_BACKEND_QUEUE_MODE=control or all so durable LLM completion tasks are consumed")
	}
	if strings.TrimSpace(llmGatewayAddr) != "" && llmGatewayToken == "" {
		return errors.New("CODEINTEL_BACKEND_LLM_ADDR requires CODEINTEL_BACKEND_LLM_TOKEN")
	}
	if strings.TrimSpace(controlAddr) != "" && !queueModeConsumesControl(queueMode) {
		return errors.New("CODEINTEL_BACKEND_CONTROL_ADDR requires CODEINTEL_BACKEND_QUEUE_MODE=control or all so scheduled mutation jobs are consumed")
	}
	if strings.TrimSpace(controlAddr) != "" && controlToken == "" {
		return errors.New("CODEINTEL_BACKEND_CONTROL_ADDR requires CODEINTEL_BACKEND_CONTROL_TOKEN")
	}
	asynqQueues := map[string]int{}
	if queueModeConsumesControl(queueMode) {
		for queue, priority := range asynqueues.DefaultPriorities() {
			asynqQueues[queue] = priority
		}
	}
	shutdownSeconds, err := envIntOrDefault("CODEINTEL_BACKEND_SHUTDOWN_TIMEOUT_SECONDS",
		int(defaultShutdown/time.Second))
	if err != nil {
		return err
	}
	asynqShutdown := time.Duration(shutdownSeconds) * time.Second
	repoPathsCfg := repopaths.LoadConfigFromEnv()

	var executorRunners []*indexexecutor.GRPCRunner
	executorHandlers := map[string]func(context.Context, *asynq.Task) error{}
	var executorClasses []workerclasses.WorkerClass
	if executorConsumersEnabled {
		if !queueModeConsumesExecutor(queueMode) {
			return errors.New("CODEINTEL_INDEX_EXECUTOR_CONSUMERS_ENABLED=true requires CODEINTEL_BACKEND_QUEUE_MODE=executor or all")
		}
		executorClasses, err = executorClassesFromEnv(os.Getenv("CODEINTEL_INDEX_EXECUTOR_CLASSES"))
		if err != nil {
			return err
		}
		if len(executorClasses) > 1 && os.Getenv("CODEINTEL_INDEX_EXECUTOR_GRPC_ADDR") != "" {
			return errors.New("CODEINTEL_INDEX_EXECUTOR_GRPC_ADDR is only allowed for a single executor class; use class-specific CODEINTEL_INDEX_EXECUTOR_GRPC_ADDR_<CLASS> variables")
		}
		executorTimeoutSeconds, err := envIntOrDefault("CODEINTEL_INDEX_EXECUTOR_RPC_TIMEOUT_SECONDS", int((2*time.Hour)/time.Second))
		if err != nil {
			return err
		}
		leaseSeconds, err := envIntOrDefault("CODEINTEL_INDEX_EXECUTOR_LEASE_SECONDS", 120)
		if err != nil {
			return err
		}
		heartbeatSeconds, err := envIntOrDefault("CODEINTEL_INDEX_EXECUTOR_HEARTBEAT_SECONDS", 30)
		if err != nil {
			return err
		}
		artifactRoot := os.Getenv("CODEINTEL_INDEX_ARTIFACT_ROOT")
		artifactValidator, err := indexexecutor.NewFilesystemArtifactValidator(artifactRoot)
		if err != nil {
			return err
		}
		scipStore, err := scipartifact.NewStore(pool.Pool, repoPathsCfg, artifactRoot)
		if err != nil {
			return err
		}
		astStore, err := astartifact.NewStore(pool.Pool, graphStore, logger, artifactRoot)
		if err != nil {
			return err
		}
		zoektStore, err := zoektartifact.NewStore(pool.Pool, repoPathsCfg, artifactRoot)
		if err != nil {
			return err
		}
		artifactIngestor := indexartifacts.NewRouter(map[indexsubjobtask.Layer]indexartifacts.Ingestor{
			indexsubjobtask.LayerZoekt:         zoektStore,
			indexsubjobtask.LayerSCIP:          scipStore,
			indexsubjobtask.LayerASTTreeSitter: astStore,
		})
		for _, class := range executorClasses {
			addr, err := executorGRPCAddrForClass(class, len(executorClasses))
			if err != nil {
				return err
			}
			runner, err := indexexecutor.NewGRPCRunner(addr, time.Duration(executorTimeoutSeconds)*time.Second)
			if err != nil {
				return err
			}
			executorRunners = append(executorRunners, runner)
			handler, err := indexexecutor.NewHandler(indexSubjobStore, runner, logger, indexexecutor.Config{
				LeaseDuration:     time.Duration(leaseSeconds) * time.Second,
				HeartbeatInterval: time.Duration(heartbeatSeconds) * time.Second,
				ArtifactValidator: artifactValidator,
				ArtifactIngestor:  artifactIngestor,
			})
			if err != nil {
				return err
			}
			if class.Name == "core" {
				if err := requireConfiguredGraphStore(graphStore); err != nil {
					return err
				}
				coreHandler, err := indexcore.NewHandler(pool.Pool, indexSubjobStore, logger, indexcore.Config{
					LeaseDuration: time.Duration(leaseSeconds) * time.Second,
					Graph:         graphStore,
				})
				if err != nil {
					return err
				}
				executorHandlers[class.QueueName] = func(ctx context.Context, task *asynq.Task) error {
					payload, err := indexsubjobtask.Unmarshal(task.Payload())
					if err != nil {
						return handler.Handle(ctx, task)
					}
					if payload.Layer == indexsubjobtask.LayerZoekt || payload.Layer == indexsubjobtask.LayerASTTreeSitter {
						return handler.Handle(ctx, task)
					}
					return coreHandler.Handle(ctx, task)
				}
			} else {
				executorHandlers[class.QueueName] = handler.Handle
			}
			asynqQueues[class.QueueName] = 1
		}
	}
	defer func() {
		for _, runner := range executorRunners {
			_ = runner.Close()
		}
	}()
	asynqConcurrency, err := backendAsynqConcurrency(queueMode, executorClasses, databaseMaxConns)
	if err != nil {
		return err
	}

	asynqServer := asynq.NewServer(asynqOpt, asynq.Config{
		Concurrency:     asynqConcurrency,
		Queues:          asynqQueues,
		Logger:          &asynqbridge.SlogLogger{Base: logger},
		ShutdownTimeout: asynqShutdown,
	})
	asynqMux := asynq.NewServeMux()
	// Phase B.1d: register the connection-sync worker handler on
	// the connection-sync-queue.
	if queueModeConsumesControl(queueMode) {
		connSyncHandler := connectionmanager.NewHandler(pool.Pool, logger)
		asynqMux.HandleFunc(asynqueues.QueueConnectionSync, connSyncHandler.AsynqHandlerFunc())
	}
	// Phase C.2 / C.3 / C.4a: register the repo-index worker on
	// the repo-index-queue. CLEANUP + REMOVE_INDEX do the full
	// DB cascade + filesystem + Zoekt shard scrub. INDEX runs a
	// real go-git clone into the resolved working dir and stamps
	// Repo.indexedCommitHash with the observed HEAD SHA. The
	// Zoekt shard write + SCIP extraction layers (C.4b / C.4c)
	// are not yet wired — search hits / xref queries return
	// nothing today.
	repoIndexStore := repoindexmanager.NewStore(pool.Pool)
	logger.Info("repo-index paths configured",
		"data_cache_dir", repoPathsCfg.DataCacheDir,
		"zoekt_efs_root", repoPathsCfg.ZoektEFSRoot,
		"zoekt_storage_layout", repoPathsCfg.ZoektStorageLayout,
	)
	if queueModeConsumesControl(queueMode) {
		repoIndexHandler := repoindexmanager.NewHandlerWithGraphRetirer(repoIndexStore, repoPathsCfg, graphStore, logger)
		asynqMux.HandleFunc(asynqueues.QueueRepoIndex, repoIndexHandler.AsynqHandlerFunc())
		codeGraphWriteHandler := codegraphwriter.NewHandler(pool.Pool, graphStore, logger)
		asynqMux.HandleFunc(asynqueues.QueueCodeGraphWrite, codeGraphWriteHandler.AsynqHandlerFunc())
		llmProcessor := llmgateway.NewProcessor(llmgateway.ProcessorConfig{
			Store:   llmgateway.NewStore(pool.Pool),
			Logger:  logger,
			Timeout: time.Duration(llmTimeoutSeconds) * time.Second,
		})
		asynqMux.HandleFunc(asynqueues.QueueLLMCompletion, llmProcessor.Handle)
	}
	if executorConsumersEnabled {
		for _, class := range executorClasses {
			asynqMux.HandleFunc(class.QueueName, executorHandlers[class.QueueName])
		}
		logger.Info("index executor consumers ready",
			"classes", workerClassNames(executorClasses),
			"queues", workerClassQueues(executorClasses),
		)
	}

	asynqErrCh := make(chan error, 1)
	go func() {
		if err := asynqServer.Run(asynqMux); err != nil && !errors.Is(err, asynq.ErrServerClosed) {
			asynqErrCh <- err
		}
	}()
	logger.Info("asynq server ready",
		"queues", sortedQueueNames(asynqQueues),
		"concurrency", asynqConcurrency,
		"shutdown_timeout", asynqShutdown,
	)

	backgroundCtx, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	dispatcherEnabled, err := envBoolOrDefault("CODEINTEL_INDEX_SUBJOB_DISPATCH_ENABLED", false)
	if err != nil {
		return err
	}
	var dispatchClient *asynq.Client
	if dispatcherEnabled {
		intervalSeconds, err := envIntOrDefault("CODEINTEL_INDEX_SUBJOB_DISPATCH_INTERVAL_SECONDS", 5)
		if err != nil {
			return err
		}
		leaseLimit, err := envIntOrDefault("CODEINTEL_INDEX_SUBJOB_LEASE_SWEEP_LIMIT", 200)
		if err != nil {
			return err
		}
		dispatchLimit, err := envIntOrDefault("CODEINTEL_INDEX_SUBJOB_DISPATCH_LIMIT", 200)
		if err != nil {
			return err
		}
		dispatchClient = asynq.NewClient(asynqOpt)
		defer dispatchClient.Close()
		dispatcher := indexsubjobs.NewDispatcher(indexSubjobStore, dispatchClient)
		if executorConsumersEnabled {
			rescueCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			rescued, err := rescueStartupIndexSubjobLeases(rescueCtx, indexSubjobStore, int32(leaseLimit), logger)
			cancel()
			if err != nil {
				return err
			}
			if rescued != 0 {
				logger.Warn("rescued abandoned index subjob leases from previous backend process", "count", rescued)
			}
		}
		go runIndexSubjobDispatchLoop(
			backgroundCtx,
			dispatcher,
			time.Duration(intervalSeconds)*time.Second,
			int32(leaseLimit),
			int32(dispatchLimit),
			logger,
		)
		logger.Info("index subjob dispatcher ready",
			"interval", time.Duration(intervalSeconds)*time.Second,
			"lease_sweep_limit", leaseLimit,
			"dispatch_limit", dispatchLimit,
		)
	}

	// gRPC Serve goroutine.
	grpcErrCh := make(chan error, 1)
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			grpcErrCh <- err
		}
	}()
	logger.Info("codeintel-backend listening", "addr", listenAddr)

	var llmHTTPServer *http.Server
	var llmAsynqClient *asynq.Client
	var controlHTTPServer *http.Server
	var controlAsynqClient *asynq.Client
	llmErrCh := make(chan error, 1)
	controlErrCh := make(chan error, 1)
	if strings.TrimSpace(llmGatewayAddr) != "" {
		llmAsynqClient = asynq.NewClient(asynqOpt)
		defer llmAsynqClient.Close()
		llmServer := llmgateway.NewServer(llmgateway.Config{
			Store:        llmgateway.NewStore(pool.Pool),
			Logger:       logger,
			Token:        llmGatewayToken,
			Timeout:      time.Duration(llmTimeoutSeconds) * time.Second,
			Enqueuer:     llmAsynqClient,
			MaxTaskRetry: llmTaskMaxRetry,
		})
		llmHTTPServer = &http.Server{
			Addr:              llmGatewayAddr,
			Handler:           llmServer,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      time.Duration(llmTimeoutSeconds+30) * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			if err := llmHTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				llmErrCh <- err
			}
		}()
		logger.Info("backend LLM gateway ready", "addr", llmGatewayAddr)
	}
	if strings.TrimSpace(controlAddr) != "" {
		controlAsynqClient = asynq.NewClient(asynqOpt)
		defer controlAsynqClient.Close()
		controlServer := schedulerhttp.NewServer(schedulerhttp.Config{
			Scheduler: scheduler.NewService(pool.Pool, controlAsynqClient),
			Logger:    logger,
			Token:     controlToken,
		})
		controlHTTPServer = &http.Server{
			Addr:              controlAddr,
			Handler:           controlServer,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		go func() {
			if err := controlHTTPServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				controlErrCh <- err
			}
		}()
		logger.Info("backend control scheduler ready", "addr", controlAddr)
	}

	// Graceful shutdown on SIGINT / SIGTERM. The gRPC server's
	// GracefulStop blocks until every in-flight RPC completes; if
	// the shutdown deadline expires first, force-stop kills any
	// remaining streams. asynq.Server.Shutdown drains in-flight
	// tasks up to its own ShutdownTimeout (set above).
	shutdownTimeout := asynqShutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case sig := <-sigCh:
		logger.Info("backend received signal, draining", "sig", sig.String(), "timeout", shutdownTimeout)
	case err := <-grpcErrCh:
		return fmt.Errorf("grpc.Serve: %w", err)
	case err := <-asynqErrCh:
		return fmt.Errorf("asynq.Server: %w", err)
	case err := <-llmErrCh:
		return fmt.Errorf("llm gateway: %w", err)
	case err := <-controlErrCh:
		return fmt.Errorf("control scheduler: %w", err)
	}
	stopBackground()

	// Stop asynq first so it stops pulling new tasks while gRPC
	// finishes its in-flight calls. Both have their own internal
	// timeout; the outer time.After is a backstop.
	asynqServer.Shutdown()

	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()
	if llmHTTPServer != nil {
		httpCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := llmHTTPServer.Shutdown(httpCtx); err != nil {
			logger.Warn("llm gateway shutdown failed", "err", err)
		}
	}
	if controlHTTPServer != nil {
		httpCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := controlHTTPServer.Shutdown(httpCtx); err != nil {
			logger.Warn("control scheduler shutdown failed", "err", err)
		}
	}
	select {
	case <-stopped:
		logger.Info("backend drained cleanly")
	case <-time.After(shutdownTimeout):
		logger.Warn("backend drain timed out, forcing stop")
		grpcServer.Stop()
	}
	return nil
}

func envOrDefault(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// envIntOrDefault reads a positive-integer env var and returns
// the parsed value, the supplied default when unset, or a typed
// error when the value is present but malformed. An earlier
// draft silently fell back to the default on parse failure — an
// operator footgun (CODEINTEL_BACKEND_ASYNQ_CONCURRENCY=abc
// would silently boot with 8 workers instead of refusing the
// misconfig).
func envIntOrDefault(name string, def int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid integer", name, raw)
	}
	if v <= 0 {
		return 0, fmt.Errorf("%s=%q must be a positive integer", name, raw)
	}
	return v, nil
}

func envOptionalPositiveInt(name string) (int, bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, false, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, true, fmt.Errorf("%s=%q is not a valid integer", name, raw)
	}
	if v <= 0 {
		return 0, true, fmt.Errorf("%s=%q must be a positive integer", name, raw)
	}
	return v, true, nil
}

func envBoolOrDefault(name string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "t", "yes", "y", "on":
		return true, nil
	case "0", "false", "f", "no", "n", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s=%q is not a valid boolean", name, raw)
	}
}

func backendQueueMode(raw string, executorConsumersEnabled bool) (string, error) {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		if executorConsumersEnabled {
			return "executor", nil
		}
		return "control", nil
	}
	switch raw {
	case "control", "executor", "all":
		if (raw == "executor" || raw == "all") && !executorConsumersEnabled {
			return "", fmt.Errorf("CODEINTEL_BACKEND_QUEUE_MODE=%q requires CODEINTEL_INDEX_EXECUTOR_CONSUMERS_ENABLED=true", raw)
		}
		return raw, nil
	default:
		return "", fmt.Errorf("CODEINTEL_BACKEND_QUEUE_MODE=%q must be one of control, executor, all", raw)
	}
}

func queueModeConsumesControl(mode string) bool {
	return mode == "control" || mode == "all"
}

func queueModeConsumesExecutor(mode string) bool {
	return mode == "executor" || mode == "all"
}

func backendAsynqConcurrency(queueMode string, executorClasses []workerclasses.WorkerClass, databaseMaxConns int) (int, error) {
	if value, ok, err := envOptionalPositiveInt("CODEINTEL_BACKEND_ASYNQ_CONCURRENCY"); ok || err != nil {
		return value, err
	}
	if queueMode == "executor" && len(executorClasses) > 0 {
		if databaseMaxConns > 0 && totalExecutorConcurrency(executorClasses) > max(1, databaseMaxConns-2) {
			return 0, fmt.Errorf("CODEINTEL_BACKEND_ASYNQ_CONCURRENCY unset would derive executor concurrency %d, exceeding CODEINTEL_DATABASE_MAX_CONNS safety budget %d; set a lower CODEINTEL_BACKEND_ASYNQ_CONCURRENCY or raise CODEINTEL_DATABASE_MAX_CONNS", totalExecutorConcurrency(executorClasses), databaseMaxConns)
		}
		total := 0
		for _, class := range executorClasses {
			if class.PodConcurrency > 0 {
				total += class.PodConcurrency
			}
		}
		if total > 0 {
			return total, nil
		}
	}
	return defaultAsynqConcurrency, nil
}

func totalExecutorConcurrency(executorClasses []workerclasses.WorkerClass) int {
	total := 0
	for _, class := range executorClasses {
		if class.PodConcurrency > 0 {
			total += class.PodConcurrency
		}
	}
	return total
}

func executorClassesFromEnv(raw string) ([]workerclasses.WorkerClass, error) {
	parts := strings.Split(raw, ",")
	seen := map[string]struct{}{}
	var out []workerclasses.WorkerClass
	for _, part := range parts {
		name := strings.TrimSpace(part)
		if name == "" {
			continue
		}
		class, ok := workerclasses.ByName(name)
		if !ok {
			return nil, fmt.Errorf("CODEINTEL_INDEX_EXECUTOR_CLASSES contains unknown worker class %q", name)
		}
		if _, ok := seen[class.Name]; ok {
			continue
		}
		seen[class.Name] = struct{}{}
		out = append(out, class)
	}
	if len(out) == 0 {
		return nil, errors.New("CODEINTEL_INDEX_EXECUTOR_CLASSES must name at least one worker class when executor consumers are enabled")
	}
	return out, nil
}

func executorGRPCAddrForClass(class workerclasses.WorkerClass, enabledClassCount int) (string, error) {
	envName := "CODEINTEL_INDEX_EXECUTOR_GRPC_ADDR_" + workerClassEnvSuffix(class.Name)
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		return value, nil
	}
	if enabledClassCount == 1 {
		if value := strings.TrimSpace(os.Getenv("CODEINTEL_INDEX_EXECUTOR_GRPC_ADDR")); value != "" {
			return value, nil
		}
	}
	return "", fmt.Errorf("%s is required for worker class %q", envName, class.Name)
}

func workerClassEnvSuffix(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 'a' + 'A')
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

func workerClassNames(classes []workerclasses.WorkerClass) []string {
	out := make([]string, 0, len(classes))
	for _, class := range classes {
		out = append(out, class.Name)
	}
	return out
}

func workerClassQueues(classes []workerclasses.WorkerClass) []string {
	out := make([]string, 0, len(classes))
	for _, class := range classes {
		out = append(out, class.QueueName)
	}
	return out
}

func requireConfiguredGraphStore(store graphstore.Store) error {
	switch store.(type) {
	case *graphstore.UnconfiguredCodeGraphStore:
		return errors.New("CODEINTEL_INDEX_EXECUTOR_CLASSES=core requires CODEINTEL_NEBULA_ADDR so AST graph artifacts cannot be silently skipped")
	default:
		return nil
	}
}

type startupLeaseRescuer interface {
	RequeueLeasesForOwnerPrefixes(context.Context, []string, time.Time, int32) (int64, error)
}

func rescueStartupIndexSubjobLeases(ctx context.Context, store startupLeaseRescuer, limit int32, logger *slog.Logger) (int64, error) {
	if store == nil || limit <= 0 {
		return 0, nil
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		if logger != nil {
			logger.Warn("skipping startup index subjob lease rescue because hostname is unavailable", "err", err)
		}
		return 0, nil
	}
	prefixes := []string{
		"index-executor-" + hostname + "-",
		"index-core-" + hostname + "-",
	}
	rescued, err := store.RequeueLeasesForOwnerPrefixes(ctx, prefixes, time.Now().UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("startup index subjob lease rescue: %w", err)
	}
	return rescued, nil
}

func sortedQueueNames(queues map[string]int) []string {
	out := make([]string, 0, len(queues))
	for name := range queues {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func runIndexSubjobDispatchLoop(ctx context.Context, dispatcher *indexsubjobs.Dispatcher, interval time.Duration, leaseLimit, dispatchLimit int32, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	logger = logger.With("component", "index-subjob-dispatcher")
	runOnce := func() {
		timeout := interval
		if timeout < 10*time.Second {
			timeout = 10 * time.Second
		}
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		stats, err := dispatcher.RequeueExpiredAndDispatch(runCtx, time.Now().UTC(), leaseLimit, dispatchLimit)
		if err != nil {
			logger.Error("index subjob dispatch failed", "err", err)
			return
		}
		if stats.ExpiredRequeued != 0 || stats.Enqueued != 0 || stats.Duplicates != 0 {
			logger.Info("index subjob dispatch completed",
				"expired_requeued", stats.ExpiredRequeued,
				"scanned", stats.Scanned,
				"enqueued", stats.Enqueued,
				"duplicates", stats.Duplicates,
			)
		}
	}
	runOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			runOnce()
		case <-ctx.Done():
			logger.Info("index subjob dispatcher stopped")
			return
		}
	}
}
