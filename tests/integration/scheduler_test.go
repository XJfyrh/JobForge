package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

// setupSchedulerStore creates a SchedulerStore with a dedicated lock connection.
func setupSchedulerStore(t *testing.T) (*postgres.SchedulerStore, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()

	lockConn, err := pgx.Connect(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("connect lock conn: %v", err)
	}
	t.Cleanup(func() { _ = lockConn.Close(ctx) })

	return postgres.NewSchedulerStore(testEnv.pool, lockConn), lockConn
}

// createScheduledJob inserts a job directly with state='scheduled' and a past
// run_at. We bypass domain.NewJob because it correctly sets state='ready' when
// run_at <= now; for scheduler tests we need the DB row to be 'scheduled' with
// an arrived run_at so the scheduler can promote it.
func createScheduledJob(t *testing.T, s store.JobStore, queue, jobType string, runAt time.Time) *domain.Job {
	t.Helper()
	ctx := context.Background()
	id := uuid.New().String()
	now := time.Now()

	_, err := testEnv.pool.Exec(ctx, `
		insert into jobs (id, tenant_id, queue, type, payload, priority, state,
			run_at, attempt, max_attempts, timeout_seconds, fencing_token,
			state_version, created_at, updated_at)
		values ($1, 'test-tenant', $2, $3, '{"scheduled":true}', 0, 'scheduled',
			$4, 0, 3, 300, 0, 1, $5, $5)`,
		id, queue, jobType, runAt, now)
	if err != nil {
		t.Fatalf("insert scheduled job: %v", err)
	}

	job, err := s.GetByID(ctx, "test-tenant", id)
	if err != nil {
		t.Fatalf("get scheduled job: %v", err)
	}
	return job
}

// TestSchedulerPromoteReady verifies that the scheduler promotes scheduled
// jobs to ready once their run_at has arrived.
func TestSchedulerPromoteReady(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// Create a job scheduled in the past (should be promoted immediately).
	pastRunAt := time.Now().Add(-10 * time.Second)
	job := createScheduledJob(t, js, "sched-promote", "demo.echo", pastRunAt)

	// Verify it's in scheduled state.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateScheduled {
		t.Fatalf("expected scheduled, got %s", got.State)
	}

	// Run promote.
	promoted, err := ss.PromoteReady(ctx, 100)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted < 1 {
		t.Fatalf("expected at least 1 promoted, got %d", promoted)
	}

	// Verify job is now ready.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after promote: %v", err)
	}
	if got.State != domain.StateReady {
		t.Errorf("expected ready after promote, got %s", got.State)
	}
}

// TestSchedulerPromoteFutureNotReady verifies that jobs with future run_at
// are NOT promoted.
func TestSchedulerPromoteFutureNotReady(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// Create a job scheduled far in the future.
	futureRunAt := time.Now().Add(1 * time.Hour)
	job := createScheduledJob(t, js, "sched-future", "demo.echo", futureRunAt)

	// Run promote.
	_, err := ss.PromoteReady(ctx, 100)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}

	// Verify job is still scheduled.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateScheduled {
		t.Errorf("expected still scheduled, got %s", got.State)
	}
}

// TestSchedulerPromoteRetryWait verifies that retry_wait jobs with arrived
// run_at are promoted to ready.
func TestSchedulerPromoteRetryWait(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// Create a ready job, claim it, then fail it (retryable) to get retry_wait.
	job := createTestJob(t, js, "sched-retry", "demo.fail")
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queues:   []string{"sched-retry"},
		WorkerID: "worker-sched-retry",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}

	err = js.Fail(ctx, job.ID, "worker-sched-retry", claimed[0].FencingToken,
		"TIMEOUT", "timed out", true, 100)
	if err != nil {
		t.Fatalf("fail: %v", err)
	}

	// Verify retry_wait.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateRetryWait {
		t.Fatalf("expected retry_wait, got %s", got.State)
	}

	// Manually set run_at to the past so promote picks it up.
	_, err = testEnv.pool.Exec(ctx,
		"update jobs set run_at = now() - interval '1 second' where id = $1", job.ID)
	if err != nil {
		t.Fatalf("set run_at past: %v", err)
	}

	// Promote.
	promoted, err := ss.PromoteReady(ctx, 100)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted < 1 {
		t.Fatalf("expected at least 1 promoted, got %d", promoted)
	}

	// Verify ready.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after promote: %v", err)
	}
	if got.State != domain.StateReady {
		t.Errorf("expected ready, got %s", got.State)
	}
}

// TestSchedulerRecoverExpiredLease verifies that running jobs with expired
// leases are recovered back to ready.
func TestSchedulerRecoverExpiredLease(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// Create and claim a job with a very short lease.
	job := createTestJob(t, js, "sched-recover", "demo.echo")
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queues:   []string{"sched-recover"},
		WorkerID: "worker-dead",
		MaxJobs:  1,
		LeaseTTL: 1 * time.Millisecond, // expires almost immediately
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}

	// Verify running.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateRunning {
		t.Fatalf("expected running, got %s", got.State)
	}

	// Force lease expiry by setting lease_until in the past.
	// This avoids Docker/WSL2 clock drift issues between Go and PostgreSQL.
	_, err = testEnv.pool.Exec(ctx,
		"update jobs set lease_until = now() - interval '1 second' where id = $1", job.ID)
	if err != nil {
		t.Fatalf("set lease_until past: %v", err)
	}

	// Recover.
	recovered, err := ss.RecoverExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered < 1 {
		t.Fatalf("expected at least 1 recovered, got %d", recovered)
	}

	// Verify back to ready with cleared lease.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after recover: %v", err)
	}
	if got.State != domain.StateReady {
		t.Errorf("expected ready after recovery, got %s", got.State)
	}
	if got.LeaseOwner != nil {
		t.Errorf("expected nil lease_owner, got %v", *got.LeaseOwner)
	}
}

// TestSchedulerRecoverCancellingExpired verifies that cancelling jobs with
// expired leases transition to cancelled.
func TestSchedulerRecoverCancellingExpired(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// Create, claim, then cancel a job (running → cancelling).
	job := createTestJob(t, js, "sched-cancel-recover", "demo.echo")
	claimed, err := js.Claim(ctx, store.ClaimParams{
		Queues:   []string{"sched-cancel-recover"},
		WorkerID: "worker-cr",
		MaxJobs:  1,
		LeaseTTL: 1 * time.Millisecond,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}

	err = js.Cancel(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}

	// Verify cancelling.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateCancelling {
		t.Fatalf("expected cancelling, got %s", got.State)
	}

	// Force lease expiry by setting lease_until in the past.
	// This avoids Docker/WSL2 clock drift issues between Go and PostgreSQL.
	_, err = testEnv.pool.Exec(ctx,
		"update jobs set lease_until = now() - interval '1 second' where id = $1", job.ID)
	if err != nil {
		t.Fatalf("set lease_until past: %v", err)
	}

	// Recover.
	recovered, err := ss.RecoverExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered < 1 {
		t.Fatalf("expected at least 1 recovered, got %d", recovered)
	}

	// Verify cancelled.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after recover: %v", err)
	}
	if got.State != domain.StateCancelled {
		t.Errorf("expected cancelled, got %s", got.State)
	}
}

// TestSchedulerAdvisoryLock verifies that only one instance can hold the
// scheduler advisory lock at a time.
func TestSchedulerAdvisoryLock(t *testing.T) {
	ctx := context.Background()

	// Create two scheduler stores with separate lock connections.
	lockConn1, err := pgx.Connect(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("connect lockConn1: %v", err)
	}
	defer func() { _ = lockConn1.Close(ctx) }()

	lockConn2, err := pgx.Connect(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("connect lockConn2: %v", err)
	}
	defer func() { _ = lockConn2.Close(ctx) }()

	ss1 := postgres.NewSchedulerStore(testEnv.pool, lockConn1)
	ss2 := postgres.NewSchedulerStore(testEnv.pool, lockConn2)

	// Instance 1 acquires lock.
	acquired, err := ss1.TryAcquireLock(ctx)
	if err != nil {
		t.Fatalf("ss1 acquire: %v", err)
	}
	if !acquired {
		t.Fatal("ss1 should acquire lock")
	}

	// Instance 2 should fail to acquire.
	acquired, err = ss2.TryAcquireLock(ctx)
	if err != nil {
		t.Fatalf("ss2 acquire: %v", err)
	}
	if acquired {
		t.Fatal("ss2 should NOT acquire lock while ss1 holds it")
	}

	// Release from instance 1.
	err = ss1.ReleaseLock(ctx)
	if err != nil {
		t.Fatalf("ss1 release: %v", err)
	}

	// Now instance 2 should succeed.
	acquired, err = ss2.TryAcquireLock(ctx)
	if err != nil {
		t.Fatalf("ss2 acquire after release: %v", err)
	}
	if !acquired {
		t.Fatal("ss2 should acquire lock after ss1 released")
	}

	// Cleanup.
	_ = ss2.ReleaseLock(ctx)
}

// TestSchedulerFailover verifies AT-09: Scheduler high-availability failover.
//
// AT-09 Requirements:
//   - Start dual Schedulers (A and B)
//   - Kill leader (Scheduler A)
//   - Follower (Scheduler B) takes over within 2 × lock_retry_interval + 2s
//   - Tasks are promoted exactly once (no duplicate promotion)
//
// NFR-004: Scheduler takeover time <= 2 × lock_retry_interval + 2s (default 6s)
func TestSchedulerFailover(t *testing.T) {
	ctx := context.Background()

	// Create two scheduler stores with separate lock connections.
	lockConnA, err := pgx.Connect(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("connect lockConnA: %v", err)
	}
	defer func() { _ = lockConnA.Close(ctx) }()

	lockConnB, err := pgx.Connect(ctx, testEnv.dsn)
	if err != nil {
		t.Fatalf("connect lockConnB: %v", err)
	}
	defer func() { _ = lockConnB.Close(ctx) }()

	ssA := postgres.NewSchedulerStore(testEnv.pool, lockConnA)
	ssB := postgres.NewSchedulerStore(testEnv.pool, lockConnB)
	js := setupStore(t)

	// Use short intervals for faster testing.
	lockRetryInterval := 500 * time.Millisecond
	maxTakeoverTime := 2*lockRetryInterval + 2*time.Second // NFR-004: 3s for test

	// 1. Scheduler A acquires leadership.
	acquired, err := ssA.TryAcquireLock(ctx)
	if err != nil {
		t.Fatalf("scheduler A acquire: %v", err)
	}
	if !acquired {
		t.Fatal("scheduler A should acquire leadership")
	}
	t.Log("Scheduler A acquired leadership")

	// 2. Create a scheduled job (run_at in past).
	pastRunAt := time.Now().Add(-10 * time.Second)
	job1 := createScheduledJob(t, js, "failover-test", "demo.echo", pastRunAt)

	// 3. Scheduler A promotes the job.
	promoted, err := ssA.PromoteReady(ctx, 100)
	if err != nil {
		t.Fatalf("scheduler A promote: %v", err)
	}
	if promoted < 1 {
		t.Fatal("scheduler A should promote at least 1 job")
	}

	// Verify job1 is ready.
	got, err := js.GetByID(ctx, "test-tenant", job1.ID)
	if err != nil {
		t.Fatalf("get job1: %v", err)
	}
	if got.State != domain.StateReady {
		t.Fatalf("expected job1 ready, got %s", got.State)
	}
	stateVersionAfterA := got.StateVersion
	t.Logf("Scheduler A promoted job1 (state_version=%d)", stateVersionAfterA)

	// 4. Kill Scheduler A (release lock / simulate crash).
	err = ssA.ReleaseLock(ctx)
	if err != nil {
		t.Fatalf("scheduler A release: %v", err)
	}
	t.Log("Scheduler A killed (lock released)")

	// 5. Scheduler B attempts to acquire leadership.
	// Measure takeover time.
	start := time.Now()
	var acquiredB bool
	deadline := time.Now().Add(maxTakeoverTime)

	for time.Now().Before(deadline) {
		acquiredB, err = ssB.TryAcquireLock(ctx)
		if err != nil {
			t.Fatalf("scheduler B acquire: %v", err)
		}
		if acquiredB {
			break
		}
		time.Sleep(100 * time.Millisecond) // Poll interval
	}

	takeoverTime := time.Since(start)
	if !acquiredB {
		t.Fatalf("AT-09 FAILED: scheduler B did not acquire leadership within %v", maxTakeoverTime)
	}
	t.Logf("Scheduler B acquired leadership (takeover time: %v)", takeoverTime)

	// NFR-004 verification.
	if takeoverTime > maxTakeoverTime {
		t.Errorf("NFR-004 FAILED: takeover time %v exceeds limit %v", takeoverTime, maxTakeoverTime)
	}

	// 6. Create another scheduled job.
	job2 := createScheduledJob(t, js, "failover-test", "demo.echo", pastRunAt)

	// 7. Scheduler B promotes the new job.
	promoted, err = ssB.PromoteReady(ctx, 100)
	if err != nil {
		t.Fatalf("scheduler B promote: %v", err)
	}
	if promoted < 1 {
		t.Fatal("scheduler B should promote at least 1 job")
	}

	// Verify job2 is ready.
	got, err = js.GetByID(ctx, "test-tenant", job2.ID)
	if err != nil {
		t.Fatalf("get job2: %v", err)
	}
	if got.State != domain.StateReady {
		t.Fatalf("expected job2 ready, got %s", got.State)
	}
	t.Log("Scheduler B promoted job2")

	// 8. CRITICAL: Verify job1 was NOT promoted again (no duplicate promotion).
	got, err = js.GetByID(ctx, "test-tenant", job1.ID)
	if err != nil {
		t.Fatalf("get job1 final: %v", err)
	}
	if got.State != domain.StateReady {
		t.Errorf("job1 state changed unexpectedly: %s", got.State)
	}
	if got.StateVersion != stateVersionAfterA {
		t.Errorf("AT-09 FAILED: job1 promoted multiple times (state_version: %d -> %d)",
			stateVersionAfterA, got.StateVersion)
	} else {
		t.Log("AT-09 PASSED: job1 promoted exactly once (no duplicate)")
	}

	// Cleanup.
	_ = ssB.ReleaseLock(ctx)
}
