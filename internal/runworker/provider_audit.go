package runworker

import (
	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func providerAuditToWire(a *run.ProviderAudit) *agentv1.ProviderAudit {
	if a == nil {
		return nil
	}
	return &agentv1.ProviderAudit{SchemaVersion: int32(a.SchemaVersion), Provider: a.Provider,
		ResponseComplete: a.ResponseComplete, HttpStatus: int32(a.HTTPStatus), ResponseSha256: a.ResponseSHA256,
		ResponseId: a.ResponseID, ResponseModel: a.ResponseModel, SystemFingerprint: a.SystemFingerprint,
		Created: a.Created, ReasoningTokens: a.ReasoningTokens, AuditHash: a.AuditHash,
		IdentityState: map[run.ProviderIdentityState]agentv1.ProviderIdentityState{
			"compatible":   agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_COMPATIBLE,
			"incompatible": agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_INCOMPATIBLE,
			"invalid":      agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_INVALID,
			"unavailable":  agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_UNAVAILABLE,
		}[a.IdentityState],
		UsageEvidence: map[run.UsageEvidenceState]agentv1.ProviderUsageEvidence{
			"complete":    agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_COMPLETE,
			"absent":      agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_ABSENT,
			"invalid":     agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_INVALID,
			"unavailable": agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_UNAVAILABLE,
		}[a.UsageEvidence],
		ReasoningState: map[run.ReasoningState]agentv1.ProviderReasoningState{
			"observed":    agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_OBSERVED,
			"absent":      agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_ABSENT,
			"invalid":     agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_INVALID,
			"unavailable": agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_UNAVAILABLE,
		}[a.ReasoningState],
		ModeState: map[run.ProviderModeState]agentv1.ProviderModeState{
			"nonthinking": agentv1.ProviderModeState_PROVIDER_MODE_STATE_NONTHINKING,
			"unexpected":  agentv1.ProviderModeState_PROVIDER_MODE_STATE_UNEXPECTED,
			"invalid":     agentv1.ProviderModeState_PROVIDER_MODE_STATE_INVALID,
			"unavailable": agentv1.ProviderModeState_PROVIDER_MODE_STATE_UNAVAILABLE,
		}[a.ModeState],
	}
}
