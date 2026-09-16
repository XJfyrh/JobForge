package runworker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runclock"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	"github.com/xjfyrh/jobforge/internal/runinput"
	v2 "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

type callRecord struct {
	intent              v2.Frame
	id                  string
	reservation         *agentv1.CallReservation
	report              *v2.Frame
	settled             bool
	observation         *v2.Frame
	confirmed           bool
	settlementAttempted bool
	deadlineMS          int64
}

type completion struct {
	kind   string
	callID string
	start  int64
	value  any
	err    error
}

// executorProcess is the consuming coordinator's I/O boundary. Production has
// exactly one implementation, runexecutor.Process with a fixed installed entry.
type executorProcess interface {
	Events() <-chan runexecutor.Event
	WriteOrdinary(context.Context, v2.Frame) error
	WriteMetering(context.Context, v2.Frame) error
	Stop()
	Wait() runexecutor.Receipt
}

// coordinator is confined to one goroutine. RPC and pipe tasks return immutable
// completions; none may mutate Conversation or grant permission themselves.
type coordinator struct {
	worker           *Worker
	lease            *agentv1.RunLease
	checkpoint       *agentv1.Checkpoint
	authority        executionAuthority
	process          executorProcess
	conversation     v2.Conversation
	execute          v2.Frame
	priceHash        string
	profile          run.Profile
	calls            map[string]*callRecord
	result           *v2.Frame
	failure          string
	stopped          bool
	fatal            bool
	tasks            int
	ordinaryRPC      bool
	meteringRPC      bool
	ordinarySend     bool
	meteringSend     bool
	pendingOrdinary  *v2.Frame
	pendingMetering  *v2.Frame
	meteringClosed   bool
	meteringExitBy   time.Time
	meteringDeadline time.Time
	completed        chan completion
}

func (w *Worker) coordinateStep(ctx context.Context, lease *agentv1.RunLease, checkpoint *agentv1.Checkpoint, authority executionAuthority) stepOutcome {
	if lease == nil || lease.Execution == nil || checkpoint == nil || checkpoint.NextStep == nil || authority.Check() != nil {
		return stepOutcome{Abandoned: true}
	}
	s := checkpoint.NextStep
	profile, ok := w.profiles[s.ProfileId]
	selected, err := w.manifest.profile(s.ProfileId, s.ProfileHash)
	if !ok || err != nil || profile.Hash != s.ProfileHash {
		return stepOutcome{Failure: "PROFILE_UNAVAILABLE"}
	}
	environment, ok := w.environments[lease.Execution.TenantId]
	if !ok {
		return stepOutcome{Failure: "PROFILE_UNAVAILABLE"}
	}
	toolID := ""
	if s.Kind == agentv1.StepKind_STEP_KIND_GET_ORDER || s.Kind == agentv1.StepKind_STEP_KIND_GET_DELIVERY || s.Kind == agentv1.StepKind_STEP_KIND_SEARCH_POLICY {
		toolID = uuid.NewString()
		bounded, cancel := context.WithTimeout(ctx, controlTimeout)
		response, beginErr := w.client.BeginTool(bounded, &agentv1.BeginToolRequest{Execution: lease.Execution, Step: s, ToolInvocationId: toolID})
		cancel()
		if beginErr != nil || response == nil || !response.NewlyStarted || response.ToolInvocationId != toolID {
			reason := rpcReason(beginErr)
			if stopsAuthority(reason) {
				authority.Stop(reason)
			} else if reason != "" {
				return stepOutcome{Failure: reason}
			}
			return stepOutcome{Abandoned: true}
		}
	}
	now, err := runclock.Now()
	if err != nil || authority.Check() != nil {
		return stepOutcome{Abandoned: true}
	}
	frame, err := runinput.BuildExecute(lease, checkpoint, runinput.Selection{ExecutorVersion: w.manifest.ExecutorVersion,
		AdapterID: selected.AdapterID, ToolInvocationID: toolID, ExpectedResponseModel: profile.ExpectedResponseModel,
		ProviderAuditPolicy: profile.ProviderAuditPolicy}, uuid.NewString(), now, authority.StepDeadline())
	if err != nil {
		if errors.Is(err, run.ErrCheckpointTooLarge) || errors.Is(err, v2.ErrFrameLimit) {
			return stepOutcome{Failure: "CHECKPOINT_TOO_LARGE"}
		}
		return stepOutcome{Failure: "EXECUTOR_PROTOCOL_ERROR"}
	}
	stepCtx, cancel := context.WithTimeout(ctx, time.Duration(frame.RemainingMS)*time.Millisecond)
	defer cancel()
	process, err := runexecutor.Start(stepCtx, runexecutor.Spec{Environment: stepEnvironment(environment, s.Kind)})
	if err != nil {
		return stepOutcome{Failure: "DEPENDENCY_UNAVAILABLE"}
	}
	c := &coordinator{worker: w, lease: lease, checkpoint: checkpoint, authority: authority, process: process, execute: frame,
		priceHash: profile.Pricing.Hash, profile: profile, calls: make(map[string]*callRecord), completed: make(chan completion, 4)}
	if c.conversation.Accept(frame, now) != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
	} else {
		c.send(stepCtx, frame, false)
	}
	c.loop(stepCtx)
	receipt := process.Wait()
	return c.finish(ctx, receipt)
}

func (c *coordinator) finish(ctx context.Context, receipt runexecutor.Receipt) stepOutcome {
	if !cleaned(receipt) {
		// These receipt fields contain process facts only, never child output.
		slog.Error("executor cleanup unconfirmed", "run_id", c.lease.Execution.RunId,
			"step_kind", c.execute.Binding.StepKind, "guardian_observed", receipt.Guardian.Observed,
			"guardian_code", receipt.Guardian.Code, "guardian_signaled", receipt.Guardian.Signaled,
			"group_gone", receipt.GroupGone, "ordinary_joined", receipt.Ordinary.Joined,
			"metering_joined", receipt.Metering.Joined, "stderr_joined", receipt.StderrJoined,
			"cleanup_timed_out", receipt.CleanupTimedOut)
		return stepOutcome{Fatal: ErrCleanup}
	}
	code := receiptFailure(receipt)
	if c.meteringClosed && !closedMeteringHasTypedExit(receipt) && code != "CHECKPOINT_TOO_LARGE" {
		code = "EXECUTOR_PROTOCOL_ERROR"
	}
	for _, call := range c.calls {
		if call.intent.Subcall == "chat" && call.reservation != nil && (code != "" || c.failure != "" || c.result == nil) {
			// Only the fully confirmed second business-validation failure may
			// finish this case without stopping the batch. Durable storage checks
			// its exact attempt/step terminal facts before any later permission.
			secondRejected := (c.execute.Binding.StepKind == "protocol_correction" || c.profile.Strategy == run.SupportAgentStrategy && c.execute.Binding.StepKind == "model_decision") && code == "MODEL_PROTOCOL_ERROR" &&
				confirmedChat(call) && call.observation.BusinessOutcome == "rejected" &&
				call.observation.ErrorCode == "OUTPUT_INVALID" && !c.stopped && c.failure == ""
			if !secondRejected {
				c.fatal = true
			}
		}
	}
	if c.fatal {
		if c.failure == "" && (code == "TIMEOUT" || code == "DEPENDENCY_UNAVAILABLE") {
			c.failure = code
		}
		if c.failure != "" {
			c.failStoppedBatch(ctx, c.failure)
		}
		return stepOutcome{Fatal: ErrBatchStopped}
	}
	if c.authority.Check() != nil || ctx.Err() != nil {
		return stepOutcome{Abandoned: true}
	}
	if c.failure != "" {
		return stepOutcome{Failure: c.failure}
	}
	if code != "" {
		if code == "abandoned" {
			return stepOutcome{Abandoned: true}
		}
		return stepOutcome{Failure: code}
	}
	if c.result == nil {
		return stepOutcome{Failure: "EXECUTOR_PROTOCOL_ERROR"}
	}
	outcome := c.commit(ctx)
	if outcome.Commit == nil && outcome.Fatal == nil {
		for _, call := range c.calls {
			if call.intent.Subcall == "chat" {
				if outcome.Failure != "" {
					c.failStoppedBatch(ctx, outcome.Failure)
				}
				return stepOutcome{Fatal: ErrBatchStopped}
			}
		}
	}
	return outcome
}

// An ACK reader may disappear while its original report is still settling.
// Close ordinary execution immediately, but allow the already exiting child a
// bounded natural Wait before TERM could replace its typed exit with a signal.
func (c *coordinator) meteringReaderClosed() {
	c.meteringClosed = true
	for _, call := range c.calls {
		if call.intent.Subcall == "chat" && call.reservation != nil {
			c.fatal = true
		}
	}
	if !c.stopped {
		c.stopped = true
		c.conversation.Stop()
		c.meteringExitBy = time.Now().Add(100 * time.Millisecond)
	}
}

// failMeasurementAnomaly is distinct from ordinary continuation: the process
// group is already gone and the original trusted report has had bounded
// settlement. ADR-0018 requires a still-authorized Run to fail permanently while
// this Worker stops all further Claims. STOP/lease loss always forbids the write.
func (c *coordinator) failMeasurementAnomaly(ctx context.Context) {
	c.failStoppedBatch(ctx, "MODEL_PROTOCOL_ERROR")
}

func (c *coordinator) failStoppedBatch(ctx context.Context, reason string) {
	if c.authority.Check() != nil || ctx.Err() != nil {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	_, _ = c.worker.client.FailAttempt(bounded, &agentv1.FailAttemptRequest{Execution: c.lease.Execution,
		Step: c.checkpoint.NextStep, ErrorCode: reason})
}

func stepEnvironment(all runexecutor.Environment, kind agentv1.StepKind) runexecutor.Environment {
	switch kind {
	case agentv1.StepKind_STEP_KIND_GET_ORDER, agentv1.StepKind_STEP_KIND_GET_DELIVERY:
		return runexecutor.Environment{BusinessOrigin: all.BusinessOrigin, BusinessReadKey: all.BusinessReadKey}
	case agentv1.StepKind_STEP_KIND_SEARCH_POLICY:
		return runexecutor.Environment{BusinessOrigin: all.BusinessOrigin, BusinessReadKey: all.BusinessReadKey, OllamaOrigin: all.OllamaOrigin}
	case agentv1.StepKind_STEP_KIND_MODEL_PROPOSAL, agentv1.StepKind_STEP_KIND_MODEL_DECISION, agentv1.StepKind_STEP_KIND_PROTOCOL_CORRECTION:
		return runexecutor.Environment{DeepSeekKey: all.DeepSeekKey}
	default:
		return runexecutor.Environment{}
	}
}

func (c *coordinator) stop(reason string) {
	// A chat reservation request may already have committed despite a lost ACK.
	// Interrupted confirmation cannot be followed by another local Claim.
	for _, call := range c.calls {
		if call.intent.Subcall == "chat" {
			c.fatal = true
		}
	}
	if c.failure == "" && reason != "" {
		c.failure = reason
	}
	c.stopped = true
	c.conversation.Stop()
	c.process.Stop()
}

func (c *coordinator) loop(ctx context.Context) {
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	events := c.process.Events()
	cancelled := ctx.Done()
	for events != nil || c.tasks != 0 {
		select {
		case <-cancelled:
			cancelled = nil
			c.stop("")
		case <-tick.C:
			if !c.meteringExitBy.IsZero() && !time.Now().Before(c.meteringExitBy) {
				c.meteringExitBy = time.Time{}
				c.process.Stop()
			}
			if !c.stopped && c.authority.Check() != nil {
				c.stop("")
			}
		case event, open := <-events:
			if !open {
				events = nil
				c.meteringDeadline = time.Now().Add(controlTimeout)
				continue
			}
			c.event(ctx, event)
		case done := <-c.completed:
			c.tasks--
			c.complete(ctx, done)
		}
	}
}

func (c *coordinator) event(ctx context.Context, event runexecutor.Event) {
	if event.Kind == runexecutor.ChannelFailed && event.Channel == runexecutor.Metering && event.Problem == runexecutor.MeteringWriteClosed {
		c.meteringReaderClosed()
		return
	}
	if event.Kind == runexecutor.ChannelFailed || event.Kind == runexecutor.DiagnosticFailed {
		code := "EXECUTOR_PROTOCOL_ERROR"
		if event.Problem == runexecutor.FrameTooLarge || event.Problem == runexecutor.StderrTooLarge {
			code = "CHECKPOINT_TOO_LARGE"
		}
		c.stop(code)
		return
	}
	// EOF is provisional until actual Wait. Known exit codes must not be
	// overwritten by a premature "missing result" diagnosis.
	if event.Kind != runexecutor.FrameReceived || event.Frame == nil {
		return
	}
	now, err := runclock.Now()
	if err != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	f := *event.Frame
	if event.Channel == runexecutor.Metering {
		c.report(ctx, f, now)
		return
	}
	if c.stopped || c.authority.Check() != nil {
		c.stop("")
		return
	}
	if c.conversation.Accept(f, now) != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	switch f.Kind {
	case "call_intent":
		c.reserve(ctx, f)
	case "call_observation":
		call := c.calls[f.PhysicalCallID]
		if call == nil || call.observation != nil {
			c.stop("EXECUTOR_PROTOCOL_ERROR")
			return
		}
		call.observation = &f
		if c.conversation.Closed() {
			c.stop("")
			return
		}
		c.observe(ctx, call)
	case "step_result":
		if c.result != nil {
			c.stop("EXECUTOR_PROTOCOL_ERROR")
			return
		}
		c.result = &f
	default:
		c.stop("EXECUTOR_PROTOCOL_ERROR")
	}
}

func (c *coordinator) send(ctx context.Context, frame v2.Frame, meter bool) {
	if (meter && c.meteringSend) || (!meter && c.ordinarySend) {
		pending := &c.pendingOrdinary
		if meter {
			pending = &c.pendingMetering
		}
		if *pending != nil {
			c.stop("EXECUTOR_PROTOCOL_ERROR")
			return
		}
		*pending = &frame
		return
	}
	if !meter {
		now, err := runclock.Now()
		if err != nil || c.stopped || c.authority.Check() != nil {
			c.stop("")
			return
		}
		if frame.Kind == "call_permit" && now >= frame.EmittedMonoMS+frame.DispatchMS {
			c.stop("TIMEOUT")
			return
		}
		if frame.Kind == "call_observation_ack" {
			call := c.calls[frame.PhysicalCallID]
			if call == nil || call.deadlineMS <= 0 {
				c.stop("EXECUTOR_PROTOCOL_ERROR")
				return
			}
			if now >= call.deadlineMS {
				c.stop("TIMEOUT")
				return
			}
		}
	}
	kind := "ordinary_write"
	if meter {
		kind, c.meteringSend = "metering_write", true
	} else {
		c.ordinarySend = true
	}
	c.tasks++
	go func() {
		bounded, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		var err error
		if meter {
			err = c.process.WriteMetering(bounded, frame)
		} else {
			err = c.process.WriteOrdinary(bounded, frame)
		}
		c.completed <- completion{kind: kind, err: err}
	}()
}

func cleaned(r runexecutor.Receipt) bool {
	return r.Guardian.Observed && r.GroupGone && r.Ordinary.Joined && r.Metering.Joined && r.StderrJoined && !r.CleanupTimedOut
}

func receiptFailure(r runexecutor.Receipt) string {
	if r.StderrBytes > 8192 || r.Ordinary.Problem == runexecutor.FrameTooLarge || r.Metering.Problem == runexecutor.FrameTooLarge {
		return "CHECKPOINT_TOO_LARGE"
	}
	meteringFailure := r.Metering.Problem != runexecutor.NoProblem
	if r.Metering.Problem == runexecutor.MeteringWriteClosed && closedMeteringHasTypedExit(r) {
		meteringFailure = false
	}
	if r.EventDeliveryFailed || r.Ordinary.Problem != runexecutor.NoProblem || meteringFailure || r.Guardian.Signaled {
		return "EXECUTOR_PROTOCOL_ERROR"
	}
	if r.Guardian.Code != 0 {
		if code := map[int]string{64: "INVALID_ARGUMENT", 65: "EXECUTOR_PROTOCOL_ERROR", 66: "PROFILE_UNAVAILABLE",
			67: "CHECKPOINT_TOO_LARGE", 68: "DEPENDENCY_UNAVAILABLE", 69: "TIMEOUT", 70: "MODEL_PROTOCOL_ERROR", 71: "abandoned"}[r.Guardian.Code]; code != "" {
			return code
		}
		return "EXECUTOR_PROTOCOL_ERROR"
	}
	if !r.Ordinary.EOF || !r.Metering.EOF {
		return "EXECUTOR_PROTOCOL_ERROR"
	}
	return ""
}

func closedMeteringHasTypedExit(r runexecutor.Receipt) bool {
	return cleaned(r) && r.Ordinary.EOF && r.Metering.EOF && !r.Guardian.Signaled && r.Guardian.Code >= 64 && r.Guardian.Code <= 71
}
