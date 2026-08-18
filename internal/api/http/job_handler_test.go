package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// fakePinger is a test double for the Pinger interface.
type fakePinger struct {
	err error
}

func (f *fakePinger) Ping(_ context.Context) error {
	return f.err
}

func testTaskTypeCatalog(t *testing.T) *domain.TaskTypeCatalog {
	t.Helper()
	catalog, err := domain.NewTaskTypeCatalog(domain.DefaultTaskTypeNames())
	if err != nil {
		t.Fatalf("create task type catalog: %v", err)
	}
	return catalog
}

func TestHealthReady_Success(t *testing.T) {
	pinger := &fakePinger{err: nil}
	handler := NewJobHandler(nil, pinger, testTaskTypeCatalog(t), slog.Default(), nil)

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()

	handler.HealthReady(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ready" {
		t.Fatalf("expected status field \"ready\", got %q", body["status"])
	}
}

func TestHealthReady_PingFails_Returns503(t *testing.T) {
	pinger := &fakePinger{err: errors.New("connection refused")}
	handler := NewJobHandler(nil, pinger, testTaskTypeCatalog(t), slog.Default(), nil)

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()

	handler.HealthReady(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status 503, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "unavailable" {
		t.Fatalf("expected status field \"unavailable\", got %q", body["status"])
	}
}

func TestHealthReady_ResponseStructureCompatible(t *testing.T) {
	pinger := &fakePinger{err: nil}
	handler := NewJobHandler(nil, pinger, testTaskTypeCatalog(t), slog.Default(), nil)

	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()

	handler.HealthReady(rec, req)

	// Verify Content-Type header is preserved.
	ct := rec.Header().Get("Content-Type")
	if ct != "application/json; charset=utf-8" {
		t.Fatalf("expected Content-Type application/json; charset=utf-8, got %q", ct)
	}

	// Verify the response is a flat JSON object with only "status" key.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("expected exactly 1 field in response, got %d", len(raw))
	}
	if _, ok := raw["status"]; !ok {
		t.Fatal("response missing \"status\" field")
	}
}

func TestHealthLive_Unchanged(t *testing.T) {
	handler := NewJobHandler(nil, &fakePinger{}, testTaskTypeCatalog(t), slog.Default(), nil)

	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()

	handler.HealthLive(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "alive" {
		t.Fatalf("expected status field \"alive\", got %q", body["status"])
	}
}

// backpressureStore is a partial store.JobStore fake for queue backpressure
// tests (FR-303). Only GetQueueDepth and Enqueue are exercised.
type backpressureStore struct {
	store.JobStore // embedded nil; any other call indicates a test bug
	depth          int
	enqueued       bool
}

// GetQueueDepth implements the backpressure probe.
func (f *backpressureStore) GetQueueDepth(_ context.Context, _ string) (int, error) {
	return f.depth, nil
}

// Enqueue records that the job passed backpressure.
func (f *backpressureStore) Enqueue(_ context.Context, _ *domain.Job) (bool, error) {
	f.enqueued = true
	return false, nil
}

func newSubmitRequest(t *testing.T) *http.Request {
	t.Helper()
	body := `{"queue":"bp-queue","type":"demo.echo","payload":{"k":"v"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), TenantIDKey, "bp-tenant")
	return req.WithContext(ctx)
}

// TestCreateJob_QueueHardLimit_Returns429 verifies FR-303: submissions are
// rejected with 429 QUEUE_OVERLOADED once the queue depth reaches the hard
// threshold configured via SetQueueLimits (JOBFORGE_QUEUE_HARD_LIMIT).
func TestCreateJob_QueueHardLimit_Returns429(t *testing.T) {
	fs := &backpressureStore{depth: 100}
	handler := NewJobHandler(fs, &fakePinger{}, testTaskTypeCatalog(t), slog.Default(), nil)
	handler.SetQueueLimits(5, 100)

	rec := httptest.NewRecorder()
	handler.CreateJob(rec, newSubmitRequest(t))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if env.Error.Code != "QUEUE_OVERLOADED" {
		t.Fatalf("expected error code QUEUE_OVERLOADED, got %q", env.Error.Code)
	}
	if fs.enqueued {
		t.Error("job must not be enqueued when the hard limit is reached")
	}
}

// TestCreateJob_BelowLimits_Accepted verifies that submissions below the
// configured thresholds are accepted normally.
func TestCreateJob_BelowLimits_Accepted(t *testing.T) {
	fs := &backpressureStore{depth: 9}
	handler := NewJobHandler(fs, &fakePinger{}, testTaskTypeCatalog(t), slog.Default(), nil)
	handler.SetQueueLimits(5, 100)

	rec := httptest.NewRecorder()
	handler.CreateJob(rec, newSubmitRequest(t))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if !fs.enqueued {
		t.Error("expected the job to be enqueued below the hard limit")
	}
}

func TestCreateJob_UnknownTypeRejectedBeforeStoreAccess(t *testing.T) {
	fs := &backpressureStore{depth: 1}
	handler := NewJobHandler(fs, &fakePinger{}, testTaskTypeCatalog(t), slog.Default(), nil)

	body := `{"queue":"bp-queue","type":"demo.unknown","payload":{"k":"v"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), TenantIDKey, "bp-tenant")
	rec := httptest.NewRecorder()
	handler.CreateJob(rec, req.WithContext(ctx))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "{\"error\":{\"code\":\"INVALID_ARGUMENT\",\"message\":\"task type is not registered\"}}\n" {
		t.Fatalf("unexpected error response: %s", rec.Body.String())
	}
	if fs.enqueued {
		t.Fatal("unknown task type must not reach the store")
	}
}

// idempotencyStore is a partial store.JobStore fake for idempotency tests.
// Only GetQueueDepth and Enqueue are exercised; enqueue is scriptable.
type idempotencyStore struct {
	store.JobStore // embedded nil; any other call indicates a test bug
	enqueue        func(ctx context.Context, job *domain.Job) (bool, error)
}

func (f *idempotencyStore) GetQueueDepth(_ context.Context, _ string) (int, error) {
	return 0, nil
}

func (f *idempotencyStore) Enqueue(ctx context.Context, job *domain.Job) (bool, error) {
	return f.enqueue(ctx, job)
}

func newIdempotentSubmitRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
	ctx := context.WithValue(req.Context(), TenantIDKey, "idem-tenant")
	return req.WithContext(ctx)
}

// TestCreateJob_IdempotentResubmit_ReturnsExistingJob verifies that an
// identical resubmission returns 202 with deduplicated=true and the real
// job_id of the existing job (PRD v0.1 FR-001).
func TestCreateJob_IdempotentResubmit_ReturnsExistingJob(t *testing.T) {
	const existingID = "existing-job-id"
	fs := &idempotencyStore{
		enqueue: func(_ context.Context, job *domain.Job) (bool, error) {
			// Mirror the store contract: on dedup the passed job is replaced
			// with the existing row.
			job.ID = existingID
			job.State = domain.StateRunning
			return true, nil
		},
	}
	handler := NewJobHandler(fs, &fakePinger{}, testTaskTypeCatalog(t), slog.Default(), nil)

	rec := httptest.NewRecorder()
	body := `{"queue":"idem-queue","type":"demo.echo","payload":{"k":"v"},"idempotency_key":"k1"}`
	handler.CreateJob(rec, newIdempotentSubmitRequest(t, body))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp CreateJobResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Deduplicated {
		t.Fatal("expected deduplicated=true for an identical resubmission")
	}
	if resp.JobID != existingID {
		t.Fatalf("expected the existing job id %q, got %q", existingID, resp.JobID)
	}
}

// TestCreateJob_IdempotencyConflict_Returns409 verifies ADR-0002: reusing an
// idempotency key with different parameters is rejected with 409 CONFLICT,
// and the error message carries the existing job id.
func TestCreateJob_IdempotencyConflict_Returns409(t *testing.T) {
	fs := &idempotencyStore{
		enqueue: func(_ context.Context, _ *domain.Job) (bool, error) {
			return false, domain.NewError(domain.CodeConflict, domain.ErrConflict,
				"idempotency key conflict: existing job orig-job-id was submitted with different parameters")
		},
	}
	handler := NewJobHandler(fs, &fakePinger{}, testTaskTypeCatalog(t), slog.Default(), nil)

	rec := httptest.NewRecorder()
	body := `{"queue":"idem-queue","type":"demo.echo","payload":{"k":"other"},"idempotency_key":"k1"}`
	handler.CreateJob(rec, newIdempotentSubmitRequest(t, body))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}

	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if env.Error.Code != "CONFLICT" {
		t.Fatalf("expected error code CONFLICT, got %q", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "orig-job-id") {
		t.Fatalf("conflict message must carry the existing job id, got %q", env.Error.Message)
	}
}
