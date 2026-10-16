package diagnosis

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestProbeShapeAcceptsOriginalSharedModelVectors(t *testing.T) {
	raw, err := os.ReadFile("../../api/support/v1/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Valid []struct {
			Name     string `json:"name"`
			Model    string `json:"model_json"`
			Snapshot struct {
				Ticket struct {
					Status string `json:"status"`
				} `json:"ticket_binding_json"`
			} `json:"snapshot"`
		} `json:"valid"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil || len(fixture.Valid) == 0 {
		t.Fatal(err)
	}
	for _, item := range fixture.Valid {
		if !probeProposalShape([]byte(item.Model), item.Snapshot.Ticket.Status) {
			t.Fatalf("shared valid vector rejected: %s", item.Name)
		}
	}
}

func TestProbeCountsClosedProposalFailuresButNotQualityFailures(t *testing.T) {
	valid := map[string]any{"decision": "proposal", "action": "escalate", "conclusion": "insufficient",
		"requested_fields": []string{}, "target_ticket_status": "escalated",
		"claims": []any{map[string]any{"kind": "critical", "status": "damaged", "event_id": "event-1", "refs": []string{"E2#/delivery/status", "P06.1"}}}}
	// A missing timing claim is a QUALITY defect, not a malformed proposal.
	if !probeProposalShape(encoded(t, valid), "open") {
		t.Fatal("quality failure incorrectly classified as protocol failure")
	}
	for _, mutation := range []string{"null", "empty", "missing", "extra", "claims", "claim_extra", "claim_enum", "claim_null", "duplicate", "ref", "target", "fields_null"} {
		var proposal any
		if err := json.Unmarshal(encoded(t, valid), &proposal); err != nil {
			t.Fatal(err)
		}
		p := proposal.(map[string]any)
		claim := p["claims"].([]any)[0].(map[string]any)
		switch mutation {
		case "null":
			proposal = nil
		case "empty":
			proposal = map[string]any{}
		case "missing":
			delete(p, "conclusion")
		case "extra":
			p["summary"] = "not allowed"
		case "claims":
			p["claims"] = []any{}
		case "claim_extra":
			claim["test"] = "delivered_late"
		case "claim_enum":
			claim["status"] = "not-a-critical-status"
		case "claim_null":
			claim["event_id"] = nil
		case "duplicate":
			p["claims"] = []any{claim, claim}
		case "ref":
			claim["refs"] = []string{"E2#/delivery/events/0", "P06.1"}
		case "target":
			p["target_ticket_status"] = "open"
		case "fields_null":
			p["requested_fields"] = nil
		}
		request := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "system"}, map[string]any{"role": "user", "content": `{"T":{"status":"open"}}`}}}
		response := responseFixture(true)
		response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] = string(encoded(t, map[string]any{"type": "final", "proposal": proposal}))
		raw := encoded(t, response)
		r := ParseReport(raw, 200, "B", "trial")
		if r.Outcome != "final" || r.Usage == nil || r.Stop != "" {
			t.Fatal("historical parser behavior or usage changed")
		}
		if probeFinalShape(encoded(t, request), raw) {
			t.Fatalf("invalid closed proposal accepted: %s", mutation)
		}
		var health protocolHealth
		if health.stop(1, "invalid_final") || !health.stop(2, "invalid_final") {
			t.Fatal("two closed-shape failures failed to stop")
		}
	}
}

func TestProbeRejectsSemanticallyDuplicateEscapedClaims(t *testing.T) {
	raw := []byte(`{"decision":"proposal","action":"escalate","conclusion":"insufficient","requested_fields":[],"target_ticket_status":"escalated","claims":[{"kind":"critical","status":"damaged","event_id":"event-1","refs":["P06.1","E2#/delivery/status"]},{"kind":"critical","status":"\u0064amaged","event_id":"event-1","refs":["E2#/delivery/status","P06.1"]}]}`)
	if probeProposalShape(raw, "open") {
		t.Fatal("JSON spelling concealed a duplicate claim")
	}
}

type probeDeadlineTransport struct {
	want    time.Time
	matched bool
}

func (p *probeDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	deadline, ok := r.Context().Deadline()
	p.matched = ok && deadline.Equal(p.want)
	if !p.matched {
		return nil, context.DeadlineExceeded
	}
	<-r.Context().Done()
	return nil, r.Context().Err()
}

func TestProbeAuthorizationDeadlineReachesPhysicalSend(t *testing.T) {
	deadline := time.Now().Add(50 * time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	transport := &probeDeadlineTransport{want: deadline}
	report, _ := send(ctx, &http.Client{Transport: transport}, []byte(`{"thinking":{"type":"enabled"}}`), "fixture-key", Trial{ID: "fixture"})
	if !transport.matched || report.Usage != nil || report.Stop != "transport_unknown" {
		t.Fatal("authorization expiry lost the physical deadline or converted unknown usage to zero")
	}
}
