package integration

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// TestAttemptTimelineStore verifies FR-002 at the store layer: every
// claim/fail/complete writes a job_attempts record and ListAttempts returns
// the full timeline ordered by attempt_no, scoped to the owning tenant.
func TestAttemptTimelineStore(t *testing.T) {
	js := setupStore(t)
	ctx := context.Background()

	tenant := "attempts-tenant-" + uuid.New().String()[:8]
	queue := "attempts-queue"

	job := createTestJobForTenant(t, js, tenant, queue, "demo.fail")

	// Attempt 1: claim then fail retryable.
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queues: []string{queue}, WorkerID: "attempt-worker-1", MaxJobs: 1, LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim 1: %v (claimed %d)", err, len(claimed))
	}
	err = js.Fail(ctx, claimed[0].ID, "attempt-worker-1", claimed[0].FencingToken,
		"TRANSIENT", "temporary outage", true, 42)
	if err != nil {
		t.Fatalf("fail 1: %v", err)
	}

	// Backoff moves run_at into the future; pull it back for the test.
	if _, err := testEnv.pool.Exec(ctx,
		"update jobs set run_at = now() - interval '1 second' where id = $1", job.ID); err != nil {
		t.Fatalf("reset run_at: %v", err)
	}
	if _, err := testEnv.pool.Exec(ctx,
		"update jobs set state = 'ready' where id = $1 and state = 'retry_wait'", job.ID); err != nil {
		t.Fatalf("reset state: %v", err)
	}

	// Attempt 2: claim then complete.
	claimed2, err := js.Claim(ctx, store.ClaimParams{
		Queues: []string{queue}, WorkerID: "attempt-worker-2", MaxJobs: 1, LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("claim 2: %v (claimed %d)", err, len(claimed2))
	}
	if err := js.Complete(ctx, claimed2[0].ID, "attempt-worker-2", claimed2[0].FencingToken, "ref", 123); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Verify the timeline.
	attempts, err := js.ListAttempts(ctx, tenant, job.ID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(attempts))
	}

	first, second := attempts[0], attempts[1]
	if first.AttemptNo != 1 || second.AttemptNo != 2 {
		t.Errorf("unexpected attempt ordering: %d, %d", first.AttemptNo, second.AttemptNo)
	}
	if first.Outcome != "failed_retry" {
		t.Errorf("attempt 1 outcome = %q, want failed_retry", first.Outcome)
	}
	if first.ErrorCode == nil || *first.ErrorCode != "TRANSIENT" {
		t.Errorf("attempt 1 error_code = %v, want TRANSIENT", first.ErrorCode)
	}
	if first.WorkerID != "attempt-worker-1" || second.WorkerID != "attempt-worker-2" {
		t.Errorf("worker ids = %q, %q", first.WorkerID, second.WorkerID)
	}
	if second.Outcome != "succeeded" {
		t.Errorf("attempt 2 outcome = %q, want succeeded", second.Outcome)
	}
	if second.DurationMs == nil || *second.DurationMs != 123 {
		t.Errorf("attempt 2 duration_ms = %v, want 123", second.DurationMs)
	}
	if first.FencingToken == second.FencingToken {
		t.Error("fencing tokens must differ across attempts")
	}

	// Tenant scoping: a foreign tenant must not see the timeline.
	if _, err := js.ListAttempts(ctx, "other-tenant", job.ID); err == nil {
		t.Error("expected NOT_FOUND for foreign tenant")
	}
}

// TestGetJobReturnsAttemptTimeline verifies FR-002 at the HTTP layer:
// GET /v1/jobs/{job_id} includes the attempt timeline.
func TestGetJobReturnsAttemptTimeline(t *testing.T) {
	js := setupStore(t)
	ctx := context.Background()

	tenant := "attempts-http-" + uuid.New().String()[:8]
	queue := "attempts-http-queue"
	job := createTestJobForTenant(t, js, tenant, queue, "demo.echo")

	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queues: []string{queue}, WorkerID: "http-attempt-worker", MaxJobs: 1, LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	if err := js.Complete(ctx, claimed[0].ID, "http-attempt-worker", claimed[0].FencingToken, "ref", 77); err != nil {
		t.Fatalf("complete: %v", err)
	}

	handler := apihttp.NewJobHandler(js, js, slog.Default(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+job.ID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("job_id", job.ID)
	reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	reqCtx = context.WithValue(reqCtx, apihttp.TenantIDKey, tenant)
	req = req.WithContext(reqCtx)

	rec := httptest.NewRecorder()
	handler.GetJob(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		ID       string `json:"id"`
		State    string `json:"state"`
		Attempts []struct {
			AttemptNo  int    `json:"attempt_no"`
			WorkerID   string `json:"worker_id"`
			Outcome    string `json:"outcome"`
			DurationMs *int64 `json:"duration_ms"`
		} `json:"attempts"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.ID != job.ID || body.State != string(domain.StateSucceeded) {
		t.Fatalf("unexpected job body: %+v", body)
	}
	if len(body.Attempts) != 1 {
		t.Fatalf("expected 1 attempt in response, got %d", len(body.Attempts))
	}
	a := body.Attempts[0]
	if a.AttemptNo != 1 || a.Outcome != "succeeded" || a.WorkerID != "http-attempt-worker" {
		t.Errorf("unexpected attempt entry: %+v", a)
	}
	if a.DurationMs == nil || *a.DurationMs != 77 {
		t.Errorf("attempt duration_ms = %v, want 77", a.DurationMs)
	}
}
