package integration

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func TestRunClaimConcurrentCapacityAndFencing(t *testing.T) {
	h := setupRunHarness(t)
	for _, key := range []string{"capacity-1", "capacity-2", "capacity-3", "capacity-4"} {
		h.submit(t, "tenant-a", key)
	}
	var wg sync.WaitGroup
	claims := make([]*agentrun.ClaimedRun, 8)
	failures := make([]error, 8)
	start := make(chan struct{})
	second, err := h.Store.Register(h.Ctx, "contract-worker-2", uuid.NewString(), "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	for i := range claims {
		wg.Go(func() {
			<-start
			principal, sessionID := h.Principal, h.Session.ID
			if i%2 == 1 {
				principal, sessionID = second.WorkerID, second.ID
			}
			claims[i], failures[i] = h.Store.Claim(h.Ctx, principal, sessionID)
		})
	}
	close(start)
	wg.Wait()
	ids := map[string]bool{}
	for i, claimed := range claims {
		if failures[i] != nil {
			t.Fatalf("concurrent Claim: %v", failures[i])
		}
		if claimed == nil {
			continue
		}
		if ids[claimed.Lease.RunID] || claimed.Lease.AttemptNo != 1 || claimed.Lease.FencingToken != 1 {
			t.Fatal("Run claimed twice or invalid atomic attempt")
		}
		ids[claimed.Lease.RunID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("profile/tenant/worker capacity allowed %d claims, want 2", len(ids))
	}
	var active, attempts, slots int
	if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from runs where state='running'),
		(select count(*) from run_attempts),(select coalesce(sum(used),0) from execution_slots)`).Scan(&active, &attempts, &slots); err != nil {
		t.Fatal(err)
	}
	if active != 2 || attempts != 2 || slots != 6 {
		t.Fatalf("Claim partially persisted active=%d attempts=%d slotuses=%d", active, attempts, slots)
	}
}

func TestRunExpiredLeaseRecoveryIsBoundedAndNeverReusesAuthority(t *testing.T) {
	h := setupRunHarness(t)
	r := h.submit(t, "tenant-a", "bounded-recovery")
	var old agentrun.Lease
	for attempt := int64(1); attempt <= 4; attempt++ {
		claimed := h.claim(t)
		if claimed.Lease.AttemptNo != attempt || claimed.Lease.FencingToken != attempt {
			t.Fatal("Claim did not atomically advance attempt and fence")
		}
		if attempt > 1 {
			if _, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, old); !errors.Is(err, agentrun.ErrStaleLease) {
				t.Fatalf("old owner accepted after recovery: %v", err)
			}
		}
		old = claimed.Lease
		if _, err := h.Pool.Exec(h.Ctx, "update runs set lease_until=clock_timestamp()-interval '1 millisecond' where run_id=$1", r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease); !errors.Is(err, agentrun.ErrStaleLease) {
			t.Fatalf("expired matching lease accepted before scanner: %v", err)
		}
		changed, err := h.Store.Sweep(h.Ctx, 100)
		if err != nil || changed != 1 {
			t.Fatalf("recovery Sweep changed=%d error=%v", changed, err)
		}
		view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		var slots int
		if err := h.Pool.QueryRow(h.Ctx, "select coalesce(sum(used),0) from execution_slots").Scan(&slots); err != nil || slots != 0 {
			t.Fatalf("closed attempt retained capacity=%d error=%v", slots, err)
		}
		if attempt == 4 {
			if view.State != agentrun.Failed || view.RecoveryCount != 3 || view.Error == nil || view.Error.Code != "LEASE_EXPIRED" {
				t.Fatalf("fourth loss exceeded recovery allowance: %+v", view)
			}
			break
		}
		if view.State != agentrun.RetryWait || view.RecoveryCount != attempt || view.NextAttemptAt == nil {
			t.Fatal("lost lease did not schedule exactly one recovery")
		}
		if got := view.NextAttemptAt.Sub(view.UpdatedAt); got.Seconds() != float64(int64(1)<<(attempt-1)) {
			t.Fatalf("recovery backoff=%s", got)
		}
		if _, err := h.Pool.Exec(h.Ctx, "update runs set next_attempt_at=clock_timestamp() where run_id=$1", r.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
			t.Fatal(err)
		}
	}
	if changed, err := h.Store.Sweep(h.Ctx, 100); err != nil || changed != 0 {
		t.Fatalf("terminal scan repeated a transition: changed=%d error=%v", changed, err)
	}
	child, err := h.Service.Retry(h.Ctx, r.TenantID, r.ID, "manual-after-loss", agentrun.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 3600})
	if err != nil || child.Run.ID == r.ID || child.Run.Budget.Family.ID != r.Budget.Family.ID || child.Run.RecoveryCount != 0 {
		t.Fatalf("manual retry identity/account policy: %v", err)
	}
}

func TestRunStopPrecedenceAndExpiredLeaseAcknowledgement(t *testing.T) {
	h := setupRunHarness(t)
	r := h.submit(t, "tenant-a", "cancel-after-timeout")
	claimed := h.claim(t)
	if _, err := h.Pool.Exec(h.Ctx, `update runs set lease_until=clock_timestamp()-interval '2 seconds',
		attempt_deadline=clock_timestamp()-interval '1 second' where run_id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	heartbeat, err := h.Store.HeartbeatExecution(h.Ctx, h.Principal, claimed.Lease)
	if err != nil || heartbeat.Continue || heartbeat.StopReason != agentrun.StopAttemptTimeout {
		t.Fatalf("attempt deadline stop: %v", err)
	}
	cancelled, err := h.Service.Cancel(h.Ctx, r.TenantID, r.ID, "later-cancel")
	if err != nil || cancelled.Run.State != agentrun.Stopping || cancelled.Run.StopReason == nil || *cancelled.Run.StopReason != agentrun.StopAttemptTimeout || cancelled.Run.CancelRequestedAt == nil {
		t.Fatalf("later cancel rewrote first cause or lost independent fact: %v", err)
	}
	closed, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease)
	if err != nil || closed.Run.State != agentrun.Cancelled || closed.Run.RecoveryCount != 0 || closed.AttemptOutcome != "cancelled" {
		t.Fatalf("cancel did not win convergence: %v", err)
	}
	if _, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("duplicate stop closure accepted: %v", err)
	}
	if replay, err := h.Service.Cancel(h.Ctx, r.TenantID, r.ID, "later-cancel"); err != nil || !replay.Reused || replay.OperationID != cancelled.OperationID {
		t.Fatalf("accepted cancel did not survive terminal state: %v", err)
	}
}

func TestRunFailureClassificationAndWaitingDeadline(t *testing.T) {
	for _, tc := range []struct {
		reason     string
		state      agentrun.State
		recoveries int64
	}{
		{"DEPENDENCY_UNAVAILABLE", agentrun.RetryWait, 1}, {"MODEL_PROTOCOL_ERROR", agentrun.Failed, 0}, {"BUDGET_EXHAUSTED", agentrun.Failed, 0},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			h := setupRunHarness(t)
			h.submit(t, "tenant-a", "failure-classification")
			claimed := h.claim(t)
			result, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, currentRunStep(claimed), tc.reason)
			if err != nil || result.State != tc.state || result.RecoveryCount != tc.recoveries {
				t.Fatalf("failure classified incorrectly: state=%s recoveries=%d error=%v", result.State, result.RecoveryCount, err)
			}
			if _, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, currentRunStep(claimed), tc.reason); !errors.Is(err, agentrun.ErrStaleLease) {
				t.Fatalf("duplicate failure closed twice: %v", err)
			}
		})
	}
	h := setupRunHarness(t)
	r := h.submit(t, "tenant-a", "waiting-expiry")
	if _, err := h.Pool.Exec(h.Ctx, "update runs set created_at=clock_timestamp()-interval '2 hours',run_deadline=clock_timestamp()-interval '1 second' where run_id=$1", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
		t.Fatal(err)
	}
	view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil || view.State != agentrun.Failed || view.AttemptNo != 0 || view.Error == nil || view.Error.Code != "RUN_DEADLINE_EXCEEDED" {
		t.Fatalf("waiting deadline failed: %v", err)
	}
}

func TestRunClaimRechecksDeadlineAfterAccountContention(t *testing.T) {
	h := setupRunHarness(t)
	request := submitFixture(h, "claim-deadline")
	request.RunTimeoutSeconds = 1
	accepted, err := h.Service.Submit(h.Ctx, "tenant-a", "claim-deadline", request)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(h.Ctx) }()
	if _, err := blocker.Exec(h.Ctx, "select account_id from budget_accounts where account_id=$1 for update", accepted.Run.Budget.Family.ID); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		claimed *agentrun.ClaimedRun
		err     error
	}
	done := make(chan outcome, 1)
	go func() { claimed, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID); done <- outcome{claimed, err} }()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		if err := h.Pool.QueryRow(h.Ctx, `select exists(select 1 from pg_stat_activity where datname=current_database()
			and pid<>pg_backend_pid() and wait_event_type='Lock')`).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("Claim did not wait for the production account row lock")
	}
	if _, err := blocker.Exec(h.Ctx, "select pg_sleep(1.1)"); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(h.Ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.claimed != nil {
			t.Fatalf("expired Run received Claim authority: %v", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Claim did not finish after contention")
	}
	view, err := h.Store.Get(h.Ctx, "tenant-a", accepted.Run.ID)
	if err != nil || view.State != agentrun.Failed || view.AttemptNo != 0 || view.Error == nil || view.Error.Code != "RUN_DEADLINE_EXCEEDED" {
		t.Fatalf("deadline loss not persisted before Claim: %v", err)
	}
	var slots int
	if err := h.Pool.QueryRow(h.Ctx, "select coalesce(sum(used),0) from execution_slots").Scan(&slots); err != nil || slots != 0 {
		t.Fatalf("expired admission consumed execution slots: %v", err)
	}
}

func TestRunApprovalExpiryConvergesWithoutInventingBusinessSuccess(t *testing.T) {
	for _, totalDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "permission_expiry", true: "run_deadline_wins_tie"}[totalDeadline], func(t *testing.T) {
			h := setupRunHarness(t)
			r := h.submit(t, "tenant-a", "approval-expiry")
			claimed := h.claim(t)
			for {
				_, response := checkpointAdvance(t, h, &claimed, "proposal", false)
				if response.AttemptClosed {
					break
				}
			}
			expired := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
			if _, err := h.Pool.Exec(h.Ctx, "update runs set permission_expires_at=$2 where run_id=$1", r.ID, expired); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(h.Ctx, "update run_approvals set permission_expires_at=$2 where run_id=$1", r.ID, expired); err != nil {
				t.Fatal(err)
			}
			if totalDeadline {
				if _, err := h.Pool.Exec(h.Ctx, "update runs set created_at=$2,run_deadline=$3 where run_id=$1", r.ID, expired.Add(-time.Hour), expired); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
				t.Fatal(err)
			}
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			code := "APPROVAL_EXPIRED"
			if totalDeadline {
				code = "RUN_DEADLINE_EXCEEDED"
			}
			if err != nil || view.State != agentrun.Failed || view.Outcome != nil || view.Error == nil || view.Error.Code != code || view.RecoveryCount != 0 {
				t.Fatalf("approval timeout result: expected=%s error=%v", code, err)
			}
			result, err := h.Store.Result(h.Ctx, r.TenantID, r.ID)
			if err != nil || !result.Available || result.Kind == nil || *result.Kind != "proposal" {
				t.Fatal("expired proposal lost its inspectable result or became applied")
			}
		})
	}
}
