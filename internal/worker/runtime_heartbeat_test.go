package worker

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// fakeWorkerClient implements workerv1.WorkerServiceClient with scripted
// per-call behavior and attempt counters.
type fakeWorkerClient struct {
	heartbeats atomic.Int32
	completes  atomic.Int32
	fails      atomic.Int32

	register  func() (*workerv1.RegisterResponse, error)
	heartbeat func(call int) (*workerv1.HeartbeatResponse, error)
	complete  func(call int) (*workerv1.CompleteResponse, error)
	fail      func(call int) (*workerv1.FailResponse, error)
}

func (f *fakeWorkerClient) Register(_ context.Context, _ *workerv1.RegisterRequest, _ ...grpc.CallOption) (*workerv1.RegisterResponse, error) {
	if f.register != nil {
		return f.register()
	}
	return &workerv1.RegisterResponse{}, nil
}

func (f *fakeWorkerClient) Poll(_ context.Context, _ *workerv1.PollRequest, _ ...grpc.CallOption) (*workerv1.PollResponse, error) {
	return &workerv1.PollResponse{}, nil
}

func (f *fakeWorkerClient) Heartbeat(_ context.Context, _ *workerv1.HeartbeatRequest, _ ...grpc.CallOption) (*workerv1.HeartbeatResponse, error) {
	n := int(f.heartbeats.Add(1))
	if f.heartbeat != nil {
		return f.heartbeat(n)
	}
	return &workerv1.HeartbeatResponse{}, nil
}

func (f *fakeWorkerClient) Complete(_ context.Context, _ *workerv1.CompleteRequest, _ ...grpc.CallOption) (*workerv1.CompleteResponse, error) {
	n := int(f.completes.Add(1))
	if f.complete != nil {
		return f.complete(n)
	}
	return &workerv1.CompleteResponse{}, nil
}

func (f *fakeWorkerClient) Fail(_ context.Context, _ *workerv1.FailRequest, _ ...grpc.CallOption) (*workerv1.FailResponse, error) {
	n := int(f.fails.Add(1))
	if f.fail != nil {
		return f.fail(n)
	}
	return &workerv1.FailResponse{}, nil
}

// newTestRuntime builds a Runtime with shrunk backoffs and a fast heartbeat
// interval so tests run in milliseconds.
func newTestRuntime(client workerv1.WorkerServiceClient) *Runtime {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := NewRuntime(RuntimeConfig{
		WorkerID:          "test-worker",
		Queues:            []string{"test"},
		HeartbeatInterval: 50 * time.Millisecond,
	}, NewRegistry(), logger, nil)
	r.client = client
	r.hbBackoffInitial = 10 * time.Millisecond
	r.hbBackoffMax = 50 * time.Millisecond
	r.reportBackoffInitial = 5 * time.Millisecond
	return r
}

// TestHeartbeatTransientFailureRetriesAndRecovers verifies that transient
// heartbeat failures (e.g. a 3-blip Gateway outage) do not stop renewal: the
// loop retries and keeps the execution alive, adopting the renewed LeaseUntil
// from the Gateway responses.
func TestHeartbeatTransientFailureRetriesAndRecovers(t *testing.T) {
	fc := &fakeWorkerClient{
		heartbeat: func(call int) (*workerv1.HeartbeatResponse, error) {
			if call <= 3 {
				return nil, status.Error(codes.Unavailable, "gateway restarting")
			}
			return &workerv1.HeartbeatResponse{
				Signal:     workerv1.ControlSignal_CONTROL_SIGNAL_CONTINUE,
				LeaseUntil: timestamppb.New(time.Now().Add(30 * time.Second)),
			}, nil
		},
	}
	r := newTestRuntime(fc)

	// The initial lease expires quickly. If the loop failed to adopt the
	// renewed LeaseUntil from successful responses, it would give up and
	// cancel execution well before the test's observation window ends.
	job := &ClaimedJob{
		ID:           "job-hb-recover",
		FencingToken: 1,
		LeaseUntil:   time.Now().Add(300 * time.Millisecond),
	}

	var cancelCount atomic.Int32
	execCancel := func() { cancelCount.Add(1) }

	var leaseLost atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.heartbeatLoop(ctx, job, execCancel, &leaseLost, nil)
		close(done)
	}()

	// Observe well past the initial lease deadline.
	time.Sleep(800 * time.Millisecond)
	cancel()
	<-done

	if n := fc.heartbeats.Load(); n < 5 {
		t.Fatalf("expected at least 5 heartbeat attempts (retry + recovery), got %d", n)
	}
	if cancelCount.Load() != 0 {
		t.Fatalf("execution must not be cancelled after transient failures, cancelled %d times", cancelCount.Load())
	}
	if leaseLost.Load() {
		t.Fatal("lease must not be marked lost after successful recovery")
	}
}

// TestHeartbeatLeaseExpiryCancelsExecution verifies that when heartbeat
// retries cannot renew before the lease deadline passes, the loop gives up
// exactly once: cancels execution and marks the lease lost.
func TestHeartbeatLeaseExpiryCancelsExecution(t *testing.T) {
	fc := &fakeWorkerClient{
		heartbeat: func(_ int) (*workerv1.HeartbeatResponse, error) {
			return nil, status.Error(codes.Unavailable, "gateway down")
		},
	}
	r := newTestRuntime(fc)

	job := &ClaimedJob{
		ID:           "job-hb-expire",
		FencingToken: 1,
		LeaseUntil:   time.Now().Add(150 * time.Millisecond),
	}

	var cancelCount atomic.Int32
	execCancel := func() { cancelCount.Add(1) }

	var leaseLost atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.heartbeatLoop(ctx, job, execCancel, &leaseLost, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat loop did not give up after lease expiry")
	}

	if cancelCount.Load() != 1 {
		t.Fatalf("expected exactly one exec cancel on give-up, got %d", cancelCount.Load())
	}
	if !leaseLost.Load() {
		t.Fatal("lease must be marked lost after expiry")
	}
}

// TestHeartbeatStaleLeaseCancelsImmediately verifies that a STALE_LEASE
// rejection (FailedPrecondition) abandons execution at once, without retries:
// the lease is already owned by someone else.
func TestHeartbeatStaleLeaseCancelsImmediately(t *testing.T) {
	fc := &fakeWorkerClient{
		heartbeat: func(_ int) (*workerv1.HeartbeatResponse, error) {
			return nil, status.Error(codes.FailedPrecondition, "stale lease")
		},
	}
	r := newTestRuntime(fc)

	job := &ClaimedJob{
		ID:           "job-hb-stale",
		FencingToken: 1,
		LeaseUntil:   time.Now().Add(time.Hour),
	}

	var cancelCount atomic.Int32
	execCancel := func() { cancelCount.Add(1) }

	var leaseLost atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		r.heartbeatLoop(ctx, job, execCancel, &leaseLost, nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat loop did not abandon on STALE_LEASE")
	}

	if n := fc.heartbeats.Load(); n != 1 {
		t.Fatalf("STALE_LEASE must not be retried: expected 1 attempt, got %d", n)
	}
	if cancelCount.Load() != 1 {
		t.Fatalf("expected exactly one exec cancel, got %d", cancelCount.Load())
	}
	if !leaseLost.Load() {
		t.Fatal("lease must be marked lost on STALE_LEASE")
	}
}

// TestHeartbeatCancelSignalCancelsExecution preserves the existing behavior:
// a CANCEL control signal cancels execution but does not mark the lease lost
// (the worker still owns the lease and must report Fail(cancelled)).
func TestHeartbeatCancelSignalCancelsExecution(t *testing.T) {
	fc := &fakeWorkerClient{
		heartbeat: func(_ int) (*workerv1.HeartbeatResponse, error) {
			return &workerv1.HeartbeatResponse{
				Signal:     workerv1.ControlSignal_CONTROL_SIGNAL_CANCEL,
				LeaseUntil: timestamppb.New(time.Now().Add(30 * time.Second)),
			}, nil
		},
	}
	r := newTestRuntime(fc)

	job := &ClaimedJob{
		ID:           "job-hb-cancel",
		FencingToken: 1,
		LeaseUntil:   time.Now().Add(time.Hour),
	}

	var cancelCount atomic.Int32
	execCancel := func() { cancelCount.Add(1) }

	var leaseLost atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	cancelSignalAt := make(chan time.Time, 1)
	go func() {
		r.heartbeatLoop(ctx, job, execCancel, &leaseLost, cancelSignalAt)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat loop did not exit on CANCEL signal")
	}

	if cancelCount.Load() != 1 {
		t.Fatalf("expected exactly one exec cancel, got %d", cancelCount.Load())
	}
	if leaseLost.Load() {
		t.Fatal("CANCEL signal must not mark the lease lost")
	}
	select {
	case receivedAt := <-cancelSignalAt:
		if time.Since(receivedAt) > time.Second {
			t.Fatalf("cancel receipt timestamp is unexpectedly old: %v", receivedAt)
		}
	default:
		t.Fatal("CANCEL signal must publish the local receipt timestamp")
	}
}

func TestRegisterHeartbeatIntervalNegotiation(t *testing.T) {
	tests := []struct {
		name        string
		local       time.Duration
		recommended *durationpb.Duration
		want        time.Duration
	}{
		{name: "unset adopts gateway", recommended: durationpb.New(2 * time.Second), want: 2 * time.Second},
		{name: "explicit local wins", local: 3 * time.Second, recommended: durationpb.New(2 * time.Second), want: 3 * time.Second},
		{name: "missing recommendation uses safe default", want: 5 * time.Second},
		{name: "non-positive recommendation uses safe default", recommended: durationpb.New(0), want: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeWorkerClient{register: func() (*workerv1.RegisterResponse, error) {
				return &workerv1.RegisterResponse{HeartbeatInterval: tt.recommended}, nil
			}}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			runtime := NewRuntime(RuntimeConfig{
				WorkerID:          "registration-worker",
				Queues:            []string{"default"},
				HeartbeatInterval: tt.local,
			}, NewRegistry(), logger, nil)
			runtime.client = client
			if err := runtime.register(context.Background()); err != nil {
				t.Fatalf("register: %v", err)
			}
			if runtime.cfg.HeartbeatInterval != tt.want {
				t.Fatalf("heartbeat interval = %v, want %v", runtime.cfg.HeartbeatInterval, tt.want)
			}
		})
	}
}

// TestReportCompleteRetriesTransientFailures verifies that Complete reporting
// retries transient failures until success (Gateway idempotency absorbs
// duplicates).
func TestReportCompleteRetriesTransientFailures(t *testing.T) {
	fc := &fakeWorkerClient{
		complete: func(call int) (*workerv1.CompleteResponse, error) {
			if call < 3 {
				return nil, status.Error(codes.Unavailable, "gateway restarting")
			}
			return &workerv1.CompleteResponse{State: "succeeded"}, nil
		},
	}
	r := newTestRuntime(fc)
	job := &ClaimedJob{ID: "job-report", FencingToken: 1}

	r.reportComplete(context.Background(), job, "result-ref", 100)

	if n := fc.completes.Load(); n != 3 {
		t.Fatalf("expected 3 complete attempts (2 transient failures + success), got %d", n)
	}
}

// TestReportFailDoesNotRetryStaleLease verifies that a STALE_LEASE rejection
// on Fail reporting is not retried: stale results must never overwrite newer
// state.
func TestReportFailDoesNotRetryStaleLease(t *testing.T) {
	fc := &fakeWorkerClient{
		fail: func(_ int) (*workerv1.FailResponse, error) {
			return nil, status.Error(codes.FailedPrecondition, "stale lease")
		},
	}
	r := newTestRuntime(fc)
	job := &ClaimedJob{ID: "job-report-stale", FencingToken: 1}

	r.reportFail(context.Background(), job, "EXECUTION_ERROR", "boom", false, 100)

	if n := fc.fails.Load(); n != 1 {
		t.Fatalf("stale lease must not be retried: expected 1 attempt, got %d", n)
	}
}
