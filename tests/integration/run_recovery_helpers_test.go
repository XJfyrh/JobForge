package integration

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

// The profile and all control transactions are real; business capture, digests
// and provider reports are labelled synthetic mechanism fixtures.
func setupRecoveryHarness(t *testing.T) *runHarness {
	t.Helper()
	h := setupSupportProfileStrategyHarness(t, "recovery-legacy-fixture", 20000000, true)
	d, err := agentrun.DecodeSupportDefinition(h.Profile.Definition)
	if err != nil {
		t.Fatal(err)
	}
	d.SchemaVersion, d.Program.RecoveryPolicy = 3, agentrun.ConfirmedUncommittedRecovery
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-04", "2026-10-04"
	h.Profile, err = agentrun.BuildSupportProfile("support-recovery-synthetic-v1", d)
	if err != nil {
		t.Fatal(err)
	}
	h.Profile.Executable = true
	h.Options.Profiles = []agentrun.Profile{h.Profile}
	h.Options.Workers = []agentrun.WorkerConfig{
		{ID: h.Principal, Tenants: []string{"tenant-north", "tenant-south"}, ProfileIDs: []string{h.Profile.ID}, Capacity: 1},
		{ID: "recovery-worker-2", Tenants: []string{"tenant-north", "tenant-south"}, ProfileIDs: []string{h.Profile.ID}, Capacity: 1},
	}
	h.Options.TenantCapacity, h.Options.ProfileCapacity = 2, 2
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

func recoveryAtDecision(t *testing.T, h *runHarness, key string) agentrun.ClaimedRun {
	t.Helper()
	var err error
	h.Session, err = h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	supportProfileCapture(t, h, "submit", "submit-"+key, "", true)
	h.submit(t, "tenant-north", key)
	claimed := h.claim(t)
	recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	if currentRunStep(claimed).Kind != "model_decision" {
		t.Fatal("model decision not reached")
	}
	return claimed
}

func recoveryCommit(t *testing.T, h *runHarness, claimed *agentrun.ClaimedRun, result agentrun.StepResult) agentrun.CommitStepRequest {
	t.Helper()
	step := currentRunStep(*claimed)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	_, canonical, err := agentrun.CanonicalRegisteredStepResult(raw, step.Kind)
	if err != nil {
		t.Fatal(err)
	}
	req := agentrun.CommitStepRequest{Lease: claimed.Lease, Step: step, ResultJSON: canonical, CommitHash: agentrun.CommitHash(step, canonical)}
	response, err := h.Store.CommitStep(h.Ctx, h.Principal, req)
	if err != nil {
		t.Fatalf("recovery commit %s: %v", step.Kind, err)
	}
	if !response.AttemptClosed {
		claimed.Checkpoint, err = h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
		if err != nil {
			t.Fatal(err)
		}
	}
	return req
}

func recoveryChat(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun, observe bool) (agentrun.ReserveCallRequest, agentrun.SettleUsageRequest) {
	t.Helper()
	req := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, req)
	report := auditReportRequest(t, h, req, 10, 5)
	auditSettle(t, h, report)
	if observe {
		auditObserve(t, h, req, report, "accepted")
	}
	return req, report
}

func recoveryToolDecision(t *testing.T, claimed agentrun.ClaimedRun, callID, name string) agentrun.StepResult {
	t.Helper()
	args := map[string]any{"order_id": "order-1"}
	if name == "search_policy" {
		args = map[string]any{"query": "recovery policy"}
	}
	raw, _ := json.Marshal(map[string]any{"type": "tool", "name": name, "arguments": args})
	_, canonical, err := agentrun.SupportAgentToolDecision(claimed.Checkpoint.Snapshot, raw)
	if err != nil {
		t.Fatal(err)
	}
	return agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: callID, EvidenceRefs: []string{}, Content: canonical}
}

func recoveryReady(t *testing.T, h *runHarness, id string, timeout time.Duration) agentrun.Run {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
			t.Fatal(err)
		}
		r, err := h.Store.Get(h.Ctx, "tenant-north", id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == agentrun.Ready || r.State.Terminal() {
			return r
		}
		select {
		case <-h.Ctx.Done():
			t.Fatal("recovery context ended")
		case <-deadline.C:
			t.Fatalf("natural recovery did not become ready: state=%s", r.State)
		case <-tick.C:
		}
	}
}
