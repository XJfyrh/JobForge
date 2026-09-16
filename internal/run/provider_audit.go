package run

import "strconv"

// MaxProviderAuditBytes bounds both the received and the encoded audit object.
const MaxProviderAuditBytes = 2048

// ProviderIdentityState distinguishes safe identity from price compatibility.
type ProviderIdentityState string

// Provider identity states describe the fixed adapter's bounded observations.
const (
	ProviderIdentityCompatible   ProviderIdentityState = "compatible"
	ProviderIdentityIncompatible ProviderIdentityState = "incompatible"
	ProviderIdentityInvalid      ProviderIdentityState = "invalid"
	ProviderIdentityUnavailable  ProviderIdentityState = "unavailable"
)

// UsageEvidenceState describes complete counters independently of their price.
type UsageEvidenceState string

// Usage evidence states never substitute zero counters for missing evidence.
const (
	UsageEvidenceComplete    UsageEvidenceState = "complete"
	UsageEvidenceAbsent      UsageEvidenceState = "absent"
	UsageEvidenceInvalid     UsageEvidenceState = "invalid"
	UsageEvidenceUnavailable UsageEvidenceState = "unavailable"
)

// ReasoningState preserves absent details separately from observed zero.
type ReasoningState string

// Reasoning states record only verified bounded counts, never reasoning text.
const (
	ReasoningObserved    ReasoningState = "observed"
	ReasoningAbsent      ReasoningState = "absent"
	ReasoningInvalid     ReasoningState = "invalid"
	ReasoningUnavailable ReasoningState = "unavailable"
)

// ProviderModeState describes the fixed nonthinking, no-tools response envelope.
type ProviderModeState string

// Provider mode states are observations, not permission to execute tool calls.
const (
	ProviderModeNonthinking ProviderModeState = "nonthinking"
	ProviderModeUnexpected  ProviderModeState = "unexpected"
	ProviderModeInvalid     ProviderModeState = "invalid"
	ProviderModeUnavailable ProviderModeState = "unavailable"
)

// ProviderAudit retains bounded facts from the supervised fixed adapter. The
// response digest cannot reconstruct or independently verify HTTP body bytes.
// Nullable pointers preserve explicit null; strict JSON decoding also requires
// every key, so a missing field never acquires a scalar zero or null default.
type ProviderAudit struct {
	SchemaVersion     int64                 `json:"schema_version"`
	Provider          string                `json:"provider"`
	ResponseComplete  bool                  `json:"response_complete"`
	HTTPStatus        int64                 `json:"http_status"`
	ResponseSHA256    *string               `json:"response_sha256"`
	IdentityState     ProviderIdentityState `json:"identity_state"`
	ResponseID        *string               `json:"response_id"`
	ResponseModel     *string               `json:"response_model"`
	SystemFingerprint *string               `json:"system_fingerprint"`
	Created           *int64                `json:"created"`
	UsageEvidence     UsageEvidenceState    `json:"usage_evidence"`
	ReasoningState    ReasoningState        `json:"reasoning_state"`
	ReasoningTokens   *int64                `json:"reasoning_tokens"`
	ModeState         ProviderModeState     `json:"mode_state"`
	AuditHash         string                `json:"audit_hash"`
}

// Hash uses ADR-0020's fixed field order and preserves null versus integer zero.
func (a ProviderAudit) Hash() string {
	complete := "0"
	if a.ResponseComplete {
		complete = "1"
	}
	return Fingerprint("jobforge.run.provider-audit.v1", strconv.FormatInt(a.SchemaVersion, 10),
		a.Provider, complete, strconv.FormatInt(a.HTTPStatus, 10), auditString(a.ResponseSHA256),
		string(a.IdentityState), auditString(a.ResponseID), auditString(a.ResponseModel),
		auditString(a.SystemFingerprint), auditInteger(a.Created), string(a.UsageEvidence),
		string(a.ReasoningState), auditInteger(a.ReasoningTokens), string(a.ModeState))
}

// Validate checks intrinsic audit facts and the hash. CallReport.Validate must
// additionally check the frozen expected model, usage count bounds and receipt.
func (a ProviderAudit) Validate() error {
	if a.SchemaVersion != 1 || a.Provider != "deepseek" || !a.validStates() ||
		!validAuditIdentifier(a.ResponseID) || !validAuditIdentifier(a.ResponseModel) ||
		!validAuditIdentifier(a.SystemFingerprint) || !validAuditInteger(a.Created) ||
		!validAuditInteger(a.ReasoningTokens) || (a.ResponseSHA256 != nil && !ValidHash(*a.ResponseSHA256)) ||
		!ValidHash(a.AuditHash) || a.AuditHash != a.Hash() {
		return ErrInvalidArgument
	}
	if !a.ResponseComplete {
		if a.HTTPStatus != 0 || a.ResponseSHA256 != nil || !a.unavailableResponse() {
			return ErrInvalidArgument
		}
		return nil
	}
	if a.HTTPStatus < 100 || a.HTTPStatus > 599 || a.ResponseSHA256 == nil {
		return ErrInvalidArgument
	}
	if a.HTTPStatus != 200 {
		if !a.unavailableResponse() {
			return ErrInvalidArgument
		}
		return nil
	}
	if a.IdentityState == ProviderIdentityUnavailable ||
		(a.safeIdentity() && !a.completeIdentity()) ||
		(a.UsageEvidence == UsageEvidenceComplete && !a.safeIdentity()) {
		return ErrInvalidArgument
	}
	return a.validateReasoning()
}

func (a ProviderAudit) validStates() bool {
	identity := a.IdentityState == ProviderIdentityCompatible || a.IdentityState == ProviderIdentityIncompatible ||
		a.IdentityState == ProviderIdentityInvalid || a.IdentityState == ProviderIdentityUnavailable
	usage := a.UsageEvidence == UsageEvidenceComplete || a.UsageEvidence == UsageEvidenceAbsent ||
		a.UsageEvidence == UsageEvidenceInvalid || a.UsageEvidence == UsageEvidenceUnavailable
	reasoning := a.ReasoningState == ReasoningObserved || a.ReasoningState == ReasoningAbsent ||
		a.ReasoningState == ReasoningInvalid || a.ReasoningState == ReasoningUnavailable
	mode := a.ModeState == ProviderModeNonthinking || a.ModeState == ProviderModeUnexpected ||
		a.ModeState == ProviderModeInvalid || a.ModeState == ProviderModeUnavailable
	return identity && usage && reasoning && mode
}

func (a ProviderAudit) unavailableResponse() bool {
	return a.ResponseID == nil && a.ResponseModel == nil && a.SystemFingerprint == nil && a.Created == nil &&
		a.IdentityState == ProviderIdentityUnavailable && a.UsageEvidence == UsageEvidenceUnavailable &&
		a.ReasoningState == ReasoningUnavailable && a.ReasoningTokens == nil && a.ModeState == ProviderModeUnavailable
}

func (a ProviderAudit) safeIdentity() bool {
	return a.IdentityState == ProviderIdentityCompatible || a.IdentityState == ProviderIdentityIncompatible
}

func (a ProviderAudit) completeIdentity() bool {
	return a.ResponseID != nil && a.ResponseModel != nil && a.SystemFingerprint != nil && a.Created != nil
}

func (a ProviderAudit) validateReasoning() error {
	if a.UsageEvidence == UsageEvidenceComplete {
		if a.ReasoningState != ReasoningObserved && a.ReasoningState != ReasoningAbsent {
			return ErrInvalidArgument
		}
	} else if a.ReasoningState != ReasoningInvalid && a.ReasoningState != ReasoningUnavailable {
		return ErrInvalidArgument
	}
	if (a.ReasoningState == ReasoningObserved) != (a.ReasoningTokens != nil) {
		return ErrInvalidArgument
	}
	if a.ReasoningState == ReasoningInvalid && a.ModeState != ProviderModeInvalid {
		return ErrInvalidArgument
	}
	if a.ModeState == ProviderModeNonthinking &&
		(a.UsageEvidence != UsageEvidenceComplete || (a.ReasoningTokens != nil && *a.ReasoningTokens != 0)) {
		return ErrInvalidArgument
	}
	if a.UsageEvidence == UsageEvidenceComplete && a.ModeState == ProviderModeUnavailable {
		return ErrInvalidArgument
	}
	if a.ReasoningTokens != nil && *a.ReasoningTokens > 0 &&
		a.ModeState != ProviderModeUnexpected && a.ModeState != ProviderModeInvalid {
		return ErrInvalidArgument
	}
	return nil
}

// ReceiptHash binds the actual returned identity, including an incompatible
// model. Hash consistency alone grants neither pricing nor execution rights.
func (a ProviderAudit) ReceiptHash(physicalCallID string) (string, error) {
	if a.Validate() != nil || !ValidUUID(physicalCallID) || !a.safeIdentity() || !a.completeIdentity() || a.ResponseSHA256 == nil {
		return "", ErrInvalidArgument
	}
	return Fingerprint("jobforge.deepseek.receipt.v1", physicalCallID, *a.ResponseSHA256,
		*a.ResponseID, *a.ResponseModel, *a.SystemFingerprint, strconv.FormatInt(*a.Created, 10)), nil
}

func validAuditIdentifier(value *string) bool { return value == nil || ValidIdentifier(*value) }

func validAuditInteger(value *int64) bool {
	return value == nil || (*value >= 0 && *value <= MaxSafeInteger)
}

func auditString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func auditInteger(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}
