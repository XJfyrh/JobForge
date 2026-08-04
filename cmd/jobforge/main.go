// Command jobforge is the single-binary entry point for the JobForge platform.
// It supports subcommands: migrate (database migration), api (HTTP control plane),
// scheduler (job promotion), worker (task execution), gateway (gRPC worker gateway),
// and publisher (outbox event publisher).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/domain"
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
		fmt.Fprintf(os.Stderr, "usage: jobforge <migrate|api|scheduler|gateway|worker|publisher>\n")
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
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nusage: jobforge <migrate|api|scheduler|gateway|worker|publisher>\n", os.Args[1])
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
	service := gatewaygrpc.NewWorkerService(jobStore, listener, cfg.LeaseTTL, cfg.TenantMaxInflight, logger, metrics)
	server := gatewaygrpc.NewServer(service, logger)

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
	logger.Info("publisher connected to PostgreSQL", "channel", cfg.OutboxChannel)

	// Run migrations.
	migrator := migrate.New(pool, logger)
	if err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	outboxStore := postgres.NewOutboxStore(pool)
	channel := outbox.NewNotifyChannel(pool, cfg.OutboxChannel, logger)

	pubCfg := outbox.Config{
		PollInterval:    cfg.OutboxPollInterval,
		MaxIdleInterval: 30 * time.Second,
		BatchSize:       cfg.OutboxBatchSize,
		Retention:       cfg.OutboxRetention,
		CleanupInterval: time.Hour,
	}

	pub := outbox.New(outboxStore, channel, pubCfg, logger, metrics)

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

func runWorker(ctx context.Context, logger *slog.Logger, cfg *config.Config, metrics *observability.Metrics) error {
	// Register demo handlers.
	registry := worker.NewRegistry()
	demo.RegisterAll(registry)
	// PageWise reindex demo handler (FR-403 / Appendix A).
	demo.RegisterPagewise(registry)

	// Determine gateway address.
	gatewayAddr := getEnvDefault("JOBFORGE_GATEWAY_ADDR", "localhost:9090")

	workerCfg := worker.RuntimeConfig{
		WorkerID:          domain.NewID(),
		InstanceID:        fmt.Sprintf("%s-%d", hostname(), os.Getpid()),
		Queues:            []string{getEnvDefault("JOBFORGE_WORKER_QUEUE", "default")},
		Capacity:          5,
		GatewayAddr:       gatewayAddr,
		HeartbeatInterval: cfg.HeartbeatInterval,
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
