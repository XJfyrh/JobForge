package micro

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/domain"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

type benchmarkPollWaiter struct{}

func (benchmarkPollWaiter) WaitForNotification(context.Context) bool { return false }

const (
	gatewayPollDirtyInflightEnv = "JOBFORGE_BENCH_GATEWAY_DIRTY_INFLIGHT"
	gatewayPollDirtyOwners      = 8
)

var (
	gatewayPollDirtyOnce sync.Once
	gatewayPollDirtyErr  error
)

const insertGatewayPollDirtyInflight = `
insert into jobs (
    id, tenant_id, queue, type, state, run_at, attempt,
    lease_owner, lease_until, fencing_token
)
select gen_random_uuid(),
       'bench-gateway-dirty-tenant-' || (fixture.n % $2)::text,
       'bench-gateway-dirty-fixture',
       'demo.echo',
       case when fixture.n % 2 = 0 then 'running' else 'cancelling' end,
       now() - interval '1 hour',
       1,
       'bench-gateway-dirty-owner-' || (fixture.n % $2)::text,
       now() + interval '5 minutes',
       1
from generate_series(1, $1) as fixture(n)`

// seedGatewayPollDirtyInflight adds unrelated owner inflight rows before the
// timer starts. The official dirty-database evidence uses 20,000 rows across
// eight owners, matching the v0.5 scale caliber. A process-level once keeps Go
// benchmark calibration from multiplying the fixture between N adjustments.
func seedGatewayPollDirtyInflight(ctx context.Context, b *testing.B) int {
	b.Helper()
	raw := os.Getenv(gatewayPollDirtyInflightEnv)
	if raw == "" || raw == "0" {
		return 0
	}

	rows, err := strconv.Atoi(raw)
	if err != nil || rows < 0 {
		b.Fatalf("%s must be a non-negative integer, got %q", gatewayPollDirtyInflightEnv, raw)
	}

	gatewayPollDirtyOnce.Do(func() {
		if _, err := benchPool.Exec(
			ctx, insertGatewayPollDirtyInflight, rows, gatewayPollDirtyOwners,
		); err != nil {
			gatewayPollDirtyErr = err
			return
		}
		_, gatewayPollDirtyErr = benchPool.Exec(ctx, "analyze jobs")
	})
	if gatewayPollDirtyErr != nil {
		b.Fatalf("seed Gateway Poll dirty inflight fixture: %v", gatewayPollDirtyErr)
	}
	return rows
}

// BenchmarkGatewayPollClaim measures the full Gateway Poll path, including
// worker liveness and request validation, rather than calling JobStore.Claim
// directly. v0.5 extends this path with atomic capability and capacity checks.
func BenchmarkGatewayPollClaim(b *testing.B) {
	ctx := context.Background()
	jobStore := postgres.NewJobStore(benchPool)
	dirtyInflight := seedGatewayPollDirtyInflight(ctx, b)
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
	if dirtyInflight > 0 {
		b.Logf("Gateway Poll dirty fixture: inflight=%d owners=%d",
			dirtyInflight, gatewayPollDirtyOwners)
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
