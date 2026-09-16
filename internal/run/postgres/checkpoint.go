package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

const stepColumns = `step_id,sequence,kind,input_hash,profile_hash,snapshot_hash,
	commit_hash,output_ref,output,cursor_version,created_at`

func readStep(row pgx.Row) (agentrun.Step, error) {
	var step agentrun.Step
	err := row.Scan(&step.ID, &step.Sequence, &step.Kind, &step.InputHash, &step.ProfileHash, &step.SnapshotHash,
		&step.CommitHash, &step.OutputRef, &step.Output, &step.CursorVersion, &step.CreatedAt)
	if err != nil {
		return step, err
	}
	step.Output, err = canonicalStoredJSON(step.Output)
	return step, err
}

func canonicalStoredJSON(raw []byte) ([]byte, error) {
	canonical, err := agentrun.CanonicalCheckpointJSON(raw)
	if err != nil {
		return nil, agentrun.ErrInternal
	}
	return canonical, nil
}

func loadCheckpoint(ctx context.Context, tx pgx.Tx, r agentrun.Run, a agentrun.Authority) (agentrun.Checkpoint, error) {
	checkpoint := agentrun.Checkpoint{Run: r, Authority: a, Steps: []agentrun.Step{}}
	snapshot := &checkpoint.Snapshot
	snapshot.TenantID, snapshot.TicketID, snapshot.ID, snapshot.ContentHash = r.TenantID, r.TicketID, r.SnapshotID, r.SnapshotHash
	snapshot.VersionVector = r.VersionVector
	if err := tx.QueryRow(ctx, `select ticket_binding,index_id,index_profile_hash from runs where tenant_id=$1 and run_id=$2`,
		r.TenantID, r.ID).Scan(&snapshot.Ticket, &snapshot.IndexID, &snapshot.IndexProfileHash); err != nil {
		return checkpoint, err
	}
	var err error
	snapshot.Ticket, err = canonicalStoredJSON(snapshot.Ticket)
	if err != nil {
		return checkpoint, err
	}
	snapshot.VersionVector, err = canonicalStoredJSON(snapshot.VersionVector)
	if err != nil {
		return checkpoint, err
	}
	rows, err := tx.Query(ctx, "select "+stepColumns+" from run_steps where tenant_id=$1 and run_id=$2 order by sequence", r.TenantID, r.ID)
	if err != nil {
		return checkpoint, err
	}
	defer rows.Close()
	for rows.Next() {
		step, err := readStep(rows)
		if err != nil {
			return checkpoint, err
		}
		checkpoint.Steps = append(checkpoint.Steps, step)
		if len(checkpoint.Steps) > 32 {
			return checkpoint, agentrun.ErrInternal
		}
	}
	if err := rows.Err(); err != nil {
		return checkpoint, err
	}
	rows.Close()
	return checkpoint, fillBudget(ctx, tx, &checkpoint.Run)
}

// GetCheckpoint reads only under current fenced execution, without reserving
// budget or requiring an available executable profile. It never advances state.
func (s *Store) GetCheckpoint(ctx context.Context, principal string, lease agentrun.Lease) (agentrun.Checkpoint, error) {
	var checkpoint agentrun.Checkpoint
	if validateLedgerLease(lease) != nil {
		return checkpoint, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, lease.TenantID, lease.RunID)
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, lease.WorkerID, lease.SessionID, r.TenantID, "", now, true); err != nil {
			return err
		}
		if err := agentrun.CheckExecution(r, a, lease, now); err != nil {
			return err
		}
		checkpoint, err = loadCheckpoint(ctx, tx, r, a)
		return err
	})
	return checkpoint, err
}

// CommitStep validates authority before any duplicate acknowledgement, then
// atomically stores protected evidence and the server-derived next cursor.
func (s *Store) CommitStep(ctx context.Context, principal string, req agentrun.CommitStepRequest) (agentrun.CommitStepResponse, error) {
	return s.commitStep(ctx, principal, req, false)
}

// YieldForApproval is an internal constrained entry point for proposal commits.
// It has no public approval/write route and cannot yield arbitrary checkpoints.
func (s *Store) YieldForApproval(ctx context.Context, principal string, req agentrun.CommitStepRequest) (agentrun.CommitStepResponse, error) {
	return s.commitStep(ctx, principal, req, true)
}

func (s *Store) commitStep(ctx context.Context, principal string, req agentrun.CommitStepRequest, requireProposal bool) (agentrun.CommitStepResponse, error) {
	var response agentrun.CommitStepResponse
	if validateLedgerLease(req.Lease) != nil || !agentrun.ValidUUID(req.Step.ID) || !agentrun.ValidHash(req.CommitHash) {
		return response, agentrun.ErrInvalidArgument
	}
	err := s.transact(ctx, func(tx pgx.Tx) error {
		r, a, err := lockRun(ctx, tx, req.Lease.TenantID, req.Lease.RunID)
		if err != nil {
			return err
		}
		var resources attemptResources
		if req.Step.Kind == "submit_proposal" {
			resources, err = lockAttemptResources(ctx, tx, r, a)
			if err != nil {
				return err
			}
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, req.Lease.WorkerID, req.Lease.SessionID, r.TenantID, "", now, true); err != nil {
			return err
		}
		if err := agentrun.CheckExecution(r, a, req.Lease, now); err != nil {
			return err
		}
		prior, priorErr := readStep(tx.QueryRow(ctx, "select "+stepColumns+" from run_steps where tenant_id=$1 and run_id=$2 and step_id=$3", r.TenantID, r.ID, req.Step.ID))
		if priorErr == nil {
			_, canonical, err := agentrun.CanonicalStepResult(req.ResultJSON, req.Step.Kind)
			if err != nil {
				return err
			}
			accepted := acceptedStep(r, prior)
			if accepted.Identity != req.Step || prior.CommitHash != req.CommitHash || req.CommitHash != agentrun.CommitHash(req.Step, canonical) ||
				!bytes.Equal(prior.Output, canonical) {
				return agentrun.ErrStepConflict
			}
			response = commitResponse(r, a, accepted, false)
			return nil
		}
		if !errors.Is(priorErr, pgx.ErrNoRows) {
			return priorErr
		}
		if err := agentrun.CheckStep(r, a, req.Step); err != nil {
			return err
		}
		if a.ActiveCallID != nil {
			return agentrun.ErrCallConflict
		}
		profile, err := s.ledgerProfile(ctx, tx, r, true)
		if err != nil {
			return err
		}
		checkpoint, err := loadCheckpoint(ctx, tx, r, a)
		if err != nil {
			return err
		}
		decision, err := agentrun.DecideCommit(profile, checkpoint.Snapshot, checkpoint.Steps, req)
		if err != nil {
			return err
		}
		if requireProposal && (!decision.CloseAttempt || decision.Proposal == nil || decision.Proposal.Decision != "proposal") {
			return agentrun.ErrInvalidTransition
		}
		if err := validateCommitObservation(ctx, tx, req, decision.Result); err != nil {
			return err
		}
		// Any locks acquired above can outlive the lease. Re-read database time
		// immediately before applying the transition and its irreversible writes.
		now, err = databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, req.Lease.WorkerID, req.Lease.SessionID, r.TenantID, r.ProfileID, now, true); err != nil {
			return err
		}
		if err := agentrun.ApplyCommit(&r, &a, req, decision, uuid.NewString(), now); err != nil {
			return err
		}
		ref := agentrun.StepReference(r.ID, req.Step.Sequence)
		_, err = tx.Exec(ctx, `insert into run_steps(tenant_id,run_id,attempt_no,sequence,step_id,kind,input_hash,profile_hash,snapshot_hash,
			commit_hash,output_ref,output,output_bytes,cursor_version,created_at) values($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
			r.TenantID, r.ID, req.Lease.AttemptNo, req.Step.Sequence, req.Step.ID, req.Step.Kind, req.Step.InputHash, req.Step.ProfileHash,
			req.Step.SnapshotHash, req.CommitHash, ref, []byte(decision.CanonicalJSON), len(decision.CanonicalJSON), r.CursorVersion, now)
		if err != nil {
			return err
		}
		if decision.CloseAttempt {
			if err := saveProposalResult(ctx, tx, r, decision, ref, now); err != nil {
				return err
			}
			outcome := "succeeded"
			if r.State == agentrun.AwaitingApproval {
				outcome = "yielded_approval"
			}
			if err := closeAttemptRecords(ctx, tx, &r, &a, resources, outcome, now); err != nil {
				return err
			}
		}
		if err := appendEvent(ctx, tx, &r, &a, "step_committed", now); err != nil {
			return err
		}
		if err := saveRun(ctx, tx, &r, &a); err != nil {
			return err
		}
		accepted := agentrun.AcceptedStep{Identity: req.Step, CommitHash: req.CommitHash, ResultJSON: decision.CanonicalJSON, ResultRef: ref}
		response = commitResponse(r, a, accepted, decision.CloseAttempt)
		return nil
	})
	return response, err
}

func validateCommitObservation(ctx context.Context, tx pgx.Tx, req agentrun.CommitStepRequest, result agentrun.StepResult) error {
	if req.Step.Kind == "read_ticket" || req.Step.Kind == "submit_proposal" {
		return nil
	}
	call, err := readCall(ctx, tx, req.Lease.TenantID, req.Lease.RunID, result.PhysicalCallID)
	if err != nil {
		return err
	}
	if call.Lease != req.Lease || call.StepID != req.Step.ID || call.StepKind != req.Step.Kind || call.ProfileHash != req.Step.ProfileHash ||
		call.ObservationHash == nil || call.TransportOutcome == nil || *call.TransportOutcome != "response" || call.BusinessOutcome == nil ||
		call.Reservation.ToolInvocationID != result.ToolInvocationID {
		return agentrun.ErrCallConflict
	}
	wantOutcome := "accepted"
	if result.CorrectionRequired {
		wantOutcome = "rejected"
	}
	if *call.BusinessOutcome != wantOutcome {
		return agentrun.ErrCallConflict
	}
	sequence := agentrun.ToolSequence(req.Step.Kind)
	if len(sequence) == 0 {
		if call.Reservation.Subcall != agentrun.SubcallChat {
			return agentrun.ErrCallConflict
		}
		return nil
	}
	tool, err := readTool(ctx, tx, result.ToolInvocationID)
	if err != nil {
		return err
	}
	if !toolMatches(tool, agentrun.BeginToolRequest{Lease: req.Lease, Step: req.Step, ToolInvocationID: result.ToolInvocationID}) {
		return agentrun.ErrCallConflict
	}
	rows, err := tx.Query(ctx, `select physical_call_id,subcall,transport_outcome,business_outcome,observation_hash from physical_calls
		where tenant_id=$1 and run_id=$2 and tool_invocation_id=$3 order by ordinal`, req.Lease.TenantID, req.Lease.RunID, result.ToolInvocationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var id string
		var subcall agentrun.Subcall
		var transport, business, observed *string
		if err := rows.Scan(&id, &subcall, &transport, &business, &observed); err != nil {
			return err
		}
		if i >= len(sequence) || subcall != sequence[i] || transport == nil || *transport != "response" || business == nil || *business != "accepted" || observed == nil {
			return agentrun.ErrCallConflict
		}
		if i == len(sequence)-1 && id != result.PhysicalCallID {
			return agentrun.ErrCallConflict
		}
		i++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if i != len(sequence) {
		return agentrun.ErrCallConflict
	}
	return nil
}

func saveProposalResult(ctx context.Context, tx pgx.Tx, r agentrun.Run, decision agentrun.CommitDecision, stepRef string, now time.Time) error {
	kind, ref := "no_action", stepRef
	if r.State == agentrun.AwaitingApproval {
		kind, ref = "proposal", *r.ProposalRef
		proposal, err := json.Marshal(decision.Proposal)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `insert into run_approvals(tenant_id,run_id,status,proposal_hash,proposal_ref,snapshot_id,snapshot_hash,
			version_vector,permission_expires_at,created_at) values($1,$2,'pending',$3,$4,$5,$6,$7,$8,$9)`,
			r.TenantID, r.ID, agentrun.Fingerprint("jobforge.run.proposal.v1", string(proposal)), ref, r.SnapshotID, r.SnapshotHash,
			[]byte(r.VersionVector), r.PermissionExpiresAt, now)
		if err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, "update runs set result_kind=$3,result_ref=$4 where tenant_id=$1 and run_id=$2", r.TenantID, r.ID, kind, ref)
	return err
}

func acceptedStep(r agentrun.Run, step agentrun.Step) agentrun.AcceptedStep {
	return agentrun.AcceptedStep{Identity: agentrun.StepIdentity{ID: step.ID, Sequence: step.Sequence, Kind: step.Kind, CursorVersion: step.Sequence - 1,
		InputHash: step.InputHash, ProfileID: r.ProfileID, ProfileHash: step.ProfileHash, SnapshotID: r.SnapshotID, SnapshotHash: step.SnapshotHash},
		CommitHash: step.CommitHash, ResultJSON: step.Output, ResultRef: step.OutputRef}
}

func commitResponse(r agentrun.Run, a agentrun.Authority, accepted agentrun.AcceptedStep, closed bool) agentrun.CommitStepResponse {
	response := agentrun.CommitStepResponse{AcceptedStep: accepted, CursorVersion: r.CursorVersion, State: r.State, AttemptClosed: closed}
	if !closed {
		response.NextStep = &agentrun.StepIdentity{ID: a.NextStepID, Sequence: r.CursorVersion + 1, Kind: a.NextStepKind, CursorVersion: r.CursorVersion,
			InputHash: a.NextInputHash, ProfileID: r.ProfileID, ProfileHash: r.ProfileHash, SnapshotID: r.SnapshotID, SnapshotHash: r.SnapshotHash}
	}
	return response
}

// GetAcceptedCommit authenticates the original attempt even after session/lease
// expiry. Its read-only transaction cannot close calls, change a cursor or renew.
func (s *Store) GetAcceptedCommit(ctx context.Context, principal string, lease agentrun.Lease, stepID string) (agentrun.AcceptedCommitResponse, error) {
	var response agentrun.AcceptedCommitResponse
	if validateLedgerLease(lease) != nil || !agentrun.ValidUUID(stepID) {
		return response, agentrun.ErrInvalidArgument
	}
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		r, _, err := readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2", lease.TenantID, lease.RunID))
		if err != nil {
			return err
		}
		now, err := databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.checkSession(ctx, tx, principal, lease.WorkerID, lease.SessionID, r.TenantID, "", now, false); err != nil {
			return err
		}
		var worker, session string
		var token int64
		var outcome *string
		err = tx.QueryRow(ctx, `select worker_id,session_id,fencing_token,outcome from run_attempts where tenant_id=$1 and run_id=$2 and attempt_no=$3`,
			lease.TenantID, lease.RunID, lease.AttemptNo).Scan(&worker, &session, &token, &outcome)
		if err != nil {
			return err
		}
		if worker != lease.WorkerID || session != lease.SessionID || token != lease.FencingToken {
			return agentrun.ErrStaleLease
		}
		response.State = r.State
		if outcome != nil {
			response.AttemptOutcome = *outcome
		}
		step, err := readStep(tx.QueryRow(ctx, "select "+stepColumns+" from run_steps where tenant_id=$1 and run_id=$2 and step_id=$3 and attempt_no=$4", r.TenantID, r.ID, stepID, lease.AttemptNo))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		accepted := acceptedStep(r, step)
		response.Found, response.AcceptedStep = true, &accepted
		return nil
	})
	return response, err
}
