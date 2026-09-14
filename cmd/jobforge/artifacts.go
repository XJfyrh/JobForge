package main

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/tasks"
)

func runArtifacts(ctx context.Context, cfg *config.Config) error {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		return err
	}
	model, err := tasks.NewOllama(cfg.OllamaURL, cfg.OllamaAPIKey)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.ArtifactAddr, Handler: tasks.NewArtifactRouter(tasks.NewPostgresArtifacts(pool), model, cfg),
		ReadTimeout: 10 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	select {
	case err = <-done:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
