package runworker

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runclock"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	v2 "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func (c *coordinator) reserve(ctx context.Context, intent v2.Frame) {
	if c.ordinaryRPC || len(c.calls) >= 4 || c.stopped {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	id := uuid.NewString()
	c.calls[id] = &callRecord{intent: intent, id: id}
	c.ordinaryRPC, c.tasks = true, c.tasks+1
	request := &agentv1.ReserveCallRequest{Execution: c.lease.Execution, Step: c.checkpoint.NextStep, PhysicalCallId: id,
		ToolInvocationId: intent.ToolInvocationID, Subcall: subcall(intent.Subcall), ParameterHash: intent.ParameterHash, PriceHash: c.priceHash}
	go func() {
		bounded, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		start, err := runclock.Now()
		var response *agentv1.ReserveCallResponse
		if err == nil {
			// Never retry this operation into a new send permission. A lost ACK
			// may have reserved a full hold; only newly_reserved can dispatch.
			response, err = c.worker.client.ReserveCall(bounded, request)
		}
		c.completed <- completion{kind: "reserve", callID: id, start: start, value: response, err: err}
	}()
}

func (c *coordinator) report(ctx context.Context, f v2.Frame, now int64) {
	if c.conversation.AcceptMetering(f, now) != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	call := c.calls[f.PhysicalCallID]
	if call == nil || call.reservation == nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	if call.report != nil {
		return // Original Conversation already verified byte-identical metering.
	}
	call.report = &f
	budget := call.reservation.Budget
	binding, err := v2.ReportBinding(f, c.profile.ExpectedResponseModel)
	if err != nil || budget == nil || binding.ExecutionBindingHash != call.reservation.ExecutionBindingHash ||
		v2.Report(f).Verify(binding, f.ReportHash) != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	disposition, err := run.FirstReportDisposition(binding, v2.Report(f), run.CallBudget{InputTokens: budget.InputTokens,
		OutputTokens: budget.OutputTokens, TotalTokens: budget.TotalTokens, CostMicroyuan: budget.CostMicroyuan}, c.profile.Pricing)
	if err != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	if disposition.BatchStopCode != "" {
		c.fatal = true
		reason := ""
		switch disposition.BatchStopCode {
		case run.BatchStopMeasurementAnomaly, run.BatchStopProviderIdentityInvalid, run.BatchStopProviderModeInvalid:
			reason = "MODEL_PROTOCOL_ERROR"
		case run.BatchStopProviderHTTPRejected:
			reason = "DEPENDENCY_UNAVAILABLE"
		}
		c.stop(reason)
	}
	c.settleNext(ctx)
}

func (c *coordinator) settleNext(ctx context.Context) {
	if c.meteringRPC {
		return
	}
	for _, call := range c.calls {
		if call.report == nil || call.settlementAttempted {
			continue
		}
		call.settlementAttempted = true
		c.meteringRPC, c.tasks = true, c.tasks+1
		request := &agentv1.SettleUsageRequest{Execution: c.lease.Execution, PhysicalCallId: call.id, Usage: usageToWire(call.report.Usage),
			ProviderAudit: providerAuditToWire(call.report.ProviderAudit), ReportHash: call.report.ReportHash}
		id, deadline := call.id, c.meteringDeadline
		go func() {
			// Accepted original-call accounting explicitly survives lease/parent
			// cancellation; this context cannot authorize HTTP or ordinary ACK.
			cleanup := context.WithoutCancel(ctx)
			if !deadline.IsZero() {
				var cancel context.CancelFunc
				cleanup, cancel = context.WithDeadline(cleanup, deadline)
				defer cancel()
			}
			response, err := confirmFact(cleanup, func(bounded context.Context) (*agentv1.SettleUsageResponse, error) {
				return c.worker.client.SettleUsage(bounded, request)
			})
			c.completed <- completion{kind: "settle", callID: id, value: response, err: err}
		}()
		return
	}
}

func (c *coordinator) observe(ctx context.Context, call *callRecord) {
	if c.conversation.Closed() {
		c.stop("")
		return
	}
	if c.stopped || call.observation == nil || call.confirmed || c.ordinaryRPC ||
		(call.observation.UsageDisposition == "reported" && !call.settled) {
		return
	}
	f := call.observation
	code, err := v2.ObservationErrorCode(f.ErrorCode)
	if err != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	request := &agentv1.ObserveCallRequest{Execution: c.lease.Execution, Step: c.checkpoint.NextStep, PhysicalCallId: call.id,
		HttpStatus: int32(f.HTTPStatus), ErrorCode: code, UsageKnown: f.UsageDisposition == "reported",
		TransportOutcome: map[string]agentv1.TransportOutcome{"response": agentv1.TransportOutcome_TRANSPORT_OUTCOME_RESPONSE,
			"unknown": agentv1.TransportOutcome_TRANSPORT_OUTCOME_UNKNOWN}[f.TransportOutcome],
		BusinessOutcome: map[string]agentv1.BusinessOutcome{"accepted": agentv1.BusinessOutcome_BUSINESS_OUTCOME_ACCEPTED,
			"rejected": agentv1.BusinessOutcome_BUSINESS_OUTCOME_REJECTED, "unknown": agentv1.BusinessOutcome_BUSINESS_OUTCOME_UNKNOWN}[f.BusinessOutcome]}
	if request.UsageKnown {
		request.Usage = usageToWire(call.report.Usage)
	}
	if f.AuditHash != nil {
		request.AuditHash = *f.AuditHash
	}
	c.ordinaryRPC, c.tasks = true, c.tasks+1
	id := call.id
	go func() {
		response, err := confirmFact(ctx, func(bounded context.Context) (*agentv1.ObserveCallResponse, error) {
			return c.worker.client.ObserveCall(bounded, request)
		})
		c.completed <- completion{kind: "observe", callID: id, value: response, err: err}
	}()
}

func (c *coordinator) complete(ctx context.Context, done completion) {
	if done.kind == "ordinary_write" || done.kind == "metering_write" {
		meter := done.kind == "metering_write"
		var pending *v2.Frame
		if meter {
			c.meteringSend = false
			pending, c.pendingMetering = c.pendingMetering, nil
		} else {
			c.ordinarySend = false
			pending, c.pendingOrdinary = c.pendingOrdinary, nil
		}
		if done.err != nil {
			if meter && (errors.Is(done.err, runexecutor.ErrStopped) || errors.Is(done.err, runexecutor.ErrMeteringClosed)) {
				c.meteringReaderClosed()
				return
			}
			if !c.stopped {
				c.stop("EXECUTOR_PROTOCOL_ERROR")
			}
			return
		}
		if pending != nil && (!c.stopped || meter) {
			c.send(ctx, *pending, meter)
		}
		return
	}
	call := c.calls[done.callID]
	if call == nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	if done.kind == "settle" {
		c.meteringRPC = false
		c.settled(ctx, call, done)
		c.settleNext(ctx)
		return
	}
	c.ordinaryRPC = false
	if done.err != nil {
		reason := rpcReason(done.err)
		if stopsAuthority(reason) {
			c.authority.Stop(reason)
			c.stop("")
		} else if reason != "" {
			// Latch the server's refusal before killing the executor; EOF or
			// local exit 71 cannot replace a known budget/profile failure.
			c.stop(reason)
		} else {
			// An unconfirmed control fact cannot be asserted as provider failure.
			c.authority.Stop("CONTROL_UNCONFIRMED")
			c.stop("")
		}
		return
	}
	if c.stopped || c.authority.Check() != nil {
		return
	}
	switch done.kind {
	case "reserve":
		c.reserved(ctx, call, done)
	case "observe":
		c.observed(ctx, call, done)
	default:
		c.stop("EXECUTOR_PROTOCOL_ERROR")
	}
}

func (c *coordinator) reserved(ctx context.Context, call *callRecord, done completion) {
	response, ok := done.value.(*agentv1.ReserveCallResponse)
	if !ok || response == nil || !response.NewlyReserved || !matchingReservation(response.Reservation, call.intent, call.id, c.priceHash) {
		c.authority.Stop("CONTROL_UNCONFIRMED")
		c.stop("")
		return
	}
	r := response.Reservation
	binding, bindingErr := v2.ReportBinding(v2.Frame{Binding: c.execute.Binding, PhysicalCallID: call.id, ParameterHash: call.intent.ParameterHash}, c.profile.ExpectedResponseModel)
	if r.Budget == nil || r.UsageKnown || r.MeasurementAnomaly || bindingErr != nil || r.ExecutionBindingHash != binding.ExecutionBindingHash ||
		r.PersistedReportHash != "" || r.PersistedAuditHash != "" {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	now, err := runclock.Now()
	if err != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	maxCall := 10 * time.Second
	if call.intent.Subcall == "chat" || call.intent.Subcall == "query_embedding" {
		maxCall = 60 * time.Second
	}
	dispatch, dispatchErr := stampDeadline(done.start, now, r.ReservedAt, r.DispatchExpiresAt, 30*time.Second)
	deadline, callErr := stampDeadline(done.start, now, r.ReservedAt, r.CallDeadline, maxCall)
	if dispatchErr != nil || callErr != nil {
		c.authority.Stop("CONTROL_UNCONFIRMED")
		c.stop("")
		return
	}
	deadline = min(deadline, c.conversation.Deadline())
	dispatch = min(dispatch, deadline)
	permit := call.intent
	permit.Kind, permit.EmittedMonoMS, permit.PhysicalCallID, permit.Granted = "call_permit", now, call.id, true
	permit.DispatchMS, permit.CallMS = dispatch-now, deadline-now
	permit.InputTokenLimit, permit.OutputTokenLimit = r.Budget.InputTokens, r.Budget.OutputTokens
	if c.conversation.Accept(permit, now) != nil || c.conversation.CanDispatch(call.id, now) != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	call.reservation, call.deadlineMS = r, deadline
	c.send(ctx, permit, false)
}

func (c *coordinator) settled(ctx context.Context, call *callRecord, done completion) {
	response, ok := done.value.(*agentv1.SettleUsageResponse)
	settlement := "unconfirmed"
	if done.err == nil && ok && response != nil && matchingReservation(response.Reservation, call.intent, call.id, c.priceHash) {
		if response.ReportConflict && response.BatchFrozen && response.BatchStopCode != "" {
			settlement = "conflict"
			c.fatal = true
			c.stop("EXECUTOR_PROTOCOL_ERROR")
		} else if persistedReportMatches(response, call) && response.Reservation.MeasurementAnomaly && response.BatchFrozen {
			settlement = "anomaly"
			c.fatal = true
			c.stop("MODEL_PROTOCOL_ERROR")
		} else if persistedReportMatches(response, call) && response.Reservation.UsageKnown && call.report.Usage != nil {
			settlement = "settled"
			call.settled = !response.BatchFrozen && response.BatchStopCode == ""
		} else if persistedReportMatches(response, call) && !response.Reservation.UsageKnown && !response.Reservation.MeasurementAnomaly && response.BatchFrozen {
			settlement = "recorded"
		}
		if response.BatchFrozen || response.BatchStopCode != "" {
			c.fatal = true
			c.stop("")
		}
	}
	now, err := runclock.Now()
	if err != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	ack := c.base("metering_ack", now)
	ack.CallSequence, ack.PhysicalCallID, ack.ReportHash, ack.Settlement = call.intent.CallSequence, call.id, call.report.ReportHash, settlement
	if c.conversation.AcceptMetering(ack, now) != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	if settlement != "settled" {
		// A trusted measurement anomaly stops this Worker, but does not itself
		// revoke an otherwise live Run's permission to report permanent failure.
		// Unconfirmed ordinary accounting still abandons execution entirely.
		if reason := rpcReason(done.err); stopsAuthority(reason) {
			c.authority.Stop(reason)
		} else if settlement == "unconfirmed" {
			c.authority.Stop("CONTROL_UNCONFIRMED")
		}
		c.stop("")
	}
	if !c.stopped && c.result == nil && c.conversation.Closed() {
		c.stop("TIMEOUT")
	}
	// ACK is advisory after Stop; failure cannot re-open ordinary execution.
	c.send(context.WithoutCancel(ctx), ack, true)
	if !c.stopped {
		c.observe(ctx, call)
	}
}

func (c *coordinator) observed(ctx context.Context, call *callRecord, done completion) {
	response, ok := done.value.(*agentv1.ObserveCallResponse)
	if !ok || response == nil || !matchingReservation(response.Reservation, call.intent, call.id, c.priceHash) ||
		call.reservation == nil || response.Reservation.ExecutionBindingHash != call.reservation.ExecutionBindingHash ||
		response.Reservation.MeasurementAnomaly || response.Reservation.UsageKnown != (call.observation.UsageDisposition == "reported") ||
		(call.report != nil && (response.Reservation.PersistedReportHash != call.report.ReportHash || response.Reservation.PersistedAuditHash != reportAuditHash(call.report))) {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	now, err := runclock.Now()
	hash, hashErr := v2.ObservationHash(*call.observation)
	if err != nil || hashErr != nil {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	if now >= call.deadlineMS {
		c.stop("TIMEOUT")
		return
	}
	ack := c.base("call_observation_ack", now)
	ack.CallSequence, ack.PhysicalCallID, ack.ObservationHash = call.intent.CallSequence, call.id, hash
	if c.conversation.Accept(ack, now) != nil || c.conversation.Closed() {
		c.stop("EXECUTOR_PROTOCOL_ERROR")
		return
	}
	call.confirmed = true
	c.send(ctx, ack, false)
}

func (c *coordinator) base(kind string, now int64) v2.Frame {
	return v2.Frame{Version: 2, Kind: kind, RequestID: c.execute.RequestID, Binding: c.execute.Binding, EmittedMonoMS: now}
}
