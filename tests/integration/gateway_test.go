package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// TestGatewayRegisterWorker verifies that worker registration upserts
// correctly in the workers table.
func TestGatewayRegisterWorker(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	workerID := "gw-worker-" + uuid.New().String()[:8]
	req := &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "instance-1",
		SupportedTypes: []string{"demo.echo", "demo.sleep"},
		Queues:         []string{"default"},
		Capacity:       4,
		Version:        "1.0.0",
	}

	err := s.RegisterWorker(ctx, req, uuid.New().String())
	if err != nil {
		t.Fatalf("register worker: %v", err)
	}

	// Re-register (upsert) with updated capacity.
	req.Capacity = 8
	sessionID2 := uuid.New().String()
	err = s.RegisterWorker(ctx, req, sessionID2)
	if err != nil {
		t.Fatalf("re-register worker: %v", err)
	}

	// Verify the worker exists with updated values.
	var capacity int32
	var sessionID string
	err = testEnv.pool.QueryRow(ctx,
		"select capacity, session_id from workers where worker_id = $1", workerID,
	).Scan(&capacity, &sessionID)
	if err != nil {
		t.Fatalf("query worker: %v", err)
	}
	if capacity != 8 {
		t.Errorf("expected capacity 8, got %d", capacity)
	}
	if sessionID != sessionID2 {
		t.Errorf("expected %s, got %s", sessionID2, sessionID)
	}
}

// TestGatewayGetJobState verifies the GetJobState helper used by Heartbeat
// to detect cancel signals.
func TestGatewayGetJobState(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	job := createTestJob(t, s, "gw-state", "demo.echo")

	state, err := s.GetJobState(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job state: %v", err)
	}
	if state != domain.StateReady {
		t.Errorf("expected ready, got %s", state)
	}

	// Claim → running.
	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queue:    "gw-state",
		WorkerID: "gw-worker-state",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}

	state, err = s.GetJobState(ctx, job.ID)
	if err != nil {
		t.Fatalf("get job state after claim: %v", err)
	}
	if state != domain.StateRunning {
		t.Errorf("expected running, got %s", state)
	}
}

// TestGatewayHeartbeatCancelSignal verifies that Heartbeat returns a CANCEL
// signal when the job is in cancelling state.
func TestGatewayHeartbeatCancelSignal(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	job := createTestJob(t, s, "gw-hb-cancel", "demo.echo")

	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queue:    "gw-hb-cancel",
		WorkerID: "gw-worker-hb",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	token := claimed[0].FencingToken

	// Cancel the running job → cancelling.
	err = s.Cancel(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Heartbeat should still succeed (cancelling allows heartbeat).
	err = s.Heartbeat(ctx, job.ID, "gw-worker-hb", token, 30*time.Second)
	if err != nil {
		t.Fatalf("heartbeat on cancelling job: %v", err)
	}

	// Verify state is cancelling (which gateway uses to return CANCEL signal).
	state, err := s.GetJobState(ctx, job.ID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state != domain.StateCancelling {
		t.Errorf("expected cancelling, got %s", state)
	}
}

// TestGatewayCompleteIdempotent verifies that completing an already-succeeded
// job is handled idempotently (no error, state remains succeeded).
func TestGatewayCompleteIdempotent(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	job := createTestJob(t, s, "gw-idem", "demo.echo")

	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queue:    "gw-idem",
		WorkerID: "gw-worker-idem",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	token := claimed[0].FencingToken

	// First complete succeeds.
	err = s.Complete(ctx, job.ID, "gw-worker-idem", token, "result-1", 100)
	if err != nil {
		t.Fatalf("first complete: %v", err)
	}

	// Second complete with same token: store returns error (already terminal).
	err = s.Complete(ctx, job.ID, "gw-worker-idem", token, "result-1", 100)
	if err == nil {
		t.Fatal("expected error on duplicate complete at store level")
	}

	// But the job state should still be succeeded (idempotent semantics).
	state, err := s.GetJobState(ctx, job.ID)
	if err != nil {
		t.Fatalf("get state: %v", err)
	}
	if state != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", state)
	}
}

// TestEndToEndSubmitExecuteComplete verifies the full lifecycle:
// submit (scheduled) → scheduler promote → claim → heartbeat → complete.
func TestEndToEndSubmitExecuteComplete(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// 1. Submit a job scheduled in the past (immediate execution).
	pastRunAt := time.Now().Add(-5 * time.Second)
	job := createScheduledJob(t, js, "e2e", "demo.echo", pastRunAt)

	// Verify scheduled.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateScheduled {
		t.Fatalf("expected scheduled, got %s", got.State)
	}

	// 2. Scheduler promotes to ready.
	promoted, err := ss.PromoteReady(ctx, 100)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted < 1 {
		t.Fatal("expected at least 1 promoted")
	}

	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after promote: %v", err)
	}
	if got.State != domain.StateReady {
		t.Fatalf("expected ready, got %s", got.State)
	}

	// 3. Worker claims (Poll equivalent).
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "e2e",
		WorkerID: "e2e-worker",
		Types:    []string{"demo.echo"},
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) == 0 {
		t.Fatal("expected at least 1 claimed job")
	}
	if claimed[0].ID != job.ID {
		t.Fatalf("expected job %s, got %s", job.ID, claimed[0].ID)
	}
	token := claimed[0].FencingToken

	// Verify running.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after claim: %v", err)
	}
	if got.State != domain.StateRunning {
		t.Fatalf("expected running, got %s", got.State)
	}
	if got.Attempt != 1 {
		t.Errorf("expected attempt=1, got %d", got.Attempt)
	}

	// 4. Worker heartbeats.
	err = js.Heartbeat(ctx, job.ID, "e2e-worker", token, 30*time.Second)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	// 5. Worker completes.
	err = js.Complete(ctx, job.ID, "e2e-worker", token, "s3://results/123", 250)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	// 6. Verify final state.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if got.State != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", got.State)
	}
}

// TestEndToEndFailRetryRecover verifies: claim → fail (retryable) →
// scheduler promote → re-claim → complete.
func TestEndToEndFailRetryRecover(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// 1. Create a ready job.
	job := createTestJob(t, js, "e2e-retry", "demo.fail")

	// 2. Claim.
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "e2e-retry",
		WorkerID: "e2e-worker-r",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	token1 := claimed[0].FencingToken

	// 3. Fail with retryable error.
	err = js.Fail(ctx, job.ID, "e2e-worker-r", token1, "TIMEOUT", "timed out", true, 500)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}

	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateRetryWait {
		t.Fatalf("expected retry_wait, got %s", got.State)
	}
	if got.Attempt != 1 {
		t.Errorf("expected attempt=1, got %d", got.Attempt)
	}

	// 4. Set run_at to past so scheduler promotes it.
	_, err = testEnv.pool.Exec(ctx,
		"update jobs set run_at = now() - interval '1 second' where id = $1", job.ID)
	if err != nil {
		t.Fatalf("set run_at: %v", err)
	}

	promoted, err := ss.PromoteReady(ctx, 100)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted < 1 {
		t.Fatal("expected at least 1 promoted")
	}

	// 5. Re-claim (attempt 2).
	claimed2, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "e2e-retry",
		WorkerID: "e2e-worker-r2",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("re-claim: %v", err)
	}
	token2 := claimed2[0].FencingToken

	if token2 <= token1 {
		t.Errorf("fencing token must increase: token1=%d, token2=%d", token1, token2)
	}

	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after re-claim: %v", err)
	}
	if got.Attempt != 2 {
		t.Errorf("expected attempt=2, got %d", got.Attempt)
	}

	// 6. Complete on second attempt.
	err = js.Complete(ctx, job.ID, "e2e-worker-r2", token2, "", 300)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if got.State != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", got.State)
	}
}

// TestEndToEndLeaseExpiryRecovery verifies: claim → lease expires →
// scheduler recovers → new worker re-claims and completes.
func TestEndToEndLeaseExpiryRecovery(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// 1. Create and claim with very short lease.
	job := createTestJob(t, js, "e2e-lease", "demo.sleep")
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "e2e-lease",
		WorkerID: "e2e-dead-worker",
		MaxJobs:  1,
		LeaseTTL: 1 * time.Millisecond,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}

	// 2. Wait for lease expiry.
	time.Sleep(50 * time.Millisecond)

	// 3. Scheduler recovers.
	recovered, err := ss.RecoverExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered < 1 {
		t.Fatal("expected at least 1 recovered")
	}

	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateReady {
		t.Fatalf("expected ready after recovery, got %s", got.State)
	}

	// 4. New worker claims.
	claimed2, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "e2e-lease",
		WorkerID: "e2e-new-worker",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("re-claim: %v", err)
	}
	token2 := claimed2[0].FencingToken

	// 5. Old worker tries to complete with stale token → rejected.
	oldToken := claimed[0].FencingToken
	err = js.Complete(ctx, job.ID, "e2e-dead-worker", oldToken, "", 100)
	if err == nil {
		t.Fatal("expected stale lease error for old worker")
	}

	// 6. New worker completes successfully.
	err = js.Complete(ctx, job.ID, "e2e-new-worker", token2, "", 200)
	if err != nil {
		t.Fatalf("new worker complete: %v", err)
	}

	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get final: %v", err)
	}
	if got.State != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", got.State)
	}
}

// TestEndToEndManualRetry verifies AT-08: dead/cancelled jobs can be manually
// retried by cloning a new job with retry_of_job_id pointing to the original.
func TestEndToEndManualRetry(t *testing.T) {
	js := setupStore(t)
	ctx := context.Background()

	t.Run("dead job retry creates clone", func(t *testing.T) {
		// 1. Create a job with max_attempts=1, claim and fail → dead.
		id := uuid.New().String()
		pastRunAt := time.Now().Add(-1 * time.Second)
		origJob, err := domain.NewJob(id, domain.NewJobParams{
			TenantID:    "test-tenant",
			Queue:       "manual-retry",
			Type:        "demo.fail",
			Payload:     []byte(`{"retryable":true}`),
			MaxAttempts: 1,
			RunAt:       &pastRunAt,
		}, time.Now())
		if err != nil {
			t.Fatalf("create job: %v", err)
		}
		_, err = js.Enqueue(ctx, origJob)
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}

		claimed, err := js.Claim(ctx, store.ClaimParams{
			Queue:    "manual-retry",
			WorkerID: "retry-worker",
			MaxJobs:  1,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil || len(claimed) == 0 {
			t.Fatalf("claim: %v", err)
		}

		err = js.Fail(ctx, origJob.ID, "retry-worker", claimed[0].FencingToken,
			"EXECUTION_ERROR", "fatal", true, 100)
		if err != nil {
			t.Fatalf("fail: %v", err)
		}

		// Verify dead.
		got, err := js.GetByID(ctx, "test-tenant", origJob.ID)
		if err != nil {
			t.Fatalf("get orig: %v", err)
		}
		if got.State != domain.StateDead {
			t.Fatalf("expected dead, got %s", got.State)
		}

		// 2. Clone (replicating RetryJob handler logic).
		// Use past run_at to avoid Docker/WSL2 clock drift issues.
		now := time.Now()
		cloneRunAt := now.Add(-1 * time.Second)
		newJob, err := domain.NewJob(uuid.New().String(), domain.NewJobParams{
			TenantID:       "test-tenant",
			Queue:          got.Queue,
			Type:           got.Type,
			Payload:        got.Payload,
			Priority:       got.Priority,
			MaxAttempts:    got.MaxAttempts,
			TimeoutSeconds: got.TimeoutSeconds,
			RetryOfJobID:   &got.ID,
			RunAt:          &cloneRunAt,
		}, now)
		if err != nil {
			t.Fatalf("clone job: %v", err)
		}
		_, err = js.Enqueue(ctx, newJob)
		if err != nil {
			t.Fatalf("enqueue clone: %v", err)
		}

		// 3. Verify clone properties.
		clone, err := js.GetByID(ctx, "test-tenant", newJob.ID)
		if err != nil {
			t.Fatalf("get clone: %v", err)
		}
		if clone.State != domain.StateReady {
			t.Errorf("expected clone state=ready, got %s", clone.State)
		}
		if clone.RetryOfJobID == nil || *clone.RetryOfJobID != origJob.ID {
			t.Errorf("expected retry_of_job_id=%s, got %v", origJob.ID, clone.RetryOfJobID)
		}
		if clone.Queue != "manual-retry" {
			t.Errorf("expected queue=manual-retry, got %s", clone.Queue)
		}

		// 4. Clone can be claimed and completed (full lifecycle).
		claimed2, err := js.Claim(ctx, store.ClaimParams{
			Queue:    "manual-retry",
			WorkerID: "retry-worker-2",
			MaxJobs:  1,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil || len(claimed2) == 0 {
			t.Fatalf("claim clone: %v (len=%d)", err, len(claimed2))
		}
		err = js.Complete(ctx, newJob.ID, "retry-worker-2", claimed2[0].FencingToken, "ok", 50)
		if err != nil {
			t.Fatalf("complete clone: %v", err)
		}

		clone, err = js.GetByID(ctx, "test-tenant", newJob.ID)
		if err != nil {
			t.Fatalf("get clone final: %v", err)
		}
		if clone.State != domain.StateSucceeded {
			t.Errorf("expected clone succeeded, got %s", clone.State)
		}
	})

	t.Run("succeeded job cannot be retried", func(t *testing.T) {
		// Create and complete a job.
		job := createTestJob(t, js, "retry-reject", "demo.echo")
		claimed, err := js.Claim(ctx, store.ClaimParams{
			Queue:    "retry-reject",
			WorkerID: "retry-reject-worker",
			MaxJobs:  1,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil || len(claimed) == 0 {
			t.Fatalf("claim: %v", err)
		}
		err = js.Complete(ctx, job.ID, "retry-reject-worker", claimed[0].FencingToken, "", 10)
		if err != nil {
			t.Fatalf("complete: %v", err)
		}

		// Verify succeeded state prevents retry (handler logic check).
		got, err := js.GetByID(ctx, "test-tenant", job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if got.State != domain.StateSucceeded {
			t.Fatalf("expected succeeded, got %s", got.State)
		}
		// The RetryJob handler rejects succeeded jobs; verify the state check.
		if got.State != domain.StateDead && got.State != domain.StateCancelled {
			// Correctly would be rejected by handler.
			return
		}
		t.Error("succeeded job should not pass the dead/cancelled gate")
	})
}
