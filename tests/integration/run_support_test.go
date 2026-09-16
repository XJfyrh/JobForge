package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

// supportHarness retains real admission, sessions, PostgreSQL and the physical
// ledger. Business/model observations are synthetic; no model HTTP is sent and
// the control store has no business writer. This is a storage contract test.
func supportHarness(t *testing.T, key string, withOrder bool, decision string) *runHarness {
	t.Helper()
	h := setupRunHarness(t)
	h.Profile.ID, h.Profile.Hash = "support-storage-synthetic-v1", agentrun.Fingerprint("support-storage-synthetic-v1")
	h.Profile.Strategy = agentrun.SupportFixedStrategy
	h.Profile.Definition = json.RawMessage(`{"fixture":true,"proposal_schema":"support-proposal-v1"}`)
	h.Options.Profiles = []agentrun.Profile{h.Profile}
	for i := range h.Options.Workers {
		h.Options.Workers[i].ProfileIDs = []string{h.Profile.ID}
	}
	var err error
	h.Store, err = runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Store.EnsureProfiles(h.Ctx); err != nil {
		t.Fatal(err)
	}
	h.Service, err = agentrun.NewService(h.Store, h.Capture, []string{"tenant-a", "tenant-b"})
	if err != nil {
		t.Fatal(err)
	}
	// Populate the existing capture fixture under the exact admission key; the
	// subsequent Service.Submit still performs its ordinary capture and Admit.
	captureKey := agentrun.SnapshotKey("tenant-a", "submit", "submit-"+key, "")
	snapshot, err := h.Capture.Capture(h.Ctx, "tenant-a", "ticket-1", captureKey)
	if err != nil {
		t.Fatal(err)
	}
	var ticket business.Ticket
	var vector business.VersionVector
	if json.Unmarshal(snapshot.Ticket, &ticket) != nil || json.Unmarshal(snapshot.VersionVector, &vector) != nil {
		t.Fatal("invalid synthetic capture")
	}
	if !withOrder {
		ticket.OrderID = nil
		vector.Order, vector.Delivery = business.OrderVersion{}, business.DeliveryVersion{}
	}
	if decision == "no_action" {
		ticket.Status = "informational_only"
	}
	snapshot.Ticket, snapshot.VersionVector = supportStorageJSON(t, ticket), supportStorageJSON(t, vector)
	snapshot.ContentHash = agentrun.Fingerprint("synthetic-support-snapshot", string(snapshot.Ticket), string(snapshot.VersionVector))
	h.Capture.mu.Lock()
	h.Capture.snapshots["tenant-a:"+captureKey] = snapshot
	h.Capture.mu.Unlock()
	return h
}

func supportStorageJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func supportStorageRequest(t *testing.T, claimed agentrun.ClaimedRun, result agentrun.StepResult) agentrun.CommitStepRequest {
	t.Helper()
	step := currentRunStep(claimed)
	raw := supportStorageJSON(t, result)
	_, canonical, err := agentrun.CanonicalStepResultForStrategy(raw, step.Kind, agentrun.SupportFixedStrategy)
	if err != nil {
		t.Fatal(err)
	}
	return agentrun.CommitStepRequest{Lease: claimed.Lease, Step: step, ResultJSON: raw, CommitHash: agentrun.CommitHash(step, canonical)}
}

func supportStorageResult(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun, decision string) agentrun.StepResult {
	t.Helper()
	step, snapshot := currentRunStep(claimed), claimed.Checkpoint.Snapshot
	result := agentrun.StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, Content: json.RawMessage("null")}
	if step.Kind == "read_ticket" {
		result.Content, result.EvidenceRefs = snapshot.Ticket, []string{"business-evidence:" + snapshot.ID + ":ticket"}
		return result
	}
	if step.Kind == "submit_proposal" {
		last := claimed.Checkpoint.Steps[len(claimed.Checkpoint.Steps)-1]
		previous, _, err := agentrun.CanonicalStepResultForStrategy(last.Output, last.Kind, agentrun.SupportFixedStrategy)
		if err != nil {
			t.Fatal(err)
		}
		result.Proposal = previous.Proposal
		return result
	}
	var ticket business.Ticket
	if json.Unmarshal(snapshot.Ticket, &ticket) != nil {
		t.Fatal("invalid synthetic ticket")
	}
	sequence := agentrun.ToolSequence(step.Kind)
	if len(sequence) != 0 {
		result.ToolInvocationID = uuid.NewString()
		started, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: claimed.Lease, Step: step, ToolInvocationID: result.ToolInvocationID})
		if err != nil || !started.NewlyStarted {
			t.Fatalf("support BeginTool: %v", err)
		}
		switch step.Kind {
		case "get_order", "get_delivery":
			kind := "order"
			if step.Kind == "get_delivery" {
				kind = "delivery"
			}
			ref := "business-evidence:" + snapshot.ID + ":" + kind
			evidence := business.Evidence{SnapshotID: snapshot.ID, EvidenceRef: ref, Kind: kind}
			if ticket.OrderID == nil {
				evidence.Missing, evidence.MissingReason = true, "not_associated"
			} else if kind == "order" {
				deliveryID := "delivery-1"
				evidence.Order = &business.Order{TenantID: ticket.TenantID, OrderID: *ticket.OrderID, Revision: 1, DeliveryID: &deliveryID,
					Status: "shipped", OrderedAt: ticket.ObservedAt.Add(-48 * time.Hour), PromisedDeliveryAt: ticket.ObservedAt.Add(-time.Hour)}
			} else {
				evidence.Delivery = &business.Delivery{TenantID: ticket.TenantID, DeliveryID: "delivery-1", OrderID: *ticket.OrderID,
					AggregateRevision: 1, Status: "delivered", Events: []business.DeliveryEvent{{EventID: "synthetic-delivered", OccurredAt: ticket.ObservedAt,
						Status: "delivered", Note: "Synthetic storage-contract observation"}}}
			}
			result.Content, result.EvidenceRefs = supportStorageJSON(t, evidence), []string{ref}
		case "search_policy":
			ref := "business-policy:" + snapshot.IndexID + ":P01.1"
			result.Content = supportStorageJSON(t, map[string]any{"snapshot_id": snapshot.ID, "matches": []business.PolicyHit{{IndexID: snapshot.IndexID,
				ChunkID: "P01.1", PolicyVersion: ticket.PolicyVersion, EvidenceRef: ref, Source: "synthetic#P01.1", Text: "Synthetic protocol fixture; not a business-quality assertion."}}})
			result.EvidenceRefs = []string{ref}
		}
	} else {
		sequence = []agentrun.Subcall{agentrun.SubcallChat}
		model := `{"decision":"proposal","action":"escalate","conclusion":"delayed","requested_fields":[],"target_ticket_status":"escalated","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["E1#/order/promised_delivery_at","P01.1"]}]}`
		if ticket.OrderID == nil {
			model = `{"decision":"proposal","action":"request_information","conclusion":"insufficient","requested_fields":["ticket.order_id"],"target_ticket_status":"awaiting_information","claims":[{"kind":"missing","field":"ticket.order_id","refs":["T#/order_id","E1#/missing","P01.1"]}]}`
		}
		if decision == "no_action" {
			model = `{"decision":"no_action","action":"","conclusion":"insufficient","requested_fields":[],"target_ticket_status":"informational_only","claims":[{"kind":"ticket_status","mode":"informational_no_action","refs":["T#/status","P01.1"]}]}`
		}
		var err error
		result.Proposal, err = agentrun.SupportProposalFromModel(snapshot, claimed.Checkpoint.Steps, []byte(model))
		if err != nil {
			t.Fatalf("synthetic support proposal: %v", err)
		}
	}
	for _, subcall := range sequence {
		result.PhysicalCallID = uuid.NewString()
		reserved, err := h.Store.ReserveCall(h.Ctx, h.Principal, agentrun.ReserveCallRequest{Lease: claimed.Lease, Step: step,
			PhysicalCallID: result.PhysicalCallID, ToolInvocationID: result.ToolInvocationID, Subcall: subcall,
			ParameterHash: agentrun.Fingerprint("synthetic-support-call", result.PhysicalCallID), PriceHash: h.Profile.Pricing.Hash})
		if err != nil || !reserved.NewlyReserved {
			t.Fatalf("support ReserveCall: %v", err)
		}
		if _, err := h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: claimed.Lease, Step: step,
			PhysicalCallID: result.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: "accepted"}); err != nil {
			t.Fatalf("support ObserveCall: %v", err)
		}
	}
	return result
}

func assertSupportStoredProposal(t *testing.T, raw []byte, want *agentrun.Proposal) {
	t.Helper()
	result, _, err := agentrun.CanonicalStepResultForStrategy(raw, "model_proposal", agentrun.SupportFixedStrategy)
	if err != nil || result.Proposal == nil || !bytes.Equal(supportStorageJSON(t, result.Proposal), supportStorageJSON(t, want)) {
		t.Fatalf("stored support proposal lost content: %v", err)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(supportStorageJSON(t, result.Proposal), &fields) != nil || len(fields) != 8 ||
		result.Proposal.SupportProposalFields == nil || len(result.Proposal.Claims) == 0 {
		t.Fatal("stored proposal is not the eight-field support contract")
	}
}

func TestRunSupportStorageContract(t *testing.T) {
	for _, tc := range []struct {
		name, decision string
		withOrder      bool
	}{{"with_order_proposal", "proposal", true}, {"missing_order_proposal", "proposal", false}, {"missing_order_no_action", "no_action", false}} {
		t.Run(tc.name, func(t *testing.T) {
			h := supportHarness(t, tc.name, tc.withOrder, tc.decision)
			r := h.submit(t, "tenant-a", tc.name)
			claimed := h.claim(t)
			originalTicket := bytes.Clone(claimed.Checkpoint.Snapshot.Ticket)
			var kinds []string
			var final agentrun.CommitStepRequest
			var proposal *agentrun.Proposal
			for {
				kind := currentRunStep(claimed).Kind
				kinds = append(kinds, kind)
				result := supportStorageResult(t, h, claimed, tc.decision)
				request := supportStorageRequest(t, claimed, result)
				if kind == "get_delivery" || kind == "model_proposal" {
					assertSupportRejectedWithoutProgress(t, h, claimed, result)
				}
				response, err := h.Store.CommitStep(h.Ctx, h.Principal, request)
				if err != nil {
					t.Fatalf("support CommitStep %s: %v", kind, err)
				}
				if result.Proposal != nil {
					proposal = result.Proposal
					assertSupportStoredProposal(t, response.AcceptedStep.ResultJSON, proposal)
					confirmed, err := h.Store.GetAcceptedCommit(h.Ctx, h.Principal, claimed.Lease, request.Step.ID)
					if err != nil || !confirmed.Found || confirmed.AcceptedStep == nil || confirmed.AcceptedStep.CommitHash != request.CommitHash {
						t.Fatalf("support GetAcceptedCommit: %v", err)
					}
					assertSupportStoredProposal(t, confirmed.AcceptedStep.ResultJSON, proposal)
				}
				if response.AttemptClosed {
					final = request
					break
				}
				checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
				if err != nil || checkpoint.Run.CursorVersion != request.Step.CursorVersion+1 {
					t.Fatalf("support GetCheckpoint: %v", err)
				}
				if result.Proposal != nil {
					assertSupportStoredProposal(t, checkpoint.Steps[len(checkpoint.Steps)-1].Output, proposal)
				}
				duplicate, err := h.Store.CommitStep(h.Ctx, h.Principal, request)
				if err != nil || duplicate.CursorVersion != checkpoint.Run.CursorVersion || duplicate.AcceptedStep.CommitHash != request.CommitHash {
					t.Fatalf("support duplicate CommitStep: %v", err)
				}
				if result.Proposal != nil {
					changed := result
					changedProposal := *result.Proposal
					changedProposal.Summary += " altered after commit"
					changed.Proposal = &changedProposal
					if _, err := h.Store.CommitStep(h.Ctx, h.Principal, supportStorageRequest(t, claimed, changed)); !errors.Is(err, agentrun.ErrStepConflict) {
						t.Fatalf("changed support duplicate: %v", err)
					}
				}
				again, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
				if err != nil || again.Run.CursorVersion != checkpoint.Run.CursorVersion || len(again.Steps) != len(checkpoint.Steps) ||
					!again.Run.UpdatedAt.Equal(checkpoint.Run.UpdatedAt) {
					t.Fatalf("duplicate advanced support cursor: %v", err)
				}
				claimed.Checkpoint = again
			}
			wantKinds := []string{"read_ticket", "get_order", "search_policy", "model_proposal", "submit_proposal"}
			if tc.withOrder {
				wantKinds = []string{"read_ticket", "get_order", "get_delivery", "search_policy", "model_proposal", "submit_proposal"}
			}
			if !slices.Equal(kinds, wantKinds) {
				t.Fatalf("support path: %v", kinds)
			}
			assertSupportFinalState(t, h, claimed, r, final, proposal, originalTicket, tc.withOrder, tc.decision)
		})
	}
}

func assertSupportRejectedWithoutProgress(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun, result agentrun.StepResult) {
	t.Helper()
	before, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
	if err != nil {
		t.Fatal(err)
	}
	changes := []string{"foreign-source"}
	if result.Proposal != nil {
		changes = append(changes, "summary")
	}
	for _, name := range changes {
		var changed agentrun.StepResult
		if json.Unmarshal(supportStorageJSON(t, result), &changed) != nil {
			t.Fatal("clone synthetic support result")
		}
		want := agentrun.ErrModelProtocol
		if result.Proposal == nil {
			var evidence business.Evidence
			if json.Unmarshal(changed.Content, &evidence) != nil || evidence.Delivery == nil {
				t.Fatal("clone synthetic delivery")
			}
			evidence.Delivery.OrderID = "another-order"
			changed.Content = supportStorageJSON(t, evidence)
			want = agentrun.ErrStepConflict
		} else if name == "summary" {
			changed.Proposal.Summary += " Approved and refunded."
		} else {
			changed.Proposal.Claims[0].Refs[0].EvidenceRef = "business-evidence:" + uuid.NewString() + ":ticket"
		}
		if _, err := h.Store.CommitStep(h.Ctx, h.Principal, supportStorageRequest(t, claimed, changed)); !errors.Is(err, want) {
			t.Fatalf("rejected support %s: %v", name, err)
		}
		checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
		if err != nil || checkpoint.Run.CursorVersion != before.Run.CursorVersion || len(checkpoint.Steps) != len(before.Steps) ||
			!checkpoint.Run.UpdatedAt.Equal(before.Run.UpdatedAt) {
			t.Fatalf("rejected support output changed cursor: %v", err)
		}
	}
}

func assertSupportFinalState(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun, r agentrun.Run,
	final agentrun.CommitStepRequest, proposal *agentrun.Proposal, originalTicket []byte, withOrder bool, decision string,
) {
	t.Helper()
	view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil || view.LeaseUntil != nil || view.AttemptDeadline != nil {
		t.Fatalf("support final authority: %v", err)
	}
	var approvals, steps, deliveryCalls, slots int
	var ticket []byte
	if err := h.Pool.QueryRow(h.Ctx, `select
		(select count(*) from run_approvals where run_id=$1 and status='pending'),
		(select count(*) from run_steps where run_id=$1),
		(select count(*) from physical_calls where run_id=$1 and step_kind='get_delivery'),
		(select coalesce(sum(used),0) from execution_slots),ticket_binding from runs where run_id=$1`, r.ID).
		Scan(&approvals, &steps, &deliveryCalls, &slots, &ticket); err != nil {
		t.Fatal(err)
	}
	canonicalTicket, err := agentrun.CanonicalCheckpointJSON(ticket)
	if err != nil || !bytes.Equal(canonicalTicket, originalTicket) || slots != 0 || int64(steps) != view.CursorVersion ||
		withOrder && deliveryCalls != 1 || !withOrder && deliveryCalls != 0 {
		t.Fatalf("support changed business binding or left partial closure: %v", err)
	}
	if decision == "proposal" {
		if view.State != agentrun.AwaitingApproval || view.Outcome != nil || view.ProposalRef == nil || approvals != 1 {
			t.Fatal("support proposal did not remain an unapplied approval recommendation")
		}
	} else if view.State != agentrun.Succeeded || view.Outcome == nil || *view.Outcome != "no_action" || approvals != 0 || proposal.Action != "" {
		t.Fatal("support no_action acquired a write or failed to finish")
	}
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, final); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("closed support attempt replay: %v", err)
	}
	confirmed, err := h.Store.GetAcceptedCommit(h.Ctx, h.Principal, claimed.Lease, final.Step.ID)
	if err != nil || !confirmed.Found || confirmed.AcceptedStep == nil || confirmed.AcceptedStep.CommitHash != final.CommitHash {
		t.Fatalf("closed support accepted commit: %v", err)
	}
	assertSupportStoredProposal(t, confirmed.AcceptedStep.ResultJSON, proposal)
	page, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 20)
	if err != nil || len(page.Items) != steps {
		t.Fatalf("support protected output query: %v", err)
	}
	assertSupportStoredProposal(t, page.Items[len(page.Items)-1].Output, proposal)
}
