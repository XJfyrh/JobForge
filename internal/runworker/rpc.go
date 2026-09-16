package runworker

import (
	"context"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runclock"
	v2 "github.com/xjfyrh/jobforge/internal/runprotocol/v2"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

const controlTimeout = 2 * time.Second

// confirmFact repeats only an identical idempotent control fact, never HTTP.
// Both tries share two seconds; the first has at most half that budget.
func confirmFact[T any](ctx context.Context, invoke func(context.Context) (T, error)) (T, error) {
	bounded, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	first, firstCancel := context.WithTimeout(bounded, time.Second)
	value, err := invoke(first)
	firstCancel()
	if err == nil || bounded.Err() != nil || !uncertainRPC(err) {
		return value, err
	}
	return invoke(bounded)
}

func uncertainRPC(err error) bool {
	return status.Code(err) == codes.Unavailable || status.Code(err) == codes.DeadlineExceeded || status.Code(err) == codes.Canceled
}

func rpcReason(err error) string {
	for _, item := range status.Convert(err).Details() {
		if detail, ok := item.(*errdetails.ErrorInfo); ok && detail.Domain == "jobforge.agent.v1" {
			switch detail.Reason {
			case "STOP_REQUESTED", "CANCEL_REQUESTED", "STALE_LEASE", "UNAUTHORIZED", "FORBIDDEN", "ALREADY_TERMINAL",
				"BUDGET_EXHAUSTED", "PROFILE_UNAVAILABLE", "INVALID_ARGUMENT", "CHECKPOINT_TOO_LARGE", "MODEL_PROTOCOL_ERROR":
				return detail.Reason
			}
		}
	}
	return ""
}

func stopsAuthority(reason string) bool {
	switch reason {
	case "STOP_REQUESTED", "CANCEL_REQUESTED", "STALE_LEASE", "UNAUTHORIZED", "FORBIDDEN", "ALREADY_TERMINAL":
		return true
	default:
		return false
	}
}

func stampDeadline(start, received int64, observed, expires *timestamppb.Timestamp, limit time.Duration) (int64, error) {
	if observed == nil || expires == nil || observed.CheckValid() != nil || expires.CheckValid() != nil {
		return 0, runclock.ErrClock
	}
	return runclock.FromAuthority(start, received, observed.AsTime(), expires.AsTime(), limit)
}

func subcall(value string) agentv1.Subcall {
	return map[string]agentv1.Subcall{
		"get_order": agentv1.Subcall_SUBCALL_GET_ORDER, "get_delivery": agentv1.Subcall_SUBCALL_GET_DELIVERY,
		"profile_version": agentv1.Subcall_SUBCALL_PROFILE_VERSION, "profile_tags": agentv1.Subcall_SUBCALL_PROFILE_TAGS,
		"query_embedding": agentv1.Subcall_SUBCALL_QUERY_EMBEDDING, "search_policy": agentv1.Subcall_SUBCALL_SEARCH_POLICY,
		"chat": agentv1.Subcall_SUBCALL_CHAT,
	}[value]
}

func usageToWire(u *v2.Usage) *agentv1.UsageReport {
	if u == nil {
		return nil
	}
	return &agentv1.UsageReport{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedInputTokens: u.CachedInputTokens,
		ReceiptHash: u.ReceiptHash, UsageHash: u.UsageHash}
}

func domainStep(s *agentv1.StepIdentity, kind string) run.StepIdentity {
	return run.StepIdentity{ID: s.StepId, Sequence: int64(s.Sequence), Kind: kind, CursorVersion: s.CursorVersion,
		InputHash: s.InputHash, ProfileID: s.ProfileId, ProfileHash: s.ProfileHash, SnapshotID: s.SnapshotId, SnapshotHash: s.SnapshotHash}
}

func matchingReservation(reservation *agentv1.CallReservation, intent v2.Frame, id, price string) bool {
	return reservation != nil && reservation.PhysicalCallId == id && reservation.ToolInvocationId == intent.ToolInvocationID &&
		reservation.Subcall == subcall(intent.Subcall) && reservation.ParameterHash == intent.ParameterHash && reservation.PriceHash == price
}

func reportAuditHash(report *v2.Frame) string {
	if report.ProviderAudit == nil {
		return ""
	}
	return report.ProviderAudit.AuditHash
}

func persistedReportMatches(response *agentv1.SettleUsageResponse, call *callRecord) bool {
	return !response.ReportConflict && response.PersistedReportHash == call.report.ReportHash &&
		response.PersistedAuditHash == reportAuditHash(call.report) &&
		response.Reservation.PersistedReportHash == response.PersistedReportHash &&
		response.Reservation.PersistedAuditHash == response.PersistedAuditHash &&
		response.Reservation.ExecutionBindingHash == call.reservation.ExecutionBindingHash
}
