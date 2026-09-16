package business

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/observability"
)

// Identity comes only from deployment configuration, never request data.
type Identity struct {
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
}

// HTTPStore is the business service boundary; implementations enforce tenant
// and immutable snapshot semantics independently of HTTP routing.
type HTTPStore interface {
	CheckReady(context.Context) error
	CheckTenantReady(context.Context, string) error
	CreateSnapshot(context.Context, string, SnapshotRequest) (*Snapshot, bool, error)
	GetSnapshot(context.Context, string, string) (*Snapshot, error)
	GetOrder(context.Context, string, string) (*OrderEvidence, error)
	GetDelivery(context.Context, string, string) (*DeliveryEvidence, error)
	GetEvidence(context.Context, string, string, string) (*Evidence, error)
	SearchPolicies(context.Context, string, string, SearchRequest) ([]PolicyHit, error)
	GetPolicy(context.Context, string, string, string) (*PolicyHit, error)
}

type identityKey struct{}

type businessHTTP struct {
	store HTTPStore
	keys  map[string]Identity
	slots chan struct{}
}

// NewHTTPHandler constructs the bounded, read-oriented business HTTP service.
// An operator may create snapshots; both roles can read within their tenant.
func NewHTTPHandler(store HTTPStore, keys map[string]Identity) (http.Handler, error) {
	if store == nil || len(keys) == 0 {
		return nil, ErrInvalidArgument
	}
	h := &businessHTTP{store: store, keys: make(map[string]Identity, len(keys)), slots: make(chan struct{}, 8)}
	for key, identity := range keys {
		if len(key) < 16 || len(key) > 256 || strings.ContainsAny(key, " \t\r\n") ||
			!validID(identity.TenantID) || (identity.Role != "operator" && identity.Role != "reader") {
			return nil, ErrInvalidArgument
		}
		h.keys[key] = identity
	}
	router := chi.NewRouter()
	router.Use(h.boundary)
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) { writeBusinessError(w, ErrNotFound) })
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeBusinessCode(w, http.StatusMethodNotAllowed, "INVALID_ARGUMENT")
	})
	router.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := h.store.CheckReady(r.Context()); err != nil {
			writeBusinessError(w, ErrDependencyUnavailable)
			return
		}
		if err := h.tenantsReady(r.Context()); err != nil {
			writeBusinessError(w, ErrDependencyUnavailable)
			return
		}
		writeBusinessJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	router.Post("/business/v1/snapshots", h.create)
	router.Get("/business/v1/snapshots/{id}", h.snapshot)
	router.Get("/business/v1/snapshots/{id}/order", h.order)
	router.Get("/business/v1/snapshots/{id}/delivery", h.delivery)
	router.Get("/business/v1/snapshots/{id}/evidence/{kind}", h.evidence)
	router.Post("/business/v1/snapshots/{id}/policies/search", h.search)
	router.Get("/business/v1/snapshots/{id}/policies/{chunk}", h.policy)
	return router, nil
}

func (h *businessHTTP) tenantsReady(ctx context.Context) error {
	checked := make(map[string]bool)
	for _, identity := range h.keys {
		if checked[identity.TenantID] {
			continue
		}
		if err := h.store.CheckTenantReady(ctx, identity.TenantID); err != nil {
			return err
		}
		checked[identity.TenantID] = true
	}
	return nil
}

func (h *businessHTTP) boundary(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.RawQuery != "" {
			writeBusinessError(w, ErrInvalidArgument)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if r.URL.Path != "/health/ready" || r.Method != http.MethodGet {
			parts := strings.Split(r.Header.Get("Authorization"), " ")
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				writeBusinessCode(w, http.StatusUnauthorized, "UNAUTHORIZED")
				return
			}
			identity, ok := h.keys[parts[1]]
			if !ok {
				writeBusinessCode(w, http.StatusUnauthorized, "UNAUTHORIZED")
				return
			}
			ctx = context.WithValue(ctx, identityKey{}, identity)
		}
		select {
		case h.slots <- struct{}{}:
			defer func() { <-h.slots }()
		default:
			writeBusinessCode(w, http.StatusTooManyRequests, "RATE_LIMITED")
			return
		}
		ctx = observability.ContextWithTraceParent(ctx, r.Header.Get(observability.TraceParentKey))
		ctx, span := otel.Tracer("jobforge/business").Start(ctx, "business.http")
		defer span.End()
		next.ServeHTTP(w, r.WithContext(ctx))
		// Only the registered route is recorded, never URL parameters or bodies.
		span.SetAttributes(attribute.String("http.route", chi.RouteContext(ctx).RoutePattern()))
	})
}

func requestIdentity(r *http.Request) Identity {
	identity, _ := r.Context().Value(identityKey{}).(Identity)
	return identity
}

func decodeBusinessJSON(w http.ResponseWriter, r *http.Request, limit int64, dst any) error {
	if r.Header.Get("Content-Type") != "application/json" {
		return ErrInvalidArgument
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return ErrInvalidArgument
	}
	if err := jsonstrict.Decode(data, dst); err != nil {
		return ErrInvalidArgument
	}
	return nil
}

func (h *businessHTTP) create(w http.ResponseWriter, r *http.Request) {
	identity := requestIdentity(r)
	if identity.Role != "operator" {
		writeBusinessCode(w, http.StatusForbidden, "FORBIDDEN")
		return
	}
	var req SnapshotRequest
	if err := decodeBusinessJSON(w, r, 4096, &req); err != nil {
		writeBusinessError(w, err)
		return
	}
	snapshot, reused, err := h.store.CreateSnapshot(r.Context(), identity.TenantID, req)
	if err != nil {
		writeBusinessError(w, err)
		return
	}
	status := http.StatusOK
	if !reused {
		status = http.StatusCreated
	}
	writeBusinessJSON(w, status, snapshotMetadata(snapshot))
}

func snapshotMetadata(snapshot *Snapshot) any {
	return struct {
		ID          string         `json:"snapshot_id"`
		TenantID    string         `json:"tenant_id"`
		Version     int            `json:"schema_version"`
		AsOf        time.Time      `json:"as_of"`
		CreatedAt   time.Time      `json:"created_at"`
		ContentHash string         `json:"content_hash"`
		Ticket      Ticket         `json:"ticket"`
		Policy      PolicyVersion  `json:"policy"`
		Index       PublishedIndex `json:"index"`
	}{snapshot.ID, snapshot.TenantID, snapshot.SchemaVersion, snapshot.AsOf, snapshot.CreatedAt, snapshot.ContentHash,
		snapshot.Ticket, snapshot.Policy, snapshot.Index}
}

func (h *businessHTTP) snapshot(w http.ResponseWriter, r *http.Request) {
	value, err := h.store.GetSnapshot(r.Context(), requestIdentity(r).TenantID, chi.URLParam(r, "id"))
	if err != nil {
		writeBusinessError(w, err)
		return
	}
	writeBusinessJSON(w, http.StatusOK, snapshotMetadata(value))
}

func (h *businessHTTP) order(w http.ResponseWriter, r *http.Request) {
	value, err := h.store.GetOrder(r.Context(), requestIdentity(r).TenantID, chi.URLParam(r, "id"))
	writeBusinessResult(w, value, err)
}

func (h *businessHTTP) delivery(w http.ResponseWriter, r *http.Request) {
	value, err := h.store.GetDelivery(r.Context(), requestIdentity(r).TenantID, chi.URLParam(r, "id"))
	writeBusinessResult(w, value, err)
}

func (h *businessHTTP) evidence(w http.ResponseWriter, r *http.Request) {
	value, err := h.store.GetEvidence(r.Context(), requestIdentity(r).TenantID, chi.URLParam(r, "id"), chi.URLParam(r, "kind"))
	writeBusinessResult(w, value, err)
}

func (h *businessHTTP) search(w http.ResponseWriter, r *http.Request) {
	var req SearchRequest
	if err := decodeBusinessJSON(w, r, 32768, &req); err != nil {
		writeBusinessError(w, err)
		return
	}
	id := chi.URLParam(r, "id")
	value, err := h.store.SearchPolicies(r.Context(), requestIdentity(r).TenantID, id, req)
	writeBusinessResult(w, struct {
		SnapshotID string      `json:"snapshot_id"`
		Matches    []PolicyHit `json:"matches"`
	}{id, value}, err)
}

func (h *businessHTTP) policy(w http.ResponseWriter, r *http.Request) {
	value, err := h.store.GetPolicy(r.Context(), requestIdentity(r).TenantID, chi.URLParam(r, "id"), chi.URLParam(r, "chunk"))
	writeBusinessResult(w, value, err)
}

func writeBusinessResult(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeBusinessError(w, err)
		return
	}
	writeBusinessJSON(w, http.StatusOK, value)
}

func writeBusinessJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil || len(data) > MaxToolBytes {
		writeBusinessCode(w, http.StatusInternalServerError, "INTERNAL")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func writeBusinessError(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, "INTERNAL"
	switch {
	case errors.Is(err, ErrInvalidArgument):
		status, code = http.StatusBadRequest, "INVALID_ARGUMENT"
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, "NOT_FOUND"
	case errors.Is(err, ErrConflict):
		status, code = http.StatusConflict, "CONFLICT"
	case errors.Is(err, ErrProfileUnavailable):
		status, code = http.StatusConflict, "PROFILE_UNAVAILABLE"
	case errors.Is(err, ErrDependencyUnavailable), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		status, code = http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE"
	}
	writeBusinessCode(w, status, code)
}

func writeBusinessCode(w http.ResponseWriter, status int, code string) {
	writeBusinessJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}
