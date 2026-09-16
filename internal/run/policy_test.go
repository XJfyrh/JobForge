package run

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func policyFixture() (Run, Authority, time.Time) {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	r := Run{ID: "run-1", TenantID: "tenant-1", State: Ready,
		RunDeadline: now.Add(2 * time.Hour), CreatedAt: now, UpdatedAt: now,
		CursorVersion: 5, Budget: BudgetView{RunUsage: Usage{PhysicalHTTP: 3, Tokens: 100, CostMicroyuan: 200}}}
	authority := Authority{FencingToken: 8, CheckpointBytes: 100, EventSequence: 7,
		NextStepID: "step-6", NextStepKind: "search_policy", NextInputHash: "fixed-input-hash"}
	return r, authority, now
}

func runningFixture(t *testing.T) (Run, Authority, Lease, time.Time) {
	t.Helper()
	r, authority, now := policyFixture()
	lease, err := ClaimExecution(&r, &authority, "worker-1", "session-1", now)
	if err != nil {
		t.Fatal(err)
	}
	return r, authority, lease, now
}

func TestClaimInstallsBoundedExecutionIdentityWithoutSpendingRecovery(t *testing.T) {
	for _, remaining := range []time.Duration{10 * time.Second, 100 * time.Second, time.Hour} {
		t.Run(remaining.String(), func(t *testing.T) {
			r, authority, now := policyFixture()
			r.RunDeadline, r.AttemptNo, r.RecoveryCount = now.Add(remaining), 4, 2
			budget := r.Budget
			lease, err := ClaimExecution(&r, &authority, "worker-1", "session-1", now)
			if err != nil || r.State != Running || r.AttemptNo != 5 || authority.FencingToken != 9 || r.RecoveryCount != 2 {
				t.Fatalf("claim identity: run=%+v authority=%+v err=%v", r, authority, err)
			}
			if !r.LeaseUntil.Equal(now.Add(min(remaining, LeaseTTL))) ||
				!r.AttemptDeadline.Equal(now.Add(min(remaining, AttemptTTL))) {
				t.Fatal("claim exceeded lease, attempt or Run time bound")
			}
			if err := CheckExecution(r, authority, lease, now); err != nil || !reflect.DeepEqual(r.Budget, budget) ||
				r.CursorVersion != 5 || authority.NextStepID != "step-6" {
				t.Fatal("claim changed checkpoint/budget or returned unusable authority")
			}
		})
	}
}

func TestClaimRejectsExpiredOrInvalidStateWithoutPartialMutation(t *testing.T) {
	for _, name := range []string{"deadline-equality", "not-ready", "cancelled", "active-call", "token-overflow", "attempt-overflow", "bad-worker"} {
		t.Run(name, func(t *testing.T) {
			r, authority, now := policyFixture()
			workerID := "worker-1"
			switch name {
			case "deadline-equality":
				r.RunDeadline = now
			case "not-ready":
				r.State = RetryWait
			case "cancelled":
				r.State = Cancelled
			case "active-call":
				id := "uncertain-call"
				authority.ActiveCallID = &id
			case "token-overflow":
				authority.FencingToken = MaxSafeInteger
			case "attempt-overflow":
				r.AttemptNo = MaxSafeInteger
			case "bad-worker":
				workerID = "invalid worker"
			}
			before, beforeAuthority := r, authority
			if _, err := ClaimExecution(&r, &authority, workerID, "session-1", now); err == nil ||
				!reflect.DeepEqual(r, before) || !reflect.DeepEqual(authority, beforeAuthority) {
				t.Fatal("rejected claim changed execution identity")
			}
		})
	}
}

func TestCheckExecutionRejectsEveryStaleIdentityAndExactExpiry(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"valid", nil}, {"before-expiry", nil}, {"lease-equality", ErrStaleLease},
		{"worker", ErrStaleLease}, {"session", ErrStaleLease}, {"token", ErrStaleLease},
		{"attempt", ErrStaleLease}, {"tenant", ErrStaleLease}, {"run", ErrStaleLease},
		{"terminal", ErrStaleLease}, {"missing-lease", ErrStaleLease},
		{"stopping", ErrStopRequested}, {"cancel", ErrCancelRequested},
		{"attempt-equality", ErrStopRequested}, {"run-equality", ErrStopRequested},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, authority, lease, now := runningFixture(t)
			switch tc.name {
			case "before-expiry":
				now = r.LeaseUntil.Add(-time.Nanosecond)
			case "lease-equality":
				now = *r.LeaseUntil
			case "worker":
				lease.WorkerID = "old-worker"
			case "session":
				lease.SessionID = "old-session"
			case "token":
				lease.FencingToken--
			case "attempt":
				lease.AttemptNo--
			case "tenant":
				lease.TenantID = "another-tenant"
			case "run":
				lease.RunID = "another-run"
			case "terminal":
				r.State = Succeeded
			case "missing-lease":
				r.LeaseUntil = nil
			case "stopping":
				r.State = Stopping
			case "cancel":
				r.State, r.CancelRequestedAt = Stopping, &now
			case "attempt-equality":
				r.AttemptDeadline = &now
			case "run-equality":
				r.RunDeadline = now
			}
			if err := CheckExecution(r, authority, lease, now); !errors.Is(err, tc.want) {
				t.Fatalf("execution check = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestHeartbeatCapsRenewalAndNeverRevivesExpiredOrStoppingExecution(t *testing.T) {
	r, authority, lease, now := runningFixture(t)
	now = now.Add(5 * time.Second)
	if err := Heartbeat(&r, authority, lease, now); err != nil || !r.LeaseUntil.Equal(now.Add(LeaseTTL)) {
		t.Fatalf("valid renewal: %v", err)
	}
	deadline := now.Add(10 * time.Second)
	r.AttemptDeadline = &deadline
	if err := Heartbeat(&r, authority, lease, now); err != nil || !r.LeaseUntil.Equal(deadline) {
		t.Fatalf("bounded renewal: %v", err)
	}
	before := r
	if err := Heartbeat(&r, authority, lease, deadline); !errors.Is(err, ErrStaleLease) || !reflect.DeepEqual(r, before) {
		t.Fatal("expired heartbeat revived execution")
	}
	if err := RequestStop(&r, StopAttemptTimeout, deadline); err != nil {
		t.Fatal(err)
	}
	before = r
	if err := Heartbeat(&r, authority, lease, deadline); !errors.Is(err, ErrStopRequested) || !reflect.DeepEqual(r, before) {
		t.Fatal("stopping heartbeat renewed or changed execution")
	}
	lease.FencingToken--
	if err := Heartbeat(&r, authority, lease, deadline); !errors.Is(err, ErrStaleLease) {
		t.Fatal("stopping state excused a stale fencing token")
	}
}

func TestCancelRecordsIndependentFactAndPreservesFirstStopReason(t *testing.T) {
	r, authority, lease, _ := runningFixture(t)
	now := *r.AttemptDeadline
	if err := RequestStop(&r, StopAttemptTimeout, now); err != nil {
		t.Fatal(err)
	}
	if err := RequestCancel(&r, now.Add(time.Second)); err != nil || r.State != Stopping ||
		r.StopReason == nil || *r.StopReason != StopAttemptTimeout || r.CancelRequestedAt == nil {
		t.Fatal("late cancel overwrote the first cause or failed to record cancellation")
	}
	before := r
	if err := RequestCancel(&r, now.Add(2*time.Second)); err != nil || !reflect.DeepEqual(r, before) {
		t.Fatal("repeated cancellation changed its original acceptance time")
	}
	if err := Heartbeat(&r, authority, lease, now); !errors.Is(err, ErrCancelRequested) || !reflect.DeepEqual(r, before) {
		t.Fatal("cancelled stopping attempt did not receive cancellation control")
	}
	for _, state := range []State{Ready, RetryWait, AwaitingApproval} {
		waiting, _, acceptedAt := policyFixture()
		waiting.State = state
		if err := RequestCancel(&waiting, acceptedAt); err != nil || waiting.State != Cancelled || waiting.CancelRequestedAt == nil {
			t.Fatalf("waiting cancellation from %s failed: %v", state, err)
		}
	}
}

func TestCloseAttemptPrioritizesCancellationThenRunDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cancel    bool
		expired   bool
		wantState State
		outcome   string
	}{
		{"late-cancel-wins", true, true, Cancelled, "cancelled"},
		{"run-deadline-wins", false, true, Failed, "failed_terminal"},
		{"attempt-timeout-recovers", false, false, RetryWait, "failed_retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, authority, _, _ := runningFixture(t)
			now := *r.AttemptDeadline
			if err := RequestStop(&r, StopAttemptTimeout, now); err != nil {
				t.Fatal(err)
			}
			if tc.expired {
				r.RunDeadline = now.Add(time.Second)
			}
			if tc.cancel {
				if err := RequestCancel(&r, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			callID := "unknown-call"
			authority.ActiveCallID = &callID
			budget, token, attempt := r.Budget, authority.FencingToken, r.AttemptNo
			outcome, err := CloseAttempt(&r, &authority, StopAttemptTimeout, now.Add(time.Second))
			if err != nil || outcome != tc.outcome || r.State != tc.wantState {
				t.Fatalf("close precedence: state=%s outcome=%s err=%v", r.State, outcome, err)
			}
			if authority.ActiveCallID != nil || authority.WorkerID != "" || authority.SessionID != "" ||
				r.LeaseUntil != nil || r.AttemptDeadline != nil || authority.FencingToken != token || r.AttemptNo != attempt ||
				!reflect.DeepEqual(r.Budget, budget) || r.Outcome != nil {
				t.Fatal("close retained active authority, refunded a hold, or invented a business outcome")
			}
			if tc.wantState == Failed && (r.Error == nil || r.Error.Code != "RUN_DEADLINE_EXCEEDED") {
				t.Fatal("total deadline did not produce its stable failure code")
			}
			if tc.wantState != RetryWait && r.RecoveryCount != 0 {
				t.Fatal("terminal close spent a recovery")
			}
		})
	}
}

func TestFailuresScheduleExactlyThreeRecoveriesWithOneTwoFourSecondDelay(t *testing.T) {
	for _, reason := range []string{FailureDependencyUnavailable, string(ErrDependencyUnavailable), "TIMEOUT", FailureLeaseExpired, StopAttemptTimeout} {
		t.Run(reason, func(t *testing.T) {
			r, authority, _, now := runningFixture(t)
			for closure := int64(0); closure <= MaxRecoveries; closure++ {
				switch reason {
				case FailureLeaseExpired:
					now = *r.LeaseUntil
				case StopAttemptTimeout:
					now = *r.AttemptDeadline
				default:
					now = now.Add(time.Second)
				}
				outcome, err := CloseAttempt(&r, &authority, reason, now)
				if err != nil || r.RecoveryCount != min(closure+1, MaxRecoveries) {
					t.Fatalf("close %d: count=%d err=%v", closure, r.RecoveryCount, err)
				}
				wantOutcome := "failed_"
				if reason == FailureLeaseExpired {
					wantOutcome = "lease_expired_"
				}
				if closure == MaxRecoveries {
					if r.State != Failed || outcome != wantOutcome+"terminal" || r.NextAttemptAt != nil {
						t.Fatal("fourth failure did not exhaust recovery")
					}
					break
				}
				if r.State != RetryWait || outcome != wantOutcome+"retry" || r.NextAttemptAt == nil ||
					!r.NextAttemptAt.Equal(now.Add(time.Second<<closure)) {
					t.Fatalf("recovery %d did not use its fixed backoff", closure+1)
				}
				before, beforeAuthority := r, authority
				if _, err := CloseAttempt(&r, &authority, reason, now); !errors.Is(err, ErrInvalidTransition) ||
					!reflect.DeepEqual(r, before) || !reflect.DeepEqual(authority, beforeAuthority) {
					t.Fatal("duplicate close scheduled another recovery")
				}
				if err := ExpireWaiting(&r, r.NextAttemptAt.Add(-time.Nanosecond)); err != nil || r.State != RetryWait {
					t.Fatal("retry became ready before backoff elapsed")
				}
				now = *r.NextAttemptAt
				if err := ExpireWaiting(&r, now); err != nil || r.State != Ready {
					t.Fatal("due retry did not become ready")
				}
				if _, err := ClaimExecution(&r, &authority, "worker-1", "session-1", now); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCloseRejectsUnknownReasonsAndMakesProtocolFailuresPermanent(t *testing.T) {
	for _, reason := range []string{string(ErrInvalidArgument), string(ErrProfileUnavailable), string(ErrBudgetExhausted),
		"MODEL_PROTOCOL_ERROR", "MODEL_UNSUPPORTED", "EXECUTOR_PROTOCOL_ERROR", "CHECKPOINT_TOO_LARGE"} {
		t.Run(reason, func(t *testing.T) {
			r, authority, _, now := runningFixture(t)
			outcome, err := CloseAttempt(&r, &authority, reason, now)
			if err != nil || r.State != Failed || r.RecoveryCount != 0 || outcome != "failed_terminal" ||
				r.Error == nil || r.Error.Code != reason || r.Error.Message != reason {
				t.Fatal("permanent failure was retried or lost its stable code")
			}
		})
	}
	for _, reason := range []string{"", "HTTP 429 should retry", "provider private response", "INVALID_OUTPUT"} {
		r, authority, _, now := runningFixture(t)
		before, beforeAuthority := r, authority
		if _, err := CloseAttempt(&r, &authority, reason, now); !errors.Is(err, ErrInvalidArgument) ||
			!reflect.DeepEqual(r, before) || !reflect.DeepEqual(authority, beforeAuthority) {
			t.Fatal("unknown failure reason changed authoritative state")
		}
	}
}

func TestWaitingDeadlinesAreStrictAndRunDeadlineWinsApprovalTie(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    State
		runDelta time.Duration
		approval time.Duration
		want     State
		code     string
	}{
		{"ready-equality", Ready, 0, 0, Failed, "RUN_DEADLINE_EXCEEDED"},
		{"retry-deadline", RetryWait, 0, 0, Failed, "RUN_DEADLINE_EXCEEDED"},
		{"approval-first", AwaitingApproval, time.Hour, 0, Failed, "APPROVAL_EXPIRED"},
		{"approval-tie", AwaitingApproval, 0, 0, Failed, "RUN_DEADLINE_EXCEEDED"},
		{"approval-future", AwaitingApproval, time.Hour, time.Nanosecond, AwaitingApproval, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _, now := policyFixture()
			r.State, r.RunDeadline = tc.state, now.Add(tc.runDelta)
			approval := now.Add(tc.approval)
			r.PermissionExpiresAt = &approval
			if err := ExpireWaiting(&r, now); err != nil || r.State != tc.want {
				t.Fatalf("waiting deadline: state=%s err=%v", r.State, err)
			}
			if tc.code != "" && (r.Error == nil || r.Error.Code != tc.code) {
				t.Fatal("waiting deadline lost precedence or stable failure code")
			}
		})
	}
}

func TestTerminalStatesRemainImmutableAcrossPolicyOperations(t *testing.T) {
	for _, state := range []State{Succeeded, Failed, Cancelled} {
		t.Run(string(state), func(t *testing.T) {
			r, authority, lease, now := runningFixture(t)
			r.State = state
			before, beforeAuthority := r, authority
			calls := []func() error{
				func() error { _, err := ClaimExecution(&r, &authority, "worker-2", "session-2", now); return err },
				func() error { return RequestCancel(&r, now) },
				func() error { return RequestStop(&r, StopRunDeadline, now) },
				func() error { _, err := CloseAttempt(&r, &authority, FailureDependencyUnavailable, now); return err },
				func() error { return ExpireWaiting(&r, now) },
			}
			for _, call := range calls {
				if err := call(); !errors.Is(err, ErrAlreadyTerminal) || !reflect.DeepEqual(r, before) || !reflect.DeepEqual(authority, beforeAuthority) {
					t.Fatal("terminal policy call changed the accepted result")
				}
			}
			if err := Heartbeat(&r, authority, lease, now); !errors.Is(err, ErrStaleLease) || !reflect.DeepEqual(r, before) {
				t.Fatal("terminal heartbeat revived execution")
			}
		})
	}
}
