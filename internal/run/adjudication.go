package run

import (
	"bytes"
	"encoding/json"
	"time"
)

const (
	// SupportAgentPromptV4 preserves v3 investigation and adds final-only adjudication.
	SupportAgentPromptV4 = "support-agent-prompt-v4"
	// SupportAgentPromptV5 separates the final stage's task from the investigation stage.
	SupportAgentPromptV5 = "support-agent-prompt-v5"
	// SupportAdjudicationExecutorVersion opts Agent into the two-stage model graph.
	SupportAdjudicationExecutorVersion = "linux-v2-adjudication-runtime-1"
	// SupportAdjudicationV2ExecutorVersion runs the same two-stage graph with the
	// separated v5 final task; the investigation contract is unchanged.
	SupportAdjudicationV2ExecutorVersion = "linux-v2-adjudication-runtime-2"
	// SupportFixedAdjudicationExecutorVersion uses the same final model in the fixed graph.
	SupportFixedAdjudicationExecutorVersion = "linux-v2-fixed-adjudication-runtime-1"
	// FinalAdjudicationMaxTokens includes reasoning and final content together.
	FinalAdjudicationMaxTokens int64 = 16384
)

// FinalAdjudicationEnabled selects only a fully frozen new profile.
func (p Profile) FinalAdjudicationEnabled() bool {
	if p.ExecutorVersion != SupportAdjudicationExecutorVersion && p.ExecutorVersion != SupportAdjudicationV2ExecutorVersion && p.ExecutorVersion != SupportFixedAdjudicationExecutorVersion {
		return false
	}
	hash, err := SupportProfileHash(p)
	return err == nil && hash == p.Hash
}

// ThinkingStep binds mode to the original step, including during late accounting.
func (p Profile) ThinkingStep(kind string) bool {
	return kind == "model_proposal" && p.FinalAdjudicationEnabled()
}

// AllowsProviderMode never upgrades an investigation or historical call.
func (p Profile) AllowsProviderMode(kind string, mode ProviderModeState) bool {
	return mode == ProviderModeNonthinking || mode == ProviderModeThinking && p.ThinkingStep(kind)
}

// AllowsProviderAudit also prevents a new schema being replayed as an old call.
func (p Profile) AllowsProviderAudit(kind string, audit *ProviderAudit) bool {
	if audit == nil || !p.AllowsProviderMode(kind, audit.ModeState) {
		return false
	}
	expected := int64(1)
	if p.ThinkingStep(kind) {
		expected = 2
	}
	return audit.SchemaVersion == expected
}

// CallTimeout is the request bound; reservation also clips it to live deadlines.
func (p Profile) CallTimeout(kind string, subcall Subcall) time.Duration {
	if subcall != SubcallChat {
		return 10 * time.Second
	}
	if p.ThinkingStep(kind) {
		return 120 * time.Second
	}
	return 60 * time.Second
}

// ReservationBudgetForStep derives both new and replayed reservations identically.
func ReservationBudgetForStep(p Profile, kind string, subcall Subcall) (CallBudget, error) {
	if !p.FinalAdjudicationEnabled() {
		return ReservationBudget(p, subcall)
	}
	if subcall == SubcallChat && kind != "model_proposal" {
		p.MaxOutputTokens = 1024
	}
	return reservationBudget(p, subcall, FinalAdjudicationMaxTokens)
}

// ValidInvestigationComplete checks a bounded draft, never its business truth.
// The draft is audit-only and cannot become an approvable Proposal.
func ValidInvestigationComplete(raw []byte) bool {
	var marker struct {
		Type  string          `json:"type"`
		Draft json.RawMessage `json:"draft"`
	}
	if ValidateStepJSON(raw, 16384) != nil || !exactSupportKeys(raw, "type", "draft") ||
		strictSupportDecode(raw, &marker) != nil || marker.Type != "investigation_complete" ||
		!exactSupportKeys(marker.Draft, "decision", "action", "conclusion", "requested_fields", "target_ticket_status", "claims") {
		return false
	}
	var draft map[string]json.RawMessage
	if json.Unmarshal(marker.Draft, &draft) != nil {
		return false
	}
	for key, value := range draft {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
		if key == "claims" || key == "requested_fields" {
			var items []json.RawMessage
			if json.Unmarshal(value, &items) != nil {
				return false
			}
		} else {
			var text string
			if json.Unmarshal(value, &text) != nil {
				return false
			}
		}
	}
	return true
}
