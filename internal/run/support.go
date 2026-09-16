package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

const (
	// SupportFixedStrategy registers only the ADR-0018 conditional read graph.
	SupportFixedStrategy = "support_fixed_v1"
	// SupportProposalSchema identifies the closed structured support contract.
	SupportProposalSchema = "support-proposal-v1"
)

// SupportSource binds one claim to an actual returned object and field.
type SupportSource struct {
	EvidenceRef   string `json:"evidence_ref"`
	SourcePointer string `json:"source_pointer"`
}

// SupportClaimFields contains the closed discriminated ADR-0018 claim variants.
// EventIDs is a pointer so order_delivery retains its required empty array.
type SupportClaimFields struct {
	Kind             string    `json:"kind"`
	Test             string    `json:"test,omitempty"`
	EventID          string    `json:"event_id,omitempty"`
	Type             string    `json:"type,omitempty"`
	DeliveredEventID string    `json:"delivered_event_id,omitempty"`
	Status           string    `json:"status,omitempty"`
	Field            string    `json:"field,omitempty"`
	EventIDs         *[]string `json:"event_ids,omitempty"`
	RecoveryEventID  string    `json:"recovery_event_id,omitempty"`
	CorrectedEventID string    `json:"corrected_event_id,omitempty"`
	Mode             string    `json:"mode,omitempty"`
}

// SupportClaim persists source locations; model-supplied aliases never escape.
type SupportClaim struct {
	SupportClaimFields
	Refs []SupportSource `json:"refs"`
}

// SupportProposalFields augments only the new registered persisted proposal.
type SupportProposalFields struct {
	Conclusion         string         `json:"conclusion"`
	RequestedFields    []string       `json:"requested_fields"`
	TargetTicketStatus string         `json:"target_ticket_status"`
	Claims             []SupportClaim `json:"claims"`
}

type supportModelClaim struct {
	SupportClaimFields
	Refs []string `json:"refs"`
}

type supportModel struct {
	Decision           string              `json:"decision"`
	Action             string              `json:"action"`
	Conclusion         string              `json:"conclusion"`
	RequestedFields    []string            `json:"requested_fields"`
	TargetTicketStatus string              `json:"target_ticket_status"`
	Claims             []supportModelClaim `json:"claims"`
}

var supportRequestedFields = []string{"ticket.order_id", "order.delivery_id", "delivery.usable_tracking_events", "delivery.delivered_event", "ticket.problem_description"}

// CanonicalRegisteredStepResult checks the closed formats of already authorized
// checkpoints. The profile-specific Commit path still decides which is allowed.
func CanonicalRegisteredStepResult(data []byte, kind string) (StepResult, json.RawMessage, error) {
	var envelope struct {
		Proposal map[string]json.RawMessage `json:"proposal"`
	}
	if json.Unmarshal(data, &envelope) != nil {
		return StepResult{}, nil, ErrInvalidArgument
	}
	strategy := BoundedReadonlyStrategy
	if _, support := envelope.Proposal["conclusion"]; support {
		strategy = SupportFixedStrategy
	}
	return CanonicalStepResultForStrategy(data, kind, strategy)
}

// SupportProposalFromModel validates structure and provenance, never whether a
// policy actually supports the model's conclusion. Scoring owns that judgment.
func SupportProposalFromModel(snapshot SnapshotBinding, prior []Step, data []byte) (*Proposal, error) {
	var model supportModel
	if err := ValidateStepJSON(data, 16384); err != nil {
		return nil, err
	}
	if !exactSupportKeys(data, "decision", "action", "conclusion", "requested_fields", "target_ticket_status", "claims") ||
		strictSupportDecode(data, &model) != nil || !validSupportFields(model.Decision, model.Action, model.Conclusion, model.RequestedFields, model.TargetTicketStatus) {
		return nil, ErrModelProtocol
	}
	var raw struct {
		Claims []json.RawMessage `json:"claims"`
	}
	if json.Unmarshal(data, &raw) != nil || len(raw.Claims) < 1 || len(raw.Claims) > 4 {
		return nil, ErrModelProtocol
	}
	sources, err := buildSupportSources(snapshot, prior)
	if err != nil {
		return nil, err
	}
	proposal := &Proposal{Decision: model.Decision, Action: model.Action, EvidenceRefs: []string{},
		SupportProposalFields: &SupportProposalFields{Conclusion: model.Conclusion, RequestedFields: model.RequestedFields,
			TargetTicketStatus: model.TargetTicketStatus, Claims: []SupportClaim{}}}
	seenClaims := map[string]bool{}
	for index, claim := range model.Claims {
		if !validSupportClaim(raw.Claims[index], claim.SupportClaimFields) || !uniqueStrings(claim.Refs, 1, 8) {
			return nil, ErrModelProtocol
		}
		expanded := SupportClaim{SupportClaimFields: claim.SupportClaimFields, Refs: []SupportSource{}}
		hasPolicy := false
		for _, alias := range claim.Refs {
			source, exists := sources.aliases[alias]
			if !exists {
				return nil, ErrModelProtocol
			}
			hasPolicy = hasPolicy || strings.HasPrefix(source.EvidenceRef, "business-policy:")
			expanded.Refs = appendUniqueSupportSource(expanded.Refs, source)
		}
		if !hasPolicy {
			return nil, ErrModelProtocol
		}
		for _, eventID := range supportEventIDs(claim.SupportClaimFields) {
			source, exists := sources.events[eventID]
			if !exists {
				return nil, ErrModelProtocol
			}
			expanded.Refs = appendUniqueSupportSource(expanded.Refs, source)
		}
		identity := supportClaimIdentity(expanded)
		if seenClaims[identity] {
			return nil, ErrModelProtocol
		}
		seenClaims[identity] = true
		for _, source := range expanded.Refs {
			if !slices.Contains(proposal.EvidenceRefs, source.EvidenceRef) {
				proposal.EvidenceRefs = append(proposal.EvidenceRefs, source.EvidenceRef)
			}
		}
		proposal.Claims = append(proposal.Claims, expanded)
	}
	proposal.Summary = renderSupportSummary(proposal)
	if !supportTargetMatchesAction(proposal, sources.ticketStatus) {
		return nil, ErrModelProtocol
	}
	encoded, err := json.Marshal(proposal)
	if err != nil || len(proposal.EvidenceRefs) > 32 {
		return nil, ErrModelProtocol
	}
	if len(encoded) > 16384 {
		return nil, ErrCheckpointTooLarge
	}
	return proposal, nil
}

func strictSupportDecode(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func exactSupportKeys(raw []byte, keys ...string) bool {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != len(keys) {
		return false
	}
	for _, key := range keys {
		value, exists := object[key]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

func uniqueStrings(values []string, minimum, maximum int) bool {
	if values == nil || len(values) < minimum || len(values) > maximum {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}

func validSupportFields(decision, action, conclusion string, requested []string, target string) bool {
	if (decision != "proposal" && decision != "no_action") || (decision == "no_action" && action != "") ||
		(decision == "proposal" && !slices.Contains([]string{"record_conclusion", "request_information", "escalate"}, action)) ||
		!slices.Contains([]string{"on_time", "delayed", "disputed", "insufficient", "conflicting"}, conclusion) ||
		!slices.Contains([]string{"open", "awaiting_information", "escalated", "informational_only"}, target) || !uniqueStrings(requested, 0, 5) {
		return false
	}
	for _, field := range requested {
		if !slices.Contains(supportRequestedFields, field) {
			return false
		}
	}
	return true
}

func validSupportClaim(raw []byte, claim SupportClaimFields) bool {
	keys := []string{"kind", "refs"}
	valid := false
	switch claim.Kind {
	case "timing":
		keys = append(keys, "test", "event_id")
		valid = slices.Contains([]string{"delivered_not_late", "delivered_late", "outstanding_not_overdue", "outstanding_overdue_lt48", "outstanding_overdue_ge48"}, claim.Test) && ValidIdentifier(claim.EventID)
	case "dispute":
		keys = append(keys, "type", "delivered_event_id")
		valid = slices.Contains([]string{"non_receipt", "wrong_address", "unauthorized_recipient", "unauthorized_safe_place"}, claim.Type) && ValidIdentifier(claim.DeliveredEventID)
	case "critical":
		keys = append(keys, "status", "event_id")
		valid = slices.Contains([]string{"lost", "damaged", "returned_to_sender"}, claim.Status) && ValidIdentifier(claim.EventID)
	case "missing":
		keys = append(keys, "field")
		valid = slices.Contains(supportRequestedFields, claim.Field)
	case "conflict":
		keys = append(keys, "type", "event_ids")
		valid = slices.Contains([]string{"pre_handover", "same_time", "source_key", "post_delivery", "order_delivery"}, claim.Type) && claim.EventIDs != nil
		if valid {
			count := 2
			if claim.Type == "order_delivery" {
				count = 0
			}
			valid = uniqueStrings(*claim.EventIDs, count, count)
			for _, id := range *claim.EventIDs {
				valid = valid && ValidIdentifier(id)
			}
		}
	case "correction":
		keys = append(keys, "recovery_event_id", "corrected_event_id")
		valid = ValidIdentifier(claim.RecoveryEventID) && ValidIdentifier(claim.CorrectedEventID) && claim.RecoveryEventID != claim.CorrectedEventID
	case "ticket_status":
		keys = append(keys, "mode")
		valid = slices.Contains([]string{"informational_no_action", "preserve_escalated"}, claim.Mode)
	}
	return valid && exactSupportKeys(raw, keys...)
}

func supportEventIDs(claim SupportClaimFields) []string {
	switch claim.Kind {
	case "timing", "critical":
		return []string{claim.EventID}
	case "dispute":
		return []string{claim.DeliveredEventID}
	case "conflict":
		return *claim.EventIDs
	case "correction":
		return []string{claim.RecoveryEventID, claim.CorrectedEventID}
	default:
		return nil
	}
}

func appendUniqueSupportSource(refs []SupportSource, source SupportSource) []SupportSource {
	if !slices.Contains(refs, source) {
		return append(refs, source)
	}
	return refs
}

func supportClaimIdentity(claim SupportClaim) string {
	// References identify sources, so reordering the same set cannot conceal a
	// duplicate claim. Field/claim order itself remains unchanged in persistence.
	claim.Refs = slices.Clone(claim.Refs)
	slices.SortFunc(claim.Refs, func(a, b SupportSource) int {
		return strings.Compare(a.EvidenceRef+"#"+a.SourcePointer, b.EvidenceRef+"#"+b.SourcePointer)
	})
	raw, _ := json.Marshal(claim)
	return string(raw)
}

func supportTargetMatchesAction(p *Proposal, current string) bool {
	switch p.Action {
	case "request_information":
		return p.TargetTicketStatus == "awaiting_information"
	case "escalate":
		return p.TargetTicketStatus == "escalated"
	default:
		return p.TargetTicketStatus == current
	}
}

func renderSupportSummary(p *Proposal) string {
	fields := "none"
	if len(p.RequestedFields) != 0 {
		fields = strings.Join(p.RequestedFields, ", ")
	}
	action := p.Action
	if p.Decision == "no_action" {
		action = "no_action"
	}
	lines := []string{fmt.Sprintf("Recommendation: %s. Conclusion asserted: %s. Suggested ticket status: %s. Requested fields: %s.", action, p.Conclusion, p.TargetTicketStatus, fields)}
	for _, claim := range p.Claims {
		description := ""
		switch claim.Kind {
		case "timing":
			description = fmt.Sprintf("timing %s; event %s", claim.Test, claim.EventID)
		case "dispute":
			description = fmt.Sprintf("dispute %s; delivered event %s", claim.Type, claim.DeliveredEventID)
		case "critical":
			description = fmt.Sprintf("critical %s; event %s", claim.Status, claim.EventID)
		case "missing":
			description = "missing " + claim.Field
		case "conflict":
			description = fmt.Sprintf("conflict %s; events [%s]", claim.Type, strings.Join(*claim.EventIDs, ", "))
		case "correction":
			description = fmt.Sprintf("correction; recovery event %s; corrected event %s", claim.RecoveryEventID, claim.CorrectedEventID)
		case "ticket_status":
			description = "ticket_status " + claim.Mode
		}
		lines = append(lines, "Claim asserted: "+description+".")
	}
	return strings.Join(lines, "\n")
}
