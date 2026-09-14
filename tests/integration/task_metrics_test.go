package integration

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/xjfyrh/jobforge/internal/domain"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

func sumMetric(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, point := range family.Metric {
			actual := map[string]string{}
			for _, label := range point.Label {
				actual[label.GetName()] = label.GetValue()
			}
			matches := true
			for key, value := range labels {
				if actual[key] != value {
					matches = false
				}
			}
			if !matches {
				continue
			}
			if point.Counter != nil {
				sum += point.Counter.GetValue()
			}
			if point.Gauge != nil {
				sum += point.Gauge.GetValue()
			}
			if point.Histogram != nil {
				sum += float64(point.Histogram.GetSampleCount())
			}
		}
	}
	return sum
}

func TestAT40FinishedAndRecoveredMetrics(t *testing.T) {
	ctx := t.Context()
	reg := prometheus.NewRegistry()
	metrics, shutdown, err := observability.SetupMetrics(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	svc := gatewaygrpc.NewWorkerService(js, stubPollWaiter{}, testTaskTypeCatalog(t), time.Minute, time.Second, 0, true, slog.New(slog.NewTextHandler(io.Discard, nil)), metrics)
	queue := "metric-" + uuid.NewString()
	owner := "metric-worker"
	claim := func(taskType string, maxAttempts int) *domain.Job {
		t.Helper()
		past := time.Now().Add(-24 * time.Hour)
		job, err := domain.NewJob(uuid.NewString(), domain.NewJobParams{TenantID: "metric-tenant", Queue: queue, Type: taskType, Payload: []byte(`{}`), MaxAttempts: maxAttempts, RunAt: &past}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = js.Enqueue(ctx, job); err != nil {
			t.Fatal(err)
		}
		result, err := js.Claim(ctx, store.ClaimParams{Queues: []string{queue}, Types: []string{taskType}, WorkerID: owner, MaxJobs: 1, LeaseTTL: time.Minute})
		if err != nil || len(result.Jobs) != 1 {
			t.Fatalf("claim: %v", err)
		}
		return result.Jobs[0]
	}
	job := claim("rag.index", 2)
	complete := &workerv1.CompleteRequest{JobId: job.ID, WorkerId: owner, FencingToken: job.FencingToken, ResultRef: "artifact:metric", Duration: durationpb.New(125 * time.Millisecond)}
	complete.Duration = durationpb.New(-time.Nanosecond)
	if _, err = svc.Complete(ctx, complete); err == nil {
		t.Fatal("negative sub-millisecond duration accepted")
	}
	complete.Duration = durationpb.New(125 * time.Millisecond)
	for range 2 {
		if _, err = svc.Complete(ctx, complete); err != nil {
			t.Fatal(err)
		}
	}
	complete.FencingToken--
	if _, err = svc.Complete(ctx, complete); err == nil {
		t.Fatal("stale completion accepted")
	}

	job = claim("agent.extract", 2)
	if err = js.Cancel(ctx, "metric-tenant", job.ID); err != nil {
		t.Fatal(err)
	}
	beforeCancel, err := js.GetByID(ctx, "metric-tenant", job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = js.Cancel(ctx, "metric-tenant", job.ID); err != nil {
		t.Fatal(err)
	}
	afterCancel, err := js.GetByID(ctx, "metric-tenant", job.ID)
	if err != nil || beforeCancel.StateVersion != afterCancel.StateVersion || !beforeCancel.CancelRequestedAt.Equal(*afterCancel.CancelRequestedAt) {
		t.Fatal("duplicate cancel mutated state")
	}
	if _, err = svc.Complete(ctx, &workerv1.CompleteRequest{JobId: job.ID, WorkerId: owner, FencingToken: job.FencingToken, ResultRef: "artifact:cancelled"}); err == nil {
		t.Fatal("cancelled Complete accepted")
	}
	fail := &workerv1.FailRequest{JobId: job.ID, WorkerId: owner, FencingToken: job.FencingToken, ErrorCode: "CANCELLED", Retryable: true, Duration: durationpb.New(time.Millisecond)}
	for range 2 {
		if _, err = svc.Fail(ctx, fail); err != nil {
			t.Fatal(err)
		}
	}
	job = claim("rag.index", 2)
	fail = &workerv1.FailRequest{JobId: job.ID, WorkerId: owner, FencingToken: job.FencingToken, ErrorCode: "secret-user-controlled-label", Retryable: true, Duration: durationpb.New(time.Millisecond)}
	for range 2 {
		if _, err = svc.Fail(ctx, fail); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = testEnv.pool.Exec(ctx, `update jobs set run_at=now()-interval '1 day' where id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = ss.PromoteReady(ctx, 100); err != nil {
		t.Fatal(err)
	}
	result, err := js.Claim(ctx, store.ClaimParams{Queues: []string{queue}, WorkerID: owner, MaxJobs: 1, LeaseTTL: time.Minute})
	if err != nil || len(result.Jobs) != 1 {
		t.Fatalf("retry claim: %v", err)
	}
	if _, err = svc.Fail(ctx, fail); err == nil {
		t.Fatal("old retry token accepted")
	}
	fail.FencingToken = result.Jobs[0].FencingToken
	for range 2 {
		if _, err = svc.Fail(ctx, fail); err != nil {
			t.Fatal(err)
		}
	}

	for _, taskType := range []string{"rag.index", "agent.extract"} {
		job = claim(taskType, 2)
		if taskType == "agent.extract" {
			if err = js.Cancel(ctx, "metric-tenant", job.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = testEnv.pool.Exec(ctx, `update jobs set lease_until=now()-interval '1 day' where id=$1`, job.ID); err != nil {
			t.Fatal(err)
		}
		rows, recoveryErr := ss.RecoverExpiredAttempts(ctx)
		if recoveryErr != nil {
			t.Fatal(recoveryErr)
		}
		for _, row := range rows {
			metrics.RecordRecovery(ctx, row)
		}
	}
	rows, err := ss.RecoverExpiredAttempts(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("duplicate recovery: %v %d", err, len(rows))
	}
	for _, check := range []struct {
		name   string
		labels map[string]string
		want   float64
	}{
		{"jobforge_job_attempts_total", map[string]string{"queue": queue, "outcome": "succeeded"}, 1},
		{"jobforge_job_attempts_total", map[string]string{"queue": queue, "outcome": "cancelled"}, 1},
		{"jobforge_job_attempts_total", map[string]string{"queue": queue, "outcome": "failed_retry"}, 1},
		{"jobforge_job_attempts_total", map[string]string{"queue": queue, "outcome": "failed_dead"}, 1},
		{"jobforge_job_attempts_total", map[string]string{"queue": queue, "outcome": "lease_expired"}, 2},
		{"jobforge_retries_total", map[string]string{"queue": queue, "type": "rag.index", "error_code": "OTHER"}, 1},
		{"jobforge_dlq_total", map[string]string{"queue": queue}, 1},
		{"jobforge_lease_expired_total", map[string]string{"queue": queue}, 2},
		{"jobforge_job_latency_seconds", map[string]string{"queue": queue}, 4},
	} {
		if got := sumMetric(t, reg, check.name, check.labels); got != check.want {
			t.Errorf("%s %v = %v, want %v", check.name, check.labels, got, check.want)
		}
	}
	families, _ := reg.Gather()
	for _, family := range families {
		for _, point := range family.Metric {
			for _, label := range point.Label {
				if (label.GetName() == "queue" || label.GetName() == "type") && label.GetValue() == "" {
					t.Error("empty execution label")
				}
				if strings.Contains(label.GetValue(), "secret-") {
					t.Error("unbounded user error label")
				}
			}
		}
	}
}
