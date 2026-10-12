package run

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
)

func adjudicationDefinition(schema int) SupportDefinition {
	d := supportDefinitionFixture()
	d.SchemaVersion = schema
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-11", "2026-10-11"
	d.Model.MessageContentBytes, d.Model.RequestBodyBytes = 65536, 131072
	final := d.Model
	final.Thinking, final.ReasoningEffort, final.MaxTokens, final.ResponseBodyBytes = "enabled", "high", 16384, 1048576
	d.FinalModel = &final
	if schema != 12 {
		d.Program.Strategy, d.Program.Adapter, d.Program.PromptVersion = SupportAgentStrategy, "support-agent-v1", SupportAgentPromptV4
		if schema == 13 {
			d.Program.PromptVersion = SupportAgentPromptV5
		}
		d.Program.DecisionSchema, d.Program.DecisionSchemaSHA256 = SupportAgentDecisionSchema, strings.Repeat("a", 64)
		d.Program.RecoveryPolicy, d.Program.ApprovalPolicy = ConfirmedUncommittedRecovery, TicketResolutionApprovalPolicy
		d.Action = &SupportActionDefinition{Operation: business.ResolutionOperation, Origin: "http://business:8092", KeyID: "test-key", PublicKeySHA256: strings.Repeat("a", 64)}
	}
	return d
}

func adjudicationProfile(t *testing.T, schema int) Profile {
	t.Helper()
	p, err := BuildSupportProfile("adjudication", adjudicationDefinition(schema))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAdjudicationFrozenStepBudgetAndCompatibility(t *testing.T) {
	for _, schema := range []int{11, 12, 13} {
		p := adjudicationProfile(t, schema)
		agent, executor := schema != 12, SupportFixedAdjudicationExecutorVersion
		if schema == 11 {
			executor = SupportAdjudicationExecutorVersion
		}
		if schema == 13 {
			executor = SupportAdjudicationV2ExecutorVersion
		}
		if p.ExecutorVersion != executor || !p.FinalAdjudicationEnabled() || p.ValidateAuditPolicy() != nil || p.ApprovalEnabled() != agent || p.ConfirmedStepRecovery() != agent {
			t.Fatal("versioned capability mismatch")
		}
		for _, kind := range []string{"model_decision", "protocol_correction", "model_proposal"} {
			budget, err := ReservationBudgetForStep(p, kind, SubcallChat)
			output, timeout := int64(1024), 60*time.Second
			if kind == "model_proposal" {
				output, timeout = 16384, 120*time.Second
			}
			if err != nil || budget.OutputTokens != output || p.CallTimeout(kind, SubcallChat) != timeout || p.AllowsProviderMode(kind, ProviderModeThinking) != (kind == "model_proposal") {
				t.Fatalf("wrong step policy %s: %+v %v", kind, budget, err)
			}
			thinking := &ProviderAudit{SchemaVersion: 2, ModeState: ProviderModeThinking}
			if p.AllowsProviderAudit(kind, thinking) != (kind == "model_proposal") {
				t.Fatalf("wrong thinking audit policy %s", kind)
			}
		}
		mutated := p
		mutated.Hash = strings.Repeat("0", 64)
		if mutated.FinalAdjudicationEnabled() || mutated.ValidateAuditPolicy() == nil {
			t.Fatal("unfrozen capability accepted")
		}
	}
	old, err := BuildSupportProfile("old", supportDefinitionFixture())
	if err != nil || old.FinalAdjudicationEnabled() || old.ThinkingStep("model_proposal") {
		t.Fatal("historical capability changed", err)
	}
	old.MaxOutputTokens = 16384
	if _, err := ReservationBudgetForStep(old, "model_proposal", SubcallChat); err == nil {
		t.Fatal("old profile acquired final capacity")
	}
}

func TestAdjudicationPromptVersionBoundToSchema(t *testing.T) {
	mismatch := adjudicationDefinition(13)
	mismatch.Program.PromptVersion = SupportAgentPromptV4
	if _, err := BuildSupportProfile("mismatch", mismatch); err == nil {
		t.Fatal("schema 13 accepted the v4 prompt")
	}
	mismatch = adjudicationDefinition(11)
	mismatch.Program.PromptVersion = SupportAgentPromptV5
	if _, err := BuildSupportProfile("mismatch", mismatch); err == nil {
		t.Fatal("schema 11 accepted the v5 prompt")
	}
}

func TestAdjudicationDraftCannotApproveAndFinalCannotRetry(t *testing.T) {
	p := adjudicationProfile(t, 11)
	request := func(kind string, result StepResult) CommitStepRequest {
		_, req := supportCommitRequest(t, kind, result)
		req.Step.ProfileID, req.Step.ProfileHash = p.ID, p.Hash
		_, canonical, err := CanonicalStepResultForStrategy(req.ResultJSON, kind, p.Strategy)
		if err != nil {
			t.Fatal(err)
		}
		req.CommitHash = CommitHash(req.Step, canonical)
		return req
	}
	snapshot, prior := supportFixture(t, true)
	marker := json.RawMessage(`{"type":"investigation_complete","draft":{"decision":"proposal","action":"escalate","conclusion":"conflicting","requested_fields":[],"target_ticket_status":"escalated","claims":[]}}`)
	result := StepResult{SchemaVersion: 1, PhysicalCallID: "44444444-4444-4444-8444-444444444444", EvidenceRefs: []string{}, Content: marker}
	req := request("model_decision", result)
	d, err := DecideCommit(p, snapshot, prior, req)
	if err != nil || d.NextKind != "model_proposal" || d.Proposal != nil || d.CloseAttempt || d.Result.Proposal != nil {
		t.Fatal("draft reached proposal path", err)
	}
	old := p
	old.ExecutorVersion = SupportApprovalExecutorVersion
	if _, err := DecideCommit(old, snapshot, prior, req); err == nil {
		t.Fatal("legacy graph accepted completion marker")
	}
	proposal, err := SupportProposalFromModel(snapshot, prior, []byte(supportModelJSON(supportClaimExamples[0].claim)))
	if err != nil {
		t.Fatal(err)
	}
	result.Content, result.Proposal = json.RawMessage("null"), proposal
	req = request("model_proposal", result)
	if _, err := DecideCommit(p, snapshot, prior, req); err == nil {
		t.Fatal("final skipped investigation marker")
	}
	prior = append(prior, Step{Kind: "model_decision", Output: d.CanonicalJSON})
	d, err = DecideCommit(p, snapshot, prior, req)
	if err != nil || d.NextKind != "submit_proposal" || d.Proposal == nil || d.CloseAttempt {
		t.Fatal("independent final refused", err)
	}
	result.Proposal, result.CorrectionRequired = nil, true
	req = request("model_proposal", result)
	if _, err := DecideCommit(p, snapshot, prior, req); err == nil {
		t.Fatal("final acquired automatic correction")
	}
}

func TestAdjudicationAuditModeBoundToOriginalStep(t *testing.T) {
	binding := auditBinding(t)
	a := completeAudit()
	a.SchemaVersion, a.ModeState = 2, ProviderModeThinking
	count := int64(1)
	a.ReasoningState, a.ReasoningTokens = ReasoningObserved, &count
	a.AuditHash = a.Hash()
	report := auditReport(t, a, 10, 2)
	budget := CallBudget{InputTokens: 100, OutputTokens: 16384, TotalTokens: 16484, CostMicroyuan: 1000000}
	price := Pricing{Denominator: 1000000, InputMissMicroyuan: 2000000, InputHitMicroyuan: 40000, OutputMicroyuan: 8000000}
	for _, allowed := range []bool{false, true} {
		binding.AllowThinking = allowed
		d, err := FirstReportDisposition(binding, report, budget, price)
		if err != nil || !d.UsageKnown || d.KnownTokens != 12 || (d.BatchStopCode == "") != allowed {
			t.Fatalf("wrong thinking settlement: %+v %v", d, err)
		}
	}
	a.SchemaVersion = 1
	a.AuditHash = a.Hash()
	if a.Validate() == nil {
		t.Fatal("old audit schema accepted thinking")
	}
}
