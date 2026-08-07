package integration

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// TestWorkerAT11GracefulShutdown verifies AT-11: Worker graceful shutdown.
//
// AT-11 Requirements:
//   - Worker stops claiming new jobs after shutdown signal
//   - In-flight jobs complete or are safely released within grace period
//   - No goroutine leaks
//
// This test verifies the store-level semantics that support graceful shutdown:
//  1. A running job with active heartbeat can be completed
//  2. A running job whose worker stops heartbeating will be recovered
//  3. Jobs claimed during shutdown are handled correctly
//
// Note: Full Worker Runtime graceful shutdown testing (goroutine lifecycle,
// signal handling) is covered by unit tests in internal/worker/runtime_test.go.
// This integration test focuses on the database-level guarantees.
func TestWorkerAT11GracefulShutdown(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	t.Run("inflight job completes before shutdown", func(t *testing.T) {
		// Simulate: Worker claims job, receives shutdown signal, but completes
		// the in-flight job within grace period.

		job := createTestJob(t, js, "at11-complete", "demo.echo")

		claimed, err := js.Claim(ctx, store.ClaimParams{
			Queues:   []string{"at11-complete"},
			WorkerID: "worker-graceful",
			MaxJobs:  1,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil || len(claimed) == 0 {
			t.Fatalf("claim: %v", err)
		}
		token := claimed[0].FencingToken

		// Worker receives shutdown signal but continues in-flight work.
		// Heartbeat still works during grace period.
		err = js.Heartbeat(ctx, job.ID, "worker-graceful", token, 30*time.Second)
		if err != nil {
			t.Fatalf("heartbeat during grace period: %v", err)
		}

		// Worker completes the job before grace period expires.
		err = js.Complete(ctx, job.ID, "worker-graceful", token, "completed", 100)
		if err != nil {
			t.Fatalf("complete during grace period: %v", err)
		}

		got, err := js.GetByID(ctx, "test-tenant", job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if got.State != domain.StateSucceeded {
			t.Errorf("expected succeeded, got %s", got.State)
		}
	})

	t.Run("inflight job released after grace period", func(t *testing.T) {
		// Simulate: Worker claims job, receives shutdown signal, but cannot
		// complete within grace period. Job is released for recovery.

		job := createTestJob(t, js, "at11-release", "demo.sleep")

		claimed, err := js.Claim(ctx, store.ClaimParams{
			Queues:   []string{"at11-release"},
			WorkerID: "worker-timeout",
			MaxJobs:  1,
			LeaseTTL: 50 * time.Millisecond, // Short lease simulates grace period expiry
		})
		if err != nil || len(claimed) == 0 {
			t.Fatalf("claim: %v", err)
		}

		// Worker stops heartbeating (grace period expired or shutdown forced).
		// Force lease expiry by setting lease_until in the past.
		// This avoids Docker/WSL2 clock drift issues between Go and PostgreSQL.
		_, err = testEnv.pool.Exec(ctx,
			"update jobs set lease_until = now() - interval '1 second' where id = $1", job.ID)
		if err != nil {
			t.Fatalf("set lease_until past: %v", err)
		}

		// Scheduler recovers the abandoned job.
		recovered, err := ss.RecoverExpiredLeases(ctx)
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if recovered < 1 {
			t.Fatal("expected at least 1 recovered job")
		}

		got, err := js.GetByID(ctx, "test-tenant", job.ID)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		if got.State != domain.StateReady {
			t.Errorf("expected ready after recovery, got %s", got.State)
		}

		// Another worker can now claim and complete the job.
		claimed2, err := js.Claim(ctx, store.ClaimParams{
			Queues:   []string{"at11-release"},
			WorkerID: "worker-replacement",
			MaxJobs:  1,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil || len(claimed2) == 0 {
			t.Fatalf("replacement claim: %v", err)
		}

		err = js.Complete(ctx, job.ID, "worker-replacement", claimed2[0].FencingToken, "ok", 50)
		if err != nil {
			t.Fatalf("replacement complete: %v", err)
		}
	})

	t.Run("no new claims during shutdown", func(t *testing.T) {
		// Simulate: Worker in shutdown mode should not claim new jobs.
		// At the store level, this is enforced by the Runtime not calling Claim.
		// Here we verify that jobs remain available for other workers.

		// Create jobs that would be claimed.
		for i := 0; i < 3; i++ {
			createTestJob(t, js, "at11-no-claim", "demo.echo")
		}

		// Worker A is shutting down (does not claim).
		// Worker B (active) claims all available jobs.
		claimed, err := js.Claim(ctx, store.ClaimParams{
			Queues:   []string{"at11-no-claim"},
			WorkerID: "worker-active",
			MaxJobs:  10,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("active worker claim: %v", err)
		}

		// All jobs should be claimed by the active worker.
		if len(claimed) != 3 {
			t.Errorf("expected 3 jobs claimed, got %d", len(claimed))
		}
	})
}

// TestWorkerGoroutineStability verifies that goroutine count returns to
// baseline after job processing (related to NFR 11.2.5).
//
// This is a basic sanity check; comprehensive goroutine leak detection
// is performed in the benchmark suite with 10,000 jobs.
func TestWorkerGoroutineStability(t *testing.T) {
	js := setupStore(t)
	ctx := context.Background()

	// Record baseline goroutine count.
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	// Process multiple jobs (claim + complete cycle).
	for i := 0; i < 10; i++ {
		job := createTestJob(t, js, "goroutine-stability", "demo.echo")
		claimed, err := js.Claim(ctx, store.ClaimParams{
			Queues:   []string{"goroutine-stability"},
			WorkerID: "worker-stability",
			MaxJobs:  1,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil || len(claimed) == 0 {
			t.Fatalf("claim %d: %v", i, err)
		}
		err = js.Complete(ctx, job.ID, "worker-stability", claimed[0].FencingToken, "", 10)
		if err != nil {
			t.Fatalf("complete %d: %v", i, err)
		}
	}

	// Allow goroutines to settle.
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	final := runtime.NumGoroutine()

	// Goroutine count should be stable (within ±5 or ±20% tolerance).
	diff := final - baseline
	if diff < 0 {
		diff = -diff
	}
	tolerance := baseline / 5 // 20% tolerance
	if tolerance < 5 {
		tolerance = 5
	}

	if diff > tolerance {
		t.Errorf("goroutine leak suspected: baseline=%d, final=%d, diff=%d (tolerance=%d)",
			baseline, final, diff, tolerance)
	} else {
		t.Logf("goroutine stability OK: baseline=%d, final=%d, diff=%d", baseline, final, diff)
	}
}
