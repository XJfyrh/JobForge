package run

import (
	"bytes"
	"encoding/json"
	"slices"
)

// Navigation metadata binds a read, never the truth of a business condition.
var supplementTopics = map[string]struct {
	aliases []string
	query   string
}{
	"timing":              {[]string{"P02.1", "P02.2"}, "Compare delivery timestamp or observation time with promised_delivery_at in UTC; on_time delayed fulfillment"},
	"extended_delay":      {[]string{"P07.1", "P07.2"}, "Outstanding undelivered order at least 48 hours after promise escalation threshold; completed late delivery"},
	"missing_facts":       {[]string{"P03.1", "P03.2"}, "Missing related order delivery tracking event or vague complaint needing specific problem description clarification"},
	"delivery_dispute":    {[]string{"P04.1", "P04.2"}, "Delivered but customer disputes receipt wrong address unauthorized recipient safe place"},
	"structured_conflict": {[]string{"P05.1"}, "Authoritative structured tracking contradiction same timestamp source key pre handover post delivery conflict"},
	"critical_exception":  {[]string{"P06.1"}, "Active lost damaged returned_to_sender critical carrier exception escalation"},
	"carrier_correction":  {[]string{"P05.2", "P06.2"}, "Explicit carrier correction identifies erroneous event and supersedes it; ordinary later scans do not correct"},
	"ticket_status":       {[]string{"P08.1", "P08.2", "P09.1"}, "Already informational_only ticket action none; already escalated preserve status no silent downgrade"},
}

type policySupplement struct {
	Type            string   `json:"type"`
	Topic           string   `json:"topic"`
	RequiredAliases []string `json:"required_aliases"`
}

// PolicySupplementEnabled never grants the new edge to a historical profile.
func (p Profile) PolicySupplementEnabled() bool {
	return (p.ExecutorVersion == SupportAdjudicationV3ExecutorVersion || p.ExecutorVersion == SupportAdjudicationV4ExecutorVersion) && p.FinalAdjudicationEnabled()
}

func parsePolicySupplement(raw []byte) (policySupplement, error) {
	var intent policySupplement
	if ValidateStepJSON(raw, 16384) != nil || !exactSupportKeys(raw, "type", "topic", "required_aliases") ||
		strictSupportDecode(raw, &intent) != nil || intent.Type != "policy_supplement" {
		return intent, ErrModelProtocol
	}
	topic, ok := supplementTopics[intent.Topic]
	if !ok || len(intent.RequiredAliases) == 0 || len(intent.RequiredAliases) > len(topic.aliases) || !slices.IsSorted(intent.RequiredAliases) {
		return intent, ErrModelProtocol
	}
	for i, alias := range intent.RequiredAliases {
		if !slices.Contains(topic.aliases, alias) || slices.Contains(intent.RequiredAliases[:i], alias) {
			return intent, ErrModelProtocol
		}
	}
	return intent, nil
}

func supplementTool(intent policySupplement) json.RawMessage {
	args, _ := json.Marshal(map[string]string{"query": supplementTopics[intent.Topic].query})
	tool, _ := json.Marshal(SupportToolDecision{Type: "tool", Name: "search_policy", Arguments: args})
	return tool
}

func checkPolicySupplement(snapshot SnapshotBinding, prior []Step, raw []byte) error {
	intent, err := parsePolicySupplement(raw)
	if err != nil {
		return err
	}
	sources, err := buildSupportSourcesWithSearch(snapshot, prior, true)
	if err != nil {
		return err
	}
	for _, alias := range intent.RequiredAliases {
		if _, present := sources.aliases[alias]; present {
			return ErrModelProtocol
		}
	}
	current := supplementTool(intent)
	if intent.Topic == "ticket_status" && statusPolicyRead(prior) {
		return ErrModelProtocol
	}
	for _, step := range prior {
		if !IsModelStep(step.Kind) {
			continue
		}
		result, _, err := CanonicalStepResultForStrategy(step.Output, step.Kind, SupportAgentStrategy)
		if err != nil || step.Kind == "model_proposal" {
			return ErrModelProtocol
		}
		if result.CorrectionRequired || ValidInvestigationComplete(result.Content) {
			continue
		}
		_, previous, err := SupportAgentToolDecision(snapshot, result.Content)
		if err != nil || bytes.Equal(previous, current) {
			return ErrModelProtocol
		}
	}
	return nil
}

func pendingAgentToolForProfile(profile Profile, snapshot SnapshotBinding, prior []Step, kind string) (json.RawMessage, error) {
	if kind == "read_policy_bundle" {
		if !profile.PolicyBundleEnabled() {
			return nil, ErrStepConflict
		}
		if err := CheckPolicyBundleStep(profile, snapshot, prior, kind); err != nil {
			return nil, err
		}
		return bundleToolDecision(), nil
	}
	if profile.StatusPolicyEnabled() && len(prior) > 0 && investigationMarker(prior[len(prior)-1]) {
		if kind != "search_policy" {
			return nil, ErrStepConflict
		}
		return pendingStatusPolicy(snapshot, prior)
	}
	if profile.PolicySupplementEnabled() && len(prior) > 0 && prior[len(prior)-1].Kind == "model_proposal" {
		last := prior[len(prior)-1]
		result, _, err := CanonicalStepResultForStrategy(last.Output, last.Kind, SupportAgentStrategy)
		if err != nil || kind != "search_policy" || result.Proposal != nil || result.CorrectionRequired {
			return nil, ErrStepConflict
		}
		if err := checkPolicySupplement(snapshot, prior[:len(prior)-1], result.Content); err != nil {
			return nil, err
		}
		intent, _ := parsePolicySupplement(result.Content)
		return supplementTool(intent), nil
	}
	return pendingSupportAgentTool(snapshot, prior, kind)
}

// CheckSupportAgentToolForProfile runs at tool admission under the Run lock.
func CheckSupportAgentToolForProfile(profile Profile, snapshot SnapshotBinding, prior []Step, kind string) error {
	if err := CheckPolicyBundleStep(profile, snapshot, prior, kind); err != nil {
		return err
	}
	if kind == "read_policy_bundle" {
		return nil
	}
	if len(prior) > 0 && (profile.PolicySupplementEnabled() && prior[len(prior)-1].Kind == "model_proposal" || profile.StatusPolicyEnabled() && investigationMarker(prior[len(prior)-1])) {
		_, err := pendingAgentToolForProfile(profile, snapshot, prior, kind)
		return err
	}
	return CheckSupportAgentTool(snapshot, prior, kind)
}

func supplementSatisfied(snapshot SnapshotBinding, prior []Step, raw []byte) error {
	intent, err := parsePolicySupplement(raw)
	if err != nil {
		return err
	}
	sources, err := buildSupportSourcesWithSearch(snapshot, prior, true)
	if err != nil {
		return err
	}
	for _, alias := range intent.RequiredAliases {
		if _, present := sources.aliases[alias]; !present {
			return ErrModelProtocol
		}
	}
	return nil
}

func adjudicationPredecessor(profile Profile, snapshot SnapshotBinding, prior []Step) error {
	if len(prior) == 0 {
		return ErrStepConflict
	}
	if profile.StatusPolicyEnabled() {
		missing, err := missingStatusPolicy(snapshot, prior)
		if err != nil {
			return err
		}
		if missing != nil {
			return ErrModelProtocol
		}
		if len(prior) >= 2 && investigationMarker(prior[len(prior)-2]) {
			return statusPolicySatisfied(snapshot, prior)
		}
	}
	last := prior[len(prior)-1]
	previous, _, err := CanonicalStepResultForStrategy(last.Output, last.Kind, SupportAgentStrategy)
	if err != nil || previous.Proposal != nil || previous.CorrectionRequired {
		return ErrStepConflict
	}
	if (last.Kind == "model_decision" || last.Kind == "protocol_correction") && ValidInvestigationComplete(previous.Content) {
		return nil
	}
	if !profile.PolicySupplementEnabled() || last.Kind != "search_policy" || len(prior) < 2 {
		return ErrStepConflict
	}
	if _, err := pendingAgentToolForProfile(profile, snapshot, prior[:len(prior)-1], last.Kind); err != nil {
		return err
	}
	intent, _, err := CanonicalStepResultForStrategy(prior[len(prior)-2].Output, "model_proposal", SupportAgentStrategy)
	if err != nil {
		return err
	}
	return supplementSatisfied(snapshot, prior, intent.Content)
}
