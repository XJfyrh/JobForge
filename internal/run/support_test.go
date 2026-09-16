package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
)

func supportJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func supportFixture(t *testing.T, withOrder bool) (SnapshotBinding, []Step) {
	t.Helper()
	orderID, deliveryID := "order-synthetic", "delivery-synthetic"
	observed := time.Date(2026, 9, 16, 8, 0, 0, 0, time.UTC)
	ticket := business.Ticket{TenantID: "tenant-synthetic", TicketID: "ticket-synthetic", Revision: 1,
		ObservedAt: observed, PolicyVersion: "policy-synthetic", Subject: "Synthetic source fixture", Description: "Synthetic protected statement", Status: "open"}
	vector := business.VersionVector{SchemaVersion: 1, Ticket: business.TicketVersion{ID: ticket.TicketID, Revision: 1},
		Policy: business.PolicyRevision{Version: ticket.PolicyVersion, Revision: 1, CorpusSHA256: strings.Repeat("a", 64)},
		Index:  business.IndexVersion{ID: "11111111-1111-4111-8111-111111111111", ProfileHash: strings.Repeat("b", 64), ContentHash: strings.Repeat("c", 64)}}
	orderEvidence := business.Evidence{SnapshotID: "22222222-2222-4222-8222-222222222222", Kind: "order", Missing: !withOrder}
	deliveryEvidence := business.Evidence{SnapshotID: orderEvidence.SnapshotID, Kind: "delivery"}
	orderEvidence.EvidenceRef = "business-evidence:" + orderEvidence.SnapshotID + ":order"
	deliveryEvidence.EvidenceRef = "business-evidence:" + orderEvidence.SnapshotID + ":delivery"
	if withOrder {
		version := int64(1)
		ticket.OrderID = &orderID
		vector.Order = business.OrderVersion{ID: &orderID, Exists: true, Revision: &version}
		vector.Delivery = business.DeliveryVersion{ID: &deliveryID, Exists: true, AggregateRevision: &version}
		orderEvidence.Order = &business.Order{TenantID: ticket.TenantID, OrderID: orderID, Revision: 1, DeliveryID: &deliveryID,
			Status: "shipped", OrderedAt: observed.Add(-48 * time.Hour), PromisedDeliveryAt: observed.Add(-time.Hour)}
		deliveryEvidence.Delivery = &business.Delivery{TenantID: ticket.TenantID, DeliveryID: deliveryID, OrderID: orderID, AggregateRevision: 1, Status: "delivered",
			Events: []business.DeliveryEvent{{EventID: "evt-first", Status: "in_transit", OccurredAt: observed.Add(-time.Hour), Note: "Carrier fixture statement"},
				{EventID: "evt-second", Status: "delivered", OccurredAt: observed, Note: "Carrier fixture correction statement"}}}
	} else {
		orderEvidence.MissingReason = "not_associated"
	}
	snapshot := SnapshotBinding{TenantID: ticket.TenantID, TicketID: ticket.TicketID, ID: orderEvidence.SnapshotID,
		ContentHash: strings.Repeat("d", 64), VersionVector: supportJSON(t, vector), Ticket: supportJSON(t, ticket),
		IndexID: vector.Index.ID, IndexProfileHash: vector.Index.ProfileHash}
	step := func(kind string, content any, refs []string) Step {
		return Step{Kind: kind, Output: supportJSON(t, StepResult{SchemaVersion: 1, ToolInvocationID: "33333333-3333-4333-8333-333333333333",
			PhysicalCallID: "44444444-4444-4444-8444-444444444444", EvidenceRefs: refs, Content: supportJSON(t, content)})}
	}
	prior := []Step{step("get_order", orderEvidence, []string{orderEvidence.EvidenceRef})}
	if withOrder {
		prior = append(prior, step("get_delivery", deliveryEvidence, []string{deliveryEvidence.EvidenceRef}))
	}
	policyRef := "business-policy:" + snapshot.IndexID + ":P01.1"
	prior = append(prior, step("search_policy", map[string]any{"snapshot_id": snapshot.ID, "matches": []business.PolicyHit{{IndexID: snapshot.IndexID,
		ChunkID: "P01.1", PolicyVersion: ticket.PolicyVersion, EvidenceRef: policyRef, Source: "synthetic-policy.md#P01.1", Text: "Synthetic paragraph; not evaluation gold."}}}, []string{policyRef}))
	return snapshot, prior
}

func supportModelJSON(claim string) string {
	return `{"decision":"proposal","action":"escalate","conclusion":"insufficient","requested_fields":[],"target_ticket_status":"escalated","claims":[` + claim + `]}`
}

var supportClaimExamples = []struct{ name, claim string }{
	{"timing", `{"kind":"timing","test":"delivered_not_late","event_id":"evt-second","refs":["T#/observed_at","E1#/order/promised_delivery_at","P01.1"]}`},
	{"dispute", `{"kind":"dispute","type":"non_receipt","delivered_event_id":"evt-second","refs":["T#/description","P01.1"]}`},
	{"critical", `{"kind":"critical","status":"lost","event_id":"evt-first","refs":["E2#/delivery/status","P01.1"]}`},
	{"missing", `{"kind":"missing","field":"ticket.problem_description","refs":["T#/description","P01.1"]}`},
	{"conflict", `{"kind":"conflict","type":"same_time","event_ids":["evt-first","evt-second"],"refs":["E2#/delivery/events","P01.1"]}`},
	{"order_delivery", `{"kind":"conflict","type":"order_delivery","event_ids":[],"refs":["E1#/order/status","E2#/delivery/status","P01.1"]}`},
	{"correction", `{"kind":"correction","recovery_event_id":"evt-second","corrected_event_id":"evt-first","refs":["E2#/delivery/events","P01.1"]}`},
	{"ticket_status", `{"kind":"ticket_status","mode":"preserve_escalated","refs":["T#/status","P01.1"]}`},
}

func TestSupportPreservesClaimsWithoutEvaluatingPolicyOrAddingEffects(t *testing.T) {
	snapshot, prior := supportFixture(t, true)
	for _, example := range supportClaimExamples {
		t.Run(example.name, func(t *testing.T) {
			p, err := SupportProposalFromModel(snapshot, prior, []byte(supportModelJSON(example.claim)))
			if err != nil {
				t.Fatal(err)
			}
			// This fixture deliberately asserts incompatible semantic claims:
			// the runtime must retain them for scoring, never repair the answer.
			if p.Action != "escalate" || p.Conclusion != "insufficient" || !strings.Contains(p.Summary, "Conclusion asserted: insufficient.") ||
				!strings.Contains(p.Summary, "Claim asserted:") || strings.Contains(p.Summary, "approved") || strings.Contains(p.Summary, "resolved") {
				t.Fatal("renderer modified the model assertion or promised an effect")
			}
			if err := validateSupportProposal(snapshot, prior, p); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func supportCommitRequest(t *testing.T, kind string, result StepResult) (Profile, CommitStepRequest) {
	t.Helper()
	profile := Profile{ID: "support-fixture-v1", Hash: Fingerprint("support-fixture"), Strategy: SupportFixedStrategy}
	step := StepIdentity{ID: "55555555-5555-4555-8555-555555555555", Kind: kind, ProfileID: profile.ID, ProfileHash: profile.Hash}
	raw := supportJSON(t, result)
	_, canonical, err := CanonicalStepResultForStrategy(raw, kind, profile.Strategy)
	if err != nil {
		t.Fatal(err)
	}
	return profile, CommitStepRequest{Step: step, ResultJSON: raw, CommitHash: CommitHash(step, canonical)}
}

func TestSupportConditionalGraphAndLegacyRemainSeparate(t *testing.T) {
	for _, withOrder := range []bool{false, true} {
		snapshot, prior := supportFixture(t, withOrder)
		var result StepResult
		if json.Unmarshal(prior[0].Output, &result) != nil {
			t.Fatal("fixture decode")
		}
		profile, request := supportCommitRequest(t, "get_order", result)
		decision, err := DecideCommit(profile, snapshot, nil, request)
		want := "search_policy"
		if withOrder {
			want = "get_delivery"
		}
		if err != nil || decision.NextKind != want {
			t.Fatalf("conditional graph: order=%v next=%s err=%v", withOrder, decision.NextKind, err)
		}
		profile.Strategy = BoundedReadonlyStrategy
		decision, err = DecideCommit(profile, snapshot, nil, request)
		if err != nil || decision.NextKind != "get_delivery" {
			t.Fatalf("legacy graph changed: %v", err)
		}
	}
	snapshot, prior := supportFixture(t, true)
	proposal, err := SupportProposalFromModel(snapshot, prior, []byte(supportModelJSON(supportClaimExamples[0].claim)))
	if err != nil {
		t.Fatal(err)
	}
	result := StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, Content: json.RawMessage("null"), Proposal: proposal,
		PhysicalCallID: "66666666-6666-4666-8666-666666666666"}
	profile, request := supportCommitRequest(t, "model_proposal", result)
	if _, _, err := CanonicalStepResult(request.ResultJSON, request.Step.Kind); err == nil {
		t.Fatal("legacy canonicalizer accepted support fields")
	}
	if _, _, err := CanonicalRegisteredStepResult(request.ResultJSON, request.Step.Kind); err != nil {
		t.Fatal(err)
	}
	decision, err := DecideCommit(profile, snapshot, prior, request)
	if err != nil || decision.NextKind != "submit_proposal" {
		t.Fatalf("support model result: %v", err)
	}
	prior = append(prior, Step{Kind: "model_proposal", Output: decision.CanonicalJSON})
	result.PhysicalCallID = ""
	profile, request = supportCommitRequest(t, "submit_proposal", result)
	decision, err = DecideCommit(profile, snapshot, prior, request)
	if err != nil || !decision.CloseAttempt || decision.Proposal == nil {
		t.Fatalf("support final result: %v", err)
	}
	for name, alter := range map[string]func(*Proposal){
		"summary":        func(p *Proposal) { p.Summary += " Approved and refunded." },
		"pointer":        func(p *Proposal) { p.Claims[0].Refs[0].SourcePointer = "/tenant_id" },
		"event-position": func(p *Proposal) { p.Claims[0].Refs[len(p.Claims[0].Refs)-1].SourcePointer = "/delivery/events/0" },
	} {
		t.Run(name, func(t *testing.T) {
			var copyProposal Proposal
			if json.Unmarshal(supportJSON(t, proposal), &copyProposal) != nil {
				t.Fatal("fixture decode")
			}
			alter(&copyProposal)
			if validateSupportProposal(snapshot, prior, &copyProposal) == nil {
				t.Fatal("modified trusted expansion accepted")
			}
		})
	}
	profile.Strategy = BoundedReadonlyStrategy
	if _, err := DecideCommit(profile, snapshot, prior, request); err == nil {
		t.Fatal("support persisted proposal accepted by legacy strategy")
	}
}

func TestSupportCorrectionIsStillBoundedAndNoActionHasNoWrite(t *testing.T) {
	result := StepResult{SchemaVersion: 1, EvidenceRefs: []string{}, Content: json.RawMessage("null"),
		PhysicalCallID: "66666666-6666-4666-8666-666666666666", CorrectionRequired: true}
	profile, request := supportCommitRequest(t, "model_proposal", result)
	decision, err := DecideCommit(profile, SnapshotBinding{}, nil, request)
	if err != nil || decision.NextKind != "protocol_correction" {
		t.Fatalf("initial correction: %v", err)
	}
	profile, request = supportCommitRequest(t, "protocol_correction", result)
	if _, err := DecideCommit(profile, SnapshotBinding{}, nil, request); !errors.Is(err, ErrModelProtocol) {
		t.Fatalf("correction loop: %v", err)
	}
	snapshot, prior := supportFixture(t, false)
	model := `{"decision":"no_action","action":"","conclusion":"insufficient","requested_fields":[],"target_ticket_status":"open","claims":[{"kind":"ticket_status","mode":"informational_no_action","refs":["T#/status","P01.1"]}]}`
	proposal, err := SupportProposalFromModel(snapshot, prior, []byte(model))
	if err != nil || proposal.Action != "" || proposal.TargetTicketStatus != "open" {
		t.Fatalf("no_action: %v", err)
	}
	for _, changed := range []string{strings.Replace(model, `"action":""`, `"action":"escalate"`, 1),
		strings.Replace(model, `"target_ticket_status":"open"`, `"target_ticket_status":"escalated"`, 1)} {
		if _, err := SupportProposalFromModel(snapshot, prior, []byte(changed)); err == nil {
			t.Fatal("no_action acquired a write action or changed target")
		}
	}
}

func TestSupportSourcesRejectForeignMissingAndAmbiguousFacts(t *testing.T) {
	for _, replacement := range []struct{ name, from, to string }{
		{"foreign-tenant", `"tenant_id":"tenant-synthetic"`, `"tenant_id":"other-tenant"`},
		{"foreign-snapshot", `"snapshot_id":"22222222-2222-4222-8222-222222222222"`, `"snapshot_id":"99999999-9999-4999-8999-999999999999"`},
		{"wrong-revision", `"aggregate_revision":1`, `"aggregate_revision":2`},
		{"wrong-delivery", `"delivery_id":"delivery-synthetic"`, `"delivery_id":"delivery-other"`},
		{"wrong-order", `"order_id":"order-synthetic"`, `"order_id":"order-other"`},
		{"duplicate-event", `"event_id":"evt-second"`, `"event_id":"evt-first"`},
	} {
		t.Run(replacement.name, func(t *testing.T) {
			snapshot, prior := supportFixture(t, true)
			prior[1].Output = bytes.Replace(prior[1].Output, []byte(replacement.from), []byte(replacement.to), 1)
			if _, err := SupportProposalFromModel(snapshot, prior, []byte(supportModelJSON(supportClaimExamples[0].claim))); err == nil {
				t.Fatal("unbound source accepted")
			}
		})
	}
	snapshot, prior := supportFixture(t, true)
	prior[0].Output = bytes.Replace(prior[0].Output, []byte(`"status":"shipped"`), []byte(`"status":"fulfilled"`), 1)
	prior[1].Output = bytes.Replace(prior[1].Output, []byte(`"status":"delivered"`), []byte(`"status":"in_transit"`), 1)
	if _, err := SupportProposalFromModel(snapshot, prior, []byte(supportModelJSON(supportClaimExamples[5].claim))); err != nil {
		t.Fatalf("actual order_delivery conflict hidden by source validator: %v", err)
	}
}

func TestSupportRejectsInvalidBytesAndPreservesExplicitNullSource(t *testing.T) {
	snapshot, prior := supportFixture(t, false)
	model := supportModelJSON(`{"kind":"missing","field":"ticket.order_id","refs":["T#/order_id","E1#/missing","P01.1"]}`)
	if _, err := SupportProposalFromModel(snapshot, prior, []byte(model)); err != nil {
		t.Fatalf("explicitly captured null source rejected: %v", err)
	}
	if _, err := SupportProposalFromModel(snapshot, prior, append([]byte(model), bytes.Repeat([]byte(" "), 16385)...)); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatalf("byte bound: %v", err)
	}
	invalid := bytes.Replace([]byte(model), []byte("ticket.order_id"), []byte{'x', 0xff}, 1)
	if _, err := SupportProposalFromModel(snapshot, prior, invalid); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid UTF-8: %v", err)
	}
}

func TestSupportSchemaPointerVocabularyMatchesRuntime(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "api", "support", "v1", "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if json.Unmarshal(raw, &schema) != nil {
		t.Fatal("invalid source schema")
	}
	var refs struct {
		OneOf []struct {
			Enum []string `json:"enum"`
		} `json:"oneOf"`
	}
	if json.Unmarshal(schema.Defs["shortRef"], &refs) != nil || len(refs.OneOf) != 2 {
		t.Fatal("missing source pointers")
	}
	var expected []string
	for prefix, pointers := range supportPointers {
		for _, pointer := range pointers {
			expected = append(expected, prefix+"#"+pointer)
		}
	}
	slices.Sort(expected)
	slices.Sort(refs.OneOf[0].Enum)
	if !slices.Equal(expected, refs.OneOf[0].Enum) {
		t.Fatal("runtime and source-schema pointer vocabularies drifted")
	}
}
