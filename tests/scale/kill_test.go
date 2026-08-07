//go:build scale

package scale

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

// recoveryUpperBound is the NFR-003 / NFR-204 recovery upper bound used by
// the suite: lease_ttl + scan_interval + 2s. The test claims with a 30s lease
// and recovery is invoked directly (scan_interval is effectively zero), so the
// measured per-round recovery duration must be far below this bound.
const recoveryUpperBound = 30*time.Second + 3*time.Second + 2*time.Second

// setupScaleSchedulerStore creates a SchedulerStore with a dedicated lock
// connection (mirrors tests/integration setupSchedulerStore).
func setupScaleSchedulerStore(t *testing.T) *postgres.SchedulerStore {
	t.Helper()
	lockConn, err := pgx.Connect(context.Background(), testEnv.dsn)
	if err != nil {
		t.Fatalf("connect lock conn: %v", err)
	}
	t.Cleanup(func() { _ = lockConn.Close(context.Background()) })
	return postgres.NewSchedulerStore(testEnv.pool, lockConn)
}

// enqueueReadyJobs enqueues n ready jobs into the given queue and returns
// their IDs. RunAt is set in the past to avoid Docker/WSL2 clock drift.
func enqueueReadyJobs(t *testing.T, js store.JobStore, queue string, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	pastRunAt := time.Now().Add(-1 * time.Second)
	for i := 0; i < n; i++ {
		id := uuid.New().String()
		job, err := domain.NewJob(id, domain.NewJobParams{
			TenantID: "scale-tenant",
			Queue:    queue,
			Type:     "demo.echo",
			Payload:  []byte(`{"scale":true}`),
			RunAt:    &pastRunAt,
		}, time.Now())
		if err != nil {
			t.Fatalf("new job: %v", err)
		}
		if _, err := js.Enqueue(ctx, job); err != nil {
			t.Fatalf("enqueue job %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// forceExpireRunningLeases moves lease_until into the past for all running
// jobs of a queue, using the PostgreSQL clock (now()) to avoid Docker/WSL2
// clock drift between the Go host and the container.
func forceExpireRunningLeases(t *testing.T, queue string) {
	t.Helper()
	_, err := testEnv.pool.Exec(context.Background(),
		"update jobs set lease_until = now() - interval '1 second' where queue = $1 and state = 'running'",
		queue)
	if err != nil {
		t.Fatalf("force expire leases: %v", err)
	}
}

// countNonSucceeded returns how many jobs of a queue are not in succeeded
// state. Used as the silent-loss counter for AT-13.
func countNonSucceeded(t *testing.T, queue string) int {
	t.Helper()
	var n int
	err := testEnv.pool.QueryRow(context.Background(),
		"select count(*) from jobs where queue = $1 and state <> 'succeeded'", queue).Scan(&n)
	if err != nil {
		t.Fatalf("count non-succeeded: %v", err)
	}
	return n
}

// recoverUntilDrained calls RecoverExpiredLeases until no expired leases
// remain and returns the total recovered count.
func recoverUntilDrained(t *testing.T, ss *postgres.SchedulerStore) int {
	t.Helper()
	ctx := context.Background()
	total := 0
	for i := 0; i < 100; i++ {
		n, err := ss.RecoverExpiredLeases(ctx)
		if err != nil {
			t.Fatalf("recover expired leases: %v", err)
		}
		total += n
		if n == 0 {
			return total
		}
	}
	t.Fatalf("recovery did not drain after 100 iterations (recovered=%d)", total)
	return total
}

// percentile returns the p-th percentile (0-100) of a sorted duration slice.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted)*p + 99) / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// TestScaleAT13WorkerKillRounds verifies AT-13 / FR-601 (NFR-001 at literal
// scale): across N rounds of Worker kill before ACK, no non-terminal job is
// ever silently lost, and per-round recovery time stays within the NFR-003
// upper bound (lease_ttl + scan_interval + 2s).
//
// Each round:
//  1. Enqueue jobsPerRound jobs into a round-scoped queue
//  2. Worker A claims all of them, then is "killed" (never sends Complete)
//  3. Leases are force-expired using the PostgreSQL clock
//  4. Scheduler recovery runs; duration is recorded
//  5. Worker B re-claims and completes every job
//  6. Loss check: every job must be succeeded (loss counter stays 0)
func TestScaleAT13WorkerKillRounds(t *testing.T) {
	js := setupStore(t)
	ss := setupScaleSchedulerStore(t)
	ctx := context.Background()

	rounds := params.killRounds
	jobsPerRound := params.killJobsPerRound
	t.Logf("AT-13: rounds=%d jobsPerRound=%d", rounds, jobsPerRound)

	recoveryTimes := make([]time.Duration, 0, rounds)
	totalLost := 0

	for r := 1; r <= rounds; r++ {
		queue := fmt.Sprintf("scale-at13-round-%03d", r)

		// 1. Enqueue the round's jobs.
		enqueueReadyJobs(t, js, queue, jobsPerRound)

		// 2. Worker A claims all jobs, then crashes before ACK.
		claimed, err := js.Claim(ctx, store.ClaimParams{
			Queues:   []string{queue},
			WorkerID: fmt.Sprintf("killer-A-r%d", r),
			MaxJobs:  jobsPerRound,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("round %d: worker A claim: %v", r, err)
		}
		if len(claimed) != jobsPerRound {
			t.Fatalf("round %d: worker A claimed %d, want %d", r, len(claimed), jobsPerRound)
		}
		// Worker A is killed here: no Complete RPC is ever sent.

		// 3. Force lease expiry (PostgreSQL clock anchored).
		forceExpireRunningLeases(t, queue)

		// 4. Scheduler recovery; measure duration.
		start := time.Now()
		recovered := recoverUntilDrained(t, ss)
		recoveryTimes = append(recoveryTimes, time.Since(start))
		if recovered < jobsPerRound {
			t.Fatalf("round %d: recovered %d, want >= %d", r, recovered, jobsPerRound)
		}

		// 5. Worker B re-claims and completes every job.
		reclaimed, err := js.Claim(ctx, store.ClaimParams{
			Queues:   []string{queue},
			WorkerID: fmt.Sprintf("recovery-B-r%d", r),
			MaxJobs:  jobsPerRound,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil {
			t.Fatalf("round %d: worker B claim: %v", r, err)
		}
		if len(reclaimed) != jobsPerRound {
			t.Fatalf("round %d: worker B reclaimed %d, want %d", r, len(reclaimed), jobsPerRound)
		}
		workerB := fmt.Sprintf("recovery-B-r%d", r)
		for _, job := range reclaimed {
			if err := js.Complete(ctx, job.ID, workerB, job.FencingToken, "scale-result", 10); err != nil {
				t.Fatalf("round %d: complete %s: %v", r, job.ID, err)
			}
		}

		// 6. Silent-loss check: all jobs must have reached succeeded.
		if lost := countNonSucceeded(t, queue); lost > 0 {
			totalLost += lost
			t.Errorf("round %d: %d jobs silently lost (not succeeded)", r, lost)
		}

		if r%25 == 0 {
			t.Logf("AT-13 progress: %d/%d rounds done, totalLost=%d", r, rounds, totalLost)
		}
	}

	sort.Slice(recoveryTimes, func(i, j int) bool { return recoveryTimes[i] < recoveryTimes[j] })
	minT, maxT := recoveryTimes[0], recoveryTimes[len(recoveryTimes)-1]
	t.Logf("AT-13 recovery duration distribution over %d rounds: min=%s p50=%s p95=%s max=%s (bound=%s)",
		rounds, minT, percentile(recoveryTimes, 50), percentile(recoveryTimes, 95), maxT, recoveryUpperBound)

	if maxT > recoveryUpperBound {
		t.Errorf("AT-13 FAILED: max recovery duration %s exceeds NFR-003 bound %s", maxT, recoveryUpperBound)
	}
	if totalLost != 0 {
		t.Fatalf("AT-13 FAILED: total silently lost jobs = %d, want 0", totalLost)
	}
	t.Logf("AT-13 PASSED: %d rounds x %d jobs, zero silent loss, all recoveries within NFR-003 bound",
		rounds, jobsPerRound)
}
