package integration

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func checkpointCommitRequest(t *testing.T, claimed agentrun.ClaimedRun, result agentrun.StepResult) agentrun.CommitStepRequest {
	t.Helper()
	step := currentRunStep(claimed)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	_, canonical, err := agentrun.CanonicalStepResult(raw, step.Kind)
	if err != nil {
		t.Fatal(err)
	}
	return agentrun.CommitStepRequest{Lease: claimed.Lease, Step: step, ResultJSON: raw, CommitHash: agentrun.CommitHash(step, canonical)}
}

// checkpointFixtureResult uses labelled synthetic observations through the real
// call ledger. It does not execute or register a production model/tool adapter.
func checkpointFixtureResult(t *testing.T, h *runHarness, claimed agentrun.ClaimedRun, decision string, correction bool) agentrun.StepResult {
	t.Helper()
	step, snapshot := currentRunStep(claimed), claimed.Checkpoint.Snapshot
	result := agentrun.StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, Content: json.RawMessage(`null`)}
	if step.Kind == "read_ticket" {
		result.Content, result.EvidenceRefs = snapshot.Ticket, []string{"business-evidence:" + snapshot.ID + ":ticket"}
		return result
	}
	if step.Kind == "submit_proposal" {
		previous := claimed.Checkpoint.Steps[len(claimed.Checkpoint.Steps)-1]
		accepted, _, err := agentrun.CanonicalStepResult(previous.Output, previous.Kind)
		if err != nil {
			t.Fatal(err)
		}
		result.Proposal = accepted.Proposal
		return result
	}
	sequence := agentrun.ToolSequence(step.Kind)
	if len(sequence) != 0 {
		result.ToolInvocationID = uuid.NewString()
		started, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: claimed.Lease, Step: step, ToolInvocationID: result.ToolInvocationID})
		if err != nil || !started.NewlyStarted {
			t.Fatalf("fixture tool authorization: %+v %v", started, err)
		}
		if step.Kind == "search_policy" {
			ref := "business-policy:" + snapshot.IndexID + ":P01.1"
			result.EvidenceRefs = []string{ref}
			result.Content = json.RawMessage(`{"snapshot_id":"` + snapshot.ID + `","matches":[{"index_id":"` + snapshot.IndexID + `","chunk_id":"P01.1","evidence_ref":"` + ref + `","distance":1e-3}]}`)
		} else {
			kind := strings.TrimPrefix(step.Kind, "get_")
			ref := "business-evidence:" + snapshot.ID + ":" + kind
			result.EvidenceRefs = []string{ref}
			result.Content = json.RawMessage(`{"snapshot_id":"` + snapshot.ID + `","kind":"` + kind + `","evidence_ref":"` + ref + `","fixture_value":1e3}`)
		}
	} else {
		sequence = []agentrun.Subcall{agentrun.SubcallChat}
		result.CorrectionRequired = correction && step.Kind == "model_proposal"
		if !result.CorrectionRequired {
			action := ""
			if decision == "proposal" {
				action = "escalate"
			}
			result.Proposal = &agentrun.Proposal{Decision: decision, Summary: "Synthetic checkpoint acceptance", Action: action,
				EvidenceRefs: []string{"business-evidence:" + snapshot.ID + ":ticket", "business-policy:" + snapshot.IndexID + ":P01.1"}}
		}
	}
	for _, subcall := range sequence {
		result.PhysicalCallID = uuid.NewString()
		reserved, err := h.Store.ReserveCall(h.Ctx, h.Principal, agentrun.ReserveCallRequest{Lease: claimed.Lease, Step: step,
			PhysicalCallID: result.PhysicalCallID, ToolInvocationID: result.ToolInvocationID, Subcall: subcall,
			ParameterHash: agentrun.Fingerprint("synthetic-call", result.PhysicalCallID), PriceHash: h.Profile.Pricing.Hash})
		if err != nil || !reserved.NewlyReserved {
			t.Fatalf("fixture physical authorization: %+v %v", reserved, err)
		}
		businessOutcome, code := "accepted", ""
		if result.CorrectionRequired {
			businessOutcome, code = "rejected", "MODEL_PROTOCOL_ERROR"
		}
		_, err = h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: claimed.Lease, Step: step,
			PhysicalCallID: result.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: businessOutcome, ErrorCode: code})
		if err != nil {
			t.Fatalf("fixture observation: %v", err)
		}
	}
	return result
}

func checkpointAdvance(t *testing.T, h *runHarness, claimed *agentrun.ClaimedRun, decision string, correction bool) (agentrun.CommitStepRequest, agentrun.CommitStepResponse) {
	t.Helper()
	request := checkpointCommitRequest(t, *claimed, checkpointFixtureResult(t, h, *claimed, decision, correction))
	response, err := h.Store.CommitStep(h.Ctx, h.Principal, request)
	if err != nil {
		t.Fatalf("commit %s: %v", request.Step.Kind, err)
	}
	if !response.AttemptClosed {
		checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
		if err != nil {
			t.Fatal(err)
		}
		claimed.Checkpoint = checkpoint
	}
	return request, response
}

func TestRunCheckpointProposalAndNoActionCloseAtomically(t *testing.T) {
	for _, outcome := range []string{"proposal", "no_action", "corrected-proposal"} {
		t.Run(outcome, func(t *testing.T) {
			h := setupRunHarness(t)
			r := h.submit(t, "tenant-a", outcome)
			claimed := h.claim(t)
			decision := outcome
			if outcome == "corrected-proposal" {
				decision = "proposal"
			}
			var final agentrun.CommitStepRequest
			for {
				request, response := checkpointAdvance(t, h, &claimed, decision, outcome == "corrected-proposal")
				if response.AttemptClosed {
					final = request
					if response.NextStep != nil {
						t.Fatal("closed attempt retained next-step permission")
					}
					break
				}
				// Repeating the exact request must retain identity after JSONB has
				// normalized exponent numbers in the synthetic tool response.
				replayed, err := h.Store.CommitStep(h.Ctx, h.Principal, request)
				if err != nil || replayed.AcceptedStep.CommitHash != response.AcceptedStep.CommitHash ||
					agentrun.CommitHash(replayed.AcceptedStep.Identity, replayed.AcceptedStep.ResultJSON) != request.CommitHash {
					t.Fatalf("lost intermediate ACK: %s %v", request.Step.Kind, err)
				}
			}
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil || view.LeaseUntil != nil || view.AttemptDeadline != nil || view.Outcome != nil && *view.Outcome != "no_action" {
				t.Fatalf("closed run view: %+v %v", view, err)
			}
			var slots, approvalCount, stepCount int
			var owner, session, active *string
			var attemptOutcome string
			if err := h.Pool.QueryRow(h.Ctx, "select coalesce(sum(used),0) from execution_slots").Scan(&slots); err != nil {
				t.Fatal(err)
			}
			if err := h.Pool.QueryRow(h.Ctx, "select worker_id,session_id::text,active_call_id::text from runs where run_id=$1", r.ID).Scan(&owner, &session, &active); err != nil {
				t.Fatal(err)
			}
			if err := h.Pool.QueryRow(h.Ctx, "select outcome from run_attempts where run_id=$1", r.ID).Scan(&attemptOutcome); err != nil {
				t.Fatal(err)
			}
			if err := h.Pool.QueryRow(h.Ctx, "select count(*) from run_approvals where run_id=$1", r.ID).Scan(&approvalCount); err != nil {
				t.Fatal(err)
			}
			if err := h.Pool.QueryRow(h.Ctx, "select count(*) from run_steps where run_id=$1", r.ID).Scan(&stepCount); err != nil {
				t.Fatal(err)
			}
			if slots != 0 || owner != nil || session != nil || active != nil || int64(stepCount) != view.CursorVersion {
				t.Fatal("final commit left partial attempt/cursor/capacity state")
			}
			if decision == "proposal" {
				if view.State != agentrun.AwaitingApproval || view.Outcome != nil || attemptOutcome != "yielded_approval" || approvalCount != 1 {
					t.Fatalf("proposal misreported as execution success: state=%s outcome=%s approvals=%d", view.State, attemptOutcome, approvalCount)
				}
				var bound bool
				if err := h.Pool.QueryRow(h.Ctx, `select a.status='pending' and a.snapshot_id=r.snapshot_id and a.snapshot_hash=r.snapshot_hash
					and a.version_vector=r.version_vector and a.proposal_ref=r.proposal_ref and a.permission_expires_at=r.permission_expires_at
					and a.permission_expires_at<=r.run_deadline and a.permission_expires_at<=a.created_at+interval '1 hour'
					from run_approvals a join runs r using(tenant_id,run_id) where r.run_id=$1`, r.ID).Scan(&bound); err != nil || !bound {
					t.Fatalf("proposal approval binding: bound=%t %v", bound, err)
				}
			} else if view.State != agentrun.Succeeded || view.Outcome == nil || *view.Outcome != "no_action" || attemptOutcome != "succeeded" || approvalCount != 0 {
				t.Fatal("explicit no_action did not close successfully")
			}
			if _, err := h.Store.CommitStep(h.Ctx, h.Principal, final); !errors.Is(err, agentrun.ErrStaleLease) {
				t.Fatalf("closed lease replay should fail: %v", err)
			}
			if _, err := h.Pool.Exec(h.Ctx, "update worker_sessions set seen_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second'"); err != nil {
				t.Fatal(err)
			}
			before, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			accepted, err := h.Store.GetAcceptedCommit(h.Ctx, h.Principal, claimed.Lease, final.Step.ID)
			if err != nil || !accepted.Found || accepted.AttemptOutcome != attemptOutcome || accepted.AcceptedStep.CommitHash != final.CommitHash ||
				agentrun.CommitHash(accepted.AcceptedStep.Identity, accepted.AcceptedStep.ResultJSON) != final.CommitHash {
				t.Fatalf("read-only final ACK confirmation: %+v %v", accepted, err)
			}
			after, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil || !before.UpdatedAt.Equal(after.UpdatedAt) || before.CursorVersion != after.CursorVersion || before.State != after.State {
				t.Fatalf("read-only confirmation mutated Run: %v", err)
			}
			wrong := claimed.Lease
			wrong.FencingToken++
			if _, err := h.Store.GetAcceptedCommit(h.Ctx, h.Principal, wrong, final.Step.ID); !errors.Is(err, agentrun.ErrStaleLease) {
				t.Fatalf("confirmation accepted another attempt identity: %v", err)
			}
		})
	}
}

func TestRunCheckpointDuplicateConflictAndExpiredAuthority(t *testing.T) {
	h := setupRunHarness(t)
	h.submit(t, "tenant-a", "duplicate-step")
	claimed := h.claim(t)
	request := checkpointCommitRequest(t, claimed, checkpointFixtureResult(t, h, claimed, "proposal", false))
	var wg sync.WaitGroup
	errorsOut := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		wg.Go(func() {
			<-start
			_, err := h.Store.CommitStep(h.Ctx, h.Principal, request)
			errorsOut <- err
		})
	}
	close(start)
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	changed := request
	changed.CommitHash = agentrun.Fingerprint("different-output")
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, changed); !errors.Is(err, agentrun.ErrStepConflict) {
		t.Fatalf("conflicting accepted step: %v", err)
	}
	options := h.Options
	options.Profiles = append([]agentrun.Profile(nil), options.Profiles...)
	options.Profiles[0].Executable = false
	offline, err := runpostgres.New(h.Pool, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set frozen=true"); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := offline.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
	if err != nil || len(checkpoint.Steps) != 1 || checkpoint.Run.CursorVersion != 1 {
		t.Fatalf("checkpoint blocked by unavailable profile/budget: %v", err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update runs set lease_until=clock_timestamp()-interval '1 millisecond' where run_id=$1", claimed.Lease.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, request); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("expired duplicate accepted: %v", err)
	}
	accepted, err := h.Store.GetAcceptedCommit(h.Ctx, h.Principal, claimed.Lease, request.Step.ID)
	if err != nil || !accepted.Found || accepted.AcceptedStep.CommitHash != request.CommitHash {
		t.Fatalf("expired read-only confirmation unavailable: %v", err)
	}
}

func TestRunCheckpointRejectsUnobservedCallsAndForeignEvidence(t *testing.T) {
	h := setupRunHarness(t)
	h.submit(t, "tenant-a", "evidence-binding")
	claimed := h.claim(t)
	checkpointAdvance(t, h, &claimed, "proposal", false)
	result := checkpointFixtureResult(t, h, claimed, "proposal", false)
	valid := checkpointCommitRequest(t, claimed, result)
	result.PhysicalCallID = uuid.NewString()
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, checkpointCommitRequest(t, claimed, result)); err == nil {
		t.Fatal("unobserved physical call accepted")
	}
	result.PhysicalCallID = ""
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, checkpointCommitRequest(t, claimed, result)); err == nil {
		t.Fatal("missing call accepted")
	}
	if err := json.Unmarshal(valid.ResultJSON, &result); err != nil {
		t.Fatal(err)
	}
	result.EvidenceRefs = []string{"business-evidence:" + uuid.NewString() + ":order"}
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, checkpointCommitRequest(t, claimed, result)); !errors.Is(err, agentrun.ErrStepConflict) {
		t.Fatalf("foreign evidence accepted: %v", err)
	}
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, valid); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
	if err != nil || len(checkpoint.Steps) != 2 {
		t.Fatalf("invalid commits advanced progress: %v", err)
	}
}

func TestRunCheckpointFinalTransactionRollback(t *testing.T) {
	h := setupRunHarness(t)
	h.submit(t, "tenant-a", "atomic-final")
	claimed := h.claim(t)
	for currentRunStep(claimed).Kind != "submit_proposal" {
		checkpointAdvance(t, h, &claimed, "proposal", false)
	}
	request := checkpointCommitRequest(t, claimed, checkpointFixtureResult(t, h, claimed, "proposal", false))
	_, err := h.Pool.Exec(h.Ctx, `create function reject_final_checkpoint() returns trigger language plpgsql as $body$
		begin if new.event_type='step_committed' and new.state='awaiting_approval' then raise exception 'synthetic transaction failure'; end if; return new; end $body$;
		create trigger reject_final_checkpoint before insert on run_events for each row execute function reject_final_checkpoint()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, request); err == nil {
		t.Fatal("injected event failure was ignored")
	}
	checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
	if err != nil || checkpoint.Run.CursorVersion != claimed.Checkpoint.Run.CursorVersion || checkpoint.Run.State != agentrun.Running ||
		checkpoint.Authority.WorkerID != claimed.Lease.WorkerID || checkpoint.Run.ProposalRef != nil {
		t.Fatalf("failed final transaction leaked Run changes: %v", err)
	}
	var approvals, finished, slots int
	if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from run_approvals),
		(select count(*) from run_attempts where finished_at is not null),(select sum(used) from execution_slots)`).Scan(&approvals, &finished, &slots); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 || finished != 0 || slots != 3 {
		t.Fatalf("failed final transaction leaked child/capacity changes: approvals=%d finished=%d slots=%d", approvals, finished, slots)
	}
	if _, err := h.Pool.Exec(h.Ctx, "drop trigger reject_final_checkpoint on run_events; drop function reject_final_checkpoint()"); err != nil {
		t.Fatal(err)
	}
	if response, err := h.Store.CommitStep(h.Ctx, h.Principal, request); err != nil || !response.AttemptClosed {
		t.Fatalf("retry after rollback: %+v %v", response, err)
	}
}

func TestRunCheckpointCancelWinsBeforeCommit(t *testing.T) {
	h := setupRunHarness(t)
	r := h.submit(t, "tenant-a", "cancel-before-step")
	claimed := h.claim(t)
	request := checkpointCommitRequest(t, claimed, checkpointFixtureResult(t, h, claimed, "proposal", false))
	if _, err := h.Store.Cancel(h.Ctx, r.TenantID, r.ID, "cancel-step"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, request); !errors.Is(err, agentrun.ErrCancelRequested) {
		t.Fatalf("cancelled execution committed: %v", err)
	}
	page, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 20)
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("cancelled step was persisted: %v", err)
	}
}

func TestRunCheckpointByteBudgetRejectsWithoutTruncation(t *testing.T) {
	h := setupRunHarness(t)
	h.submit(t, "tenant-a", "checkpoint-limit")
	claimed := h.claim(t)
	request := checkpointCommitRequest(t, claimed, checkpointFixtureResult(t, h, claimed, "no_action", false))
	// The fixed graph currently needs at most seven steps. Inject the persisted
	// boundary counter to exercise rejection without inventing a production graph.
	if _, err := h.Pool.Exec(h.Ctx, "update runs set checkpoint_bytes=$2 where run_id=$1", claimed.Lease.RunID, agentrun.MaxCheckpointBytes-1); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, request); !errors.Is(err, agentrun.ErrorCode("CHECKPOINT_TOO_LARGE")) {
		t.Fatalf("oversized aggregate checkpoint accepted: %v", err)
	}
	checkpoint, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, claimed.Lease)
	if err != nil || len(checkpoint.Steps) != 0 || checkpoint.Run.CursorVersion != 0 ||
		!bytes.Equal(checkpoint.Snapshot.Ticket, claimed.Checkpoint.Snapshot.Ticket) || !checkpoint.Run.UpdatedAt.Equal(claimed.Checkpoint.Run.UpdatedAt) {
		t.Fatalf("limit rejection truncated evidence or advanced state: %v", err)
	}
	if !checkpoint.Run.LeaseUntil.After(time.Now()) {
		t.Fatal("test exhausted the live lease before verifying the boundary")
	}
}
