package integration

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// createTestJob is a helper that enqueues a ready job and returns it.
// RunAt is set 1 second in the past to avoid Docker/WSL2 clock drift issues
// where the host clock may be slightly ahead of the PostgreSQL container clock.
func createTestJob(t *testing.T, s store.JobStore, queue, jobType string) *domain.Job {
	t.Helper()
	id := uuid.New().String()
	pastRunAt := time.Now().Add(-1 * time.Second)
	job, err := domain.NewJob(id, domain.NewJobParams{
		TenantID: "test-tenant",
		Queue:    queue,
		Type:     jobType,
		Payload:  []byte(`{"key":"value"}`),
		RunAt:    &pastRunAt,
	}, time.Now())
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	_, err = s.Enqueue(context.Background(), job)
	if err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	return job
}

// TestConcurrentClaim verifies AT-01: multiple Workers claiming concurrently
// never receive the same job. Each fencing token belongs to exactly one Worker.
func TestConcurrentClaim(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	const numJobs = 20
	const numWorkers = 5

	// Enqueue jobs.
	for i := 0; i < numJobs; i++ {
		createTestJob(t, s, "claim-test", "demo.echo")
	}

	// Concurrent claims.
	type claimResult struct {
		workerID string
		jobIDs   []string
	}

	var wg sync.WaitGroup
	results := make(chan claimResult, numWorkers)

	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func(workerIdx int) {
			defer wg.Done()
			workerID := fmt.Sprintf("worker-%d", workerIdx)
			jobs, err := s.Claim(ctx, store.ClaimParams{
				Queues:   []string{"claim-test"},
				WorkerID: workerID,
				MaxJobs:  numJobs, // each tries to grab all
				LeaseTTL: 30 * time.Second,
			})
			if err != nil {
				t.Errorf("worker %s claim failed: %v", workerID, err)
				return
			}
			ids := make([]string, len(jobs))
			for i, j := range jobs {
				ids[i] = j.ID
			}
			results <- claimResult{workerID: workerID, jobIDs: ids}
		}(w)
	}

	wg.Wait()
	close(results)

	// Verify no job was claimed by more than one worker.
	seen := make(map[string]string) // job_id -> worker_id
	totalClaimed := 0
	for r := range results {
		for _, jobID := range r.jobIDs {
			if prev, exists := seen[jobID]; exists {
				t.Errorf("job %s claimed by both %s and %s", jobID, prev, r.workerID)
			}
			seen[jobID] = r.workerID
			totalClaimed++
		}
	}

	if totalClaimed != numJobs {
		t.Errorf("expected %d total claims, got %d", numJobs, totalClaimed)
	}
}

// TestIdempotentEnqueue verifies FR-001: same tenant + idempotency key returns
// the same job without creating a duplicate.
func TestIdempotentEnqueue(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	key := "idem-key-" + uuid.New().String()
	id1 := uuid.New().String()

	job1, err := domain.NewJob(id1, domain.NewJobParams{
		TenantID:       "test-tenant",
		Queue:          "idem-test",
		Type:           "demo.echo",
		Payload:        []byte(`{}`),
		IdempotencyKey: &key,
	}, time.Now())
	if err != nil {
		t.Fatalf("create job1: %v", err)
	}

	dedup, err := s.Enqueue(ctx, job1)
	if err != nil {
		t.Fatalf("enqueue job1: %v", err)
	}
	if dedup {
		t.Fatal("first enqueue should not be deduplicated")
	}

	// Second enqueue with same key but different job ID.
	id2 := uuid.New().String()
	job2, err := domain.NewJob(id2, domain.NewJobParams{
		TenantID:       "test-tenant",
		Queue:          "idem-test",
		Type:           "demo.echo",
		Payload:        []byte(`{}`),
		IdempotencyKey: &key,
	}, time.Now())
	if err != nil {
		t.Fatalf("create job2: %v", err)
	}

	dedup, err = s.Enqueue(ctx, job2)
	if err != nil {
		t.Fatalf("enqueue job2: %v", err)
	}
	if !dedup {
		t.Fatal("second enqueue with same idempotency key should be deduplicated")
	}

	// The deduplicated submission must report the existing job: job2 is
	// replaced with the original row so the caller receives the real job_id
	// (PRD v0.1: "相同幂等键返回同一任务").
	if job2.ID != id1 {
		t.Fatalf("deduplicated job must be replaced with the existing row: got %s, want %s", job2.ID, id1)
	}
	if job2.State != domain.StateReady {
		t.Fatalf("expected the existing job's state ready, got %s", job2.State)
	}
}

// TestIdempotencyKeyConflict verifies ADR-0002 CONFLICT: reusing an
// idempotency key with different parameters is rejected with CONFLICT
// (HTTP 409) carrying the existing job id, and the original job stays
// untouched.
func TestIdempotencyKeyConflict(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	key := "conflict-key-" + uuid.New().String()

	orig, err := domain.NewJob(uuid.New().String(), domain.NewJobParams{
		TenantID:       "test-tenant",
		Queue:          "conflict-test",
		Type:           "demo.echo",
		Payload:        []byte(`{"version":1}`),
		IdempotencyKey: &key,
	}, time.Now())
	if err != nil {
		t.Fatalf("create original job: %v", err)
	}
	dedup, err := s.Enqueue(ctx, orig)
	if err != nil || dedup {
		t.Fatalf("first enqueue: dedup=%v, err=%v", dedup, err)
	}

	// Same key, different payload → CONFLICT.
	diffPayload, err := domain.NewJob(uuid.New().String(), domain.NewJobParams{
		TenantID:       "test-tenant",
		Queue:          "conflict-test",
		Type:           "demo.echo",
		Payload:        []byte(`{"version":2}`),
		IdempotencyKey: &key,
	}, time.Now())
	if err != nil {
		t.Fatalf("create conflicting job: %v", err)
	}
	dedup, err = s.Enqueue(ctx, diffPayload)
	assertConflict(t, dedup, err, orig.ID)

	// Same key, same payload, different priority → CONFLICT.
	diffPriority, err := domain.NewJob(uuid.New().String(), domain.NewJobParams{
		TenantID:       "test-tenant",
		Queue:          "conflict-test",
		Type:           "demo.echo",
		Payload:        []byte(`{"version":1}`),
		Priority:       5,
		IdempotencyKey: &key,
	}, time.Now())
	if err != nil {
		t.Fatalf("create priority-conflicting job: %v", err)
	}
	dedup, err = s.Enqueue(ctx, diffPriority)
	assertConflict(t, dedup, err, orig.ID)

	// The original job remains untouched.
	got, err := s.GetByID(ctx, "test-tenant", orig.ID)
	if err != nil {
		t.Fatalf("get original job: %v", err)
	}
	if got.State != domain.StateReady {
		t.Fatalf("original job must stay ready, got %s", got.State)
	}
	if got.Priority != 0 {
		t.Fatalf("original job priority must not be overwritten, got %d", got.Priority)
	}
}

// assertConflict verifies that Enqueue returned a CONFLICT domain error whose
// message carries the existing job id.
func assertConflict(t *testing.T, dedup bool, err error, existingID string) {
	t.Helper()
	if err == nil {
		t.Fatalf("same key with different parameters must be rejected, got dedup=%v", dedup)
	}
	de, ok := errors.AsType[*domain.Error](err)
	if !ok || de.Code != domain.CodeConflict {
		t.Fatalf("expected CONFLICT domain error, got %v", err)
	}
	if !strings.Contains(de.Message, existingID) {
		t.Fatalf("conflict message must carry the existing job id %s, got %q", existingID, de.Message)
	}
}

// TestCompleteStaleToken verifies AT-03 precondition: a stale fencing token
// is rejected with STALE_LEASE and does not overwrite the current state.
func TestCompleteStaleToken(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	job := createTestJob(t, s, "stale-test", "demo.echo")

	// Worker-1 claims the job.
	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queues:   []string{"stale-test"},
		WorkerID: "worker-1",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim failed: err=%v, claimed=%d", err, len(claimed))
	}
	token := claimed[0].FencingToken

	// Complete with correct token succeeds.
	err = s.Complete(ctx, job.ID, "worker-1", token, "", 100)
	if err != nil {
		t.Fatalf("complete with valid token: %v", err)
	}

	// Attempt complete again with same (now stale) token.
	err = s.Complete(ctx, job.ID, "worker-1", token, "", 100)
	if err == nil {
		t.Fatal("expected error for stale token complete")
	}
	if !errors.Is(err, domain.ErrStaleLease) && !errors.Is(err, domain.ErrAlreadyTerminal) {
		t.Fatalf("expected STALE_LEASE or ALREADY_TERMINAL, got: %v", err)
	}
}

// TestCancelStates verifies FR-004: waiting-state jobs cancel immediately;
// running jobs enter cancelling.
func TestCancelStates(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	// Cancel a ready job -> should become cancelled.
	readyJob := createTestJob(t, s, "cancel-test", "demo.echo")
	err := s.Cancel(ctx, "test-tenant", readyJob.ID)
	if err != nil {
		t.Fatalf("cancel ready job: %v", err)
	}
	got, err := s.GetByID(ctx, "test-tenant", readyJob.ID)
	if err != nil {
		t.Fatalf("get cancelled job: %v", err)
	}
	if got.State != domain.StateCancelled {
		t.Errorf("expected cancelled, got %s", got.State)
	}

	// Cancel a running job -> should become cancelling.
	runJob := createTestJob(t, s, "cancel-test", "demo.echo")
	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queues:   []string{"cancel-test"},
		WorkerID: "worker-cancel",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim for cancel: err=%v len=%d", err, len(claimed))
	}
	_ = runJob

	err = s.Cancel(ctx, "test-tenant", claimed[0].ID)
	if err != nil {
		t.Fatalf("cancel running job: %v", err)
	}
	got, err = s.GetByID(ctx, "test-tenant", claimed[0].ID)
	if err != nil {
		t.Fatalf("get cancelling job: %v", err)
	}
	if got.State != domain.StateCancelling {
		t.Errorf("expected cancelling, got %s", got.State)
	}
}

// TestCancelTerminal verifies that cancelling a terminal job returns
// ALREADY_TERMINAL.
func TestCancelTerminal(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	job := createTestJob(t, s, "cancel-terminal", "demo.echo")

	// Claim and complete.
	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queues:   []string{"cancel-terminal"},
		WorkerID: "worker-t",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	err = s.Complete(ctx, job.ID, "worker-t", claimed[0].FencingToken, "", 50)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	// Cancel should fail.
	err = s.Cancel(ctx, "test-tenant", job.ID)
	if err == nil {
		t.Fatal("expected error cancelling terminal job")
	}
	if !errors.Is(err, domain.ErrAlreadyTerminal) {
		t.Fatalf("expected ALREADY_TERMINAL, got: %v", err)
	}
}

// TestFailRetry verifies FR-107: a retryable error transitions to retry_wait
// with a future run_at.
func TestFailRetry(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	job := createTestJob(t, s, "retry-test", "demo.fail")

	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queues:   []string{"retry-test"},
		WorkerID: "worker-retry",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}

	before := time.Now()
	err = s.Fail(ctx, job.ID, "worker-retry", claimed[0].FencingToken,
		"TIMEOUT", "connection timed out", true, 500)
	if err != nil {
		t.Fatalf("fail retryable: %v", err)
	}

	got, err := s.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateRetryWait {
		t.Errorf("expected retry_wait, got %s", got.State)
	}
	if got.RunAt.Before(before) {
		t.Errorf("run_at should be in the future, got %v", got.RunAt)
	}
}

// TestFailDead verifies FR-107: exhausting max_attempts transitions to dead.
func TestFailDead(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	// Create a job with max_attempts=1 so first failure is terminal.
	id := uuid.New().String()
	pastRunAt := time.Now().Add(-1 * time.Second)
	job, err := domain.NewJob(id, domain.NewJobParams{
		TenantID:    "test-tenant",
		Queue:       "dead-test",
		Type:        "demo.fail",
		Payload:     []byte(`{}`),
		MaxAttempts: 1,
		RunAt:       &pastRunAt,
	}, time.Now())
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	_, err = s.Enqueue(ctx, job)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queues:   []string{"dead-test"},
		WorkerID: "worker-dead",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}

	err = s.Fail(ctx, job.ID, "worker-dead", claimed[0].FencingToken,
		"FATAL", "unrecoverable", true, 200)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}

	got, err := s.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateDead {
		t.Errorf("expected dead, got %s", got.State)
	}
}

// TestCompleteCancelRace verifies AT-05/AT-06: concurrent Complete and Cancel
// result in exactly one winner (first transaction committed).
func TestCompleteCancelRace(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	const iterations = 20
	for i := 0; i < iterations; i++ {
		job := createTestJob(t, s, "race-test", "demo.echo")

		claimed, err := s.Claim(ctx, store.ClaimParams{
			Queues:   []string{"race-test"},
			WorkerID: "worker-race",
			MaxJobs:  1,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil || len(claimed) == 0 {
			t.Fatalf("claim: %v", err)
		}
		token := claimed[0].FencingToken

		// Race Complete vs Cancel.
		var wg sync.WaitGroup
		var completeErr, cancelErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			completeErr = s.Complete(ctx, job.ID, "worker-race", token, "", 10)
		}()
		go func() {
			defer wg.Done()
			cancelErr = s.Cancel(ctx, "test-tenant", job.ID)
		}()
		wg.Wait()

		// Exactly one should succeed (or both may "succeed" in the sense that
		// cancel transitions to cancelling and complete is rejected).
		got, err := s.GetByID(ctx, "test-tenant", job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}

		switch got.State {
		case domain.StateSucceeded:
			// Complete won. Cancel should have gotten ALREADY_TERMINAL.
			if cancelErr == nil {
				// Cancel may succeed if it ran first (running->cancelling),
				// then complete would fail. But if state is succeeded,
				// cancel must have failed.
				t.Errorf("iter %d: state=succeeded but cancel succeeded", i)
			}
		case domain.StateCancelling:
			// Cancel won. Complete should have gotten CANCEL_REQUESTED.
			if completeErr == nil {
				t.Errorf("iter %d: state=cancelling but complete succeeded", i)
			}
		default:
			t.Errorf("iter %d: unexpected state %s (completeErr=%v, cancelErr=%v)",
				i, got.State, completeErr, cancelErr)
		}
	}
}

// TestHeartbeatStaleToken verifies FR-103: only the correct owner + token
// can extend the lease.
func TestHeartbeatStaleToken(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	job := createTestJob(t, s, "hb-test", "demo.echo")

	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queues:   []string{"hb-test"},
		WorkerID: "worker-hb",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	token := claimed[0].FencingToken

	// Valid heartbeat.
	err = s.Heartbeat(ctx, job.ID, "worker-hb", token, 30*time.Second)
	if err != nil {
		t.Fatalf("valid heartbeat: %v", err)
	}

	// Stale token heartbeat.
	err = s.Heartbeat(ctx, job.ID, "worker-hb", token+999, 30*time.Second)
	if err == nil {
		t.Fatal("expected error for stale token heartbeat")
	}
	if !errors.Is(err, domain.ErrStaleLease) {
		t.Fatalf("expected STALE_LEASE, got: %v", err)
	}

	// Wrong worker heartbeat.
	err = s.Heartbeat(ctx, job.ID, "wrong-worker", token, 30*time.Second)
	if err == nil {
		t.Fatal("expected error for wrong worker heartbeat")
	}
}

// createPriorityJob enqueues a ready job with an explicit priority.
func createPriorityJob(t *testing.T, s store.JobStore, queue string, priority int16) *domain.Job {
	t.Helper()
	pastRunAt := time.Now().Add(-1 * time.Second)
	job, err := domain.NewJob(uuid.New().String(), domain.NewJobParams{
		TenantID: "test-tenant",
		Queue:    queue,
		Type:     "demo.echo",
		Payload:  []byte(`{"key":"value"}`),
		Priority: priority,
		RunAt:    &pastRunAt,
	}, time.Now())
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err = s.Enqueue(context.Background(), job); err != nil {
		t.Fatalf("enqueue job: %v", err)
	}
	return job
}

// TestClaimMultiQueue verifies multi-queue claims: every declared queue
// participates, earlier-declared queues are claimed first, and within a queue
// the priority/created_at ordering still applies.
func TestClaimMultiQueue(t *testing.T) {
	s := setupStore(t)
	ctx := context.Background()

	queueA := "mq-a-" + uuid.New().String()[:8]
	queueB := "mq-b-" + uuid.New().String()[:8]

	// queueA: two jobs, higher priority first within the queue.
	aHigh := createPriorityJob(t, s, queueA, 9)
	aLow := createPriorityJob(t, s, queueA, 1)
	// queueB: one job whose priority (5) exceeds queueA's low job, proving
	// that declaration order dominates over cross-queue priority.
	bMid := createPriorityJob(t, s, queueB, 5)

	claimed, err := s.Claim(ctx, store.ClaimParams{
		Queues:   []string{queueA, queueB},
		WorkerID: "mq-worker",
		MaxJobs:  10,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 3 {
		t.Fatalf("expected 3 claimed jobs across both queues, got %d", len(claimed))
	}

	var gotIDs []string
	for _, j := range claimed {
		gotIDs = append(gotIDs, j.ID)
	}
	wantIDs := []string{aHigh.ID, aLow.ID, bMid.ID}
	for i, want := range wantIDs {
		if gotIDs[i] != want {
			t.Fatalf("claim order mismatch at %d: want %s, got %s (order %v)", i, want, gotIDs[i], gotIDs)
		}
	}

	// Both queues must be represented in the claim result.
	seen := map[string]bool{}
	for _, j := range claimed {
		seen[j.Queue] = true
	}
	if !seen[queueA] || !seen[queueB] {
		t.Fatalf("expected jobs from both queues, got queues %v", seen)
	}

	// Single-queue claim still works and does not touch the other queue.
	leftover, err := s.Claim(ctx, store.ClaimParams{
		Queues:   []string{queueA},
		WorkerID: "mq-worker-2",
		MaxJobs:  10,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("single-queue claim: %v", err)
	}
	if len(leftover) != 0 {
		t.Fatalf("expected no remaining queueA jobs, got %d", len(leftover))
	}
}
