package integration

import (
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func auditSettle(t *testing.T, h *runHarness, report agentrun.SettleUsageRequest) agentrun.SettleUsageResponse {
	t.Helper()
	response, err := h.Store.SettleUsage(h.Ctx, h.Principal, report)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func auditCallView(t *testing.T, h *runHarness, request agentrun.ReserveCallRequest) agentrun.CallView {
	t.Helper()
	response, err := h.Store.Calls(h.Ctx, request.Lease.TenantID, request.Lease.RunID)
	if err != nil || response.CapturedAt.IsZero() {
		t.Fatalf("calls snapshot: %+v %v", response, err)
	}
	for _, call := range response.Items {
		if call.PhysicalCallID == request.PhysicalCallID {
			return call
		}
	}
	t.Fatal("reserved call absent from snapshot")
	return agentrun.CallView{}
}

func TestRunProviderAuditStorageKnownReplayAndBusinessSeparation(t *testing.T) {
	h := setupAuditHarness(t)
	claimed := auditAtModelStep(t, h, "tenant-a", "audit-known")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	reservation := ledgerReserve(t, h, request)
	binding, err := agentrun.ExecutionBindingHash(request.Lease, request.Step)
	if err != nil || reservation.ExecutionBindingHash != binding {
		t.Fatalf("original binding: %s %v", reservation.ExecutionBindingHash, err)
	}
	report := auditReportRequest(t, h, request, 10, 5)
	observation := agentrun.ObserveCallRequest{Lease: request.Lease, Step: request.Step, PhysicalCallID: request.PhysicalCallID,
		TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: "rejected", ErrorCode: "MODEL_PROTOCOL_ERROR",
		UsageKnown: true, Usage: report.Usage, AuditHash: report.ProviderAudit.AuditHash}
	if _, err := h.Store.ObserveCall(h.Ctx, h.Principal, observation); !errors.Is(err, agentrun.ErrCallConflict) {
		t.Fatalf("observation bypassed missing report: %v", err)
	}
	before := ledgerView(t, h, claimed.Lease)
	settled := auditSettle(t, h, report)
	if !settled.NewlySettled || !settled.Reservation.UsageKnown || settled.BatchFrozen || settled.ReportConflict ||
		settled.PersistedReportHash != report.ReportHash || settled.PersistedAuditHash != report.ProviderAudit.AuditHash {
		t.Fatalf("first settlement: %+v", settled)
	}
	after := ledgerView(t, h, claimed.Lease)
	cost, err := agentrun.UsageCost(h.Profile.Pricing, *report.Usage)
	if err != nil {
		t.Fatal(err)
	}
	for i, account := range ledgerAccounts(after) {
		old := ledgerAccounts(before)[i]
		if account.Used.Chat != old.Used.Chat || account.KnownTokens-old.KnownTokens != 15 ||
			account.KnownCostMicroyuan-old.KnownCostMicroyuan != cost || old.HeldTokens-account.HeldTokens != reservation.Budget.TotalTokens ||
			old.HeldCostMicroyuan-account.HeldCostMicroyuan != reservation.Budget.CostMicroyuan {
			t.Fatalf("scope %d first report accounting mismatch", i)
		}
	}
	if after.CursorVersion != before.CursorVersion || !after.UpdatedAt.Equal(before.UpdatedAt) || after.State != before.State {
		t.Fatal("report advanced execution")
	}
	if replay := auditSettle(t, h, report); replay.NewlySettled || replay.PersistedReportHash != report.ReportHash {
		t.Fatalf("same report was not an idempotent confirmation: %+v", replay)
	}
	if !reflect.DeepEqual(after.Budget, ledgerView(t, h, claimed.Lease).Budget) {
		t.Fatal("replay changed any account")
	}
	wrong := observation
	wrong.AuditHash = agentrun.Fingerprint("wrong-audit")
	if _, err := h.Store.ObserveCall(h.Ctx, h.Principal, wrong); !errors.Is(err, agentrun.ErrCallConflict) {
		t.Fatalf("wrong audit joined settled report: %v", err)
	}
	auditObserve(t, h, request, report, "rejected")
	view := auditCallView(t, h, request)
	if !view.UsageKnown || view.KnownTokens != 15 || view.KnownCostMicroyuan != cost || view.HeldTokens != 0 ||
		view.ObservedUsage == nil || view.SettledUsage == nil || *view.ObservedUsage != *report.Usage ||
		view.BusinessOutcome == nil || *view.BusinessOutcome != "rejected" || view.ObservedAt == nil ||
		view.ReportRecordedAt == nil || view.SettledAt == nil || view.AuditStatus != "recorded" || view.ReportConflict {
		t.Fatalf("typed known/rejected view: %+v", view)
	}
	if _, err := h.Store.Calls(h.Ctx, "tenant-b", request.Lease.RunID); !errors.Is(err, agentrun.ErrNotFound) {
		t.Fatalf("cross-tenant audit disclosed: %v", err)
	}
	auditCommitChat(t, h, &claimed, request, true)
	if currentRunStep(claimed).Kind != "protocol_correction" {
		t.Fatal("business-only rejection lost the one correction path")
	}
	ledgerReserve(t, h, ledgerRequest(h, claimed, agentrun.SubcallChat, ""))
}

func TestRunProviderAuditStorageFirstReportDisposition(t *testing.T) {
	for _, name := range []string{"incompatible", "incompatible_anomaly", "positive_reasoning", "unavailable"} {
		t.Run(name, func(t *testing.T) {
			h := setupAuditHarness(t)
			claimed := auditAtModelStep(t, h, "tenant-a", name)
			request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reservation := ledgerReserve(t, h, request)
			report := auditReportRequest(t, h, request, 10, 5)
			stop, known, anomaly := agentrun.BatchStopProviderIdentityInvalid, false, false
			switch name {
			case "incompatible", "incompatible_anomaly":
				report.ProviderAudit.IdentityState = agentrun.ProviderIdentityIncompatible
				report.ProviderAudit.ResponseModel = auditValue("different-model")
				if name == "incompatible_anomaly" {
					report.Usage.InputTokens = reservation.Budget.InputTokens + 1
					stop, anomaly = agentrun.BatchStopMeasurementAnomaly, true
				}
			case "positive_reasoning":
				report.ProviderAudit.ReasoningState = agentrun.ReasoningObserved
				report.ProviderAudit.ReasoningTokens = auditValue(int64(2))
				report.ProviderAudit.ModeState = agentrun.ProviderModeUnexpected
				stop, known = agentrun.BatchStopProviderModeInvalid, true
			case "unavailable":
				report.Usage = nil
				report.ProviderAudit = &agentrun.ProviderAudit{SchemaVersion: 1, Provider: "deepseek",
					IdentityState: agentrun.ProviderIdentityUnavailable, UsageEvidence: agentrun.UsageEvidenceUnavailable,
					ReasoningState: agentrun.ReasoningUnavailable, ModeState: agentrun.ProviderModeUnavailable}
				stop = agentrun.BatchStopChatUsageUnknown
			}
			report = auditBindReport(t, h, request, agentrun.CallReport{Usage: report.Usage, ProviderAudit: report.ProviderAudit})
			before := ledgerView(t, h, claimed.Lease)
			response := auditSettle(t, h, report)
			if !response.BatchFrozen || response.BatchStopCode != stop || response.Reservation.UsageKnown != known ||
				response.Reservation.MeasurementAnomaly != anomaly || response.NewlySettled != known {
				t.Fatalf("first disposition: %+v", response)
			}
			after := ledgerView(t, h, claimed.Lease)
			for i, account := range ledgerAccounts(after) {
				old := ledgerAccounts(before)[i]
				if account.Frozen != (i == 2 || anomaly) {
					t.Fatalf("wrong frozen scope %d", i)
				}
				if !known && (account.Used != old.Used || account.KnownTokens != old.KnownTokens || account.KnownCostMicroyuan != old.KnownCostMicroyuan ||
					account.HeldTokens != old.HeldTokens || account.HeldCostMicroyuan != old.HeldCostMicroyuan) {
					t.Fatal("ineligible report changed financial exposure")
				}
			}
			view := auditCallView(t, h, request)
			if view.UsageKnown != known || (view.SettledUsage != nil) != known || (view.ObservedUsage != nil) != (report.Usage != nil) ||
				view.ProviderAudit == nil || view.ReportHash == nil || *view.ReportHash != report.ReportHash {
				t.Fatalf("observed/settled separation: %+v", view)
			}
			if !known && (view.HeldTokens != reservation.Budget.TotalTokens || view.HeldCostMicroyuan != reservation.Budget.CostMicroyuan) {
				t.Fatal("ineligible first report released its hold")
			}
			if known && view.KnownTokens != 15 {
				t.Fatal("reasoning tokens were double counted")
			}
			if replay := auditSettle(t, h, report); replay.NewlySettled || replay.BatchStopCode != stop {
				t.Fatal("frozen report replay changed disposition")
			}
		})
	}
}

func TestRunProviderAuditStorageConflictNeverReevaluatesSecondUsage(t *testing.T) {
	for _, firstUnknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "known", true: "unknown"}[firstUnknown], func(t *testing.T) {
			h := setupAuditHarness(t)
			claimed := auditAtModelStep(t, h, "tenant-a", "conflict")
			request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			reservation := ledgerReserve(t, h, request)
			first := auditReportRequest(t, h, request, 10, 5)
			if firstUnknown {
				first.ProviderAudit.IdentityState = agentrun.ProviderIdentityIncompatible
				first.ProviderAudit.ResponseModel = auditValue("different-model")
				first = auditBindReport(t, h, request, agentrun.CallReport{Usage: first.Usage, ProviderAudit: first.ProviderAudit})
			}
			auditSettle(t, h, first)
			before := ledgerView(t, h, claimed.Lease)
			second := auditReportRequest(t, h, request, reservation.Budget.InputTokens+10, reservation.Budget.OutputTokens+10)
			conflict := auditSettle(t, h, second)
			if !conflict.ReportConflict || !conflict.BatchFrozen || conflict.NewlySettled || conflict.Reservation.MeasurementAnomaly ||
				conflict.PersistedReportHash != first.ReportHash || conflict.PersistedAuditHash != first.ProviderAudit.AuditHash {
				t.Fatalf("conflict was not batch-only: %+v", conflict)
			}
			wantStop := agentrun.BatchStopReportConflict
			if firstUnknown {
				wantStop = agentrun.BatchStopProviderIdentityInvalid
			}
			if conflict.BatchStopCode != wantStop {
				t.Fatal("batch reason did not retain first write")
			}
			after := ledgerView(t, h, claimed.Lease)
			for i, account := range ledgerAccounts(after) {
				old := ledgerAccounts(before)[i]
				old.Frozen, old.BatchStopCode = account.Frozen, account.BatchStopCode
				if account != old || account.Frozen != (i == 2) {
					t.Fatalf("second report changed financial scope %d", i)
				}
			}
			view := auditCallView(t, h, request)
			if !view.ReportConflict || view.ObservedUsage == nil || *view.ObservedUsage != *first.Usage || view.MeasurementAnomaly {
				t.Fatalf("conflict replaced first evidence: %+v", view)
			}
			var conflictHash string
			if err := h.Pool.QueryRow(h.Ctx, "select report_conflict_hash from physical_calls where physical_call_id=$1", request.PhysicalCallID).Scan(&conflictHash); err != nil || conflictHash != second.ReportHash {
				t.Fatalf("first conflict identity: %s %v", conflictHash, err)
			}
			third := auditReportRequest(t, h, request, 1, 1)
			auditSettle(t, h, third)
			if err := h.Pool.QueryRow(h.Ctx, "select report_conflict_hash from physical_calls where physical_call_id=$1", request.PhysicalCallID).Scan(&conflictHash); err != nil || conflictHash != second.ReportHash {
				t.Fatal("subsequent conflict overwrote first conflict identity")
			}
		})
	}
}

func TestRunProviderAuditStorageConcurrentReportsHaveOneFinancialWinner(t *testing.T) {
	h := setupAuditHarness(t)
	claimed := auditAtModelStep(t, h, "tenant-a", "report-race")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	reports := []agentrun.SettleUsageRequest{auditReportRequest(t, h, request, 10, 5), auditReportRequest(t, h, request, 20, 5)}
	responses := make([]agentrun.SettleUsageResponse, 2)
	errorsFound := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range reports {
		wg.Go(func() {
			<-start
			responses[i], errorsFound[i] = h.Store.SettleUsage(h.Ctx, h.Principal, reports[i])
		})
	}
	close(start)
	wg.Wait()
	winners, conflicts := 0, 0
	for i, response := range responses {
		if errorsFound[i] != nil {
			t.Fatal(errorsFound[i])
		}
		if response.NewlySettled {
			winners++
		}
		if response.ReportConflict {
			conflicts++
		}
	}
	view := auditCallView(t, h, request)
	if winners != 1 || conflicts != 1 || !view.UsageKnown || !view.ReportConflict || view.MeasurementAnomaly ||
		(view.KnownTokens != 15 && view.KnownTokens != 25) {
		t.Fatalf("concurrent reports: winners=%d conflicts=%d view=%+v", winners, conflicts, view)
	}
}

func TestRunProviderAuditStorageLateOriginalSessionAndWindow(t *testing.T) {
	h := setupAuditHarness(t)
	claimed := auditAtModelStep(t, h, "tenant-a", "audit-late")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	report := auditReportRequest(t, h, request, 10, 5)
	if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancel-before-audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update worker_sessions set seen_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' where session_id=$1", h.Session.ID); err != nil {
		t.Fatal(err)
	}
	newSession, err := h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	wrong := report
	wrong.Lease.SessionID = newSession.ID
	if _, err := h.Store.SettleUsage(h.Ctx, h.Principal, wrong); !errors.Is(err, agentrun.ErrCallConflict) {
		t.Fatalf("new session replaced original metering authority: %v", err)
	}
	h.Options.Profiles[0].Executable = false
	h.Options.Workers[0].ProfileIDs = nil
	h.Store, err = runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set valid_until=clock_timestamp()-interval '1 second' where scope='batch'"); err != nil {
		t.Fatal(err)
	}
	before := ledgerView(t, h, claimed.Lease)
	if response := auditSettle(t, h, report); !response.NewlySettled || response.PersistedReportHash != report.ReportHash {
		t.Fatalf("old session/disabled profile/expired batch lost original reporting right: %+v", response)
	}
	after := ledgerView(t, h, claimed.Lease)
	if after.State != agentrun.Cancelled || before.CursorVersion != after.CursorVersion || !before.UpdatedAt.Equal(after.UpdatedAt) ||
		after.LeaseUntil != nil || after.AttemptDeadline != nil {
		t.Fatal("late report changed terminal execution")
	}
	if _, err := h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: request.Lease, Step: request.Step,
		PhysicalCallID: request.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: "accepted",
		UsageKnown: true, Usage: report.Usage, AuditHash: report.ProviderAudit.AuditHash}); !errors.Is(err, agentrun.ErrProfileUnavailable) {
		t.Fatalf("late report restored ordinary execution: %v", err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update physical_calls set reserved_at=clock_timestamp()-interval '30 days' where physical_call_id=$1", request.PhysicalCallID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.SettleUsage(h.Ctx, h.Principal, report); !errors.Is(err, agentrun.ErrCallSettlementExpired) {
		t.Fatalf("window equality accepted: %v", err)
	}
	if !reflect.DeepEqual(after.Budget, ledgerView(t, h, claimed.Lease).Budget) {
		t.Fatal("expired replay modified finances")
	}
}
