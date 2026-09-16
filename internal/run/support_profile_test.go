package run

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
)

func supportDefinitionFixture() SupportDefinition {
	hash := strings.Repeat("a", 64)
	d := SupportDefinition{SchemaVersion: 1,
		Model: SupportModelDefinition{Provider: "deepseek", Origin: "https://api.deepseek.com", Path: "/chat/completions",
			RequestModel: "deepseek-flash", ObservedVersion: "DeepSeek-V4.1-Flash", ObservedOn: "2026-09-16",
			Thinking: "disabled", MaxTokens: 1024, ResponseFormat: "json_object", RequestBodyBytes: 65536,
			MessageContentBytes: 16384, ResponseBodyBytes: 65536, ModelContentBytes: 16384},
		Program: SupportProgramDefinition{Strategy: SupportFixedStrategy, Adapter: "support-fixed-v1",
			ProposalSchema: SupportProposalSchema, ProposalSchemaSHA256: hash, PromptVersion: SupportPromptVersion,
			PromptSHA256: hash, AdapterSourceSHA256: hash},
		Resources: SupportResourceDefinition{DatasetID: SupportDatasetID, RuntimeManifestSHA256: hash, SeedSHA256: hash,
			ObservedAt: SupportObservedAt, PolicyVersion: SupportPolicyVersion, CorpusSHA256: hash,
			IndexProfile: business.IndexProfile{PolicyVersion: SupportPolicyVersion, CorpusSHA256: hash,
				ChunkerVersion: business.ChunkerVersion, EmbeddingModel: business.EmbeddingModel,
				EmbeddingDigest: business.EmbeddingDigest, Dimensions: business.Dimensions},
			Tenants: []SupportTenantDefinition{
				{TenantID: "tenant-north", PolicyRevision: 2, IndexID: "00000000-0000-4000-8000-000000000001", IndexContentHash: hash},
				{TenantID: "tenant-south", PolicyRevision: 2, IndexID: "00000000-0000-4000-8000-000000000002", IndexContentHash: hash},
			}},
		Price: SupportPriceDefinition{ObservedOn: "2026-09-16", SourceSHA256: hash, Currency: "CNY", Denominator: 1000000,
			InputMissMicroyuan: 2000000, InputHitMicroyuan: 40000, OutputMicroyuan: 8000000}}
	raw, _ := json.Marshal(d.Resources.IndexProfile)
	digest := sha256.Sum256(raw)
	d.Resources.IndexProfileHash = hex.EncodeToString(digest[:])
	return d
}

func TestSupportProfileFixture(t *testing.T) {
	d := supportDefinitionFixture()
	p, err := BuildSupportProfile("support-cloud-vector-v1", d)
	if err != nil {
		t.Fatal(err)
	}
	negative := map[string]string{
		"missing_false":    strings.Replace(string(p.Definition), `"stream":false,`, "", 1),
		"null_false":       strings.Replace(string(p.Definition), `"stream":false`, `"stream":null`, 1),
		"duplicate":        strings.Replace(string(p.Definition), `"stream":false`, `"stream":false,"stream":false`, 1),
		"unknown":          strings.Replace(string(p.Definition), `"stream":false`, `"stream":false,"metadata":{}`, 1),
		"case_alias":       strings.Replace(string(p.Definition), `"stream":false`, `"Stream":false`, 1),
		"floating_integer": strings.Replace(string(p.Definition), `"temperature":0`, `"temperature":0.0`, 1),
		"integer_overflow": strings.Replace(string(p.Definition), `"temperature":0`, `"temperature":9223372036854775808`, 1),
	}
	for name, raw := range negative {
		if _, err := DecodeSupportDefinition([]byte(raw)); err != ErrProfileUnavailable {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	fixture := struct {
		SchemaVersion       int               `json:"schema_version"`
		Profile             Profile           `json:"profile"`
		CanonicalDefinition string            `json:"canonical_definition_json"`
		PriceHash           string            `json:"price_hash"`
		ProfileHash         string            `json:"profile_hash"`
		InvalidDefinitions  map[string]string `json:"invalid_definitions"`
	}{1, p, string(p.Definition), p.Pricing.Hash, p.Hash, negative}
	raw, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	path := filepath.Join("..", "..", "api", "support", "profile-v1", "fixtures.json")
	if os.Getenv("UPDATE_SUPPORT_PROFILE_FIXTURE") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(expected, raw) {
		t.Fatalf("support profile fixture differs; regenerate with UPDATE_SUPPORT_PROFILE_FIXTURE=1: %v", err)
	}
}

func TestSupportProfileRejectsFrozenFactTampering(t *testing.T) {
	d := supportDefinitionFixture()
	p, err := BuildSupportProfile("support-profile-v1", d)
	if err != nil {
		t.Fatal(err)
	}
	if p.Executable || ValidateSupportProfile(p) != nil {
		t.Fatal("generated profile must be valid and disabled")
	}
	p.Executable = true
	if ValidateSupportProfile(p) != nil {
		t.Fatal("enabling must not change identity")
	}
	for name, mutate := range map[string]func(*Profile){
		"declared_hash": func(p *Profile) { p.Hash = strings.Repeat("b", 64) },
		"price_hash":    func(p *Profile) { p.Pricing.Hash = strings.Repeat("b", 64) },
		"tokens":        func(p *Profile) { p.MaxInputTokens-- },
		"family":        func(p *Profile) { p.FamilyTokenLimit++ },
		"definition": func(p *Profile) {
			p.Definition = bytes.ReplaceAll(p.Definition, []byte(strings.Repeat("a", 64)), []byte(strings.Repeat("b", 64)))
		},
		"stream": func(p *Profile) {
			p.Definition = bytes.Replace(p.Definition, []byte(`"stream":false`), []byte(`"stream":true`), 1)
		},
		"tenant_order": func(p *Profile) {
			d := supportDefinitionFixture()
			d.Resources.Tenants[0], d.Resources.Tenants[1] = d.Resources.Tenants[1], d.Resources.Tenants[0]
			p.Definition, _ = json.Marshal(d)
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := p
			mutate(&changed)
			if ValidateSupportProfile(changed) != ErrProfileUnavailable {
				t.Fatal("tampered profile accepted")
			}
		})
	}
	for _, raw := range [][]byte{append(p.Definition, []byte(`{}`)...), []byte(strings.Repeat(" ", SupportDefinitionMaxBytes+1)), append([]byte{0xff}, p.Definition...)} {
		if _, err := DecodeSupportDefinition(raw); err != ErrProfileUnavailable {
			t.Fatal("invalid raw definition accepted")
		}
	}
}

func supportProfileSnapshot(t *testing.T) (Profile, SnapshotBinding) {
	t.Helper()
	d := supportDefinitionFixture()
	p, err := BuildSupportProfile("support-profile-v1", d)
	if err != nil {
		t.Fatal(err)
	}
	observed, _ := time.Parse(time.RFC3339, SupportObservedAt)
	ticket := business.Ticket{TenantID: "tenant-north", TicketID: "T-1", Revision: 1,
		ObservedAt: observed, PolicyVersion: SupportPolicyVersion, Status: "open"}
	vector := business.VersionVector{SchemaVersion: 1, Ticket: business.TicketVersion{ID: ticket.TicketID, Revision: 1},
		Policy: business.PolicyRevision{Version: SupportPolicyVersion, Revision: 2, CorpusSHA256: d.Resources.CorpusSHA256},
		Index:  business.IndexVersion{ID: d.Resources.Tenants[0].IndexID, ProfileHash: d.Resources.IndexProfileHash, ContentHash: d.Resources.Tenants[0].IndexContentHash}}
	ticketRaw, _ := json.Marshal(ticket)
	vectorRaw, _ := json.Marshal(vector)
	return p, SnapshotBinding{TenantID: ticket.TenantID, TicketID: ticket.TicketID, ID: "00000000-0000-4000-8000-000000000003",
		ContentHash: strings.Repeat("f", 64), Ticket: ticketRaw, VersionVector: vectorRaw,
		IndexID: vector.Index.ID, IndexProfileHash: vector.Index.ProfileHash}
}

func TestSupportSnapshotRejectsWrongFrozenResources(t *testing.T) {
	p, snapshot := supportProfileSnapshot(t)
	if ValidateSupportSnapshot(p, snapshot) != nil {
		t.Fatal("valid snapshot rejected")
	}
	for name, mutate := range map[string]func(*business.Ticket, *business.VersionVector, *SnapshotBinding){
		"tenant": func(ticket *business.Ticket, _ *business.VersionVector, s *SnapshotBinding) {
			ticket.TenantID = "tenant-other"
			s.TenantID = ticket.TenantID
		},
		"policy": func(ticket *business.Ticket, v *business.VersionVector, _ *SnapshotBinding) {
			ticket.PolicyVersion = "policy-other"
			v.Policy.Version = ticket.PolicyVersion
		},
		"revision": func(_ *business.Ticket, v *business.VersionVector, _ *SnapshotBinding) { v.Policy.Revision++ },
		"corpus": func(_ *business.Ticket, v *business.VersionVector, _ *SnapshotBinding) {
			v.Policy.CorpusSHA256 = strings.Repeat("b", 64)
		},
		"index": func(_ *business.Ticket, v *business.VersionVector, s *SnapshotBinding) {
			v.Index.ID = "00000000-0000-4000-8000-000000000004"
			s.IndexID = v.Index.ID
		},
		"content": func(_ *business.Ticket, v *business.VersionVector, _ *SnapshotBinding) {
			v.Index.ContentHash = strings.Repeat("b", 64)
		},
		"index_profile": func(_ *business.Ticket, v *business.VersionVector, s *SnapshotBinding) {
			v.Index.ProfileHash = strings.Repeat("b", 64)
			s.IndexProfileHash = v.Index.ProfileHash
		},
		"observed_at": func(ticket *business.Ticket, _ *business.VersionVector, _ *SnapshotBinding) {
			ticket.ObservedAt = ticket.ObservedAt.Add(time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := snapshot
			var ticket business.Ticket
			var v business.VersionVector
			_ = json.Unmarshal(s.Ticket, &ticket)
			_ = json.Unmarshal(s.VersionVector, &v)
			mutate(&ticket, &v, &s)
			s.Ticket, _ = json.Marshal(ticket)
			s.VersionVector, _ = json.Marshal(v)
			if err := ValidateSupportSnapshot(p, s); err != ErrProfileUnavailable {
				t.Fatalf("wrong resource accepted or misclassified: %v", err)
			}
		})
	}
	snapshot.TicketID = "wrong-ticket"
	if ValidateSupportSnapshot(p, snapshot) != ErrDependencyUnavailable {
		t.Fatal("internally inconsistent capture must remain dependency failure")
	}
}
