package integration

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func TestRunProviderAuditGuardRequiresCommittedOriginalStep(t *testing.T) {
	h := setupAuditHarness(t)
	first := auditAtModelStep(t, h, "tenant-a", "guard-first")
	second := auditAtModelStep(t, h, "tenant-b", "guard-second")
	request := ledgerRequest(h, first, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	report := auditReportRequest(t, h, request, 10, 5)
	next := ledgerRequest(h, second, agentrun.SubcallChat, "")
	for _, stage := range []string{"reservation", "report", "observation"} {
		switch stage {
		case "report":
			auditSettle(t, h, report)
		case "observation":
			auditObserve(t, h, request, report, "accepted")
		}
		before := ledgerView(t, h, second.Lease)
		if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, next); !errors.Is(err, agentrun.ErrBudgetExhausted) {
			t.Fatalf("stage %s released next reservation: %v", stage, err)
		}
		after := ledgerView(t, h, second.Lease)
		if !reflect.DeepEqual(before.Budget, after.Budget) || !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Fatalf("stage %s partially reserved after rejection", stage)
		}
		if stage != "observation" {
			replay, err := h.Store.ReserveCall(h.Ctx, h.Principal, request)
			if err != nil || replay.NewlyReserved || replay.Reservation.PhysicalCallID != request.PhysicalCallID {
				t.Fatalf("same reservation confirmation deadlocked against its own pending guard: %+v %v", replay, err)
			}
		}
	}
	commit := auditCommitChat(t, h, &first, request, false)
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, commit); err != nil {
		t.Fatalf("lost commit ACK confirmation failed: %v", err)
	}
	ledgerReserve(t, h, next)
}

func TestRunProviderAuditGuardConcurrentReserveAuthorizesOnlyOne(t *testing.T) {
	h := setupAuditHarness(t)
	first := auditAtModelStep(t, h, "tenant-a", "reserve-a")
	second := auditAtModelStep(t, h, "tenant-b", "reserve-b")
	requests := []agentrun.ReserveCallRequest{ledgerRequest(h, first, agentrun.SubcallChat, ""), ledgerRequest(h, second, agentrun.SubcallChat, "")}
	results := make([]agentrun.ReserveCallResponse, 2)
	errorsFound := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range requests {
		wg.Go(func() {
			<-start
			results[i], errorsFound[i] = h.Store.ReserveCall(h.Ctx, h.Principal, requests[i])
		})
	}
	close(start)
	wg.Wait()
	winners, blocked := 0, 0
	for i, err := range errorsFound {
		if err == nil && results[i].NewlyReserved {
			winners++
		} else if errors.Is(err, agentrun.ErrBudgetExhausted) {
			blocked++
		} else {
			t.Fatalf("unexpected concurrent authorization: %+v %v", results[i], err)
		}
	}
	if winners != 1 || blocked != 1 {
		t.Fatalf("batch row lock failed to serialize pending chat: winners=%d blocked=%d", winners, blocked)
	}
}

func TestRunProviderAuditGuardBlocksNewToolBeforeCharging(t *testing.T) {
	h := setupAuditHarness(t)
	first := auditAtModelStep(t, h, "tenant-a", "tool-guard-first")
	h.submit(t, "tenant-b", "tool-guard-second")
	second := h.claim(t)
	checkpointAdvance(t, h, &second, "proposal", false)
	if currentRunStep(second).Kind != "get_order" {
		t.Fatal("fixture did not reach first tool")
	}
	request := ledgerRequest(h, first, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	before := ledgerView(t, h, second.Lease)
	if _, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: second.Lease, Step: currentRunStep(second), ToolInvocationID: uuid.NewString()}); !errors.Is(err, agentrun.ErrBudgetExhausted) {
		t.Fatalf("pending chat permitted next logical tool: %v", err)
	}
	if !reflect.DeepEqual(before.Budget, ledgerView(t, h, second.Lease).Budget) {
		t.Fatal("rejected logical tool charged accounts")
	}
}

func TestRunProviderAuditGuardExactSecondCorrectionFailure(t *testing.T) {
	for _, variant := range []string{"complete", "missing_observation", "wrong_failure", "wrong_run_fence", "wrong_attempt_worker", "wrong_input", "wrong_step", "cancelled"} {
		t.Run(variant, func(t *testing.T) {
			h := setupAuditHarness(t)
			claimed := auditAtModelStep(t, h, "tenant-a", "terminal-"+variant)
			first := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			ledgerReserve(t, h, first)
			firstReport := auditReportRequest(t, h, first, 10, 5)
			auditSettle(t, h, firstReport)
			auditObserve(t, h, first, firstReport, "rejected")
			auditCommitChat(t, h, &claimed, first, true)
			second := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			ledgerReserve(t, h, second)
			report := auditReportRequest(t, h, second, 10, 5)
			auditSettle(t, h, report)
			if variant != "missing_observation" {
				auditObserve(t, h, second, report, "rejected")
			}
			if variant == "cancelled" {
				if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancelled-correction"); err != nil {
					t.Fatal(err)
				}
				if _, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease); err != nil {
					t.Fatal(err)
				}
			} else {
				reason := "MODEL_PROTOCOL_ERROR"
				if variant == "wrong_failure" {
					reason = "EXECUTOR_PROTOCOL_ERROR"
				}
				if _, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, second.Step, reason); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "wrong_attempt_worker" {
				other, err := h.Store.Register(h.Ctx, "audit-worker-2", uuid.NewString(), h.Profile.ExecutorVersion)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := h.Pool.Exec(h.Ctx, "update run_attempts set worker_id=$2,session_id=$3 where run_id=$1", claimed.Lease.RunID, other.WorkerID, other.ID); err != nil {
					t.Fatal(err)
				}
			}
			mutations := map[string]string{
				"wrong_run_fence": "update runs set fencing_token=fencing_token+1 where run_id=$1",
				"wrong_input":     "update runs set next_input_hash=repeat('a',64) where run_id=$1",
				"wrong_step":      "update runs set next_step_id=gen_random_uuid() where run_id=$1",
			}
			if mutation := mutations[variant]; mutation != "" {
				if _, err := h.Pool.Exec(h.Ctx, mutation, claimed.Lease.RunID); err != nil {
					t.Fatal(err)
				}
			}
			next := h.submit(t, "tenant-b", "next-case-"+variant)
			result, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
			if variant == "complete" {
				if err != nil || result == nil || result.Lease.RunID != next.ID {
					t.Fatalf("exact failed second correction did not release next case: %+v %v", result, err)
				}
			} else if !errors.Is(err, agentrun.ErrBudgetExhausted) || result != nil {
				t.Fatalf("incomplete terminal facts released next case: %+v %v", result, err)
			}
		})
	}
}
