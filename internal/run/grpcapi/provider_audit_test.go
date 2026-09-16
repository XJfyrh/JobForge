package grpcapi

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func auditWireFixture(t *testing.T) *agentv1.ProviderAudit {
	t.Helper()
	wire := &agentv1.ProviderAudit{SchemaVersion: 1, Provider: "deepseek", ResponseComplete: true,
		HttpStatus: 200, ResponseSha256: proto.String(strings.Repeat("e", 64)),
		IdentityState: agentv1.ProviderIdentityState_PROVIDER_IDENTITY_STATE_COMPATIBLE,
		ResponseId:    proto.String("synthetic-chat"), ResponseModel: proto.String("deepseek-flash"),
		SystemFingerprint: proto.String("fp-test"), Created: proto.Int64(0),
		UsageEvidence:   agentv1.ProviderUsageEvidence_PROVIDER_USAGE_EVIDENCE_COMPLETE,
		ReasoningState:  agentv1.ProviderReasoningState_PROVIDER_REASONING_STATE_OBSERVED,
		ReasoningTokens: proto.Int64(0), ModeState: agentv1.ProviderModeState_PROVIDER_MODE_STATE_NONTHINKING}
	audit := run.ProviderAudit{SchemaVersion: 1, Provider: "deepseek", ResponseComplete: true, HTTPStatus: 200,
		ResponseSHA256: wire.ResponseSha256, IdentityState: run.ProviderIdentityCompatible,
		ResponseID: wire.ResponseId, ResponseModel: wire.ResponseModel, SystemFingerprint: wire.SystemFingerprint,
		Created: wire.Created, UsageEvidence: run.UsageEvidenceComplete, ReasoningState: run.ReasoningObserved,
		ReasoningTokens: wire.ReasoningTokens, ModeState: run.ProviderModeNonthinking}
	wire.AuditHash = audit.Hash()
	return wire
}

func TestProviderAuditWirePreservesZeroPresenceAndDetachesPointers(t *testing.T) {
	wire := auditWireFixture(t)
	audit, err := providerAuditFromWire(wire)
	if err != nil || audit.Created == nil || *audit.Created != 0 || audit.ReasoningTokens == nil || *audit.ReasoningTokens != 0 {
		t.Fatalf("observed zero lost presence: %v", err)
	}
	*wire.Created, *wire.ReasoningTokens, *wire.ResponseId = 99, 99, "mutated"
	if *audit.Created != 0 || *audit.ReasoningTokens != 0 || *audit.ResponseID != "synthetic-chat" || audit.Validate() != nil {
		t.Fatal("caller mutation changed immutable decoded provider facts")
	}
	if audit, err = providerAuditFromWire(nil); err != nil || audit != nil {
		t.Fatal("absent audit became an observed zero report")
	}
}

func TestProviderAuditWireRejectsUnspecifiedEnumsAndFalsePresence(t *testing.T) {
	for name, mutate := range map[string]func(*agentv1.ProviderAudit){
		"identity_unspecified":   func(a *agentv1.ProviderAudit) { a.IdentityState = 0 },
		"usage_unspecified":      func(a *agentv1.ProviderAudit) { a.UsageEvidence = 0 },
		"reasoning_unspecified":  func(a *agentv1.ProviderAudit) { a.ReasoningState = 0 },
		"mode_unspecified":       func(a *agentv1.ProviderAudit) { a.ModeState = 0 },
		"identity_unknown":       func(a *agentv1.ProviderAudit) { a.IdentityState = 99 },
		"absent_created":         func(a *agentv1.ProviderAudit) { a.Created = nil },
		"absent_reasoning":       func(a *agentv1.ProviderAudit) { a.ReasoningTokens = nil },
		"present_empty_identity": func(a *agentv1.ProviderAudit) { a.ResponseId = proto.String("") },
		"tampered_hash":          func(a *agentv1.ProviderAudit) { a.AuditHash = strings.Repeat("f", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			a := auditWireFixture(t)
			mutate(a)
			if _, err := providerAuditFromWire(a); !errors.Is(err, run.ErrInvalidArgument) {
				t.Fatalf("untrusted wire audit accepted: %v", err)
			}
		})
	}
}
