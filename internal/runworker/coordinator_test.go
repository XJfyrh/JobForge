package runworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runclock"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	v2 "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

type coordinatorAuthority struct{ stopped atomic.Bool }

func (a *coordinatorAuthority) Check() error {
	if a.stopped.Load() {
		return ErrAuthority
	}
	return nil
}
func (*coordinatorAuthority) StepDeadline() int64 { return 1 << 52 }
func (a *coordinatorAuthority) Stop(string)       { a.stopped.Store(true) }

type coordinatorProcess struct {
	writes  chan v2.Frame
	stopped atomic.Bool
}

func (*coordinatorProcess) Events() <-chan runexecutor.Event { return nil }
func (*coordinatorProcess) Wait() runexecutor.Receipt        { return cleanReceipt() }
func (p *coordinatorProcess) Stop()                          { p.stopped.Store(true) }
func (p *coordinatorProcess) WriteOrdinary(ctx context.Context, frame v2.Frame) error {
	select {
	case p.writes <- frame:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (p *coordinatorProcess) WriteMetering(ctx context.Context, frame v2.Frame) error {
	return p.WriteOrdinary(ctx, frame)
}

type coordinatorClient struct {
	agentv1.AgentServiceClient
	commit  func(context.Context, *agentv1.CommitStepRequest) (*agentv1.CommitStepResponse, error)
	lookup  func(context.Context, *agentv1.GetAcceptedCommitRequest) (*agentv1.GetAcceptedCommitResponse, error)
	observe func(context.Context, *agentv1.ObserveCallRequest) (*agentv1.ObserveCallResponse, error)
	settle  func(context.Context, *agentv1.SettleUsageRequest) (*agentv1.SettleUsageResponse, error)
}

func (f *coordinatorClient) CommitStep(ctx context.Context, request *agentv1.CommitStepRequest, _ ...grpc.CallOption) (*agentv1.CommitStepResponse, error) {
	return f.commit(ctx, request)
}
func (f *coordinatorClient) GetAcceptedCommit(ctx context.Context, request *agentv1.GetAcceptedCommitRequest, _ ...grpc.CallOption) (*agentv1.GetAcceptedCommitResponse, error) {
	return f.lookup(ctx, request)
}
func (f *coordinatorClient) ObserveCall(ctx context.Context, request *agentv1.ObserveCallRequest, _ ...grpc.CallOption) (*agentv1.ObserveCallResponse, error) {
	return f.observe(ctx, request)
}
func (f *coordinatorClient) SettleUsage(ctx context.Context, request *agentv1.SettleUsageRequest, _ ...grpc.CallOption) (*agentv1.SettleUsageResponse, error) {
	return f.settle(ctx, request)
}

func cleanReceipt() runexecutor.Receipt {
	return runexecutor.Receipt{Guardian: runexecutor.Exit{Observed: true}, GroupGone: true,
		Ordinary: runexecutor.ChannelReceipt{EOF: true, Joined: true},
		Metering: runexecutor.ChannelReceipt{EOF: true, Joined: true}, StderrJoined: true}
}

func TestCoordinatorReceiptRequiresEveryIndependentBarrier(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*runexecutor.Receipt)
	}{
		{"wait", func(r *runexecutor.Receipt) { r.Guardian.Observed = false }},
		{"group", func(r *runexecutor.Receipt) { r.GroupGone = false }},
		{"ordinary_join", func(r *runexecutor.Receipt) { r.Ordinary.Joined = false }},
		{"metering_join", func(r *runexecutor.Receipt) { r.Metering.Joined = false }},
		{"stderr_join", func(r *runexecutor.Receipt) { r.StderrJoined = false }},
		{"cleanup_timeout", func(r *runexecutor.Receipt) { r.CleanupTimedOut = true }},
		{"ordinary_eof", func(r *runexecutor.Receipt) { r.Ordinary.EOF = false }},
		{"metering_eof", func(r *runexecutor.Receipt) { r.Metering.EOF = false }},
		{"trailing_frame", func(r *runexecutor.Receipt) { r.Ordinary.Problem = runexecutor.InvalidFrame }},
		{"lost_event", func(r *runexecutor.Receipt) { r.EventDeliveryFailed = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := cleanReceipt()
			tc.mutate(&r)
			if cleaned(r) && receiptFailure(r) == "" {
				t.Fatal("missing process fact passed commit barriers")
			}
		})
	}
}

func TestCoordinatorExitFailureAndSizePrecedence(t *testing.T) {
	expected := map[int]string{0: "", 64: "INVALID_ARGUMENT", 65: "EXECUTOR_PROTOCOL_ERROR", 66: "PROFILE_UNAVAILABLE", 67: "CHECKPOINT_TOO_LARGE", 68: "DEPENDENCY_UNAVAILABLE", 69: "TIMEOUT", 70: "MODEL_PROTOCOL_ERROR", 71: "abandoned", 72: "EXECUTOR_PROTOCOL_ERROR"}
	for code, want := range expected {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := cleanReceipt()
			r.Guardian.Code = code
			if got := receiptFailure(r); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			r.StderrBytes = 8193
			if receiptFailure(r) != "CHECKPOINT_TOO_LARGE" {
				t.Fatal("known child exit masked a local size failure")
			}
		})
	}
	r := cleanReceipt()
	r.Guardian.Signaled = true
	if receiptFailure(r) != "EXECUTOR_PROTOCOL_ERROR" {
		t.Fatal("signal was treated as a provider outcome")
	}
}

func coordinatorFixture(t *testing.T, kind string) (*coordinator, *coordinatorClient, *coordinatorProcess) {
	t.Helper()
	binding := v2.Binding{TenantID: "tenant", WorkerID: "worker", RunID: "00000000-0000-4000-8000-000000000001", StepID: "00000000-0000-4000-8000-000000000002", SessionID: "00000000-0000-4000-8000-000000000003", ProfileID: "profile", ProfileHash: strings.Repeat("a", 64), SnapshotID: "00000000-0000-4000-8000-000000000004", SnapshotHash: strings.Repeat("b", 64), InputHash: strings.Repeat("c", 64), AttemptNo: 1, FencingToken: 1, StepSequence: 1, StepKind: kind}
	step := &agentv1.StepIdentity{StepId: binding.StepID, Sequence: 1, ProfileId: binding.ProfileID, ProfileHash: binding.ProfileHash, SnapshotId: binding.SnapshotID, SnapshotHash: binding.SnapshotHash, InputHash: binding.InputHash}
	client := &coordinatorClient{}
	process := &coordinatorProcess{writes: make(chan v2.Frame, 8)}
	c := &coordinator{worker: &Worker{client: client}, authority: &coordinatorAuthority{}, process: process,
		lease:      &agentv1.RunLease{Execution: &agentv1.ExecutionIdentity{TenantId: binding.TenantID, RunId: binding.RunID}},
		checkpoint: &agentv1.Checkpoint{NextStep: step}, calls: make(map[string]*callRecord), completed: make(chan completion, 8), priceHash: strings.Repeat("d", 64)}
	c.execute = v2.Frame{Version: 2, Kind: "execute_step", RequestID: "00000000-0000-4000-8000-000000000005", Binding: binding, RemainingMS: 180000, Input: json.RawMessage(`{}`), Checkpoint: json.RawMessage(`{}`)}
	return c, client, process
}

func coordinatorResult(t *testing.T, c *coordinator, correction bool) {
	t.Helper()
	result := run.StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, Content: json.RawMessage(`null`), CorrectionRequired: correction}
	if !correction {
		result.Proposal = &run.Proposal{Decision: "no_action", Summary: "fixture", EvidenceRefs: []string{"fixture"}}
	}
	for _, call := range c.calls {
		result.PhysicalCallID, result.ToolInvocationID = call.id, call.intent.ToolInvocationID
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	f := c.base("step_result", 1000)
	f.Result, f.Outcome = encoded, "success"
	if correction {
		f.Outcome, f.ErrorCode = "error", "OUTPUT_INVALID"
	}
	c.result = &f
}

func confirmedFixtureCall(c *coordinator, reported bool) *callRecord {
	call := &callRecord{id: "00000000-0000-4000-8000-000000000006", confirmed: true, settled: reported,
		intent: v2.Frame{CallSequence: 1}, observation: &v2.Frame{TransportOutcome: "response", BusinessOutcome: "accepted", UsageDisposition: "unknown"}}
	if reported {
		call.observation.UsageDisposition = "reported"
	}
	c.calls[call.id] = call
	return call
}

func successfulCommit(request *agentv1.CommitStepRequest) *agentv1.CommitStepResponse {
	return &agentv1.CommitStepResponse{AcceptedStep: &agentv1.AcceptedStep{Step: proto.Clone(request.Step).(*agentv1.StepIdentity), CommitHash: request.CommitHash, ResultJson: append([]byte(nil), request.ResultJson...), ResultRef: fmt.Sprintf("run-step:%s:%d", request.Execution.RunId, request.Step.Sequence)}}
}

func TestCoordinatorCommitNeedsConfirmedObservationAndMetering(t *testing.T) {
	for _, mode := range []string{"free_unknown", "reported", "missing_ack", "unsettled", "wrong_call", "stopped"} {
		t.Run(mode, func(t *testing.T) {
			c, client, _ := coordinatorFixture(t, "model_proposal")
			call := confirmedFixtureCall(c, mode == "reported" || mode == "unsettled")
			coordinatorResult(t, c, false)
			switch mode {
			case "missing_ack":
				call.confirmed = false
			case "unsettled":
				call.settled = false
			case "wrong_call":
				call.id = "00000000-0000-4000-8000-000000000099"
			case "stopped":
				c.authority.Stop("STOP_REQUESTED")
			}
			calls := 0
			client.commit = func(_ context.Context, request *agentv1.CommitStepRequest) (*agentv1.CommitStepResponse, error) {
				calls++
				return successfulCommit(request), nil
			}
			outcome := c.commit(context.Background())
			valid := mode == "free_unknown" || mode == "reported"
			if valid && (outcome.Commit == nil || calls != 1) || !valid && (outcome.Commit != nil || calls != 0) {
				t.Fatalf("unexpected commit count %d, outcome %+v", calls, outcome)
			}
		})
	}
}

func TestCoordinatorCommitOnlyFirstConfirmedCorrection(t *testing.T) {
	for _, mode := range []string{"first", "second", "timeout", "unknown_transport", "unsettled"} {
		t.Run(mode, func(t *testing.T) {
			kind := "model_proposal"
			if mode == "second" {
				kind = "protocol_correction"
			}
			c, client, _ := coordinatorFixture(t, kind)
			call := confirmedFixtureCall(c, true)
			call.observation.BusinessOutcome, call.observation.ErrorCode = "rejected", "OUTPUT_INVALID"
			if mode == "timeout" {
				call.observation.ErrorCode = "TIMEOUT"
			}
			if mode == "unknown_transport" {
				call.observation.TransportOutcome = "unknown"
			}
			if mode == "unsettled" {
				call.settled = false
			}
			coordinatorResult(t, c, true)
			calls := 0
			client.commit = func(_ context.Context, request *agentv1.CommitStepRequest) (*agentv1.CommitStepResponse, error) {
				calls++
				return successfulCommit(request), nil
			}
			outcome := c.commit(context.Background())
			if mode == "first" && (calls != 1 || outcome.Commit == nil) || mode != "first" && (calls != 0 || outcome.Commit != nil) {
				t.Fatalf("correction bypassed its barrier: calls=%d outcome=%+v", calls, outcome)
			}
		})
	}
}

func TestCoordinatorCommitSizePreservesLocalFact(t *testing.T) {
	c, _, _ := coordinatorFixture(t, "read_ticket")
	coordinatorResult(t, c, false)
	c.result.Result = json.RawMessage(`{"schema_version":1,"tool_invocation_id":"","physical_call_id":"","evidence_refs":[],"content":{"padding":"` + strings.Repeat("x", 8192) + `"},"proposal":null,"correction_required":false}`)
	if got := c.commit(context.Background()); got.Failure != "CHECKPOINT_TOO_LARGE" {
		t.Fatalf("size failure lost: %+v", got)
	}
}

func TestCoordinatorLostCommitAckOnlyReadsSameCommit(t *testing.T) {
	for _, mode := range []string{"not_found", "same", "different"} {
		t.Run(mode, func(t *testing.T) {
			c, client, _ := coordinatorFixture(t, "submit_proposal")
			coordinatorResult(t, c, false)
			commits, lookups := 0, 0
			var accepted *agentv1.AcceptedStep
			client.commit = func(_ context.Context, request *agentv1.CommitStepRequest) (*agentv1.CommitStepResponse, error) {
				commits++
				accepted = successfulCommit(request).AcceptedStep
				return nil, status.Error(codes.Unavailable, "fixed test failure")
			}
			client.lookup = func(ctx context.Context, request *agentv1.GetAcceptedCommitRequest) (*agentv1.GetAcceptedCommitResponse, error) {
				lookups++
				deadline, bounded := ctx.Deadline()
				if !bounded || time.Until(deadline) > controlTimeout || request.StepId != c.checkpoint.NextStep.StepId {
					t.Error("unbounded or wrong commit confirmation")
				}
				if lookups == 1 {
					return nil, status.Error(codes.Unavailable, "fixed test failure")
				}
				if mode == "different" {
					accepted.CommitHash = strings.Repeat("f", 64)
				}
				return &agentv1.GetAcceptedCommitResponse{Found: mode != "not_found", AcceptedStep: accepted}, nil
			}
			outcome := c.commit(context.Background())
			if commits != 1 || lookups != 2 || outcome.Commit != nil || outcome.Failure != "" {
				t.Fatalf("commit replay or false failure: %d/%d %+v", commits, lookups, outcome)
			}
			if mode == "different" && !errors.Is(outcome.Fatal, run.ErrStepConflict) || mode != "different" && !outcome.Abandoned {
				t.Fatalf("wrong confirmation outcome %+v", outcome)
			}
		})
	}
}

func TestCoordinatorBudgetRefusalSurvivesFollowupProtocolFailure(t *testing.T) {
	c, _, process := coordinatorFixture(t, "model_proposal")
	call := confirmedFixtureCall(c, false)
	refusal, err := status.New(codes.ResourceExhausted, "fixed test refusal").WithDetails(&errdetails.ErrorInfo{Domain: "jobforge.agent.v1", Reason: "BUDGET_EXHAUSTED"})
	if err != nil {
		t.Fatal(err)
	}
	c.complete(context.Background(), completion{kind: "reserve", callID: call.id, err: refusal.Err()})
	c.stop("EXECUTOR_PROTOCOL_ERROR")
	if c.failure != "BUDGET_EXHAUSTED" || !process.stopped.Load() || c.authority.Check() != nil {
		t.Fatal("known refusal was replaced or confused with authority loss")
	}
	c.authority.Stop("STALE_LEASE")
	if c.authority.Check() == nil {
		t.Fatal("late authority loss did not stop execution")
	}
}

func TestCoordinatorDeadlineAnchorsRPCStartAndRejectsEquality(t *testing.T) {
	observed := time.Unix(100, 0)
	expires := observed.Add(10 * time.Second)
	deadline, err := stampDeadline(1000, 2000, timestamppb.New(observed), timestamppb.New(expires), 10*time.Second)
	if err != nil || deadline != 11000 {
		t.Fatalf("deadline reset at receipt: %d %v", deadline, err)
	}
	if _, err := stampDeadline(1000, 11000, timestamppb.New(observed), timestamppb.New(expires), 10*time.Second); !errors.Is(err, runclock.ErrExpired) {
		t.Fatal("equal deadline renewed authority")
	}
}

func activeCoordinator(t *testing.T, kind string, reported bool) (*coordinator, *coordinatorClient, *coordinatorProcess, *callRecord, v2.Frame) {
	t.Helper()
	now, err := runclock.Now()
	if errors.Is(err, runclock.ErrUnsupported) {
		t.Skip("coordinator ACK timing requires actual Linux BOOTTIME; run fixed Linux checks")
	}
	if err != nil {
		t.Fatal(err)
	}
	c, client, process := coordinatorFixture(t, kind)
	c.execute.EmittedMonoMS = now
	if err := c.conversation.Accept(c.execute, now); err != nil {
		t.Fatal(err)
	}
	intent := c.base("call_intent", now)
	intent.CallSequence, intent.ParameterHash = 1, strings.Repeat("e", 64)
	intent.Subcall = "chat"
	if kind == "get_order" {
		intent.Subcall, intent.ToolInvocationID = "get_order", "00000000-0000-4000-8000-000000000008"
	}
	if err := c.conversation.Accept(intent, now); err != nil {
		t.Fatal(err)
	}
	permit := intent
	permit.Kind, permit.PhysicalCallID, permit.Granted = "call_permit", "00000000-0000-4000-8000-000000000006", true
	permit.DispatchMS, permit.CallMS, permit.InputTokenLimit, permit.OutputTokenLimit = 10000, 60000, 1000, 1000
	if kind == "get_order" {
		permit.CallMS, permit.InputTokenLimit, permit.OutputTokenLimit = 10000, 0, 0
	}
	if err := c.conversation.Accept(permit, now); err != nil {
		t.Fatal(err)
	}
	if err := c.conversation.CanDispatch(permit.PhysicalCallID, now); err != nil {
		t.Fatal(err)
	}
	call := &callRecord{id: permit.PhysicalCallID, intent: intent, deadlineMS: now + permit.CallMS,
		reservation: &agentv1.CallReservation{PhysicalCallId: permit.PhysicalCallID, ToolInvocationId: intent.ToolInvocationID,
			Subcall: subcall(intent.Subcall), ParameterHash: intent.ParameterHash, PriceHash: c.priceHash,
			Budget: &agentv1.CallBudget{InputTokens: 1000, OutputTokens: 1000, TotalTokens: 2000}}}
	c.calls[call.id] = call
	observation := c.base("call_observation", now)
	observation.CallSequence, observation.PhysicalCallID = 1, call.id
	observation.TransportOutcome, observation.HTTPStatus, observation.BusinessOutcome, observation.UsageDisposition = "response", 200, "accepted", "unknown"
	if reported {
		usage := &v2.Usage{InputTokens: 1, OutputTokens: 1, ReceiptHash: strings.Repeat("a", 64)}
		usage.UsageHash = run.Fingerprint("jobforge.run.usage.v1", "1", "1", "0", usage.ReceiptHash)
		report := c.base("metering_report", now)
		report.CallSequence, report.PhysicalCallID, report.ParameterHash, report.Usage = 1, call.id, intent.ParameterHash, usage
		// Keep a separately prepared frame until the test deliberately delivers
		// the metering lane. Merely constructing it cannot settle the call.
		observation.UsageDisposition, observation.UsageHash = "reported", &usage.UsageHash
		client.settle = func(_ context.Context, request *agentv1.SettleUsageRequest) (*agentv1.SettleUsageResponse, error) {
			if !proto.Equal(request.Usage, usageToWire(report.Usage)) {
				t.Error("settlement changed original usage")
			}
			reservation := proto.Clone(call.reservation).(*agentv1.CallReservation)
			reservation.UsageKnown = true
			return &agentv1.SettleUsageResponse{Reservation: reservation}, nil
		}
	}
	client.observe = func(_ context.Context, request *agentv1.ObserveCallRequest) (*agentv1.ObserveCallResponse, error) {
		if request.PhysicalCallId != call.id || request.UsageKnown != reported {
			t.Error("observation changed original identity or metering")
		}
		reservation := proto.Clone(call.reservation).(*agentv1.CallReservation)
		reservation.UsageKnown = reported
		return &agentv1.ObserveCallResponse{Reservation: reservation}, nil
	}
	return c, client, process, call, observation
}

func nextCompletion(t *testing.T, c *coordinator) completion {
	t.Helper()
	select {
	case done := <-c.completed:
		c.tasks--
		return done
	case <-time.After(3 * time.Second):
		t.Fatal("bounded test task did not return")
		return completion{}
	}
}

func nextWritten(t *testing.T, p *coordinatorProcess) v2.Frame {
	t.Helper()
	select {
	case frame := <-p.writes:
		return frame
	case <-time.After(3 * time.Second):
		t.Fatal("bounded protocol write did not occur")
		return v2.Frame{}
	}
}

func joinCoordinatorTasks(t *testing.T, c *coordinator) {
	t.Helper()
	for c.tasks > 0 {
		c.complete(context.Background(), nextCompletion(t, c))
	}
}

func TestCoordinatorObservationACKIncludesFreeAndUnknownCalls(t *testing.T) {
	for _, kind := range []string{"get_order", "model_proposal"} {
		t.Run(kind, func(t *testing.T) {
			c, _, process, call, observation := activeCoordinator(t, kind, false)
			c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Ordinary, Frame: &observation})
			if call.confirmed {
				t.Fatal("observation confirmed before RPC completion")
			}
			done := nextCompletion(t, c)
			if done.kind != "observe" {
				t.Fatal("free/unknown observation did not require ObserveCall")
			}
			c.complete(context.Background(), done)
			ack := nextWritten(t, process)
			hash, err := v2.ObservationHash(observation)
			if err != nil || ack.Kind != "call_observation_ack" || ack.ObservationHash != hash || !call.confirmed || call.settled {
				t.Fatal("free/unknown ACK lost original observation identity")
			}
			joinCoordinatorTasks(t, c)
		})
	}
}

func TestCoordinatorReportedObservationWaitsForIndependentMetering(t *testing.T) {
	c, _, process, call, observation := activeCoordinator(t, "model_proposal", true)
	c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Ordinary, Frame: &observation})
	if c.tasks != 0 || call.confirmed {
		t.Fatal("reported observation acknowledged before settlement")
	}
	usage := &v2.Usage{InputTokens: 1, OutputTokens: 1, ReceiptHash: strings.Repeat("a", 64), UsageHash: *observation.UsageHash}
	report := c.base("metering_report", observation.EmittedMonoMS)
	report.CallSequence, report.PhysicalCallID, report.ParameterHash, report.Usage = 1, call.id, call.intent.ParameterHash, usage
	c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Metering, Frame: &report})
	done := nextCompletion(t, c)
	if done.kind != "settle" {
		t.Fatal("metering report did not settle original call")
	}
	c.complete(context.Background(), done)
	// Write completion and RPC completion may arrive in either order. Both are
	// consumed by the same coordinator, never by a task-mutated Conversation.
	joinCoordinatorTasks(t, c)
	first, second := nextWritten(t, process), nextWritten(t, process)
	kinds := map[string]bool{first.Kind: true, second.Kind: true}
	if !kinds["metering_ack"] || !kinds["call_observation_ack"] || !call.settled || !call.confirmed || c.stopped {
		t.Fatal("independent confirmation lanes did not join")
	}
}

func TestCoordinatorResultCanPrecedeACKWriteCompletionDelivery(t *testing.T) {
	c, _, process, call, observation := activeCoordinator(t, "model_proposal", false)
	c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Ordinary, Frame: &observation})
	c.complete(context.Background(), nextCompletion(t, c))
	ack := nextWritten(t, process)
	if !c.ordinarySend || ack.Kind != "call_observation_ack" {
		t.Fatal("test did not retain pending write completion")
	}
	coordinatorResult(t, c, false)
	result := *c.result
	result.EmittedMonoMS = ack.EmittedMonoMS
	c.result = nil
	c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Ordinary, Frame: &result})
	if c.result == nil || !call.confirmed || c.stopped {
		t.Fatal("valid child result rejected solely by task completion ordering")
	}
	joinCoordinatorTasks(t, c)
}

func TestCoordinatorLateQueuedACKCannotRenewOriginalCall(t *testing.T) {
	c, _, process, call, observation := activeCoordinator(t, "model_proposal", false)
	c.ordinarySend = true // Earlier bytes were delivered; its task completion is queued.
	c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Ordinary, Frame: &observation})
	c.complete(context.Background(), nextCompletion(t, c))
	if c.pendingOrdinary == nil || c.pendingOrdinary.Kind != "call_observation_ack" {
		t.Fatal("test did not queue ACK behind previous write completion")
	}
	call.deadlineMS = 1 // Deterministic expired BOOTTIME, without an arbitrary sleep.
	c.complete(context.Background(), completion{kind: "ordinary_write"})
	if c.failure != "TIMEOUT" || !c.stopped || !process.stopped.Load() || len(process.writes) != 0 || c.tasks != 0 {
		t.Fatal("queued ACK extended a finished physical call")
	}
}
