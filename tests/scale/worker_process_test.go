//go:build scale

package scale

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

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/worker"
	"github.com/xjfyrh/jobforge/internal/worker/demo"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

const (
	scaleWorkerHelperFlag      = "JOBFORGE_TEST_WORKER_HELPER"
	scaleWorkerHelperDSN       = "JOBFORGE_TEST_WORKER_DSN"
	scaleWorkerHelperGateway   = "JOBFORGE_TEST_WORKER_GATEWAY"
	scaleWorkerHelperQueue     = "JOBFORGE_TEST_WORKER_QUEUE"
	scaleWorkerHelperID        = "JOBFORGE_TEST_WORKER_ID"
	scaleWorkerHelperCapacity  = "JOBFORGE_TEST_WORKER_CAPACITY"
	scaleWorkerHelperHeartbeat = "JOBFORGE_TEST_WORKER_HEARTBEAT"
	scaleWorkerExitLimit       = 10 * time.Second
)

// TestScaleWorkerProcessHelper runs only in a re-executed scale test binary.
// Keeping it in _test.go avoids adding a crash surface to production code.
func TestScaleWorkerProcessHelper(t *testing.T) {
	if os.Getenv(scaleWorkerHelperFlag) != "1" {
		t.Skip("subprocess helper")
	}
	ctx := context.Background()
	poolConfig, err := pgxpool.ParseConfig(requiredScaleHelperEnv(t, scaleWorkerHelperDSN))
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

	capacity, err := strconv.Atoi(requiredScaleHelperEnv(t, scaleWorkerHelperCapacity))
	if err != nil || capacity <= 0 {
		t.Fatal("invalid helper capacity")
	}
	heartbeat, err := time.ParseDuration(requiredScaleHelperEnv(t, scaleWorkerHelperHeartbeat))
	if err != nil || heartbeat <= 0 {
		t.Fatal("invalid helper heartbeat")
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	registry := worker.NewRegistry()
	registry.Register("demo.idempotent_effect", demo.NewIdempotentEffectHandler(
		demo.NewPostgresEffectStore(pool), logger, nil,
	))
	runtime := worker.NewRuntime(worker.RuntimeConfig{
		WorkerID:          requiredScaleHelperEnv(t, scaleWorkerHelperID),
		InstanceID:        fmt.Sprintf("scale-helper-%d", os.Getpid()),
		Queues:            []string{requiredScaleHelperEnv(t, scaleWorkerHelperQueue)},
		Capacity:          capacity,
		GatewayAddr:       requiredScaleHelperEnv(t, scaleWorkerHelperGateway),
		HeartbeatInterval: heartbeat,
		PollTimeout:       2 * time.Second,
		ShutdownGrace:     time.Second,
		Version:           "v0.4-scale-helper",
	}, registry, logger, nil)
	if err := runtime.Run(ctx); err != nil {
		t.Fatalf("run scale helper Worker: %v", err)
	}
}

func requiredScaleHelperEnv(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Fatalf("missing helper environment %s", key)
	}
	return value
}

type scaleWorkerProcess struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}

	mu      sync.Mutex
	waitErr error
}

func startScaleWorkerProcess(
	t *testing.T,
	gatewayAddr, queue, workerID string,
	capacity int,
) *scaleWorkerProcess {
	t.Helper()
	process := &scaleWorkerProcess{done: make(chan struct{})}
	process.cmd = exec.Command(os.Args[0], "-test.run=^TestScaleWorkerProcessHelper$", "-test.v=false")
	process.cmd.Env = scaleHelperEnvironment(map[string]string{
		scaleWorkerHelperFlag:      "1",
		scaleWorkerHelperDSN:       testEnv.dsn,
		scaleWorkerHelperGateway:   gatewayAddr,
		scaleWorkerHelperQueue:     queue,
		scaleWorkerHelperID:        workerID,
		scaleWorkerHelperCapacity:  strconv.Itoa(capacity),
		scaleWorkerHelperHeartbeat: "200ms",
	})
	process.cmd.Stdout = &process.stdout
	process.cmd.Stderr = &process.stderr
	if err := process.cmd.Start(); err != nil {
		t.Fatalf("start scale Worker helper: %v", err)
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

func scaleHelperEnvironment(overrides map[string]string) []string {
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

func (p *scaleWorkerProcess) killAndWait(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		t.Fatalf("scale Worker exited before kill: %v\n%s", p.waitError(), p.output())
	default:
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill scale Worker: %v", err)
	}
	if !p.wait(scaleWorkerExitLimit) {
		t.Fatalf("scale Worker did not exit within %s", scaleWorkerExitLimit)
	}
	if p.cmd.ProcessState == nil || !p.cmd.ProcessState.Exited() {
		t.Fatal("scale Worker was killed but not reaped")
	}
}

func (p *scaleWorkerProcess) ensureStopped(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	if !p.wait(scaleWorkerExitLimit) {
		t.Errorf("cleanup could not reap scale Worker within %s", scaleWorkerExitLimit)
	}
}

func (p *scaleWorkerProcess) wait(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return true
	case <-timer.C:
		return false
	}
}

func (p *scaleWorkerProcess) waitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.waitErr
}

func (p *scaleWorkerProcess) output() string {
	return "stdout:\n" + p.stdout.String() + "\nstderr:\n" + p.stderr.String()
}

type scalePollWaiter struct{}

func (scalePollWaiter) WaitForNotification(ctx context.Context) bool {
	<-ctx.Done()
	return false
}

func startScaleWorkerGateway(t *testing.T, leaseTTL time.Duration) string {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := gatewaygrpc.NewWorkerService(
		setupStore(t), scalePollWaiter{}, leaseTTL, 200*time.Millisecond,
		0, true, logger, nil,
	)
	server := grpc.NewServer()
	workerv1.RegisterWorkerServiceServer(server, service)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen scale Worker gateway: %v", err)
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
		case <-time.After(scaleWorkerExitLimit):
			t.Errorf("scale Worker gateway did not stop")
		}
	})
	return lis.Addr().String()
}

func waitForScaleEffectsOwnedBy(
	t *testing.T,
	process *scaleWorkerProcess,
	queue, workerID string,
	want int,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-process.done:
			t.Fatalf("scale Worker exited before effect barrier: %v\n%s", process.waitError(), process.output())
		case <-deadline.C:
			t.Fatalf("scale effect barrier for queue %s did not reach %d within %s", queue, want, timeout)
		case <-ticker.C:
			var effects int
			err := testEnv.pool.QueryRow(context.Background(), `
				select count(*)
				from jobs job
				join demo_idempotent_effects effect on effect.job_id = job.id
				where job.queue = $1
				  and job.state = 'running'
				  and job.lease_owner = $2`, queue, workerID).Scan(&effects)
			if err == nil && effects == want {
				return
			}
		}
	}
}

func (p *scaleWorkerProcess) appliedOutcomes() int {
	return strings.Count(p.stdout.String(), `"effect_outcome":"applied"`)
}
