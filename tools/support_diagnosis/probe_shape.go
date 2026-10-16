package diagnosis

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"slices"
	"strings"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
	"github.com/xjfyrh/jobforge/internal/run"
)

// This descriptor is generated from the unchanged Python contract. The parity
// test rejects drift; it contains no gold, business predicates or source facts.
//
//go:embed probe-contract.json
var probeContractJSON []byte

var probePolicyAlias = regexp.MustCompile(`^P(0[1-9]|10)\.[12]$`)

type probeContract struct {
	ModelFields     []string                       `json:"model_fields"`
	RequestedFields []string                       `json:"requested_fields"`
	Pointers        map[string][]string            `json:"pointers"`
	ClaimVariants   map[string]map[string][]string `json:"claim_variants"`
}

func probeObject(raw []byte, keys []string) (map[string]json.RawMessage, bool) {
	var fields map[string]json.RawMessage
	if jsonstrict.Decode(raw, &fields) != nil || len(fields) != len(keys) {
		return nil, false
	}
	for _, key := range keys {
		if fields[key] == nil || strings.TrimSpace(string(fields[key])) == "null" {
			return nil, false
		}
	}
	return fields, true
}

func probeStrings(raw []byte, minimum, maximum int) ([]string, bool) {
	var values []string
	if json.Unmarshal(raw, &values) != nil || values == nil || len(values) < minimum || len(values) > maximum {
		return nil, false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			return nil, false
		}
		seen[value] = true
	}
	return values, true
}

func probeProposalShape(raw []byte, currentStatus string) bool {
	var contract probeContract
	if json.Unmarshal(probeContractJSON, &contract) != nil {
		return false
	}
	fields, ok := probeObject(raw, contract.ModelFields)
	var p struct {
		Decision string            `json:"decision"`
		Action   string            `json:"action"`
		Finding  string            `json:"conclusion"`
		Target   string            `json:"target_ticket_status"`
		Claims   []json.RawMessage `json:"claims"`
	}
	if !ok || json.Unmarshal(raw, &p) != nil {
		return false
	}
	validDecision := p.Decision == "no_action" && p.Action == "" ||
		p.Decision == "proposal" && slices.Contains([]string{"record_conclusion", "request_information", "escalate"}, p.Action)
	// Decode the scalar fields separately because requested_fields has its own
	// bounds, while the closed field set is checked above.
	if !validDecision ||
		!slices.Contains([]string{"on_time", "delayed", "disputed", "insufficient", "conflicting"}, p.Finding) ||
		!slices.Contains([]string{"open", "awaiting_information", "escalated", "informational_only"}, p.Target) ||
		len(p.Claims) < 1 || len(p.Claims) > 4 {
		return false
	}
	requested, ok := probeStrings(fields["requested_fields"], 0, 5)
	if !ok {
		return false
	}
	for _, field := range requested {
		if !slices.Contains(contract.RequestedFields, field) {
			return false
		}
	}
	target := currentStatus
	switch p.Action {
	case "request_information":
		target = "awaiting_information"
	case "escalate":
		target = "escalated"
	}
	if p.Target != target {
		return false
	}
	seen := map[string]bool{}
	for _, claim := range p.Claims {
		identity, valid := probeClaimShape(claim, contract)
		if !valid || seen[identity] {
			return false
		}
		seen[identity] = true
	}
	return true
}

func probeClaimShape(raw []byte, contract probeContract) (string, bool) {
	var header struct {
		Kind string `json:"kind"`
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &header) != nil || contract.ClaimVariants[header.Kind] == nil {
		return "", false
	}
	variant := contract.ClaimVariants[header.Kind]
	keys := []string{"kind", "refs"}
	for key := range variant {
		keys = append(keys, key)
	}
	fields, ok := probeObject(raw, keys)
	if !ok {
		return "", false
	}
	ids := map[string]bool{}
	for key, allowed := range variant {
		if key == "event_ids" {
			count := 2
			if header.Type == "order_delivery" {
				count = 0
			}
			events, valid := probeStrings(fields[key], count, count)
			if !valid {
				return "", false
			}
			for _, id := range events {
				if !run.ValidIdentifier(id) {
					return "", false
				}
			}
			continue
		}
		var value string
		if json.Unmarshal(fields[key], &value) != nil || allowed != nil && !slices.Contains(allowed, value) ||
			allowed == nil && (!run.ValidIdentifier(value) || ids[value]) {
			return "", false
		}
		if allowed == nil {
			ids[value] = true
		}
	}
	refs, ok := probeStrings(fields["refs"], 1, 8)
	if !ok {
		return "", false
	}
	for _, ref := range refs {
		prefix, pointer, _ := strings.Cut(ref, "#")
		if !probePolicyAlias.MatchString(ref) && !slices.Contains(contract.Pointers[prefix], pointer) {
			return "", false
		}
	}
	slices.Sort(refs)
	var semantic map[string]any
	if json.Unmarshal(raw, &semantic) != nil {
		return "", false
	}
	semantic["refs"] = refs
	identity, err := json.Marshal(semantic)
	return string(identity), err == nil
}

func probeFinalShape(request, response []byte) bool {
	var r chatRequest
	var e envelope
	if json.Unmarshal(request, &r) != nil || len(r.Messages) != 2 || json.Unmarshal(response, &e) != nil ||
		len(e.Choices) != 1 || e.Choices[0].Message.Content == nil {
		return false
	}
	var facts struct {
		Ticket struct {
			Status string `json:"status"`
		} `json:"T"`
	}
	if json.Unmarshal([]byte(r.Messages[1].Content), &facts) != nil {
		return false
	}
	var final struct {
		Proposal json.RawMessage `json:"proposal"`
	}
	if json.Unmarshal([]byte(*e.Choices[0].Message.Content), &final) != nil {
		return false
	}
	return probeProposalShape(final.Proposal, facts.Ticket.Status)
}
