// Package httpapi implements the thin public Run transport defined by
// api/run/v2/openapi.yaml. Execution and idempotency decisions belong to API.
package httpapi

import (
	"context"
	"net/http"
	"net/url"
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
	Calls(context.Context, string, string) (run.CallsResponse, error)
	Cancel(context.Context, string, string, string) (run.CancelResponse, error)
	Retry(context.Context, string, string, string, run.RetryRequest) (run.SubmitResponse, error)
}

// Identity is fixed by trusted deployment configuration, never request JSON.
type Identity struct {
	TenantID string
	Role     string
	ActorID  string
}

type identityKey struct{}

type handler struct {
	api  API
	keys map[string]Identity
}

// NewRouter validates and copies credential configuration before serving.
// A reader may query; an operator submits/cancels/retries/reconciles, and a
// separately configured approver decides the original proposal. Worker and
// budget-administration operations remain on their trusted boundaries.
func NewRouter(api API, keys map[string]Identity) (http.Handler, error) {
	if api == nil || len(keys) == 0 {
		return nil, run.ErrInvalidArgument
	}
	h := &handler{api: api, keys: make(map[string]Identity, len(keys))}
	for key, identity := range keys {
		if !validCredential(key) || !run.ValidIdentifier(identity.TenantID) ||
			(identity.Role != "reader" && identity.Role != "operator" && identity.Role != "approver") ||
			(identity.ActorID != "" && !run.ValidIdentifier(identity.ActorID)) || (identity.Role == "approver" && !run.ValidIdentifier(identity.ActorID)) {
			return nil, run.ErrInvalidArgument
		}
		h.keys[key] = identity
	}
	router := chi.NewRouter()
	router.Get("/ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
	})
	router.Get("/ui/*", serveUI)
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) { writeError(w, run.ErrNotFound) })
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) { writeError(w, run.ErrInvalidArgument) })
	router.Group(func(router chi.Router) {
		router.Use(h.boundary)
		router.Get("/v2/identity", func(w http.ResponseWriter, r *http.Request) {
			if _, err := query(r); err != nil {
				writeError(w, err)
				return
			}
			i := authenticated(r)
			writeJSON(w, http.StatusOK, struct {
				TenantID string `json:"tenant_id"`
				Role     string `json:"role"`
				ActorID  string `json:"actor_id"`
			}{i.TenantID, i.Role, i.ActorID})
		})
		router.Post("/v2/runs", h.submit)
		router.Get("/v2/runs", h.list)
		router.Get("/v2/runs/{run_id}", h.get)
		router.Get("/v2/runs/{run_id}/steps", h.steps)
		router.Get("/v2/runs/{run_id}/events", h.events)
		router.Get("/v2/runs/{run_id}/result", h.result)
		router.Get("/v2/runs/{run_id}/calls", h.calls)
		router.Post("/v2/runs/{run_id}/cancel", h.cancel)
		router.Post("/v2/runs/{run_id}/retry", h.retry)
		router.Get("/v2/runs/{run_id}/approval", h.approval)
		router.Post("/v2/runs/{run_id}/approval", h.decideApproval)
		router.Get("/v2/runs/{run_id}/effect", h.effect)
		router.Post("/v2/runs/{run_id}/reconcile", h.reconcile)
		router.Get("/v2/runs/{run_id}/action-calls", h.actionCalls)
	})
	return router, nil
}

func (h *handler) boundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := &requestObservation{ResponseWriter: w, result: "ok"}
		w = observed
		defer func() {
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			observability.RunHTTPRequests.WithLabelValues(route, observed.result).Inc()
		}()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Browser mutations must be same-origin. Bearer-only SDK calls have no
		// Origin; forwarding headers do not grant a different approval origin.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			writeError(w, run.ErrForbidden)
			return
		}
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
		carrier := propagation.MapCarrier{}
		propagation.TraceContext{}.Inject(ctx, carrier)
		ctx = run.WithTraceContext(ctx, carrier["traceparent"])
		next.ServeHTTP(w, r.WithContext(ctx))
		// Registered patterns contain no raw identifiers, query strings or bodies.
		span.SetAttributes(attribute.String("http.route", chi.RouteContext(ctx).RoutePattern()))
		if id := chi.URLParam(r, "run_id"); run.ValidUUID(id) {
			span.SetAttributes(attribute.String("jobforge.run_id", id))
		}
	})
}

type requestObservation struct {
	http.ResponseWriter
	result string
}

func sameOrigin(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	origin, err := url.Parse(values[0])
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return err == nil && origin.Scheme == scheme && strings.EqualFold(origin.Host, r.Host) &&
		origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
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
