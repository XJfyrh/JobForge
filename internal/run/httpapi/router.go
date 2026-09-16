// Package httpapi implements the thin public Run transport defined by
// api/run/v2/openapi.yaml. Execution and idempotency decisions belong to API.
package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/run"
)

// API is the consuming transport's tenant-scoped service boundary. Its
// implementation owns atomic admission, operations, state and cursor identity.
type API interface {
	Submit(context.Context, string, string, run.SubmitRequest) (run.SubmitResponse, error)
	Get(context.Context, string, string) (run.Run, error)
	List(context.Context, string, run.ListFilter) (run.Page, error)
	Steps(context.Context, string, string, int64, int) (run.StepPage, error)
	Events(context.Context, string, string, int64, int) (run.EventPage, error)
	Result(context.Context, string, string) (run.Result, error)
	Cancel(context.Context, string, string, string) (run.CancelResponse, error)
	Retry(context.Context, string, string, string, run.RetryRequest) (run.SubmitResponse, error)
}

// Identity is fixed by trusted deployment configuration, never request JSON.
type Identity struct {
	TenantID string
	Role     string
}

type identityKey struct{}

type handler struct {
	api  API
	keys map[string]Identity
}

// NewRouter validates and copies credential configuration before serving.
// A reader may query; only an operator may submit, cancel or retry. There are no
// approval, Worker, budget-administration or arbitrary-code transport routes.
func NewRouter(api API, keys map[string]Identity) (http.Handler, error) {
	if api == nil || len(keys) == 0 {
		return nil, run.ErrInvalidArgument
	}
	h := &handler{api: api, keys: make(map[string]Identity, len(keys))}
	for key, identity := range keys {
		if !validCredential(key) || !run.ValidIdentifier(identity.TenantID) ||
			(identity.Role != "reader" && identity.Role != "operator") {
			return nil, run.ErrInvalidArgument
		}
		h.keys[key] = identity
	}
	router := chi.NewRouter()
	router.Use(h.boundary)
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) { writeError(w, run.ErrNotFound) })
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) { writeError(w, run.ErrInvalidArgument) })
	router.Post("/v2/runs", h.submit)
	router.Get("/v2/runs", h.list)
	router.Get("/v2/runs/{run_id}", h.get)
	router.Get("/v2/runs/{run_id}/steps", h.steps)
	router.Get("/v2/runs/{run_id}/events", h.events)
	router.Get("/v2/runs/{run_id}/result", h.result)
	router.Post("/v2/runs/{run_id}/cancel", h.cancel)
	router.Post("/v2/runs/{run_id}/retry", h.retry)
	return router, nil
}

func (h *handler) boundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			writeError(w, run.ErrUnauthorized)
			return
		}
		parts := strings.Split(headers[0], " ")
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeError(w, run.ErrUnauthorized)
			return
		}
		identity, ok := h.keys[parts[1]]
		if !ok {
			writeError(w, run.ErrUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), identityKey{}, identity)
		ctx = propagation.TraceContext{}.Extract(ctx, propagation.HeaderCarrier(r.Header))
		ctx, span := observability.Tracer("jobforge/run/httpapi").Start(ctx, "run.http")
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
		// Registered patterns contain no raw identifiers, query strings or bodies.
		span.SetAttributes(attribute.String("http.route", chi.RouteContext(ctx).RoutePattern()))
	})
}

func authenticated(r *http.Request) Identity {
	identity, _ := r.Context().Value(identityKey{}).(Identity)
	return identity
}

func validCredential(key string) bool {
	if len(key) == 0 || len(key) > 256 {
		return false
	}
	for _, c := range key {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}
