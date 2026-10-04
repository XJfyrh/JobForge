package runworker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func coordinatorRecoveryProfile(t *testing.T) run.Profile {
	t.Helper()
	raw, err := os.ReadFile("../../api/support/recovery-v1/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Profile run.Profile `json:"profile"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || !fixture.Profile.ConfirmedStepRecovery() {
		t.Fatalf("invalid recovery fixture: %v", err)
	}
	return fixture.Profile
}

func TestCoordinatorRecoveryRequiresActualLossAndEveryProcessBarrier(t *testing.T) {
	for _, mode := range []string{"signal", "guardian72", "metering_closed", "bad_frame", "partial", "size", "stderr", "event_loss", "missing_eof", "group_alive", "known_error", "local_protocol", "unknown_chat"} {
		t.Run(mode, func(t *testing.T) {
			c, _, _ := coordinatorFixture(t, "model_decision")
			c.profile = coordinatorRecoveryProfile(t)
			r := cleanReceipt()
			r.Guardian.Signaled = true
			switch mode {
			case "guardian72":
				r.Guardian.Signaled, r.Guardian.Code = false, 72
			case "metering_closed":
				r.Metering.Problem = runexecutor.MeteringWriteClosed
			case "bad_frame":
				r.Ordinary.Problem = runexecutor.InvalidFrame
			case "partial":
				r.Metering.Problem = runexecutor.InvalidFrame
			case "size":
				r.Ordinary.Problem = runexecutor.FrameTooLarge
			case "stderr":
				r.StderrBytes = 8193
			case "event_loss":
				r.EventDeliveryFailed = true
			case "missing_eof":
				r.Metering.EOF = false
			case "group_alive":
				r.GroupGone = false
			case "known_error":
				r.Guardian.Signaled, r.Guardian.Code = false, 65
			case "local_protocol":
				c.failure = "EXECUTOR_PROTOCOL_ERROR"
			case "unknown_chat":
				confirmedFixtureCall(t, c, false)
			}
			outcome := c.finish(context.Background(), r)
			allowed := mode == "signal" || mode == "guardian72" || mode == "metering_closed"
			if allowed && (!outcome.Abandoned || outcome.Failure != "" || outcome.Fatal != nil || outcome.Commit != nil) {
				t.Fatalf("pure loss invented an outcome: %+v", outcome)
			}
			if !allowed && outcome.Abandoned {
				t.Fatalf("protocol or missing fact became process loss: %s", mode)
			}
			if mode == "unknown_chat" && !errors.Is(outcome.Fatal, ErrBatchStopped) {
				t.Fatal("missing chat confirmation did not stop Worker")
			}
		})
	}
}

func TestCoordinatorRecoveryCommitACKUncertaintyDoesNotFailChat(t *testing.T) {
	c, client, _ := coordinatorFixture(t, "model_decision")
	c.profile = coordinatorRecoveryProfile(t)
	call := confirmedFixtureCall(t, c, true)
	coordinatorResult(t, c, false)
	c.result.Result, _ = json.Marshal(run.StepResult{SchemaVersion: 1, PhysicalCallID: call.id, EvidenceRefs: []string{},
		Content: json.RawMessage(`{"type":"tool","name":"get_order","arguments":{"order_id":null}}`)})
	commits, reads := 0, 0
	client.commit = func(context.Context, *agentv1.CommitStepRequest) (*agentv1.CommitStepResponse, error) {
		commits++
		return nil, status.Error(codes.Unavailable, "synthetic missing ACK")
	}
	client.lookup = func(context.Context, *agentv1.GetAcceptedCommitRequest) (*agentv1.GetAcceptedCommitResponse, error) {
		reads++
		return &agentv1.GetAcceptedCommitResponse{Found: false}, nil
	}
	outcome := c.finish(context.Background(), cleanReceipt())
	if !outcome.Abandoned || outcome.Fatal != nil || outcome.Failure != "" || commits != 1 || reads < 1 || reads > 2 {
		t.Fatalf("ACK uncertainty reported failure: %+v writes=%d reads=%d", outcome, commits, reads)
	}
}
