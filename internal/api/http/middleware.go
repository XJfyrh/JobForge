// Package http provides the HTTP transport layer for the JobForge control
// plane API. Handlers are thin: they validate input, call the store/domain
// layer, and map responses. No state transition logic lives here.
package http

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/xjfyrh/jobforge/internal/config"
)

type contextKey string

const (
	// TenantIDKey is the context key for the authenticated tenant ID.
	TenantIDKey contextKey = "tenant_id"

	// RequestIDKey is the context key for the request correlation ID.
	RequestIDKey contextKey = "request_id"
)

// TenantFromContext extracts the authenticated tenant ID from the request
// context. Returns empty string if not set.
func TenantFromContext(ctx context.Context) string {
	v, _ := ctx.Value(TenantIDKey).(string)
	return v
}

// RequestIDFromContext extracts the request ID from context.
func RequestIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(RequestIDKey).(string)
	return v
}

// AuthMiddleware validates the Bearer API key and injects the mapped tenant_id
// into the request context. Per ADR-0001, P0 uses static config mapping.
// The tenant_id is NEVER taken from the request body.
func AuthMiddleware(cfg *config.Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for health endpoints.
			if strings.HasPrefix(r.URL.Path, "/health") {
				next.ServeHTTP(w, r)
				return
			}

			auth := r.Header.Get("Authorization")
			if auth == "" {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing Authorization header")
				return
			}

			parts := strings.SplitN(auth, " ", 2)
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid Authorization format")
				return
			}

			apiKey := parts[1]
			tenantID, ok := cfg.TenantForKey(apiKey)
			if !ok {
				writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid API key")
				return
			}

			ctx := context.WithValue(r.Context(), TenantIDKey, tenantID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequestIDMiddleware generates a unique request ID and injects it into
// context and response headers for correlation.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := generateRequestID()
		ctx := context.WithValue(r.Context(), RequestIDKey, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// LoggingMiddleware logs each request with structured fields.
func LoggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(sw, r)

			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFromContext(r.Context()),
				"tenant_id", TenantFromContext(r.Context()),
			)
		})
	}
}

// statusWriter captures the HTTP response status code for logging.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// generateRequestID produces a random hex request identifier.
func generateRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
