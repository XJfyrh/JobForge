//go:build scale

package scale

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

// TestScaleNFR306HeartbeatWriteAmplification quantifies the expected write
// multiplication over a logical 30s lease window: 10s cadence performs three
// lease renewals per active job, while the M4 5s cadence performs six. Every
// heartbeat remains a real PostgreSQL UPDATE; worker liveness throttling is
// deliberately not allowed to coalesce job lease renewals. Latency is reported
// for capacity evidence only and has no machine-specific hard threshold.
func TestScaleNFR306HeartbeatWriteAmplification(t *testing.T) {
	const jobsPerScenario = 100
	jobStore := setupStore(t)
	testID := uuid.New().String()[:8]
	tenSecondQueue := "nfr306-10s-" + testID
	fiveSecondQueue := "nfr306-5s-" + testID
	t.Cleanup(func() {
		_, _ = testEnv.pool.Exec(context.Background(),
			`delete from jobs where queue = any($1::text[])`, []string{tenSecondQueue, fiveSecondQueue})
	})

	tenSecondJobs := enqueueAndClaimHeartbeatJobs(t, jobStore, tenSecondQueue, "nfr306-worker-10s-"+testID, jobsPerScenario)
	fiveSecondJobs := enqueueAndClaimHeartbeatJobs(t, jobStore, fiveSecondQueue, "nfr306-worker-5s-"+testID, jobsPerScenario)

	tenSecondLatencies := measureHeartbeatWrites(t, jobStore, tenSecondJobs, "nfr306-worker-10s-"+testID, 3)
	fiveSecondLatencies := measureHeartbeatWrites(t, jobStore, fiveSecondJobs, "nfr306-worker-5s-"+testID, 6)
	if got, want := len(fiveSecondLatencies), 2*len(tenSecondLatencies); got != want {
		t.Fatalf("5s heartbeat writes = %d, want exactly 2x the 10s baseline (%d)", got, want)
	}

	tenP50, tenP95 := latencyPercentiles(tenSecondLatencies)
	fiveP50, fiveP95 := latencyPercentiles(fiveSecondLatencies)
	t.Logf("NFR-306 logical_window=30s active_jobs=%d cadence=10s writes=%d p50=%s p95=%s",
		jobsPerScenario, len(tenSecondLatencies), tenP50, tenP95)
	t.Logf("NFR-306 logical_window=30s active_jobs=%d cadence=5s writes=%d amplification=2.00x p50=%s p95=%s",
		jobsPerScenario, len(fiveSecondLatencies), fiveP50, fiveP95)
}

func enqueueAndClaimHeartbeatJobs(t *testing.T, jobStore *postgres.JobStore, queue, workerID string, count int) []*domain.Job {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < count; i++ {
		job, err := domain.NewJob(uuid.New().String(), domain.NewJobParams{
			TenantID: "nfr306-tenant",
			Queue:    queue,
			Type:     "nfr306.heartbeat",
			Payload:  []byte(`{"test":true}`),
		}, time.Now())
		if err != nil {
			t.Fatalf("new heartbeat job: %v", err)
		}
		if _, err := jobStore.Enqueue(ctx, job); err != nil {
			t.Fatalf("enqueue heartbeat job: %v", err)
		}
	}
	if _, err := testEnv.pool.Exec(ctx,
		`update jobs set run_at = clock_timestamp() - interval '1 second' where queue = $1`, queue); err != nil {
		t.Fatalf("anchor heartbeat jobs: %v", err)
	}
	claimed, err := claimJobs(ctx, jobStore, store.ClaimParams{
		Queues:   []string{queue},
		WorkerID: workerID,
		MaxJobs:  count,
		LeaseTTL: domain.DefaultLeaseTTL,
	})
	if err != nil || len(claimed) != count {
		t.Fatalf("claim heartbeat jobs: jobs=%d want=%d err=%v", len(claimed), count, err)
	}
	return claimed
}

func measureHeartbeatWrites(t *testing.T, jobStore *postgres.JobStore, jobs []*domain.Job, workerID string, rounds int) []time.Duration {
	t.Helper()
	ctx := context.Background()
	latencies := make([]time.Duration, 0, len(jobs)*rounds)
	for round := 0; round < rounds; round++ {
		for _, job := range jobs {
			started := time.Now()
			if _, err := jobStore.Heartbeat(ctx, job.ID, workerID, job.FencingToken, domain.DefaultLeaseTTL); err != nil {
				t.Fatalf("heartbeat round %d job %s: %v", round+1, job.ID, err)
			}
			latencies = append(latencies, time.Since(started))
		}
	}
	return latencies
}
