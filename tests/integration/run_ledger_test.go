package integration

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func ledgerAtStep(t *testing.T, h *runHarness, tenant, key, kind string) agentrun.ClaimedRun {
	t.Helper()
	h.submit(t, tenant, key)
	claimed := h.claim(t)
	for currentRunStep(claimed).Kind != kind {
		checkpointAdvance(t, h, &claimed, "proposal", false)
	}
	return claimed
}

func ledgerRequest(h *runHarness, claimed agentrun.ClaimedRun, subcall agentrun.Subcall, toolID string) agentrun.ReserveCallRequest {
	id := uuid.NewString()
	return agentrun.ReserveCallRequest{Lease: claimed.Lease, Step: currentRunStep(claimed), PhysicalCallID: id,
		ToolInvocationID: toolID, Subcall: subcall, ParameterHash: agentrun.Fingerprint("fixture-parameters", id), PriceHash: h.Profile.Pricing.Hash}
}

func ledgerReserve(t *testing.T, h *runHarness, request agentrun.ReserveCallRequest) agentrun.CallReservation {
	t.Helper()
	response, err := h.Store.ReserveCall(h.Ctx, request.Lease.WorkerID, request)
	if err != nil || !response.NewlyReserved {
		t.Fatalf("reserve fixture call: %+v %v", response, err)
	}
	return response.Reservation
}

func ledgerView(t *testing.T, h *runHarness, lease agentrun.Lease) agentrun.Run {
	t.Helper()
	view, err := h.Store.Get(h.Ctx, lease.TenantID, lease.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func ledgerAccounts(view agentrun.Run) [3]agentrun.Account {
	return [3]agentrun.Account{view.Budget.Family, view.Budget.Tenant, view.Budget.Batch}
}

func ledgerUsage(input, output int64) agentrun.UsageReport {
	usage := agentrun.UsageReport{InputTokens: input, OutputTokens: output, ReceiptHash: agentrun.Fingerprint("synthetic-usage-receipt")}
	usage.UsageHash = usage.Hash()
	return usage
}

func ledgerObserve(t *testing.T, h *runHarness, request agentrun.ReserveCallRequest, transport, business string, usage *agentrun.UsageReport) agentrun.CallReservation {
	t.Helper()
	status, code := 0, ""
	if transport == "response" {
		status = 200
	}
	if business == "rejected" {
		code = "MODEL_PROTOCOL_ERROR"
	}
	response, err := h.Store.ObserveCall(h.Ctx, request.Lease.WorkerID, agentrun.ObserveCallRequest{Lease: request.Lease,
		Step: request.Step, PhysicalCallID: request.PhysicalCallID, TransportOutcome: transport, BusinessOutcome: business,
		HTTPStatus: status, ErrorCode: code, UsageKnown: usage != nil, Usage: usage})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestRunLedgerThreeScopeReservationCompetition(t *testing.T) {
	h := setupRunHarness(t)
	first := ledgerAtStep(t, h, "tenant-a", "budget-racer-a", "model_proposal")
	second := ledgerAtStep(t, h, "tenant-b", "budget-racer-b", "model_proposal")
	if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set limit_chat=used_chat+1 where scope='batch'"); err != nil {
		t.Fatal(err)
	}
	claims := []agentrun.ClaimedRun{first, second}
	before := []agentrun.Run{ledgerView(t, h, first.Lease), ledgerView(t, h, second.Lease)}
	type outcome struct {
		index int
		err   error
	}
	finished := make(chan outcome, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, claimed := range claims {
		request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
		wg.Go(func() {
			<-start
			_, err := h.Store.ReserveCall(h.Ctx, h.Principal, request)
			finished <- outcome{index: i, err: err}
		})
	}
	close(start)
	wg.Wait()
	close(finished)
	winners, rejected := 0, 0
	for result := range finished {
		after := ledgerView(t, h, claims[result.index].Lease)
		if result.err == nil {
			winners++
			if after.Budget.Family.Used.Chat != before[result.index].Budget.Family.Used.Chat+1 ||
				after.Budget.Tenant.Used.Chat != before[result.index].Budget.Tenant.Used.Chat+1 {
				t.Fatal("winner did not reserve family and tenant together")
			}
		} else if errors.Is(result.err, agentrun.ErrBudgetExhausted) {
			rejected++
			if after.Budget.Family != before[result.index].Budget.Family || after.Budget.Tenant != before[result.index].Budget.Tenant {
				t.Fatal("losing reservation partially charged family or tenant")
			}
		} else {
			t.Fatalf("unexpected concurrent reservation error: %v", result.err)
		}
		if after.Budget.Batch.Used.Chat != 1 {
			t.Fatal("shared batch exceeded its final chat allowance")
		}
	}
	if winners != 1 || rejected != 1 {
		t.Fatalf("last shared allowance: winners=%d rejected=%d", winners, rejected)
	}
}

func TestRunLedgerEveryScopeRejectsWithoutPartialReservation(t *testing.T) {
	for index, scope := range []string{"family", "tenant", "batch"} {
		t.Run(scope, func(t *testing.T) {
			h := setupRunHarness(t)
			claimed := ledgerAtStep(t, h, "tenant-a", "atomic-budget", "model_proposal")
			accounts := ledgerAccounts(ledgerView(t, h, claimed.Lease))
			if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set limit_physical_http=used_physical_http where account_id=$1", accounts[index].ID); err != nil {
				t.Fatal(err)
			}
			before := ledgerView(t, h, claimed.Lease)
			request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, request); !errors.Is(err, agentrun.ErrBudgetExhausted) {
				t.Fatalf("exhausted %s accepted: %v", scope, err)
			}
			after := ledgerView(t, h, claimed.Lease)
			if !reflect.DeepEqual(before.Budget, after.Budget) || !before.UpdatedAt.Equal(after.UpdatedAt) {
				t.Fatal("failed three-scope reservation left a partial debit or Run mutation")
			}
			var count int
			if err := h.Pool.QueryRow(h.Ctx, "select count(*) from physical_calls where physical_call_id=$1", request.PhysicalCallID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed reservation left a call record: %d %v", count, err)
			}
		})
	}
}

func TestRunLedgerLostReservationACKNeverCreatesSecondPermit(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "lost-reserve-ack", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	first := ledgerReserve(t, h, request)
	before := ledgerView(t, h, claimed.Lease)
	duplicate, err := h.Store.ReserveCall(h.Ctx, h.Principal, request)
	if err != nil || duplicate.NewlyReserved || !reflect.DeepEqual(first, duplicate.Reservation) {
		t.Fatalf("lost reserve ACK became a new permit: %+v %v", duplicate, err)
	}
	conflict := request
	conflict.ParameterHash = agentrun.Fingerprint("different physical parameters")
	if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, conflict); !errors.Is(err, agentrun.ErrCallConflict) {
		t.Fatalf("conflicting command content accepted: %v", err)
	}
	newRequest := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, newRequest); !errors.Is(err, agentrun.ErrCallConflict) {
		t.Fatalf("second active physical call accepted: %v", err)
	}
	ledgerObserve(t, h, request, "unknown", "unknown", nil)
	afterUnknown := ledgerView(t, h, claimed.Lease)
	if !reflect.DeepEqual(before.Budget, afterUnknown.Budget) || before.CursorVersion != afterUnknown.CursorVersion {
		t.Fatal("unknown observation refunded budget or advanced a step")
	}
	ledgerReserve(t, h, newRequest)
	afterRedo := ledgerAccounts(ledgerView(t, h, claimed.Lease))
	for i, account := range ledgerAccounts(afterUnknown) {
		if afterRedo[i].Used.Chat != account.Used.Chat+1 || afterRedo[i].Used.PhysicalHTTP != account.Used.PhysicalHTTP+1 ||
			afterRedo[i].HeldTokens != account.HeldTokens+first.Budget.TotalTokens || afterRedo[i].HeldCostMicroyuan != account.HeldCostMicroyuan+first.Budget.CostMicroyuan {
			t.Fatal("redo did not use a fresh physical identity and full three-scope hold")
		}
	}
	if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancel-uncertain-call"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, newRequest); !errors.Is(err, agentrun.ErrCancelRequested) {
		t.Fatalf("cancelled duplicate reservation authorized: %v", err)
	}
	if _, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease); err != nil {
		t.Fatal(err)
	}
	terminal := ledgerView(t, h, claimed.Lease)
	if terminal.State != agentrun.Cancelled || ledgerAccounts(terminal) != afterRedo {
		t.Fatal("closing an unobserved active call refunded its conservative hold")
	}
	var unknown int
	if err := h.Pool.QueryRow(h.Ctx, "select count(*) from physical_calls where run_id=$1 and kind='chat' and status='unknown'", claimed.Lease.RunID).Scan(&unknown); err != nil || unknown != 2 {
		t.Fatalf("unacknowledged calls not retained as unknown: %d %v", unknown, err)
	}
}

func TestRunLedgerLogicalToolsAndEverySearchSubcallNeedAuthorization(t *testing.T) {
	for cancelAt := 0; cancelAt < 4; cancelAt++ {
		t.Run(string(rune('0'+cancelAt)), func(t *testing.T) {
			h := setupRunHarness(t)
			claimed := ledgerAtStep(t, h, "tenant-a", "search-dispatch-cancel", "search_policy")
			tool := agentrun.BeginToolRequest{Lease: claimed.Lease, Step: currentRunStep(claimed), ToolInvocationID: uuid.NewString()}
			first, err := h.Store.BeginTool(h.Ctx, h.Principal, tool)
			if err != nil || !first.NewlyStarted {
				t.Fatal(err)
			}
			before := ledgerView(t, h, claimed.Lease)
			duplicate, err := h.Store.BeginTool(h.Ctx, h.Principal, tool)
			if err != nil || duplicate.NewlyStarted || duplicate.ToolInvocationID != tool.ToolInvocationID {
				t.Fatalf("logical duplicate restarted: %+v %v", duplicate, err)
			}
			if !reflect.DeepEqual(before.Budget, ledgerView(t, h, claimed.Lease).Budget) {
				t.Fatal("duplicate logical invocation consumed quota")
			}
			sequence := agentrun.ToolSequence("search_policy")
			wrong := ledgerRequest(h, claimed, agentrun.SubcallSearchPolicy, tool.ToolInvocationID)
			if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, wrong); !errors.Is(err, agentrun.ErrCallConflict) {
				t.Fatalf("tool permit bypassed required metadata/embedding: %v", err)
			}
			for i := 0; i < cancelAt; i++ {
				request := ledgerRequest(h, claimed, sequence[i], tool.ToolInvocationID)
				ledgerReserve(t, h, request)
				ledgerObserve(t, h, request, "response", "accepted", nil)
			}
			beforeCancel := ledgerView(t, h, claimed.Lease)
			if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancel-subcall"); err != nil {
				t.Fatal(err)
			}
			request := ledgerRequest(h, claimed, sequence[cancelAt], tool.ToolInvocationID)
			if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, request); !errors.Is(err, agentrun.ErrCancelRequested) {
				t.Fatalf("subcall %d authorized after cancel: %v", cancelAt, err)
			}
			if !reflect.DeepEqual(beforeCancel.Budget, ledgerView(t, h, claimed.Lease).Budget) {
				t.Fatal("cancelled subcall changed budget")
			}
		})
	}
}

func TestRunLedgerRejectedBusinessOutputStillSettlesValidUsage(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "usage-rejected-output", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	reservation := ledgerReserve(t, h, request)
	before := ledgerView(t, h, claimed.Lease)
	usage := ledgerUsage(10, 2)
	observed := ledgerObserve(t, h, request, "response", "rejected", &usage)
	if !observed.UsageKnown {
		t.Fatal("valid metering was discarded with rejected business output")
	}
	after := ledgerView(t, h, claimed.Lease)
	cost, err := agentrun.UsageCost(h.Profile.Pricing, usage)
	if err != nil {
		t.Fatal(err)
	}
	for i, old := range ledgerAccounts(before) {
		account := ledgerAccounts(after)[i]
		if account.Used.Chat != old.Used.Chat || account.Used.PhysicalHTTP != old.Used.PhysicalHTTP ||
			account.HeldTokens != old.HeldTokens-reservation.Budget.TotalTokens || account.KnownTokens != old.KnownTokens+12 ||
			account.HeldCostMicroyuan != old.HeldCostMicroyuan-reservation.Budget.CostMicroyuan || account.KnownCostMicroyuan != old.KnownCostMicroyuan+cost {
			t.Fatal("valid rejected-output usage was not settled consistently across all accounts")
		}
	}
	if after.CursorVersion != before.CursorVersion || after.State != agentrun.Running {
		t.Fatal("usage settlement advanced business progress")
	}
	duplicate, err := h.Store.SettleUsage(h.Ctx, h.Principal, agentrun.SettleUsageRequest{Lease: claimed.Lease, PhysicalCallID: request.PhysicalCallID, Usage: &usage})
	if err != nil || duplicate.NewlySettled || !duplicate.Reservation.UsageKnown || !reflect.DeepEqual(after.Budget, ledgerView(t, h, claimed.Lease).Budget) {
		t.Fatalf("duplicate settlement changed balances: %+v %v", duplicate, err)
	}
	conflict := ledgerUsage(11, 2)
	if _, err := h.Store.SettleUsage(h.Ctx, h.Principal, agentrun.SettleUsageRequest{Lease: claimed.Lease, PhysicalCallID: request.PhysicalCallID, Usage: &conflict}); !errors.Is(err, agentrun.ErrCallConflict) {
		t.Fatalf("conflicting usage accepted: %v", err)
	}
}

func TestRunLedgerLateUsagePreservesNewAttemptAndTerminalRun(t *testing.T) {
	h := setupRunHarness(t)
	old := ledgerAtStep(t, h, "tenant-a", "late-usage", "model_proposal")
	oldRequest := ledgerRequest(h, old, agentrun.SubcallChat, "")
	ledgerReserve(t, h, oldRequest)
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, oldRequest.Step, string(agentrun.ErrDependencyUnavailable)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update runs set next_attempt_at=clock_timestamp()-interval '1 second' where run_id=$1", old.Lease.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
		t.Fatal(err)
	}
	secondPrincipal := "contract-worker-2"
	secondSession, err := h.Store.Register(h.Ctx, secondPrincipal, uuid.NewString(), "fixture-v1")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := h.Store.Claim(h.Ctx, secondPrincipal, secondSession.ID)
	if err != nil || claimed == nil || claimed.Lease.AttemptNo != old.Lease.AttemptNo+1 || len(claimed.Checkpoint.Steps) != 4 {
		t.Fatalf("recovered checkpoint: %+v %v", claimed, err)
	}
	newRequest := ledgerRequest(h, *claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, newRequest)
	if _, err := h.Pool.Exec(h.Ctx, "update worker_sessions set seen_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' where session_id=$1", old.Lease.SessionID); err != nil {
		t.Fatal(err)
	}
	before := ledgerView(t, h, claimed.Lease)
	usage := ledgerUsage(10, 2)
	settled, err := h.Store.SettleUsage(h.Ctx, h.Principal, agentrun.SettleUsageRequest{Lease: old.Lease, PhysicalCallID: oldRequest.PhysicalCallID, Usage: &usage})
	if err != nil || !settled.NewlySettled || !settled.Reservation.UsageKnown {
		t.Fatalf("expired old-session usage rejected: %+v %v", settled, err)
	}
	checkpoint, err := h.Store.GetCheckpoint(h.Ctx, secondPrincipal, claimed.Lease)
	if err != nil || checkpoint.Authority.ActiveCallID == nil || *checkpoint.Authority.ActiveCallID != newRequest.PhysicalCallID ||
		checkpoint.Run.CursorVersion != before.CursorVersion || !checkpoint.Run.UpdatedAt.Equal(before.UpdatedAt) || checkpoint.Run.State != before.State {
		t.Fatalf("late accounting disturbed new attempt: %v", err)
	}
	if _, err := h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: old.Lease, Step: oldRequest.Step,
		PhysicalCallID: oldRequest.PhysicalCallID, TransportOutcome: "response", BusinessOutcome: "accepted", HTTPStatus: 200}); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("old execution regained business authority: %v", err)
	}
	if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancel-late-accounting"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.AcknowledgeStop(h.Ctx, secondPrincipal, claimed.Lease); err != nil {
		t.Fatal(err)
	}
	terminal := ledgerView(t, h, claimed.Lease)
	settled, err = h.Store.SettleUsage(h.Ctx, secondPrincipal, agentrun.SettleUsageRequest{Lease: claimed.Lease, PhysicalCallID: newRequest.PhysicalCallID, Usage: &usage})
	if err != nil || !settled.NewlySettled {
		t.Fatalf("terminal usage rejected: %+v %v", settled, err)
	}
	after := ledgerView(t, h, claimed.Lease)
	if after.State != agentrun.Cancelled || after.CursorVersion != terminal.CursorVersion || !after.UpdatedAt.Equal(terminal.UpdatedAt) {
		t.Fatal("terminal metering changed Run execution outcome")
	}
	retried, err := h.Service.Retry(h.Ctx, old.Lease.TenantID, old.Lease.RunID, "retry-budget-family", agentrun.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 3600})
	if err != nil || retried.Run.Budget.Family != after.Budget.Family || retried.Run.CursorVersion != 0 || retried.Run.ID == after.ID {
		t.Fatalf("manual retry reset shared family budget: %v", err)
	}
}

func TestRunLedgerUsageAnomalyFreezesAllScopesWithoutRefund(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "usage-anomaly", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	reservation := ledgerReserve(t, h, request)
	ledgerObserve(t, h, request, "unknown", "unknown", nil)
	before := ledgerView(t, h, claimed.Lease)
	usage := ledgerUsage(reservation.Budget.InputTokens+1, 1)
	response, err := h.Store.SettleUsage(h.Ctx, h.Principal, agentrun.SettleUsageRequest{Lease: claimed.Lease, PhysicalCallID: request.PhysicalCallID, Usage: &usage})
	if err != nil || response.NewlySettled || response.Reservation.UsageKnown || !response.Reservation.MeasurementAnomaly ||
		response.Reservation.ReportedUsage == nil || *response.Reservation.ReportedUsage != usage {
		t.Fatalf("anomalous usage not preserved explicitly: %+v %v", response, err)
	}
	after := ledgerView(t, h, claimed.Lease)
	for i, account := range ledgerAccounts(after) {
		old := ledgerAccounts(before)[i]
		if !account.Frozen || account.Used != old.Used || account.HeldTokens != old.HeldTokens || account.HeldCostMicroyuan != old.HeldCostMicroyuan ||
			account.KnownTokens != old.KnownTokens || account.KnownCostMicroyuan != old.KnownCostMicroyuan {
			t.Fatal("anomalous metering refunded/rounded exposure or failed to freeze a scope")
		}
	}
	if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, claimed, agentrun.SubcallChat, "")); !errors.Is(err, agentrun.ErrBudgetExhausted) {
		t.Fatalf("frozen anomaly permitted new dispatch: %v", err)
	}
	if _, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease); err != nil {
		t.Fatalf("frozen accounting prevented protected recovery read: %v", err)
	}
}

func TestRunLedgerSettlementWindowAndWrongPrincipalNeverRefund(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "usage-window", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	usage := ledgerUsage(10, 2)
	before := ledgerView(t, h, claimed.Lease)
	settlement := agentrun.SettleUsageRequest{Lease: claimed.Lease, PhysicalCallID: request.PhysicalCallID, Usage: &usage}
	if _, err := h.Store.SettleUsage(h.Ctx, "contract-worker-2", settlement); !errors.Is(err, agentrun.ErrForbidden) {
		t.Fatalf("different principal settled another call: %v", err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update physical_calls set reserved_at=clock_timestamp()-interval '30 days' where physical_call_id=$1", request.PhysicalCallID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.SettleUsage(h.Ctx, h.Principal, settlement); !errors.Is(err, agentrun.ErrCallSettlementExpired) {
		t.Fatalf("expired accounting window accepted: %v", err)
	}
	after := ledgerView(t, h, claimed.Lease)
	if !reflect.DeepEqual(before.Budget, after.Budget) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("failed/expired settlement modified exposure or execution")
	}
}

func TestRunLedgerReservationRechecksBatchAfterAccountLockWait(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "reserve-lock-expiry", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	before := ledgerView(t, h, claimed.Lease)
	blocker, err := h.Pool.Begin(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(h.Ctx) }()
	if _, err := blocker.Exec(h.Ctx, "select account_id from budget_accounts where account_id=$1 for update", before.Budget.Family.ID); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		_, err := h.Store.ReserveCall(h.Ctx, h.Principal, request)
		finished <- err
	}()
	deadline, blocked := time.Now().Add(5*time.Second), false
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
		t.Fatal("reservation never reached the real account lock")
	}
	if _, err := blocker.Exec(h.Ctx, "update budget_accounts set valid_until=clock_timestamp()-interval '1 second' where scope='batch'"); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(h.Ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if !errors.Is(err, agentrun.ErrBudgetExhausted) {
			t.Fatalf("expired batch authorized after lock wait: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not finish after account lock release")
	}
	after := ledgerView(t, h, claimed.Lease)
	if !reflect.DeepEqual(before.Budget, after.Budget) || before.CursorVersion != after.CursorVersion || !before.UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatal("rejected post-wait authorization partially applied")
	}
}
