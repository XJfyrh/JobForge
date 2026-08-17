package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/xjfyrh/jobforge/internal/domain"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	"github.com/xjfyrh/jobforge/internal/worker"
	"github.com/xjfyrh/jobforge/internal/worker/demo"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

const (
	workerHelperFlag       = "JOBFORGE_TEST_WORKER_HELPER"
	workerHelperDSN        = "JOBFORGE_TEST_WORKER_DSN"
	workerHelperGateway    = "JOBFORGE_TEST_WORKER_GATEWAY"
	workerHelperQueue      = "JOBFORGE_TEST_WORKER_QUEUE"
	workerHelperID         = "JOBFORGE_TEST_WORKER_ID"
	workerHelperCapacity   = "JOBFORGE_TEST_WORKER_CAPACITY"
	workerHelperHeartbeat  = "JOBFORGE_TEST_WORKER_HEARTBEAT"
	workerProcessExitLimit = 10 * time.Second
)

// TestWorkerProcessHelper is selected only by a parent test re-executing the
// current test binary. It is deliberately test-only: production binaries gain
// no crash flag or remote kill surface.
func TestWorkerProcessHelper(t *testing.T) {
	if os.Getenv(workerHelperFlag) != "1" {
		t.Skip("subprocess helper")
	}

	ctx := context.Background()
	poolConfig, err := pgxpool.ParseConfig(requiredHelperEnv(t, workerHelperDSN))
	if err != nil {
		t.Fatal("parse helper database config: invalid value")
	}
	poolConfig.MaxConns = 2
	poolConfig.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("create helper effect pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping helper effect database: %v", err)
	}

	capacity, err := strconv.Atoi(requiredHelperEnv(t, workerHelperCapacity))
	if err != nil || capacity <= 0 {
		t.Fatalf("invalid helper capacity")
	}
	heartbeat, err := time.ParseDuration(requiredHelperEnv(t, workerHelperHeartbeat))
	if err != nil || heartbeat <= 0 {
		t.Fatalf("invalid helper heartbeat")
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	registry := worker.NewRegistry()
	registry.Register("demo.idempotent_effect", demo.NewIdempotentEffectHandler(
		demo.NewPostgresEffectStore(pool), logger, nil,
	))
	runtime := worker.NewRuntime(worker.RuntimeConfig{
		WorkerID:          requiredHelperEnv(t, workerHelperID),
		InstanceID:        fmt.Sprintf("test-helper-%d", os.Getpid()),
		Queues:            []string{requiredHelperEnv(t, workerHelperQueue)},
		Capacity:          capacity,
		GatewayAddr:       requiredHelperEnv(t, workerHelperGateway),
		HeartbeatInterval: heartbeat,
		PollTimeout:       2 * time.Second,
		ShutdownGrace:     time.Second,
		Version:           "v0.4-test-helper",
	}, registry, logger, nil)
	if err := runtime.Run(ctx); err != nil {
		t.Fatalf("run helper Worker: %v", err)
	}
}

func requiredHelperEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("missing helper environment %s", key)
	}
	return value
}

type testWorkerProcess struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}

	mu      sync.Mutex
	waitErr error
}

func startTestWorkerProcess(
	t *testing.T,
	gatewayAddr, queue, workerID string,
	capacity int,
) *testWorkerProcess {
	t.Helper()
	process := &testWorkerProcess{done: make(chan struct{})}
	process.cmd = exec.Command(os.Args[0], "-test.run=^TestWorkerProcessHelper$", "-test.v=false")
	process.cmd.Env = helperEnvironment(map[string]string{
		workerHelperFlag:      "1",
		workerHelperDSN:       testEnv.dsn,
		workerHelperGateway:   gatewayAddr,
		workerHelperQueue:     queue,
		workerHelperID:        workerID,
		workerHelperCapacity:  strconv.Itoa(capacity),
		workerHelperHeartbeat: "200ms",
	})
	process.cmd.Stdout = &process.stdout
	process.cmd.Stderr = &process.stderr
	if err := process.cmd.Start(); err != nil {
		t.Fatalf("start Worker helper process: %v", err)
	}
	go func() {
		err := process.cmd.Wait()
		process.mu.Lock()
		process.waitErr = err
		process.mu.Unlock()
		close(process.done)
	}()
	t.Cleanup(func() { process.ensureStopped(t) })
	return process
}

func helperEnvironment(overrides map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, overridden := overrides[strings.ToUpper(key)]; overridden {
				continue
			}
		}
		env = append(env, entry)
	}
	for key, value := range overrides {
		env = append(env, key+"="+value)
	}
	return env
}

func (p *testWorkerProcess) killAndWait(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("Worker helper exited before kill: %v\n%s", p.waitError(), p.output())
	default:
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill Worker helper: %v", err)
	}
	if !p.wait(workerProcessExitLimit) {
		t.Fatalf("Worker helper did not exit within %s", workerProcessExitLimit)
	}
	// The done channel closes only after Cmd.Wait returns, which is the reaping
	// barrier. On Unix, ProcessState.Exited reports false for a process that was
	// terminated by a signal even though Wait has successfully reaped it.
	if p.cmd.ProcessState == nil {
		t.Fatal("Worker helper Wait returned without process state")
	}
}

func (p *testWorkerProcess) ensureStopped(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	if !p.wait(workerProcessExitLimit) {
		t.Errorf("cleanup could not reap Worker helper within %s", workerProcessExitLimit)
	}
}

func (p *testWorkerProcess) wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

func (p *testWorkerProcess) waitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *testWorkerProcess) output() string {
	return "stdout:\n" + p.stdout.String() + "\nstderr:\n" + p.stderr.String()
}

func startTestWorkerGateway(t *testing.T, leaseTTL time.Duration) string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := gatewaygrpc.NewWorkerService(
		setupStore(t), stubPollWaiter{}, leaseTTL, 200*time.Millisecond,
		0, true, logger, nil,
	)
	server := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(server, service)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen Worker test gateway: %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		_ = server.Serve(lis)
		close(serveDone)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = lis.Close()
		select {
		case <-serveDone:
		case <-time.After(workerProcessExitLimit):
			t.Errorf("Worker test gateway did not stop")
		}
	})
	return lis.Addr().String()
}

func createPersistentEffectJob(
	t *testing.T,
	jobStore store.JobStore,
	queue string,
	postEffectDelayMS int,
) *domain.Job {
	t.Helper()
	jobID := uuid.NewString()
	runAt := time.Now().Add(-time.Second)
	payload := []byte(fmt.Sprintf(`{"post_effect_delay_ms":%d}`, postEffectDelayMS))
	job, err := domain.NewJob(jobID, domain.NewJobParams{
		TenantID: "test-tenant",
		Queue:    queue,
		Type:     "demo.idempotent_effect",
		Payload:  payload,
		RunAt:    &runAt,
	}, time.Now())
	if err != nil {
		t.Fatalf("create persistent effect job: %v", err)
	}
	if _, err := jobStore.Enqueue(context.Background(), job); err != nil {
		t.Fatalf("enqueue persistent effect job: %v", err)
	}
	reanchorRunAt(t, job.ID)
	return job
}

func waitForEffectOwnedBy(
	t *testing.T,
	process *testWorkerProcess,
	jobID, workerID string,
	timeout time.Duration,
) *domain.Job {
	t.Helper()
	jobStore := postgres.NewJobStore(testEnv.pool)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-process.done:
			t.Fatalf("Worker helper exited before effect barrier: %v\n%s", process.waitError(), process.output())
		case <-deadline.C:
			t.Fatalf("effect barrier not reached for job %s within %s", jobID, timeout)
		case <-ticker.C:
			job, err := jobStore.GetByID(context.Background(), "test-tenant", jobID)
			if err != nil || job.State != domain.StateRunning || job.LeaseOwner == nil || *job.LeaseOwner != workerID {
				continue
			}
			var resultRef string
			err = testEnv.pool.QueryRow(context.Background(),
				"select result_ref from demo_idempotent_effects where job_id = $1",
				jobID,
			).Scan(&resultRef)
			if err == nil && resultRef == "effect:"+jobID {
				return job
			}
		}
	}
}

func waitForSucceededByProcess(
	t *testing.T,
	process *testWorkerProcess,
	jobID string,
	timeout time.Duration,
) *domain.Job {
	t.Helper()
	jobStore := postgres.NewJobStore(testEnv.pool)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-process.done:
			t.Fatalf("recovery Worker exited before success: %v\n%s", process.waitError(), process.output())
		case <-deadline.C:
			t.Fatalf("job %s did not succeed within %s", jobID, timeout)
		case <-ticker.C:
			job, err := jobStore.GetByID(context.Background(), "test-tenant", jobID)
			if err == nil && job.State == domain.StateSucceeded {
				return job
			}
		}
	}
}

func forceExpireJobAndRecover(t *testing.T, schedulerStore *postgres.SchedulerStore, jobID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := testEnv.pool.Exec(ctx,
		"update jobs set lease_until = now() - interval '1 second' where id = $1 and state = 'running'",
		jobID,
	); err != nil {
		t.Fatalf("force expire crashed Worker lease: %v", err)
	}
	if _, err := schedulerStore.RecoverExpiredLeases(ctx); err != nil {
		t.Fatalf("recover crashed Worker lease: %v", err)
	}
}

func countEffectOutcomes(processes ...*testWorkerProcess) (applied, deduplicated int) {
	for _, process := range processes {
		output := process.stdout.String()
		applied += strings.Count(output, `"effect_outcome":"applied"`)
		deduplicated += strings.Count(output, `"effect_outcome":"deduplicated"`)
	}
	return applied, deduplicated
}
