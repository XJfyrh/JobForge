// Command jobforge is the single-binary entry point for the JobForge platform.
// It supports subcommands: api (HTTP control plane), scheduler (job promotion),
// and worker (task execution). In W1, only the api subcommand is implemented.
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

	"github.com/jackc/pgx/v5/pgxpool"

	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/migrate"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: jobforge <api|scheduler|worker>\n")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "api":
		if err := runAPI(logger); err != nil {
			logger.Error("api server failed", "error", err)
			os.Exit(1)
		}
	case "scheduler":
		fmt.Fprintf(os.Stderr, "scheduler not yet implemented (W4)\n")
		os.Exit(1)
	case "worker":
		fmt.Fprintf(os.Stderr, "worker not yet implemented (W2)\n")
		os.Exit(1)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\nusage: jobforge <api|scheduler|worker>\n", os.Args[1])
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

	// Run database migrations automatically.
	migrator := migrate.New(pool, logger)
	if err := migrator.Up(ctx); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Create store and router.
	jobStore := postgres.NewJobStore(pool)
	router := apihttp.NewRouter(jobStore, cfg, logger)

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
