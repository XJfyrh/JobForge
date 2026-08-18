package micro

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/domain"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

type benchmarkPollWaiter struct{}

func (benchmarkPollWaiter) WaitForNotification(context.Context) bool { return false }

// BenchmarkGatewayPollClaim measures the full Gateway Poll path, including
// worker liveness and request validation, rather than calling JobStore.Claim
// directly. v0.5 extends this path with atomic capability and capacity checks.
func BenchmarkGatewayPollClaim(b *testing.B) {
	ctx := context.Background()
	jobStore := postgres.NewJobStore(benchPool)
	// Each count iteration gets isolated registration/inflight state now that
	// server-side capacity is enforced across the entire workers row.
	queue := "bench-gateway-poll-" + domain.NewID()
	workerID := "bench-gateway-worker-" + domain.NewID()
	seedJobs(ctx, b, jobStore, queue, b.N+100)
	catalog, err := domain.NewTaskTypeCatalog(domain.DefaultTaskTypeNames())
	if err != nil {
		b.Fatalf("create task type catalog: %v", err)
	}

	service := gatewaygrpc.NewWorkerService(
		jobStore,
		benchmarkPollWaiter{},
		catalog,
		30*time.Second,
		5*time.Second,
		0,
		true,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	if _, err := service.Register(ctx, &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "benchmark",
		Queues:         []string{queue},
		SupportedTypes: []string{"demo.echo"},
		Capacity:       int32(b.N + 100),
		Version:        "benchmark",
	}); err != nil {
		b.Fatalf("register worker: %v", err)
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		response, err := service.Poll(ctx, &workerv1.PollRequest{
			WorkerId:          workerID,
			MaxJobs:           1,
			AvailableCapacity: int32(b.N + 100 - i),
			Queues:            []string{queue},
			Types:             []string{"demo.echo"},
		})
		if err != nil {
			b.Fatalf("poll: %v", err)
		}
		if len(response.Jobs) != 1 {
			b.Fatalf("poll returned %d jobs, want 1", len(response.Jobs))
		}
	}
}
