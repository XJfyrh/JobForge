// Package domain defines the core JobForge task entities, state machine and
// domain errors. All state transitions are centralized here; transport layers
// (HTTP, gRPC) must not contain transition logic.
package domain

// JobState represents the current lifecycle state of a job.
type JobState string

const (
	// StateScheduled indicates the job is waiting for its run_at time.
	StateScheduled JobState = "scheduled"

	// StateReady indicates the job is available for Worker claim.
	StateReady JobState = "ready"

	// StateRunning indicates the job has been claimed and holds a valid lease.
	StateRunning JobState = "running"

	// StateCancelling indicates a cancel request has been received while the
	// job is running. The Worker should stop work; the job transitions to
	// cancelled when the Worker acknowledges or the lease expires.
	StateCancelling JobState = "cancelling"

	// StateRetryWait indicates the job failed with a retryable error and is
	// waiting for the backoff period to expire before becoming ready again.
	StateRetryWait JobState = "retry_wait"

	// StateSucceeded is a terminal state: the job completed successfully.
	StateSucceeded JobState = "succeeded"

	// StateDead is a terminal state: the job exhausted retries or hit a
	// non-retryable error.
	StateDead JobState = "dead"

	// StateCancelled is a terminal state: the job was cancelled.
	StateCancelled JobState = "cancelled"
)

// IsTerminal reports whether the state is a final state from which no further
// transitions are allowed.
func (s JobState) IsTerminal() bool {
	switch s {
	case StateSucceeded, StateDead, StateCancelled:
		return true
	default:
		return false
	}
}

// validTransitions defines the allowed state transitions. Any transition not
// listed here is invalid and must be rejected by the domain layer.
//
// Invariant: terminal states (succeeded, dead, cancelled) have no outgoing
// transitions. Manual retry creates a NEW job rather than reverting state.
var validTransitions = map[JobState][]JobState{
	StateScheduled:  {StateReady, StateCancelled},
	StateReady:      {StateRunning, StateCancelled},
	StateRunning:    {StateSucceeded, StateRetryWait, StateDead, StateReady, StateCancelling},
	StateCancelling: {StateCancelled},
	StateRetryWait:  {StateReady, StateCancelled},
	// Terminal states: no transitions allowed.
	StateSucceeded: {},
	StateDead:      {},
	StateCancelled: {},
}

// CanTransition reports whether transitioning from state 'from' to state 'to'
// is a valid state machine transition.
func CanTransition(from, to JobState) bool {
	targets, ok := validTransitions[from]
	if !ok {
		return false
	}
	for _, t := range targets {
		if t == to {
			return true
		}
	}
	return false
}

// AllStates returns all defined job states. Useful for validation and
// database CHECK constraints documentation.
func AllStates() []JobState {
	return []JobState{
		StateScheduled,
		StateReady,
		StateRunning,
		StateCancelling,
		StateRetryWait,
		StateSucceeded,
		StateDead,
		StateCancelled,
	}
}
