package run

import "time"

const (
	// ProviderAuditPolicyDeepSeekV1 fixes the accepted immutable report policy.
	ProviderAuditPolicyDeepSeekV1 = "deepseek-audit-v1"
	// ProviderAuditExecutorVersion selects the single audited runtime deployment.
	ProviderAuditExecutorVersion = "linux-v2-audit-runtime-1"
	// MaxCallsPerRun is the existing family physical-call limit, not pagination.
	MaxCallsPerRun = 44
	// MaxCallsResponseBytes bounds the complete encoded tenant-scoped response.
	MaxCallsResponseBytes = 256 * 1024
)

// AuditEnabled selects semantics from the immutable profile, never request data.
func (p Profile) AuditEnabled() bool { return p.ProviderAuditPolicy == ProviderAuditPolicyDeepSeekV1 }

// ValidateAuditPolicy rejects mismatched policy/runtime deployments. The profile
// registrar must include these fields in its immutable content and profile hash.
func (p Profile) ValidateAuditPolicy() error {
	if p.ProviderAuditPolicy == "" {
		if p.ExpectedResponseModel != "" || p.ExecutorVersion == ProviderAuditExecutorVersion {
			return ErrProfileUnavailable
		}
		return nil
	}
	if !p.AuditEnabled() || p.ExecutorVersion != ProviderAuditExecutorVersion || !ValidIdentifier(p.ExpectedResponseModel) {
		return ErrProfileUnavailable
	}
	return nil
}

// CallView exposes only this Run's bounded evidence, never execution credentials
// or another tenant's call identity. ObservedUsage does not imply known pricing.
type CallView struct {
	PhysicalCallID     string         `json:"physical_call_id"`
	StepID             string         `json:"step_id"`
	StepKind           string         `json:"step_kind"`
	AttemptNo          int64          `json:"attempt_no"`
	Ordinal            int64          `json:"ordinal"`
	Subcall            Subcall        `json:"subcall"`
	ParameterHash      string         `json:"parameter_hash"`
	ProfileID          string         `json:"profile_id"`
	ProfileHash        string         `json:"profile_hash"`
	PriceHash          string         `json:"price_hash"`
	ReservedAt         time.Time      `json:"reserved_at"`
	DispatchExpiresAt  time.Time      `json:"dispatch_expires_at"`
	CallDeadline       time.Time      `json:"call_deadline"`
	ObservedAt         *time.Time     `json:"observed_at"`
	ReportRecordedAt   *time.Time     `json:"report_recorded_at"`
	SettledAt          *time.Time     `json:"settled_at"`
	Reserved           CallBudget     `json:"reserved"`
	KnownTokens        int64          `json:"known_tokens"`
	KnownCostMicroyuan int64          `json:"known_cost_microyuan"`
	HeldTokens         int64          `json:"held_tokens"`
	HeldCostMicroyuan  int64          `json:"held_cost_microyuan"`
	UsageKnown         bool           `json:"usage_known"`
	MeasurementAnomaly bool           `json:"measurement_anomaly"`
	ReportHash         *string        `json:"report_hash"`
	AuditHash          *string        `json:"audit_hash"`
	ProviderAudit      *ProviderAudit `json:"provider_audit"`
	ObservedUsage      *UsageReport   `json:"observed_usage"`
	SettledUsage       *UsageReport   `json:"settled_usage"`
	ReportConflict     bool           `json:"report_conflict"`
	AuditStatus        string         `json:"audit_status"`
	TransportOutcome   *string        `json:"transport_outcome"`
	HTTPStatus         *int           `json:"http_status"`
	ErrorCode          *string        `json:"error_code"`
	BusinessOutcome    *string        `json:"business_outcome"`
}

// CallsResponse is one repeatable-read snapshot. The batch reason can be shared,
// but its triggering call/Run/tenant is never exposed through this response.
type CallsResponse struct {
	RunID         string         `json:"run_id"`
	CapturedAt    time.Time      `json:"captured_at"`
	BatchFrozen   bool           `json:"batch_frozen"`
	BatchStopCode *BatchStopCode `json:"batch_stop_code"`
	Items         []CallView     `json:"items"`
}
