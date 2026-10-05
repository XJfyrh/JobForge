package grpcapi

import (
	"context"
	"encoding/json"

	"github.com/xjfyrh/jobforge/internal/business"
	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type actionAPI interface {
	GetAction(context.Context, string, run.Lease, run.StepIdentity) (run.ActionBinding, error)
	AuthorizeAction(context.Context, string, run.Lease, run.StepIdentity) (run.ActionBinding, error)
	ReserveActionCall(context.Context, string, run.ReserveActionCallRequest) (run.ReserveActionCallResponse, error)
	ObserveActionCall(context.Context, string, run.ObserveActionCallRequest) (run.ActionCall, error)
	CompleteAction(context.Context, string, run.CompleteActionRequest) (run.CommitStepResponse, error)
}

func actionBindingJSON(binding run.ActionBinding) ([]byte, error) {
	raw, err := json.Marshal(binding)
	if err != nil || len(raw) > 40*1024 {
		return nil, run.ErrInternal
	}
	return raw, nil
}

func (h *service) GetAction(ctx context.Context, r *agentv1.GetActionRequest) (*agentv1.GetActionResponse, error) {
	if r == nil {
		return nil, run.ErrInvalidArgument
	}
	p := authenticated(ctx)
	lease, step, err := h.executionStep(p, r.Execution, r.Step)
	if err != nil {
		return nil, err
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		return nil, run.ErrProfileUnavailable
	}
	binding, err := api.GetAction(ctx, p, lease, step)
	if err != nil {
		return nil, err
	}
	raw, err := actionBindingJSON(binding)
	if err != nil {
		return nil, err
	}
	return &agentv1.GetActionResponse{ActionBindingJson: raw}, nil
}
func (h *service) AuthorizeAction(ctx context.Context, r *agentv1.AuthorizeActionRequest) (*agentv1.AuthorizeActionResponse, error) {
	if r == nil {
		return nil, run.ErrInvalidArgument
	}
	p := authenticated(ctx)
	lease, step, err := h.executionStep(p, r.Execution, r.Step)
	if err != nil {
		return nil, err
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		return nil, run.ErrProfileUnavailable
	}
	binding, err := api.AuthorizeAction(ctx, p, lease, step)
	if err != nil {
		return nil, err
	}
	raw, err := actionBindingJSON(binding)
	if err != nil {
		return nil, err
	}
	return &agentv1.AuthorizeActionResponse{ActionBindingJson: raw}, nil
}

func actionKind(code agentv1.ActionCallKind) string {
	if code == agentv1.ActionCallKind_ACTION_CALL_KIND_RECEIPT_QUERY {
		return "receipt_query"
	}
	if code == agentv1.ActionCallKind_ACTION_CALL_KIND_ACTION_WRITE {
		return "action_write"
	}
	return ""
}
func actionReservation(c run.ActionCall) *agentv1.ActionCallReservation {
	kind := agentv1.ActionCallKind_ACTION_CALL_KIND_RECEIPT_QUERY
	if c.Kind == "action_write" {
		kind = agentv1.ActionCallKind_ACTION_CALL_KIND_ACTION_WRITE
	}
	return &agentv1.ActionCallReservation{PhysicalCallId: c.ID, OperationId: c.OperationID, AuthorizationHash: c.AuthorizationHash, Kind: kind,
		ReservedAt: timestamppb.New(c.ReservedAt), DispatchExpiresAt: timestamppb.New(c.DispatchExpiresAt), CallDeadline: timestamppb.New(c.Deadline)}
}
func (h *service) ReserveActionCall(ctx context.Context, r *agentv1.ReserveActionCallRequest) (*agentv1.ReserveActionCallResponse, error) {
	if r == nil || actionKind(r.Kind) == "" {
		return nil, run.ErrInvalidArgument
	}
	p := authenticated(ctx)
	lease, step, err := h.executionStep(p, r.Execution, r.Step)
	if err != nil {
		return nil, err
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		return nil, run.ErrProfileUnavailable
	}
	result, err := api.ReserveActionCall(ctx, p, run.ReserveActionCallRequest{Lease: lease, Step: step, ID: r.PhysicalCallId, Kind: actionKind(r.Kind), OperationID: r.OperationId, AuthorizationHash: r.AuthorizationHash})
	if err != nil {
		return nil, err
	}
	return &agentv1.ReserveActionCallResponse{Reservation: actionReservation(result.Call), NewlyReserved: result.NewlyReserved}, nil
}
func (h *service) ObserveActionCall(ctx context.Context, r *agentv1.ObserveActionCallRequest) (*agentv1.ObserveActionCallResponse, error) {
	if r == nil {
		return nil, run.ErrInvalidArgument
	}
	p := authenticated(ctx)
	lease, err := h.lease(p, r.Execution)
	if err != nil {
		return nil, err
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		return nil, run.ErrProfileUnavailable
	}
	result, err := api.ObserveActionCall(ctx, p, run.ObserveActionCallRequest{Lease: lease, ID: r.PhysicalCallId, TransportOutcome: r.TransportOutcome, ObservationHash: r.ObservationHash})
	if err != nil {
		return nil, err
	}
	return &agentv1.ObserveActionCallResponse{PhysicalCallId: result.ID}, nil
}
func (h *service) CompleteAction(ctx context.Context, r *agentv1.CompleteActionRequest) (*agentv1.CompleteActionResponse, error) {
	if r == nil || len(r.ReceiptJson) > 4096 {
		return nil, run.ErrInvalidArgument
	}
	p := authenticated(ctx)
	lease, step, err := h.executionStep(p, r.Execution, r.Step)
	if err != nil {
		return nil, err
	}
	var receipt business.ActionReceipt
	if jsonstrict.Decode(r.ReceiptJson, &receipt) != nil {
		return nil, run.ErrInvalidArgument
	}
	api, ok := h.api.(actionAPI)
	if !ok {
		return nil, run.ErrProfileUnavailable
	}
	result, err := api.CompleteAction(ctx, p, run.CompleteActionRequest{Lease: lease, Step: step, Receipt: receipt, PhysicalCallID: r.PhysicalCallId})
	if err != nil {
		return nil, err
	}
	accepted, err := acceptedToWire(result.AcceptedStep)
	if err != nil {
		return nil, err
	}
	return &agentv1.CompleteActionResponse{AcceptedStep: accepted, CursorVersion: result.CursorVersion, State: stateToWire(result.State), AttemptClosed: result.AttemptClosed}, nil
}
