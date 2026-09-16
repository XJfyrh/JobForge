package runprotocol

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/xjfyrh/jobforge/internal/run"
)

var updateAuditWireFixtures = flag.Bool("update-audit-wire-fixtures", false, "regenerate ADR-0020 audit frame vectors")

func auditFrameCopy(t testing.TB, f Frame) Frame {
	t.Helper()
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var result Frame
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func sealAuditReport(t testing.TB, f *Frame) {
	t.Helper()
	if f.ProviderAudit != nil {
		f.ProviderAudit.AuditHash = f.ProviderAudit.Hash()
		if f.Usage != nil {
			hash, err := f.ProviderAudit.ReceiptHash(f.PhysicalCallID)
			if err != nil {
				t.Fatal(err)
			}
			f.Usage.ReceiptHash = hash
		}
	}
	if f.Usage != nil {
		f.Usage.UsageHash = f.Usage.Hash()
	}
	binding, err := ReportBinding(*f, "deepseek-flash")
	if err != nil {
		t.Fatal(err)
	}
	f.ReportHash = Report(*f).Hash(binding)
}

func makeAuditWireFixtures(t *testing.T) fixtures {
	t.Helper()
	frames := observationFrames(t)
	valid := []Frame{}
	conversations := []any{}
	invalid := []any{}
	wire := func(f Frame) json.RawMessage {
		encoded, err := Encode(f)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.TrimSuffix(encoded, []byte("\n"))
	}
	event := func(op string, f Frame, accepted bool) any {
		return map[string]any{"op": op, "frame": wire(f), "now_mono_ms": 1000, "accept": accepted}
	}
	prefix := func() []any {
		return []any{event("ordinary", frames["execute_step"], true), event("ordinary", frames["call_intent"], true),
			event("ordinary", frames["call_permit"], true), map[string]any{"op": "dispatch", "physical_call_id": frames["call_permit"].PhysicalCallID, "now_mono_ms": 1000, "accept": true}}
	}
	// Equal receiver samples represent independent FD reader scheduling without
	// changing causal emission time or replenishing either original deadline.
	for _, order := range []string{"rom a", "r o a m", "o r m a", "o r a m", "o a r m"} {
		sequence := strings.ReplaceAll(order, " ", "")
		events := prefix()
		report, observation, meter := frames["metering_report"], frames["call_observation"], frames["metering_ack"]
		report.EmittedMonoMS, observation.EmittedMonoMS, meter.EmittedMonoMS = 1000, 1000, 1000
		ack := observationACK(t, observation, 1000)
		for _, code := range sequence {
			switch code {
			case 'r':
				events = append(events, event("metering", report, true))
			case 'o':
				events = append(events, event("ordinary", observation, true))
			case 'm':
				events = append(events, event("metering", meter, true))
			case 'a':
				events = append(events, event("ordinary", ack, true))
			}
		}
		events = append(events, event("ordinary", frames["step_result"], true))
		conversations = append(conversations, map[string]any{"name": "audit_join_" + sequence, "events": events})
	}
	for _, name := range []string{"normal", "audit_only", "incompatible", "mode_unexpected", "reasoning_positive", "no_response", "conflict", "unconfirmed", "anomaly"} {
		report := auditFrameCopy(t, frames["metering_report"])
		a := report.ProviderAudit
		settlement := "settled"
		switch name {
		case "audit_only":
			report.Usage = nil
			a.UsageEvidence = run.UsageEvidenceAbsent
			a.ReasoningState = run.ReasoningUnavailable
			a.ModeState = run.ProviderModeUnavailable
			settlement = "recorded"
		case "incompatible":
			model := "another-safe-model"
			a.ResponseModel = &model
			a.IdentityState = run.ProviderIdentityIncompatible
			settlement = "recorded"
		case "mode_unexpected":
			a.ModeState = run.ProviderModeUnexpected
		case "reasoning_positive":
			n := int64(1)
			a.ReasoningState = run.ReasoningObserved
			a.ReasoningTokens = &n
			a.ModeState = run.ProviderModeUnexpected
		case "no_response":
			report.Usage = nil
			a = &run.ProviderAudit{SchemaVersion: 1, Provider: "deepseek", IdentityState: run.ProviderIdentityUnavailable, UsageEvidence: run.UsageEvidenceUnavailable, ReasoningState: run.ReasoningUnavailable, ModeState: run.ProviderModeUnavailable}
			report.ProviderAudit = a
			settlement = "recorded"
		case "conflict", "unconfirmed":
			settlement = name
		case "anomaly":
			report.Usage.OutputTokens = 1025
			settlement = "anomaly"
		}
		sealAuditReport(t, &report)
		ack := frames["metering_ack"]
		ack.ReportHash, ack.Settlement = report.ReportHash, settlement
		valid = append(valid, report, ack)
		events := append(prefix(), event("metering", report, true), event("metering", ack, true))
		if name != "normal" {
			events = append(events, event("ordinary", frames["step_result"], false))
		}
		conversations = append(conversations, map[string]any{"name": "audit_disposition_" + name, "events": events})
	}
	base := frames["metering_report"]
	for _, name := range []string{"wrong_report_hash", "wrong_audit_hash", "missing_audit", "missing_both", "wrong_receipt", "aggregate_overflow", "reasoning_above_output", "unknown_field", "null_scalar", "missing_nullable", "duplicate_key", "oversized_audit"} {
		f := auditFrameCopy(t, base)
		switch name {
		case "wrong_report_hash":
			f.ReportHash = strings.Repeat("f", 64)
		case "wrong_audit_hash":
			f.ProviderAudit.AuditHash = strings.Repeat("f", 64)
		case "missing_audit":
			f.ProviderAudit = nil
		case "missing_both":
			f.ProviderAudit = nil
			f.Usage = nil
		case "wrong_receipt":
			f.Usage.ReceiptHash = strings.Repeat("f", 64)
			f.Usage.UsageHash = f.Usage.Hash()
		case "aggregate_overflow":
			f.Usage.InputTokens = MaxInteger
			f.Usage.UsageHash = f.Usage.Hash()
		case "reasoning_above_output":
			n := int64(6)
			f.ProviderAudit.ReasoningTokens = &n
			f.ProviderAudit.ReasoningState = run.ReasoningObserved
			f.ProviderAudit.ModeState = run.ProviderModeUnexpected
			f.ProviderAudit.AuditHash = f.ProviderAudit.Hash()
		}
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		var all map[string]json.RawMessage
		if json.Unmarshal(raw, &all) != nil {
			t.Fatal("fixture marshal")
		}
		selected := map[string]json.RawMessage{}
		for _, key := range append(append([]string{}, commonFields...), kindFields[f.Kind]...) {
			selected[key] = all[key]
		}
		raw, err = json.Marshal(selected)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		switch name {
		case "unknown_field":
			s = strings.Replace(s, `"provider_audit":{`, `"provider_audit":{"secret":"synthetic-only",`, 1)
		case "null_scalar":
			s = strings.Replace(s, `"response_complete":true`, `"response_complete":null`, 1)
		case "missing_nullable":
			s = strings.Replace(s, `"reasoning_tokens":null,`, "", 1)
		case "duplicate_key":
			s = strings.Replace(s, `"provider_audit":{`, `"provider_audit":{"provider":"deepseek",`, 1)
		case "oversized_audit":
			s = strings.Replace(s, `"provider_audit":{`, `"provider_audit":{`+strings.Repeat(" ", 2048), 1)
		}
		invalid = append(invalid, map[string]any{"name": name, "wire": s + "\n", "channel": "metering"})
	}
	encodedValid := []json.RawMessage{}
	for _, f := range valid {
		encodedValid = append(encodedValid, wire(f))
	}
	raw, err := json.Marshal(map[string]any{"valid_frames": encodedValid, "invalid_frames": invalid, "conversations": conversations, "observation_hash_cases": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	var corpus fixtures
	if json.Unmarshal(raw, &corpus) != nil {
		t.Fatal("fixture decode")
	}
	return corpus
}

func TestSharedAuditWireFixtures(t *testing.T) {
	path := "../../../api/executor/v2/fixtures/audit-frames.json"
	generated := makeAuditWireFixtures(t)
	encoded, err := json.MarshalIndent(generated, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if *updateAuditWireFixtures {
		if err = os.WriteFile(path, encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, encoded) {
		t.Fatal("audit wire fixtures differ from Go source")
	}
	checkSharedFixtures(t, generated)
}
