// Command jobforge is the single-binary entry point for the JobForge platform.
// It supports subcommands: migrate (database migration), api (HTTP control plane),
// scheduler (job promotion), worker (task execution), and gateway (gRPC worker gateway).
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
		fmt.Fprintf(os.Stderr, "usage: jobforge <migrate|api|scheduler|gateway|worker>\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "migrate":
		if err := runMigrate(logger); err != nil {
			logger.Error("migrate failed", "error", err)
			os.Exit(1)
		}
	case "api":
		if err := runAPI(logger); err != nil {
			logger.Error("api server failed", "error", err)
			os.Exit(1)
		}
	case "scheduler":
		if err := runScheduler(logger); err != nil {
			logger.Error("scheduler failed", "error", err)
			os.Exit(1)
		}
	case "worker":
		if err := runWorker(logger); err != nil {
			logger.Error("worker failed", "error", err)
			os.Exit(1)
		}
	case "gateway":
		if err := runGateway(logger); err != nil {
			logger.Error("gateway failed", "error", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nusage: jobforge <migrate|api|scheduler|gateway|worker>\n", os.Args[1])
		os.Exit(1)
	}
}

func runAPI(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Create PostgreSQL connection pool.
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = 20
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	router := apihttp.NewRouter(jobStore, jobStore, cfg, logger)

	// Create HTTP server.
	srv := &http.Server{
		Addr:         cfg.HTTPAddr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown on SIGINT/SIGTERM.
	errCh := make(chan error, 1)
	go func() {
		logger.Info("starting HTTP server", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("server error: %w", err)
	case sig := <-sigCh:
		logger.Info("shutting down", "signal", sig.String())
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

func runScheduler(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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

	sched := scheduler.New(schedStore, notifier, listener, schedCfg, logger)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- sched.Run(ctx)
	}()

	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			return fmt.Errorf("scheduler error: %w", err)
		}
	case sig := <-sigCh:
		logger.Info("scheduler shutting down", "signal", sig.String())
		cancel()
		<-errCh
	}

	logger.Info("scheduler stopped")
	return nil
}

func runGateway(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
	service := gatewaygrpc.NewWorkerService(jobStore, listener, cfg.LeaseTTL, logger)
	server := gatewaygrpc.NewServer(service, logger)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(cfg.GRPCAddr)
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("gateway server error: %w", err)
	case sig := <-sigCh:
		logger.Info("gateway shutting down", "signal", sig.String())
		server.GracefulStop()
	}

	logger.Info("gateway stopped")
	return nil
}

func runWorker(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register demo handlers.
	registry := worker.NewRegistry()
	demo.RegisterAll(registry)

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

	rt := worker.NewRuntime(workerCfg, registry, logger)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		errCh <- rt.Run(ctx)
	}()

	select {
	case err := <-errCh:
		if err != nil && err != context.Canceled {
			return fmt.Errorf("worker error: %w", err)
		}
	case sig := <-sigCh:
		logger.Info("worker shutting down", "signal", sig.String())
		cancel()
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
