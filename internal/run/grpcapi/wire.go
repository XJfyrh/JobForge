package grpcapi

import (
	"slices"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

var stepNames = map[agentv1.StepKind]string{
	agentv1.StepKind_STEP_KIND_READ_TICKET: "read_ticket", agentv1.StepKind_STEP_KIND_GET_ORDER: "get_order",
	agentv1.StepKind_STEP_KIND_GET_DELIVERY: "get_delivery", agentv1.StepKind_STEP_KIND_SEARCH_POLICY: "search_policy",
	agentv1.StepKind_STEP_KIND_MODEL_PROPOSAL: "model_proposal", agentv1.StepKind_STEP_KIND_PROTOCOL_CORRECTION: "protocol_correction",
	agentv1.StepKind_STEP_KIND_SUBMIT_PROPOSAL: "submit_proposal",
}

var subcallNames = map[agentv1.Subcall]run.Subcall{
	agentv1.Subcall_SUBCALL_GET_ORDER: run.SubcallGetOrder, agentv1.Subcall_SUBCALL_GET_DELIVERY: run.SubcallGetDelivery,
	agentv1.Subcall_SUBCALL_PROFILE_VERSION: run.SubcallProfileVersion, agentv1.Subcall_SUBCALL_PROFILE_TAGS: run.SubcallProfileTags,
	agentv1.Subcall_SUBCALL_QUERY_EMBEDDING: run.SubcallQueryEmbedding, agentv1.Subcall_SUBCALL_SEARCH_POLICY: run.SubcallSearchPolicy,
	agentv1.Subcall_SUBCALL_CHAT: run.SubcallChat,
}

func (h *service) session(principal string, value *agentv1.SessionIdentity) (string, error) {
	if value == nil || !run.ValidUUID(value.SessionId) || !run.ValidIdentifier(value.WorkerId) {
		return "", run.ErrInvalidArgument
	}
	if value.WorkerId != principal {
		return "", run.ErrForbidden
	}
	return value.SessionId, nil
}

func (h *service) lease(principal string, value *agentv1.ExecutionIdentity) (run.Lease, error) {
	if value == nil || !run.ValidIdentifier(value.TenantId) || !run.ValidUUID(value.RunId) ||
		value.AttemptNo < 1 || value.AttemptNo > run.MaxSafeInteger || value.FencingToken < 1 || value.FencingToken > run.MaxSafeInteger {
		return run.Lease{}, run.ErrInvalidArgument
	}
	sessionID, err := h.session(principal, value.Session)
	if err != nil {
		return run.Lease{}, err
	}
	if !slices.Contains(h.workers[principal].Tenants, value.TenantId) {
		return run.Lease{}, run.ErrForbidden
	}
	return run.Lease{TenantID: value.TenantId, RunID: value.RunId, WorkerID: principal,
		SessionID: sessionID, AttemptNo: value.AttemptNo, FencingToken: value.FencingToken}, nil
}

func (h *service) step(principal string, value *agentv1.StepIdentity) (run.StepIdentity, error) {
	if value == nil || !run.ValidUUID(value.StepId) || value.Sequence < 1 || value.Sequence > 32 ||
		value.CursorVersion < 0 || value.CursorVersion >= 32 || !run.ValidHash(value.InputHash) ||
		!run.ValidIdentifier(value.ProfileId) || !run.ValidHash(value.ProfileHash) ||
		!run.ValidUUID(value.SnapshotId) || !run.ValidHash(value.SnapshotHash) || stepNames[value.Kind] == "" {
		return run.StepIdentity{}, run.ErrInvalidArgument
	}
	if !slices.Contains(h.workers[principal].ProfileIDs, value.ProfileId) {
		return run.StepIdentity{}, run.ErrProfileUnavailable
	}
	return run.StepIdentity{ID: value.StepId, Sequence: int64(value.Sequence), Kind: stepNames[value.Kind],
		CursorVersion: value.CursorVersion, InputHash: value.InputHash, ProfileID: value.ProfileId,
		ProfileHash: value.ProfileHash, SnapshotID: value.SnapshotId, SnapshotHash: value.SnapshotHash}, nil
}

func usageFromWire(value *agentv1.UsageReport) (*run.UsageReport, error) {
	if value == nil {
		return nil, nil
	}
	usage := &run.UsageReport{InputTokens: value.InputTokens, OutputTokens: value.OutputTokens,
		CachedInputTokens: value.CachedInputTokens, ReceiptHash: value.ReceiptHash, UsageHash: value.UsageHash}
	if err := usage.Validate(); err != nil {
		return nil, err
	}
	return usage, nil
}

func stateToWire(state run.State) agentv1.RunState {
	switch state {
	case run.Ready:
		return agentv1.RunState_RUN_STATE_READY
	case run.Running:
		return agentv1.RunState_RUN_STATE_RUNNING
	case run.Stopping:
		return agentv1.RunState_RUN_STATE_STOPPING
	case run.RetryWait:
		return agentv1.RunState_RUN_STATE_RETRY_WAIT
	case run.AwaitingApproval:
		return agentv1.RunState_RUN_STATE_AWAITING_APPROVAL
	case run.Succeeded:
		return agentv1.RunState_RUN_STATE_SUCCEEDED
	case run.Failed:
		return agentv1.RunState_RUN_STATE_FAILED
	case run.Cancelled:
		return agentv1.RunState_RUN_STATE_CANCELLED
	default:
		return agentv1.RunState_RUN_STATE_UNSPECIFIED
	}
}

func stepToWire(value run.StepIdentity) *agentv1.StepIdentity {
	var kind agentv1.StepKind
	for code, name := range stepNames {
		if name == value.Kind {
			kind = code
			break
		}
	}
	return &agentv1.StepIdentity{StepId: value.ID, Sequence: int32(value.Sequence), Kind: kind,
		CursorVersion: value.CursorVersion, InputHash: value.InputHash, ProfileId: value.ProfileID,
		ProfileHash: value.ProfileHash, SnapshotId: value.SnapshotID, SnapshotHash: value.SnapshotHash}
}

func leaseToWire(value run.Lease) *agentv1.ExecutionIdentity {
	return &agentv1.ExecutionIdentity{TenantId: value.TenantID, RunId: value.RunID,
		Session:   &agentv1.SessionIdentity{WorkerId: value.WorkerID, SessionId: value.SessionID},
		AttemptNo: value.AttemptNo, FencingToken: value.FencingToken}
}

func optionalTime(value *time.Time) *timestamppb.Timestamp {
	if value == nil {
		return nil
	}
	return timestamppb.New(*value)
}

func reservationToWire(value run.CallReservation) (*agentv1.CallReservation, error) {
	var subcall agentv1.Subcall
	for code, name := range subcallNames {
		if name == value.Subcall {
			subcall = code
			break
		}
	}
	if subcall == agentv1.Subcall_SUBCALL_UNSPECIFIED || !run.ValidUUID(value.PhysicalCallID) ||
		!run.ValidHash(value.ParameterHash) || !run.ValidHash(value.PriceHash) ||
		(value.ToolInvocationID != "" && !run.ValidUUID(value.ToolInvocationID)) {
		return nil, run.ErrInternal
	}
	for _, number := range []int64{value.Budget.InputTokens, value.Budget.OutputTokens, value.Budget.TotalTokens, value.Budget.CostMicroyuan} {
		if number < 0 || number > run.MaxSafeInteger {
			return nil, run.ErrInternal
		}
	}
	return &agentv1.CallReservation{PhysicalCallId: value.PhysicalCallID, ToolInvocationId: value.ToolInvocationID,
		Subcall: subcall, ParameterHash: value.ParameterHash, PriceHash: value.PriceHash,
		ReservedAt: timestamppb.New(value.ReservedAt), DispatchExpiresAt: timestamppb.New(value.DispatchExpiresAt),
		CallDeadline: timestamppb.New(value.CallDeadline), UsageKnown: value.UsageKnown,
		Budget: &agentv1.CallBudget{InputTokens: value.Budget.InputTokens, OutputTokens: value.Budget.OutputTokens,
			TotalTokens: value.Budget.TotalTokens, CostMicroyuan: value.Budget.CostMicroyuan}}, nil
}

func acceptedToWire(value run.AcceptedStep) (*agentv1.AcceptedStep, error) {
	if len(value.ResultJSON) > 16*1024 || !run.ValidHash(value.CommitHash) || len(value.ResultRef) > 128 {
		return nil, run.ErrInternal
	}
	return &agentv1.AcceptedStep{Step: stepToWire(value.Identity), CommitHash: value.CommitHash,
		ResultJson: slices.Clone(value.ResultJSON), ResultRef: value.ResultRef}, nil
}

func checkpointToWire(value run.Checkpoint) (*agentv1.Checkpoint, error) {
	if len(value.Steps) > 32 || value.Run.CursorVersion < 0 || value.Run.CursorVersion > 32 ||
		len(value.Snapshot.Ticket) > 8*1024 || len(value.Snapshot.VersionVector) > 8*1024 {
		return nil, run.ErrInternal
	}
	result := &agentv1.Checkpoint{CursorVersion: value.Run.CursorVersion,
		Snapshot: &agentv1.SnapshotBinding{TenantId: value.Snapshot.TenantID, TicketId: value.Snapshot.TicketID,
			SnapshotId: value.Snapshot.ID, SnapshotHash: value.Snapshot.ContentHash,
			VersionVectorJson: slices.Clone(value.Snapshot.VersionVector), TicketBindingJson: slices.Clone(value.Snapshot.Ticket),
			IndexId: value.Snapshot.IndexID, IndexProfileHash: value.Snapshot.IndexProfileHash}}
	if value.Run.State == run.Running {
		result.NextStep = stepToWire(run.StepIdentity{ID: value.Authority.NextStepID,
			Sequence: value.Run.CursorVersion + 1, Kind: value.Authority.NextStepKind,
			CursorVersion: value.Run.CursorVersion, InputHash: value.Authority.NextInputHash,
			ProfileID: value.Run.ProfileID, ProfileHash: value.Run.ProfileHash,
			SnapshotID: value.Run.SnapshotID, SnapshotHash: value.Run.SnapshotHash})
	}
	for _, step := range value.Steps {
		converted, err := acceptedToWire(run.AcceptedStep{Identity: run.StepIdentity{ID: step.ID,
			Sequence: step.Sequence, Kind: step.Kind, CursorVersion: step.Sequence - 1,
			InputHash: step.InputHash, ProfileID: value.Run.ProfileID, ProfileHash: step.ProfileHash,
			SnapshotID: value.Run.SnapshotID, SnapshotHash: step.SnapshotHash}, CommitHash: step.CommitHash,
			ResultJSON: step.Output, ResultRef: step.OutputRef})
		if err != nil {
			return nil, err
		}
		result.Steps = append(result.Steps, converted)
	}
	if proto.Size(result) > 256*1024 {
		return nil, run.ErrInternal
	}
	return result, nil
}
