package run

import "encoding/json"

// StatusPolicyEnabled binds read prerequisites to the new frozen profile only.
func (p Profile) StatusPolicyEnabled() bool {
	return p.ExecutorVersion == SupportAdjudicationV4ExecutorVersion && p.FinalAdjudicationEnabled()
}

// CheckStatusPolicyAdjudication validates the prerequisite before a new paid call.
func CheckStatusPolicyAdjudication(p Profile, snapshot SnapshotBinding, prior []Step) error {
	if !p.StatusPolicyEnabled() {
		return nil
	}
	return adjudicationPredecessor(p, snapshot, prior)
}

func missingStatusPolicy(snapshot SnapshotBinding, prior []Step) (*policySupplement, error) {
	var ticket struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(snapshot.Ticket, &ticket) != nil {
		return nil, ErrStepConflict
	}
	alias := map[string]string{"informational_only": "P08.1", "escalated": "P08.2"}[ticket.Status]
	if alias == "" {
		return nil, nil
	}
	sources, err := buildSupportSourcesWithSearch(snapshot, prior, true)
	if err != nil {
		return nil, err
	}
	if _, present := sources.aliases[alias]; present {
		return nil, nil
	}
	return &policySupplement{Type: "policy_supplement", Topic: "ticket_status", RequiredAliases: []string{alias}}, nil
}

func investigationMarker(step Step) bool {
	if step.Kind != "model_decision" && step.Kind != "protocol_correction" {
		return false
	}
	result, _, err := CanonicalStepResultForStrategy(step.Output, step.Kind, SupportAgentStrategy)
	return err == nil && result.Proposal == nil && !result.CorrectionRequired && ValidInvestigationComplete(result.Content)
}

func pendingStatusPolicy(snapshot SnapshotBinding, prior []Step) (json.RawMessage, error) {
	if len(prior) == 0 || !investigationMarker(prior[len(prior)-1]) {
		return nil, ErrStepConflict
	}
	intent, err := missingStatusPolicy(snapshot, prior)
	if err != nil {
		return nil, err
	}
	if intent == nil {
		return nil, ErrStepConflict
	}
	raw, _ := json.Marshal(intent)
	if err := checkPolicySupplement(snapshot, prior, raw); err != nil {
		return nil, err
	}
	return supplementTool(*intent), nil
}

func statusPolicySatisfied(snapshot SnapshotBinding, prior []Step) error {
	if len(prior) < 2 || prior[len(prior)-1].Kind != "search_policy" {
		return ErrStepConflict
	}
	if _, err := pendingStatusPolicy(snapshot, prior[:len(prior)-1]); err != nil {
		return err
	}
	intent, err := missingStatusPolicy(snapshot, prior)
	if err != nil {
		return err
	}
	if intent != nil {
		return ErrModelProtocol
	}
	return nil
}

func statusPolicyRead(prior []Step) bool {
	for i, step := range prior {
		if i > 0 && step.Kind == "search_policy" && investigationMarker(prior[i-1]) {
			return true
		}
	}
	return false
}
