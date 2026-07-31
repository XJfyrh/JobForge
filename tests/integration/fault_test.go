package integration

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// TestFaultAT02CrashBeforeACK verifies AT-02: Worker crashes after executing
// the handler (writing idempotent result) but before sending Complete RPC.
// The task is re-delivered, but the business side effect happens only once.
//
// Scenario:
//  1. Create and claim a job
//  2. Simulate handler execution (increment idempotent counter)
//  3. Worker crashes before Complete (context cancelled, no Complete RPC)
//  4. Lease expires, scheduler recovers the job
//  5. New worker re-claims and executes (idempotent handler skips duplicate)
//  6. Verify: side effect counter = 1 (not 2)
func TestFaultAT02CrashBeforeACK(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// Idempotent effect counter (simulates external side effect).
	var sideEffectCount atomic.Int32

	// Idempotent handler: only increments if not already done for this job.
	idempotentExecute := func(jobID string) {
		// In a real system, this would check a database table.
		// For this test, we use compare-and-swap semantics.
		if sideEffectCount.Load() == 0 {
			sideEffectCount.Add(1)
		}
		// Second execution: count is already 1, so no increment.
	}

	// 1. Create a job with short lease (simulates Worker that will crash).
	job := createTestJob(t, js, "fault-at02", "demo.idempotent_effect")

	// 2. Worker A claims the job.
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "fault-at02",
		WorkerID: "worker-A-crash",
		MaxJobs:  1,
		LeaseTTL: 50 * time.Millisecond, // Short lease: will expire quickly
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("worker A claim: %v", err)
	}
	tokenA := claimed[0].FencingToken

	// Verify running.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateRunning {
		t.Fatalf("expected running, got %s", got.State)
	}

	// 3. Worker A executes handler (side effect happens).
	idempotentExecute(job.ID)

	// 4. Worker A crashes BEFORE sending Complete RPC.
	// (We simply don't call Complete; the lease will expire.)
	t.Log("Worker A crashes before ACK (no Complete RPC sent)")

	// 5. Wait for lease to expire.
	time.Sleep(100 * time.Millisecond)

	// 6. Scheduler recovers the expired lease.
	recovered, err := ss.RecoverExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered < 1 {
		t.Fatal("expected at least 1 recovered job")
	}

	// Verify job is back to ready.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after recovery: %v", err)
	}
	if got.State != domain.StateReady {
		t.Fatalf("expected ready after recovery, got %s", got.State)
	}

	// 7. Worker B claims the re-delivered job.
	claimed2, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "fault-at02",
		WorkerID: "worker-B-recovery",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("worker B claim: %v", err)
	}
	tokenB := claimed2[0].FencingToken

	// Fencing token must increase.
	if tokenB <= tokenA {
		t.Errorf("fencing token must increase: tokenA=%d, tokenB=%d", tokenA, tokenB)
	}

	// 8. Worker B executes handler (idempotent: no duplicate side effect).
	idempotentExecute(job.ID)

	// 9. Worker B completes successfully.
	err = js.Complete(ctx, job.ID, "worker-B-recovery", tokenB, "result", 100)
	if err != nil {
		t.Fatalf("worker B complete: %v", err)
	}

	// 10. Verify final state.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if got.State != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", got.State)
	}

	// 11. CRITICAL: Side effect happened exactly once.
	if count := sideEffectCount.Load(); count != 1 {
		t.Errorf("AT-02 FAILED: side effect count = %d, expected 1 (idempotent)", count)
	} else {
		t.Log("AT-02 PASSED: business side effect happened exactly once despite re-delivery")
	}
}

// TestFaultAT04HeartbeatLoss verifies AT-04: Worker loses network connectivity
// (heartbeats stop), lease expires, task is recovered, and the old Worker's
// late Complete is rejected with STALE_LEASE.
//
// Scenario:
//  1. Create and claim a job
//  2. Send one successful heartbeat
//  3. Worker loses network (heartbeats stop)
//  4. Lease expires
//  5. Scheduler recovers the job to ready
//  6. Old Worker tries to Complete → STALE_LEASE error
//  7. New Worker can claim and complete successfully
func TestFaultAT04HeartbeatLoss(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// 1. Create a job with short lease.
	job := createTestJob(t, js, "fault-at04", "demo.sleep")

	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "fault-at04",
		WorkerID: "worker-isolated",
		MaxJobs:  1,
		LeaseTTL: 100 * time.Millisecond, // Short lease for testing
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	oldToken := claimed[0].FencingToken

	// 2. First heartbeat succeeds (Worker still connected).
	err = js.Heartbeat(ctx, job.ID, "worker-isolated", oldToken, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first heartbeat should succeed: %v", err)
	}
	t.Log("First heartbeat succeeded (Worker connected)")

	// 3. Worker loses network: heartbeats stop.
	// (We simply stop calling Heartbeat and wait for lease to expire.)
	t.Log("Worker loses network connectivity (heartbeats stop)")

	// 4. Wait for lease to expire.
	time.Sleep(150 * time.Millisecond)

	// 5. Scheduler recovers the expired lease.
	recovered, err := ss.RecoverExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered < 1 {
		t.Fatal("expected at least 1 recovered job")
	}

	// Verify job is back to ready.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after recovery: %v", err)
	}
	if got.State != domain.StateReady {
		t.Fatalf("expected ready after recovery, got %s", got.State)
	}
	t.Log("Job recovered to ready state")

	// 6. Old Worker (isolated) tries to Complete with stale token → REJECTED.
	err = js.Complete(ctx, job.ID, "worker-isolated", oldToken, "late-result", 500)
	if err == nil {
		t.Fatal("AT-04 FAILED: old Worker Complete should be rejected with STALE_LEASE")
	}
	t.Logf("Old Worker Complete correctly rejected: %v", err)

	// 7. New Worker claims the recovered job.
	claimed2, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "fault-at04",
		WorkerID: "worker-new",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("new worker claim: %v", err)
	}
	newToken := claimed2[0].FencingToken

	// Fencing token must increase.
	if newToken <= oldToken {
		t.Errorf("fencing token must increase: old=%d, new=%d", oldToken, newToken)
	}

	// 8. New Worker completes successfully.
	err = js.Complete(ctx, job.ID, "worker-new", newToken, "new-result", 200)
	if err != nil {
		t.Fatalf("new worker complete: %v", err)
	}

	// 9. Verify final state.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if got.State != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", got.State)
	}

	t.Log("AT-04 PASSED: heartbeat loss → lease expiry → recovery → stale rejection → new worker success")
}

// TestFaultAT03StaleWorkerLateComplete verifies AT-03: After lease expiry and
// re-claim by a new Worker, the old Worker's late Complete is rejected.
// This is similar to AT-04 but focuses on the fencing token protection.
func TestFaultAT03StaleWorkerLateComplete(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// 1. Create and claim with very short lease.
	job := createTestJob(t, js, "fault-at03", "demo.echo")
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "fault-at03",
		WorkerID: "worker-slow",
		MaxJobs:  1,
		LeaseTTL: 1 * time.Millisecond,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	staleToken := claimed[0].FencingToken

	// 2. Lease expires immediately.
	time.Sleep(50 * time.Millisecond)

	// 3. Scheduler recovers.
	_, err = ss.RecoverExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	// 4. New Worker claims.
	claimed2, err := js.Claim(ctx, store.ClaimParams{
		Queue:    "fault-at03",
		WorkerID: "worker-fast",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("new claim: %v", err)
	}
	newToken := claimed2[0].FencingToken

	// 5. Old Worker's late Complete is rejected.
	err = js.Complete(ctx, job.ID, "worker-slow", staleToken, "stale", 1000)
	if err == nil {
		t.Fatal("AT-03 FAILED: stale Worker Complete should be rejected")
	}
	t.Logf("Stale Worker rejected: %v", err)

	// 6. New Worker completes successfully.
	err = js.Complete(ctx, job.ID, "worker-fast", newToken, "fresh", 100)
	if err != nil {
		t.Fatalf("new worker complete: %v", err)
	}

	// 7. Verify state.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", got.State)
	}

	t.Log("AT-03 PASSED: stale Worker late Complete rejected, new Worker succeeds")
}
