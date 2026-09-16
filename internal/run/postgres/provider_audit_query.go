package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// Calls reads one bounded tenant-local snapshot. Missing reports never mean that
// no request was sent; neither this query nor its exposure view can refund holds.
func (s *Store) Calls(ctx context.Context, tenant, runID string) (agentrun.CallsResponse, error) {
	result := agentrun.CallsResponse{RunID: runID, Items: []agentrun.CallView{}}
	if !agentrun.ValidIdentifier(tenant) || !agentrun.ValidUUID(runID) {
		return result, agentrun.ErrInvalidArgument
	}
	err := s.readOnly(ctx, func(tx pgx.Tx) error {
		r, _, err := readRun(tx.QueryRow(ctx, "select "+runColumns+" from runs where tenant_id=$1 and run_id=$2", tenant, runID))
		if err != nil {
			return err
		}
		profile, err := s.ledgerProfile(ctx, tx, r, false)
		if err != nil {
			return err
		}
		accounts, err := loadAccounts(ctx, tx, tenant, r.BusinessRequestID, false)
		if err != nil {
			return err
		}
		result.BatchFrozen = accounts[2].Account.Frozen
		if accounts[2].Account.BatchStopCode != "" {
			code := accounts[2].Account.BatchStopCode
			result.BatchStopCode = &code
		}
		result.CapturedAt, err = databaseTime(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "select "+callColumns+` from physical_calls c
			where c.tenant_id=$1 and c.run_id=$2 order by c.ordinal,c.physical_call_id limit 45`, tenant, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if len(result.Items) == agentrun.MaxCallsPerRun {
				return agentrun.ErrInternal
			}
			var call callRow
			if err := rows.Scan(callTargets(&call)...); err != nil {
				return err
			}
			if err := decodeCall(&call); err != nil {
				return err
			}
			if err := restoreCallBudget(&call, profile); err != nil {
				return err
			}
			if call.Report != nil && call.Report.Verify(reportBinding(call, profile), call.Reservation.PersistedReportHash) != nil {
				return agentrun.ErrInternal
			}
			result.Items = append(result.Items, callView(call, profile))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		body, err := json.Marshal(result)
		if err != nil {
			return agentrun.ErrInternal
		}
		if len(body)+1 > agentrun.MaxCallsResponseBytes {
			return agentrun.ErrCheckpointTooLarge
		}
		return nil
	})
	if err != nil {
		return agentrun.CallsResponse{}, err
	}
	return result, nil
}

func callView(call callRow, profile agentrun.Profile) agentrun.CallView {
	r := call.Reservation
	view := agentrun.CallView{PhysicalCallID: r.PhysicalCallID, StepID: call.StepID, StepKind: call.StepKind,
		AttemptNo: call.Lease.AttemptNo, Ordinal: r.Ordinal, Subcall: r.Subcall, ParameterHash: r.ParameterHash,
		ProfileID: profile.ID, ProfileHash: profile.Hash, PriceHash: r.PriceHash, ReservedAt: r.ReservedAt,
		DispatchExpiresAt: r.DispatchExpiresAt, CallDeadline: r.CallDeadline, ObservedAt: call.ObservedAt,
		ReportRecordedAt: call.ReportRecordedAt, SettledAt: call.SettledAt, Reserved: r.Budget,
		KnownTokens: call.KnownTokens, KnownCostMicroyuan: call.KnownCostMicroyuan,
		UsageKnown: r.UsageKnown, MeasurementAnomaly: r.MeasurementAnomaly,
		ReportConflict: call.ReportConflictHash != nil, AuditStatus: "legacy_not_collected",
		TransportOutcome: call.TransportOutcome, HTTPStatus: call.HTTPStatus, ErrorCode: call.ErrorCode,
		BusinessOutcome: call.BusinessOutcome, ObservedUsage: r.ReportedUsage}
	if r.UsageKnown {
		view.SettledUsage = r.ReportedUsage
	} else {
		view.HeldTokens, view.HeldCostMicroyuan = r.Budget.TotalTokens, r.Budget.CostMicroyuan
	}
	if profile.AuditEnabled() {
		view.AuditStatus = "not_applicable"
		if r.Subcall == agentrun.SubcallChat {
			view.AuditStatus = "missing"
			if call.Report != nil {
				view.AuditStatus = "recorded"
			}
		}
	}
	if call.Report != nil {
		view.ReportHash, view.ObservedUsage, view.ProviderAudit = &r.PersistedReportHash, call.Report.Usage, call.Report.ProviderAudit
		if r.PersistedAuditHash != "" {
			view.AuditHash = &r.PersistedAuditHash
		}
	}
	return view
}
