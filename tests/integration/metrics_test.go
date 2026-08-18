package integration

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/scheduler"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// createScheduledJobForTenant inserts a job with state='scheduled' directly
// for the given tenant (bypasses domain.NewJob, which would mark past run_at
// jobs as ready).
func createScheduledJobForTenant(t *testing.T, tenant, queue, jobType string, runAt time.Time) string {
	t.Helper()
	ctx := context.Background()
	id := uuid.New().String()
	now := time.Now()

	_, err := testEnv.pool.Exec(ctx, `
		insert into jobs (id, tenant_id, queue, type, payload, priority, state,
			run_at, attempt, max_attempts, timeout_seconds, fencing_token,
			state_version, created_at, updated_at)
		values ($1, $2, $3, $4, '{"scheduled":true}', 0, 'scheduled',
			$5, 0, 3, 300, 0, 1, $6, $6)`,
		id, tenant, queue, jobType, runAt, now)
	if err != nil {
		t.Fatalf("insert scheduled job: %v", err)
	}
	return id
}

// TestQueueDepthMetricsSampling verifies the data source behind the
// jobforge_queue_depth gauge (PRD 12.1 / FR-502): pending jobs are sampled
// per (tenant, queue, state).
func TestQueueDepthMetricsSampling(t *testing.T) {
	js := setupStore(t)
	schedStore, _ := setupSchedulerStore(t)
	ctx := context.Background()

	tenant := "depth-tenant-" + uuid.New().String()[:8]
	queue := "depth-queue"

	// 2 ready jobs via normal enqueue.
	_ = createTestJobForTenant(t, js, tenant, queue, "demo.echo")
	_ = createTestJobForTenant(t, js, tenant, queue, "demo.echo")
	// 1 scheduled job with a future run_at (stays scheduled).
	_ = createScheduledJobForTenant(t, tenant, queue, "demo.echo", time.Now().Add(time.Hour))

	rows, err := schedStore.QueueDepthMetrics(ctx)
	if err != nil {
		t.Fatalf("queue depth metrics: %v", err)
	}

	var readyCount, scheduledCount int64
	for _, r := range rows {
		if r.TenantID != tenant || r.Queue != queue {
			continue
		}
		switch r.State {
		case "ready":
			readyCount += r.Count
		case "scheduled":
			scheduledCount += r.Count
		}
	}
	if readyCount != 2 {
		t.Errorf("ready depth = %d, want 2", readyCount)
	}
	if scheduledCount != 1 {
		t.Errorf("scheduled depth = %d, want 1", scheduledCount)
	}
}

// TestSchedulerEmitsQueueDepthGauge verifies that a Scheduler scan cycle
// actually records the jobforge_queue_depth gauge (PRD 12.1 / FR-502),
// closing the "instrument defined but never emitted" gap.
func TestSchedulerEmitsQueueDepthGauge(t *testing.T) {
	ctx := context.Background()

	reg := prometheus.NewRegistry()
	metrics, shutdown, err := observability.SetupMetrics(ctx, reg)
	if err != nil {
		t.Fatalf("setup metrics: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	tenant := "depth-gauge-" + uuid.New().String()[:8]
	queue := "depth-gauge-queue"
	js := setupStore(t)
	_ = createTestJobForTenant(t, js, tenant, queue, "demo.echo")

	schedStore, _ := setupSchedulerStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sched := scheduler.New(schedStore, nil, blockingWaiter{}, scheduler.Config{
		ScanInterval:      200 * time.Millisecond,
		PromoteBatchSize:  100,
		LockRetryInterval: 50 * time.Millisecond,
	}, logger, metrics)

	runCtx, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
	defer cancel()
	_ = sched.Run(runCtx)

	if n, err := testutil.GatherAndCount(reg, "jobforge_queue_depth"); err != nil || n == 0 {
		t.Fatalf("jobforge_queue_depth gauge was never emitted during scheduler scan (n=%d, err=%v)", n, err)
	}
}

// TestGatewayRegisterEmitsWorkersActive verifies that worker registration
// records the jobforge_workers_active gauge (PRD 12.1).
func TestGatewayRegisterEmitsWorkersActive(t *testing.T) {
	ctx := context.Background()

	reg := prometheus.NewRegistry()
	metrics, shutdown, err := observability.SetupMetrics(ctx, reg)
	if err != nil {
		t.Fatalf("setup metrics: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	js := setupStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := gatewaygrpc.NewWorkerService(js, blockingWaiter{}, testTaskTypeCatalog(t), 30*time.Second, 5*time.Second, 0, true, logger, metrics)

	workerID := "gauge-worker-" + uuid.New().String()[:8]
	_, err = svc.Register(ctx, &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "instance-gauge",
		SupportedTypes: []string{"demo.echo"},
		Queues:         []string{"default"},
		Capacity:       2,
		Version:        "9.9.9-test",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	if n, err := testutil.GatherAndCount(reg, "jobforge_workers_active"); err != nil || n == 0 {
		t.Fatalf("jobforge_workers_active gauge was never emitted on Register (n=%d, err=%v)", n, err)
	}

	// Sanity: the sampling source sees the registered worker.
	counts, err := js.WorkerCounts(ctx, time.Minute)
	if err != nil {
		t.Fatalf("worker counts: %v", err)
	}
	found := false
	for _, c := range counts {
		if c.Version == "9.9.9-test" && c.Count >= 1 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("WorkerCounts did not include the registered worker: %+v", counts)
	}
}

// TestWorkersActiveGaugeDecaysToZero verifies the gauge decays after a
// worker's heartbeat goes stale: the freshness filter drops the worker and
// the sampler explicitly records 0 for the vanished (version, status)
// series, so workers_active reaches zero without another Register.
func TestWorkersActiveGaugeDecaysToZero(t *testing.T) {
	ctx := context.Background()

	reg := prometheus.NewRegistry()
	metrics, shutdown, err := observability.SetupMetrics(ctx, reg)
	if err != nil {
		t.Fatalf("setup metrics: %v", err)
	}
	defer func() { _ = shutdown(ctx) }()

	js := setupStore(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := gatewaygrpc.NewWorkerService(js, blockingWaiter{}, testTaskTypeCatalog(t), 30*time.Second, 5*time.Second, 0, true, logger, metrics)

	version := "7.7.7-decay-" + uuid.New().String()[:8]
	workerID := "decay-worker-" + uuid.New().String()[:8]
	_, err = svc.Register(ctx, &workerv1.RegisterRequest{
		WorkerId:       workerID,
		InstanceId:     "instance-decay",
		SupportedTypes: []string{"demo.echo"},
		Queues:         []string{"default"},
		Capacity:       1,
		Version:        version,
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := workersActiveValue(t, reg, version); *got != 1 {
		t.Fatalf("expected gauge 1 after register, got %v", *got)
	}

	// Simulate a crash: age the heartbeat beyond the freshness window
	// (2x lease TTL). Uses the PostgreSQL clock to avoid host drift.
	if _, err := testEnv.pool.Exec(ctx,
		`update workers set last_heartbeat_at = now() - interval '5 minutes' where worker_id = $1`,
		workerID); err != nil {
		t.Fatalf("backdate heartbeat: %v", err)
	}

	// The periodic sampler must drive the series to zero.
	svc.SampleWorkerCounts(ctx)
	if got := workersActiveValue(t, reg, version); *got != 0 {
		t.Fatalf("expected gauge 0 after heartbeat went stale, got %v", *got)
	}
}

// workersActiveValue returns the jobforge_workers_active gauge value for the
// given version label, or fails the test if the series is missing.
func workersActiveValue(t *testing.T, reg *prometheus.Registry, version string) *float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != "jobforge_workers_active" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "version" && lp.GetValue() == version {
					return m.GetGauge().Value
				}
			}
		}
	}
	t.Fatalf("jobforge_workers_active series for version %s not found", version)
	return nil
}
