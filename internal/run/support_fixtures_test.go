package run

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These synthetic source cases are protocol fixtures, not business answers.
// Python consumes the serialized inputs and exact persisted rendering directly.
type supportFixtureSnapshot struct {
	TenantID          string          `json:"tenant_id"`
	TicketID          string          `json:"ticket_id"`
	SnapshotID        string          `json:"snapshot_id"`
	SnapshotHash      string          `json:"snapshot_hash"`
	VersionVectorJSON json.RawMessage `json:"version_vector_json"`
	TicketBindingJSON json.RawMessage `json:"ticket_binding_json"`
	IndexID           string          `json:"index_id"`
	IndexProfileHash  string          `json:"index_profile_hash"`
}

func (s supportFixtureSnapshot) binding() SnapshotBinding {
	return SnapshotBinding{TenantID: s.TenantID, TicketID: s.TicketID, ID: s.SnapshotID, ContentHash: s.SnapshotHash,
		VersionVector: s.VersionVectorJSON, Ticket: s.TicketBindingJSON, IndexID: s.IndexID, IndexProfileHash: s.IndexProfileHash}
}

type supportFixtureStep struct {
	Kind       string          `json:"kind"`
	ResultJSON json.RawMessage `json:"result_json"`
}

type supportVector struct {
	Name     string                 `json:"name"`
	Snapshot supportFixtureSnapshot `json:"snapshot"`
	Steps    []supportFixtureStep   `json:"steps"`
	Model    string                 `json:"model_json"`
	Expected *Proposal              `json:"expected"`
}

func (v supportVector) prior() []Step {
	prior := make([]Step, 0, len(v.Steps))
	for _, step := range v.Steps {
		prior = append(prior, Step{Kind: step.Kind, Output: step.ResultJSON})
	}
	return prior
}

type supportFixtureFile struct {
	SchemaVersion  int             `json:"schema_version"`
	Strategy       string          `json:"strategy"`
	ProposalSchema string          `json:"proposal_schema"`
	Valid          []supportVector `json:"valid"`
	Invalid        []supportVector `json:"invalid"`
}

func supportVectorInput(t *testing.T, name, model string, withOrder bool) supportVector {
	t.Helper()
	snapshot, prior := supportFixture(t, withOrder)
	v := supportVector{Name: name, Model: model, Snapshot: supportFixtureSnapshot{TenantID: snapshot.TenantID, TicketID: snapshot.TicketID,
		SnapshotID: snapshot.ID, SnapshotHash: snapshot.ContentHash, VersionVectorJSON: snapshot.VersionVector, TicketBindingJSON: snapshot.Ticket,
		IndexID: snapshot.IndexID, IndexProfileHash: snapshot.IndexProfileHash}, Steps: []supportFixtureStep{}}
	for _, step := range prior {
		v.Steps = append(v.Steps, supportFixtureStep{Kind: step.Kind, ResultJSON: step.Output})
	}
	return v
}

func makeSupportFixtures(t *testing.T) supportFixtureFile {
	t.Helper()
	f := supportFixtureFile{SchemaVersion: 1, Strategy: SupportFixedStrategy, ProposalSchema: SupportProposalSchema,
		Valid: []supportVector{}, Invalid: []supportVector{}}
	for _, example := range supportClaimExamples {
		f.Valid = append(f.Valid, supportVectorInput(t, example.name, supportModelJSON(example.claim), true))
	}
	missing := `{"kind":"missing","field":"ticket.order_id","refs":["T#/order_id","E1#/missing_reason","P01.1"]}`
	model := supportModelJSON(missing)
	model = strings.Replace(model, `"action":"escalate"`, `"action":"request_information"`, 1)
	model = strings.Replace(model, `"requested_fields":[]`, `"requested_fields":["ticket.order_id"]`, 1)
	model = strings.Replace(model, `"target_ticket_status":"escalated"`, `"target_ticket_status":"awaiting_information"`, 1)
	f.Valid = append(f.Valid, supportVectorInput(t, "missing_order", model, false))
	model = `{"decision":"no_action","action":"","conclusion":"insufficient","requested_fields":[],"target_ticket_status":"open","claims":[{"kind":"ticket_status","mode":"informational_no_action","refs":["T#/status","P01.1"]}]}`
	f.Valid = append(f.Valid, supportVectorInput(t, "no_action", model, false))
	for index := range f.Valid {
		v := &f.Valid[index]
		expected, err := SupportProposalFromModel(v.Snapshot.binding(), v.prior(), []byte(v.Model))
		if err != nil {
			t.Fatalf("valid fixture %s: %v", v.Name, err)
		}
		v.Expected = expected
	}
	base := supportModelJSON(supportClaimExamples[0].claim)
	for _, change := range []struct{ name, from, to string }{
		{"unknown_top_field", `"decision":`, `"url":"https://invalid.example","decision":`},
		{"free_summary", `"decision":`, `"summary":"Already refunded","decision":`},
		{"duplicate_key", `"decision":"proposal"`, `"decision":"proposal","decision":"proposal"`},
		{"alias_key", `"decision":`, `"Decision":`},
		{"missing_key", `"requested_fields":[],`, ``},
		{"null_decision", `"decision":"proposal"`, `"decision":null`},
		{"null_fields", `"requested_fields":[]`, `"requested_fields":null`},
		{"null_array_item", `"requested_fields":[]`, `"requested_fields":[null]`},
		{"unknown_requested_field", `"requested_fields":[]`, `"requested_fields":["tenant_id"]`},
		{"duplicate_requested_field", `"requested_fields":[]`, `"requested_fields":["ticket.order_id","ticket.order_id"]`},
		{"unknown_action", `"action":"escalate"`, `"action":"refund"`},
		{"no_action_write", `"decision":"proposal"`, `"decision":"no_action"`},
		{"unknown_conclusion", `"conclusion":"insufficient"`, `"conclusion":"resolved"`},
		{"wrong_action_target", `"target_ticket_status":"escalated"`, `"target_ticket_status":"open"`},
		{"claim_extra_field", `"kind":"timing"`, `"kind":"timing","reason":"approved"`},
		{"claim_duplicate_key", `"kind":"timing"`, `"kind":"timing","kind":"timing"`},
		{"claim_alias_key", `"event_id":`, `"Event_ID":`},
		{"claim_null_scalar", `"event_id":"evt-second"`, `"event_id":null`},
		{"unknown_event", `"event_id":"evt-second"`, `"event_id":"not-returned"`},
		{"invalid_event_identifier", `"event_id":"evt-second"`, `"event_id":"event/path"`},
		{"unknown_test", `"test":"delivered_not_late"`, `"test":"arbitrary_expression"`},
		{"unreturned_policy", `"P01.1"`, `"P02.1"`},
		{"unknown_policy", `"P01.1"`, `"P11.1"`},
		{"missing_policy", `,"P01.1"`, ``},
		{"duplicate_reference", `"P01.1"`, `"P01.1","P01.1"`},
		{"source_pointer_not_allowed", `"T#/observed_at"`, `"T#/tenant_id"`},
		{"source_pointer_absent", `"T#/observed_at"`, `"E1#/missing_reason"`},
		{"raw_event_pointer", `"T#/observed_at"`, `"E2#/delivery/events/0"`},
		{"foreign_full_reference", `"P01.1"`, `"business-policy:other-index:P01.1"`},
		{"unpaired_surrogate", `"event_id":"evt-second"`, `"event_id":"\ud800"`},
		{"nul_escape", `"event_id":"evt-second"`, `"event_id":"\u0000"`},
	} {
		f.Invalid = append(f.Invalid, supportVectorInput(t, change.name, strings.Replace(base, change.from, change.to, 1), true))
	}
	for _, invalid := range []struct{ name, raw string }{
		{"trailing_json", base + `{}`},
		{"no_claims", strings.Replace(base, supportClaimExamples[0].claim, "", 1)},
		{"duplicate_claim", strings.Replace(base, supportClaimExamples[0].claim, supportClaimExamples[0].claim+","+supportClaimExamples[0].claim, 1)},
		{"too_many_claims", strings.Replace(base, supportClaimExamples[0].claim, strings.Repeat(supportClaimExamples[0].claim+",", 4)+supportClaimExamples[0].claim, 1)},
		{"conflict_single_event", supportModelJSON(`{"kind":"conflict","type":"same_time","event_ids":["evt-first"],"refs":["P01.1"]}`)},
		{"conflict_duplicate_event", supportModelJSON(`{"kind":"conflict","type":"same_time","event_ids":["evt-first","evt-first"],"refs":["P01.1"]}`)},
		{"order_conflict_has_event", supportModelJSON(`{"kind":"conflict","type":"order_delivery","event_ids":["evt-first"],"refs":["P01.1"]}`)},
		{"correction_same_event", supportModelJSON(`{"kind":"correction","recovery_event_id":"evt-first","corrected_event_id":"evt-first","refs":["P01.1"]}`)},
	} {
		f.Invalid = append(f.Invalid, supportVectorInput(t, invalid.name, invalid.raw, true))
	}
	// Bound sources are part of each shared vector, including negative inputs.
	for _, change := range []struct{ name, from, to string }{
		{"foreign_delivery_tenant", `"tenant_id":"tenant-synthetic"`, `"tenant_id":"other-tenant"`},
		{"ambiguous_event_id", `"event_id":"evt-second"`, `"event_id":"evt-first"`},
		{"wrong_delivery_revision", `"aggregate_revision":1`, `"aggregate_revision":2`},
		{"wrong_delivery_order", `"order_id":"order-synthetic"`, `"order_id":"order-other"`},
	} {
		v := supportVectorInput(t, change.name, base, true)
		v.Steps[1].ResultJSON = bytes.Replace(v.Steps[1].ResultJSON, []byte(change.from), []byte(change.to), 1)
		f.Invalid = append(f.Invalid, v)
	}
	return f
}

func TestSupportSharedFixtures(t *testing.T) {
	path := filepath.Join("..", "..", "api", "support", "v1", "fixtures.json")
	expected, err := json.MarshalIndent(makeSupportFixtures(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	expected = append(expected, '\n')
	if os.Getenv("JOBFORGE_UPDATE_SUPPORT_FIXTURES") == "1" {
		if err := os.WriteFile(path, expected, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, expected) {
		t.Fatalf("support fixture drift; regenerate with JOBFORGE_UPDATE_SUPPORT_FIXTURES=1 go test ./internal/run -run TestSupportSharedFixtures: %v", err)
	}
	var fixture supportFixtureFile
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("shared fixture decode")
	}
	for _, v := range fixture.Valid {
		t.Run(v.Name, func(t *testing.T) {
			actual, err := SupportProposalFromModel(v.Snapshot.binding(), v.prior(), []byte(v.Model))
			if err != nil || !sameProposal(actual, v.Expected) {
				t.Fatalf("shared valid rendering mismatch: %v", err)
			}
		})
	}
	for _, v := range fixture.Invalid {
		t.Run(v.Name, func(t *testing.T) {
			if _, err := SupportProposalFromModel(v.Snapshot.binding(), v.prior(), []byte(v.Model)); err == nil {
				t.Fatal("invalid shared fixture accepted")
			}
		})
	}
}
