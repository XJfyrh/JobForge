package run

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/xjfyrh/jobforge/internal/business"
)

const (
	// SupportAgentPromptV8 uses complete policy evidence and one final-only call.
	SupportAgentPromptV8 = "support-agent-prompt-v8"
	// SupportFixedPromptV2 binds the fixed graph to the same final-only contract.
	SupportFixedPromptV2 = "support-fixed-prompt-v2"
	// SupportAdjudicationV5ExecutorVersion reads policy before investigation.
	SupportAdjudicationV5ExecutorVersion = "linux-v2-adjudication-runtime-5"
	// SupportFixedBundleExecutorVersion supplies the same policy capability to Fixed.
	SupportFixedBundleExecutorVersion = "linux-v2-fixed-adjudication-runtime-2"
)

// PolicyBundleEnabled never changes a historical profile's graph.
func (p Profile) PolicyBundleEnabled() bool {
	return (p.ExecutorVersion == SupportAdjudicationV5ExecutorVersion || p.ExecutorVersion == SupportFixedBundleExecutorVersion) && p.FinalAdjudicationEnabled()
}

func validatePolicyBundle(snapshot SnapshotBinding, result StepResult) error {
	var content struct {
		SnapshotID string `json:"snapshot_id"`
		Matches    []struct {
			IndexID       string `json:"index_id"`
			ChunkID       string `json:"chunk_id"`
			PolicyVersion string `json:"policy_version"`
			EvidenceRef   string `json:"evidence_ref"`
			Source        string `json:"source"`
			Text          string `json:"text"`
		} `json:"matches"`
	}
	ticket, _, err := supportSnapshot(snapshot)
	if err != nil || strictSupportDecode(result.Content, &content) != nil || content.SnapshotID != snapshot.ID {
		return ErrStepConflict
	}
	aliases := business.PolicyBundleAliases()
	if len(content.Matches) != len(aliases) || len(result.EvidenceRefs) != len(aliases) {
		return ErrStepConflict
	}
	for i, hit := range content.Matches {
		ref := "business-policy:" + snapshot.IndexID + ":" + aliases[i]
		if hit.ChunkID != aliases[i] || hit.IndexID != snapshot.IndexID || hit.PolicyVersion != ticket.PolicyVersion ||
			hit.EvidenceRef != ref || result.EvidenceRefs[i] != ref || hit.Source == "" ||
			!utf8.ValidString(hit.Text) || strings.ContainsRune(hit.Text, '\x00') || strings.TrimSpace(hit.Text) == "" || len(hit.Text) > 768 {
			return ErrStepConflict
		}
	}
	return nil
}

// CheckPolicyBundleStep enforces the prerequisite at admission and commit.
func CheckPolicyBundleStep(p Profile, snapshot SnapshotBinding, prior []Step, kind string) error {
	if !p.PolicyBundleEnabled() {
		if kind == "read_policy_bundle" || slices.ContainsFunc(prior, func(s Step) bool { return s.Kind == "read_policy_bundle" }) {
			return ErrStepConflict
		}
		return nil
	}
	if kind == "read_ticket" {
		if len(prior) != 0 {
			return ErrStepConflict
		}
		return nil
	}
	if len(prior) == 0 || prior[0].Kind != "read_ticket" {
		return ErrStepConflict
	}
	if kind == "read_policy_bundle" {
		if len(prior) != 1 {
			return ErrStepConflict
		}
		return nil
	}
	if len(prior) < 2 || prior[1].Kind != "read_policy_bundle" || slices.ContainsFunc(prior[2:], func(s Step) bool { return s.Kind == "read_policy_bundle" }) {
		return ErrStepConflict
	}
	result, _, err := CanonicalStepResultForStrategy(prior[1].Output, "read_policy_bundle", p.Strategy)
	if err != nil {
		return err
	}
	return validatePolicyBundle(snapshot, result)
}

func bundleToolDecision() json.RawMessage {
	return json.RawMessage(`{"type":"tool","name":"read_policy_bundle","arguments":{}}`)
}
