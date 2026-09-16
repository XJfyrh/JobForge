package integration

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func TestRunSupportReducedBatchCostCapAndImmutableHold(t *testing.T) {
	for _, cap := range []int64{2105343, 2105344, 2846003} {
		t.Run(fmt.Sprint(cap), func(t *testing.T) {
			h := setupSupportProfileBudgetHarness(t, "support-reduced-cap", cap)
			inspection := supportInspectionRequest(t, h)
			var batch agentrun.BudgetSpec
			for _, budget := range inspection.Budgets {
				if budget.Scope == "batch" {
					batch = budget
				}
			}
			if batch.Limits.CostMicroyuan != cap {
				t.Fatal("prepared cap did not persist")
			}
			var err error
			h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
			if err != nil {
				t.Fatal(err)
			}
			supportProfileCapture(t, h, "submit", "submit-reduced-cap", "", true)
			h.submit(t, "tenant-north", "reduced-cap")
			claimed := h.claim(t)
			for currentRunStep(claimed).Kind != "model_proposal" {
				result := supportStorageResult(t, h, claimed, "proposal")
				if _, err := h.Store.CommitStep(h.Ctx, h.Principal, supportStorageRequest(t, claimed, result)); err != nil {
					t.Fatal(err)
				}
				claimed.Checkpoint, err = h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
				if err != nil {
					t.Fatal(err)
				}
			}
			before := ledgerView(t, h, claimed.Lease)
			request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reserved, err := h.Store.ReserveCall(h.Ctx, h.Principal, request)
			if cap < 2105344 {
				if !errors.Is(err, agentrun.ErrBudgetExhausted) {
					t.Fatalf("insufficient batch cap accepted chat: %v", err)
				}
				after := ledgerView(t, h, claimed.Lease)
				var count int
				if !reflect.DeepEqual(before.Budget, after.Budget) || !before.UpdatedAt.Equal(after.UpdatedAt) {
					t.Fatal("rejected call partially charged an account or changed Run")
				}
				if err := h.Pool.QueryRow(h.Ctx, "select count(*) from physical_calls where physical_call_id=$1", request.PhysicalCallID).Scan(&count); err != nil || count != 0 {
					t.Fatal("rejected call created a ledger row", err)
				}
				return
			}
			if err != nil || !reserved.NewlyReserved || reserved.Reservation.Budget.CostMicroyuan != 2105344 {
				t.Fatal("sufficient reduced cap did not reserve the unchanged conservative bound", err)
			}
			unknown := &agentrun.ProviderAudit{SchemaVersion: 1, Provider: "deepseek",
				IdentityState: agentrun.ProviderIdentityUnavailable, UsageEvidence: agentrun.UsageEvidenceUnavailable,
				ReasoningState: agentrun.ReasoningUnavailable, ModeState: agentrun.ProviderModeUnavailable}
			report := auditBindReport(t, h, request, agentrun.CallReport{ProviderAudit: unknown})
			settled := auditSettle(t, h, report)
			if !settled.BatchFrozen || settled.BatchStopCode != agentrun.BatchStopChatUsageUnknown || settled.Reservation.UsageKnown {
				t.Fatal("reduced cap changed unknown freeze semantics")
			}
			beforeReplay, err := h.Store.InspectSupport(h.Ctx, inspection)
			if err != nil || !beforeReplay.MatchesConfig {
				t.Fatal("inspection lost reduced cap", err)
			}
			if err := h.Store.CreateBudget(h.Ctx, batch); err != nil {
				t.Fatal("same setup replay rejected", err)
			}
			batch.Limits.CostMicroyuan++
			if err := h.Store.CreateBudget(h.Ctx, batch); !errors.Is(err, agentrun.ErrConflict) {
				t.Fatal("setup replay expanded existing cap", err)
			}
			afterReplay, err := h.Store.InspectSupport(h.Ctx, inspection)
			if err != nil || !afterReplay.MatchesConfig || !reflect.DeepEqual(beforeReplay.Accounts, afterReplay.Accounts) {
				t.Fatal("setup replay changed usage, hold, freeze or limits", err)
			}
			after := ledgerView(t, h, claimed.Lease)
			if after.Budget.Batch.HeldCostMicroyuan != 2105344 || after.Budget.Batch.KnownCostMicroyuan != 0 ||
				after.Budget.Batch.Limits.CostMicroyuan != cap || after.Budget.Tenant.Limits.CostMicroyuan != 5000000 {
				t.Fatal("unknown full hold or independent limits changed")
			}
		})
	}
}
