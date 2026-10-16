package run

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/business"
)

func bundleFixture(t *testing.T) (SnapshotBinding, []Step, StepResult) {
	t.Helper()
	snapshot, reads := supportFixture(t, true)
	var template StepResult
	if err := json.Unmarshal(reads[len(reads)-1].Output, &template); err != nil {
		t.Fatal(err)
	}
	var body struct {
		SnapshotID string               `json:"snapshot_id"`
		Matches    []business.PolicyHit `json:"matches"`
	}
	if err := json.Unmarshal(template.Content, &body); err != nil {
		t.Fatal(err)
	}
	base := body.Matches[0]
	body.Matches = nil
	template.EvidenceRefs = []string{}
	for _, alias := range business.PolicyBundleAliases() {
		hit := base
		hit.ChunkID, hit.EvidenceRef = alias, "business-policy:"+snapshot.IndexID+":"+alias
		body.Matches = append(body.Matches, hit)
		template.EvidenceRefs = append(template.EvidenceRefs, hit.EvidenceRef)
	}
	template.Content = supportJSON(t, body)
	ticket := StepResult{SchemaVersion: 1, EvidenceRefs: []string{"business-evidence:" + snapshot.ID + ":ticket"}, Content: snapshot.Ticket}
	prefix := []Step{{Kind: "read_ticket", Output: supportJSON(t, ticket)}}
	return snapshot, append(prefix, reads...), template
}

func TestPolicyBundleBothGraphsAndOldIsolation(t *testing.T) {
	for _, schema := range []int{16, 17, 18} {
		t.Run(string(rune('A'+schema-16)), func(t *testing.T) {
			p := adjudicationProfile(t, schema)
			snapshot, reads, bundle := bundleFixture(t)
			var ticket StepResult
			if err := json.Unmarshal(reads[0].Output, &ticket); err != nil {
				t.Fatal(err)
			}
			d, err := commitStatusResult(t, p, snapshot, nil, "read_ticket", ticket)
			if err != nil || d.NextKind != "read_policy_bundle" {
				t.Fatal("missing prerequisite", d, err)
			}
			d, err = commitStatusResult(t, p, snapshot, reads[:1], "read_policy_bundle", bundle)
			want := "model_decision"
			if schema == 17 {
				want = "get_order"
			}
			if err != nil || d.NextKind != want {
				t.Fatal("bundle next", d, err)
			}
			prior := append(slices.Clone(reads[:1]), Step{Kind: "read_policy_bundle", Output: d.CanonicalJSON})
			if err := CheckPolicyBundleStep(p, snapshot, prior, want); err != nil {
				t.Fatal(err)
			}
			if CheckPolicyBundleStep(p, snapshot, reads[:1], want) == nil ||
				CheckPolicyBundleStep(p, snapshot, prior, "read_policy_bundle") == nil {
				t.Fatal("skipped or repeated bundle")
			}
			for _, oldSchema := range []int{11, 12, 13, 14, 15} {
				old := adjudicationProfile(t, oldSchema)
				if CheckPolicyBundleStep(old, snapshot, reads[:1], "read_policy_bundle") == nil ||
					CheckPolicyBundleStep(old, snapshot, prior, "model_proposal") == nil {
					t.Fatal("old version acquired bundle")
				}
			}
			if schema == 17 {
				for _, read := range reads[1:3] {
					var result StepResult
					if err := json.Unmarshal(read.Output, &result); err != nil {
						t.Fatal(err)
					}
					d, err = commitStatusResult(t, p, snapshot, prior, read.Kind, result)
					if err != nil {
						t.Fatal(err)
					}
					prior = append(prior, Step{Kind: read.Kind, Output: d.CanonicalJSON})
				}
				if d.NextKind != "model_proposal" {
					t.Fatal("Fixed still needs search")
				}
			}
			proposalPrior := append(slices.Clone(prior[:2]), reads[1:]...)
			if _, err := SupportProposalFromModel(snapshot, proposalPrior, []byte(supportModelJSON(supportClaimExamples[0].claim))); err != nil {
				t.Fatal("bundle source expansion", err)
			}
		})
	}
}

func TestPolicyBundleStrictShapeAndOwnLimit(t *testing.T) {
	for _, mutation := range []string{"missing", "extra", "duplicate", "reorder", "alias", "snapshot", "index", "version", "ref", "empty", "oversize", "distance", "source"} {
		t.Run(mutation, func(t *testing.T) {
			snapshot, prior, bundle := bundleFixture(t)
			var body map[string]any
			if err := json.Unmarshal(bundle.Content, &body); err != nil {
				t.Fatal(err)
			}
			hits := body["matches"].([]any)
			hit := hits[0].(map[string]any)
			switch mutation {
			case "missing":
				hits = hits[:19]
			case "extra":
				hits = append(hits, hit)
			case "duplicate":
				hits[1] = hit
			case "reorder":
				slices.Reverse(hits)
			case "alias":
				hit["chunk_id"] = "P11.1"
			case "snapshot":
				body["snapshot_id"] = "wrong-snapshot"
			case "index":
				hit["index_id"] = "wrong-index"
			case "version":
				hit["policy_version"] = "wrong-version"
			case "ref":
				hit["evidence_ref"] = "wrong-ref"
			case "empty":
				hit["text"] = " "
			case "oversize":
				hit["text"] = strings.Repeat("x", 769)
			case "distance":
				hit["distance"] = nil
			case "source":
				hit["source"] = ""
			}
			body["matches"], bundle.EvidenceRefs = hits, []string{}
			for _, value := range hits {
				bundle.EvidenceRefs = append(bundle.EvidenceRefs, value.(map[string]any)["evidence_ref"].(string))
			}
			bundle.Content = supportJSON(t, body)
			if _, err := commitStatusResult(t, adjudicationProfile(t, 16), snapshot, prior[:1], "read_policy_bundle", bundle); err == nil {
				t.Fatal("bad bundle accepted")
			}
		})
	}
	snapshot, prior, bundle := bundleFixture(t)
	var body map[string]any
	if err := json.Unmarshal(bundle.Content, &body); err != nil {
		t.Fatal(err)
	}
	for _, hit := range body["matches"].([]any) {
		hit.(map[string]any)["text"] = strings.Repeat("x", 700)
	}
	bundle.Content = supportJSON(t, body)
	raw := supportJSON(t, bundle)
	if len(raw) <= 8192 {
		t.Fatal("fixture does not cross old bound")
	}
	if _, _, err := CanonicalStepResultForStrategy(raw, "read_policy_bundle", SupportAgentStrategy); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CanonicalStepResultForStrategy(raw, "search_policy", SupportAgentStrategy); err == nil {
		t.Fatal("old bound enlarged")
	}
	if _, err := commitStatusResult(t, adjudicationProfile(t, 16), snapshot, prior[:1], "read_policy_bundle", bundle); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyBundleSearchCannotReplaceEvidence(t *testing.T) {
	snapshot, reads, bundle := bundleFixture(t)
	prior := append(slices.Clone(reads[:1]), Step{Kind: "read_policy_bundle", Output: supportJSON(t, bundle)})
	prior = append(prior, reads[1:]...)
	if _, err := buildSupportSourcesWithSearch(snapshot, prior, true); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"text", "source"} {
		var result StepResult
		if err := json.Unmarshal(reads[len(reads)-1].Output, &result); err != nil {
			t.Fatal(err)
		}
		var content map[string]any
		if err := json.Unmarshal(result.Content, &content); err != nil {
			t.Fatal(err)
		}
		content["matches"].([]any)[0].(map[string]any)[field] = "changed"
		result.Content = supportJSON(t, content)
		prior[len(prior)-1].Output = supportJSON(t, result)
		if _, err := buildSupportSourcesWithSearch(snapshot, prior, true); err == nil {
			t.Fatal("overwritten " + field)
		}
	}
}
