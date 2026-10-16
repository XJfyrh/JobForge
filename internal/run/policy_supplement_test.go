package run

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func TestPolicySupplementSharedContract(t *testing.T) {
	raw, err := os.ReadFile("../../api/support/agent-v2/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Topics map[string]struct {
			Aliases []string `json:"aliases"`
			Query   string   `json:"query"`
		} `json:"topics"`
		Cases []struct {
			Name  string          `json:"name"`
			Valid bool            `json:"valid"`
			Value json.RawMessage `json:"value"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Topics) != len(supplementTopics) {
		t.Fatal("topic drift")
	}
	for key, expected := range fixture.Topics {
		actual := supplementTopics[key]
		if !slices.Equal(actual.aliases, expected.Aliases) || actual.query != expected.Query {
			t.Fatalf("navigation drift: %s", key)
		}
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			_, err := parsePolicySupplement(c.Value)
			if (err == nil) != c.Valid {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestPolicySupplementVersionedGraphAndSources(t *testing.T) {
	snapshot, prior := supportFixture(t, true)
	marker := StepResult{SchemaVersion: 1, PhysicalCallID: "44444444-4444-4444-8444-444444444444", EvidenceRefs: []string{}, Content: json.RawMessage(`{"type":"investigation_complete","draft":{"decision":"proposal","action":"record_conclusion","conclusion":"on_time","requested_fields":[],"target_ticket_status":"open","claims":[]}}`)}
	markerRaw, _ := json.Marshal(marker)
	prior = append(prior, Step{Kind: "model_decision", Output: markerRaw})
	intent := marker
	intent.Content = json.RawMessage(`{"type":"policy_supplement","topic":"missing_facts","required_aliases":["P03.2"]}`)
	commit := func(p Profile, steps []Step, kind string, result StepResult) (CommitDecision, error) {
		_, req := supportCommitRequest(t, kind, result)
		req.Step.ProfileID, req.Step.ProfileHash = p.ID, p.Hash
		_, raw, err := CanonicalStepResultForStrategy(req.ResultJSON, kind, p.Strategy)
		if err != nil {
			t.Fatal(err)
		}
		req.CommitHash = CommitHash(req.Step, raw)
		return DecideCommit(p, snapshot, steps, req)
	}
	p := adjudicationProfile(t, 14)
	for _, schema := range []int{11, 13} {
		if _, err := commit(adjudicationProfile(t, schema), prior, "model_proposal", intent); err == nil {
			t.Fatal("old graph accepted supplement")
		}
	}
	d, err := commit(p, prior, "model_proposal", intent)
	if err != nil || d.NextKind != "search_policy" || d.Proposal != nil {
		t.Fatal("intent", d, err)
	}
	withIntent := append(slices.Clone(prior), Step{Kind: "model_proposal", Output: d.CanonicalJSON})
	if err := CheckSupportAgentToolForProfile(p, snapshot, withIntent, "search_policy"); err != nil {
		t.Fatal(err)
	}
	if CheckSupportAgentToolForProfile(p, snapshot, withIntent, "get_order") == nil {
		t.Fatal("arbitrary tool")
	}
	var read StepResult
	for _, step := range prior {
		if step.Kind == "search_policy" {
			if err := json.Unmarshal(step.Output, &read); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := commit(p, withIntent, "search_policy", read); err == nil {
		t.Fatal("unrelated read completed supplement")
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(read.Content, &content); err != nil {
		t.Fatal(err)
	}
	var matches []map[string]any
	if err := json.Unmarshal(content["matches"], &matches); err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("fixture missing policy")
	}
	hit := matches[0]
	hit["chunk_id"], hit["source"], hit["text"] = "P03.2", "P03.md", "Synthetic clarification definition"
	// The existing evidence ref carries the paragraph alias as its suffix.
	oldRef := hit["evidence_ref"].(string)
	hit["evidence_ref"] = oldRef[:len(oldRef)-5] + "P03.2"
	content["matches"], _ = json.Marshal([]map[string]any{hit})
	read.Content, _ = json.Marshal(content)
	read.EvidenceRefs = []string{hit["evidence_ref"].(string)}
	d, err = commit(p, withIntent, "search_policy", read)
	if err != nil || d.NextKind != "model_proposal" {
		t.Fatal("supplement result", err)
	}
	after := append(slices.Clone(withIntent), Step{Kind: "search_policy", Output: d.CanonicalJSON})
	if _, err := commit(p, after, "model_proposal", intent); err == nil {
		t.Fatal("second supplement")
	}
	proposal, err := supportProposalFromModel(snapshot, after, []byte(supportModelJSON(supportClaimExamples[0].claim)), true)
	if err != nil {
		t.Fatal(err)
	}
	final := intent
	final.Content, final.Proposal = json.RawMessage("null"), proposal
	d, err = commit(p, after, "model_proposal", final)
	if err != nil || d.NextKind != "submit_proposal" || d.Proposal == nil {
		t.Fatal("final", err)
	}
	final.Proposal, final.CorrectionRequired = nil, true
	if _, err := commit(p, after, "model_proposal", final); err == nil {
		t.Fatal("final retry")
	}
}
