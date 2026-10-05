package business

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/xjfyrh/jobforge/internal/jsonstrict"
)

var actionRequestedFields = []string{"ticket.order_id", "order.delivery_id", "delivery.usable_tracking_events", "delivery.delivered_event", "ticket.problem_description"}

func uniqueActionStrings(values []string) bool {
	seen := make(map[string]bool)
	for _, value := range values {
		if value == "" || seen[value] {
			return false
		}
		seen[value] = true
	}
	return values != nil
}

// This is the frozen persisted support-v1 shape, never a semantic scorer.
// Authorization reconstructs it from the original server-validated proposal.
func validResolutionClaims(p ResolutionParameters) bool {
	if !uniqueActionStrings(p.RequestedFields) || !uniqueActionStrings(p.EvidenceRefs) {
		return false
	}
	for _, field := range p.RequestedFields {
		if !slices.Contains(actionRequestedFields, field) {
			return false
		}
	}
	for _, ref := range p.EvidenceRefs {
		if !validText(ref, 256) {
			return false
		}
	}
	var claims []map[string]json.RawMessage
	if jsonstrict.Decode(p.Claims, &claims) != nil || len(claims) < 1 || len(claims) > 4 {
		return false
	}
	seenClaims := make(map[string]bool)
	for _, claim := range claims {
		text := func(name string) string {
			var value string
			if json.Unmarshal(claim[name], &value) != nil {
				return ""
			}
			return value
		}
		keys := []string{"kind", "refs"}
		valid := false
		switch text("kind") {
		case "timing":
			keys = append(keys, "test", "event_id")
			valid = validID(text("event_id")) && slices.Contains([]string{"delivered_not_late", "delivered_late", "outstanding_not_overdue", "outstanding_overdue_lt48", "outstanding_overdue_ge48"}, text("test"))
		case "dispute":
			keys = append(keys, "type", "delivered_event_id")
			valid = validID(text("delivered_event_id")) && slices.Contains([]string{"non_receipt", "wrong_address", "unauthorized_recipient", "unauthorized_safe_place"}, text("type"))
		case "critical":
			keys = append(keys, "status", "event_id")
			valid = validID(text("event_id")) && slices.Contains([]string{"lost", "damaged", "returned_to_sender"}, text("status"))
		case "missing":
			keys = append(keys, "field")
			valid = slices.Contains(actionRequestedFields, text("field"))
		case "conflict":
			keys = append(keys, "type", "event_ids")
			var ids []string
			count := 2
			if text("type") == "order_delivery" {
				count = 0
			}
			valid = jsonstrict.Decode(claim["event_ids"], &ids) == nil && uniqueActionStrings(ids) && len(ids) == count && slices.Contains([]string{"pre_handover", "same_time", "source_key", "post_delivery", "order_delivery"}, text("type"))
			for _, id := range ids {
				valid = valid && validID(id)
			}
		case "correction":
			keys = append(keys, "recovery_event_id", "corrected_event_id")
			valid = validID(text("recovery_event_id")) && validID(text("corrected_event_id")) && text("recovery_event_id") != text("corrected_event_id")
		case "ticket_status":
			keys = append(keys, "mode")
			valid = slices.Contains([]string{"informational_no_action", "preserve_escalated"}, text("mode"))
		}
		if !valid || len(claim) != len(keys) {
			return false
		}
		for _, key := range keys {
			if raw, ok := claim[key]; !ok || strings.TrimSpace(string(raw)) == "null" {
				return false
			}
		}
		var refs []struct {
			EvidenceRef string `json:"evidence_ref"`
			Pointer     string `json:"source_pointer"`
		}
		if jsonstrict.Decode(claim["refs"], &refs) != nil || len(refs) < 1 || len(refs) > 10 {
			return false
		}
		seen := make(map[string]bool)
		for _, ref := range refs {
			key := ref.EvidenceRef + "#" + ref.Pointer
			if !slices.Contains(p.EvidenceRefs, ref.EvidenceRef) || !validText(ref.Pointer, 128) || ref.Pointer == "" || seen[key] {
				return false
			}
			seen[key] = true
		}
		canonical, _ := actionJSON(claim)
		if seenClaims[string(canonical)] {
			return false
		}
		seenClaims[string(canonical)] = true
	}
	return true
}
