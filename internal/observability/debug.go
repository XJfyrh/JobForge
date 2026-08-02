package observability

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"time"

	promhttp "github.com/prometheus/client_golang/prometheus/promhttp"
)

// StartDebugServer starts an HTTP server exposing pprof and Prometheus
// /metrics endpoints. Per PRD 11.4, it binds to localhost only by default.
// The server runs until ctx is cancelled.
func StartDebugServer(ctx context.Context, addr string, logger *slog.Logger) error {
	mux := http.NewServeMux()

	// Prometheus metrics endpoint.
	mux.Handle("/metrics", promhttp.Handler())

	// pprof endpoints (net/http/pprof registers on DefaultServeMux,
	// so we register explicitly on our private mux).
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // pprof profile can take time
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("debug server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("debug server: %w", err)
		}
		return nil
	}
}
