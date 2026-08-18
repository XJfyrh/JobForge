package grpc

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/xjfyrh/jobforge/internal/domain"
	workerv1 "github.com/xjfyrh/jobforge/proto/jobforge/worker/v1"
)

// domainStatusError builds the Worker API's standard status plus exactly one
// stable DomainErrorDetail (PRD v0.5 FR-906, ADR-0002).
func domainStatusError(code domain.ErrorCode, message string) error {
	if !knownDomainCode(code) {
		code = domain.CodeInternal
		message = "internal error"
	}
	return statusErrorWithDomainDetail(
		grpcCodeForDomain(code),
		code,
		domainCodeRetryable(code),
		message,
	)
}

func knownDomainCode(code domain.ErrorCode) bool {
	switch code {
	case domain.CodeInvalidArgument,
		domain.CodeUnauthorized,
		domain.CodeForbidden,
		domain.CodeNotFound,
		domain.CodeConflict,
		domain.CodeAlreadyTerminal,
		domain.CodeStaleLease,
		domain.CodeCancelRequested,
		domain.CodeQueueOverloaded,
		domain.CodeInvalidTransition,
		domain.CodeInternal:
		return true
	default:
		return false
	}
}

func statusErrorWithDomainDetail(
	grpcCode codes.Code,
	domainCode domain.ErrorCode,
	retryable bool,
	message string,
) error {
	withDetail, err := status.New(grpcCode, message).WithDetails(&workerv1.DomainErrorDetail{
		Code:      string(domainCode),
		Retryable: retryable,
	})
	if err != nil {
		// A generated, field-only protobuf detail is always serializable. Treat
		// an impossible failure as INTERNAL without leaking the cause.
		return status.Error(codes.Internal, "internal error")
	}
	return withDetail.Err()
}

// mapError converts domain errors to gRPC status errors per ADR-0002. Unknown
// implementation errors are hidden behind retryable INTERNAL.
func mapError(err error) error {
	var domainError *domain.Error
	if !errors.As(err, &domainError) {
		return domainStatusError(domain.CodeInternal, "internal error")
	}
	return domainStatusError(domainError.Code, domainError.Message)
}

func grpcCodeForDomain(code domain.ErrorCode) codes.Code {
	switch code {
	case domain.CodeInvalidArgument:
		return codes.InvalidArgument
	case domain.CodeUnauthorized:
		return codes.Unauthenticated
	case domain.CodeForbidden:
		return codes.PermissionDenied
	case domain.CodeNotFound:
		return codes.NotFound
	case domain.CodeConflict:
		return codes.AlreadyExists
	case domain.CodeAlreadyTerminal,
		domain.CodeStaleLease,
		domain.CodeCancelRequested,
		domain.CodeInvalidTransition:
		return codes.FailedPrecondition
	case domain.CodeQueueOverloaded:
		return codes.ResourceExhausted
	case domain.CodeInternal:
		return codes.Internal
	default:
		return codes.Internal
	}
}

func domainCodeRetryable(code domain.ErrorCode) bool {
	return code == domain.CodeQueueOverloaded || code == domain.CodeInternal
}

// ensureDomainErrorDetail protects the interceptor boundary: errors returned
// by internal/custom handlers receive a stable fallback detail without
// changing their standard transport code.
func ensureDomainErrorDetail(err error) error {
	if err == nil {
		return nil
	}
	grpcStatus := status.Convert(err)
	for _, detail := range grpcStatus.Details() {
		if _, ok := detail.(*workerv1.DomainErrorDetail); ok {
			return err
		}
	}

	grpcCode := grpcStatus.Code()
	if errors.Is(err, context.DeadlineExceeded) {
		grpcCode = codes.DeadlineExceeded
	} else if errors.Is(err, context.Canceled) {
		grpcCode = codes.Canceled
	}
	domainCode := fallbackDomainCode(grpcCode)
	return statusErrorWithDomainDetail(
		grpcCode,
		domainCode,
		domainCodeRetryable(domainCode),
		grpcStatus.Message(),
	)
}

func fallbackDomainCode(code codes.Code) domain.ErrorCode {
	switch code {
	case codes.InvalidArgument:
		return domain.CodeInvalidArgument
	case codes.Unauthenticated:
		return domain.CodeUnauthorized
	case codes.PermissionDenied:
		return domain.CodeForbidden
	case codes.NotFound:
		return domain.CodeNotFound
	case codes.AlreadyExists:
		return domain.CodeConflict
	case codes.ResourceExhausted:
		return domain.CodeQueueOverloaded
	default:
		return domain.CodeInternal
	}
}
