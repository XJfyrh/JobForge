package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"

	"github.com/xjfyrh/jobforge/internal/domain"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	"github.com/xjfyrh/jobforge/internal/worker"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// capacityCompletionStore observes committed outcomes without replacing the
// real claim, fencing, attempt, quota, or outbox transactions.
type capacityCompletionStore struct {
	*postgres.JobStore
	completed chan string
}

func (s *capacityCompletionStore) Complete(ctx context.Context, jobID, workerID string, token int64, result string, durationMS int64) error {
	err := s.JobStore.Complete(ctx, jobID, workerID, token, result, durationMS)
	if err == nil {
		s.completed <- jobID
	}
	return err
}

func (s *capacityCompletionStore) Fail(ctx context.Context, jobID, workerID string, token int64, code, message string, retryable bool, durationMS int64) error {
	err := s.JobStore.Fail(ctx, jobID, workerID, token, code, message, retryable, durationMS)
	if err == nil {
		s.completed <- jobID
	}
	return err
}

func newCapacityRuntime(tb testing.TB, capacity, jobs int, handler worker.Handler) (*worker.Runtime, <-chan string, string) {
	tb.Helper()
	ctx := tb.Context()
	queue := "runtime-capacity-" + uuid.NewString()
	jobStore := postgres.NewJobStore(testEnv.pool)
	for range jobs {
		runAt := time.Now().Add(-time.Hour)
		job, err := domain.NewJob(uuid.NewString(), domain.NewJobParams{
			TenantID: "test-tenant", Queue: queue, Type: "demo.echo",
			Payload: []byte(`{}`), RunAt: &runAt,
		}, time.Now())
		if err != nil {
			tb.Fatalf("create capacity job: %v", err)
		}
		if _, err := jobStore.Enqueue(ctx, job); err != nil {
			tb.Fatalf("enqueue capacity job: %v", err)
		}
	}

	observed := &capacityCompletionStore{JobStore: jobStore, completed: make(chan string, jobs)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	catalog, err := domain.NewTaskTypeCatalog([]string{"demo.echo"})
	if err != nil {
		tb.Fatal(err)
	}
	service := gatewaygrpc.NewWorkerService(observed, stubPollWaiter{}, catalog,
		30*time.Second, 5*time.Second, 0, true, logger, nil)
	server := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(server, service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen capacity gateway: %v", err)
	}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = server.Serve(listener)
	}()
	tb.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		<-serverDone
	})
	registry := worker.NewRegistry()
	registry.Register("demo.echo", handler)
	r := worker.NewRuntime(worker.RuntimeConfig{
		WorkerID: "capacity-worker-" + uuid.NewString(), InstanceID: "capacity-test",
		Queues: []string{queue}, Capacity: capacity, GatewayAddr: listener.Addr().String(),
		ShutdownGrace: 5 * time.Second,
	}, registry, logger, nil)
	return r, observed.completed, queue
}

func runCapacityRuntime(tb testing.TB, r *worker.Runtime) context.Context {
	tb.Helper()
	ctx, cancel := context.WithTimeout(tb.Context(), 3*time.Minute)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	tb.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				tb.Errorf("run capacity runtime: %v", err)
			}
		case <-time.After(10 * time.Second):
			tb.Error("capacity runtime did not stop")
		}
	})
	return ctx
}

func TestWorkerRuntimeRefillsAfterSuccessAndFailure(t *testing.T) {
	for _, capacity := range []int{1, 4} {
		t.Run(fmt.Sprintf("capacity%d", capacity), func(t *testing.T) {
			const jobs = 24
			started := make(chan struct{}, jobs)
			release := make(chan struct{})
			var active, peak, executions atomic.Int32
			handler := worker.HandlerFunc(func(ctx context.Context, _ *worker.ClaimedJob) (string, error) {
				n := active.Add(1)
				defer active.Add(-1)
				for previous := peak.Load(); n > previous; previous = peak.Load() {
					if peak.CompareAndSwap(previous, n) {
						break
					}
				}
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return "", ctx.Err()
				}
				if executions.Add(1)%3 == 0 {
					return "", errors.New("expected permanent test failure")
				}
				return "ok", nil
			})
			r, completed, queue := newCapacityRuntime(t, capacity, jobs, handler)
			ctx := runCapacityRuntime(t, r)
			deadline := time.NewTimer(20 * time.Second)
			defer deadline.Stop()
			// Hold the first batch until every slot is occupied. Both Complete
			// and Fail must subsequently release slots across many batches.
			for range capacity {
				select {
				case <-started:
				case <-deadline.C:
					t.Fatal("runtime did not fill initial capacity")
				}
			}
			close(release)
			seen := make(map[string]bool, jobs)
			for range jobs {
				select {
				case id := <-completed:
					if seen[id] {
						t.Fatalf("duplicate terminal transaction for %s", id)
					}
					seen[id] = true
				case <-deadline.C:
					t.Fatalf("capacity refill stalled after %d/%d jobs", len(seen), jobs)
				}
			}
			if peak.Load() != int32(capacity) || active.Load() != 0 {
				t.Fatalf("handler concurrency: peak=%d active=%d capacity=%d", peak.Load(), active.Load(), capacity)
			}
			var succeeded, dead, attempts int
			err := testEnv.pool.QueryRow(ctx, `select count(*) filter (where state = 'succeeded'),
				count(*) filter (where state = 'dead'), sum(attempt) from jobs where queue = $1`, queue).
				Scan(&succeeded, &dead, &attempts)
			if err != nil || succeeded != 16 || dead != 8 || attempts != jobs {
				t.Fatalf("outcomes: succeeded=%d dead=%d attempts=%d err=%v", succeeded, dead, attempts, err)
			}
			var finishedAttempts int
			err = testEnv.pool.QueryRow(ctx, `select count(*) from job_attempts a
				join jobs j on j.id = a.job_id where j.queue = $1 and a.finished_at is not null`, queue).
				Scan(&finishedAttempts)
			if err != nil || finishedAttempts != jobs {
				t.Fatalf("finished attempts=%d want=%d err=%v", finishedAttempts, jobs, err)
			}
		})
	}
}

func TestWorkerRuntimeStopsWhileCapacityIsFull(t *testing.T) {
	const capacity = 4
	started := make(chan struct{}, capacity)
	handler := worker.HandlerFunc(func(ctx context.Context, _ *worker.ClaimedJob) (string, error) {
		started <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	})
	r, _, queue := newCapacityRuntime(t, capacity, capacity+1, handler)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		runErr = r.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
			if runErr != nil {
				t.Errorf("shutdown runtime: %v", runErr)
			}
		case <-time.After(10 * time.Second):
			t.Error("runtime remained blocked waiting for capacity after cancellation")
		}
	})
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for range capacity {
		select {
		case <-started:
		case <-deadline.C:
			t.Fatal("runtime did not fill initial capacity")
		}
	}
	cancel()
	select {
	case <-done:
	case <-deadline.C:
		t.Fatal("runtime remained blocked waiting for capacity after cancellation")
	}
	// The unclaimed job must remain available for another Worker after
	// Run has joined the cancelled executions and returned.
	var ready, attempts int
	err := testEnv.pool.QueryRow(t.Context(), `select count(*) filter (where state = 'ready'),
		sum(attempt) from jobs where queue = $1`, queue).Scan(&ready, &attempts)
	if err != nil || ready != 1 || attempts != capacity {
		t.Fatalf("shutdown claims: ready=%d attempts=%d err=%v", ready, attempts, err)
	}
}

// BenchmarkWorkerRuntimeThroughput includes the real Runtime and loopback
// gRPC Gateway, unlike the existing store-level e2e benchmark. The 1ms handler
// models short work; fixture creation is excluded, registration is included.
func BenchmarkWorkerRuntimeThroughput(b *testing.B) {
	for _, capacity := range []int{1, 4} {
		b.Run(fmt.Sprintf("capacity%d", capacity), func(b *testing.B) {
			handler := worker.HandlerFunc(func(ctx context.Context, _ *worker.ClaimedJob) (string, error) {
				timer := time.NewTimer(time.Millisecond)
				defer timer.Stop()
				select {
				case <-timer.C:
					return "ok", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			})
			r, completed, queue := newCapacityRuntime(b, capacity, b.N, handler)
			b.ResetTimer()
			ctx := runCapacityRuntime(b, r)
			for range b.N {
				select {
				case <-completed:
				case <-ctx.Done():
					b.Fatal("runtime benchmark did not finish all jobs")
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "jobs/s")
			var succeeded, attempts int
			err := testEnv.pool.QueryRow(ctx, `select count(*) filter (where state = 'succeeded'),
				coalesce(sum(attempt), 0) from jobs where queue = $1`, queue).Scan(&succeeded, &attempts)
			if err != nil || succeeded != b.N || attempts != b.N {
				b.Fatalf("runtime outcomes: succeeded=%d attempts=%d want=%d err=%v", succeeded, attempts, b.N, err)
			}
		})
	}
}
