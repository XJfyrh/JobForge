package domain

import (
	"errors"
	"fmt"
)

// Domain sentinel errors. These are stable error values that transport layers
// map to HTTP status codes and gRPC codes per ADR-0002.
var (
	// ErrNotFound indicates the requested job does not exist or does not
	// belong to the current tenant. Maps to HTTP 404 / gRPC NOT_FOUND.
	ErrNotFound = errors.New("job not found")

	// ErrAlreadyTerminal indicates the job is in a terminal state and the
	// requested operation is not applicable. Maps to HTTP 409 / gRPC
	// FAILED_PRECONDITION.
	ErrAlreadyTerminal = errors.New("job already in terminal state")

	// ErrStaleLease indicates the fencing token or lease owner does not match.
	// The write is rejected to prevent a stale Worker from overwriting newer
	// state. Maps to HTTP 409 / gRPC FAILED_PRECONDITION.
	ErrStaleLease = errors.New("stale lease: fencing token or owner mismatch")

	// ErrCancelRequested indicates the job has entered cancelling state and
	// Complete is rejected. Maps to HTTP 409 / gRPC FAILED_PRECONDITION.
	ErrCancelRequested = errors.New("cancel requested: complete rejected")

	// ErrQueueOverloaded indicates the queue has exceeded its hard threshold
	// and new submissions are rejected. Maps to HTTP 429 / gRPC
	// RESOURCE_EXHAUSTED.
	ErrQueueOverloaded = errors.New("queue overloaded: submission rejected")

	// ErrInvalidTransition indicates an attempted state transition that is not
	// allowed by the state machine. Maps to HTTP 409 / gRPC
	// FAILED_PRECONDITION.
	ErrInvalidTransition = errors.New("invalid state transition")

	// ErrInvalidArgument indicates request parameters are invalid (unregistered
	// type, payload too large, missing fields). Maps to HTTP 400 / gRPC
	// INVALID_ARGUMENT.
	ErrInvalidArgument = errors.New("invalid argument")

	// ErrForbidden indicates that a caller attempted to exceed its declared
	// capability. Maps to HTTP 403 / gRPC PERMISSION_DENIED.
	ErrForbidden = errors.New("forbidden")

	// ErrConflict indicates an idempotency key conflict where the same key was
	// used with different parameters. Maps to HTTP 409 / gRPC ALREADY_EXISTS.
	ErrConflict = errors.New("idempotency key conflict")
)

// ErrorCode is a machine-readable error code string aligned with ADR-0002.
type ErrorCode string

// Error code constants aligned with ADR-0002. Used in DomainError.Code and
// mapped to HTTP/gRPC codes at the transport boundary.
const (
	CodeInvalidArgument   ErrorCode = "INVALID_ARGUMENT"
	CodeUnauthorized      ErrorCode = "UNAUTHORIZED"
	CodeForbidden         ErrorCode = "FORBIDDEN"
	CodeNotFound          ErrorCode = "NOT_FOUND"
	CodeConflict          ErrorCode = "CONFLICT"
	CodeAlreadyTerminal   ErrorCode = "ALREADY_TERMINAL"
	CodeStaleLease        ErrorCode = "STALE_LEASE"
	CodeCancelRequested   ErrorCode = "CANCEL_REQUESTED"
	CodeQueueOverloaded   ErrorCode = "QUEUE_OVERLOADED"
	CodeInvalidTransition ErrorCode = "INVALID_TRANSITION"
	CodeInternal          ErrorCode = "INTERNAL"
)

// Error wraps a sentinel error with a stable machine-readable code and
// an optional human-readable message. Transport layers use Code for mapping
// and Message for the response body.
type Error struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error {
	return e.Err
}

// NewError creates an Error with the given code, underlying sentinel
// and formatted message.
func NewError(code ErrorCode, sentinel error, format string, args ...any) *Error {
	return &Error{
		Code:    code,
		Message: fmt.Sprintf(format, args...),
		Err:     sentinel,
	}
}

// IsRetryableError reports whether a domain error indicates a transient
// condition that may succeed on retry. Only QUEUE_OVERLOADED and INTERNAL
// are retryable from the control-plane perspective.
func IsRetryableError(err error) bool {
	de, ok := errors.AsType[*Error](err)
	if ok {
		return de.Code == CodeQueueOverloaded || de.Code == CodeInternal
	}
	return false
}
