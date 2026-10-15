package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func bundleStorageContent(t *testing.T, snapshot agentrun.SnapshotBinding) (json.RawMessage, []string) {
	t.Helper()
	var ticket business.Ticket
	if json.Unmarshal(snapshot.Ticket, &ticket) != nil {
		t.Fatal("ticket")
	}
	hits, refs := []business.PolicyHit{}, []string{}
	for _, alias := range business.PolicyBundleAliases() {
		ref := "business-policy:" + snapshot.IndexID + ":" + alias
		hits = append(hits, business.PolicyHit{IndexID: snapshot.IndexID, ChunkID: alias, PolicyVersion: ticket.PolicyVersion, EvidenceRef: ref, Source: "synthetic#" + alias, Text: "Synthetic protocol fixture; not a business-quality assertion."})
		refs = append(refs, ref)
	}
	return supportStorageJSON(t, map[string]any{"snapshot_id": snapshot.ID, "matches": hits}), refs
}

func bundleAtRead(t *testing.T, h *runHarness, withOrder bool) agentrun.ClaimedRun {
	t.Helper()
	var err error
	h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	supportProfileCapture(t, h, "submit", "submit-bundle", "", withOrder)
	h.submit(t, "tenant-north", "bundle")
	claimed := h.claim(t)
	recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	if currentRunStep(claimed).Kind != "read_policy_bundle" {
		t.Fatal("bundle not scheduled")
	}
	return claimed
}

func bundleAtFinal(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun) agentrun.ClaimedRun {
	t.Helper()
	if h.Profile.Strategy == agentrun.SupportFixedStrategy {
		for currentRunStep(claimed).Kind != "model_proposal" {
			recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
		}
		return claimed
	}
	for _, tool := range []string{"get_order", "get_delivery"} {
		chat, _ := recoveryChat(t, h, claimed, true)
		recoveryCommit(t, h, &claimed, recoveryToolDecision(t, claimed, chat.PhysicalCallID, tool))
		recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	}
	chat, _ := recoveryChat(t, h, claimed, true)
	marker := agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: chat.PhysicalCallID, EvidenceRefs: []string{}, Content: json.RawMessage(`{"type":"investigation_complete","draft":{"decision":"proposal","action":"escalate","conclusion":"conflicting","requested_fields":[],"target_ticket_status":"escalated","claims":[]}}`)}
	recoveryCommit(t, h, &claimed, marker)
	return claimed
}

func TestRunPolicyBundleBothStrategies(t *testing.T) {
	for _, schema := range []int{16, 17, 18} {
		t.Run(fmt.Sprint(schema), func(t *testing.T) {
			h := setupAdjudicationProfileHarness(t, schema == 17, schema)
			claimed := bundleAtRead(t, h, true)
			before := ledgerView(t, h, claimed.Lease).Budget.Family.Used
			result := supportStorageResult(t, h, claimed, "proposal")
			req := recoveryCommit(t, h, &claimed, result)
			if _, err := h.Store.CommitStep(h.Ctx, h.Principal, req); err != nil {
				t.Fatal("idempotent bundle", err)
			}
			after := ledgerView(t, h, claimed.Lease).Budget.Family.Used
			if after.LogicalTools-before.LogicalTools != 1 || after.PhysicalHTTP-before.PhysicalHTTP != 1 || after.Chat != before.Chat || after.QueryEmbedding != before.QueryEmbedding {
				t.Fatal("bundle accounting", before, after)
			}
			claimed = bundleAtFinal(t, h, claimed)
			call := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reserved := ledgerReserve(t, h, call)
			if reserved.Budget.OutputTokens != 16384 || reserved.CallDeadline.Sub(reserved.ReservedAt) != 120*time.Second {
				t.Fatal("final permit")
			}
			report := adjudicationReport(t, h, call, 8000)
			auditSettle(t, h, report)
			auditObserve(t, h, call, report, "accepted")
			recoveryCommit(t, h, &claimed, adjudicationProposal(t, claimed, call.PhysicalCallID))
			recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
			calls, err := h.Store.Calls(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
			if err != nil || calls.BatchFrozen {
				t.Fatal("calls", err)
			}
			bundles, thinking := 0, 0
			for _, c := range calls.Items {
				if c.StepKind == "read_policy_bundle" {
					bundles++
				}
				if c.StepKind == "model_proposal" {
					thinking++
				}
				if c.HeldCostMicroyuan != 0 {
					t.Fatal("unsettled", c.StepKind)
				}
			}
			if bundles != 1 || thinking != 1 {
				t.Fatal("duplicated calls", bundles, thinking)
			}
		})
	}
}

func TestRunPolicyBundleRecoveryCancellationAndBudget(t *testing.T) {
	for _, boundary := range []string{"before_read", "confirmed_uncommitted", "committed", "cancel", "budget"} {
		t.Run(boundary, func(t *testing.T) {
			h := setupAdjudicationProfileHarness(t, false, 16)
			claimed := bundleAtRead(t, h, true)
			if boundary == "cancel" {
				if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancel-bundle"); err != nil {
					t.Fatal(err)
				}
				if _, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: claimed.Lease, Step: currentRunStep(claimed), ToolInvocationID: uuid.NewString()}); err == nil {
					t.Fatal("cancelled read")
				}
				return
			}
			if boundary == "budget" {
				if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set used_logical_tools=limit_logical_tools where scope='family'"); err != nil {
					t.Fatal(err)
				}
				if _, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: claimed.Lease, Step: currentRunStep(claimed), ToolInvocationID: uuid.NewString()}); !errors.Is(err, agentrun.ErrBudgetExhausted) {
					t.Fatal("bundle bypasses tool budget", err)
				}
				return
			}
			if boundary != "before_read" {
				result := supportStorageResult(t, h, claimed, "proposal")
				if boundary == "committed" {
					recoveryCommit(t, h, &claimed, result)
				}
			}
			old, pending := claimed, currentRunStep(claimed)
			if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, pending, "TIMEOUT"); err != nil {
				t.Fatal(err)
			}
			recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
			claimed = h.claim(t)
			if currentRunStep(claimed) != pending || claimed.Lease.FencingToken != old.Lease.FencingToken+1 {
				t.Fatal("recovery cursor")
			}
			var staleErr error
			if boundary == "committed" {
				_, staleErr = h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, old, agentrun.SubcallChat, ""))
			} else {
				_, staleErr = h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: old.Lease, Step: pending, ToolInvocationID: uuid.NewString()})
			}
			if !errors.Is(staleErr, agentrun.ErrStaleLease) {
				t.Fatal("stale read", staleErr)
			}
			if boundary == "committed" {
				if currentRunStep(claimed).Kind != "model_decision" {
					t.Fatal("reread committed bundle")
				}
			} else {
				recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
			}
		})
	}
}
