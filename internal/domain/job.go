package domain

import (
	"time"

	"github.com/google/uuid"
)

// NewID generates a new UUID v4 string for use as entity identifiers.
func NewID() string {
	return uuid.New().String()
}

// Default configuration values per ADR-0001.
const (
	DefaultLeaseTTL       = 30 * time.Second
	DefaultHeartbeat      = 10 * time.Second
	DefaultScanInterval   = 1 * time.Second
	DefaultMaxAttempts    = 3
	DefaultTimeout        = 300 * time.Second
	MaxPayloadSize        = 256 * 1024 // 256 KiB
	MaxAttemptsUpperBound = 10
)

// Job represents a task in the JobForge system. It is the core aggregate root.
// All state mutations go through domain methods that enforce the state machine
// invariants defined in state.go.
type Job struct {
	ID                string
	TenantID          string
	Queue             string
	Type              string
	Payload           []byte
	Priority          int16
	State             JobState
	RunAt             time.Time
	Attempt           int
	MaxAttempts       int
	TimeoutSeconds    int
	IdempotencyKey    *string
	LeaseOwner        *string
	LeaseUntil        *time.Time
	FencingToken      int64
	CancelRequestedAt *time.Time
	TraceID           *string
	// TraceContext holds the serialized W3C TraceContext (traceparent value)
	// captured at submission, used to restore the submit span context in the
	// Gateway/Worker (FR-503). Nullable for legacy jobs.
	TraceContext *string
	StateVersion int64
	RetryOfJobID *string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// NewJobParams holds the parameters for creating a new job.
type NewJobParams struct {
	TenantID       string
	Queue          string
	Type           string
	Payload        []byte
	Priority       int16
	RunAt          *time.Time
	MaxAttempts    int
	TimeoutSeconds int
	IdempotencyKey *string
	TraceID        *string
	TraceContext   *string
	RetryOfJobID   *string
}

// Clock abstracts time for deterministic testing. Production code uses
// RealClock; tests inject a fixed or controllable clock.
type Clock interface {
	Now() time.Time
}

// RealClock returns the actual current time.
type RealClock struct{}

// Now returns the current time from the system clock.
func (RealClock) Now() time.Time { return time.Now() }

// NewJob creates a Job with validated parameters and determines the initial
// state based on run_at. If run_at is nil or <= now, the job starts as ready;
// otherwise it starts as scheduled.
//
// Invariant: the initial state is determined solely by run_at vs now. No other
// field can influence initial state selection.
func NewJob(id string, params NewJobParams, now time.Time) (*Job, error) {
	if params.TenantID == "" {
		return nil, NewError(CodeInvalidArgument, ErrInvalidArgument, "tenant_id is required")
	}
	if params.Queue == "" {
		return nil, NewError(CodeInvalidArgument, ErrInvalidArgument, "queue is required")
	}
	if params.Type == "" {
		return nil, NewError(CodeInvalidArgument, ErrInvalidArgument, "type is required")
	}
	if len(params.Payload) > MaxPayloadSize {
		return nil, NewError(CodeInvalidArgument, ErrInvalidArgument,
			"payload exceeds maximum size of %d bytes", MaxPayloadSize)
	}

	maxAttempts := params.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = DefaultMaxAttempts
	}
	if maxAttempts < 1 || maxAttempts > MaxAttemptsUpperBound {
		return nil, NewError(CodeInvalidArgument, ErrInvalidArgument,
			"max_attempts must be between 1 and %d", MaxAttemptsUpperBound)
	}

	timeoutSeconds := params.TimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = int(DefaultTimeout.Seconds())
	}

	runAt := now
	if params.RunAt != nil {
		runAt = *params.RunAt
	}

	state := StateReady
	if runAt.After(now) {
		state = StateScheduled
	}

	return &Job{
		ID:             id,
		TenantID:       params.TenantID,
		Queue:          params.Queue,
		Type:           params.Type,
		Payload:        params.Payload,
		Priority:       params.Priority,
		State:          state,
		RunAt:          runAt,
		Attempt:        0,
		MaxAttempts:    maxAttempts,
		TimeoutSeconds: timeoutSeconds,
		IdempotencyKey: params.IdempotencyKey,
		FencingToken:   0,
		TraceID:        params.TraceID,
		TraceContext:   params.TraceContext,
		RetryOfJobID:   params.RetryOfJobID,
		StateVersion:   1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

// Claim attempts to assign this job to a worker. It enforces that the job must
// be in the ready state. On success it updates lease fields, increments attempt
// and fencing token, and transitions to running.
//
// Invariant: claim atomically updates owner, lease, attempt, fencing_token and
// state. The store layer guarantees this within a single transaction.
func (j *Job) Claim(workerID string, leaseTTL time.Duration, now time.Time) error {
	if j.State != StateReady {
		return NewError(CodeInvalidTransition, ErrInvalidTransition,
			"cannot claim job in state %q", j.State)
	}

	leaseUntil := now.Add(leaseTTL)
	j.State = StateRunning
	j.LeaseOwner = &workerID
	j.LeaseUntil = &leaseUntil
	j.Attempt++
	j.FencingToken++
	j.StateVersion++
	j.UpdatedAt = now
	return nil
}

// Complete transitions a running job to succeeded. It validates that the
// worker is the current lease owner with the correct fencing token.
//
// Invariant: Complete must match job_id, lease_owner, fencing_token and
// state=running. A cancel_requested job rejects Complete with ErrCancelRequested.
func (j *Job) Complete(workerID string, fencingToken int64, now time.Time) error {
	if j.State == StateCancelling {
		return NewError(CodeCancelRequested, ErrCancelRequested,
			"job is cancelling: complete rejected")
	}
	if j.State != StateRunning {
		if j.State.IsTerminal() {
			return NewError(CodeAlreadyTerminal, ErrAlreadyTerminal,
				"job already in terminal state %q", j.State)
		}
		return NewError(CodeInvalidTransition, ErrInvalidTransition,
			"cannot complete job in state %q", j.State)
	}
	if err := j.validateLease(workerID, fencingToken); err != nil {
		return err
	}

	j.State = StateSucceeded
	j.StateVersion++
	j.UpdatedAt = now
	return nil
}

// Fail transitions a running job to retry_wait or dead depending on the
// retryable flag and remaining attempts.
//
// Invariant: Fail in cancelling state does not trigger retry; the job
// transitions to cancelled (handled by the store layer within the transaction).
func (j *Job) Fail(workerID string, fencingToken int64, retryable bool, nextRetryAt time.Time, now time.Time) error {
	if j.State == StateCancelling {
		// In cancelling state, fail leads to cancelled, not retry.
		j.State = StateCancelled
		j.StateVersion++
		j.UpdatedAt = now
		return nil
	}
	if j.State != StateRunning {
		if j.State.IsTerminal() {
			return NewError(CodeAlreadyTerminal, ErrAlreadyTerminal,
				"job already in terminal state %q", j.State)
		}
		return NewError(CodeInvalidTransition, ErrInvalidTransition,
			"cannot fail job in state %q", j.State)
	}
	if err := j.validateLease(workerID, fencingToken); err != nil {
		return err
	}

	if !retryable || j.Attempt >= j.MaxAttempts {
		j.State = StateDead
	} else {
		j.State = StateRetryWait
		j.RunAt = nextRetryAt
		j.LeaseOwner = nil
		j.LeaseUntil = nil
	}
	j.StateVersion++
	j.UpdatedAt = now
	return nil
}

// Cancel requests cancellation. Waiting-state jobs (scheduled, ready,
// retry_wait) transition immediately to cancelled. Running jobs transition to
// cancelling. Terminal jobs return ErrAlreadyTerminal.
func (j *Job) Cancel(now time.Time) error {
	if j.State.IsTerminal() {
		return NewError(CodeAlreadyTerminal, ErrAlreadyTerminal,
			"job already in terminal state %q", j.State)
	}

	switch j.State {
	case StateScheduled, StateReady, StateRetryWait:
		j.State = StateCancelled
	case StateRunning:
		j.State = StateCancelling
		j.CancelRequestedAt = &now
	case StateCancelling:
		// Already cancelling; idempotent.
		return nil
	default:
		return NewError(CodeInvalidTransition, ErrInvalidTransition,
			"cannot cancel job in state %q", j.State)
	}
	j.StateVersion++
	j.UpdatedAt = now
	return nil
}

// Heartbeat extends the lease for a running or cancelling job. Only the
// current owner with the correct fencing token may heartbeat.
func (j *Job) Heartbeat(workerID string, fencingToken int64, ttl time.Duration, now time.Time) error {
	if j.State != StateRunning && j.State != StateCancelling {
		return NewError(CodeInvalidTransition, ErrInvalidTransition,
			"cannot heartbeat job in state %q", j.State)
	}
	if err := j.validateLease(workerID, fencingToken); err != nil {
		return err
	}

	leaseUntil := now.Add(ttl)
	j.LeaseUntil = &leaseUntil
	j.UpdatedAt = now
	return nil
}

// validateLease checks that the given workerID and fencingToken match the
// current lease. Returns ErrStaleLease on mismatch.
func (j *Job) validateLease(workerID string, fencingToken int64) error {
	if j.LeaseOwner == nil || *j.LeaseOwner != workerID || j.FencingToken != fencingToken {
		return NewError(CodeStaleLease, ErrStaleLease,
			"fencing token or owner mismatch: expected owner=%v token=%d, got owner=%s token=%d",
			j.LeaseOwner, j.FencingToken, workerID, fencingToken)
	}
	return nil
}

// Backoff calculates the retry delay using exponential backoff with full
// jitter: min(base * 2^(attempt-1) + jitter, maxBackoff).
// The jitter parameter should be a random duration in [0, base*2^(attempt-1)).
func Backoff(attempt int, base, maxBackoff time.Duration, jitter time.Duration) time.Duration {
	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay > maxBackoff {
			delay = maxBackoff
			break
		}
	}
	delay += jitter
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}
