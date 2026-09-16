package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/xjfyrh/jobforge/internal/business"
)

// These are field locations in actual captured objects, not expressions. Array
// indexes are deliberately absent: only a uniquely bound event ID creates one.
var supportPointers = map[string][]string{
	"T":  {"/order_id", "/status", "/subject", "/description", "/observed_at", "/policy_version"},
	"E1": {"/missing", "/missing_reason", "/order/order_id", "/order/delivery_id", "/order/status", "/order/ordered_at", "/order/promised_delivery_at"},
	"E2": {"/missing", "/missing_reason", "/delivery/delivery_id", "/delivery/order_id", "/delivery/status", "/delivery/events"},
}

type supportSources struct {
	aliases      map[string]SupportSource
	events       map[string]SupportSource
	ticketStatus string
}

func supportSnapshot(snapshot SnapshotBinding) (business.Ticket, business.VersionVector, error) {
	var ticket business.Ticket
	var vector business.VersionVector
	if !ValidUUID(snapshot.ID) || !ValidUUID(snapshot.IndexID) ||
		strictSupportDecode(snapshot.Ticket, &ticket) != nil || strictSupportDecode(snapshot.VersionVector, &vector) != nil ||
		ticket.TenantID != snapshot.TenantID || ticket.TicketID != snapshot.TicketID ||
		vector.SchemaVersion != 1 || vector.Ticket.ID != ticket.TicketID || vector.Ticket.Revision != ticket.Revision ||
		vector.Policy.Version != ticket.PolicyVersion || vector.Index.ID != snapshot.IndexID || vector.Index.ProfileHash != snapshot.IndexProfileHash ||
		!sameSupportID(vector.Order.ID, ticket.OrderID) {
		return ticket, vector, ErrStepConflict
	}
	return ticket, vector, nil
}

func sameSupportID(a, b *string) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// validateSupportTool verifies source identity, not delivery-policy semantics.
// For example, conflicting statuses on the bound order and delivery remain
// evidence; a different order identity, tenant or aggregate revision never does.
func validateSupportTool(snapshot SnapshotBinding, kind string, result StepResult) (bool, error) {
	ticket, vector, err := supportSnapshot(snapshot)
	if err != nil || !toolEvidence(snapshot, kind, result) {
		return false, ErrStepConflict
	}
	if kind == "search_policy" {
		var search struct {
			SnapshotID string               `json:"snapshot_id"`
			Matches    []business.PolicyHit `json:"matches"`
		}
		if strictSupportDecode(result.Content, &search) != nil || search.Matches == nil {
			return false, ErrStepConflict
		}
		seen := map[string]bool{}
		for _, hit := range search.Matches {
			if !validSupportPolicyAlias(hit.ChunkID) || hit.PolicyVersion != ticket.PolicyVersion || hit.Text == "" ||
				seen[hit.ChunkID] || hit.Source == "" {
				return false, ErrStepConflict
			}
			seen[hit.ChunkID] = true
		}
		return false, nil
	}
	var evidence business.Evidence
	var object map[string]json.RawMessage
	if strictSupportDecode(result.Content, &evidence) != nil || json.Unmarshal(result.Content, &object) != nil ||
		object["missing"] == nil || bytes.Equal(bytes.TrimSpace(object["missing"]), []byte("null")) || evidence.Ticket != nil {
		return false, ErrStepConflict
	}
	var expectedID *string
	var exists bool
	switch kind {
	case "get_order":
		expectedID, exists = vector.Order.ID, vector.Order.Exists
		if evidence.Delivery != nil || (!evidence.Missing && (evidence.Order == nil || !exists || vector.Order.Revision == nil || expectedID == nil ||
			evidence.Order.TenantID != snapshot.TenantID || evidence.Order.OrderID != *expectedID || evidence.Order.Revision != *vector.Order.Revision ||
			!sameSupportID(evidence.Order.DeliveryID, vector.Delivery.ID))) {
			return false, ErrStepConflict
		}
	case "get_delivery":
		expectedID, exists = vector.Delivery.ID, vector.Delivery.Exists
		if evidence.Order != nil || (!evidence.Missing && (evidence.Delivery == nil || !exists || vector.Delivery.AggregateRevision == nil || expectedID == nil ||
			evidence.Delivery.TenantID != snapshot.TenantID || evidence.Delivery.DeliveryID != *expectedID ||
			ticket.OrderID == nil || evidence.Delivery.OrderID != *ticket.OrderID ||
			evidence.Delivery.AggregateRevision != *vector.Delivery.AggregateRevision || evidence.Delivery.Events == nil)) {
			return false, ErrStepConflict
		}
		if evidence.Delivery != nil {
			seen := map[string]bool{}
			for _, event := range evidence.Delivery.Events {
				if !ValidIdentifier(event.EventID) || seen[event.EventID] {
					return false, ErrStepConflict
				}
				seen[event.EventID] = true
			}
		}
	default:
		return false, ErrStepConflict
	}
	if evidence.Missing {
		reason := "record_not_found"
		if expectedID == nil {
			reason = "not_associated"
		}
		if exists || evidence.Order != nil || evidence.Delivery != nil || evidence.MissingReason != reason {
			return false, ErrStepConflict
		}
	} else if evidence.MissingReason != "" {
		return false, ErrStepConflict
	}
	return evidence.Missing, nil
}

func validSupportPolicyAlias(value string) bool {
	return len(value) == 5 && value[0] == 'P' && value[3] == '.' &&
		value[4] >= '1' && value[4] <= '2' && (value[1] == '0' && value[2] >= '1' && value[2] <= '9' || value[1:3] == "10")
}

func buildSupportSources(snapshot SnapshotBinding, prior []Step) (supportSources, error) {
	sources := supportSources{aliases: map[string]SupportSource{}, events: map[string]SupportSource{}}
	ticket, _, err := supportSnapshot(snapshot)
	if err != nil {
		return sources, err
	}
	sources.ticketStatus = ticket.Status
	addSupportPointers(sources.aliases, "T", "business-evidence:"+snapshot.ID+":ticket", snapshot.Ticket)
	seenKinds := map[string]bool{}
	for _, step := range prior {
		if len(ToolSequence(step.Kind)) == 0 {
			continue
		}
		if seenKinds[step.Kind] {
			return sources, ErrStepConflict
		}
		seenKinds[step.Kind] = true
		result, _, err := CanonicalStepResultForStrategy(step.Output, step.Kind, SupportFixedStrategy)
		if err != nil {
			return sources, err
		}
		if _, err := validateSupportTool(snapshot, step.Kind, result); err != nil {
			return sources, err
		}
		switch step.Kind {
		case "get_order":
			addSupportPointers(sources.aliases, "E1", result.EvidenceRefs[0], result.Content)
		case "get_delivery":
			ref := result.EvidenceRefs[0]
			addSupportPointers(sources.aliases, "E2", ref, result.Content)
			var evidence business.Evidence
			if json.Unmarshal(result.Content, &evidence) != nil {
				return sources, ErrStepConflict
			}
			if evidence.Delivery != nil {
				for index, event := range evidence.Delivery.Events {
					sources.events[event.EventID] = SupportSource{EvidenceRef: ref, SourcePointer: fmt.Sprintf("/delivery/events/%d", index)}
				}
			}
		case "search_policy":
			var search struct {
				Matches []business.PolicyHit `json:"matches"`
			}
			if json.Unmarshal(result.Content, &search) != nil {
				return sources, ErrStepConflict
			}
			for _, hit := range search.Matches {
				sources.aliases[hit.ChunkID] = SupportSource{EvidenceRef: hit.EvidenceRef, SourcePointer: "/text"}
			}
		}
	}
	return sources, nil
}

func addSupportPointers(aliases map[string]SupportSource, prefix, ref string, raw []byte) {
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil {
		return
	}
	for _, pointer := range supportPointers[prefix] {
		var current any = object
		found := true
		for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
			parent, ok := current.(map[string]any)
			if !ok {
				found = false
				break
			}
			current, found = parent[part]
			if !found {
				break
			}
		}
		if found {
			aliases[prefix+"#"+pointer] = SupportSource{EvidenceRef: ref, SourcePointer: pointer}
		}
	}
}

func validateSupportProposalShape(raw []byte, proposal *Proposal) error {
	if proposal == nil || proposal.SupportProposalFields == nil ||
		!exactSupportKeys(raw, "decision", "action", "summary", "evidence_refs", "conclusion", "requested_fields", "target_ticket_status", "claims") ||
		!validSupportFields(proposal.Decision, proposal.Action, proposal.Conclusion, proposal.RequestedFields, proposal.TargetTicketStatus) {
		return ErrModelProtocol
	}
	var shape struct {
		Claims []json.RawMessage `json:"claims"`
	}
	if json.Unmarshal(raw, &shape) != nil || len(shape.Claims) < 1 || len(shape.Claims) > 4 {
		return ErrModelProtocol
	}
	seen := map[string]bool{}
	for index, claim := range proposal.Claims {
		if !validSupportClaim(shape.Claims[index], claim.SupportClaimFields) || claim.Refs == nil || len(claim.Refs) < 1 || len(claim.Refs) > 10 {
			return ErrModelProtocol
		}
		var rawClaim struct {
			Refs []json.RawMessage `json:"refs"`
		}
		if json.Unmarshal(shape.Claims[index], &rawClaim) != nil {
			return ErrModelProtocol
		}
		for index, source := range claim.Refs {
			if !exactSupportKeys(rawClaim.Refs[index], "evidence_ref", "source_pointer") || source.EvidenceRef == "" || source.SourcePointer == "" ||
				slices.Contains(claim.Refs[:index], source) {
				return ErrModelProtocol
			}
		}
		identity := supportClaimIdentity(claim)
		if seen[identity] {
			return ErrModelProtocol
		}
		seen[identity] = true
	}
	return nil
}

func validateSupportProposal(snapshot SnapshotBinding, prior []Step, proposal *Proposal) error {
	sources, err := buildSupportSources(snapshot, prior)
	if err != nil || proposal == nil || proposal.SupportProposalFields == nil {
		return ErrModelProtocol
	}
	reverse := map[SupportSource]string{}
	for alias, source := range sources.aliases {
		reverse[source] = alias
	}
	model := supportModel{Decision: proposal.Decision, Action: proposal.Action, Conclusion: proposal.Conclusion,
		RequestedFields: proposal.RequestedFields, TargetTicketStatus: proposal.TargetTicketStatus, Claims: []supportModelClaim{}}
	for _, claim := range proposal.Claims {
		modelClaim := supportModelClaim{SupportClaimFields: claim.SupportClaimFields, Refs: []string{}}
		eventSources := []SupportSource{}
		for _, id := range supportEventIDs(claim.SupportClaimFields) {
			source, exists := sources.events[id]
			if !exists {
				return ErrModelProtocol
			}
			eventSources = append(eventSources, source)
		}
		for _, source := range claim.Refs {
			if alias, exists := reverse[source]; exists {
				modelClaim.Refs = append(modelClaim.Refs, alias)
			} else if !slices.Contains(eventSources, source) {
				return ErrModelProtocol
			}
		}
		model.Claims = append(model.Claims, modelClaim)
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return ErrModelProtocol
	}
	expected, err := SupportProposalFromModel(snapshot, prior, raw)
	if err != nil || !sameProposal(expected, proposal) {
		return ErrModelProtocol
	}
	return nil
}
