package runworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/xjfyrh/jobforge/internal/runexecutor"
	v2 "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

type closedMeteringProcess struct {
	*coordinatorProcess
	writeError error
	closed     chan struct{}
}

func (p *closedMeteringProcess) WriteMetering(context.Context, v2.Frame) error {
	return p.writeError
}

func (p *closedMeteringProcess) Stop() {
	p.coordinatorProcess.Stop()
	if p.closed != nil {
		select {
		case <-p.closed:
		default:
			close(p.closed)
		}
	}
}

func TestCoordinatorLateSettlementPreservesTypedExit(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		writeError error
		eventFirst bool
	}{
		{"already_closed", runexecutor.ErrStopped, false},
		{"epipe_event_first", runexecutor.ErrMeteringClosed, true},
		{"epipe_completion_first", runexecutor.ErrMeteringClosed, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c, client, original, call, observation := activeCoordinator(t, "model_proposal", true)
			process := &closedMeteringProcess{coordinatorProcess: original, writeError: scenario.writeError}
			c.process = process
			settling, release := make(chan struct{}), make(chan struct{})
			client.settle = func(ctx context.Context, request *agentv1.SettleUsageRequest) (*agentv1.SettleUsageResponse, error) {
				close(settling)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				if request.PhysicalCallId != call.id {
					t.Error("late settlement changed physical identity")
				}
				reservation := proto.Clone(call.reservation).(*agentv1.CallReservation)
				reservation.UsageKnown = true
				return &agentv1.SettleUsageResponse{Reservation: reservation}, nil
			}
			report := c.base("metering_report", observation.EmittedMonoMS)
			report.CallSequence, report.PhysicalCallID, report.ParameterHash = 1, call.id, call.intent.ParameterHash
			report.Usage = &v2.Usage{InputTokens: 1, OutputTokens: 1, ReceiptHash: strings.Repeat("a", 64), UsageHash: *observation.UsageHash}
			c.event(context.Background(), runexecutor.Event{Kind: runexecutor.FrameReceived, Channel: runexecutor.Metering, Frame: &report})
			select {
			case <-settling:
			case <-time.After(time.Second):
				t.Fatal("settlement did not start")
			}
			r := cleanReceipt()
			r.Guardian.Code = 69
			// The original report survives ordinary EOF and a known child exit.
			c.event(context.Background(), runexecutor.Event{Kind: runexecutor.ChannelEOF, Channel: runexecutor.Ordinary})
			c.event(context.Background(), runexecutor.Event{Kind: runexecutor.GuardianExited, Exit: r.Guardian})
			close(release)
			c.complete(context.Background(), nextCompletion(t, c))
			failed := runexecutor.Event{Kind: runexecutor.ChannelFailed, Channel: runexecutor.Metering, Problem: runexecutor.MeteringWriteClosed}
			if errors.Is(scenario.writeError, runexecutor.ErrMeteringClosed) {
				r.Metering.Problem = runexecutor.MeteringWriteClosed
				if scenario.eventFirst {
					c.event(context.Background(), failed)
				}
			}
			c.complete(context.Background(), nextCompletion(t, c))
			if errors.Is(scenario.writeError, runexecutor.ErrMeteringClosed) && !scenario.eventFirst {
				c.event(context.Background(), failed)
			}
			if !call.settled || !c.stopped || !c.conversation.Closed() || process.stopped.Load() || c.tasks != 0 || c.failure != "" {
				t.Fatal("late ACK did not close ordinary work while preserving bounded natural exit")
			}
			if got := c.finish(context.Background(), r); got.Failure != "TIMEOUT" || got.Fatal != nil || got.Commit != nil || got.Abandoned {
				t.Fatalf("late ACK replaced actual timeout: %+v", got)
			}
		})
	}
}

func TestCoordinatorClosedMeteringNeverMasksInputOrCleanupFailure(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*runexecutor.Receipt)
		want   string
		fatal  bool
	}{
		{"success", func(r *runexecutor.Receipt) { r.Guardian.Code = 0 }, "EXECUTOR_PROTOCOL_ERROR", false},
		{"unknown_exit", func(r *runexecutor.Receipt) { r.Guardian.Code = 99 }, "EXECUTOR_PROTOCOL_ERROR", false},
		{"signal", func(r *runexecutor.Receipt) { r.Guardian.Signaled = true }, "EXECUTOR_PROTOCOL_ERROR", false},
		{"ordinary_input", func(r *runexecutor.Receipt) { r.Ordinary.Problem = runexecutor.InvalidFrame }, "EXECUTOR_PROTOCOL_ERROR", false},
		{"metering_input", func(r *runexecutor.Receipt) { r.Metering.Problem = runexecutor.InvalidFrame }, "EXECUTOR_PROTOCOL_ERROR", false},
		{"other_pipe", func(r *runexecutor.Receipt) { r.Metering.Problem = runexecutor.PipeFailure }, "EXECUTOR_PROTOCOL_ERROR", false},
		{"size", func(r *runexecutor.Receipt) { r.Metering.Problem = runexecutor.FrameTooLarge }, "CHECKPOINT_TOO_LARGE", false},
		{"stderr", func(r *runexecutor.Receipt) { r.StderrBytes = 8193 }, "CHECKPOINT_TOO_LARGE", false},
		{"missing_eof", func(r *runexecutor.Receipt) { r.Metering.EOF = false }, "EXECUTOR_PROTOCOL_ERROR", false},
		{"delivery", func(r *runexecutor.Receipt) { r.EventDeliveryFailed = true }, "EXECUTOR_PROTOCOL_ERROR", false},
		{"missing_wait", func(r *runexecutor.Receipt) { r.Guardian.Observed = false }, "", true},
		{"remaining_group", func(r *runexecutor.Receipt) { r.GroupGone = false }, "", true},
		{"missing_join", func(r *runexecutor.Receipt) { r.Metering.Joined = false }, "", true},
		{"cleanup_timeout", func(r *runexecutor.Receipt) { r.CleanupTimedOut = true }, "", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			c, _, _ := coordinatorFixture(t, "model_proposal")
			c.meteringReaderClosed()
			r := cleanReceipt()
			r.Guardian.Code, r.Metering.Problem = 69, runexecutor.MeteringWriteClosed
			scenario.mutate(&r)
			got := c.finish(context.Background(), r)
			if got.Failure != scenario.want || errors.Is(got.Fatal, ErrCleanup) != scenario.fatal || got.Commit != nil {
				t.Fatalf("closed ACK masked an independent failure: %+v", got)
			}
		})
	}
}

func TestCoordinatorOtherWriteFailureRemainsProtocolFailure(t *testing.T) {
	for _, kind := range []string{"metering_write", "ordinary_write"} {
		c, _, _ := coordinatorFixture(t, "model_proposal")
		c.complete(context.Background(), completion{kind: kind, err: runexecutor.ErrWrite})
		if c.failure != "EXECUTOR_PROTOCOL_ERROR" || c.meteringClosed {
			t.Fatal("ordinary or non-EPIPE write failure was treated as advisory")
		}
	}
}

func TestCoordinatorClosedACKStillStopsAChildThatDoesNotExit(t *testing.T) {
	c, _, original := coordinatorFixture(t, "model_proposal")
	process := &closedMeteringProcess{coordinatorProcess: original, closed: make(chan struct{})}
	c.process = process
	c.meteringReaderClosed()
	c.meteringExitBy = time.Now().Add(-time.Second)
	// Keep one local task pending while the loop must independently terminate.
	c.tasks = 1
	go func() {
		<-process.closed
		c.completed <- completion{kind: "metering_write", err: runexecutor.ErrStopped}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.loop(ctx)
	if !process.stopped.Load() || c.tasks != 0 || ctx.Err() != nil {
		t.Fatal("provisional ACK close prevented bounded process termination")
	}
}
