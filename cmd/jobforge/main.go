// Command jobforge is the single-binary entry point for the JobForge platform.
// It supports subcommands: migrate (database migration), api (HTTP control plane),
// scheduler (job promotion), worker (task execution), gateway (gRPC worker gateway),
// publisher (outbox event publisher), consumer (reference event consumer), and
// ctl (operational CLI client).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/ctl"
	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/eventconsumer"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/migrate"
	"github.com/xjfyrh/jobforge/internal/notify"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/outbox"
	"github.com/xjfyrh/jobforge/internal/scheduler"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	"github.com/xjfyrh/jobforge/internal/worker"
	"github.com/xjfyrh/jobforge/internal/worker/demo"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: jobforge <migrate|api|scheduler|gateway|worker|publisher|consumer|ctl>\n")
		os.Exit(1)
	}

	// Initialize observability (tracing + metrics) for all subcommands.
	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	obsCfg := observability.Config{
		ServiceName:    "jobforge",
		ServiceVersion: "0.1.0",
		Environment:    "development",
		ExporterType:   cfg.OTelExporterType,
		SampleRatio:    cfg.OTelSampleRatio,
		MetricsAddr:    cfg.MetricsAddr,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	traceShutdown, err := observability.SetupTracing(ctx, obsCfg)
	if err != nil {
		logger.Error("setup tracing", "error", err)
		os.Exit(1)
	}
	defer func() { _ = traceShutdown(context.Background()) }()

	metrics, metricsShutdown, err := observability.SetupMetrics(ctx, nil)
	if err != nil {
		logger.Error("setup metrics", "error", err)
		os.Exit(1)
	}
	defer func() { _ = metricsShutdown(context.Background()) }()

	// Start debug server (pprof + /metrics) in background.
	go func() {
		if err := observability.StartDebugServer(ctx, obsCfg.MetricsAddr, logger); err != nil {
			logger.Error("debug server failed", "error", err)
		}
	}()

	switch os.Args[1] {
	case "migrate":
		if err := runMigrate(logger); err != nil {
			logger.Error("migrate failed", "error", err)
			os.Exit(1)
		}
	case "api":
		if err := runAPI(ctx, logger, cfg, metrics); err != nil {
			logger.Error("api server failed", "error", err)
			os.Exit(1)
		}
	case "scheduler":
		if err := runScheduler(ctx, logger, cfg, metrics); err != nil {
			logger.Error("scheduler failed", "error", err)
			os.Exit(1)
		}
	case "worker":
		if err := runWorker(ctx, logger, cfg, metrics); err != nil {
			logger.Error("worker failed", "error", err)
			os.Exit(1)
		}
	case "gateway":
		if err := runGateway(ctx, logger, cfg, metrics); err != nil {
			logger.Error("gateway failed", "error", err)
			os.Exit(1)
		}
	case "publisher":
		if err := runPublisher(ctx, logger, cfg, metrics); err != nil {
			logger.Error("publisher failed", "error", err)
			os.Exit(1)
		}
	case "consumer":
		if err := runConsumer(ctx, logger, cfg, metrics); err != nil {
			logger.Error("consumer failed", "error", err)
			os.Exit(1)
		}
	case "ctl":
		if err := runCtl(ctx, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "ctl: %v\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nusage: jobforge <migrate|api|scheduler|gateway|worker|publisher|consumer|ctl>\n", os.Args[1])
		os.Exit(1)
	}
}

func runAPI(ctx context.Context, logger *slog.Logger, cfg *config.Config, metrics *observability.Metrics) error {
	// Create PostgreSQL connection pool.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}
	defer pool.Close()

	// Verify database connectivity.
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	logger.Info("connected to PostgreSQL", "max_conns", poolCfg.MaxConns)

	// Run database migrations.
	migrator := migrate.New(pool, logger)
	if err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Create store and router.
	jobStore := postgres.NewJobStore(pool)
	router := apihttp.NewRouter(jobStore, jobStore, cfg, logger, metrics)

	// Create HTTP server.
	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on signal.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting HTTP server", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case <-ctx.Done():
		logger.Info("shutting down")
	}

	// Graceful shutdown with timeout.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info("server stopped")
	return nil
}

func runScheduler(ctx context.Context, logger *slog.Logger, cfg *config.Config, metrics *observability.Metrics) error {
	// Create PostgreSQL connection pool.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = 10
	poolCfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	logger.Info("scheduler connected to PostgreSQL")

	// Run migrations.
	migrator := migrate.New(pool, logger)
	if err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Dedicated connection for advisory lock (session-level).
	lockConn, err := pgx.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect lock conn: %w", err)
	}
	defer func() { _ = lockConn.Close(context.Background()) }()

	// Create scheduler dependencies.
	schedStore := postgres.NewSchedulerStore(pool, lockConn)
	notifier := notify.NewNotifier(pool, logger)
	listener, err := notify.NewListener(cfg.DatabaseURL, logger)
	if err != nil {
		return fmt.Errorf("create listener: %w", err)
	}
	defer listener.Close()

	schedCfg := scheduler.Config{
		ScanInterval:      cfg.ScanInterval,
		PromoteBatchSize:  1000,
		LockRetryInterval: 2 * time.Second,
		// Stable per-process identity for the leadership lease row (ADR-0005).
		InstanceID:        fmt.Sprintf("scheduler-%s-%d", hostname(), os.Getpid()),
		LeadershipTimeout: cfg.SchedulerLeadershipTimeout,
		// Periodic quota counter reconcile + repair (PRD v0.3 FR-724).
		QuotaReconcileInterval: cfg.QuotaReconcileInterval,
	}

	sched := scheduler.New(schedStore, notifier, listener, schedCfg, logger, metrics)

	// Run scheduler until context cancelled.
	errCh := make(chan error, 1)
	go func() {
		errCh <- sched.Run(ctx)
	}()

	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			return fmt.Errorf("scheduler error: %w", err)
		}
	case <-ctx.Done():
		logger.Info("scheduler shutting down")
		<-errCh
	}

	logger.Info("scheduler stopped")
	return nil
}

func runGateway(ctx context.Context, logger *slog.Logger, cfg *config.Config, metrics *observability.Metrics) error {
	// Create PostgreSQL connection pool.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	logger.Info("gateway connected to PostgreSQL")

	// Run migrations.
	migrator := migrate.New(pool, logger)
	if err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Create store and listener for Poll long-poll.
	jobStore := postgres.NewJobStore(pool)
	listener, err := notify.NewListener(cfg.DatabaseURL, logger)
	if err != nil {
		return fmt.Errorf("create listener: %w", err)
	}
	defer listener.Close()

	// Create gRPC service and server.
	service := gatewaygrpc.NewWorkerService(jobStore, listener, cfg.LeaseTTL, cfg.HeartbeatInterval, cfg.TenantMaxInflight, cfg.TenantQuotaPrefilter, logger, metrics)
	server := gatewaygrpc.NewServer(service, logger)

	// Periodically sample jobforge_workers_active so the gauge decays when
	// workers crash: the freshness filter drops stale heartbeats and vanished
	// groups are recorded as 0.
	sampleInterval := cfg.LeaseTTL / 2
	if sampleInterval < 5*time.Second {
		sampleInterval = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				service.SampleWorkerCounts(ctx)
			}
		}
	}()

	// Run gateway until context cancelled.
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(cfg.GRPCAddr)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("gateway server error: %w", err)
	case <-ctx.Done():
		logger.Info("gateway shutting down")
		server.GracefulStop()
	}

	logger.Info("gateway stopped")
	return nil
}

// runPublisher starts the outbox publisher: it polls outbox_events for
// unpublished rows and publishes them via PostgreSQL LISTEN/NOTIFY
// (PRD v0.2 FR-610~613, ADR-0003). Multiple instances are safe: batches are
// claimed with FOR UPDATE SKIP LOCKED.
func runPublisher(ctx context.Context, logger *slog.Logger, cfg *config.Config, metrics *observability.Metrics) error {
	// Create PostgreSQL connection pool.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 1

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	logger.Info("publisher connected to PostgreSQL")

	// Run migrations.
	migrator := migrate.New(pool, logger)
	if err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Select the external event transport (PRD v0.3 FR-705, ADR-0006).
	// notify is the v0.2-compatible non-durable default; redis_streams
	// requires the durable-events deployment profile. Redis reachability is
	// NOT checked here on purpose: a down broker must only back off event
	// publishing, never stop the publisher from starting (NFR-303).
	var transport outbox.Transport
	switch cfg.OutboxTransport {
	case outbox.TransportRedisStreams:
		rt, err := outbox.NewRedisStreamsTransport(cfg.RedisURL, cfg.RedisStreamKey, cfg.RedisStreamMaxLen)
		if err != nil {
			return fmt.Errorf("create redis streams transport: %w", err)
		}
		defer func() { _ = rt.Close() }()
		transport = rt
	default:
		transport = outbox.NewNotifyTransport(pool, cfg.OutboxChannel, logger)
	}

	outboxStore := postgres.NewOutboxStore(pool)

	pubCfg := outbox.Config{
		PollInterval:    cfg.OutboxPollInterval,
		MaxIdleInterval: 30 * time.Second,
		BatchSize:       cfg.OutboxBatchSize,
		Retention:       cfg.OutboxRetention,
		CleanupInterval: time.Hour,
	}

	pub := outbox.New(outboxStore, transport, pubCfg, logger, metrics)

	// Run publisher until context cancelled.
	errCh := make(chan error, 1)
	go func() {
		errCh <- pub.Run(ctx)
	}()

	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			return fmt.Errorf("publisher error: %w", err)
		}
	case <-ctx.Done():
		logger.Info("publisher shutting down")
		<-errCh
	}

	logger.Info("publisher stopped")
	return nil
}

// runConsumer starts the fixed reference consumer. PostgreSQL commit always
// precedes Redis ACK, so an ACK failure or process exit leaves a recoverable
// pending entry that the event_id inbox absorbs on redelivery.
func runConsumer(
	ctx context.Context,
	logger *slog.Logger,
	cfg *config.Config,
	metrics *observability.Metrics,
) error {
	if cfg.RedisURL == "" {
		return fmt.Errorf("JOBFORGE_REDIS_URL is required for jobforge consumer")
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}

	migrator := migrate.New(pool, logger)
	if err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	processor, err := eventconsumer.NewInboxProcessor(
		pool, cfg.ConsumerGroup, eventconsumer.DemoEffectHandler{}, metrics,
	)
	if err != nil {
		return fmt.Errorf("create inbox processor: %w", err)
	}
	if err := processor.EnsureBinding(ctx); err != nil {
		return fmt.Errorf("bind consumer inbox: %w", err)
	}

	redisOptions, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		// Do not wrap the parser error: it may include credentials from the URL.
		return fmt.Errorf("parse JOBFORGE_REDIS_URL: invalid value")
	}
	redisClient := redis.NewClient(redisOptions)
	source, err := eventconsumer.NewRedisSource(
		redisClient,
		cfg.RedisStreamKey,
		cfg.ConsumerGroup,
		cfg.ConsumerName,
		cfg.ConsumerPoisonStream,
	)
	if err != nil {
		_ = redisClient.Close()
		return fmt.Errorf("create event source: %w", err)
	}
	consumer, err := eventconsumer.New(source, processor, eventconsumer.Config{
		Group:               cfg.ConsumerGroup,
		BlockTimeout:        cfg.ConsumerBlockTimeout,
		PendingScanInterval: cfg.ConsumerPendingScanInterval,
		PendingMinIdle:      cfg.ConsumerPendingMinIdle,
		ProcessTimeout:      cfg.ConsumerProcessTimeout,
		MaxDeliveries:       cfg.ConsumerMaxDeliveries,
		RetryBase:           cfg.ConsumerRetryBase,
		RetryMax:            cfg.ConsumerRetryMax,
	}, metrics, logger)
	if err != nil {
		_ = source.Close()
		return fmt.Errorf("create event consumer: %w", err)
	}
	defer func() { _ = consumer.Close() }()

	logger.Info("starting reference event consumer",
		"transport", source.Transport(),
		"consumer_group", cfg.ConsumerGroup,
	)
	if err := consumer.Start(ctx); err != nil {
		return err
	}
	if err := consumer.Wait(); err != nil {
		return fmt.Errorf("consumer loop: %w", err)
	}
	logger.Info("consumer stopped")
	return nil
}

func runWorker(ctx context.Context, logger *slog.Logger, cfg *config.Config, metrics *observability.Metrics) error {
	// Register demo handlers.
	registry := worker.NewRegistry()
	demo.RegisterAll(registry)
	// PageWise reindex demo handler (FR-403 / Appendix A).
	demo.RegisterPagewise(registry)

	// Determine gateway address.
	gatewayAddr := getEnvDefault("JOBFORGE_GATEWAY_ADDR", "localhost:9090")

	// Worker ID: prefer the JOBFORGE_WORKER_ID environment variable (set by
	// deploy/compose.yaml for stable, identifiable worker-1/worker-2 labels
	// in logs, metrics and the workers table); fall back to a random UUID.
	// JOBFORGE_WORKER_QUEUE accepts a comma-separated queue list; declaration
	// order is the queue priority for claims.
	queues := parseQueues(getEnvDefault("JOBFORGE_WORKER_QUEUE", "default"))
	if len(queues) == 0 {
		return fmt.Errorf("JOBFORGE_WORKER_QUEUE must contain at least one non-empty queue name")
	}
	var localHeartbeatInterval time.Duration
	if cfg.HeartbeatIntervalExplicit {
		localHeartbeatInterval = cfg.HeartbeatInterval
	}
	workerCfg := worker.RuntimeConfig{
		WorkerID:          getEnvDefault("JOBFORGE_WORKER_ID", domain.NewID()),
		InstanceID:        fmt.Sprintf("%s-%d", hostname(), os.Getpid()),
		Queues:            queues,
		Capacity:          5,
		GatewayAddr:       gatewayAddr,
		HeartbeatInterval: localHeartbeatInterval,
		PollTimeout:       30 * time.Second,
		ShutdownGrace:     30 * time.Second,
		Version:           "0.1.0",
	}

	rt := worker.NewRuntime(workerCfg, registry, logger, metrics)

	// Run worker until context cancelled.
	errCh := make(chan error, 1)
	go func() {
		errCh <- rt.Run(ctx)
	}()

	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			return fmt.Errorf("worker error: %w", err)
		}
	case <-ctx.Done():
		logger.Info("worker shutting down")
		<-errCh
	}

	logger.Info("worker stopped")
	return nil
}

// runCtl executes the operational CLI client (PRD v0.2 FR-620/621). It is a
// pure client of the existing HTTP API: list/get/cancel/retry use Bearer API
// key auth; outbox-status and workers-status perform read-only database
// queries. The API key is never logged (NFR-205).
func runCtl(ctx context.Context, cfg *config.Config) error {
	args := os.Args[2:]
	if len(args) == 0 {
		return fmt.Errorf("usage: jobforge ctl <list|get|cancel|retry|outbox-status|workers-status|quota-reconcile> [flags] (outbox status also accepted)")
	}
	command := args[0]
	rest := args[1:]
	// PRD v0.2 FR-621 spells the subcommand "outbox status"; accept both the
	// literal two-word form and the hyphenated CLI form "outbox-status".
	if command == "outbox" && len(rest) > 0 && rest[0] == "status" {
		command = "outbox-status"
		rest = rest[1:]
	}

	fs := flag.NewFlagSet("ctl", flag.ContinueOnError)
	apiURL := fs.String("api-url", getEnvDefault("JOBFORGE_API_URL", "http://localhost:8080"),
		"control plane base URL (env JOBFORGE_API_URL)")
	apiKey := fs.String("api-key", os.Getenv("JOBFORGE_API_KEY"),
		"API key for Bearer auth (env JOBFORGE_API_KEY)")
	output := fs.String("output", ctl.OutputTable, "output format: table|json")
	// List filters.
	state := fs.String("state", "", "list filter: job state")
	queue := fs.String("queue", "", "list filter: queue name")
	jobType := fs.String("type", "", "list filter: job type")
	limit := fs.Int("limit", 20, "list page size")
	cursor := fs.String("cursor", "", "list pagination cursor")
	// workers-status staleness threshold.
	staleAfter := fs.Duration("stale-after", 3*cfg.LeaseTTL,
		"workers-status: mark workers stale when their last heartbeat is older than this")
	// quota-reconcile repair switch.
	repair := fs.Bool("repair", false,
		"quota-reconcile: overwrite divergent counters from the jobs aggregation")

	if err := fs.Parse(rest); err != nil {
		return err
	}
	if *output != ctl.OutputTable && *output != ctl.OutputJSON {
		return fmt.Errorf("invalid --output %q (expected table|json)", *output)
	}

	out := os.Stdout

	switch command {
	case "outbox-status":
		// Read-only backlog query (FR-621); needs database credentials,
		// not the HTTP API.
		status, err := ctl.QueryOutboxStatus(ctx, cfg.DatabaseURL)
		if err != nil {
			return err
		}
		return ctl.RenderOutboxStatus(out, *output, status)

	case "workers-status":
		// Read-only worker registry query; needs database credentials,
		// not the HTTP API.
		workers, err := ctl.QueryWorkers(ctx, cfg.DatabaseURL, *staleAfter)
		if err != nil {
			return err
		}
		return ctl.RenderWorkers(out, *output, workers, *staleAfter)

	case "quota-reconcile":
		// Quota counter drift check (and optional repair) straight against
		// PostgreSQL (PRD v0.3 FR-724); needs database credentials, not the
		// HTTP API.
		res, err := ctl.ReconcileQuotaCounters(ctx, cfg.DatabaseURL, *repair)
		if err != nil {
			return err
		}
		return ctl.RenderQuotaReconcile(out, *output, res)
	}

	// All remaining commands talk to the HTTP control plane.
	if *apiKey == "" {
		return fmt.Errorf("missing API key: set --api-key or JOBFORGE_API_KEY")
	}
	client := ctl.NewClient(*apiURL, *apiKey)

	switch command {
	case "list":
		res, err := client.List(ctx, ctl.ListOptions{
			State:  *state,
			Queue:  *queue,
			Type:   *jobType,
			Limit:  *limit,
			Cursor: *cursor,
		})
		if err != nil {
			return err
		}
		return ctl.RenderList(out, *output, res)

	case "get":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: jobforge ctl get <job_id> [flags]")
		}
		job, err := client.Get(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return ctl.RenderJob(out, *output, job)

	case "cancel":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: jobforge ctl cancel <job_id> [flags]")
		}
		if err := client.Cancel(ctx, fs.Arg(0)); err != nil {
			return err
		}
		if *output == ctl.OutputJSON {
			_, _ = fmt.Fprintf(out, "{\n  \"status\": \"cancel_requested\"\n}\n")
		} else {
			_, _ = fmt.Fprintf(out, "cancel requested: job_id=%s\n", fs.Arg(0))
		}
		return nil

	case "retry":
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: jobforge ctl retry <job_id> [flags]")
		}
		res, err := client.Retry(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		return ctl.RenderSubmitResult(out, *output, "retry", res)

	default:
		return fmt.Errorf("unknown ctl command: %s (expected list|get|cancel|retry|outbox-status|workers-status|quota-reconcile)", command)
	}
}

// runMigrate runs database migrations as a standalone subcommand.
// migrator.Up acquires a PostgreSQL advisory lock to prevent concurrent migrations.
func runMigrate(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = 5
	poolCfg.MinConns = 1

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return fmt.Errorf("create connection pool: %w", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	logger.Info("migrate connected to PostgreSQL")

	migrator := migrate.New(pool, logger)
	if err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	logger.Info("migrations completed successfully")
	return nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func getEnvDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseQueues splits a comma-separated queue list, trimming whitespace and
// dropping empty entries.
func parseQueues(raw string) []string {
	var queues []string
	for _, q := range strings.Split(raw, ",") {
		if q = strings.TrimSpace(q); q != "" {
			queues = append(queues, q)
		}
	}
	return queues
}
