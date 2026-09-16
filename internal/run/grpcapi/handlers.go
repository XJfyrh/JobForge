package grpcapi

import (
	"context"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func (h *service) Register(ctx context.Context, request *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	if request == nil || !run.ValidUUID(request.StartupId) || !run.ValidIdentifier(request.Version) {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	session, err := h.api.Register(ctx, principal, request.StartupId, request.Version)
	if err != nil {
		return nil, err
	}
	if session.WorkerID != principal || !run.ValidUUID(session.ID) {
		return nil, run.ErrInternal
	}
	observedAt, err := authorityTimeToWire(session.AuthorityObservedAt)
	if err != nil {
		return nil, err
	}
	worker := h.workers[principal]
	return &agentv1.RegisterResponse{Session: &agentv1.SessionIdentity{WorkerId: principal, SessionId: session.ID},
		ExpiresAt: timestamppb.New(session.ExpiresAt), HeartbeatInterval: durationpb.New(5 * time.Second),
		ProfileIds: slices.Clone(worker.ProfileIDs), Capacity: int32(worker.Capacity), AuthorityObservedAt: observedAt}, nil
}

func (h *service) Claim(ctx context.Context, request *agentv1.ClaimRequest) (*agentv1.ClaimResponse, error) {
	if request == nil {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	sessionID, err := h.session(principal, request.Session)
	if err != nil {
		return nil, err
	}
	claimed, err := h.api.Claim(ctx, principal, sessionID)
	if err != nil {
		return nil, err
	}
	if claimed == nil {
		return &agentv1.ClaimResponse{}, nil
	}
	checkpoint, err := checkpointToWire(claimed.Checkpoint)
	if err != nil {
		return nil, err
	}
	execution := leaseToWire(claimed.Lease)
	if _, err = h.lease(principal, execution); err != nil || execution.Session.SessionId != sessionID ||
		claimed.Checkpoint.Run.LeaseUntil == nil || claimed.Checkpoint.Run.AttemptDeadline == nil {
		return nil, run.ErrInternal
	}
	observedAt, err := authorityTimeToWire(claimed.AuthorityObservedAt)
	if err != nil {
		return nil, err
	}
	return &agentv1.ClaimResponse{Lease: &agentv1.RunLease{Execution: execution,
		LeaseUntil: optionalTime(claimed.Checkpoint.Run.LeaseUntil), AttemptDeadline: optionalTime(claimed.Checkpoint.Run.AttemptDeadline),
		RunDeadline: timestamppb.New(claimed.Checkpoint.Run.RunDeadline), Checkpoint: checkpoint, AuthorityObservedAt: observedAt}}, nil
}

func (h *service) Heartbeat(ctx context.Context, request *agentv1.HeartbeatRequest) (*agentv1.HeartbeatResponse, error) {
	if request == nil || (request.Execution != nil && !proto.Equal(request.Session, request.Execution.Session)) {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	sessionID, err := h.session(principal, request.Session)
	if err != nil {
		return nil, err
	}
	if request.Execution == nil {
		session, err := h.api.HeartbeatSession(ctx, principal, principal, sessionID)
		if err != nil {
			return nil, err
		}
		if session.ID != sessionID || session.WorkerID != principal {
			return nil, run.ErrInternal
		}
		observedAt, err := authorityTimeToWire(session.AuthorityObservedAt)
		if err != nil {
			return nil, err
		}
		return &agentv1.HeartbeatResponse{Signal: agentv1.ControlSignal_CONTROL_SIGNAL_CONTINUE,
			SessionExpiresAt: timestamppb.New(session.ExpiresAt), AuthorityObservedAt: observedAt}, nil
	}
	lease, err := h.lease(principal, request.Execution)
	if err != nil {
		return nil, err
	}
	result, err := h.api.HeartbeatExecution(ctx, principal, lease)
	if err != nil {
		return nil, err
	}
	signal := agentv1.ControlSignal_CONTROL_SIGNAL_STOP
	if result.Continue {
		signal = agentv1.ControlSignal_CONTROL_SIGNAL_CONTINUE
	}
	if result.StopReason != "" && result.StopReason != run.StopCancel && result.StopReason != run.StopRunDeadline && result.StopReason != run.StopAttemptTimeout {
		return nil, run.ErrInternal
	}
	observedAt, err := authorityTimeToWire(result.AuthorityObservedAt)
	if err != nil {
		return nil, err
	}
	return &agentv1.HeartbeatResponse{Signal: signal, LeaseUntil: timestamppb.New(result.LeaseUntil),
		StopReason: result.StopReason, SessionExpiresAt: timestamppb.New(result.SessionExpiresAt), AuthorityObservedAt: observedAt}, nil
}

func (h *service) GetCheckpoint(ctx context.Context, request *agentv1.GetCheckpointRequest) (*agentv1.GetCheckpointResponse, error) {
	if request == nil {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	lease, err := h.lease(principal, request.Execution)
	if err != nil {
		return nil, err
	}
	checkpoint, err := h.api.GetCheckpoint(ctx, principal, lease)
	if err != nil {
		return nil, err
	}
	converted, err := checkpointToWire(checkpoint)
	if err != nil {
		return nil, err
	}
	return &agentv1.GetCheckpointResponse{Checkpoint: converted}, nil
}

func (h *service) BeginTool(ctx context.Context, request *agentv1.BeginToolRequest) (*agentv1.BeginToolResponse, error) {
	if request == nil || !run.ValidUUID(request.ToolInvocationId) {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	lease, step, err := h.executionStep(principal, request.Execution, request.Step)
	if err != nil {
		return nil, err
	}
	result, err := h.api.BeginTool(ctx, principal, run.BeginToolRequest{Lease: lease, Step: step, ToolInvocationID: request.ToolInvocationId})
	if err != nil {
		return nil, err
	}
	if result.ToolInvocationID != request.ToolInvocationId {
		return nil, run.ErrInternal
	}
	return &agentv1.BeginToolResponse{ToolInvocationId: result.ToolInvocationID, NewlyStarted: result.NewlyStarted}, nil
}

func (h *service) ReserveCall(ctx context.Context, request *agentv1.ReserveCallRequest) (*agentv1.ReserveCallResponse, error) {
	if request == nil || !run.ValidUUID(request.PhysicalCallId) || !run.ValidHash(request.ParameterHash) ||
		!run.ValidHash(request.PriceHash) || subcallNames[request.Subcall] == "" ||
		(request.ToolInvocationId != "" && !run.ValidUUID(request.ToolInvocationId)) {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	lease, step, err := h.executionStep(principal, request.Execution, request.Step)
	if err != nil {
		return nil, err
	}
	result, err := h.api.ReserveCall(ctx, principal, run.ReserveCallRequest{Lease: lease, Step: step,
		PhysicalCallID: request.PhysicalCallId, ToolInvocationID: request.ToolInvocationId,
		Subcall: subcallNames[request.Subcall], ParameterHash: request.ParameterHash, PriceHash: request.PriceHash})
	if err != nil {
		return nil, err
	}
	reservation, err := reservationToWire(result.Reservation)
	if err != nil || reservation.PhysicalCallId != request.PhysicalCallId {
		return nil, run.ErrInternal
	}
	return &agentv1.ReserveCallResponse{Reservation: reservation, NewlyReserved: result.NewlyReserved}, nil
}

func (h *service) ObserveCall(ctx context.Context, request *agentv1.ObserveCallRequest) (*agentv1.ObserveCallResponse, error) {
	if request == nil {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	lease, step, err := h.executionStep(principal, request.Execution, request.Step)
	if err != nil {
		return nil, err
	}
	usage, err := usageFromWire(request.Usage)
	if err != nil {
		return nil, err
	}
	transport := map[agentv1.TransportOutcome]string{agentv1.TransportOutcome_TRANSPORT_OUTCOME_RESPONSE: "response", agentv1.TransportOutcome_TRANSPORT_OUTCOME_UNKNOWN: "unknown"}
	business := map[agentv1.BusinessOutcome]string{agentv1.BusinessOutcome_BUSINESS_OUTCOME_ACCEPTED: "accepted", agentv1.BusinessOutcome_BUSINESS_OUTCOME_REJECTED: "rejected", agentv1.BusinessOutcome_BUSINESS_OUTCOME_UNKNOWN: "unknown"}
	command := run.ObserveCallRequest{Lease: lease, Step: step, PhysicalCallID: request.PhysicalCallId,
		TransportOutcome: transport[request.TransportOutcome], HTTPStatus: int(request.HttpStatus),
		ErrorCode: request.ErrorCode, BusinessOutcome: business[request.BusinessOutcome], UsageKnown: request.UsageKnown, Usage: usage}
	if err = command.Validate(); err != nil {
		return nil, err
	}
	result, err := h.api.ObserveCall(ctx, principal, command)
	if err != nil {
		return nil, err
	}
	reservation, err := reservationToWire(result)
	if err != nil || reservation.PhysicalCallId != request.PhysicalCallId {
		return nil, run.ErrInternal
	}
	return &agentv1.ObserveCallResponse{Reservation: reservation}, nil
}

func (h *service) SettleUsage(ctx context.Context, request *agentv1.SettleUsageRequest) (*agentv1.SettleUsageResponse, error) {
	if request == nil || !run.ValidUUID(request.PhysicalCallId) || request.Usage == nil {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	lease, err := h.lease(principal, request.Execution)
	if err != nil {
		return nil, err
	}
	usage, err := usageFromWire(request.Usage)
	if err != nil {
		return nil, err
	}
	// Deliberately do not heartbeat, read checkpoint or require a live lease here.
	result, err := h.api.SettleUsage(ctx, principal, run.SettleUsageRequest{Lease: lease, PhysicalCallID: request.PhysicalCallId, Usage: *usage})
	if err != nil {
		return nil, err
	}
	reservation, err := reservationToWire(result.Reservation)
	if err != nil || reservation.PhysicalCallId != request.PhysicalCallId {
		return nil, run.ErrInternal
	}
	return &agentv1.SettleUsageResponse{Reservation: reservation, NewlySettled: result.NewlySettled}, nil
}

func (h *service) CommitStep(ctx context.Context, request *agentv1.CommitStepRequest) (*agentv1.CommitStepResponse, error) {
	if request == nil || !run.ValidHash(request.CommitHash) {
		return nil, run.ErrInvalidArgument
	}
	if err := run.ValidateStepJSON(request.ResultJson, 16*1024); err != nil {
		return nil, err
	}
	principal := authenticated(ctx)
	lease, step, err := h.executionStep(principal, request.Execution, request.Step)
	if err != nil {
		return nil, err
	}
	result, err := h.api.CommitStep(ctx, principal, run.CommitStepRequest{Lease: lease, Step: step,
		ResultJSON: slices.Clone(request.ResultJson), CommitHash: request.CommitHash})
	if err != nil {
		return nil, err
	}
	accepted, err := acceptedToWire(result.AcceptedStep)
	if err != nil || !result.State.Valid() || result.CursorVersion < 0 || result.CursorVersion > 32 {
		return nil, run.ErrInternal
	}
	response := &agentv1.CommitStepResponse{AcceptedStep: accepted, CursorVersion: result.CursorVersion,
		State: stateToWire(result.State), AttemptClosed: result.AttemptClosed}
	if result.NextStep != nil {
		response.NextStep = stepToWire(*result.NextStep)
	}
	return response, nil
}

func (h *service) FailAttempt(ctx context.Context, request *agentv1.FailAttemptRequest) (*agentv1.FailAttemptResponse, error) {
	if request == nil || !slices.Contains([]string{"INVALID_ARGUMENT", "PROFILE_UNAVAILABLE", "BUDGET_EXHAUSTED", "DEPENDENCY_UNAVAILABLE",
		"EXECUTOR_PROTOCOL_ERROR", "MODEL_PROTOCOL_ERROR", "MODEL_UNSUPPORTED", "CHECKPOINT_TOO_LARGE", "TIMEOUT"}, request.ErrorCode) {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	lease, step, err := h.executionStep(principal, request.Execution, request.Step)
	if err != nil {
		return nil, err
	}
	result, err := h.api.FailExecution(ctx, principal, lease, step, request.ErrorCode)
	if err != nil {
		return nil, err
	}
	if !result.State.Valid() || result.RecoveryCount < 0 || result.RecoveryCount > 3 {
		return nil, run.ErrInternal
	}
	return &agentv1.FailAttemptResponse{State: stateToWire(result.State), RecoveryCount: int32(result.RecoveryCount), RetryAt: optionalTime(result.NextAttemptAt)}, nil
}

func (h *service) AcknowledgeStopped(ctx context.Context, request *agentv1.AcknowledgeStoppedRequest) (*agentv1.AcknowledgeStoppedResponse, error) {
	if request == nil {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	lease, err := h.lease(principal, request.Execution)
	if err != nil {
		return nil, err
	}
	result, err := h.api.AcknowledgeStop(ctx, principal, lease)
	if err != nil {
		return nil, err
	}
	if !result.Run.State.Valid() || !validAttemptOutcome(result.AttemptOutcome) {
		return nil, run.ErrInternal
	}
	return &agentv1.AcknowledgeStoppedResponse{State: stateToWire(result.Run.State), AttemptOutcome: result.AttemptOutcome}, nil
}

func (h *service) GetAcceptedCommit(ctx context.Context, request *agentv1.GetAcceptedCommitRequest) (*agentv1.GetAcceptedCommitResponse, error) {
	if request == nil || !run.ValidUUID(request.StepId) {
		return nil, run.ErrInvalidArgument
	}
	principal := authenticated(ctx)
	lease, err := h.lease(principal, request.Execution)
	if err != nil {
		return nil, err
	}
	result, err := h.api.GetAcceptedCommit(ctx, principal, lease, request.StepId)
	if err != nil {
		return nil, err
	}
	if !result.State.Valid() || !validAttemptOutcome(result.AttemptOutcome) || result.Found != (result.AcceptedStep != nil) {
		return nil, run.ErrInternal
	}
	response := &agentv1.GetAcceptedCommitResponse{Found: result.Found, AttemptOutcome: result.AttemptOutcome, State: stateToWire(result.State)}
	if result.AcceptedStep != nil {
		response.AcceptedStep, err = acceptedToWire(*result.AcceptedStep)
		if err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (h *service) executionStep(principal string, execution *agentv1.ExecutionIdentity, identity *agentv1.StepIdentity) (run.Lease, run.StepIdentity, error) {
	lease, err := h.lease(principal, execution)
	if err != nil {
		return run.Lease{}, run.StepIdentity{}, err
	}
	step, err := h.step(principal, identity)
	return lease, step, err
}

func validAttemptOutcome(value string) bool {
	return slices.Contains([]string{"", "cancelled", "failed_terminal", "failed_retry", "lease_expired_terminal", "lease_expired_retry", "yielded_approval", "succeeded_no_action", "succeeded"}, value)
}
