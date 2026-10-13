package run

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func statusPolicyFixture(t *testing.T, status string) (SnapshotBinding, []Step, StepResult) {
	t.Helper()
	snapshot, prior := supportFixture(t, true)
	var ticket map[string]any
	if err := json.Unmarshal(snapshot.Ticket, &ticket); err != nil {
		t.Fatal(err)
	}
	ticket["status"] = status
	snapshot.Ticket = supportJSON(t, ticket)
	marker := StepResult{SchemaVersion: 1, PhysicalCallID: "44444444-4444-4444-8444-444444444444", EvidenceRefs: []string{},
		Content: json.RawMessage(`{"type":"investigation_complete","draft":{"decision":"proposal","action":"escalate","conclusion":"insufficient","requested_fields":[],"target_ticket_status":"open","claims":[]}}`)}
	return snapshot, prior, marker
}

func commitStatusResult(t *testing.T, p Profile, snapshot SnapshotBinding, prior []Step, kind string, result StepResult) (CommitDecision, error) {
	t.Helper()
	_, req := supportCommitRequest(t, kind, result)
	req.Step.ProfileID, req.Step.ProfileHash = p.ID, p.Hash
	_, raw, err := CanonicalStepResultForStrategy(req.ResultJSON, kind, p.Strategy)
	if err != nil {
		t.Fatal(err)
	}
	req.CommitHash = CommitHash(req.Step, raw)
	return DecideCommit(p, snapshot, prior, req)
}

func statusPolicyResult(t *testing.T, snapshot SnapshotBinding, prior []Step, alias string) StepResult {
	t.Helper()
	var result StepResult
	for _, step := range prior {
		if step.Kind == "search_policy" {
			if err := json.Unmarshal(step.Output, &result); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	var content map[string]any
	if err := json.Unmarshal(result.Content, &content); err != nil {
		t.Fatal(err)
	}
	hit := content["matches"].([]any)[0].(map[string]any)
	hit["chunk_id"], hit["source"], hit["text"] = alias, alias[:3]+".md", "Synthetic status policy"
	hit["evidence_ref"] = "business-policy:" + snapshot.IndexID + ":" + alias
	content["matches"] = []any{hit}
	result.Content, result.EvidenceRefs = supportJSON(t, content), []string{hit["evidence_ref"].(string)}
	return result
}

func TestStatusPolicyPrerequisiteSharedRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../api/support/s5-agent-v7/status-policy-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Aliases  map[string]string `json:"status_aliases"`
		Ordinary []string          `json:"ordinary_statuses"`
		Query    string            `json:"query"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Query != supplementTopics["ticket_status"].query {
		t.Fatal("query drift")
	}
	for _, status := range fixture.Ordinary {
		fixture.Aliases[status] = ""
	}
	for status, alias := range fixture.Aliases {
		t.Run(status, func(t *testing.T) {
			snapshot, prior, marker := statusPolicyFixture(t, status)
			p := adjudicationProfile(t, 15)
			for _, kind := range []string{"model_decision", "protocol_correction"} {
				d, err := commitStatusResult(t, p, snapshot, prior, kind, marker)
				want := "model_proposal"
				if alias != "" {
					want = "search_policy"
				}
				if err != nil || d.NextKind != want || d.Proposal != nil {
					t.Fatal("marker route", d, err)
				}
				marked := append(slices.Clone(prior), Step{Kind: kind, Output: d.CanonicalJSON})
				if alias == "" {
					if err := CheckStatusPolicyAdjudication(p, snapshot, marked); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if CheckStatusPolicyAdjudication(p, snapshot, marked) == nil {
					t.Fatal("thinking before policy")
				}
				if CheckSupportAgentToolForProfile(p, snapshot, marked, "get_order") == nil {
					t.Fatal("arbitrary read")
				}
				tool, err := pendingAgentToolForProfile(p, snapshot, marked, "search_policy")
				if err != nil || !sameJSON(tool, supplementTool(policySupplement{Topic: "ticket_status"})) {
					t.Fatal("fixed query", err)
				}
				read := statusPolicyResult(t, snapshot, prior, alias)
				d, err = commitStatusResult(t, p, snapshot, marked, "search_policy", read)
				if err != nil || d.NextKind != "model_proposal" {
					t.Fatal("policy read", err)
				}
				after := append(slices.Clone(marked), Step{Kind: "search_policy", Output: d.CanonicalJSON})
				if err := CheckStatusPolicyAdjudication(p, snapshot, after); err != nil {
					t.Fatal(err)
				}
				// An actual paragraph obtained during investigation costs no extra read.
				present := append(slices.Clone(prior), Step{Kind: "search_policy", Output: d.CanonicalJSON})
				d, err = commitStatusResult(t, p, snapshot, present, kind, marker)
				if err != nil || d.NextKind != "model_proposal" {
					t.Fatal("duplicate prerequisite", err)
				}
			}
			for _, schema := range []int{11, 13, 14} {
				old := adjudicationProfile(t, schema)
				d, err := commitStatusResult(t, old, snapshot, prior, "model_decision", marker)
				if err != nil || d.NextKind != "model_proposal" || old.StatusPolicyEnabled() {
					t.Fatal("old route changed", err)
				}
			}
		})
	}
}

func TestStatusPolicyReadRejectsMissingSourcesAndDuplicateQuery(t *testing.T) {
	snapshot, prior, marker := statusPolicyFixture(t, "informational_only")
	p := adjudicationProfile(t, 15)
	marked := append(slices.Clone(prior), Step{Kind: "model_decision", Output: supportJSON(t, marker)})
	for _, mutation := range []string{"empty", "unrelated", "cross_snapshot", "cross_policy"} {
		t.Run(mutation, func(t *testing.T) {
			read := statusPolicyResult(t, snapshot, prior, "P08.1")
			var content map[string]any
			if err := json.Unmarshal(read.Content, &content); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "empty":
				content["matches"], read.EvidenceRefs = []any{}, []string{}
			case "unrelated":
				read = statusPolicyResult(t, snapshot, prior, "P08.2")
			case "cross_snapshot":
				content["snapshot_id"] = "11111111-1111-4111-8111-111111111111"
			case "cross_policy":
				content["matches"].([]any)[0].(map[string]any)["policy_version"] = "another-policy"
			}
			if mutation != "unrelated" {
				read.Content = supportJSON(t, content)
			}
			if _, err := commitStatusResult(t, p, snapshot, marked, "search_policy", read); err == nil {
				t.Fatal("invalid prerequisite accepted")
			}
		})
	}
	duplicate := marker
	duplicate.Content = supplementTool(policySupplement{Topic: "ticket_status"})
	prior = append([]Step{{Kind: "model_decision", Output: supportJSON(t, duplicate)}}, prior...)
	if _, err := commitStatusResult(t, p, snapshot, prior, "model_decision", marker); err == nil {
		t.Fatal("repeated failed query")
	}
}

func TestStatusPolicyKeepsSeparateOptionalSupplement(t *testing.T) {
	snapshot, prior, marker := statusPolicyFixture(t, "escalated")
	p := adjudicationProfile(t, 15)
	marked := append(slices.Clone(prior), Step{Kind: "model_decision", Output: supportJSON(t, marker)})
	read := statusPolicyResult(t, snapshot, prior, "P08.2")
	d, err := commitStatusResult(t, p, snapshot, marked, "search_policy", read)
	if err != nil {
		t.Fatal(err)
	}
	after := append(marked, Step{Kind: "search_policy", Output: d.CanonicalJSON})
	intent := marker
	intent.Content = json.RawMessage(`{"type":"policy_supplement","topic":"ticket_status","required_aliases":["P09.1"]}`)
	if _, err := commitStatusResult(t, p, snapshot, after, "model_proposal", intent); err == nil {
		t.Fatal("status query repeated")
	}
	intent.Content = json.RawMessage(`{"type":"policy_supplement","topic":"missing_facts","required_aliases":["P03.2"]}`)
	d, err = commitStatusResult(t, p, snapshot, after, "model_proposal", intent)
	if err != nil || d.NextKind != "search_policy" {
		t.Fatal("optional supplement lost", err)
	}
	after = append(after, Step{Kind: "model_proposal", Output: d.CanonicalJSON})
	d, err = commitStatusResult(t, p, snapshot, after, "search_policy", statusPolicyResult(t, snapshot, prior, "P03.2"))
	if err != nil || d.NextKind != "model_proposal" {
		t.Fatal("optional result", err)
	}
	after = append(after, Step{Kind: "search_policy", Output: d.CanonicalJSON})
	if CheckStatusPolicyAdjudication(p, snapshot, after) != nil {
		t.Fatal("second thinking blocked")
	}
	if _, err := commitStatusResult(t, p, snapshot, after, "model_proposal", intent); err == nil {
		t.Fatal("second optional supplement")
	}
}
