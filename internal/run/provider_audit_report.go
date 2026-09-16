package run

import (
	"errors"
	"strconv"
)

// ReportBinding comes from the original reservation and immutable profile, not
// from the report or a later Run cursor. It contains no live execution grant.
type ReportBinding struct {
	ExecutionBindingHash  string  `json:"execution_binding_hash"`
	PhysicalCallID        string  `json:"physical_call_id"`
	ParameterHash         string  `json:"parameter_hash"`
	Subcall               Subcall `json:"subcall"`
	ExpectedResponseModel string  `json:"expected_response_model"`
}

// CallReport is one immutable report, including non-priceable observed usage.
// It is never updated in place from audit-only to a different usage report.
type CallReport struct {
	Usage         *UsageReport   `json:"usage"`
	ProviderAudit *ProviderAudit `json:"provider_audit"`
}

// ExecutionBindingHash hashes the already authorized Lease and Step. Its shape
// checks cannot establish authority; Reserve must validate and persist it there.
func ExecutionBindingHash(lease Lease, step StepIdentity) (string, error) {
	if !ValidIdentifier(lease.TenantID) || !ValidIdentifier(lease.WorkerID) ||
		!ValidUUID(lease.RunID) || !ValidUUID(lease.SessionID) ||
		lease.AttemptNo < 1 || lease.AttemptNo > MaxSafeInteger || lease.FencingToken < 1 || lease.FencingToken > MaxSafeInteger ||
		!ValidUUID(step.ID) || !validStepKind(step.Kind) || step.Sequence < 1 || step.Sequence > 32 ||
		step.CursorVersion < 0 || step.CursorVersion >= 32 || step.Sequence != step.CursorVersion+1 ||
		!ValidHash(step.InputHash) || !ValidIdentifier(step.ProfileID) || !ValidHash(step.ProfileHash) ||
		!ValidUUID(step.SnapshotID) || !ValidHash(step.SnapshotHash) {
		return "", ErrInvalidArgument
	}
	return Fingerprint("jobforge.run.call-binding.v1", lease.TenantID, lease.RunID, lease.WorkerID, lease.SessionID,
		strconv.FormatInt(lease.AttemptNo, 10), strconv.FormatInt(lease.FencingToken, 10), step.ID,
		strconv.FormatInt(step.Sequence, 10), step.Kind, strconv.FormatInt(step.CursorVersion, 10),
		step.InputHash, step.ProfileID, step.ProfileHash, step.SnapshotID, step.SnapshotHash), nil
}

// Hash binds report content to the persisted original execution and call.
func (r CallReport) Hash(binding ReportBinding) string {
	usageHash, auditHash := "", ""
	if r.Usage != nil {
		usageHash = r.Usage.UsageHash
	}
	if r.ProviderAudit != nil {
		auditHash = r.ProviderAudit.AuditHash
	}
	return Fingerprint("jobforge.run.call-report.v1", binding.ExecutionBindingHash,
		binding.PhysicalCallID, binding.ParameterHash, usageHash, auditHash)
}

// Validate verifies the complete report against original reservation facts.
// ExpectedResponseModel must be selected from the frozen profile by the caller.
func (r CallReport) Validate(binding ReportBinding) error {
	if !ValidHash(binding.ExecutionBindingHash) || !ValidUUID(binding.PhysicalCallID) ||
		!ValidHash(binding.ParameterHash) || r.validateContent() != nil {
		return ErrInvalidArgument
	}
	if binding.Subcall != SubcallChat {
		if binding.Subcall != SubcallQueryEmbedding || r.ProviderAudit != nil || r.Usage == nil {
			return ErrInvalidArgument
		}
		return nil
	}
	if !ValidIdentifier(binding.ExpectedResponseModel) || r.ProviderAudit == nil {
		return ErrInvalidArgument
	}
	a := r.ProviderAudit
	if a.safeIdentity() && ((*a.ResponseModel == binding.ExpectedResponseModel) != (a.IdentityState == ProviderIdentityCompatible)) {
		return ErrInvalidArgument
	}
	if r.Usage != nil {
		receiptHash, err := a.ReceiptHash(binding.PhysicalCallID)
		if err != nil || r.Usage.ReceiptHash != receiptHash {
			return ErrInvalidArgument
		}
	}
	return nil
}

// Verify also compares the submitted report hash to the server's recomputation.
func (r CallReport) Verify(binding ReportBinding, reportHash string) error {
	if r.Validate(binding) != nil || !ValidHash(reportHash) || r.Hash(binding) != reportHash {
		return ErrInvalidArgument
	}
	return nil
}

func (r CallReport) validateContent() error {
	if r.Usage == nil && r.ProviderAudit == nil {
		return ErrInvalidArgument
	}
	if r.Usage != nil && r.Usage.Validate() != nil {
		return ErrInvalidArgument
	}
	if r.ProviderAudit == nil {
		return nil
	}
	a := r.ProviderAudit
	if a.Validate() != nil || (a.UsageEvidence == UsageEvidenceComplete) != (r.Usage != nil) {
		return ErrInvalidArgument
	}
	if r.Usage != nil && (r.Usage.InputTokens > MaxSafeInteger-r.Usage.OutputTokens ||
		(a.ReasoningTokens != nil && *a.ReasoningTokens > r.Usage.OutputTokens)) {
		return ErrInvalidArgument
	}
	return nil
}

// ObservationHashV2 hashes mapped domain errors, never the original wire error.
// Report association and current execution authority require transactional checks.
func ObservationHashV2(observation ObserveCallRequest, auditHash string) (string, error) {
	if observation.Validate() != nil || (auditHash != "" && !ValidHash(auditHash)) {
		return "", ErrInvalidArgument
	}
	usageHash := ""
	if observation.Usage != nil {
		usageHash = observation.Usage.UsageHash
	}
	return Fingerprint("jobforge.run.observation.v2", observation.TransportOutcome,
		strconv.Itoa(observation.HTTPStatus), observation.ErrorCode, observation.BusinessOutcome, usageHash, auditHash), nil
}

// BatchStopCode is a bounded persistent batch-freeze reason, not a Run state.
type BatchStopCode string

// Batch stop reasons are applied first-write-wins by the locked account store.
const (
	BatchStopMeasurementAnomaly      BatchStopCode = "MEASUREMENT_ANOMALY"
	BatchStopProviderHTTPRejected    BatchStopCode = "PROVIDER_HTTP_REJECTED"
	BatchStopProviderIdentityInvalid BatchStopCode = "PROVIDER_IDENTITY_INVALID"
	BatchStopProviderModeInvalid     BatchStopCode = "PROVIDER_MODE_INVALID"
	BatchStopChatUsageUnknown        BatchStopCode = "CHAT_USAGE_UNKNOWN"
	BatchStopReportConflict          BatchStopCode = "REPORT_CONFLICT"
)

// Valid accepts the closed nonempty set of persisted stop reasons.
func (code BatchStopCode) Valid() bool {
	switch code {
	case BatchStopMeasurementAnomaly, BatchStopProviderHTTPRejected, BatchStopProviderIdentityInvalid,
		BatchStopProviderModeInvalid, BatchStopChatUsageUnknown, BatchStopReportConflict:
		return true
	default:
		return false
	}
}

// ReportDisposition describes a first report's required atomic ledger changes.
// Recorded/anomaly keep the complete hold and cannot produce UsageKnown=true.
// PriceEligible remains independent of a token/cost anomaly or provider mode.
type ReportDisposition struct {
	PriceEligible      bool          `json:"price_eligible"`
	MeasurementAnomaly bool          `json:"measurement_anomaly"`
	UsageKnown         bool          `json:"usage_known"`
	KnownTokens        int64         `json:"known_tokens"`
	KnownCostMicroyuan int64         `json:"known_cost_microyuan"`
	Settlement         string        `json:"settlement"`
	BatchStopCode      BatchStopCode `json:"batch_stop_code"`
}

// FirstReportDisposition evaluates only the first authenticated coherent report.
// Under the same call/account locks, the store MUST first route identical replay
// to existing facts and different-report conflict to batch-only freezing. Never
// evaluate a second report here: even larger counters cannot alter first-report
// finances or newly trigger family/tenant measurement-anomaly freezing.
func FirstReportDisposition(binding ReportBinding, report CallReport, budget CallBudget, price Pricing) (ReportDisposition, error) {
	var result ReportDisposition
	if report.Validate(binding) != nil || !validReportBudget(budget) {
		return result, ErrInvalidArgument
	}
	result.Settlement = "recorded"
	usage := report.Usage
	result.PriceEligible = usage != nil && (binding.Subcall == SubcallQueryEmbedding ||
		report.ProviderAudit.IdentityState == ProviderIdentityCompatible)
	var cost int64
	if usage != nil {
		result.MeasurementAnomaly = usage.InputTokens > budget.InputTokens || usage.OutputTokens > budget.OutputTokens ||
			usage.InputTokens > MaxSafeInteger-usage.OutputTokens || usage.InputTokens+usage.OutputTokens > budget.TotalTokens ||
			(binding.Subcall == SubcallQueryEmbedding && usage.CachedInputTokens != 0)
		// An incompatible model's observed counters never enter the old tariff.
		if result.PriceEligible && binding.Subcall == SubcallChat {
			var err error
			cost, err = UsageCost(price, *usage)
			if errors.Is(err, ErrBudgetExhausted) {
				result.MeasurementAnomaly = true
			} else if err != nil {
				return ReportDisposition{}, err
			}
			result.MeasurementAnomaly = result.MeasurementAnomaly || cost > budget.CostMicroyuan
		}
	}
	if result.MeasurementAnomaly {
		result.Settlement, result.BatchStopCode = "anomaly", BatchStopMeasurementAnomaly
		return result, nil
	}
	if result.PriceEligible {
		result.Settlement, result.UsageKnown = "settled", true
		result.KnownTokens, result.KnownCostMicroyuan = usage.InputTokens+usage.OutputTokens, cost
	}
	result.BatchStopCode = firstAuditStop(binding.Subcall, report.ProviderAudit, result.PriceEligible)
	return result, nil
}

func validReportBudget(budget CallBudget) bool {
	for _, value := range []int64{budget.InputTokens, budget.OutputTokens, budget.TotalTokens, budget.CostMicroyuan} {
		if value < 0 || value > MaxSafeInteger {
			return false
		}
	}
	return budget.InputTokens <= MaxSafeInteger-budget.OutputTokens && budget.TotalTokens == budget.InputTokens+budget.OutputTokens
}

func firstAuditStop(subcall Subcall, audit *ProviderAudit, priceEligible bool) BatchStopCode {
	if subcall != SubcallChat {
		return ""
	}
	switch {
	case audit.ResponseComplete && audit.HTTPStatus != 200:
		return BatchStopProviderHTTPRejected
	case audit.ResponseComplete && audit.HTTPStatus == 200 &&
		(audit.IdentityState == ProviderIdentityIncompatible || audit.IdentityState == ProviderIdentityInvalid):
		return BatchStopProviderIdentityInvalid
	case audit.ModeState == ProviderModeUnexpected || audit.ModeState == ProviderModeInvalid || audit.ReasoningState == ReasoningInvalid:
		return BatchStopProviderModeInvalid
	case !priceEligible:
		return BatchStopChatUsageUnknown
	default:
		return ""
	}
}
