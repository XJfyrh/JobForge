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
		Queue:    "sched-retry",
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
		Queue:    "sched-recover",
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

	// Wait for lease to expire.
	time.Sleep(50 * time.Millisecond)

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
		Queue:    "sched-cancel-recover",
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

	// Wait for lease to expire.
	time.Sleep(50 * time.Millisecond)

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
