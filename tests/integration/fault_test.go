package integration

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/domain"
	gatewaygrpc "github.com/xjfyrh/jobforge/internal/gateway/grpc"
	"github.com/xjfyrh/jobforge/internal/store"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	"github.com/xjfyrh/jobforge/internal/worker"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// TestFaultAT02CrashBeforeACK verifies AT-02 with real Worker OS processes.
// Worker A commits the persistent business effect and enters the bounded
// post-effect delay. The parent observes that database barrier, kills and
// reaps A before Complete, then Worker B reclaims and deduplicates the effect.
func TestFaultAT02CrashBeforeACK(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()
	queue := "fault-at02-" + uuid.NewString()[:8]
	gatewayAddr := startTestWorkerGateway(t, 30*time.Second)
	job := createPersistentEffectJob(t, js, queue, 60_000)

	workerAID := "worker-A-crash-" + uuid.NewString()[:8]
	workerA := startTestWorkerProcess(t, gatewayAddr, queue, workerAID, 1)
	claimedA := waitForEffectOwnedBy(t, workerA, job.ID, workerAID, 15*time.Second)
	tokenA := claimedA.FencingToken
	workerA.killAndWait(t)

	forceExpireJobAndRecover(t, ss, job.ID)
	recovered, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after Worker A recovery: %v", err)
	}
	if recovered.State != domain.StateReady {
		t.Fatalf("job after Worker A recovery: state=%v", recovered.State)
	}

	workerBID := "worker-B-recovery-" + uuid.NewString()[:8]
	workerB := startTestWorkerProcess(t, gatewayAddr, queue, workerBID, 1)
	finalJob := waitForSucceededByProcess(t, workerB, job.ID, 15*time.Second)
	workerB.killAndWait(t)

	if finalJob.FencingToken <= tokenA {
		t.Errorf("fencing token must increase: tokenA=%d tokenB=%d", tokenA, finalJob.FencingToken)
	}
	if finalJob.Attempt != 2 {
		t.Errorf("attempt=%d, want 2", finalJob.Attempt)
	}
	var effectRows int
	var resultRef string
	if err := testEnv.pool.QueryRow(ctx, `
		select count(*), min(result_ref)
		from demo_idempotent_effects
		where job_id = $1`, job.ID).Scan(&effectRows, &resultRef); err != nil {
		t.Fatalf("query persistent effect: %v", err)
	}
	if effectRows != 1 || resultRef != "effect:"+job.ID {
		t.Fatalf("persistent effects=%d result_ref=%q", effectRows, resultRef)
	}
	applied, deduplicated := countEffectOutcomes(workerA, workerB)
	if applied != 1 || deduplicated < 1 {
		t.Fatalf("effect outcomes applied=%d deduplicated=%d\nA: %s\nB: %s",
			applied, deduplicated, workerA.output(), workerB.output())
	}
	t.Logf("AT-02 PASSED: real Worker kill, token %d→%d, applied=%d deduplicated=%d",
		tokenA, finalJob.FencingToken, applied, deduplicated)
}

// TestFaultAT04HeartbeatLoss verifies AT-04: Worker loses network connectivity
// (heartbeats stop), lease expires, task is recovered, and the old Worker's
// late Complete is rejected with STALE_LEASE.
//
// Scenario:
//  1. Create and claim a job
//  2. Send one successful heartbeat
//  3. Worker loses network (heartbeats stop)
//  4. Lease expires
//  5. Scheduler recovers the job to ready
//  6. Old Worker tries to Complete → STALE_LEASE error
//  7. New Worker can claim and complete successfully
func TestFaultAT04HeartbeatLoss(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// 1. Create a job with short lease.
	job := createTestJob(t, js, "fault-at04", "demo.sleep")

	claimed, err := claimJobs(ctx, js, store.ClaimParams{
		Queues:   []string{"fault-at04"},
		WorkerID: "worker-isolated",
		MaxJobs:  1,
		LeaseTTL: 100 * time.Millisecond, // Short lease for testing
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	oldToken := claimed[0].FencingToken

	// 2. First heartbeat succeeds (Worker still connected).
	_, err = js.Heartbeat(ctx, job.ID, "worker-isolated", oldToken, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first heartbeat should succeed: %v", err)
	}
	t.Log("First heartbeat succeeded (Worker connected)")

	// 3. Worker loses network: heartbeats stop.
	// (We simply stop calling Heartbeat and wait for lease to expire.)
	t.Log("Worker loses network connectivity (heartbeats stop)")

	// 4. Force lease expiry by setting lease_until in the past.
	// This avoids Docker/WSL2 clock drift issues between Go and PostgreSQL.
	_, err = testEnv.pool.Exec(ctx,
		"update jobs set lease_until = now() - interval '1 second' where id = $1", job.ID)
	if err != nil {
		t.Fatalf("set lease_until past: %v", err)
	}

	// 5. Scheduler recovers the expired lease.
	recovered, err := ss.RecoverExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if recovered < 1 {
		t.Fatal("expected at least 1 recovered job")
	}

	// Verify job is back to ready.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job after recovery: %v", err)
	}
	if got.State != domain.StateReady {
		t.Fatalf("expected ready after recovery, got %s", got.State)
	}
	t.Log("Job recovered to ready state")

	// 6. Old Worker (isolated) tries to Complete with stale token → REJECTED.
	err = js.Complete(ctx, job.ID, "worker-isolated", oldToken, "late-result", 500)
	if err == nil {
		t.Fatal("AT-04 FAILED: old Worker Complete should be rejected with STALE_LEASE")
	}
	t.Logf("Old Worker Complete correctly rejected: %v", err)

	// 7. New Worker claims the recovered job.
	claimed2, err := claimJobs(ctx, js, store.ClaimParams{
		Queues:   []string{"fault-at04"},
		WorkerID: "worker-new",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("new worker claim: %v", err)
	}
	newToken := claimed2[0].FencingToken

	// Fencing token must increase.
	if newToken <= oldToken {
		t.Errorf("fencing token must increase: old=%d, new=%d", oldToken, newToken)
	}

	// 8. New Worker completes successfully.
	err = js.Complete(ctx, job.ID, "worker-new", newToken, "new-result", 200)
	if err != nil {
		t.Fatalf("new worker complete: %v", err)
	}

	// 9. Verify final state.
	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if got.State != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", got.State)
	}

	t.Log("AT-04 PASSED: heartbeat loss → lease expiry → recovery → stale rejection → new worker success")
}

// TestFaultAT03StaleWorkerLateComplete verifies AT-03: After lease expiry and
// re-claim by a new Worker, the old Worker's late Complete is rejected.
// This is similar to AT-04 but focuses on the fencing token protection.
func TestFaultAT03StaleWorkerLateComplete(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	// 1. Create and claim with very short lease.
	job := createTestJob(t, js, "fault-at03", "demo.echo")
	claimed, err := claimJobs(ctx, js, store.ClaimParams{
		Queues:   []string{"fault-at03"},
		WorkerID: "worker-slow",
		MaxJobs:  1,
		LeaseTTL: 1 * time.Millisecond,
	})
	if err != nil || len(claimed) == 0 {
		t.Fatalf("claim: %v", err)
	}
	staleToken := claimed[0].FencingToken

	// 2. Force lease expiry by setting lease_until in the past.
	// This avoids Docker/WSL2 clock drift issues between Go and PostgreSQL.
	_, err = testEnv.pool.Exec(ctx,
		"update jobs set lease_until = now() - interval '1 second' where id = $1", job.ID)
	if err != nil {
		t.Fatalf("set lease_until past: %v", err)
	}

	// 3. Scheduler recovers.
	_, err = ss.RecoverExpiredLeases(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	// 4. New Worker claims.
	claimed2, err := claimJobs(ctx, js, store.ClaimParams{
		Queues:   []string{"fault-at03"},
		WorkerID: "worker-fast",
		MaxJobs:  1,
		LeaseTTL: 30 * time.Second,
	})
	if err != nil || len(claimed2) == 0 {
		t.Fatalf("new claim: %v", err)
	}
	newToken := claimed2[0].FencingToken

	// 5. Old Worker's late Complete is rejected.
	err = js.Complete(ctx, job.ID, "worker-slow", staleToken, "stale", 1000)
	if err == nil {
		t.Fatal("AT-03 FAILED: stale Worker Complete should be rejected")
	}
	t.Logf("Stale Worker rejected: %v", err)

	// 6. New Worker completes successfully.
	err = js.Complete(ctx, job.ID, "worker-fast", newToken, "fresh", 100)
	if err != nil {
		t.Fatalf("new worker complete: %v", err)
	}

	// 7. Verify state.
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.State != domain.StateSucceeded {
		t.Errorf("expected succeeded, got %s", got.State)
	}

	t.Log("AT-03 PASSED: stale Worker late Complete rejected, new Worker succeeds")
}

// stubPollWaiter implements gatewaygrpc.PollWaiter for tests: every wait
// blocks until its context expires, so Poll long-polling re-checks the store
// periodically without a LISTEN/NOTIFY connection.
type stubPollWaiter struct{}

func (stubPollWaiter) WaitForNotification(ctx context.Context) bool {
	<-ctx.Done()
	return false
}

// startFaultGateway starts a real Gateway (real PostgreSQL + gRPC server) with
// a fault injection switch: while enabled, every RPC fails with Unavailable,
// simulating a Gateway/network outage. Returns the listen address and the
// fault switch.
func startFaultGateway(t *testing.T, leaseTTL time.Duration) (string, func(bool)) {
	t.Helper()
	s := setupStore(t)

	var fault atomic.Bool
	interceptor := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if fault.Load() {
			return nil, status.Error(codes.Unavailable, "fault injection: gateway unavailable")
		}
		return handler(ctx, req)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service := gatewaygrpc.NewWorkerService(s, stubPollWaiter{}, leaseTTL, 5*time.Second, 0, true, logger, nil)
	server := grpc.NewServer(grpc.UnaryInterceptor(interceptor))
	workerv1.RegisterWorkerServiceServer(server, service)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fault gateway: %v", err)
	}
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)

	return lis.Addr().String(), fault.Store
}

// waitForJobState polls until the job reaches the wanted state or fails the test.
func waitForJobState(t *testing.T, js *postgres.JobStore, jobID string, want domain.JobState, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got, err := js.GetByID(context.Background(), "test-tenant", jobID)
		if err == nil && got.State == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach state %s within %s", jobID, want, timeout)
}

// TestFaultGatewayBlipWithinTTLNoRedelivery verifies that a transient Gateway
// outage shorter than the lease TTL does not lose the lease: the Worker's
// heartbeat loop retries through the outage, the job is never recovered or
// redelivered, and the original Worker completes it.
func TestFaultGatewayBlipWithinTTLNoRedelivery(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	addr, setFault := startFaultGateway(t, 10*time.Second)

	const workerID = "worker-blip"
	const queue = "fault-blip"

	handlerDone := make(chan struct{})
	registry := worker.NewRegistry()
	registry.Register("demo.blip", worker.HandlerFunc(func(hctx context.Context, _ *worker.ClaimedJob) (string, error) {
		defer close(handlerDone)
		// Run longer than the injected outage (6s > 5s) so the heartbeat
		// loop must survive the blip while the handler is executing.
		select {
		case <-time.After(6 * time.Second):
			return "blip-result", nil
		case <-hctx.Done():
			return "", hctx.Err()
		}
	}))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := worker.NewRuntime(worker.RuntimeConfig{
		WorkerID:          workerID,
		InstanceID:        "blip-test",
		Queues:            []string{queue},
		Capacity:          1,
		GatewayAddr:       addr,
		HeartbeatInterval: 500 * time.Millisecond,
		PollTimeout:       2 * time.Second,
		ShutdownGrace:     5 * time.Second,
		Version:           "test",
	}, registry, logger, nil)

	runCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	runDone := make(chan struct{})
	go func() {
		_ = rt.Run(runCtx)
		close(runDone)
	}()

	job := createTestJob(t, js, queue, "demo.blip")
	waitForJobState(t, js, job.ID, domain.StateRunning, 10*time.Second)

	// Inject a 5s full outage — well within the 10s lease TTL.
	setFault(true)
	time.Sleep(5 * time.Second)

	// Run recovery while the outage persists: the lease must not have
	// expired, so this job must stay running under the original Worker.
	if _, err := ss.RecoverExpiredLeases(ctx); err != nil {
		t.Fatalf("recover during outage: %v", err)
	}
	got, err := js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get job during outage: %v", err)
	}
	if got.State != domain.StateRunning {
		t.Fatalf("job must not be redelivered within TTL, state = %s", got.State)
	}
	if got.LeaseOwner == nil || *got.LeaseOwner != workerID {
		t.Fatalf("lease owner must remain the original worker, got %v", got.LeaseOwner)
	}
	if got.Attempt != 1 {
		t.Fatalf("attempt must remain 1 (no redelivery), got %d", got.Attempt)
	}

	setFault(false)

	// The handler finishes after the outage; heartbeats have resumed, so the
	// original Worker reports success with its original fencing token.
	select {
	case <-handlerDone:
	case <-time.After(15 * time.Second):
		t.Fatal("handler did not finish after outage recovery")
	}
	waitForJobState(t, js, job.ID, domain.StateSucceeded, 20*time.Second)

	got, err = js.GetByID(ctx, "test-tenant", job.ID)
	if err != nil {
		t.Fatalf("get final job: %v", err)
	}
	if got.Attempt != 1 {
		t.Errorf("expected attempt 1 (no redelivery), got %d", got.Attempt)
	}
	if got.LeaseOwner == nil || *got.LeaseOwner != workerID {
		t.Errorf("expected the original worker to complete the job, owner = %v", got.LeaseOwner)
	}

	stopWorker()
	<-runDone
	t.Log("blip-within-TTL PASSED: heartbeat retries survived the outage, no redelivery")
}

// TestFaultLeaseLostCancelsExecutionAndDiscardsResult verifies that when the
// outage outlasts the lease TTL, the Worker cancels the running handler at
// the lease deadline and discards the result (no stale reporting), so the
// scheduler cleanly recovers the job to ready.
func TestFaultLeaseLostCancelsExecutionAndDiscardsResult(t *testing.T) {
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	ctx := context.Background()

	addr, setFault := startFaultGateway(t, 3*time.Second)

	const workerID = "worker-lost"
	const queue = "fault-lost"

	handlerStarted := make(chan struct{})
	handlerErrCh := make(chan error, 1)
	registry := worker.NewRegistry()
	registry.Register("demo.lost", worker.HandlerFunc(func(hctx context.Context, _ *worker.ClaimedJob) (string, error) {
		close(handlerStarted)
		<-hctx.Done()
		handlerErrCh <- hctx.Err()
		return "", hctx.Err()
	}))

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rt := worker.NewRuntime(worker.RuntimeConfig{
		WorkerID:          workerID,
		InstanceID:        "lost-test",
		Queues:            []string{queue},
		Capacity:          1,
		GatewayAddr:       addr,
		HeartbeatInterval: 300 * time.Millisecond,
		PollTimeout:       2 * time.Second,
		ShutdownGrace:     5 * time.Second,
		Version:           "test",
	}, registry, logger, nil)

	runCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	runDone := make(chan struct{})
	go func() {
		_ = rt.Run(runCtx)
		close(runDone)
	}()

	job := createTestJob(t, js, queue, "demo.lost")

	select {
	case <-handlerStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never started")
	}

	// Outage longer than the lease TTL: renewal retries must give up at the
	// lease deadline and cancel execution.
	setFault(true)

	select {
	case handlerErr := <-handlerErrCh:
		if !errors.Is(handlerErr, context.Canceled) {
			t.Fatalf("expected execution cancelled with context.Canceled, got %v", handlerErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execution was not cancelled after lease expiry during outage")
	}

	// Stop the Worker before recovery so it cannot re-claim the job.
	stopWorker()
	<-runDone
	setFault(false)

	// The lease expired during the outage; recovery returns the job to ready.
	// The Worker must not have reported Fail(cancelled) — a stale Worker's
	// result is discarded, so the state must still be recoverable. Poll the
	// recovery to absorb Docker/WSL2 clock drift between Go and PostgreSQL.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := ss.RecoverExpiredLeases(ctx); err != nil {
			t.Fatalf("recover after outage: %v", err)
		}
		got, err := js.GetByID(ctx, "test-tenant", job.ID)
		if err != nil {
			t.Fatalf("get job after recovery: %v", err)
		}
		if got.State == domain.StateReady {
			break
		}
		if got.State != domain.StateRunning {
			t.Fatalf("expected recoverable running/ready state, got %s (stale result was reported?)", got.State)
		}
		if time.Now().After(deadline) {
			t.Fatal("job was not recovered to ready after lease expiry")
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Log("lease-loss PASSED: execution cancelled at lease deadline, stale result discarded")
}
