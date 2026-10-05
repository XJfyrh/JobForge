package runworker

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/businessclient"
	"github.com/xjfyrh/jobforge/internal/runclock"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func actionRPCOutcome(err error) stepOutcome {
	if uncertainRPC(err) {
		return stepOutcome{Abandoned: true}
	}
	reason := rpcReason(err)
	if validStepFailure(reason) {
		return stepOutcome{Failure: reason}
	}
	return stepOutcome{Abandoned: true}
}

func decodeActionBinding(raw []byte, lease *agentv1.RunLease, step *agentv1.StepIdentity) (run.ActionBinding, error) {
	var binding run.ActionBinding
	if len(raw) > 40*1024 || jsonstrict.Decode(raw, &binding) != nil {
		return binding, run.ErrInvalidArgument
	}
	if binding.Action != nil {
		a := binding.Action.Authorization
		if a.TenantID != lease.Execution.TenantId || a.RunID != lease.Execution.RunId || a.SnapshotID != step.SnapshotId || a.SnapshotHash != step.SnapshotHash {
			return binding, run.ErrActionConflict
		}
		if _, err := a.Hash(); err != nil {
			return binding, run.ErrInvalidArgument
		}
		p, err := binding.Action.Parameters.Hash()
		if err != nil || p != a.ParametersHash {
			return binding, run.ErrActionConflict
		}
	}
	if binding.Effect.State == "applied" && (binding.Action == nil || binding.Effect.Receipt == nil || binding.Effect.Receipt.Validate(*binding.Action) != nil) {
		return binding, run.ErrActionConflict
	}
	return binding, nil
}

// runAction executes only Go HTTP with its independent keys. All physical sends
// are authorized first and remain bound to this lease keeper's BOOTTIME rights.
func (w *Worker) runAction(ctx context.Context, lease *agentv1.RunLease, checkpoint *agentv1.Checkpoint, authority executionAuthority) stepOutcome {
	if lease == nil || lease.Execution == nil || checkpoint == nil || checkpoint.NextStep == nil || w.actions == nil {
		return stepOutcome{Failure: "PROFILE_UNAVAILABLE"}
	}
	step := checkpoint.NextStep
	p, ok := w.profiles[step.ProfileId]
	if !ok || p.Hash != step.ProfileHash || !p.ApprovalEnabled() {
		return stepOutcome{Failure: "PROFILE_UNAVAILABLE"}
	}
	if authority.Check() != nil {
		return stepOutcome{Abandoned: true}
	}
	rpcCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	response, err := w.client.GetAction(rpcCtx, &agentv1.GetActionRequest{Execution: lease.Execution, Step: step})
	cancel()
	if err != nil {
		return actionRPCOutcome(err)
	}
	if response == nil {
		return stepOutcome{Abandoned: true}
	}
	binding, err := decodeActionBinding(response.ActionBindingJson, lease, step)
	if err != nil {
		return stepOutcome{Failure: "ACTION_CONFLICT"}
	}
	if authority.Check() != nil {
		return stepOutcome{Abandoned: true}
	}
	if binding.Effect.State == "applied" {
		return w.completeAction(ctx, lease, step, *binding.Effect.Receipt, "", authority)
	}
	if binding.Action == nil {
		rpcCtx, cancel = context.WithTimeout(ctx, controlTimeout)
		authorized, authErr := w.client.AuthorizeAction(rpcCtx, &agentv1.AuthorizeActionRequest{Execution: lease.Execution, Step: step})
		cancel()
		if authErr != nil {
			return actionRPCOutcome(authErr)
		}
		if authorized == nil {
			return stepOutcome{Abandoned: true}
		}
		binding, err = decodeActionBinding(authorized.ActionBindingJson, lease, step)
		if err != nil || binding.Action == nil {
			return stepOutcome{Abandoned: true}
		}
	}
	observation, err := w.actionHTTP(ctx, lease, step, p, *binding.Action, agentv1.ActionCallKind_ACTION_CALL_KIND_RECEIPT_QUERY, authority)
	if err != nil {
		return actionHTTPOutcome(err)
	}
	if observation.Found {
		return w.completeAction(ctx, lease, step, observation.Receipt, observation.PhysicalCallID, authority)
	}
	observation, err = w.actionHTTP(ctx, lease, step, p, *binding.Action, agentv1.ActionCallKind_ACTION_CALL_KIND_ACTION_WRITE, authority)
	if err != nil {
		return actionHTTPOutcome(err)
	}
	if !observation.Found {
		return stepOutcome{Failure: "ACTION_OUTCOME_UNKNOWN"}
	}
	return w.completeAction(ctx, lease, step, observation.Receipt, observation.PhysicalCallID, authority)
}

func actionHTTPOutcome(err error) stepOutcome {
	if errors.Is(err, ErrAuthority) {
		return stepOutcome{Abandoned: true}
	}
	var domain run.ErrorCode
	if errors.As(err, &domain) && validStepFailure(string(domain)) {
		return stepOutcome{Failure: string(domain)}
	}
	if reason := rpcReason(err); validStepFailure(reason) {
		return stepOutcome{Failure: reason}
	}
	return stepOutcome{Abandoned: true}
}

func (w *Worker) actionHTTP(ctx context.Context, lease *agentv1.RunLease, step *agentv1.StepIdentity, p run.Profile, action business.SignedAction, kind agentv1.ActionCallKind, authority executionAuthority) (businessclient.ActionObservation, error) {
	var observation businessclient.ActionObservation
	if authority.Check() != nil {
		return observation, ErrAuthority
	}
	start, err := runclock.Now()
	if err != nil {
		return observation, ErrAuthority
	}
	hash, _ := action.Authorization.Hash()
	id := uuid.NewString()
	rpcCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	response, err := w.client.ReserveActionCall(rpcCtx, &agentv1.ReserveActionCallRequest{Execution: lease.Execution, Step: step, PhysicalCallId: id, Kind: kind, OperationId: action.Authorization.OperationID, AuthorizationHash: hash})
	cancel()
	if err != nil {
		if uncertainRPC(err) {
			return observation, ErrAuthority
		}
		return observation, err
	}
	received, clockErr := runclock.Now()
	if clockErr != nil || authority.Check() != nil || response == nil || response.Reservation == nil || !response.NewlyReserved {
		return observation, ErrAuthority
	}
	r := response.Reservation
	if r.PhysicalCallId != id || r.OperationId != action.Authorization.OperationID || r.AuthorizationHash != hash || r.Kind != kind {
		return observation, ErrAuthority
	}
	dispatch, err := stampDeadline(start, received, r.ReservedAt, r.DispatchExpiresAt, 2*time.Second)
	if err != nil {
		return observation, ErrAuthority
	}
	deadline, err := stampDeadline(start, received, r.ReservedAt, r.CallDeadline, 10*time.Second)
	if err != nil {
		return observation, ErrAuthority
	}
	now, err := runclock.Now()
	if err != nil || now >= dispatch || now >= deadline || authority.Check() != nil {
		return observation, ErrAuthority
	}
	httpCtx, finish := context.WithTimeout(ctx, time.Duration(deadline-now)*time.Millisecond)
	if kind == agentv1.ActionCallKind_ACTION_CALL_KIND_RECEIPT_QUERY {
		observation, err = w.actions.Query(httpCtx, lease.Execution.TenantId, p, action)
	} else {
		observation, err = w.actions.Apply(httpCtx, lease.Execution.TenantId, p, action)
	}
	finish()
	observation.PhysicalCallID = id
	if observation.Transport == "" || !run.ValidHash(observation.Hash) {
		return observation, err
	}
	// Original facts can be recorded after authority loss; they never complete
	// a successor or revive this attempt. No action HTTP is repeated here.
	observeCtx, observeCancel := context.WithTimeout(context.WithoutCancel(ctx), controlTimeout)
	_, observeErr := w.client.ObserveActionCall(observeCtx, &agentv1.ObserveActionCallRequest{Execution: lease.Execution, PhysicalCallId: id, TransportOutcome: observation.Transport, ObservationHash: observation.Hash})
	observeCancel()
	if authority.Check() != nil {
		return observation, ErrAuthority
	}
	if err != nil {
		return observation, err
	}
	if observeErr != nil {
		return observation, ErrAuthority
	}
	return observation, nil
}

func (w *Worker) completeAction(ctx context.Context, lease *agentv1.RunLease, step *agentv1.StepIdentity, receipt business.ActionReceipt, callID string, authority executionAuthority) stepOutcome {
	if authority.Check() != nil {
		return stepOutcome{Abandoned: true}
	}
	raw, _ := json.Marshal(receipt)
	output, _ := json.Marshal(run.StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, Content: raw})
	canonical, err := run.CanonicalCheckpointJSON(output)
	if err != nil {
		return stepOutcome{Failure: "INVALID_ARGUMENT"}
	}
	hash := run.CommitHash(domainStep(step, "apply_ticket_resolution"), canonical)
	rpcCtx, cancel := context.WithTimeout(ctx, controlTimeout)
	response, err := w.client.CompleteAction(rpcCtx, &agentv1.CompleteActionRequest{Execution: lease.Execution, Step: step, ReceiptJson: raw, PhysicalCallId: callID})
	cancel()
	if err == nil && response != nil && response.AttemptClosed && response.State == agentv1.RunState_RUN_STATE_SUCCEEDED && acceptedMatches(response.AcceptedStep, step, lease.Execution.RunId, hash, canonical) {
		return stepOutcome{Commit: &agentv1.CommitStepResponse{AcceptedStep: response.AcceptedStep, CursorVersion: response.CursorVersion, State: response.State, AttemptClosed: true}}
	}
	if err != nil && !uncertainRPC(err) {
		return actionRPCOutcome(err)
	}
	confirmation, confirmErr := confirmFact(context.WithoutCancel(ctx), func(readCtx context.Context) (*agentv1.GetAcceptedCommitResponse, error) {
		return w.client.GetAcceptedCommit(readCtx, &agentv1.GetAcceptedCommitRequest{Execution: lease.Execution, StepId: step.StepId})
	})
	if confirmErr == nil && confirmation != nil && confirmation.Found && !acceptedMatches(confirmation.AcceptedStep, step, lease.Execution.RunId, hash, canonical) {
		return stepOutcome{Fatal: run.ErrStepConflict}
	}
	return stepOutcome{Abandoned: true}
}
