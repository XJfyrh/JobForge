package micro

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
)

// BenchmarkFinish isolates the transaction used by result reporting. Setup is
// excluded and both revisions use the same database, batch size and command.
func BenchmarkFinish(b *testing.B) {
	for _, operation := range []string{"complete", "fail"} {
		b.Run(operation, func(b *testing.B) {
			ctx := context.Background()
			js := postgres.NewJobStore(benchPool)
			queue := fmt.Sprintf("bench-finish-%s-%d", operation, time.Now().UnixNano())
			seedJobs(ctx, b, js, queue, b.N)
			if _, err := benchPool.Exec(ctx, "update jobs set run_at = now() - interval '1 second' where queue = $1", queue); err != nil {
				b.Fatal(err)
			}
			claimed, err := js.Claim(ctx, store.ClaimParams{
				Queues: []string{queue}, WorkerID: "finish-benchmark", MaxJobs: b.N,
				LeaseTTL: time.Hour,
			})
			if err != nil || len(claimed.Jobs) != b.N {
				b.Fatalf("prepare finish jobs: %v", err)
			}
			b.ResetTimer()
			b.ReportAllocs()
			for _, job := range claimed.Jobs {
				if operation == "complete" {
					err = js.Complete(ctx, job.ID, "finish-benchmark", job.FencingToken, "artifact:benchmark", 10)
				} else {
					err = js.Fail(ctx, job.ID, "finish-benchmark", job.FencingToken, "TEST", "synthetic", true, 10)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
