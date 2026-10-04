package integration

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func TestRunRecoveryReservedFreeToolUsesNewIDsAndKeepsCounters(t *testing.T) {
	h := setupRecoveryHarness(t)
	old := recoveryAtDecision(t, h, "free-tool-recovery")
	chat, _ := recoveryChat(t, h, old, true)
	recoveryCommit(t, h, &old, recoveryToolDecision(t, old, chat.PhysicalCallID, "get_order"))
	tool := uuid.NewString()
	if _, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: old.Lease, Step: currentRunStep(old), ToolInvocationID: tool}); err != nil {
		t.Fatal(err)
	}
	oldCall := ledgerRequest(h, old, agentrun.SubcallGetOrder, tool)
	ledgerReserve(t, h, oldCall)
	before := ledgerView(t, h, old.Lease)
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, currentRunStep(old), "TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
	current := h.claim(t)
	newTool := uuid.NewString()
	if _, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: current.Lease, Step: currentRunStep(current), ToolInvocationID: newTool}); err != nil {
		t.Fatal(err)
	}
	newCall := ledgerRequest(h, current, agentrun.SubcallGetOrder, newTool)
	ledgerReserve(t, h, newCall)
	after := ledgerView(t, h, current.Lease)
	for i, oldAccount := range ledgerAccounts(before) {
		account := ledgerAccounts(after)[i]
		if account.Used.LogicalTools != oldAccount.Used.LogicalTools+1 || account.Used.BusinessToolHTTP != oldAccount.Used.BusinessToolHTTP+1 || account.KnownCostMicroyuan != oldAccount.KnownCostMicroyuan || account.HeldCostMicroyuan != oldAccount.HeldCostMicroyuan {
			t.Fatal("free tool recovery reset or refunded a scope")
		}
	}
	calls, err := h.Store.Calls(h.Ctx, current.Lease.TenantID, current.Lease.RunID)
	if err != nil || len(calls.Items) != 3 || oldCall.PhysicalCallID == newCall.PhysicalCallID || tool == newTool {
		t.Fatal("tool recovery reused or lost old physical identity", err)
	}
	var oldUnreported bool
	if err := h.Pool.QueryRow(h.Ctx, "select report_hash is null and observed_at is null from physical_calls where physical_call_id=$1", oldCall.PhysicalCallID).Scan(&oldUnreported); err != nil || !oldUnreported {
		t.Fatal("old unknown tool was fabricated as observed", err)
	}
}

func TestRunRecoveryDoesNotResetAnyScopeAtChatTokenOrCostBoundary(t *testing.T) {
	for _, scope := range []string{"family", "tenant", "batch"} {
		for _, dimension := range []string{"chat", "tokens", "cost_microyuan"} {
			t.Run(scope+"/"+dimension, func(t *testing.T) {
				h := setupRecoveryHarness(t)
				old := recoveryAtDecision(t, h, "budget-recovery")
				recoveryChat(t, h, old, true)
				if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, currentRunStep(old), "TIMEOUT"); err != nil {
					t.Fatal(err)
				}
				recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
				current := h.claim(t)
				// These are explicit quota-policy fixtures, never timing changes.
				if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set limit_"+dimension+"=used_"+dimension+" where scope=$1", scope); err != nil {
					t.Fatal(err)
				}
				before := ledgerView(t, h, current.Lease)
				request := ledgerRequest(h, current, agentrun.SubcallChat, "")
				if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, request); !errors.Is(err, agentrun.ErrBudgetExhausted) {
					t.Fatalf("recovery bypassed %s %s: %v", scope, dimension, err)
				}
				if !reflect.DeepEqual(before.Budget, ledgerView(t, h, current.Lease).Budget) {
					t.Fatal("blocked recovery left a partial reservation")
				}
			})
		}
	}
}
