package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

type chatBarrier struct {
	Call                                                               callRow
	Profile                                                            agentrun.Profile
	Run                                                                agentrun.Run
	Authority                                                          agentrun.Authority
	AttemptWorker, AttemptSession                                      string
	AttemptFence                                                       int64
	AttemptFinished                                                    *time.Time
	AttemptOutcome, AttemptError, RunError                             *string
	StepID, StepKind, StepInput, StepProfile, StepSnapshot, CommitHash *string
	StepSequence, StepCursor                                           *int64
	StepOutput                                                         []byte
	profileJSON                                                        []byte
}

// checkBatchAuditGuard is called with the batch account locked, before creating
// a new Claim/tool/call. It only reads older Runs/attempts: locking those here
// would reverse Run -> account and deadlock original-call confirmation.
func checkBatchAuditGuard(ctx context.Context, tx pgx.Tx, batchID string, current agentrun.Profile) error {
	if !current.AuditEnabled() {
		var audited bool
		if err := tx.QueryRow(ctx, `select exists(select 1 from physical_calls c
			join runs r on r.tenant_id=c.tenant_id and r.run_id=c.run_id
			join business_requests b on b.tenant_id=r.tenant_id and b.business_request_id=r.business_request_id
			where b.batch_account_id=$1 and c.kind='chat' and c.execution_binding_hash is not null)`, batchID).Scan(&audited); err != nil {
			return err
		}
		if !audited {
			return nil
		}
	}
	rows, err := tx.Query(ctx, "select "+callColumns+`,r.profile_id,r.profile_hash,r.snapshot_id,r.snapshot_hash,
		r.state,r.attempt_no,r.fencing_token,r.next_step_id,r.next_step_kind,r.next_input_hash,r.cursor_version,r.error_code,
		ra.worker_id,ra.session_id,ra.fencing_token,ra.finished_at,ra.outcome,ra.error_code,
		s.step_id,s.sequence,s.cursor_version,s.kind,s.input_hash,s.profile_hash,s.snapshot_hash,s.output,s.commit_hash,p.definition
		from physical_calls c
		join runs r on r.tenant_id=c.tenant_id and r.run_id=c.run_id
		join business_requests b on b.tenant_id=r.tenant_id and b.business_request_id=r.business_request_id
		join run_attempts ra on ra.tenant_id=c.tenant_id and ra.run_id=c.run_id and ra.attempt_no=c.attempt_no
		join agent_profiles p on p.profile_id=r.profile_id
		left join run_steps s on s.tenant_id=c.tenant_id and s.run_id=c.run_id and s.step_id=c.step_id and s.attempt_no=c.attempt_no
		where b.batch_account_id=$1 and c.kind='chat' order by c.reserved_at,c.physical_call_id`, batchID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var barrier chatBarrier
		targets := append(callTargets(&barrier.Call), &barrier.Run.ProfileID, &barrier.Run.ProfileHash,
			&barrier.Run.SnapshotID, &barrier.Run.SnapshotHash, &barrier.Run.State, &barrier.Run.AttemptNo,
			&barrier.Authority.FencingToken, &barrier.Authority.NextStepID, &barrier.Authority.NextStepKind,
			&barrier.Authority.NextInputHash, &barrier.Run.CursorVersion, &barrier.RunError,
			&barrier.AttemptWorker, &barrier.AttemptSession, &barrier.AttemptFence, &barrier.AttemptFinished,
			&barrier.AttemptOutcome, &barrier.AttemptError, &barrier.StepID, &barrier.StepSequence, &barrier.StepCursor,
			&barrier.StepKind, &barrier.StepInput, &barrier.StepProfile, &barrier.StepSnapshot, &barrier.StepOutput,
			&barrier.CommitHash, &barrier.profileJSON)
		if err := rows.Scan(targets...); err != nil {
			return err
		}
		if err := decodeCall(&barrier.Call); err != nil {
			return err
		}
		if json.Unmarshal(barrier.profileJSON, &barrier.Profile) != nil || barrier.Profile.ValidateAuditPolicy() != nil ||
			barrier.Profile.ID != barrier.Run.ProfileID || barrier.Profile.Hash != barrier.Run.ProfileHash {
			return agentrun.ErrInternal
		}
		if !barrier.complete() {
			return agentrun.ErrBudgetExhausted
		}
	}
	return rows.Err()
}

func persistedChatObservation(call callRow, profile agentrun.Profile) bool {
	if !profile.AuditEnabled() || call.Report == nil || call.Report.ProviderAudit == nil ||
		call.Report.ProviderAudit.IdentityState != agentrun.ProviderIdentityCompatible ||
		call.Report.ProviderAudit.ModeState != agentrun.ProviderModeNonthinking || call.ReportRecordedAt == nil ||
		call.ObservedAt == nil || call.ObservationHash == nil || call.HTTPStatus == nil || call.ErrorCode == nil ||
		call.TransportOutcome == nil || call.BusinessOutcome == nil || call.Reservation.ReportedUsage == nil ||
		call.Report.Usage == nil || *call.Reservation.ReportedUsage != *call.Report.Usage {
		return false
	}
	observation := agentrun.ObserveCallRequest{PhysicalCallID: call.Reservation.PhysicalCallID,
		TransportOutcome: *call.TransportOutcome, HTTPStatus: *call.HTTPStatus, ErrorCode: *call.ErrorCode,
		BusinessOutcome: *call.BusinessOutcome, UsageKnown: call.Reservation.UsageKnown,
		Usage: call.Report.Usage, AuditHash: call.Reservation.PersistedAuditHash}
	hash, err := checkAuditObservation(call, profile, observation)
	return err == nil && hash == *call.ObservationHash
}

func (b chatBarrier) complete() bool {
	c := b.Call
	if !persistedChatObservation(c, b.Profile) || b.AttemptWorker != c.Lease.WorkerID ||
		b.AttemptSession != c.Lease.SessionID || b.AttemptFence != c.Lease.FencingToken {
		return false
	}
	if b.StepID != nil {
		return b.committedStep()
	}
	if (c.StepKind != "protocol_correction" && (b.Profile.Strategy != agentrun.SupportAgentStrategy || c.StepKind != "model_decision")) || *c.BusinessOutcome != "rejected" || *c.ErrorCode != "MODEL_PROTOCOL_ERROR" ||
		b.Run.State != agentrun.Failed || b.RunError == nil || *b.RunError != "MODEL_PROTOCOL_ERROR" ||
		b.Run.AttemptNo != c.Lease.AttemptNo || b.Authority.FencingToken != c.Lease.FencingToken ||
		b.AttemptFinished == nil || b.AttemptOutcome == nil || *b.AttemptOutcome != "failed_terminal" ||
		b.AttemptError == nil || *b.AttemptError != "MODEL_PROTOCOL_ERROR" {
		return false
	}
	step := agentrun.StepIdentity{ID: b.Authority.NextStepID, Kind: b.Authority.NextStepKind,
		Sequence: b.Run.CursorVersion + 1, CursorVersion: b.Run.CursorVersion, InputHash: b.Authority.NextInputHash,
		ProfileID: b.Run.ProfileID, ProfileHash: b.Run.ProfileHash, SnapshotID: b.Run.SnapshotID, SnapshotHash: b.Run.SnapshotHash}
	hash, err := agentrun.ExecutionBindingHash(c.Lease, step)
	return err == nil && step.ID == c.StepID && step.Kind == c.StepKind && hash == c.Reservation.ExecutionBindingHash
}

func (b chatBarrier) committedStep() bool {
	c := b.Call
	if b.StepSequence == nil || b.StepCursor == nil || b.StepKind == nil || b.StepInput == nil ||
		b.StepProfile == nil || b.StepSnapshot == nil || b.CommitHash == nil || *b.StepID != c.StepID ||
		*b.StepKind != c.StepKind || *b.StepCursor != *b.StepSequence {
		return false
	}
	step := agentrun.StepIdentity{ID: *b.StepID, Sequence: *b.StepSequence, CursorVersion: *b.StepSequence - 1,
		Kind: *b.StepKind, InputHash: *b.StepInput, ProfileID: b.Run.ProfileID, ProfileHash: *b.StepProfile,
		SnapshotID: b.Run.SnapshotID, SnapshotHash: *b.StepSnapshot}
	bindingHash, err := agentrun.ExecutionBindingHash(c.Lease, step)
	if err != nil || bindingHash != c.Reservation.ExecutionBindingHash || step.ProfileHash != b.Run.ProfileHash || step.SnapshotHash != b.Run.SnapshotHash {
		return false
	}
	result, canonical, err := agentrun.CanonicalRegisteredStepResult(b.StepOutput, c.StepKind)
	if err != nil || agentrun.CommitHash(step, canonical) != *b.CommitHash || result.PhysicalCallID != c.Reservation.PhysicalCallID {
		return false
	}
	if *c.BusinessOutcome == "accepted" {
		return !result.CorrectionRequired
	}
	return (c.StepKind == "model_proposal" || b.Profile.Strategy == agentrun.SupportAgentStrategy && c.StepKind == "model_decision") && *c.BusinessOutcome == "rejected" && *c.ErrorCode == "MODEL_PROTOCOL_ERROR" &&
		result.CorrectionRequired && result.Proposal == nil
}
