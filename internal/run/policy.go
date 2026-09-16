package run

import "time"

const (
	// LeaseTTL caps each renewal, while attempt and Run deadlines cap its end.
	LeaseTTL = 30 * time.Second
	// AttemptTTL bounds a single continuously executing attempt.
	AttemptTTL = 180 * time.Second
	// MaxRecoveries counts actual scheduled recoveries, never normal Claims.
	MaxRecoveries int64 = 3

	// StopCancel records the first stop cause without overwriting later facts.
	StopCancel = "cancel"
	// StopRunDeadline records exhaustion of the Run's total execution time.
	StopRunDeadline = "run_deadline"
	// StopAttemptTimeout records the bounded execution segment expiring.
	StopAttemptTimeout = "attempt_timeout"
	// FailureLeaseExpired identifies recovery after lost execution authority.
	FailureLeaseExpired = "lease_expired"
	// FailureDependencyUnavailable identifies a bounded transient dependency failure.
	FailureDependencyUnavailable = "dependency_unavailable"
)

func matchesExecution(r Run, authority Authority, lease Lease) bool {
	return r.ID == lease.RunID && r.TenantID == lease.TenantID &&
		authority.WorkerID != "" && authority.WorkerID == lease.WorkerID &&
		authority.SessionID != "" && authority.SessionID == lease.SessionID &&
		r.AttemptNo > 0 && r.AttemptNo == lease.AttemptNo &&
		authority.FencingToken > 0 && authority.FencingToken == lease.FencingToken
}

// CheckExecution rejects expired or stale writes even when an accepted result
// already exists. The caller supplies database time after acquiring its locks.
func CheckExecution(r Run, authority Authority, lease Lease, now time.Time) error {
	if !matchesExecution(r, authority, lease) || (r.State != Running && r.State != Stopping) ||
		r.LeaseUntil == nil || !r.LeaseUntil.After(now) {
		return ErrStaleLease
	}
	if r.CancelRequestedAt != nil {
		return ErrCancelRequested
	}
	if r.State == Stopping {
		return ErrStopRequested
	}
	if r.AttemptDeadline == nil || !r.AttemptDeadline.After(now) || !r.RunDeadline.After(now) {
		return ErrStopRequested
	}
	return nil
}

// ClaimExecution installs all execution identity fields together. Admission,
// session liveness, profile availability and capacity are checked by the locked
// transaction before persisting this change and its attempt/event/slot records.
func ClaimExecution(r *Run, authority *Authority, workerID, sessionID string, now time.Time) (Lease, error) {
	if r == nil || authority == nil || !ValidIdentifier(workerID) || !ValidIdentifier(sessionID) {
		return Lease{}, ErrInvalidArgument
	}
	if r.State.Terminal() {
		return Lease{}, ErrAlreadyTerminal
	}
	if r.State != Ready || !r.RunDeadline.After(now) || r.CancelRequestedAt != nil ||
		authority.WorkerID != "" || authority.SessionID != "" || authority.ActiveCallID != nil || r.LeaseUntil != nil {
		return Lease{}, ErrInvalidTransition
	}
	if r.AttemptNo < 0 || r.AttemptNo >= MaxSafeInteger || authority.FencingToken < 0 || authority.FencingToken >= MaxSafeInteger ||
		r.RecoveryCount < 0 || r.RecoveryCount > MaxRecoveries {
		return Lease{}, ErrInternal
	}
	attemptDeadline := minTime(now.Add(AttemptTTL), r.RunDeadline)
	leaseUntil := minTime(now.Add(LeaseTTL), attemptDeadline)
	r.State, r.AttemptNo = Running, r.AttemptNo+1
	r.AttemptDeadline, r.LeaseUntil = &attemptDeadline, &leaseUntil
	r.NextAttemptAt, r.StopReason, r.Error = nil, nil, nil
	r.UpdatedAt = now
	authority.WorkerID, authority.SessionID = workerID, sessionID
	authority.FencingToken++
	return Lease{TenantID: r.TenantID, RunID: r.ID, WorkerID: workerID, SessionID: sessionID,
		AttemptNo: r.AttemptNo, FencingToken: authority.FencingToken}, nil
}

// RequestCancel records a cancellation fact independently of the first stop
// cause. Active execution closes only after stop acknowledgement or lease expiry.
func RequestCancel(r *Run, now time.Time) error {
	if r == nil {
		return ErrInvalidArgument
	}
	if r.State.Terminal() {
		return ErrAlreadyTerminal
	}
	if !r.State.Valid() {
		return ErrInvalidTransition
	}
	if r.CancelRequestedAt != nil {
		return nil
	}
	r.CancelRequestedAt = &now
	r.UpdatedAt = now
	if r.State == Running || r.State == Stopping {
		if r.StopReason == nil {
			reason := StopCancel
			r.StopReason = &reason
		}
		r.State = Stopping
		return nil
	}
	finishRun(r, Cancelled, "", now)
	return nil
}

// RequestStop preserves the first reason while deadlines and a later explicit
// cancellation remain independent facts considered when the attempt closes.
func RequestStop(r *Run, reason string, now time.Time) error {
	if r == nil || (reason != StopCancel && reason != StopRunDeadline && reason != StopAttemptTimeout) {
		return ErrInvalidArgument
	}
	if r.State.Terminal() {
		return ErrAlreadyTerminal
	}
	if r.State != Running && r.State != Stopping {
		return ErrInvalidTransition
	}
	if reason == StopCancel {
		return RequestCancel(r, now)
	}
	if (reason == StopRunDeadline && r.RunDeadline.After(now)) ||
		(reason == StopAttemptTimeout && (r.AttemptDeadline == nil || r.AttemptDeadline.After(now))) {
		return ErrInvalidTransition
	}
	if r.State == Stopping {
		return nil
	}
	r.State, r.StopReason, r.UpdatedAt = Stopping, &reason, now
	return nil
}

// Heartbeat only renews current execution. A matching stopping attempt receives
// stop control even after expiry; this response never grants new execution rights.
func Heartbeat(r *Run, authority Authority, lease Lease, now time.Time) error {
	if r == nil {
		return ErrInvalidArgument
	}
	if !matchesExecution(*r, authority, lease) {
		return ErrStaleLease
	}
	if r.State == Stopping {
		if r.CancelRequestedAt != nil {
			return ErrCancelRequested
		}
		return ErrStopRequested
	}
	if err := CheckExecution(*r, authority, lease, now); err != nil {
		return err
	}
	leaseUntil := minTime(now.Add(LeaseTTL), minTime(*r.AttemptDeadline, r.RunDeadline))
	r.LeaseUntil, r.UpdatedAt = &leaseUntil, now
	return nil
}

// CloseAttempt converges an already authenticated failure, stop acknowledgement,
// or expired lease. The caller must atomically mark any previous ActiveCallID as
// unknown and close the attempt/release slots; this policy never refunds holds.
// Repeated closes reject the resulting nonactive state without spending recovery.
func CloseAttempt(r *Run, authority *Authority, reason string, now time.Time) (string, error) {
	if r == nil || authority == nil || !validCloseReason(reason) {
		return "", ErrInvalidArgument
	}
	if r.State.Terminal() {
		return "", ErrAlreadyTerminal
	}
	if r.State != Running && r.State != Stopping {
		return "", ErrInvalidTransition
	}
	if r.RecoveryCount < 0 || r.RecoveryCount > MaxRecoveries {
		return "", ErrInternal
	}
	effectiveReason := reason
	if r.State == Stopping {
		if r.StopReason == nil || (*r.StopReason != StopCancel && *r.StopReason != StopRunDeadline && *r.StopReason != StopAttemptTimeout) {
			return "", ErrInternal
		}
		effectiveReason = *r.StopReason
	}
	if r.CancelRequestedAt == nil && r.RunDeadline.After(now) {
		if (effectiveReason == StopCancel) || (effectiveReason == StopRunDeadline) ||
			(effectiveReason == StopAttemptTimeout && (r.AttemptDeadline == nil || r.AttemptDeadline.After(now))) ||
			(reason == FailureLeaseExpired && (r.LeaseUntil == nil || r.LeaseUntil.After(now))) {
			return "", ErrInvalidTransition
		}
	}
	outcome := "failed_terminal"
	switch {
	case r.CancelRequestedAt != nil:
		outcome = "cancelled"
		finishRun(r, Cancelled, "", now)
	case !r.RunDeadline.After(now):
		finishRun(r, Failed, "RUN_DEADLINE_EXCEEDED", now)
	case recoverableReason(effectiveReason):
		prefix := "failed_"
		if reason == FailureLeaseExpired {
			prefix = "lease_expired_"
		}
		outcome = prefix + "terminal"
		if r.RecoveryCount == MaxRecoveries {
			finishRun(r, Failed, failureCode(effectiveReason), now)
		} else {
			nextAttempt := now.Add(time.Second << r.RecoveryCount)
			r.RecoveryCount++
			r.State, r.NextAttemptAt = RetryWait, &nextAttempt
			r.Error = &Failure{Code: failureCode(effectiveReason), Message: failureCode(effectiveReason)}
			r.LeaseUntil, r.AttemptDeadline = nil, nil
			r.UpdatedAt = now
			outcome = prefix + "retry"
		}
	default:
		finishRun(r, Failed, failureCode(effectiveReason), now)
	}
	authority.WorkerID, authority.SessionID, authority.ActiveCallID = "", "", nil
	return outcome, nil
}

// ExpireWaiting applies waiting-state deadlines and promotes a due retry. Run
// deadline wins ties with approval expiry and prevents a due retry being claimed.
func ExpireWaiting(r *Run, now time.Time) error {
	if r == nil {
		return ErrInvalidArgument
	}
	if r.State.Terminal() {
		return ErrAlreadyTerminal
	}
	if r.State != Ready && r.State != RetryWait && r.State != AwaitingApproval {
		return ErrInvalidTransition
	}
	if r.CancelRequestedAt != nil {
		finishRun(r, Cancelled, "", now)
		return nil
	}
	if !r.RunDeadline.After(now) {
		finishRun(r, Failed, "RUN_DEADLINE_EXCEEDED", now)
		return nil
	}
	if r.State == AwaitingApproval {
		if r.PermissionExpiresAt == nil {
			return ErrInternal
		}
		if !r.PermissionExpiresAt.After(now) {
			finishRun(r, Failed, "APPROVAL_EXPIRED", now)
		}
	}
	if r.State == RetryWait {
		if r.NextAttemptAt == nil {
			return ErrInternal
		}
		if !r.NextAttemptAt.After(now) {
			r.State, r.NextAttemptAt, r.UpdatedAt = Ready, nil, now
		}
	}
	return nil
}

func finishRun(r *Run, state State, code string, now time.Time) {
	r.State, r.UpdatedAt = state, now
	r.LeaseUntil, r.AttemptDeadline, r.NextAttemptAt = nil, nil, nil
	r.Error = nil
	if code != "" {
		r.Error = &Failure{Code: code, Message: code}
	}
}

func validCloseReason(reason string) bool {
	return reason == StopCancel || reason == StopRunDeadline || recoverableReason(reason) ||
		reason == string(ErrInvalidArgument) || reason == string(ErrProfileUnavailable) || reason == string(ErrBudgetExhausted) ||
		reason == "EXECUTOR_PROTOCOL_ERROR" || reason == "MODEL_PROTOCOL_ERROR" ||
		reason == "MODEL_UNSUPPORTED" || reason == "CHECKPOINT_TOO_LARGE"
}

func recoverableReason(reason string) bool {
	return reason == FailureLeaseExpired || reason == FailureDependencyUnavailable ||
		reason == string(ErrDependencyUnavailable) || reason == StopAttemptTimeout || reason == "TIMEOUT"
}

func failureCode(reason string) string {
	switch reason {
	case FailureLeaseExpired:
		return "LEASE_EXPIRED"
	case StopAttemptTimeout:
		return "ATTEMPT_DEADLINE_EXCEEDED"
	case FailureDependencyUnavailable:
		return string(ErrDependencyUnavailable)
	default:
		return reason
	}
}

func minTime(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
