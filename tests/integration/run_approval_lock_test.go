package integration

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func TestRunApprovalEarlierTerminalReconcileLockOrder(t *testing.T) {
	h := setupApprovalHarness(t)
	supportProfileCapture(t, h, "submit", "submit-before-authorization", "", true)
	root := h.submit(t, "tenant-north", "before-authorization")
	if _, err := h.Service.Cancel(h.Ctx, root.TenantID, root.ID, "cancel-root"); err != nil {
		t.Fatal(err)
	}
	firstResult, err := h.Store.Result(h.Ctx, root.TenantID, root.ID)
	if err != nil || firstResult.Disposition != "none" {
		t.Fatal("root no result", err)
	}
	supportProfileCapture(t, h, "retry", "retry-child", root.ID, true)
	child, err := h.Service.Retry(h.Ctx, root.TenantID, root.ID, "retry-child", agentrun.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	claimed := h.claim(t)
	recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	pending := approvalCommitProposal(t, h, claimed)
	approveRun(t, h, pending, "approve")
	claimed = h.claim(t)
	step := currentRunStep(claimed)
	binding, err := h.Store.AuthorizeAction(h.Ctx, h.Principal, claimed.Lease, step)
	if err != nil {
		t.Fatal(err)
	}
	receipt := approvalSyntheticReceipt(*binding.Action)
	permit, err := h.Store.ReserveReceiptQuery(h.Ctx, root.TenantID, root.ID, "reconcile", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(h.Ctx, 8*time.Second)
	defer cancel()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var pid int
	var operation string
	if err := tx.QueryRow(ctx, `select pg_backend_pid(),operation_id::text from action_authorizations where tenant_id=$1 and business_request_id=$2 for update`, root.TenantID, root.BusinessRequestID).Scan(&pid, &operation); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- h.Store.RecordReceiptQuery(ctx, root.TenantID, permit.ID, *binding.Action, &receipt, "found")
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var blocked bool
		if err := h.Pool.QueryRow(ctx, `select exists(select 1 from pg_stat_activity where $1=any(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("receipt transaction did not meet controlled authorization barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Equivalent CompleteAction transaction barrier: authorization is already
	// locked, and its first effect child is inserted next. An inverted effect ->
	// FK authorization order deadlocks here; the fixed reader waits before INSERT.
	raw, _ := json.Marshal(receipt)
	if _, err := tx.Exec(ctx, `insert into action_receipt_views(tenant_id,operation_id,receipt,receipt_hash,source,observed_at)
	 values($1,$2,$3,$4,'action_response',clock_timestamp())`, root.TenantID, operation, raw, receipt.ReceiptHash); err != nil {
		t.Fatal("authorization/effect lock inversion", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal("receipt query after completion barrier", err)
	}
	if _, err := h.Store.CompleteAction(h.Ctx, h.Principal, agentrun.CompleteActionRequest{Lease: claimed.Lease, Step: step, Receipt: receipt}); err != nil {
		t.Fatal("current child completion", err)
	}
	lastResult, err := h.Store.Result(h.Ctx, root.TenantID, root.ID)
	if err != nil || !reflect.DeepEqual(firstResult, lastResult) {
		t.Fatal("child authorization changed earlier terminal result", err)
	}
	effect, err := h.Store.Effect(h.Ctx, root.TenantID, root.ID)
	if err != nil || effect.State != "applied" || effect.Receipt.ReceiptHash != receipt.ReceiptHash {
		t.Fatal("first family effect changed", err)
	}
	view, err := h.Store.Get(h.Ctx, child.Run.TenantID, child.Run.ID)
	if err != nil || view.State != agentrun.Succeeded {
		t.Fatal("child completion", err)
	}
}

func TestRunApprovalLateQueryAndNewPermitFKLockOrder(t *testing.T) {
	h := setupApprovalHarness(t)
	r, action, _ := approvalAuthorizedCancelled(t, h, "query-fk-lock-order")
	first, err := h.Store.ReserveReceiptQuery(h.Ctx, r.TenantID, r.ID, "reconcile", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	// The old request really expires; no database clock/lease edits.
	time.Sleep(time.Until(first.Deadline) + 30*time.Millisecond)
	ctx, cancel := context.WithTimeout(h.Ctx, 6*time.Second)
	defer cancel()
	barrier, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = barrier.Rollback(context.Background()) }()
	var pid int
	if err := barrier.QueryRow(ctx, `select pg_backend_pid() from action_authorizations
		where tenant_id=$1 and operation_id=$2 for update`, r.TenantID, action.Authorization.OperationID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	nextID := uuid.NewString()
	reserved := make(chan error, 1)
	go func() {
		_, err := h.Store.ReserveReceiptQuery(ctx, r.TenantID, r.ID, "retry", nextID)
		reserved <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		if err := h.Pool.QueryRow(ctx, `select exists(select 1 from pg_stat_activity
			where $1=any(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new permit did not reach authorization FK barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Reserve must wait for its authorization identity before taking the gate.
	// The former gate -> INSERT/FK order would make this NOWAIT check fail.
	gate, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `select 1 from tenant_receipt_query_gates
		where tenant_id=$1 for update nowait`, r.TenantID); err != nil {
		_ = gate.Rollback(context.Background())
		t.Fatal("query gate acquired before authorization FK", err)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	recorded := make(chan error, 1)
	go func() { recorded <- h.Store.RecordReceiptQuery(ctx, r.TenantID, first.ID, action, nil, "absent") }()
	if err := barrier.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-reserved; err != nil {
		t.Fatal("new query permit", err)
	}
	if err := <-recorded; err != nil {
		t.Fatal("late old query", err)
	}
	var current string
	if err := h.Pool.QueryRow(ctx, `select query_id::text from tenant_receipt_query_gates
		where tenant_id=$1`, r.TenantID).Scan(&current); err != nil || current != nextID {
		t.Fatal("late release changed new query permit", err)
	}
	receipt := approvalSyntheticReceipt(action)
	if err := h.Store.RecordReceiptQuery(ctx, r.TenantID, first.ID, action, &receipt, "found"); !errors.Is(err, agentrun.ErrActionConflict) {
		t.Fatal("first query outcome changed", err)
	}
	effect, err := h.Store.Effect(ctx, r.TenantID, r.ID)
	if err != nil || effect.State != "unknown" {
		t.Fatal("conflicting late outcome stored an effect", err)
	}
	if err := h.Store.RecordReceiptQuery(ctx, r.TenantID, first.ID, action, nil, "absent"); err != nil {
		t.Fatal("identical outcome replay", err)
	}
}
