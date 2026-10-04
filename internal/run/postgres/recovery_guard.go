package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

type recoveryAttempt struct {
	Lease    agentrun.Lease
	Finished *time.Time
	Outcome  *string
	Code     *string
	Ordinal  *int64
	StepJSON []byte
	Step     agentrun.StepIdentity
}

type recoveryHistory map[string][]recoveryAttempt

// The batch lock serializes new permissions with S3 closure and confirmation.
// Reading other Runs here must never acquire their Run or attempt row locks.
func readRecoveryHistory(ctx context.Context, tx pgx.Tx, batchID string) (recoveryHistory, error) {
	rows, err := tx.Query(ctx, `select ra.tenant_id,ra.run_id,ra.worker_id,ra.session_id,ra.attempt_no,
		ra.fencing_token,ra.finished_at,ra.outcome,ra.error_code,ra.recovery_step,ra.recovery_ordinal
		from run_attempts ra join runs r on r.tenant_id=ra.tenant_id and r.run_id=ra.run_id
		join business_requests b on b.tenant_id=r.tenant_id and b.business_request_id=r.business_request_id
		where b.batch_account_id=$1 and r.recovery_count>0 order by ra.run_id,ra.attempt_no`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	history := make(recoveryHistory)
	for rows.Next() {
		var attempt recoveryAttempt
		l := &attempt.Lease
		if err := rows.Scan(&l.TenantID, &l.RunID, &l.WorkerID, &l.SessionID, &l.AttemptNo, &l.FencingToken,
			&attempt.Finished, &attempt.Outcome, &attempt.Code, &attempt.StepJSON, &attempt.Ordinal); err != nil {
			return nil, err
		}
		history[l.RunID] = append(history[l.RunID], attempt)
	}
	return history, rows.Err()
}

func (h recoveryHistory) proofs(r agentrun.Run) ([]recoveryAttempt, bool) {
	if r.RecoveryCount < 1 || r.RecoveryCount > agentrun.MaxRecoveries {
		return nil, false
	}
	var proofs []recoveryAttempt
	for _, attempt := range h[r.ID] {
		retry := attempt.Outcome != nil && (*attempt.Outcome == "failed_retry" || *attempt.Outcome == "lease_expired_retry")
		if !retry {
			if attempt.Ordinal != nil || len(attempt.StepJSON) != 0 {
				return nil, false
			}
			continue
		}
		step, err := agentrun.DecodeRecoveryStep(attempt.StepJSON)
		if err != nil || attempt.Ordinal == nil || *attempt.Ordinal != int64(len(proofs)+1) ||
			attempt.Finished == nil || attempt.Code == nil || !agentrun.RecoveryClosure(*attempt.Outcome, *attempt.Code) ||
			attempt.Lease.RunID != r.ID || attempt.Lease.TenantID != r.TenantID || attempt.Lease.AttemptNo > r.AttemptNo ||
			step.ProfileID != r.ProfileID || step.ProfileHash != r.ProfileHash || step.SnapshotID != r.SnapshotID || step.SnapshotHash != r.SnapshotHash {
			return nil, false
		}
		if _, err := agentrun.ExecutionBindingHash(attempt.Lease, step); err != nil {
			return nil, false
		}
		if len(proofs) > 0 && attempt.Lease.FencingToken <= proofs[len(proofs)-1].Lease.FencingToken {
			return nil, false
		}
		attempt.Step = step
		proofs = append(proofs, attempt)
	}
	return proofs, int64(len(proofs)) == r.RecoveryCount
}

func pendingRecovery(r agentrun.Run, a agentrun.Authority, proofs []recoveryAttempt, now time.Time) bool {
	if len(proofs) == 0 || r.CancelRequestedAt != nil || !r.RunDeadline.After(now) {
		return false
	}
	last := proofs[len(proofs)-1]
	if agentrun.CheckStep(r, a, last.Step) != nil || last.Ordinal == nil || *last.Ordinal != r.RecoveryCount {
		return false
	}
	if r.State == agentrun.Ready {
		return r.AttemptNo == last.Lease.AttemptNo && a.FencingToken == last.Lease.FencingToken && a.WorkerID == "" && a.SessionID == ""
	}
	return r.State == agentrun.Running && r.AttemptNo == last.Lease.AttemptNo+1 && a.FencingToken == last.Lease.FencingToken+1 &&
		a.WorkerID != "" && a.SessionID != "" && r.LeaseUntil != nil && r.LeaseUntil.After(now) &&
		r.AttemptDeadline != nil && r.AttemptDeadline.After(now)
}

func acceptedRecoveryChat(b chatBarrier) bool {
	c := b.Call
	if !persistedChatObservation(c, b.Profile) || !c.Reservation.UsageKnown || c.Reservation.MeasurementAnomaly || c.ReportConflictHash != nil ||
		c.SettledAt == nil || c.Report == nil || c.Report.Verify(reportBinding(c, b.Profile), c.Reservation.PersistedReportHash) != nil ||
		c.HTTPStatus == nil || *c.HTTPStatus != 200 || *c.TransportOutcome != "response" || *c.BusinessOutcome != "accepted" || *c.ErrorCode != "" {
		return false
	}
	disposition, err := agentrun.FirstReportDisposition(reportBinding(c, b.Profile), *c.Report, c.Reservation.Budget, b.Profile.Pricing)
	return err == nil && disposition.UsageKnown && !disposition.MeasurementAnomaly && disposition.BatchStopCode == "" &&
		disposition.KnownTokens == c.KnownTokens && disposition.KnownCostMicroyuan == c.KnownCostMicroyuan
}

// recoveredChat grants either a same-Run/same-step pending exemption or validates
// a real successor commit. The old call never becomes that successor's checkpoint.
func recoveredChat(b chatBarrier, calls map[string]chatBarrier, history recoveryHistory,
	target agentrun.Run, authority agentrun.Authority, now time.Time) bool {
	if !b.Profile.ConfirmedStepRecovery() || !acceptedRecoveryChat(b) ||
		b.AttemptWorker != b.Call.Lease.WorkerID || b.AttemptSession != b.Call.Lease.SessionID || b.AttemptFence != b.Call.Lease.FencingToken ||
		b.AttemptStarted.After(b.Call.Reservation.ReservedAt) {
		return false
	}
	proofs, ok := history.proofs(b.Run)
	if !ok {
		return false
	}
	var original *recoveryAttempt
	for i := range proofs {
		if proofs[i].Lease == b.Call.Lease {
			original = &proofs[i]
			break
		}
	}
	c := b.Call
	if original == nil || original.Finished.Before(c.Reservation.ReservedAt) || original.Finished.Before(*c.ReportRecordedAt) ||
		original.Finished.Before(*c.ObservedAt) || original.Finished.Before(*c.SettledAt) || original.Step.ID != c.StepID || original.Step.Kind != c.StepKind {
		return false
	}
	binding, err := agentrun.ExecutionBindingHash(c.Lease, original.Step)
	if err != nil || binding != c.Reservation.ExecutionBindingHash {
		return false
	}
	if b.StepID == nil {
		return target.ID == c.Lease.RunID && target.TenantID == c.Lease.TenantID &&
			agentrun.PendingStep(target, authority) == original.Step && pendingRecovery(target, authority, proofs, now)
	}
	// The actual new step must reference a different, fully confirmed call from
	// a strictly newer attempt. Its immutable logical identity equals the proof.
	result, canonical, err := agentrun.CanonicalRegisteredStepResult(b.StepOutput, original.Step.Kind)
	if err != nil || result.PhysicalCallID == c.Reservation.PhysicalCallID {
		return false
	}
	successor, found := calls[result.PhysicalCallID]
	step, valid := b.committedIdentity()
	successorStep, successorValid := successor.committedIdentity()
	return found && valid && successorValid && step == original.Step && successorStep == original.Step &&
		successor.Call.StepID == original.Step.ID && successor.Call.StepKind == original.Step.Kind &&
		b.StepAttempt != nil && *b.StepAttempt == successor.Call.Lease.AttemptNo && b.CommitHash != nil &&
		agentrun.CommitHash(original.Step, canonical) == *b.CommitHash && successor.Call.Lease.RunID == c.Lease.RunID &&
		successor.Call.Lease.TenantID == c.Lease.TenantID && successor.Call.Lease.AttemptNo > c.Lease.AttemptNo &&
		successor.Call.Lease.FencingToken > c.Lease.FencingToken && successor.StepAttempt != nil &&
		*successor.StepAttempt == successor.Call.Lease.AttemptNo && successor.AttemptStarted.Compare(*original.Finished) >= 0 &&
		acceptedRecoveryChat(successor) && successor.committedStep()
}
