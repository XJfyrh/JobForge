package postgres

import (
	"bytes"
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

func reportBinding(call callRow, profile agentrun.Profile) agentrun.ReportBinding {
	return agentrun.ReportBinding{ExecutionBindingHash: call.Reservation.ExecutionBindingHash,
		PhysicalCallID: call.Reservation.PhysicalCallID, ParameterHash: call.Reservation.ParameterHash,
		Subcall: call.Reservation.Subcall, ExpectedResponseModel: profile.ExpectedResponseModel}
}

func freezeReportBatch(accounts *[3]budgetRow, reason agentrun.BatchStopCode) {
	accounts[2].Account.Frozen = true
	if accounts[2].Account.BatchStopCode == "" {
		accounts[2].Account.BatchStopCode = reason
	}
}

// settleCallReport runs after Run -> accounts -> original call locks and original
// session authentication. Conflicts are successful batch-stop transactions, not
// errors that would roll back the freeze; no second report reaches disposition.
func settleCallReport(ctx context.Context, tx pgx.Tx, call *callRow, profile agentrun.Profile,
	accounts *[3]budgetRow, req agentrun.SettleUsageRequest, now time.Time) (agentrun.SettleUsageResponse, error) {
	var result agentrun.SettleUsageResponse
	if !call.Reservation.ReservedAt.Add(agentrun.UsageSettlementWindow).After(now) {
		return result, agentrun.ErrCallSettlementExpired
	}
	report := agentrun.CallReport{Usage: req.Usage, ProviderAudit: req.ProviderAudit}
	if err := report.Verify(reportBinding(*call, profile), req.ReportHash); err != nil {
		return result, err
	}
	body, err := agentrun.CallReportJSON(report)
	if err != nil {
		return result, err
	}
	if call.Report != nil {
		if call.Reservation.PersistedReportHash == req.ReportHash {
			original, err := agentrun.CallReportJSON(*call.Report)
			if err != nil || !bytes.Equal(body, original) {
				return result, agentrun.ErrCallConflict
			}
			return result, nil
		}
		// Preserve both the first report and its financial facts, including when
		// this different second report alleges an over-reservation token count.
		if _, err := tx.Exec(ctx, `update physical_calls set report_conflict_hash=coalesce(report_conflict_hash,$4)
			where tenant_id=$1 and run_id=$2 and physical_call_id=$3`, call.Lease.TenantID, call.Lease.RunID,
			call.Reservation.PhysicalCallID, req.ReportHash); err != nil {
			return result, err
		}
		freezeReportBatch(accounts, agentrun.BatchStopReportConflict)
		if err := saveAccount(ctx, tx, accounts[2]); err != nil {
			return result, err
		}
		result.ReportConflict = true
		return result, nil
	}
	if call.Reservation.PersistedReportHash != "" || call.UsageHash != nil {
		return result, agentrun.ErrInternal
	}
	disposition, err := agentrun.FirstReportDisposition(reportBinding(*call, profile), report, call.Reservation.Budget, profile.Pricing)
	if err != nil {
		return result, err
	}
	if disposition.MeasurementAnomaly {
		for i := range accounts {
			accounts[i].Account.Frozen = true
		}
	} else if disposition.UsageKnown {
		for i := range accounts {
			settled, err := agentrun.SettleAccount(accounts[i].Account, call.Reservation.Budget.TotalTokens,
				call.Reservation.Budget.CostMicroyuan, disposition.KnownTokens, disposition.KnownCostMicroyuan)
			if err != nil {
				return result, err
			}
			accounts[i].Account = settled
		}
	}
	if disposition.BatchStopCode != "" {
		freezeReportBatch(accounts, disposition.BatchStopCode)
	}
	if err := saveLedgerAccounts(ctx, tx, *accounts); err != nil {
		return result, err
	}
	var usageHash *string
	var usageBody []byte
	var knownTokens, knownCost *int64
	var settledAt *time.Time
	status := "unknown"
	// The old usage column remains known metering or first-report anomaly only.
	// Incompatible, non-anomalous observations exist solely in call_report.
	if disposition.UsageKnown || disposition.MeasurementAnomaly {
		usageBody, err = agentrun.UsageJSON(*report.Usage)
		if err != nil {
			return result, err
		}
		usageHash = &report.Usage.UsageHash
	}
	if disposition.UsageKnown {
		status, settledAt = "known", &now
		knownTokens, knownCost = &disposition.KnownTokens, &disposition.KnownCostMicroyuan
	}
	_, err = tx.Exec(ctx, `update physical_calls set report_hash=$4,call_report=$5,report_recorded_at=$6,
		status=$7,measurement_anomaly=$8,usage_hash=$9,usage=$10,known_tokens=$11,known_cost_microyuan=$12,settled_at=$13
		where tenant_id=$1 and run_id=$2 and physical_call_id=$3`, call.Lease.TenantID, call.Lease.RunID,
		call.Reservation.PhysicalCallID, req.ReportHash, body, now, status, disposition.MeasurementAnomaly,
		usageHash, usageBody, knownTokens, knownCost, settledAt)
	if err != nil {
		return result, err
	}
	call.Report, call.ReportRecordedAt, call.Status, call.UsageHash = &report, &now, status, usageHash
	call.Reservation.PersistedReportHash = req.ReportHash
	if report.ProviderAudit != nil {
		call.Reservation.PersistedAuditHash = report.ProviderAudit.AuditHash
	}
	call.Reservation.UsageKnown, call.Reservation.MeasurementAnomaly = disposition.UsageKnown, disposition.MeasurementAnomaly
	if usageHash != nil {
		call.Reservation.ReportedUsage = report.Usage
	}
	result.NewlySettled = disposition.UsageKnown
	return result, nil
}

func accountsUnfrozen(accounts [3]budgetRow) error {
	for _, account := range accounts {
		if account.Account.Frozen {
			return agentrun.ErrBudgetExhausted
		}
	}
	return nil
}

func checkAuditObservation(call callRow, profile agentrun.Profile, req agentrun.ObserveCallRequest) (string, error) {
	if !profile.AuditEnabled() {
		if req.AuditHash != "" {
			return "", agentrun.ErrInvalidArgument
		}
		return req.Hash(), nil
	}
	if call.Reservation.Subcall == agentrun.SubcallChat {
		if req.AuditHash == "" || !req.UsageKnown || req.Usage == nil || req.TransportOutcome != "response" || req.HTTPStatus != 200 {
			return "", agentrun.ErrCallConflict
		}
	} else if req.AuditHash != "" {
		return "", agentrun.ErrInvalidArgument
	}
	if req.Usage != nil || call.Reservation.Subcall == agentrun.SubcallChat {
		if req.Usage == nil || !req.UsageKnown || call.Report == nil || !call.Reservation.UsageKnown || call.Reservation.MeasurementAnomaly ||
			call.ReportConflictHash != nil || call.Report.Usage == nil || *call.Report.Usage != *req.Usage ||
			call.Reservation.PersistedAuditHash != req.AuditHash || call.Report.Verify(reportBinding(call, profile), call.Reservation.PersistedReportHash) != nil {
			return "", agentrun.ErrCallConflict
		}
	} else if call.Report != nil {
		return "", agentrun.ErrCallConflict
	}
	return agentrun.ObservationHashV2(req, req.AuditHash)
}
