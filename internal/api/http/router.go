package http

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store"
)

// NewRouter creates the HTTP router with all middleware and routes configured.
func NewRouter(jobStore store.JobStore, pinger Pinger, cfg *config.Config, logger *slog.Logger, metrics *observability.Metrics) http.Handler {
	handler := NewJobHandler(jobStore, pinger, logger, metrics)

	r := chi.NewRouter()

	// Global middleware stack.
	r.Use(chimiddleware.Recoverer)
	r.Use(RequestIDMiddleware)
	r.Use(LoggingMiddleware(logger))
	r.Use(AuthMiddleware(cfg))

	// Health endpoints (no auth required, handled by middleware skip).
	r.Get("/health/live", handler.HealthLive)
	r.Get("/health/ready", handler.HealthReady)

	// Job API v1.
	r.Route("/v1/jobs", func(r chi.Router) {
		r.Post("/", handler.CreateJob)
		r.Get("/", handler.ListJobs)
		r.Get("/{job_id}", handler.GetJob)
		r.Post("/{job_id}:cancel", handler.CancelJob)
		r.Post("/{job_id}:retry", handler.RetryJob)
	})

	return r
}

// errorResponse is the standard error envelope per ADR-0002.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeError writes a structured error response.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{
		Error: errorBody{
			Code:    code,
			Message: message,
		},
	})
}

// writeJSON serializes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
