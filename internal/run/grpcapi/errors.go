package grpcapi

import (
	"context"
	"errors"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/run"
)

func mapError(err error) error {
	if err == nil {
		return nil
	}
	code, reason := codes.Internal, string(run.ErrInternal)
	var domain run.ErrorCode
	if errors.As(err, &domain) {
		switch domain {
		case run.ErrInvalidArgument, run.ErrCheckpointTooLarge, run.ErrModelProtocol:
			code, reason = codes.InvalidArgument, string(domain)
		case run.ErrUnauthorized:
			code, reason = codes.Unauthenticated, string(domain)
		case run.ErrForbidden:
			code, reason = codes.PermissionDenied, string(domain)
		case run.ErrNotFound:
			code, reason = codes.NotFound, string(domain)
		case run.ErrConflict, run.ErrAlreadyTerminal, run.ErrInvalidTransition, run.ErrStaleLease,
			run.ErrCancelRequested, run.ErrStopRequested, run.ErrStepConflict, run.ErrCallConflict,
			run.ErrBudgetExhausted, run.ErrProfileUnavailable, run.ErrCallSettlementExpired:
			code, reason = codes.FailedPrecondition, string(domain)
		case run.ErrQueueOverloaded:
			code, reason = codes.ResourceExhausted, string(domain)
		case run.ErrDependencyUnavailable:
			code, reason = codes.Unavailable, string(domain)
		}
	} else if errors.Is(err, context.Canceled) {
		code, reason = codes.Canceled, "REQUEST_CANCELLED"
	} else if errors.Is(err, context.DeadlineExceeded) {
		code, reason = codes.DeadlineExceeded, "REQUEST_DEADLINE_EXCEEDED"
	}
	// Never echo SQL, credentials, provider bodies or arbitrary status metadata.
	safe, detailErr := status.New(code, reason).WithDetails(&errdetails.ErrorInfo{
		Reason: reason, Domain: "jobforge.agent.v1",
	})
	if detailErr != nil {
		return status.Error(codes.Internal, string(run.ErrInternal))
	}
	return safe.Err()
}
