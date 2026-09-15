package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/worker"
)

var artifactIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// NewArtifactRouter serves authenticated business artifacts independently of
// the task API. Knowing a reference never grants access. Searches are bounded
// to four concurrent calls; overload does not create another job scheduler.
func NewArtifactRouter(store ArtifactStore, model Model, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	slots := make(chan struct{}, 4)
	getArtifact := func(r *http.Request) (*Artifact, error) {
		id := r.PathValue("id")
		if !artifactIDPattern.MatchString(id) {
			return nil, ErrNotFound
		}
		return store.Get(r.Context(), apihttp.TenantFromContext(r.Context()), "jobforge-artifact:"+id)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeArtifactJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx, span := observability.Tracer("jobforge.tasks").Start(r.Context(), "business.artifact.get")
		defer span.End()
		a, err := getArtifact(r.WithContext(ctx))
		if err != nil {
			writeArtifactError(w, err)
			return
		}
		writeArtifactJSON(w, http.StatusOK, a)
	})
	mux.HandleFunc("POST /v1/artifacts/{id}/search", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		ctx, span := observability.Tracer("jobforge.tasks").Start(ctx, "business.artifact.search")
		defer span.End()
		a, err := getArtifact(r.WithContext(ctx))
		if err != nil {
			writeArtifactError(w, err)
			return
		}
		var query struct {
			Query string `json:"query"`
			K     int    `json:"k"`
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
		if err != nil || strictJSON(data, &query) != nil {
			writeArtifactError(w, permanent("BUSINESS_INPUT_INVALID"))
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeArtifactJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{"code": "OVERLOADED", "message": "search capacity exhausted"}})
			return
		}
		hits, err := SearchArtifact(ctx, model, a, query.Query, query.K)
		if err != nil {
			writeArtifactError(w, err)
			return
		}
		writeArtifactJSON(w, http.StatusOK, map[string]any{"result_ref": a.ResultRef, "hits": hits})
	})
	return apihttp.TraceContextMiddleware(apihttp.AuthMiddleware(cfg)(mux))
}

func writeArtifactJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeArtifactError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	var code string
	switch {
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, "NOT_FOUND"
	case errors.Is(err, context.DeadlineExceeded):
		status, code = http.StatusGatewayTimeout, "TIMEOUT"
	case errors.Is(err, context.Canceled):
		status, code = http.StatusRequestTimeout, "CANCELLED"
	case worker.IsRetryable(err):
		status, code = http.StatusServiceUnavailable, "UNAVAILABLE"
	default:
		var taskErr *Error
		if errors.As(err, &taskErr) {
			code = taskErr.Code
		} else {
			status, code = http.StatusInternalServerError, "INTERNAL"
		}
	}
	writeArtifactJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": code}})
}
