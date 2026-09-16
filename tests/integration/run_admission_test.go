package integration

import (
	"errors"
	"sync"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func submitFixture(h *runHarness, businessKey string) agentrun.SubmitRequest {
	return agentrun.SubmitRequest{SchemaVersion: 1, TicketID: "ticket-1", BusinessRequestKey: businessKey,
		ProfileID: h.Profile.ID, BudgetBatchID: "contract-batch", RunTimeoutSeconds: 3600}
}

func TestRunAdmissionConcurrentIdentityAndAliases(t *testing.T) {
	h := setupRunHarness(t)
	request := submitFixture(h, "concurrent-intent")
	start := make(chan struct{})
	responses := make([]agentrun.SubmitResponse, 2)
	failures := make([]error, 2)
	var wg sync.WaitGroup
	for i, key := range []string{"first-op", "second-op"} {
		wg.Go(func() {
			<-start
			responses[i], failures[i] = h.Service.Submit(h.Ctx, "tenant-a", key, request)
		})
	}
	close(start)
	wg.Wait()
	for _, err := range failures {
		if err != nil {
			t.Fatalf("concurrent admission: %v", err)
		}
	}
	if responses[0].Run.ID != responses[1].Run.ID || responses[0].Reused == responses[1].Reused {
		t.Fatal("same business intent must create one root and report one reused response")
	}
	var runs, identities, families, operations int
	if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from runs), (select count(*) from business_requests),
		(select count(*) from budget_accounts where scope='family'), (select count(*) from run_operations)`).
		Scan(&runs, &identities, &families, &operations); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || identities != 1 || families != 1 || operations != 2 {
		t.Fatalf("insert race leaked provisional rows: runs=%d identities=%d families=%d operations=%d", runs, identities, families, operations)
	}
	request.TicketID = "other-ticket"
	if _, err := h.Service.Submit(h.Ctx, "tenant-a", "third-op", request); !errors.Is(err, agentrun.ErrConflict) {
		t.Fatalf("changed business content must conflict: %v", err)
	}
	if _, err := h.Service.Get(h.Ctx, "tenant-b", responses[0].Run.ID); !errors.Is(err, agentrun.ErrNotFound) {
		t.Fatalf("foreign Run must be hidden: %v", err)
	}
}

func TestRunAcceptedAdmissionSurvivesDynamicUnavailability(t *testing.T) {
	h := setupRunHarness(t)
	root := h.submit(t, "tenant-a", "replay-intent")
	if _, err := h.Service.Cancel(h.Ctx, "tenant-a", root.ID, "cancel-root"); err != nil {
		t.Fatal(err)
	}
	retryRequest := agentrun.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 600}
	child, err := h.Service.Retry(h.Ctx, "tenant-a", root.ID, "retry-first", retryRequest)
	if err != nil {
		t.Fatal(err)
	}
	if child.Run.ID == root.ID || child.Run.SnapshotID == root.SnapshotID || child.Run.CursorVersion != 0 ||
		child.Run.BusinessRequestID != root.BusinessRequestID || child.Run.Budget.Family.ID != root.Budget.Family.ID {
		t.Fatal("manual retry must get a fresh Run/snapshot/cursor and preserve the original budget family")
	}
	captures := h.Capture.count()
	h.Capture.mu.Lock()
	h.Capture.unavailable = true
	h.Capture.mu.Unlock()
	if _, err := h.Pool.Exec(h.Ctx, `update budget_accounts set frozen=true,valid_until=clock_timestamp()-interval '1 second'
		where scope='batch'`); err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Microsecond)
	if _, err := h.Pool.Exec(h.Ctx, "update business_requests set created_at=$1,retry_until=$2", past, past.Add(7*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	h.Options.Profiles[0].Executable = false
	unavailableStore, err := runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentrun.NewService(unavailableStore, h.Capture, []string{"tenant-a", "tenant-b"})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"submit-replay-intent", "submit-new-alias"} {
		response, err := service.Submit(h.Ctx, "tenant-a", key, submitFixture(h, "replay-intent"))
		if err != nil || !response.Reused || response.Run.ID != root.ID || !response.Run.RunDeadline.Equal(root.RunDeadline) {
			t.Fatalf("accepted Submit depends on live configuration: %v", err)
		}
	}
	for _, key := range []string{"retry-first", "retry-new-alias"} {
		response, err := service.Retry(h.Ctx, "tenant-a", root.ID, key, retryRequest)
		if err != nil || !response.Reused || response.Run.ID != child.Run.ID {
			t.Fatalf("accepted retry depends on expired original window/profile/batch: %v", err)
		}
	}
	if h.Capture.count() != captures {
		t.Fatal("accepted replay reached unavailable capture service")
	}
	retryRequest.RunTimeoutSeconds++
	if _, err := service.Retry(h.Ctx, "tenant-a", root.ID, "retry-conflict", retryRequest); err != agentrun.ErrConflict {
		t.Fatalf("different retry parameters must conflict before dynamic checks: %v", err)
	}
}

func TestRunAdmissionFailureCreatesNoControlIdentity(t *testing.T) {
	h := setupRunHarness(t)
	h.Capture.mu.Lock()
	h.Capture.unavailable = true
	h.Capture.mu.Unlock()
	if _, err := h.Service.Submit(h.Ctx, "tenant-a", "failed-capture", submitFixture(h, "failed-capture")); err != agentrun.ErrDependencyUnavailable {
		t.Fatalf("capture failure mapping: %v", err)
	}
	var count int
	if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from runs)+(select count(*) from business_requests)+
		(select count(*) from budget_accounts where scope='family')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed capture left a control identity: count=%d error=%v", count, err)
	}
}

func TestRunAdmissionReusesCaptureAfterControlRollback(t *testing.T) {
	h := setupRunHarness(t)
	if _, err := h.Pool.Exec(h.Ctx, `create function reject_run_admission() returns trigger language plpgsql as $$
		begin raise exception 'injected control commit failure'; end $$;
		create trigger reject_run_admission before insert on runs for each row execute function reject_run_admission()`); err != nil {
		t.Fatal(err)
	}
	request := submitFixture(h, "capture-before-control")
	if _, err := h.Service.Submit(h.Ctx, "tenant-a", "same-capture-key", request); err != agentrun.ErrDependencyUnavailable {
		t.Fatalf("injected control failure not reported safely: %v", err)
	}
	var captured agentrun.SnapshotBinding
	h.Capture.mu.Lock()
	for _, snapshot := range h.Capture.snapshots {
		captured = snapshot
	}
	h.Capture.mu.Unlock()
	if captured.ID == "" {
		t.Fatal("test never crossed capture-before-control failure window")
	}
	var count int
	if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from runs)+(select count(*) from business_requests)+
		(select count(*) from budget_accounts where scope='family')+(select count(*) from run_operations)`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed admission left partial control records: count=%d error=%v", count, err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "drop trigger reject_run_admission on runs; drop function reject_run_admission()"); err != nil {
		t.Fatal(err)
	}
	accepted, err := h.Service.Submit(h.Ctx, "tenant-a", "same-capture-key", request)
	if err != nil || accepted.Reused || accepted.Run.SnapshotID != captured.ID {
		t.Fatalf("explicit replay did not bind first captured snapshot: %v", err)
	}
	if h.Capture.count() != 2 {
		t.Fatal("service hid a capture retry within a single request")
	}
}

func TestRunAdmissionRechecksBatchAfterLockWait(t *testing.T) {
	h := setupRunHarness(t)
	blocker, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(h.Ctx) }()
	if _, err := blocker.Exec(h.Ctx, "select account_id from budget_accounts where scope='tenant' and scope_key='tenant-a' for update"); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := h.Service.Submit(h.Ctx, "tenant-a", "blocked-admission", submitFixture(h, "blocked-admission"))
		finished <- err
	}()
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
		t.Fatal("admission never reached the real account row lock")
	}
	if _, err := blocker.Exec(h.Ctx, "update budget_accounts set valid_until=clock_timestamp()-interval '1 second' where scope='batch'"); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(h.Ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != agentrun.ErrBudgetExhausted {
			t.Fatalf("expired batch admitted after lock wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission did not finish after account lock release")
	}
}
