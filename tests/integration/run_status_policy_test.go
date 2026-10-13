package integration

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func statusPolicyCapture(t *testing.T, h *runHarness, key, status string) agentrun.SnapshotBinding {
	t.Helper()
	snapshot := supportProfileCapture(t, h, "submit", "submit-"+key, "", true)
	var ticket map[string]any
	if err := json.Unmarshal(snapshot.Ticket, &ticket); err != nil {
		t.Fatal(err)
	}
	ticket["status"] = status
	snapshot.Ticket, _ = json.Marshal(ticket)
	snapshot.ContentHash = agentrun.Fingerprint("synthetic-support-capture", string(snapshot.Ticket), string(snapshot.VersionVector))
	h.Capture.snapshots["tenant-north:"+agentrun.SnapshotKey("tenant-north", "submit", "submit-"+key, "")] = snapshot
	return snapshot
}

func statusPolicyAtRead(t *testing.T, h *runHarness, count int) agentrun.ClaimedRun {
	t.Helper()
	var err error
	h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	statusPolicyCapture(t, h, "status", "informational_only")
	h.submit(t, "tenant-north", "status")
	claimed := h.claim(t)
	recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	for i := 0; i < count; i++ {
		tool := "search_policy"
		if i == 0 {
			tool = "get_order"
		}
		if i == 1 {
			tool = "get_delivery"
		}
		chat, _ := recoveryChat(t, h, claimed, true)
		decision := recoveryToolDecision(t, claimed, chat.PhysicalCallID, tool)
		if tool == "search_policy" {
			decision.Content = json.RawMessage(fmt.Sprintf(`{"type":"tool","name":"search_policy","arguments":{"query":"synthetic distinct query %d"}}`, i))
		}
		recoveryCommit(t, h, &claimed, decision)
		recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	}
	chat, _ := recoveryChat(t, h, claimed, true)
	marker := agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: chat.PhysicalCallID, EvidenceRefs: []string{}, Content: json.RawMessage(`{"type":"investigation_complete","draft":{"decision":"proposal","action":"escalate","conclusion":"conflicting","requested_fields":[],"target_ticket_status":"escalated","claims":[]}}`)}
	req := recoveryCommit(t, h, &claimed, marker)
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, req); err != nil {
		t.Fatal("marker replay", err)
	}
	if currentRunStep(claimed).Kind != "search_policy" || claimed.Checkpoint.Run.ProposalRef != nil {
		t.Fatal("missing status read")
	}
	return claimed
}

func statusPolicySearch(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun) agentrun.StepResult {
	t.Helper()
	result := supplementSearch(t, h, claimed)
	var content map[string]any
	if err := json.Unmarshal(result.Content, &content); err != nil {
		t.Fatal(err)
	}
	hit := content["matches"].([]any)[0].(map[string]any)
	hit["chunk_id"], hit["source"], hit["text"] = "P08.1", "P08.md", "Synthetic status definition"
	hit["evidence_ref"] = "business-policy:" + claimed.Checkpoint.Snapshot.IndexID + ":P08.1"
	result.Content, _ = json.Marshal(content)
	result.EvidenceRefs = []string{hit["evidence_ref"].(string)}
	return result
}

func TestRunStatusPolicyRecoveryAndAccounting(t *testing.T) {
	for _, boundary := range []string{"marker_committed", "read_committed", "final_committed", "cancel_before_read", "cancel_before_thinking", "unknown_first"} {
		t.Run(boundary, func(t *testing.T) {
			h := setupAdjudicationProfileHarness(t, false, 15)
			claimed := statusPolicyAtRead(t, h, 3)
			proposal, err := agentrun.SupportProposalFromModel(claimed.Checkpoint.Snapshot, claimed.Checkpoint.Steps,
				[]byte(`{"decision":"proposal","action":"record_conclusion","conclusion":"delayed","requested_fields":[],"target_ticket_status":"informational_only","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["E1#/order/promised_delivery_at","P01.1"]}]}`))
			if err != nil {
				t.Fatal(err)
			}
			final := agentrun.StepResult{SchemaVersion: 1, Content: json.RawMessage("null"), EvidenceRefs: []string{}, Proposal: proposal}
			recoverAt := func() {
				pending, old := currentRunStep(claimed), claimed
				if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, pending, "TIMEOUT"); err != nil {
					t.Fatal(err)
				}
				recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
				claimed = h.claim(t)
				if currentRunStep(claimed) != pending || claimed.Lease.FencingToken != old.Lease.FencingToken+1 {
					t.Fatal("recovery changed read cursor")
				}
				if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, old, agentrun.SubcallChat, "")); !errors.Is(err, agentrun.ErrStaleLease) {
					t.Fatal("stale send", err)
				}
			}
			cancelAt := func() {
				if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancel-status"); err != nil {
					t.Fatal(err)
				}
				if currentRunStep(claimed).Kind == "search_policy" {
					if _, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: claimed.Lease, Step: currentRunStep(claimed), ToolInvocationID: uuid.NewString()}); err == nil {
						t.Fatal("read after cancel")
					}
				} else if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, claimed, agentrun.SubcallChat, "")); err == nil {
					t.Fatal("thinking after cancel")
				}
			}
			if boundary == "cancel_before_read" {
				cancelAt()
				return
			}
			if boundary == "marker_committed" {
				recoverAt()
			}
			req := recoveryCommit(t, h, &claimed, statusPolicySearch(t, h, claimed))
			if _, err := h.Store.CommitStep(h.Ctx, h.Principal, req); err != nil {
				t.Fatal("read replay", err)
			}
			if currentRunStep(claimed).Kind != "model_proposal" {
				t.Fatal("read did not reach thinking")
			}
			if boundary == "cancel_before_thinking" {
				cancelAt()
				return
			}
			if boundary == "read_committed" {
				recoverAt()
			}
			call := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reserved := ledgerReserve(t, h, call)
			if boundary == "unknown_first" {
				a := agentrun.ProviderAudit{SchemaVersion: 2, Provider: "deepseek", IdentityState: agentrun.ProviderIdentityUnavailable, UsageEvidence: agentrun.UsageEvidenceUnavailable, ReasoningState: agentrun.ReasoningUnavailable, ModeState: agentrun.ProviderModeUnavailable}
				settled := auditSettle(t, h, auditBindReport(t, h, call, agentrun.CallReport{ProviderAudit: &a}))
				if !settled.BatchFrozen || settled.Reservation.UsageKnown || settled.Reservation.Budget != reserved.Budget {
					t.Fatal("first thinking unknown released hold")
				}
				if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, claimed, agentrun.SubcallChat, "")); err == nil {
					t.Fatal("unknown thinking resent")
				}
				return
			}
			report := adjudicationReport(t, h, call, 8000)
			auditSettle(t, h, report)
			auditObserve(t, h, call, report, "accepted")
			final.PhysicalCallID = call.PhysicalCallID
			recoveryCommit(t, h, &claimed, final)
			if boundary == "final_committed" {
				recoverAt()
			}
			final.PhysicalCallID = ""
			recoveryCommit(t, h, &claimed, final)
			var tools, chats, thinking int
			if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from tool_invocations where run_id=$1),count(*) filter(where kind='chat'),count(*) filter(where step_kind='model_proposal') from physical_calls where run_id=$1`, claimed.Lease.RunID).Scan(&tools, &chats, &thinking); err != nil {
				t.Fatal(err)
			}
			if tools != 4 || chats != 5 || thinking != 1 {
				t.Fatal("prerequisite replay or synthetic chat", tools, chats, thinking)
			}
		})
	}
}

func TestRunStatusPolicySharedToolBudget(t *testing.T) {
	for _, used := range []int{7, 8} {
		t.Run(fmt.Sprint(used), func(t *testing.T) {
			h := setupAdjudicationProfileHarness(t, false, 15)
			claimed := statusPolicyAtRead(t, h, used)
			req := agentrun.BeginToolRequest{Lease: claimed.Lease, Step: currentRunStep(claimed), ToolInvocationID: uuid.NewString()}
			response, err := h.Store.BeginTool(h.Ctx, h.Principal, req)
			if used == 8 {
				if !errors.Is(err, agentrun.ErrBudgetExhausted) {
					t.Fatal("ninth tool", err)
				}
				return
			}
			if err != nil || !response.NewlyStarted {
				t.Fatal("eighth tool", err)
			}
			response, err = h.Store.BeginTool(h.Ctx, h.Principal, req)
			if err != nil || response.NewlyStarted {
				t.Fatal("tool replay", err)
			}
			view := ledgerView(t, h, claimed.Lease)
			if view.Budget.Family.Used.LogicalTools != 8 {
				t.Fatal("shared budget changed")
			}
		})
	}
}
