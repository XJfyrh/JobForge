package micro

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

// seedJobs creates n ready jobs for claim benchmarks.
func seedJobs(ctx context.Context, b *testing.B, js *postgres.JobStore, queue string, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		id := uuid.New().String()
		pastRunAt := time.Now().Add(-1 * time.Second)
		job, err := domain.NewJob(id, domain.NewJobParams{
			TenantID: "bench-tenant",
			Queue:    queue,
			Type:     "demo.echo",
			Payload:  []byte(`{"benchmark":true}`),
			RunAt:    &pastRunAt,
		}, time.Now())
		if err != nil {
			b.Fatalf("create job: %v", err)
		}
		_, err = js.Enqueue(ctx, job)
		if err != nil {
			b.Fatalf("enqueue: %v", err)
		}
	}
}

// BenchmarkClaim measures single-worker claim throughput.
func BenchmarkClaim(b *testing.B) {
	ctx := context.Background()
	js := postgres.NewJobStore(benchPool)
	queue := "bench-claim-single"

	// Pre-seed jobs.
	seedJobs(ctx, b, js, queue, b.N+100)

	b.ResetTimer()
	b.ReportAllocs()

	for i := 0; i < b.N; i++ {
		_, err := js.Claim(ctx, store.ClaimParams{
			Queue:    queue,
			WorkerID: "bench-worker-single",
			MaxJobs:  1,
			LeaseTTL: 30 * time.Second,
		})
		if err != nil {
			b.Fatalf("claim: %v", err)
		}
	}
}

// BenchmarkClaimBatch measures batch claim throughput (multiple jobs per claim).
func BenchmarkClaimBatch(b *testing.B) {
	ctx := context.Background()
	js := postgres.NewJobStore(benchPool)

	batchSizes := []int{1, 5, 10, 20}

	for _, batchSize := range batchSizes {
		b.Run(fmt.Sprintf("batch%d", batchSize), func(b *testing.B) {
			queue := fmt.Sprintf("bench-claim-batch%d", batchSize)

			// Pre-seed jobs.
			seedJobs(ctx, b, js, queue, b.N*batchSize+100)

			b.ResetTimer()
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_, err := js.Claim(ctx, store.ClaimParams{
					Queue:    queue,
					WorkerID: "bench-worker-batch",
					MaxJobs:  batchSize,
					LeaseTTL: 30 * time.Second,
				})
				if err != nil {
					b.Fatalf("claim: %v", err)
				}
			}
		})
	}
}

// BenchmarkClaimParallel measures concurrent claim throughput from multiple workers.
func BenchmarkClaimParallel(b *testing.B) {
	ctx := context.Background()
	js := postgres.NewJobStore(benchPool)
	queue := "bench-claim-parallel"

	// Pre-seed enough jobs for parallel claims.
	seedJobs(ctx, b, js, queue, b.N*4+1000)

	b.ResetTimer()
	b.ReportAllocs()

	b.RunParallel(func(pb *testing.PB) {
		workerID := fmt.Sprintf("bench-worker-%s", uuid.New().String()[:8])
		for pb.Next() {
			_, err := js.Claim(ctx, store.ClaimParams{
				Queue:    queue,
				WorkerID: workerID,
				MaxJobs:  1,
				LeaseTTL: 30 * time.Second,
			})
			if err != nil {
				b.Fatalf("claim: %v", err)
			}
		}
	})
}

// BenchmarkClaimContention measures claim performance under high contention
// (many workers competing for few jobs).
func BenchmarkClaimContention(b *testing.B) {
	ctx := context.Background()
	js := postgres.NewJobStore(benchPool)
	queue := "bench-claim-contention"

	// Seed limited jobs to create contention.
	seedJobs(ctx, b, js, queue, b.N+100)

	b.ResetTimer()
	b.ReportAllocs()

	// Run with high parallelism to simulate contention.
	b.SetParallelism(16)
	b.RunParallel(func(pb *testing.PB) {
		workerID := fmt.Sprintf("bench-worker-%s", uuid.New().String()[:8])
		for pb.Next() {
			jobs, err := js.Claim(ctx, store.ClaimParams{
				Queue:    queue,
				WorkerID: workerID,
				MaxJobs:  1,
				LeaseTTL: 30 * time.Second,
			})
			if err != nil {
				b.Fatalf("claim: %v", err)
			}
			// Empty result is expected under contention.
			_ = jobs
		}
	})
}
