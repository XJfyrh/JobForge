package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// JobHandler handles HTTP requests for job operations. It is a thin transport
// layer: validate input, call store, map response. No domain logic here.
type JobHandler struct {
	store  store.JobStore
	logger *slog.Logger
}

// NewJobHandler creates a new job handler.
func NewJobHandler(s store.JobStore, logger *slog.Logger) *JobHandler {
	return &JobHandler{store: s, logger: logger}
}

// CreateJobRequest is the request body for POST /v1/jobs.
type CreateJobRequest struct {
	Queue          string          `json:"queue"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	Priority       int16           `json:"priority"`
	RunAt          *time.Time      `json:"run_at"`
	MaxAttempts    int             `json:"max_attempts"`
	TimeoutSeconds int             `json:"timeout_seconds"`
	IdempotencyKey *string         `json:"idempotency_key"`
}

// CreateJobResponse is the response for POST /v1/jobs.
type CreateJobResponse struct {
	JobID        string `json:"job_id"`
	State        string `json:"state"`
	Deduplicated bool   `json:"deduplicated"`
}

// CreateJob handles POST /v1/jobs.
func (h *JobHandler) CreateJob(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantFromContext(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED", "tenant not authenticated")
		return
	}

	var req CreateJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON body")
		return
	}

	if req.Queue == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "queue is required")
		return
	}
	if req.Type == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "type is required")
		return
	}
	if len(req.Payload) > domain.MaxPayloadSize {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "payload exceeds 256 KiB limit")
		return
	}

	jobID := uuid.New().String()
	now := time.Now()

	job, err := domain.NewJob(jobID, domain.NewJobParams{
		TenantID:       tenantID,
		Queue:          req.Queue,
		Type:           req.Type,
		Payload:        req.Payload,
		Priority:       req.Priority,
		RunAt:          req.RunAt,
		MaxAttempts:    req.MaxAttempts,
		TimeoutSeconds: req.TimeoutSeconds,
		IdempotencyKey: req.IdempotencyKey,
	}, now)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	deduplicated, err := h.store.Enqueue(r.Context(), job)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	h.logger.Info("job created",
		"job_id", job.ID,
		"tenant_id", tenantID,
		"queue", job.Queue,
		"type", job.Type,
		"state", string(job.State),
		"deduplicated", deduplicated,
	)

	writeJSON(w, http.StatusAccepted, CreateJobResponse{
		JobID:        job.ID,
		State:        string(job.State),
		Deduplicated: deduplicated,
	})
}

// JobResponse is the response for GET /v1/jobs/{job_id}.
type JobResponse struct {
	ID             string          `json:"id"`
	TenantID       string          `json:"tenant_id"`
	Queue          string          `json:"queue"`
	Type           string          `json:"type"`
	Payload        json.RawMessage `json:"payload"`
	Priority       int16           `json:"priority"`
	State          string          `json:"state"`
	RunAt          time.Time       `json:"run_at"`
	Attempt        int             `json:"attempt"`
	MaxAttempts    int             `json:"max_attempts"`
	TimeoutSeconds int             `json:"timeout_seconds"`
	IdempotencyKey *string         `json:"idempotency_key"`
	LeaseOwner     *string         `json:"lease_owner"`
	LeaseUntil     *time.Time      `json:"lease_until"`
	FencingToken   int64           `json:"fencing_token"`
	TraceID        *string         `json:"trace_id"`
	RetryOfJobID   *string         `json:"retry_of_job_id"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// GetJob handles GET /v1/jobs/{job_id}.
func (h *JobHandler) GetJob(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantFromContext(r.Context())
	jobID := chi.URLParam(r, "job_id")

	job, err := h.store.GetByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, toJobResponse(job))
}

// ListJobsResponse is the response for GET /v1/jobs.
type ListJobsResponse struct {
	Jobs       []JobResponse `json:"jobs"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// ListJobs handles GET /v1/jobs.
func (h *JobHandler) ListJobs(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantFromContext(r.Context())
	q := r.URL.Query()

	filter := store.ListFilter{
		TenantID: tenantID,
		Queue:    queryPtr(q.Get("queue")),
		Type:     queryPtr(q.Get("type")),
		Cursor:   queryPtr(q.Get("cursor")),
	}

	if s := q.Get("state"); s != "" {
		state := domain.JobState(s)
		filter.State = &state
	}

	if l := q.Get("limit"); l != "" {
		var n int
		if _, err := json.Number(l).Int64(); err == nil {
			n = int(json.Number(l).String()[0]) // simplified
		}
		_ = n
	}

	jobs, cursor, err := h.store.List(r.Context(), filter)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	resp := ListJobsResponse{
		Jobs:       make([]JobResponse, 0, len(jobs)),
		NextCursor: cursor,
	}
	for _, j := range jobs {
		resp.Jobs = append(resp.Jobs, toJobResponse(j))
	}

	writeJSON(w, http.StatusOK, resp)
}

// CancelJob handles POST /v1/jobs/{job_id}:cancel.
func (h *JobHandler) CancelJob(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantFromContext(r.Context())
	jobID := chi.URLParam(r, "job_id")

	err := h.store.Cancel(r.Context(), tenantID, jobID)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	h.logger.Info("job cancel requested",
		"job_id", jobID,
		"tenant_id", tenantID,
	)

	writeJSON(w, http.StatusOK, map[string]string{"status": "cancel_requested"})
}

// HealthLive handles GET /health/live.
func (h *JobHandler) HealthLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

// HealthReady handles GET /health/ready.
func (h *JobHandler) HealthReady(w http.ResponseWriter, _ *http.Request) {
	// Basic readiness: if we can reach here, the HTTP server is up.
	// A full implementation would ping PostgreSQL.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// writeDomainError maps a domain error to the appropriate HTTP response
// per ADR-0002.
func (h *JobHandler) writeDomainError(w http.ResponseWriter, err error) {
	de, ok := errors.AsType[*domain.Error](err)
	if ok {
		status := domainCodeToHTTPStatus(de.Code)
		writeError(w, status, string(de.Code), de.Message)
		return
	}

	h.logger.Error("internal error", "error", err)
	writeError(w, http.StatusInternalServerError, "INTERNAL", "internal server error")
}

// domainCodeToHTTPStatus maps domain error codes to HTTP status codes per
// ADR-0002.
func domainCodeToHTTPStatus(code domain.ErrorCode) int {
	switch code {
	case domain.CodeInvalidArgument:
		return http.StatusBadRequest
	case domain.CodeUnauthorized:
		return http.StatusUnauthorized
	case domain.CodeForbidden:
		return http.StatusForbidden
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeConflict, domain.CodeAlreadyTerminal, domain.CodeStaleLease, domain.CodeCancelRequested:
		return http.StatusConflict
	case domain.CodeQueueOverloaded:
		return http.StatusTooManyRequests
	default:
		return http.StatusInternalServerError
	}
}

// toJobResponse converts a domain.Job to the HTTP response representation.
func toJobResponse(j *domain.Job) JobResponse {
	return JobResponse{
		ID:             j.ID,
		TenantID:       j.TenantID,
		Queue:          j.Queue,
		Type:           j.Type,
		Payload:        j.Payload,
		Priority:       j.Priority,
		State:          string(j.State),
		RunAt:          j.RunAt,
		Attempt:        j.Attempt,
		MaxAttempts:    j.MaxAttempts,
		TimeoutSeconds: j.TimeoutSeconds,
		IdempotencyKey: j.IdempotencyKey,
		LeaseOwner:     j.LeaseOwner,
		LeaseUntil:     j.LeaseUntil,
		FencingToken:   j.FencingToken,
		TraceID:        j.TraceID,
		RetryOfJobID:   j.RetryOfJobID,
		CreatedAt:      j.CreatedAt,
		UpdatedAt:      j.UpdatedAt,
	}
}

func queryPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
