package runworker

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	"github.com/xjfyrh/jobforge/internal/runinput"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

// lifecycleClient exercises coordinator ownership without pretending that these
// synthetic RPC responses are PostgreSQL or real executor process evidence.
type lifecycleClient struct {
	agentv1.AgentServiceClient
	checkpoints []*agentv1.Checkpoint
	reads       int
	failures    []string
	acks        int
	registerFn  func(context.Context, *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error)
	heartbeatFn func(context.Context, *agentv1.HeartbeatRequest) (*agentv1.HeartbeatResponse, error)
}

func (c *lifecycleClient) GetCheckpoint(ctx context.Context, _ *agentv1.GetCheckpointRequest, _ ...grpc.CallOption) (*agentv1.GetCheckpointResponse, error) {
	if _, ok := ctx.Deadline(); !ok || c.reads >= len(c.checkpoints) {
		return nil, run.ErrInternal
	}
	value := c.checkpoints[c.reads]
	c.reads++
	return &agentv1.GetCheckpointResponse{Checkpoint: proto.Clone(value).(*agentv1.Checkpoint)}, nil
}

func (c *lifecycleClient) FailAttempt(ctx context.Context, req *agentv1.FailAttemptRequest, _ ...grpc.CallOption) (*agentv1.FailAttemptResponse, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, run.ErrInternal
	}
	c.failures = append(c.failures, req.ErrorCode)
	return &agentv1.FailAttemptResponse{State: agentv1.RunState_RUN_STATE_FAILED}, nil
}

func (c *lifecycleClient) AcknowledgeStopped(ctx context.Context, _ *agentv1.AcknowledgeStoppedRequest, _ ...grpc.CallOption) (*agentv1.AcknowledgeStoppedResponse, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, run.ErrInternal
	}
	c.acks++
	return &agentv1.AcknowledgeStoppedResponse{State: agentv1.RunState_RUN_STATE_CANCELLED}, nil
}

func (c *lifecycleClient) Register(ctx context.Context, req *agentv1.RegisterRequest, _ ...grpc.CallOption) (*agentv1.RegisterResponse, error) {
	return c.registerFn(ctx, req)
}

func (c *lifecycleClient) Heartbeat(ctx context.Context, req *agentv1.HeartbeatRequest, _ ...grpc.CallOption) (*agentv1.HeartbeatResponse, error) {
	return c.heartbeatFn(ctx, req)
}

func lifecycleLease() *agentv1.RunLease {
	return &agentv1.RunLease{
		Execution: &agentv1.ExecutionIdentity{TenantId: "runtime-test", RunId: "00000000-0000-4000-8000-000000000001", AttemptNo: 1, FencingToken: 1,
			Session: &agentv1.SessionIdentity{WorkerId: "test-worker", SessionId: "00000000-0000-4000-8000-000000000002"}},
		AuthorityObservedAt: timestamppb.New(testDatabaseTime), LeaseUntil: timestamppb.New(testDatabaseTime.Add(30 * time.Second)),
		AttemptDeadline: timestamppb.New(testDatabaseTime.Add(180 * time.Second)), RunDeadline: timestamppb.New(testDatabaseTime.Add(time.Hour)),
		Checkpoint: &agentv1.Checkpoint{NextStep: &agentv1.StepIdentity{StepId: "00000000-0000-4000-8000-000000000003", Sequence: 1,
			Kind: agentv1.StepKind_STEP_KIND_READ_TICKET, ProfileId: "test-profile", ProfileHash: strings.Repeat("a", 64)}, Snapshot: &agentv1.SnapshotBinding{}},
	}
}

func TestLifecycleWaitsForServerCheckpointAfterConfirmedCommit(t *testing.T) {
	lease := lifecycleLease()
	next := proto.Clone(lease.Checkpoint).(*agentv1.Checkpoint)
	next.CursorVersion, next.NextStep.CursorVersion, next.NextStep.Sequence = 1, 1, 2
	next.NextStep.StepId = "00000000-0000-4000-8000-000000000004"
	next.NextStep.Kind = agentv1.StepKind_STEP_KIND_GET_ORDER
	client := &lifecycleClient{checkpoints: []*agentv1.Checkpoint{lease.Checkpoint, next}}
	w := &Worker{client: client}
	steps := 0
	execute := func(_ context.Context, original *agentv1.RunLease, cp *agentv1.Checkpoint, authority executionAuthority) stepOutcome {
		steps++
		if original != lease || authority.Check() != nil || cp.CursorVersion != int64(steps-1) || client.reads != steps {
			t.Fatal("step did not use the newly read server cursor")
		}
		commit := &agentv1.CommitStepResponse{AcceptedStep: &agentv1.AcceptedStep{Step: cp.NextStep}, CursorVersion: cp.CursorVersion + 1}
		if steps == 1 {
			commit.State, commit.NextStep = agentv1.RunState_RUN_STATE_RUNNING, next.NextStep
		} else {
			commit.State, commit.AttemptClosed = agentv1.RunState_RUN_STATE_SUCCEEDED, true
		}
		return stepOutcome{Commit: commit}
	}
	if _, err := w.runClaim(context.Background(), func() (int64, error) { return 1000, nil }, execute, lease, 61000, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if steps != 2 || client.reads != 2 || len(client.failures) != 0 || client.acks != 0 || lease.Checkpoint.CursorVersion != 0 {
		t.Fatal("local lifecycle changed server progress or original Claim")
	}
}

func TestLifecycleStopFailureAndAbandonmentPriority(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		outcome stepOutcome
		stop    string
		fails   int
		acks    int
	}{
		{name: "known budget rejection", outcome: stepOutcome{Failure: "BUDGET_EXHAUSTED"}, fails: 1},
		{name: "bare local stop", outcome: stepOutcome{Abandoned: true}},
		{name: "unknown commit acknowledgement", outcome: stepOutcome{Abandoned: true}},
		{name: "server STOP outranks known failure", outcome: stepOutcome{Failure: "BUDGET_EXHAUSTED"}, stop: "server", acks: 1},
		{name: "local stop cannot fabricate ack", outcome: stepOutcome{Failure: "TIMEOUT"}, stop: "local"},
		{name: "cleanup fatal forbids fail or ack", outcome: stepOutcome{Fatal: ErrCleanup}, stop: "server"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			lease := lifecycleLease()
			client := &lifecycleClient{checkpoints: []*agentv1.Checkpoint{lease.Checkpoint}}
			w := &Worker{client: client}
			var held executionAuthority
			execute := func(_ context.Context, _ *agentv1.RunLease, _ *agentv1.Checkpoint, authority executionAuthority) stepOutcome {
				held = authority
				switch scenario.stop {
				case "local":
					authority.Stop("STOP_REQUESTED")
				case "server":
					response := continuingHeartbeat()
					response.Signal, response.StopReason = agentv1.ControlSignal_CONTROL_SIGNAL_STOP, run.StopCancel
					_ = authority.(*leaseKeeper).heartbeat(1000, 1000, response)
				}
				return scenario.outcome
			}
			_, err := w.runClaim(context.Background(), func() (int64, error) { return 1000, nil }, execute, lease, 61000, 1000, 1000)
			if !errors.Is(err, scenario.outcome.Fatal) || len(client.failures) != scenario.fails || client.acks != scenario.acks || client.reads != 1 {
				t.Fatalf("wrong close path: err=%v failures=%d acks=%d reads=%d", err, len(client.failures), client.acks, client.reads)
			}
			if held.Check() == nil {
				t.Fatal("attempt return left lease renewal authority alive")
			}
		})
	}
}

func TestRegisterBindsVersionProfilesAndSubtractsRoundTrip(t *testing.T) {
	clock := &atomic.Int64{}
	clock.Store(1000)
	client := &lifecycleClient{registerFn: func(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
		if req.Version != runinput.ExecutorVersion || !run.ValidUUID(req.StartupId) {
			t.Fatal("registration did not bind the fixed runtime")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("registration lacks a deadline")
		}
		clock.Store(3000)
		return &agentv1.RegisterResponse{Session: lifecycleLease().Execution.Session, Capacity: 1, ProfileIds: []string{"test-profile"},
			HeartbeatInterval: durationpb.New(5 * time.Second), AuthorityObservedAt: timestamppb.New(testDatabaseTime), ExpiresAt: timestamppb.New(testDatabaseTime.Add(time.Minute))}, nil
	}}
	w := &Worker{client: client, profiles: map[string]run.Profile{"test-profile": {}}}
	session, err := w.register(context.Background(), func() (int64, error) { return clock.Load(), nil })
	if err != nil || session.deadline != 61000 {
		t.Fatalf("registration reset relative deadline on receipt: %d %v", session.deadline, err)
	}
}

func TestIdleHeartbeatCannotRenewAnExpiredSession(t *testing.T) {
	clock := &atomic.Int64{}
	clock.Store(1000)
	client := &lifecycleClient{heartbeatFn: func(_ context.Context, req *agentv1.HeartbeatRequest) (*agentv1.HeartbeatResponse, error) {
		if req.Execution != nil {
			t.Fatal("idle heartbeat granted Run authority")
		}
		clock.Store(2000)
		response := continuingHeartbeat()
		response.LeaseUntil = nil
		return response, nil
	}}
	w := &Worker{client: client}
	session := workerSession{identity: lifecycleLease().Execution.Session, deadline: 2000}
	if err := w.idleHeartbeat(context.Background(), func() (int64, error) { return clock.Load(), nil }, &session); !errors.Is(err, ErrAuthority) || session.deadline != 2000 {
		t.Fatal("late idle heartbeat revived expired local session")
	}
}

func TestClaimCannotChangeSessionTenantOrProfile(t *testing.T) {
	lease := lifecycleLease()
	w := &Worker{manifest: Manifest{Profiles: []ManifestProfile{{ProfileID: "test-profile", ProfileHash: strings.Repeat("a", 64)}}},
		environments: map[string]runexecutor.Environment{"runtime-test": {}}}
	if err := w.validClaim(lease, lease.Execution.Session); err != nil {
		t.Fatal(err)
	}
	original := proto.Clone(lease.Execution.Session).(*agentv1.SessionIdentity)
	lease.Execution.Session.SessionId = "00000000-0000-4000-8000-000000000009"
	if w.validClaim(lease, original) == nil {
		t.Fatal("accepted wrong session")
	}
	lease = lifecycleLease()
	lease.Execution.TenantId = "other-tenant"
	if w.validClaim(lease, original) == nil {
		t.Fatal("accepted unauthorized tenant")
	}
	lease = lifecycleLease()
	lease.Checkpoint.NextStep.ProfileHash = strings.Repeat("f", 64)
	if w.validClaim(lease, original) == nil {
		t.Fatal("accepted unregistered profile hash")
	}
}

func TestBlockedHeartbeatDoesNotDelayAuthorityWatchdog(t *testing.T) {
	lease := lifecycleLease()
	clock := &atomic.Int64{}
	clock.Store(1000)
	heartbeatEntered := make(chan struct{})
	client := &lifecycleClient{checkpoints: []*agentv1.Checkpoint{lease.Checkpoint},
		heartbeatFn: func(ctx context.Context, request *agentv1.HeartbeatRequest) (*agentv1.HeartbeatResponse, error) {
			if request.Execution == nil {
				t.Error("active heartbeat omitted execution")
			}
			close(heartbeatEntered)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
	w := &Worker{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	joined := make(chan error, 1)
	go func() {
		_, err := w.runClaim(ctx, func() (int64, error) { return clock.Load(), nil },
			func(ctx context.Context, _ *agentv1.RunLease, _ *agentv1.Checkpoint, _ executionAuthority) stepOutcome {
				<-ctx.Done()
				return stepOutcome{Abandoned: true}
			}, lease, 61000, 1000, 1000)
		joined <- err
	}()
	select {
	case <-heartbeatEntered:
	case <-ctx.Done():
		t.Fatal("heartbeat did not start on its fixed schedule")
	}
	// This barrier represents passage of the old BOOTTIME lease deadline while
	// the control RPC is still blocked. The watchdog must cancel it separately.
	clock.Store(31000)
	select {
	case err := <-joined:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked heartbeat prevented revocation or goroutine join")
	}
	if client.acks != 0 || len(client.failures) != 0 {
		t.Fatal("local expiry invented a cancellation or failure write")
	}
}
