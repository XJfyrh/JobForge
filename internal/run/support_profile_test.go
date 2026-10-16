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

func TestSupportS5FixedComparisonProfile(t *testing.T) {
	d := supportDefinitionFixture()
	d.SchemaVersion = 5
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-07", "2026-10-07"
	d.Model.MessageContentBytes, d.Model.RequestBodyBytes = 65536, 131072
	d.Resources.DatasetID = SupportS5DatasetID
	p, err := BuildSupportProfile("s5-fixed-comparison", d)
	if err != nil || ValidateSupportProfile(p) != nil || p.ValidateAuditPolicy() != nil ||
		p.ExecutorVersion != SupportFixedComparisonExecutorVersion || p.Executable || p.ApprovalEnabled() || p.ConfirmedStepRecovery() {
		t.Fatal("invalid separately versioned comparison capability", err)
	}
	for _, mutate := range []func(*SupportDefinition){
		func(d *SupportDefinition) { d.SchemaVersion = 1 },
		func(d *SupportDefinition) { d.Program.Strategy = SupportAgentStrategy },
		func(d *SupportDefinition) { d.Model.MessageContentBytes = 16384 },
		func(d *SupportDefinition) { d.Price.ObservedOn = "2026-09-16" },
		func(d *SupportDefinition) { d.Resources.DatasetID = "arbitrary-dataset" },
		func(d *SupportDefinition) { d.Program.RecoveryPolicy = ConfirmedUncommittedRecovery },
	} {
		changed := d
		mutate(&changed)
		if _, err := BuildSupportProfile("bad-comparison", changed); err == nil {
			t.Fatal("widened or mixed comparison capability accepted")
		}
	}
}

func TestSupportS5AgentProfileKeepsApprovalAndRecovery(t *testing.T) {
	d := supportDefinitionFixture()
	d.SchemaVersion = 6
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-07", "2026-10-07"
	d.Model.MessageContentBytes, d.Model.RequestBodyBytes = 65536, 131072
	d.Program = SupportProgramDefinition{Strategy: SupportAgentStrategy, Adapter: "support-agent-v1",
		RecoveryPolicy: ConfirmedUncommittedRecovery, ApprovalPolicy: TicketResolutionApprovalPolicy,
		DecisionSchema: SupportAgentDecisionSchema, DecisionSchemaSHA256: strings.Repeat("a", 64),
		ProposalSchema: SupportProposalSchema, ProposalSchemaSHA256: strings.Repeat("a", 64),
		PromptVersion: SupportAgentPromptVersion, PromptSHA256: strings.Repeat("a", 64), AdapterSourceSHA256: strings.Repeat("a", 64)}
	d.Action = &SupportActionDefinition{Operation: business.ResolutionOperation, Origin: "http://business:8092", KeyID: "s5-key", PublicKeySHA256: strings.Repeat("a", 64)}
	d.Resources.DatasetID = SupportS5DatasetID
	p, err := BuildSupportProfile("s5-agent", d)
	if err != nil || ValidateSupportProfile(p) != nil || !p.ApprovalEnabled() || !p.ConfirmedStepRecovery() || p.ExecutorVersion != SupportApprovalExecutorVersion {
		t.Fatal("S5 lost original approval/recovery capability", err)
	}
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-06", "2026-10-06"
	if _, err := BuildSupportProfile("s5-old-price", d); err == nil {
		t.Fatal("pre-S5 price snapshot accepted")
	}
}

func TestSupportS5CandidateVersionsKeepHistoricalPromptBoundary(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "support-recovery.source.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var source struct {
		Definition SupportDefinition `json:"definition"`
	}
	if json.Unmarshal(raw, &source) != nil {
		t.Fatal("source template")
	}
	d := source.Definition
	d.SchemaVersion = 7
	d.Program.ApprovalPolicy = TicketResolutionApprovalPolicy
	d.Action = &SupportActionDefinition{Operation: business.ResolutionOperation, Origin: "http://business:8092", KeyID: "synthetic-key", PublicKeySHA256: strings.Repeat("a", 64)}
	d.Program.PromptVersion = SupportAgentPromptV2
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-07", "2026-10-07"
	d.Resources.DatasetID = SupportS5V2DatasetID
	p, err := BuildSupportProfile("candidate-v2", d)
	if err != nil || !p.ApprovalEnabled() || !p.ConfirmedStepRecovery() || p.ExecutorVersion != SupportApprovalExecutorVersion {
		t.Fatal("new candidate lost capabilities", err)
	}
	for _, mutate := range []func(*SupportDefinition){
		func(d *SupportDefinition) { d.SchemaVersion = 6 },
		func(d *SupportDefinition) { d.Program.PromptVersion = SupportAgentPromptVersion },
		func(d *SupportDefinition) { d.Resources.DatasetID = "unknown-cohort" },
	} {
		changed := d
		mutate(&changed)
		if _, err := BuildSupportProfile("mixed", changed); err == nil {
			t.Fatal("mixed candidate accepted")
		}
	}
	d.SchemaVersion, d.Program.PromptVersion, d.Resources.DatasetID = 6, SupportAgentPromptVersion, SupportS5DatasetID
	if _, err := BuildSupportProfile("historical", d); err != nil {
		t.Fatal("historical profile changed", err)
	}
	fixed := supportDefinitionFixture()
	fixed.SchemaVersion = 8
	fixed.Model.ObservedOn, fixed.Price.ObservedOn = "2026-10-07", "2026-10-07"
	fixed.Model.MessageContentBytes, fixed.Model.RequestBodyBytes = 65536, 131072
	fixed.Resources.DatasetID = SupportS5V2DatasetID
	if _, err := BuildSupportProfile("fixed-v2", fixed); err != nil {
		t.Fatal(err)
	}
	fixed.SchemaVersion = 5
	if _, err := BuildSupportProfile("old-mixed", fixed); err == nil {
		t.Fatal("historical schema5 silently widened")
	}
	d.SchemaVersion, d.Program.PromptVersion, d.Resources.DatasetID = 9, SupportAgentPromptV3, SupportS5V3DatasetID
	p, err = BuildSupportProfile("candidate-v3", d)
	if err != nil || ValidateSupportProfile(p) != nil || !p.ApprovalEnabled() || !p.ConfirmedStepRecovery() || p.ExecutorVersion != SupportApprovalExecutorVersion {
		t.Fatal("v3 candidate lost capabilities", err)
	}
	for _, mutate := range []func(*SupportDefinition){
		func(d *SupportDefinition) { d.SchemaVersion = 7 },
		func(d *SupportDefinition) { d.Program.PromptVersion = SupportAgentPromptV2 },
		func(d *SupportDefinition) { d.Resources.DatasetID = "unknown-cohort" },
	} {
		changed := d
		mutate(&changed)
		if _, err := BuildSupportProfile("mixed-v3", changed); err == nil {
			t.Fatal("mixed v3 candidate accepted")
		}
	}
	fixed.SchemaVersion, fixed.Resources.DatasetID = 10, SupportS5V3DatasetID
	fixedProfile, err := BuildSupportProfile("fixed-v3", fixed)
	if err != nil || ValidateSupportProfile(fixedProfile) != nil || fixedProfile.ApprovalEnabled() || fixedProfile.ConfirmedStepRecovery() || fixedProfile.ExecutorVersion != SupportFixedComparisonExecutorVersion {
		t.Fatal("fixed v3 capability changed", err)
	}
	fixed.SchemaVersion = 8
	if _, err := BuildSupportProfile("old-fixed-v3-data", fixed); err == nil {
		t.Fatal("historical schema8 silently widened")
	}
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
