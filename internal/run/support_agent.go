package run

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
)

const (
	// SupportAgentStrategy registers the bounded, server-validated read loop.
	SupportAgentStrategy = "support_agent_v1"
	// SupportAgentExecutorVersion prevents old executors accepting new decisions.
	SupportAgentExecutorVersion = "linux-v2-agent-runtime-1"
	// SupportAgentDecisionSchema identifies the closed tool/final model union.
	SupportAgentDecisionSchema = "support-agent-decision-v1"
	// SupportAgentPromptVersion binds the bounded agent's instructions.
	SupportAgentPromptVersion = "support-agent-prompt-v1"
)

// IsSupportStrategy selects only the two statically registered support programs.
func IsSupportStrategy(strategy string) bool {
	return strategy == SupportFixedStrategy || strategy == SupportAgentStrategy
}

// IsModelStep identifies audited chat steps, including the single correction.
func IsModelStep(kind string) bool {
	return kind == "model_proposal" || kind == "model_decision" || kind == "protocol_correction"
}

// SupportToolDecision records intent, never permission to dispatch a tool.
type SupportToolDecision struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// SupportAgentToolDecision validates a closed decision against the captured
// object identity. Canonical query whitespace also fixes repeated-call identity.
func SupportAgentToolDecision(snapshot SnapshotBinding, data []byte) (SupportToolDecision, json.RawMessage, error) {
	var decision SupportToolDecision
	if ValidateStepJSON(data, 16384) != nil || !exactSupportKeys(data, "type", "name", "arguments") ||
		strictSupportDecode(data, &decision) != nil || decision.Type != "tool" || len(ToolSequence(decision.Name)) == 0 {
		return decision, nil, ErrModelProtocol
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(decision.Arguments, &args) != nil || len(args) != 1 {
		return decision, nil, ErrModelProtocol
	}
	if decision.Name == "search_policy" {
		var query string
		if args["query"] == nil || json.Unmarshal(args["query"], &query) != nil || len(query) > 512 {
			return decision, nil, ErrModelProtocol
		}
		query = strings.Trim(query, " \t\r\n")
		if query == "" {
			return decision, nil, ErrModelProtocol
		}
		decision.Arguments, _ = json.Marshal(map[string]string{"query": query})
	} else {
		ticket, _, err := supportSnapshot(snapshot)
		var orderID *string
		if err != nil || args["order_id"] == nil || json.Unmarshal(args["order_id"], &orderID) != nil || !sameSupportID(orderID, ticket.OrderID) {
			return decision, nil, ErrModelProtocol
		}
		decision.Arguments, _ = json.Marshal(map[string]*string{"order_id": orderID})
	}
	canonical, err := json.Marshal(decision)
	return decision, canonical, err
}

func pendingSupportAgentTool(snapshot SnapshotBinding, prior []Step, kind string) (json.RawMessage, error) {
	if len(prior) == 0 {
		return nil, ErrStepConflict
	}
	last := prior[len(prior)-1]
	result, _, err := CanonicalStepResultForStrategy(last.Output, last.Kind, SupportAgentStrategy)
	if err != nil || (last.Kind != "model_decision" && last.Kind != "protocol_correction") ||
		result.Proposal != nil || result.CorrectionRequired {
		return nil, ErrStepConflict
	}
	decision, canonical, err := SupportAgentToolDecision(snapshot, result.Content)
	if err != nil || decision.Name != kind || !sameJSON(canonical, result.Content) {
		return nil, ErrStepConflict
	}
	return canonical, nil
}

// CheckSupportAgentTool runs under the Run lock before consuming a logical
// tool count. A committed model decision alone never grants send permission.
func CheckSupportAgentTool(snapshot SnapshotBinding, prior []Step, kind string) error {
	current, err := pendingSupportAgentTool(snapshot, prior, kind)
	if err != nil {
		return err
	}
	for _, step := range prior[:len(prior)-1] {
		if step.Kind != "model_decision" && step.Kind != "protocol_correction" {
			continue
		}
		result, _, err := CanonicalStepResultForStrategy(step.Output, step.Kind, SupportAgentStrategy)
		if err != nil {
			return ErrStepConflict
		}
		if result.Proposal != nil || result.CorrectionRequired {
			continue
		}
		_, previous, err := SupportAgentToolDecision(snapshot, result.Content)
		if err != nil {
			return ErrStepConflict
		}
		if bytes.Equal(current, previous) {
			return ErrModelProtocol
		}
	}
	return nil
}

func decideSupportAgentModel(snapshot SnapshotBinding, prior []Step, req CommitStepRequest, decision CommitDecision, allowed map[string]bool) (CommitDecision, error) {
	result, kind := decision.Result, req.Step.Kind
	if kind != "model_decision" && kind != "protocol_correction" && kind != "submit_proposal" {
		return decision, ErrStepConflict
	}
	if result.ToolInvocationID != "" || len(result.EvidenceRefs) != 0 {
		return decision, ErrStepConflict
	}
	nullContent := bytes.Equal(bytes.TrimSpace(result.Content), []byte("null"))
	if kind == "submit_proposal" {
		if result.PhysicalCallID != "" || result.CorrectionRequired || !nullContent || len(prior) == 0 {
			return decision, ErrStepConflict
		}
		last := prior[len(prior)-1]
		previous, _, err := CanonicalStepResultForStrategy(last.Output, last.Kind, SupportAgentStrategy)
		if err != nil || (last.Kind != "model_decision" && last.Kind != "protocol_correction") || previous.Proposal == nil ||
			!sameProposal(previous.Proposal, result.Proposal) {
			return decision, ErrStepConflict
		}
		decision.CloseAttempt = true
	} else if result.PhysicalCallID == "" {
		return decision, ErrStepConflict
	}
	if result.CorrectionRequired {
		if kind != "model_decision" || result.Proposal != nil || !nullContent || slices.ContainsFunc(prior, func(s Step) bool { return s.Kind == "protocol_correction" }) {
			return decision, ErrModelProtocol
		}
		decision.NextKind = "protocol_correction"
		return decision, nil
	}
	if !nullContent {
		tool, canonical, err := SupportAgentToolDecision(snapshot, result.Content)
		if err != nil || result.Proposal != nil || decision.CloseAttempt || !sameJSON(canonical, result.Content) {
			return decision, ErrModelProtocol
		}
		decision.NextKind = tool.Name
		return decision, nil
	}
	if !validProposal(result.Proposal, allowed) || validateSupportProposalWithSearch(snapshot, prior, result.Proposal, true) != nil {
		return decision, ErrModelProtocol
	}
	decision.Proposal = result.Proposal
	if !decision.CloseAttempt {
		decision.NextKind = "submit_proposal"
	}
	return decision, nil
}
