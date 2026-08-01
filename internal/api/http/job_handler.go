package http

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// Pinger checks connectivity to a backing store (e.g. PostgreSQL).
// It is a minimal interface used exclusively by health-readiness probes.
type Pinger interface {
	Ping(ctx context.Context) error
}

// JobHandler handles HTTP requests for job operations. It is a thin transport
// layer: validate input, call store, map response. No domain logic here.
type JobHandler struct {
	store  store.JobStore
	pinger Pinger
	logger *slog.Logger

	// Queue backpressure limits (FR-303).
	queueSoftLimit int
	queueHardLimit int
}

// NewJobHandler creates a new job handler.
func NewJobHandler(s store.JobStore, p Pinger, logger *slog.Logger) *JobHandler {
	return &JobHandler{
		store:          s,
		pinger:         p,
		logger:         logger,
		queueSoftLimit: 10000, // default, can be overridden
		queueHardLimit: 50000, // default, can be overridden
	}
}

// SetQueueLimits configures the queue backpressure thresholds.
func (h *JobHandler) SetQueueLimits(soft, hard int) {
	h.queueSoftLimit = soft
	h.queueHardLimit = hard
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

	// Trace ID propagation (AT-12): use X-Trace-ID header or generate new.
	traceID := r.Header.Get("X-Trace-ID")
	if traceID == "" {
		traceID = uuid.New().String()
	}

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
		TraceID:        &traceID,
	}, now)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	// Queue backpressure check (FR-303).
	if h.queueHardLimit > 0 {
		depth, err := h.store.GetQueueDepth(r.Context(), req.Queue)
		if err != nil {
			h.logger.Error("failed to get queue depth", "queue", req.Queue, "error", err)
			// Continue anyway; backpressure is best-effort.
		} else {
			if depth >= h.queueHardLimit {
				h.logger.Warn("queue hard limit exceeded, rejecting submission",
					"queue", req.Queue, "depth", depth, "hard_limit", h.queueHardLimit)
				writeError(w, http.StatusTooManyRequests, "QUEUE_OVERLOADED",
					"queue is at capacity, please retry later")
				return
			}
			if depth >= h.queueSoftLimit {
				h.logger.Warn("queue soft limit exceeded",
					"queue", req.Queue, "depth", depth, "soft_limit", h.queueSoftLimit)
			}
		}
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
		"trace_id", traceID,
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
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			filter.Limit = n
		}
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

// RetryJob handles POST /v1/jobs/{job_id}:retry.
// Creates a new job cloned from a dead/cancelled job (ADR-0001).
func (h *JobHandler) RetryJob(w http.ResponseWriter, r *http.Request) {
	tenantID := TenantFromContext(r.Context())
	jobID := chi.URLParam(r, "job_id")

	// Fetch the original job.
	origJob, err := h.store.GetByID(r.Context(), tenantID, jobID)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	// Only dead or cancelled jobs can be retried.
	if origJob.State == domain.StateSucceeded {
		h.writeDomainError(w, domain.NewError(domain.CodeAlreadyTerminal, domain.ErrAlreadyTerminal,
			"succeeded jobs cannot be retried"))
		return
	}
	if origJob.State != domain.StateDead && origJob.State != domain.StateCancelled {
		h.writeDomainError(w, domain.NewError(domain.CodeInvalidTransition, domain.ErrInvalidTransition,
			"only dead or cancelled jobs can be retried, current state: %s", origJob.State))
		return
	}

	// Clone as new job.
	now := time.Now()
	newJob, err := domain.NewJob(uuid.New().String(), domain.NewJobParams{
		TenantID:       tenantID,
		Queue:          origJob.Queue,
		Type:           origJob.Type,
		Payload:        origJob.Payload,
		Priority:       origJob.Priority,
		MaxAttempts:    origJob.MaxAttempts,
		TimeoutSeconds: origJob.TimeoutSeconds,
		RetryOfJobID:   &origJob.ID,
	}, now)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	_, err = h.store.Enqueue(r.Context(), newJob)
	if err != nil {
		h.writeDomainError(w, err)
		return
	}

	h.logger.Info("job retry created",
		"new_job_id", newJob.ID,
		"original_job_id", jobID,
		"tenant_id", tenantID,
	)

	writeJSON(w, http.StatusAccepted, CreateJobResponse{
		JobID:        newJob.ID,
		State:        string(newJob.State),
		Deduplicated: false,
	})
}

// HealthLive handles GET /health/live.
func (h *JobHandler) HealthLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "alive"})
}

// HealthReady handles GET /health/ready.
// It verifies PostgreSQL connectivity before reporting readiness.
func (h *JobHandler) HealthReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := h.pinger.Ping(ctx); err != nil {
		h.logger.Warn("health ready check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}

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
