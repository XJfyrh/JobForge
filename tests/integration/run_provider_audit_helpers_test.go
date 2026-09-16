package integration

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

// setupAuditHarness registers a separate immutable synthetic audited profile.
// It uses real PG/control transactions, never a provider or business acceptance.
func setupAuditHarness(t *testing.T) *runHarness {
	t.Helper()
	h := setupRunHarness(t)
	h.Profile.ID = "contract-audit-profile-v1"
	h.Profile.ExecutorVersion = agentrun.ProviderAuditExecutorVersion
	h.Profile.ProviderAuditPolicy = agentrun.ProviderAuditPolicyDeepSeekV1
	h.Profile.ExpectedResponseModel = "deepseek-flash"
	h.Profile.Definition = json.RawMessage(`{"fixture":true,"provider_audit_policy":"deepseek-audit-v1","expected_response_model":"deepseek-flash"}`)
	h.Profile.Hash = agentrun.Fingerprint("contract-audit-profile.v1", h.Profile.ID, h.Profile.ExecutorVersion,
		h.Profile.ProviderAuditPolicy, h.Profile.ExpectedResponseModel, string(h.Profile.Definition))
	h.Options.Profiles = []agentrun.Profile{h.Profile}
	h.Principal = "audit-worker"
	h.Options.Workers = []agentrun.WorkerConfig{
		{ID: h.Principal, Tenants: []string{"tenant-a", "tenant-b"}, ProfileIDs: []string{h.Profile.ID}, Capacity: 2},
		{ID: "audit-worker-2", Tenants: []string{"tenant-a", "tenant-b"}, ProfileIDs: []string{h.Profile.ID}, Capacity: 2},
	}
	var err error
	h.Store, err = runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Store.EnsureProfiles(h.Ctx); err != nil {
		t.Fatal(err)
	}
	h.Service, err = agentrun.NewService(h.Store, h.Capture, []string{"tenant-a", "tenant-b"})
	if err != nil {
		t.Fatal(err)
	}
	h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// auditAtModelStep advances only preceding non-chat steps through real ledger
// reservations/observations/commits. It never invents a chat report or barrier.
func auditAtModelStep(t *testing.T, h *runHarness, tenant, key string) agentrun.ClaimedRun {
	t.Helper()
	h.submit(t, tenant, key)
	claimed := h.claim(t)
	for currentRunStep(claimed).Kind != "model_proposal" {
		if currentRunStep(claimed).Kind != "search_policy" {
			checkpointAdvance(t, h, &claimed, "proposal", false)
			continue
		}
		result := auditPolicyResult(t, h, claimed)
		if _, err := h.Store.CommitStep(h.Ctx, h.Principal, checkpointCommitRequest(t, claimed, result)); err != nil {
			t.Fatal(err)
		}
		checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
		if err != nil {
			t.Fatal(err)
		}
		claimed.Checkpoint = checkpoint
	}
	return claimed
}

func auditPolicyResult(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun) agentrun.StepResult {
	t.Helper()
	step, snapshot := currentRunStep(claimed), claimed.Checkpoint.Snapshot
	toolID := uuid.NewString()
	if _, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: claimed.Lease, Step: step, ToolInvocationID: toolID}); err != nil {
		t.Fatal(err)
	}
	ref := "business-policy:" + snapshot.IndexID + ":P01.1"
	result := agentrun.StepResult{SchemaVersion: 1, ToolInvocationID: toolID, EvidenceRefs: []string{ref},
		Content: json.RawMessage(`{"snapshot_id":"` + snapshot.ID + `","matches":[{"index_id":"` + snapshot.IndexID + `","chunk_id":"P01.1","evidence_ref":"` + ref + `","distance":1e-3}]}`)}
	for _, subcall := range agentrun.ToolSequence(step.Kind) {
		request := ledgerRequest(h, claimed, subcall, toolID)
		ledgerReserve(t, h, request)
		observation := agentrun.ObserveCallRequest{Lease: claimed.Lease, Step: step, PhysicalCallID: request.PhysicalCallID,
			TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: "accepted"}
		if subcall == agentrun.SubcallQueryEmbedding {
			usage := ledgerUsage(10, 0)
			report := auditBindReport(t, h, request, agentrun.CallReport{Usage: &usage})
			settled, err := h.Store.SettleUsage(h.Ctx, h.Principal, report)
			if err != nil || !settled.NewlySettled || settled.PersistedReportHash != report.ReportHash {
				t.Fatalf("audited embedding report: %+v %v", settled, err)
			}
			observation.UsageKnown, observation.Usage = true, &usage
		}
		if _, err := h.Store.ObserveCall(h.Ctx, h.Principal, observation); err != nil {
			t.Fatal(err)
		}
		result.PhysicalCallID = request.PhysicalCallID
	}
	return result
}

func auditValue[T any](value T) *T { return &value }

func auditReportRequest(t *testing.T, h *runHarness, request agentrun.ReserveCallRequest, input, output int64) agentrun.SettleUsageRequest {
	t.Helper()
	audit := agentrun.ProviderAudit{SchemaVersion: 1, Provider: "deepseek", ResponseComplete: true, HTTPStatus: 200,
		ResponseSHA256: auditValue(agentrun.Fingerprint("synthetic-response", request.PhysicalCallID)),
		IdentityState:  agentrun.ProviderIdentityCompatible, ResponseID: auditValue("synthetic-" + request.PhysicalCallID),
		ResponseModel: auditValue(h.Profile.ExpectedResponseModel), SystemFingerprint: auditValue("synthetic-fp"),
		Created: auditValue(int64(1)), UsageEvidence: agentrun.UsageEvidenceComplete,
		ReasoningState: agentrun.ReasoningAbsent, ModeState: agentrun.ProviderModeNonthinking}
	audit.AuditHash = audit.Hash()
	receipt, err := audit.ReceiptHash(request.PhysicalCallID)
	if err != nil {
		t.Fatal(err)
	}
	usage := agentrun.UsageReport{InputTokens: input, OutputTokens: output, CachedInputTokens: min(input, 2), ReceiptHash: receipt}
	usage.UsageHash = usage.Hash()
	return auditBindReport(t, h, request, agentrun.CallReport{Usage: &usage, ProviderAudit: &audit})
}

func auditBindReport(t *testing.T, h *runHarness, request agentrun.ReserveCallRequest, report agentrun.CallReport) agentrun.SettleUsageRequest {
	t.Helper()
	if report.ProviderAudit != nil {
		report.ProviderAudit.AuditHash = report.ProviderAudit.Hash()
		if report.Usage != nil {
			receipt, err := report.ProviderAudit.ReceiptHash(request.PhysicalCallID)
			if err != nil {
				t.Fatal(err)
			}
			report.Usage.ReceiptHash = receipt
			report.Usage.UsageHash = report.Usage.Hash()
		}
	}
	binding, err := agentrun.ExecutionBindingHash(request.Lease, request.Step)
	if err != nil {
		t.Fatal(err)
	}
	identity := agentrun.ReportBinding{ExecutionBindingHash: binding, PhysicalCallID: request.PhysicalCallID,
		ParameterHash: request.ParameterHash, Subcall: request.Subcall, ExpectedResponseModel: h.Profile.ExpectedResponseModel}
	if err := report.Validate(identity); err != nil {
		t.Fatal(err)
	}
	return agentrun.SettleUsageRequest{Lease: request.Lease, PhysicalCallID: request.PhysicalCallID,
		Usage: report.Usage, ProviderAudit: report.ProviderAudit, ReportHash: report.Hash(identity)}
}

func auditObserve(t *testing.T, h *runHarness, request agentrun.ReserveCallRequest, report agentrun.SettleUsageRequest, business string) agentrun.CallReservation {
	t.Helper()
	code := ""
	if business == "rejected" {
		code = "MODEL_PROTOCOL_ERROR"
	}
	result, err := h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: request.Lease, Step: request.Step,
		PhysicalCallID: request.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: business,
		ErrorCode: code, UsageKnown: true, Usage: report.Usage, AuditHash: report.ProviderAudit.AuditHash})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func auditCommitChat(t *testing.T, h *runHarness, claimed *agentrun.ClaimedRun, request agentrun.ReserveCallRequest, correction bool) agentrun.CommitStepRequest {
	t.Helper()
	result := agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: request.PhysicalCallID,
		EvidenceRefs: []string{}, Content: json.RawMessage(`null`), CorrectionRequired: correction}
	if !correction {
		result.Proposal = &agentrun.Proposal{Decision: "proposal", Summary: "Synthetic audited proposal", Action: "escalate",
			EvidenceRefs: []string{"business-evidence:" + claimed.Checkpoint.Snapshot.ID + ":ticket",
				"business-policy:" + claimed.Checkpoint.Snapshot.IndexID + ":P01.1"}}
	}
	commit := checkpointCommitRequest(t, *claimed, result)
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, commit); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
	if err != nil {
		t.Fatal(err)
	}
	claimed.Checkpoint = checkpoint
	return commit
}
