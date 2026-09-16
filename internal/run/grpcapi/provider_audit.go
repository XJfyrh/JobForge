package grpcapi

import (
	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func providerAuditFromWire(value *agentv1.ProviderAudit) (*run.ProviderAudit, error) {
	if value == nil {
		return nil, nil
	}
	audit := run.ProviderAudit{SchemaVersion: int64(value.SchemaVersion), Provider: value.Provider,
		ResponseComplete: value.ResponseComplete, HTTPStatus: int64(value.HttpStatus), ResponseSHA256: value.ResponseSha256,
		ResponseID: value.ResponseId, ResponseModel: value.ResponseModel, SystemFingerprint: value.SystemFingerprint,
		Created: value.Created, ReasoningTokens: value.ReasoningTokens, AuditHash: value.AuditHash,
		IdentityState: map[agentv1.ProviderIdentityState]run.ProviderIdentityState{
			agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_COMPATIBLE:   run.ProviderIdentityCompatible,
			agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_INCOMPATIBLE: run.ProviderIdentityIncompatible,
			agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_INVALID:      run.ProviderIdentityInvalid,
			agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_UNAVAILABLE:  run.ProviderIdentityUnavailable}[value.IdentityState],
		UsageEvidence: map[agentv1.ProviderUsageEvidence]run.UsageEvidenceState{
			agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_COMPLETE:    run.UsageEvidenceComplete,
			agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_ABSENT:      run.UsageEvidenceAbsent,
			agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_INVALID:     run.UsageEvidenceInvalid,
			agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_UNAVAILABLE: run.UsageEvidenceUnavailable}[value.UsageEvidence],
		ReasoningState: map[agentv1.ProviderReasoningState]run.ReasoningState{
			agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_OBSERVED:    run.ReasoningObserved,
			agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_ABSENT:      run.ReasoningAbsent,
			agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_INVALID:     run.ReasoningInvalid,
			agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_UNAVAILABLE: run.ReasoningUnavailable}[value.ReasoningState],
		ModeState: map[agentv1.ProviderModeState]run.ProviderModeState{
			agentv1.ProviderModeState_PROVIDER_MODE_STATE_NONTHINKING: run.ProviderModeNonthinking,
			agentv1.ProviderModeState_PROVIDER_MODE_STATE_UNEXPECTED:  run.ProviderModeUnexpected,
			agentv1.ProviderModeState_PROVIDER_MODE_STATE_INVALID:     run.ProviderModeInvalid,
			agentv1.ProviderModeState_PROVIDER_MODE_STATE_UNAVAILABLE: run.ProviderModeUnavailable}[value.ModeState]}
	encoded, err := run.ProviderAuditJSON(audit)
	if err != nil {
		return nil, err
	}
	// The strict domain decoder also detaches all optional pointers from the
	// caller-owned message before the storage service receives this immutable fact.
	copyAudit, err := run.DecodeProviderAudit(encoded)
	return &copyAudit, err
}
