package integration

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func setupApprovalHarness(t *testing.T) *runHarness {
	t.Helper()
	h := setupRecoveryHarness(t)
	d, err := agentrun.DecodeSupportDefinition(h.Profile.Definition)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	d.SchemaVersion, d.Program.ApprovalPolicy = 4, agentrun.TicketResolutionApprovalPolicy
	d.Action = &agentrun.SupportActionDefinition{Operation: business.ResolutionOperation, Origin: "http://127.0.0.1:18093", KeyID: "synthetic-approval-key", PublicKeySHA256: business.PublicKeyHash(key.Public().(ed25519.PublicKey))}
	h.Profile, err = agentrun.BuildSupportProfile("approval-synthetic-v1", d)
	if err != nil {
		t.Fatal(err)
	}
	h.Profile.Executable = true
	h.Options.Profiles = []agentrun.Profile{h.Profile}
	h.Options.ActionSigners = map[string]ed25519.PrivateKey{d.Action.KeyID: key}
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

// Uses the ordinary fenced commits and labelled synthetic provider observations;
// it never inserts approval state or fabricates a natural lease failure.
func approvalPending(t *testing.T, h *runHarness, key string) agentrun.Run {
	t.Helper()
	var claimed agentrun.ClaimedRun
	if h.Session.ID == "" {
		claimed = recoveryAtDecision(t, h, key)
	} else {
		supportProfileCapture(t, h, "submit", "submit-"+key, "", true)
		h.submit(t, "tenant-north", key)
		claimed = h.claim(t)
		recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	}
	return approvalCommitProposal(t, h, claimed)
}

func approvalCommitProposal(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun) agentrun.Run {
	t.Helper()
	for _, tool := range []string{"get_order", "get_delivery", "search_policy"} {
		chat, _ := recoveryChat(t, h, claimed, true)
		recoveryCommit(t, h, &claimed, recoveryToolDecision(t, claimed, chat.PhysicalCallID, tool))
		recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	}
	proposal, err := agentrun.SupportProposalFromModel(claimed.Checkpoint.Snapshot, claimed.Checkpoint.Steps, []byte(`{"decision":"proposal","action":"escalate","conclusion":"delayed","requested_fields":[],"target_ticket_status":"escalated","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["T#/observed_at","E1#/order/promised_delivery_at","P01.1"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	chat, _ := recoveryChat(t, h, claimed, true)
	result := agentrun.StepResult{SchemaVersion: 1, PhysicalCallID: chat.PhysicalCallID, EvidenceRefs: []string{}, Content: json.RawMessage("null"), Proposal: proposal}
	recoveryCommit(t, h, &claimed, result)
	result.PhysicalCallID = ""
	recoveryCommit(t, h, &claimed, result)
	r, err := h.Store.Get(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID)
	if err != nil || r.State != agentrun.AwaitingApproval {
		t.Fatal("pending approval", r.State, err)
	}
	return r
}

func approveRun(t *testing.T, h *runHarness, r agentrun.Run, decision string) agentrun.ApprovalResponse {
	t.Helper()
	v, err := h.Store.Approval(h.Ctx, r.TenantID, r.ID)
	if err != nil || !v.Available {
		t.Fatal("approval view", err)
	}
	result, err := h.Store.DecideApproval(h.Ctx, r.TenantID, r.ID, "synthetic-approver", "approval-key", agentrun.ApprovalRequest{SchemaVersion: 1, Decision: decision, ProposalHash: v.ProposalHash})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRunApprovalConcurrentDecisionsAndStableActor(t *testing.T) {
	h := setupApprovalHarness(t)
	r := approvalPending(t, h, "approval-concurrent")
	v, err := h.Store.Approval(h.Ctx, r.TenantID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := agentrun.ApprovalRequest{SchemaVersion: 1, Decision: "approve", ProposalHash: v.ProposalHash}
	var wg sync.WaitGroup
	results := make(chan agentrun.ApprovalResponse, 10)
	failures := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := h.Store.DecideApproval(h.Ctx, r.TenantID, r.ID, "stable-actor", uuid.NewString(), request)
			if err != nil {
				failures <- err
			} else {
				results <- result
			}
		}()
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	var first agentrun.ApprovalResponse
	for result := range results {
		if first.Approval.ApprovalID == nil {
			first = result
		}
		if *first.Approval.ApprovalID != *result.Approval.ApprovalID || !reflect.DeepEqual(first.Approval.DecidedAt, result.Approval.DecidedAt) || result.Run.State != agentrun.Ready || result.Run.RecoveryCount != 0 {
			t.Fatal("decision identity changed")
		}
	}
	if _, err := h.Store.DecideApproval(h.Ctx, r.TenantID, r.ID, "other-actor", "new-key", request); !errors.Is(err, agentrun.ErrApprovalConflict) {
		t.Fatal("actor conflict", err)
	}
	request.Decision = "reject"
	if _, err := h.Store.DecideApproval(h.Ctx, r.TenantID, r.ID, "stable-actor", "new-key", request); !errors.Is(err, agentrun.ErrApprovalConflict) {
		t.Fatal("decision conflict", err)
	}
	if _, err := h.Store.Approval(h.Ctx, "tenant-south", r.ID); !errors.Is(err, agentrun.ErrNotFound) {
		t.Fatal("tenant leak", err)
	}
	var count int
	if err := h.Pool.QueryRow(h.Ctx, `select count(*) from run_events where run_id=$1 and event_type='approval_approved'`, r.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("approval queued more than once", count, err)
	}
}

func TestRunApprovalDisabledProfileRejectAndFrozenActionRead(t *testing.T) {
	h := setupApprovalHarness(t)
	r := approvalPending(t, h, "disabled-profile")
	h.Options.Profiles[0].Executable = false
	disabled, err := runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	h.Store = disabled
	response := approveRun(t, h, r, "reject")
	if response.Run.State != agentrun.Succeeded || response.Run.Outcome == nil || *response.Run.Outcome != "rejected" {
		t.Fatal("rejection outcome")
	}
	if _, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRunApprovalActionLedgerBudgetAndReceiptCompletion(t *testing.T) {
	h := setupApprovalHarness(t)
	r := approvalPending(t, h, "action-ledger")
	approveRun(t, h, r, "approve")
	claimed := h.claim(t)
	step := currentRunStep(claimed)
	if step.Kind != business.ResolutionOperation || claimed.Lease.AttemptNo != 2 || claimed.Checkpoint.Run.RecoveryCount != 0 {
		t.Fatal("approval consumed recovery")
	}
	binding, err := h.Store.AuthorizeAction(h.Ctx, h.Principal, claimed.Lease, step)
	if err != nil || binding.Action == nil {
		t.Fatal("authorize action", err)
	}
	a := binding.Action.Authorization
	hash, _ := a.Hash()
	before := ledgerView(t, h, claimed.Lease)
	req := agentrun.ReserveActionCallRequest{Lease: claimed.Lease, Step: step, ID: uuid.NewString(), Kind: "action_write", OperationID: a.OperationID, AuthorizationHash: hash}
	permit, err := h.Store.ReserveActionCall(h.Ctx, h.Principal, req)
	if err != nil || !permit.NewlyReserved {
		t.Fatal("write permit", err)
	}
	replay, err := h.Store.ReserveActionCall(h.Ctx, h.Principal, req)
	if err != nil || replay.NewlyReserved {
		t.Fatal("permit replay sent again", err)
	}
	req.ID = uuid.NewString()
	if _, err := h.Store.ReserveActionCall(h.Ctx, h.Principal, req); !errors.Is(err, agentrun.ErrCallConflict) {
		t.Fatal("second send in attempt", err)
	}
	if !reflect.DeepEqual(before.Budget, ledgerView(t, h, claimed.Lease).Budget) {
		t.Fatal("action changed provider budget")
	}
	if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set frozen=true"); err != nil {
		t.Fatal(err)
	}
	query := req
	query.ID = uuid.NewString()
	query.Kind = "receipt_query"
	if _, err := h.Store.ReserveActionCall(h.Ctx, h.Principal, query); err != nil {
		t.Fatal("frozen receipt query", err)
	}
	if _, err := h.Store.GetAction(h.Ctx, h.Principal, claimed.Lease, step); err != nil {
		t.Fatal("frozen get action", err)
	}
	// Synthetic receipt validates control completion only. Real business HTTP
	// commit/response loss is a separate acceptance layer, never claimed here.
	receipt := business.ActionReceipt{SchemaVersion: 1, TenantID: r.TenantID, OperationID: a.OperationID, BusinessRequestID: r.BusinessRequestID, AuthorizationHash: hash, ProposalHash: a.ProposalHash,
		ParametersHash: a.ParametersHash, ApprovalID: a.ApprovalID, TicketID: r.TicketID, BeforeRevision: 1, AfterRevision: 2, TicketStatus: "escalated", AppliedAt: a.AuthorizedAt + 1, RetainUntil: a.AuthorizedAt + 1 + 30*24*time.Hour.Microseconds()}
	receipt.ReceiptHash = receipt.Hash()
	if _, err := h.Store.CompleteAction(h.Ctx, h.Principal, agentrun.CompleteActionRequest{Lease: claimed.Lease, Step: step, Receipt: receipt}); !errors.Is(err, agentrun.ErrCallConflict) {
		t.Fatal("unobserved receipt accepted", err)
	}
	if _, err := h.Store.ObserveActionCall(h.Ctx, h.Principal, agentrun.ObserveActionCallRequest{Lease: claimed.Lease, ID: query.ID, TransportOutcome: "response", ObservationHash: agentrun.Fingerprint("jobforge.run.action-receipt-observation.v1", receipt.ReceiptHash)}); err != nil {
		t.Fatal(err)
	}
	completed, err := h.Store.CompleteAction(h.Ctx, h.Principal, agentrun.CompleteActionRequest{Lease: claimed.Lease, Step: step, Receipt: receipt, PhysicalCallID: query.ID})
	if err != nil || !completed.AttemptClosed || completed.State != agentrun.Succeeded {
		t.Fatal("frozen completion", err)
	}
	if _, err := h.Store.CompleteAction(h.Ctx, h.Principal, agentrun.CompleteActionRequest{Lease: claimed.Lease, Step: step, Receipt: receipt}); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatal("old completion altered final state", err)
	}
	effect, err := h.Store.Effect(h.Ctx, r.TenantID, r.ID)
	if err != nil || effect.State != "applied" {
		t.Fatal("effect", err)
	}
	calls, err := h.Store.ActionCalls(h.Ctx, r.TenantID, r.ID)
	if err != nil || len(calls.Items) != 2 {
		t.Fatal("independent ledger", err)
	}
	if accepted, err := h.Store.GetAcceptedCommit(h.Ctx, h.Principal, claimed.Lease, step.ID); err != nil || !accepted.Found {
		t.Fatal("lost ack lookup", err)
	}
	runApprovalSDK(t, h, "applied", r.ID)
}
