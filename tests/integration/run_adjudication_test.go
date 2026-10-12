package integration

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

// The ledger and fenced execution are real; provider content and usage are synthetic.
func setupAdjudicationHarness(t *testing.T) *runHarness {
	t.Helper()
	return setupAdjudicationProfileHarness(t, false, 11)
}

// setupAdjudicationV5Harness binds the ADR-0034 candidate: Agent schema 13 with
// support-agent-prompt-v5 on linux-v2-adjudication-runtime-2, against the
// unchanged schema 12 Fixed baseline.
func setupAdjudicationV5Harness(t *testing.T) *runHarness {
	t.Helper()
	return setupAdjudicationProfileHarness(t, false, 13)
}

func setupAdjudicationProfileHarness(t *testing.T, fixed bool, agentSchema int) *runHarness {
	t.Helper()
	var h *runHarness
	if fixed {
		h = setupSupportProfileStrategyHarness(t, "adjudication-fixed-legacy", 20000000, false)
	} else {
		h = setupApprovalHarness(t)
	}
	d, err := agentrun.DecodeSupportDefinition(h.Profile.Definition)
	if err != nil {
		t.Fatal(err)
	}
	d.SchemaVersion, d.Program.PromptVersion = agentSchema, agentrun.SupportAgentPromptV4
	if agentSchema == 13 {
		d.Program.PromptVersion = agentrun.SupportAgentPromptV5
	}
	if fixed {
		d.SchemaVersion, d.Program.PromptVersion = 12, agentrun.SupportPromptVersion
		d.Model.MessageContentBytes, d.Model.RequestBodyBytes = 65536, 131072
	}
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-11", "2026-10-11"
	final := d.Model
	final.Thinking, final.ReasoningEffort, final.MaxTokens, final.ResponseBodyBytes = "enabled", "high", 16384, 1048576
	d.FinalModel = &final
	h.Profile, err = agentrun.BuildSupportProfile("adjudication-synthetic-v1", d)
	if err != nil {
		t.Fatal(err)
	}
	h.Profile.Executable = true
	h.Options.Profiles = []agentrun.Profile{h.Profile}
	for i := range h.Options.Workers {
		h.Options.Workers[i].ProfileIDs = []string{h.Profile.ID}
	}
	h.Store, err = runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Store.EnsureProfiles(h.Ctx); err != nil {
		t.Fatal(err)
	}
	h.Service, err = agentrun.NewService(h.Store, h.Capture, []string{"tenant-north", "tenant-south"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRunAdjudicationFixedFinal(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "rejected"}[rejected], func(t *testing.T) {
			h := setupAdjudicationProfileHarness(t, true, 11)
			var err error
			h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
			if err != nil {
				t.Fatal(err)
			}
			supportProfileCapture(t, h, "submit", "submit-fixed-final", "", true)
			h.submit(t, "tenant-north", "fixed-final")
			claimed := h.claim(t)
			for currentRunStep(claimed).Kind != "model_proposal" {
				recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
			}
			req := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reserved := ledgerReserve(t, h, req)
			if reserved.Budget.OutputTokens != 16384 || reserved.CallDeadline.Sub(reserved.ReservedAt) != 120*time.Second {
				t.Fatal("Fixed final capacity differs")
			}
			report := adjudicationReport(t, h, req, 16000)
			auditSettle(t, h, report)
			if rejected {
				auditObserve(t, h, req, report, "rejected")
				if _, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, req.Step, "MODEL_PROTOCOL_ERROR"); err != nil {
					t.Fatal(err)
				}
			} else {
				auditObserve(t, h, req, report, "accepted")
				recoveryCommit(t, h, &claimed, adjudicationProposal(t, claimed, req.PhysicalCallID))
				recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
			}
			calls, err := h.Store.Calls(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
			if err != nil || calls.BatchFrozen {
				t.Fatal("Fixed terminal did not close", err)
			}
			chats := 0
			for _, call := range calls.Items {
				if call.Subcall == agentrun.SubcallChat {
					chats++
					if call.StepKind != "model_proposal" || call.KnownTokens != 16484 || call.HeldCostMicroyuan != 0 {
						t.Fatal("Fixed terminal changed accounting or corrected")
					}
				}
			}
			if chats != 1 {
				t.Fatal("Fixed terminal resent")
			}
			supportProfileCapture(t, h, "submit", "submit-next-fixed", "", true)
			next := h.submit(t, "tenant-north", "next-fixed")
			resumed, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
			if err != nil || resumed == nil || resumed.Lease.RunID != next.ID {
				t.Fatal("closed Fixed final blocked next case", err)
			}
		})
	}
}

func adjudicationAtFinal(t *testing.T, h *runHarness) agentrun.ClaimedRun {
	t.Helper()
	claimed := recoveryAtDecision(t, h, "adjudication")
	for _, tool := range []string{"get_order", "get_delivery", "search_policy"} {
		chat, _ := recoveryChat(t, h, claimed, true)
		recoveryCommit(t, h, &claimed, recoveryToolDecision(t, claimed, chat.PhysicalCallID, tool))
		recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	}
	chat, _ := recoveryChat(t, h, claimed, true)
	marker := agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: chat.PhysicalCallID, EvidenceRefs: []string{},
		Content: json.RawMessage(`{"type":"investigation_complete","draft":{"decision":"proposal","action":"escalate","conclusion":"conflicting","requested_fields":[],"target_ticket_status":"escalated","claims":[]}}`)}
	commit := recoveryCommit(t, h, &claimed, marker)
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, commit); err != nil {
		t.Fatal("completion marker replay", err)
	}
	if currentRunStep(claimed).Kind != "model_proposal" || claimed.Checkpoint.Run.ProposalRef != nil {
		t.Fatal("draft became approvable or did not advance")
	}
	approval, err := h.Store.Approval(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
	if (err != nil && !errors.Is(err, agentrun.ErrNotFound)) || approval.Available {
		t.Fatal("draft exposed through approval", err)
	}
	return claimed
}

func adjudicationReport(t *testing.T, h *runHarness, req agentrun.ReserveCallRequest, reasoning int64) agentrun.SettleUsageRequest {
	t.Helper()
	report := auditReportRequest(t, h, req, 100, 16384)
	report.ProviderAudit.SchemaVersion = 2
	report.ProviderAudit.ReasoningState, report.ProviderAudit.ReasoningTokens = agentrun.ReasoningObserved, &reasoning
	if reasoning > 0 {
		report.ProviderAudit.ModeState = agentrun.ProviderModeThinking
	}
	return auditBindReport(t, h, req, agentrun.CallReport{Usage: report.Usage, ProviderAudit: report.ProviderAudit})
}

func adjudicationProposal(t *testing.T, claimed agentrun.ClaimedRun, callID string) agentrun.StepResult {
	t.Helper()
	proposal, err := agentrun.SupportProposalFromModel(claimed.Checkpoint.Snapshot, claimed.Checkpoint.Steps,
		[]byte(`{"decision":"proposal","action":"record_conclusion","conclusion":"delayed","requested_fields":[],"target_ticket_status":"open","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["E1#/order/promised_delivery_at","P01.1"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: callID, Content: json.RawMessage("null"), EvidenceRefs: []string{}, Proposal: proposal}
}

func TestRunAdjudicationPhaseBoundaryRecovery(t *testing.T) {
	for _, mode := range []string{"marker_committed", "final_confirmed", "final_committed"} {
		t.Run(mode, func(t *testing.T) {
			h := setupAdjudicationHarness(t)
			old := adjudicationAtFinal(t, h)
			var oldReport agentrun.SettleUsageRequest
			var oldRequest agentrun.ReserveCallRequest
			if mode != "marker_committed" {
				oldRequest = ledgerRequest(h, old, agentrun.SubcallChat, "")
				ledgerReserve(t, h, oldRequest)
				oldReport = adjudicationReport(t, h, oldRequest, 16000)
				auditSettle(t, h, oldReport)
				auditObserve(t, h, oldRequest, oldReport, "accepted")
				if mode == "final_committed" {
					recoveryCommit(t, h, &old, adjudicationProposal(t, old, oldRequest.PhysicalCallID))
				}
			}
			pending := currentRunStep(old)
			if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, pending, "TIMEOUT"); err != nil {
				t.Fatal(err)
			}
			recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
			resumed := h.claim(t)
			if currentRunStep(resumed) != pending || resumed.Lease.AttemptNo != 2 || resumed.Lease.FencingToken != old.Lease.FencingToken+1 {
				t.Fatal("recovery changed phase or fencing")
			}
			if mode == "final_committed" {
				if pending.Kind != "submit_proposal" {
					t.Fatal("committed final replayed model")
				}
			} else {
				if pending.Kind != "model_proposal" {
					t.Fatal("marker repeated investigation")
				}
				req := ledgerRequest(h, resumed, agentrun.SubcallChat, "")
				reserved := ledgerReserve(t, h, req)
				if reserved.Budget.OutputTokens != 16384 {
					t.Fatal("recovered final lost original capacity")
				}
				if mode == "final_confirmed" {
					if _, err := h.Store.SettleUsage(h.Ctx, h.Principal, oldReport); err != nil {
						t.Fatal("old report replay", err)
					}
					_, err := h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: old.Lease, Step: pending,
						PhysicalCallID: oldRequest.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: "accepted"})
					if !errors.Is(err, agentrun.ErrStaleLease) {
						t.Fatal("stale final retained authority", err)
					}
				}
				report := adjudicationReport(t, h, req, 16000)
				auditSettle(t, h, report)
				auditObserve(t, h, req, report, "accepted")
				recoveryCommit(t, h, &resumed, adjudicationProposal(t, resumed, req.PhysicalCallID))
			}
			recoveryCommit(t, h, &resumed, adjudicationProposal(t, resumed, ""))
			calls, err := h.Store.Calls(h.Ctx, resumed.Lease.TenantID, resumed.Lease.RunID)
			if err != nil || calls.BatchFrozen {
				t.Fatal("recovered final not closed", err)
			}
			finals, investigations := 0, 0
			for _, call := range calls.Items {
				if call.Subcall == agentrun.SubcallChat {
					if call.StepKind == "model_proposal" {
						finals++
						if call.KnownTokens != 16484 || call.HeldCostMicroyuan != 0 {
							t.Fatal("recovery reset original exposure")
						}
					} else {
						investigations++
					}
				}
			}
			wantFinals := 1
			if mode == "final_confirmed" {
				wantFinals = 2
			}
			if finals != wantFinals || investigations != 4 {
				t.Fatal("recovery duplicated committed work", finals, investigations)
			}
		})
	}
}

func TestRunAdjudicationLedgerFinalAndTerminalRejection(t *testing.T) {
	for _, mode := range []string{"thinking", "empty_reasoning", "truncated", "cancelled_late", "unknown", "contradictory"} {
		t.Run(mode, func(t *testing.T) {
			h := setupAdjudicationHarness(t)
			claimed := adjudicationAtFinal(t, h)
			req := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reserved := ledgerReserve(t, h, req)
			if reserved.Budget.OutputTokens != 16384 || reserved.CallDeadline.Sub(reserved.ReservedAt) != 120*time.Second || reserved.DispatchExpiresAt.Sub(reserved.ReservedAt) > 30*time.Second {
				t.Fatal("final permit changed lease or lost capacity", reserved)
			}
			reasoning := int64(16384)
			if mode == "empty_reasoning" {
				reasoning = 0
			}
			report := adjudicationReport(t, h, req, reasoning)
			if mode == "unknown" || mode == "contradictory" {
				a := agentrun.ProviderAudit{SchemaVersion: 2, Provider: "deepseek", IdentityState: agentrun.ProviderIdentityUnavailable,
					UsageEvidence: agentrun.UsageEvidenceUnavailable, ReasoningState: agentrun.ReasoningUnavailable, ModeState: agentrun.ProviderModeUnavailable}
				if mode == "contradictory" {
					a = *report.ProviderAudit
					a.UsageEvidence, a.ReasoningState, a.ModeState, a.ReasoningTokens = agentrun.UsageEvidenceInvalid, agentrun.ReasoningInvalid, agentrun.ProviderModeInvalid, nil
				}
				report = auditBindReport(t, h, req, agentrun.CallReport{ProviderAudit: &a})
				settled := auditSettle(t, h, report)
				if !settled.BatchFrozen || settled.Reservation.UsageKnown || settled.Reservation.Budget != reserved.Budget {
					t.Fatal("unknown exposure released")
				}
				if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, claimed, agentrun.SubcallChat, "")); err == nil {
					t.Fatal("unknown final resent")
				}
				return
			}
			if mode == "cancelled_late" {
				if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancel-final"); err != nil {
					t.Fatal(err)
				}
			}
			settled := auditSettle(t, h, report)
			if settled.BatchFrozen || !settled.Reservation.UsageKnown || settled.Reservation.Budget != reserved.Budget {
				t.Fatal("final usage incorrectly held or repriced")
			}
			if mode == "cancelled_late" {
				_, err := h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: req.Lease, Step: req.Step,
					PhysicalCallID: req.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: "accepted",
					UsageKnown: true, Usage: report.Usage, AuditHash: report.ProviderAudit.AuditHash})
				if !errors.Is(err, agentrun.ErrCancelRequested) && !errors.Is(err, agentrun.ErrStaleLease) {
					t.Fatal("cancelled final retained execution authority", err)
				}
				return
			}
			if mode == "truncated" {
				auditObserve(t, h, req, report, "rejected")
				if _, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, req.Step, "MODEL_PROTOCOL_ERROR"); err != nil {
					t.Fatal(err)
				}
			} else {
				auditObserve(t, h, req, report, "accepted")
				proposal, err := agentrun.SupportProposalFromModel(claimed.Checkpoint.Snapshot, claimed.Checkpoint.Steps,
					[]byte(`{"decision":"proposal","action":"record_conclusion","conclusion":"delayed","requested_fields":[],"target_ticket_status":"open","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["E1#/order/promised_delivery_at","P01.1"]}]}`))
				if err != nil {
					t.Fatal(err)
				}
				result := agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: req.PhysicalCallID, Content: json.RawMessage("null"), EvidenceRefs: []string{}, Proposal: proposal}
				commit := recoveryCommit(t, h, &claimed, result)
				if _, err := h.Store.CommitStep(h.Ctx, h.Principal, commit); err != nil {
					t.Fatal("final commit replay", err)
				}
				result.PhysicalCallID = ""
				recoveryCommit(t, h, &claimed, result)
			}
			calls, err := h.Store.Calls(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
			if err != nil || calls.BatchFrozen {
				t.Fatal("final audit gap", err)
			}
			finals := 0
			for _, call := range calls.Items {
				if call.StepKind == "model_proposal" {
					finals++
					if call.Reserved.OutputTokens != 16384 || call.KnownTokens != 16484 || call.HeldCostMicroyuan != 0 {
						t.Fatal("final accounting double-counted reasoning or used old bound")
					}
				}
			}
			if finals != 1 {
				t.Fatal("final resent")
			}
			supportProfileCapture(t, h, "submit", "submit-next-final", "", true)
			next := h.submit(t, "tenant-north", "next-final")
			resumed, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
			if err != nil || resumed == nil || resumed.Lease.RunID != next.ID {
				t.Fatal("closed final blocked next case", err)
			}
		})
	}
}

// TestRunAdjudicationV5InheritsFrozenFinalContract backs ADR-0034's claim that
// schema 13 changes only what the two stages are shown. Against real PostgreSQL
// the permit, audit schema, accounting and no-resend boundary must match schema
// 11 exactly, and a terminal tool object must still end the Run rather than buy
// a second final call.
func TestRunAdjudicationV5InheritsFrozenFinalContract(t *testing.T) {
	for _, mode := range []string{"accepted", "tool_rejected"} {
		t.Run(mode, func(t *testing.T) {
			h := setupAdjudicationV5Harness(t)
			if h.Profile.ExecutorVersion != agentrun.SupportAdjudicationV2ExecutorVersion {
				t.Fatal("schema 13 did not bind the v2 adjudication executor", h.Profile.ExecutorVersion)
			}
			if !h.Profile.FinalAdjudicationEnabled() || !h.Profile.ApprovalEnabled() || !h.Profile.ConfirmedStepRecovery() {
				t.Fatal("schema 13 lost a capability inherited from schema 11")
			}
			if !h.Profile.ThinkingStep("model_proposal") || h.Profile.ThinkingStep("model_decision") {
				t.Fatal("schema 13 moved the thinking step binding")
			}
			claimed := adjudicationAtFinal(t, h)
			req := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reserved := ledgerReserve(t, h, req)
			if reserved.Budget.OutputTokens != 16384 || reserved.CallDeadline.Sub(reserved.ReservedAt) != 120*time.Second || reserved.DispatchExpiresAt.Sub(reserved.ReservedAt) > 30*time.Second {
				t.Fatal("schema 13 changed the frozen final permit", reserved)
			}
			report := adjudicationReport(t, h, req, 16384)
			if report.ProviderAudit == nil || report.ProviderAudit.SchemaVersion != 2 || report.ProviderAudit.ModeState != agentrun.ProviderModeThinking {
				t.Fatal("schema 13 changed the terminal audit policy", report.ProviderAudit)
			}
			settled := auditSettle(t, h, report)
			if settled.BatchFrozen || !settled.Reservation.UsageKnown || settled.Reservation.Budget != reserved.Budget {
				t.Fatal("schema 13 final usage incorrectly held or repriced")
			}
			if mode == "tool_rejected" {
				auditObserve(t, h, req, report, "rejected")
				if _, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, req.Step, "MODEL_PROTOCOL_ERROR"); err != nil {
					t.Fatal(err)
				}
				if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, claimed, agentrun.SubcallChat, "")); err == nil {
					t.Fatal("schema 13 resent a rejected final call")
				}
			} else {
				auditObserve(t, h, req, report, "accepted")
				proposal, err := agentrun.SupportProposalFromModel(claimed.Checkpoint.Snapshot, claimed.Checkpoint.Steps,
					[]byte(`{"decision":"proposal","action":"record_conclusion","conclusion":"delayed","requested_fields":[],"target_ticket_status":"open","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["E1#/order/promised_delivery_at","P01.1"]}]}`))
				if err != nil {
					t.Fatal(err)
				}
				result := agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: req.PhysicalCallID, Content: json.RawMessage("null"), EvidenceRefs: []string{}, Proposal: proposal}
				commit := recoveryCommit(t, h, &claimed, result)
				if _, err := h.Store.CommitStep(h.Ctx, h.Principal, commit); err != nil {
					t.Fatal("schema 13 rejected a valid final proposal", err)
				}
			}
			calls, err := h.Store.Calls(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
			if err != nil || calls.BatchFrozen {
				t.Fatal("schema 13 final audit gap", err)
			}
			finals := 0
			for _, call := range calls.Items {
				if call.StepKind == "model_proposal" {
					finals++
					if call.Reserved.OutputTokens != 16384 || call.KnownTokens != 16484 || call.HeldCostMicroyuan != 0 {
						t.Fatal("schema 13 changed final accounting", call)
					}
				}
			}
			if finals != 1 {
				t.Fatal("schema 13 did not make exactly one final call", finals)
			}
		})
	}
}
