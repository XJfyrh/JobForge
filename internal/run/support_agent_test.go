package run

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func agentDecisionStep(t *testing.T, query string) Step {
	t.Helper()
	return Step{Kind: "model_decision", Output: supportJSON(t, StepResult{SchemaVersion: 1,
		PhysicalCallID: "44444444-4444-4444-8444-444444444444", EvidenceRefs: []string{},
		Content: supportJSON(t, map[string]any{"type": "tool", "name": "search_policy", "arguments": map[string]string{"query": query}})})}
}

func TestSupportAgentSharedToolDecisions(t *testing.T) {
	raw, err := os.ReadFile("../../api/support/agent-v1/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			Name         string          `json:"name"`
			Model        string          `json:"model_json"`
			MissingOrder bool            `json:"missing_order"`
			Valid        bool            `json:"valid"`
			Expected     json.RawMessage `json:"expected"`
		} `json:"cases"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("invalid fixture")
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			snapshot, _ := supportFixture(t, !c.MissingOrder)
			_, canonical, err := SupportAgentToolDecision(snapshot, []byte(c.Model))
			if (err == nil) != c.Valid {
				t.Fatalf("valid=%v error=%v", c.Valid, err)
			}
			if c.Valid && !sameJSON(canonical, c.Expected) {
				t.Fatal("normalization differs")
			}
		})
	}
}

func TestSupportAgentDecisionCommitDoesNotAuthorizeRepeatedTool(t *testing.T) {
	snapshot, reads := supportFixture(t, true)
	first := agentDecisionStep(t, "delivery timing")
	var result StepResult
	if json.Unmarshal(first.Output, &result) != nil {
		t.Fatal("fixture")
	}
	profile, req := supportCommitRequest(t, first.Kind, result)
	profile.Strategy = SupportAgentStrategy
	decision, err := DecideCommit(profile, snapshot, nil, req)
	if err != nil || decision.NextKind != "search_policy" {
		t.Fatalf("decision: %+v %v", decision, err)
	}
	if err := CheckSupportAgentTool(snapshot, []Step{first}, "search_policy"); err != nil {
		t.Fatal(err)
	}
	prior := []Step{first, reads[len(reads)-1], first}
	if _, err := DecideCommit(profile, snapshot, prior[:2], req); err != nil {
		t.Fatal("intent must remain queryable", err)
	}
	if !errors.Is(CheckSupportAgentTool(snapshot, prior, "search_policy"), ErrModelProtocol) {
		t.Fatal("repeated query authorized")
	}
	prior[2] = agentDecisionStep(t, "customer dispute")
	if err := CheckSupportAgentTool(snapshot, prior, "search_policy"); err != nil {
		t.Fatal("new query refused", err)
	}
	if CheckSupportAgentTool(snapshot, prior, "get_order") == nil {
		t.Fatal("tool changed after decision")
	}
}

func TestSupportAgentAccumulatesOnlyConsistentPolicies(t *testing.T) {
	snapshot, prior := supportFixture(t, true)
	last := prior[len(prior)-1]
	// Retrieval distance is query-specific; source identity and content are not.
	second := last
	second.Output = []byte(strings.Replace(string(last.Output), `"distance":0`, `"distance":0.5`, 1))
	prior = append(prior, second)
	if _, err := buildSupportSourcesWithSearch(snapshot, prior, true); err != nil {
		t.Fatal(err)
	}
	if _, err := buildSupportSources(snapshot, prior); err == nil {
		t.Fatal("fixed strategy widened")
	}
	second.Output = []byte(strings.ReplaceAll(string(last.Output), "P01.1", "P02.1"))
	prior[len(prior)-1] = second
	sources, err := buildSupportSourcesWithSearch(snapshot, prior, true)
	if err != nil || sources.aliases["P01.1"].EvidenceRef == "" || sources.aliases["P02.1"].EvidenceRef == "" {
		t.Fatal("earlier source lost", err)
	}
	second.Output = []byte(strings.Replace(string(last.Output), "Synthetic paragraph; not evaluation gold.", "Different content", 1))
	prior[len(prior)-1] = second
	if _, err := buildSupportSourcesWithSearch(snapshot, prior, true); err == nil {
		t.Fatal("conflicting policy accepted")
	}
}

func TestSupportAgentProfileDoesNotUpgradeS1(t *testing.T) {
	d := supportDefinitionFixture()
	old, err := BuildSupportProfile("old-support", d)
	if err != nil {
		t.Fatal(err)
	}
	d.SchemaVersion = 2
	d.Program.Strategy, d.Program.Adapter = SupportAgentStrategy, "support-agent-v1"
	d.Program.PromptVersion = SupportAgentPromptVersion
	d.Program.DecisionSchema, d.Program.DecisionSchemaSHA256 = SupportAgentDecisionSchema, strings.Repeat("c", 64)
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-09-17", "2026-09-17"
	d.Model.MessageContentBytes, d.Model.RequestBodyBytes = 65536, 131072
	agent, err := BuildSupportProfile("new-agent", d)
	if err != nil || agent.ExecutorVersion != SupportAgentExecutorVersion || agent.Hash == old.Hash {
		t.Fatalf("agent profile: %v", err)
	}
	if ValidateSupportProfile(old) != nil || ValidateSupportProfile(agent) != nil {
		t.Fatal("registered profile invalid")
	}
	agent.ExecutorVersion = ProviderAuditExecutorVersion
	if ValidateSupportProfile(agent) == nil {
		t.Fatal("old executor accepted dynamic profile")
	}
}
