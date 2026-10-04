package run

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recoveryDefinitionFixture() SupportDefinition {
	d := supportDefinitionFixture()
	d.SchemaVersion = 3
	d.Program.Strategy, d.Program.Adapter = SupportAgentStrategy, "support-agent-v1"
	d.Program.PromptVersion = SupportAgentPromptVersion
	d.Program.DecisionSchema, d.Program.DecisionSchemaSHA256 = SupportAgentDecisionSchema, strings.Repeat("c", 64)
	d.Program.RecoveryPolicy = ConfirmedUncommittedRecovery
	d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-04", "2026-10-04"
	d.Model.MessageContentBytes, d.Model.RequestBodyBytes = 65536, 131072
	return d
}

func TestRecoveryProfileDoesNotUpgradeHistoricalVersions(t *testing.T) {
	d := recoveryDefinitionFixture()
	p, err := BuildSupportProfile("recovery-shared-profile", d)
	if err != nil || !p.ConfirmedStepRecovery() || p.ExecutorVersion != SupportRecoveryExecutorVersion {
		t.Fatalf("S3 profile: %v", err)
	}
	for _, version := range []string{ProviderAuditExecutorVersion, SupportAgentExecutorVersion, "wrong-runtime"} {
		wrong := p
		wrong.ExecutorVersion = version
		if wrong.ConfirmedStepRecovery() || ValidateSupportProfile(wrong) == nil {
			t.Fatalf("wrong runtime %s upgraded S3", version)
		}
	}
	for _, schema := range []int{1, 2} {
		old := d
		if schema == 1 {
			old = supportDefinitionFixture()
		}
		old.SchemaVersion, old.Program.RecoveryPolicy = schema, ""
		legacy, err := BuildSupportProfile("recovery-legacy-profile", old)
		if err != nil || legacy.ConfirmedStepRecovery() || ValidateSupportProfile(legacy) != nil || legacy.ExecutorVersion == SupportRecoveryExecutorVersion {
			t.Fatalf("legacy schema %d: %v", schema, err)
		}
		old.Program.RecoveryPolicy = ConfirmedUncommittedRecovery
		if _, err := BuildSupportProfile("invalid-upgrade", old); err == nil {
			t.Fatal("recovery field accepted in historical definition")
		}
	}
	d.Program.RecoveryPolicy = ""
	if _, err := BuildSupportProfile("missing-policy", d); err == nil {
		t.Fatal("schema3 silently defaulted missing policy")
	}
}

func TestRecoverySharedIdentityFixture(t *testing.T) {
	d := recoveryDefinitionFixture()
	p, err := BuildSupportProfile("recovery-shared-profile", d)
	if err != nil {
		t.Fatal(err)
	}
	lease, step := auditLeaseStep()
	step.ProfileID, step.ProfileHash = p.ID, p.Hash
	raw, _ := json.Marshal(step)
	invalid := map[string]string{
		"missing_cursor": strings.Replace(string(raw), `"cursor_version":4,`, "", 1),
		"null_cursor":    strings.Replace(string(raw), `"cursor_version":4`, `"cursor_version":null`, 1),
		"float_cursor":   strings.Replace(string(raw), `"cursor_version":4`, `"cursor_version":4.0`, 1),
		"alias":          strings.Replace(string(raw), `"step_id"`, `"Step_ID"`, 1),
		"duplicate":      strings.Replace(string(raw), `"sequence":5`, `"sequence":5,"sequence":5`, 1),
		"wrong_sequence": strings.Replace(string(raw), `"sequence":5`, `"sequence":6`, 1),
		"unknown":        strings.TrimSuffix(string(raw), "}") + `,"lease":{}}`,
	}
	for name, value := range invalid {
		if _, err := DecodeRecoveryStep([]byte(value)); err == nil {
			t.Fatalf("invalid proof accepted: %s", name)
		}
	}
	decoded, err := DecodeRecoveryStep(raw)
	if err != nil || decoded != step {
		t.Fatalf("valid proof rejected: %v", err)
	}
	for _, code := range []string{"LEASE_EXPIRED", "TIMEOUT", "DEPENDENCY_UNAVAILABLE", "ATTEMPT_DEADLINE_EXCEEDED"} {
		if !RecoveryClosure("failed_retry", code) || RecoveryClosure("failed_terminal", code) || RecoveryClosure("cancelled", code) {
			t.Fatal("retry proof outcome changed")
		}
	}
	if RecoveryClosure("failed_retry", "EXECUTOR_PROTOCOL_ERROR") || RecoveryClosure("failed_retry", "RUN_DEADLINE_EXCEEDED") {
		t.Fatal("terminal cause gained a recovery proof")
	}
	binding, _ := ExecutionBindingHash(lease, step)
	fixture := struct {
		SchemaVersion int               `json:"schema_version"`
		Profile       Profile           `json:"profile"`
		Lease         Lease             `json:"lease"`
		Step          StepIdentity      `json:"step"`
		Binding       string            `json:"execution_binding_hash"`
		Invalid       map[string]string `json:"invalid_steps"`
	}{1, p, lease, step, binding, invalid}
	encoded, _ := json.MarshalIndent(fixture, "", "  ")
	encoded = append(encoded, '\n')
	path := filepath.Join("..", "..", "api", "support", "recovery-v1", "fixtures.json")
	if os.Getenv("UPDATE_RECOVERY_FIXTURE") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(expected, encoded) {
		t.Fatalf("recovery fixture differs; regenerate with UPDATE_RECOVERY_FIXTURE=1: %v", err)
	}
}
