// Package worker implements the Go Worker Runtime. It manages a concurrent
// goroutine pool that polls the Gateway for jobs, executes registered Handlers,
// maintains heartbeats, and reports results via gRPC.
//
// Only pre-registered Handlers may execute; unknown task types are rejected
// with a non-retryable error (PRD FR-101, security invariant).
package worker

import (
	"context"
	"fmt"
	"time"
)

// ClaimedJob represents a job lease granted by the Gateway via Poll RPC.
type ClaimedJob struct {
	ID           string
	Queue        string
	Type         string
	Payload      []byte
	Attempt      int
	MaxAttempts  int
	FencingToken int64
	LeaseUntil   time.Time
	Timeout      time.Duration
	TraceID      string
	// TraceContext is the serialized W3C TraceContext (traceparent) captured
	// at submission. Used to attach worker.execute spans to the submit trace.
	TraceContext string
}

// Handler executes a specific task type. Implementations must be safe for
// concurrent use. The context carries the execution deadline and cancel signal.
type Handler interface {
	// Execute runs the task. Return a resultRef (e.g. storage URI) on success.
	// Return an error to signal failure; wrap with RetryableError for retryable
	// failures.
	Execute(ctx context.Context, job *ClaimedJob) (resultRef string, err error)
}

// HandlerFunc is an adapter to allow ordinary functions as Handlers.
type HandlerFunc func(ctx context.Context, job *ClaimedJob) (string, error)

// Execute implements Handler.
func (f HandlerFunc) Execute(ctx context.Context, job *ClaimedJob) (string, error) {
	return f(ctx, job)
}

// Registry maps task types to their registered Handlers.
type Registry struct {
	handlers map[string]Handler
}

// NewRegistry creates an empty Handler registry.
func NewRegistry() *Registry {
	return &Registry{handlers: make(map[string]Handler)}
}

// Register adds a Handler for the given task type. Panics on duplicate
// registration to catch configuration errors at startup.
func (r *Registry) Register(taskType string, h Handler) {
	if _, exists := r.handlers[taskType]; exists {
		panic(fmt.Sprintf("worker: duplicate handler registration for type %q", taskType))
	}
	r.handlers[taskType] = h
}

// Lookup returns the Handler for a task type, or nil if not registered.
func (r *Registry) Lookup(taskType string) Handler {
	return r.handlers[taskType]
}

// Types returns all registered task types.
func (r *Registry) Types() []string {
	types := make([]string, 0, len(r.handlers))
	for t := range r.handlers {
		types = append(types, t)
	}
	return types
}

// RetryableError wraps an error to indicate it is transient and the job
// should be retried. Non-wrapped errors are treated as non-retryable.
type RetryableError struct {
	Err error
}

// Error implements the error interface.
func (e *RetryableError) Error() string {
	return e.Err.Error()
}

// Unwrap returns the underlying error.
func (e *RetryableError) Unwrap() error {
	return e.Err
}

// NewRetryableError wraps an error as retryable.
func NewRetryableError(err error) *RetryableError {
	return &RetryableError{Err: err}
}

// IsRetryable reports whether an error indicates a retryable failure.
func IsRetryable(err error) bool {
	var re *RetryableError
	return err != nil && func() bool {
		for e := err; e != nil; {
			if _, ok := e.(*RetryableError); ok {
				return true
			}
			if u, ok := e.(interface{ Unwrap() error }); ok {
				e = u.Unwrap()
			} else {
				break
			}
		}
		_ = re
		return false
	}()
}
