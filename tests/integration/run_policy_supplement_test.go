package integration

import (
	"encoding/json"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func supplementIntent(callID string) agentrun.StepResult {
	return agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: callID, EvidenceRefs: []string{},
		Content: json.RawMessage(`{"type":"policy_supplement","topic":"missing_facts","required_aliases":["P03.2"]}`)}
}

func supplementSearch(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun) agentrun.StepResult {
	t.Helper()
	result := supportStorageResult(t, h, claimed, "proposal")
	var content map[string]json.RawMessage
	if err := json.Unmarshal(result.Content, &content); err != nil {
		t.Fatal(err)
	}
	var matches []map[string]any
	if err := json.Unmarshal(content["matches"], &matches); err != nil {
		t.Fatal(err)
	}
	hit := matches[0]
	hit["chunk_id"], hit["source"], hit["text"] = "P03.2", "P03.md", "Synthetic supplement definition"
	hit["evidence_ref"] = "business-policy:" + claimed.Checkpoint.Snapshot.IndexID + ":P03.2"
	content["matches"], _ = json.Marshal([]map[string]any{hit})
	result.Content, _ = json.Marshal(content)
	result.EvidenceRefs = []string{hit["evidence_ref"].(string)}
	return result
}

func TestRunPolicySupplementRecoveryAndAccounting(t *testing.T) {
	for _, boundary := range []string{"intent_committed", "search_committed", "final_committed", "unknown_second"} {
		t.Run(boundary, func(t *testing.T) {
			h := setupAdjudicationProfileHarness(t, false, 14)
			claimed := adjudicationAtFinal(t, h)
			final := adjudicationProposal(t, claimed, "")
			first := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			ledgerReserve(t, h, first)
			report := adjudicationReport(t, h, first, 8000)
			auditSettle(t, h, report)
			auditObserve(t, h, first, report, "accepted")
			recoveryCommit(t, h, &claimed, supplementIntent(first.PhysicalCallID))
			if currentRunStep(claimed).Kind != "search_policy" || claimed.Checkpoint.Run.ProposalRef != nil {
				t.Fatal("intent became proposal")
			}
			recoverAt := func() {
				pending, old := currentRunStep(claimed), claimed
				if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, pending, "TIMEOUT"); err != nil {
					t.Fatal(err)
				}
				recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
				claimed = h.claim(t)
				if currentRunStep(claimed) != pending || claimed.Lease.FencingToken != old.Lease.FencingToken+1 {
					t.Fatal("recovery reset supplement state")
				}
			}
			if boundary == "intent_committed" {
				recoverAt()
			}
			recoveryCommit(t, h, &claimed, supplementSearch(t, h, claimed))
			if currentRunStep(claimed).Kind != "model_proposal" {
				t.Fatal("supplement returned to investigation")
			}
			if boundary == "search_committed" {
				recoverAt()
			}
			second := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reserved := ledgerReserve(t, h, second)
			if reserved.Budget.OutputTokens != 16384 || reserved.CallDeadline.Sub(reserved.ReservedAt) > 120*time.Second {
				t.Fatal("second thinking permit")
			}
			if boundary == "unknown_second" {
				a := agentrun.ProviderAudit{SchemaVersion: 2, Provider: "deepseek", IdentityState: agentrun.ProviderIdentityUnavailable, UsageEvidence: agentrun.UsageEvidenceUnavailable, ReasoningState: agentrun.ReasoningUnavailable, ModeState: agentrun.ProviderModeUnavailable}
				settled := auditSettle(t, h, auditBindReport(t, h, second, agentrun.CallReport{ProviderAudit: &a}))
				if !settled.BatchFrozen || settled.Reservation.UsageKnown || settled.Reservation.Budget != reserved.Budget {
					t.Fatal("second unknown released hold")
				}
				if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, claimed, agentrun.SubcallChat, "")); err == nil {
					t.Fatal("unknown second resent")
				}
				return
			}
			report = adjudicationReport(t, h, second, 8000)
			auditSettle(t, h, report)
			auditObserve(t, h, second, report, "accepted")
			final.PhysicalCallID = second.PhysicalCallID
			recoveryCommit(t, h, &claimed, final)
			if boundary == "final_committed" {
				recoverAt()
			}
			final.PhysicalCallID = ""
			recoveryCommit(t, h, &claimed, final)
			calls, err := h.Store.Calls(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
			if err != nil || calls.BatchFrozen {
				t.Fatal("audit closure", err)
			}
			thinking, known := 0, int64(0)
			for _, call := range calls.Items {
				if call.StepKind == "model_proposal" {
					thinking++
					known += call.KnownTokens
					if call.HeldCostMicroyuan != 0 || call.Reserved.OutputTokens != 16384 {
						t.Fatal("lost accounting")
					}
				}
			}
			if thinking != 2 || known != 32968 {
				t.Fatal("supplement calls resent or omitted", thinking, known)
			}
		})
	}
}
