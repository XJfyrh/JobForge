package run

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var updateProviderAuditFixture = flag.Bool("update-provider-audit-fixture", false, "regenerate the provider audit shared fixture")

func auditPtr[T any](value T) *T { return &value }

func auditLeaseStep() (Lease, StepIdentity) {
	return Lease{TenantID: "tenant-a", RunID: "11111111-1111-4111-8111-111111111111", WorkerID: "worker-a",
			SessionID: "22222222-2222-4222-8222-222222222222", AttemptNo: 2, FencingToken: 3},
		StepIdentity{ID: "33333333-3333-4333-8333-333333333333", Sequence: 5, Kind: "model_proposal", CursorVersion: 4,
			InputHash: strings.Repeat("a", 64), ProfileID: "audit-profile-v1", ProfileHash: strings.Repeat("b", 64),
			SnapshotID: "44444444-4444-4444-8444-444444444444", SnapshotHash: strings.Repeat("c", 64)}
}

func auditBinding(t *testing.T) ReportBinding {
	t.Helper()
	lease, step := auditLeaseStep()
	hash, err := ExecutionBindingHash(lease, step)
	if err != nil {
		t.Fatal(err)
	}
	return ReportBinding{ExecutionBindingHash: hash, PhysicalCallID: "55555555-5555-4555-8555-555555555555",
		ParameterHash: strings.Repeat("d", 64), Subcall: SubcallChat, ExpectedResponseModel: "deepseek-flash"}
}

func completeAudit() ProviderAudit {
	audit := ProviderAudit{SchemaVersion: 1, Provider: "deepseek", ResponseComplete: true, HTTPStatus: 200,
		ResponseSHA256: auditPtr(strings.Repeat("e", 64)), IdentityState: ProviderIdentityCompatible,
		ResponseID: auditPtr("synthetic-response-1"), ResponseModel: auditPtr("deepseek-flash"),
		SystemFingerprint: auditPtr("synthetic-fp-1"), Created: auditPtr(int64(0)),
		UsageEvidence: UsageEvidenceComplete, ReasoningState: ReasoningAbsent, ModeState: ProviderModeNonthinking}
	audit.AuditHash = audit.Hash()
	return audit
}

func incompleteAudit() ProviderAudit {
	audit := ProviderAudit{SchemaVersion: 1, Provider: "deepseek", IdentityState: ProviderIdentityUnavailable,
		UsageEvidence: UsageEvidenceUnavailable, ReasoningState: ReasoningUnavailable, ModeState: ProviderModeUnavailable}
	audit.AuditHash = audit.Hash()
	return audit
}

func auditReport(t *testing.T, audit ProviderAudit, input, output int64) CallReport {
	t.Helper()
	audit.AuditHash = audit.Hash()
	report := CallReport{ProviderAudit: &audit}
	if audit.UsageEvidence == UsageEvidenceComplete {
		receipt, err := audit.ReceiptHash(auditBinding(t).PhysicalCallID)
		if err != nil {
			t.Fatal(err)
		}
		usage := UsageReport{InputTokens: input, OutputTokens: output, CachedInputTokens: min(input, 4), ReceiptHash: receipt}
		usage.UsageHash = usage.Hash()
		report.Usage = &usage
	}
	return report
}

func auditPriceBudget() (Pricing, CallBudget) {
	return Pricing{Denominator: 10, InputMissMicroyuan: 7, InputHitMicroyuan: 2, OutputMicroyuan: 11},
		CallBudget{InputTokens: 100, OutputTokens: 20, TotalTokens: 120, CostMicroyuan: 92}
}

func auditJSON(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestProviderAuditPreservesPresenceAndReasoningMatrix(t *testing.T) {
	binding := auditBinding(t)
	for _, tc := range []struct {
		name      string
		reasoning ReasoningState
		tokens    *int64
		usage     UsageEvidenceState
		mode      ProviderModeState
		valid     bool
	}{
		{"absent", ReasoningAbsent, nil, UsageEvidenceComplete, ProviderModeNonthinking, true},
		{"explicit-zero", ReasoningObserved, auditPtr(int64(0)), UsageEvidenceComplete, ProviderModeNonthinking, true},
		{"positive", ReasoningObserved, auditPtr(int64(2)), UsageEvidenceComplete, ProviderModeUnexpected, true},
		{"reasoning-invalid", ReasoningInvalid, nil, UsageEvidenceInvalid, ProviderModeInvalid, true},
		{"unknown-counters", ReasoningUnavailable, nil, UsageEvidenceAbsent, ProviderModeUnavailable, true},
		{"observed-null", ReasoningObserved, nil, UsageEvidenceComplete, ProviderModeNonthinking, false},
		{"absent-zero", ReasoningAbsent, auditPtr(int64(0)), UsageEvidenceComplete, ProviderModeNonthinking, false},
		{"negative", ReasoningObserved, auditPtr(int64(-1)), UsageEvidenceComplete, ProviderModeUnexpected, false},
		{"unsafe", ReasoningObserved, auditPtr(MaxSafeInteger + 1), UsageEvidenceComplete, ProviderModeUnexpected, false},
		{"over-completion", ReasoningObserved, auditPtr(int64(3)), UsageEvidenceComplete, ProviderModeUnexpected, false},
		{"positive-nonthinking", ReasoningObserved, auditPtr(int64(1)), UsageEvidenceComplete, ProviderModeNonthinking, false},
		{"isolated-reasoning", ReasoningObserved, auditPtr(int64(1)), UsageEvidenceInvalid, ProviderModeUnexpected, false},
		{"unknown-absent", ReasoningAbsent, nil, UsageEvidenceAbsent, ProviderModeUnavailable, false},
		{"invalid-with-complete", ReasoningInvalid, nil, UsageEvidenceComplete, ProviderModeInvalid, false},
		{"invalid-but-nonthinking", ReasoningInvalid, nil, UsageEvidenceInvalid, ProviderModeNonthinking, false},
		{"complete-but-mode-unavailable", ReasoningAbsent, nil, UsageEvidenceComplete, ProviderModeUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := completeAudit()
			a.ReasoningState, a.ReasoningTokens, a.UsageEvidence, a.ModeState = tc.reasoning, tc.tokens, tc.usage, tc.mode
			a.AuditHash = a.Hash()
			report := CallReport{ProviderAudit: &a}
			if tc.usage == UsageEvidenceComplete {
				usage := UsageReport{InputTokens: 10, OutputTokens: 2, ReceiptHash: Fingerprint("jobforge.deepseek.receipt.v1",
					binding.PhysicalCallID, *a.ResponseSHA256, *a.ResponseID, *a.ResponseModel, *a.SystemFingerprint, "0")}
				usage.UsageHash = usage.Hash()
				report.Usage = &usage
			}
			if valid := report.Validate(binding) == nil; valid != tc.valid {
				t.Fatalf("valid=%v, want=%v", valid, tc.valid)
			}
		})
	}
	absent, zero := completeAudit(), completeAudit()
	zero.ReasoningState, zero.ReasoningTokens = ReasoningObserved, auditPtr(int64(0))
	if absent.Hash() == zero.Hash() {
		t.Fatal("absent reasoning collided with observed zero")
	}
}

func TestProviderAuditStrictDecodeAndEncodedBounds(t *testing.T) {
	audit := completeAudit()
	body, err := ProviderAuditJSON(audit)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeProviderAudit(body)
	if err != nil || !reflect.DeepEqual(decoded, audit) {
		t.Fatalf("round trip: %v", err)
	}
	var nested struct {
		Audit ProviderAudit `json:"audit"`
	}
	if err := json.Unmarshal(append(append([]byte(`{"audit":`), body...), '}'), &nested); err != nil || !reflect.DeepEqual(nested.Audit, audit) {
		t.Fatalf("nested typed audit bypassed the strict decoder: %v", err)
	}
	missing := strings.Replace(string(body), `"reasoning_tokens":null,`, "", 1)
	if err := json.Unmarshal([]byte(`{"audit":`+missing+`}`), &nested); err == nil {
		t.Fatal("nested typed audit accepted a missing nullable field")
	}
	// Raw whitespace counts toward the input bound even though encoding removes it.
	padded := append(bytes.Repeat([]byte(" "), MaxProviderAuditBytes-len(body)), body...)
	if _, err := DecodeProviderAudit(padded); err != nil {
		t.Fatalf("exact size boundary: %v", err)
	}
	if _, err := DecodeProviderAudit(append(padded, ' ')); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatal("raw audit exceeded its bound")
	}
	for _, invalid := range invalidAuditFixtures(t) {
		t.Run(invalid.Name, func(t *testing.T) {
			raw := []byte(invalid.Raw)
			if invalid.RawBase64 != "" {
				raw, err = base64.StdEncoding.DecodeString(invalid.RawBase64)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := DecodeProviderAudit(raw); err == nil {
				t.Fatal("invalid audit accepted")
			}
		})
	}
	for name := range providerAuditFields {
		t.Run("missing-"+name, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			delete(fields, name)
			if _, err := DecodeProviderAudit(auditJSON(t, fields)); err == nil {
				t.Fatal("missing field acquired a default value")
			}
		})
		if !providerAuditFields[name] {
			t.Run("null-"+name, func(t *testing.T) {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				fields[name] = json.RawMessage("null")
				if _, err := DecodeProviderAudit(auditJSON(t, fields)); err == nil {
					t.Fatal("scalar null acquired a default value")
				}
			})
		}
	}
}

func TestProviderReportRejectsWrongReceiptHashBindingAndUnsafeCompleteTotal(t *testing.T) {
	for _, fixture := range invalidReportFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			report, err := DecodeCallReport([]byte(fixture.Raw))
			if err == nil {
				err = report.Verify(fixture.Binding, fixture.ReportHash)
			}
			if err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
	for _, raw := range []string{"null", "{}", `{"usage":null,"provider_audit":null}`, `{"usage":null,"usage":null,"provider_audit":null}`} {
		if _, err := DecodeCallReport([]byte(raw)); err == nil {
			t.Fatal("invalid report shape accepted")
		}
	}
}

func TestProviderFirstReportSeparatesPricingAnomalyAndBusinessValidity(t *testing.T) {
	binding := auditBinding(t)
	price, budget := auditPriceBudget()
	for _, tc := range validReportFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			if err := tc.Report.Verify(tc.Binding, tc.ReportHash); err != nil {
				t.Fatal(err)
			}
			got, err := FirstReportDisposition(tc.Binding, tc.Report, budget, price)
			if err != nil || got != tc.Disposition {
				t.Fatalf("disposition=%+v err=%v, want=%+v", got, err, tc.Disposition)
			}
		})
	}
	audit := completeAudit()
	audit.IdentityState, audit.ResponseModel = ProviderIdentityIncompatible, auditPtr("other-model")
	for _, input := range []int64{10, 101} {
		report := auditReport(t, audit, input, 2)
		result, err := FirstReportDisposition(binding, report, budget, Pricing{})
		if err != nil || result.PriceEligible || result.UsageKnown || result.KnownCostMicroyuan != 0 ||
			result.MeasurementAnomaly != (input > budget.InputTokens) {
			t.Fatalf("incompatible model used invalid original tariff: %+v %v", result, err)
		}
	}
	// Schema rejection is not an input to pricing; it changes only observation.
	report := auditReport(t, completeAudit(), 10, 2)
	for _, accepted := range []bool{true, false} {
		observation := ObserveCallRequest{PhysicalCallID: binding.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200,
			BusinessOutcome: "accepted", UsageKnown: true, Usage: report.Usage}
		if !accepted {
			observation.BusinessOutcome, observation.ErrorCode = "rejected", "MODEL_PROTOCOL_ERROR"
		}
		if _, err := ObservationHashV2(observation, report.ProviderAudit.AuditHash); err != nil {
			t.Fatal(err)
		}
		result, err := FirstReportDisposition(binding, report, budget, price)
		if err != nil || !result.UsageKnown || result.BatchStopCode != "" || result.KnownCostMicroyuan != 8 {
			t.Fatalf("business validity affected legal metering: %+v %v", result, err)
		}
	}
}

func TestProviderBindingIncludesEveryOriginalExecutionField(t *testing.T) {
	lease, step := auditLeaseStep()
	want, err := ExecutionBindingHash(lease, step)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Lease, *StepIdentity){
		func(l *Lease, _ *StepIdentity) { l.TenantID += "x" },
		func(l *Lease, _ *StepIdentity) { l.RunID = "66666666-6666-4666-8666-666666666666" },
		func(l *Lease, _ *StepIdentity) { l.WorkerID += "x" },
		func(l *Lease, _ *StepIdentity) { l.SessionID = "66666666-6666-4666-8666-666666666666" },
		func(l *Lease, _ *StepIdentity) { l.AttemptNo++ },
		func(l *Lease, _ *StepIdentity) { l.FencingToken++ },
		func(_ *Lease, s *StepIdentity) { s.ID = "66666666-6666-4666-8666-666666666666" },
		func(_ *Lease, s *StepIdentity) { s.Sequence++; s.CursorVersion++ },
		func(_ *Lease, s *StepIdentity) { s.Kind = "protocol_correction" },
		func(_ *Lease, s *StepIdentity) { s.InputHash = strings.Repeat("f", 64) },
		func(_ *Lease, s *StepIdentity) { s.ProfileID += "x" },
		func(_ *Lease, s *StepIdentity) { s.ProfileHash = strings.Repeat("f", 64) },
		func(_ *Lease, s *StepIdentity) { s.SnapshotID = "66666666-6666-4666-8666-666666666666" },
		func(_ *Lease, s *StepIdentity) { s.SnapshotHash = strings.Repeat("f", 64) },
	} {
		otherLease, otherStep := lease, step
		mutate(&otherLease, &otherStep)
		got, err := ExecutionBindingHash(otherLease, otherStep)
		if err != nil || got == want {
			t.Fatal("changed original execution was not independently bound")
		}
	}
	step.Sequence++
	if _, err := ExecutionBindingHash(lease, step); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("inconsistent cursor/sequence accepted")
	}
}

func TestProviderAuditExactHashVectors(t *testing.T) {
	// These literals were independently checked with Python hashlib and explicit
	// uint64 big-endian lengths; fixture regeneration cannot silently replace them.
	binding := auditBinding(t)
	report := auditReport(t, completeAudit(), 10, 2)
	receipt, err := report.ProviderAudit.ReceiptHash(binding.PhysicalCallID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"audit", report.ProviderAudit.Hash(), "b4d45c85f5ffc3b880e4b52caee3e01316785299fa1a2025fb40c9c0becbf0e3"},
		{"execution-binding", binding.ExecutionBindingHash, "63e05721d436671f6685a53089ae19c988befc4a5ffbcd827d2d4c5df8ea46ca"},
		{"receipt", receipt, "16375a5e82077d0de9a11e72756567243bc4d12c504b00355aea7721a2a34a5c"},
		{"legacy-usage", report.Usage.Hash(), "feb54145ee5d46899a0ef598e24ae6fe6aa51b2fc72ce2f1bb3bb610ff751dcc"},
		{"report", report.Hash(binding), "74c6204e089238f4b332a7bbf25da8b4f577c60a30ea28407a0d8698c7dd928d"},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s: got %s want %s", tc.name, tc.got, tc.want)
		}
	}
	wantObservations := []string{
		"e0c72308dfba1f9dae7ef39367b77b72122a9de13b0b9d79f913bc45345ae8e0",
		"c4c93010f2e8fcd005cddc4e1ac949a71b58d20c8536dea9b31c6769a6cd8a84",
		"8ee57f9a17bd6fab9a125f55b44645e3e49ae9d01c413be2ed7dccfea2cd2118",
		"2ff0968eda7cfc52e930756d43879ab503ad581e8b9e6d8ffad03725077ec33c",
	}
	for index, observation := range auditObservationFixtures(t) {
		if observation.ObservationHash != wantObservations[index] {
			t.Fatalf("observation %s: %s", observation.Name, observation.ObservationHash)
		}
	}
}

func TestProviderReportExactSafeIntegerAndCostAnomaly(t *testing.T) {
	binding := auditBinding(t)
	price, budget := auditPriceBudget()
	audit := completeAudit()
	audit.Created = auditPtr(MaxSafeInteger)
	audit.ResponseID = auditPtr(strings.Repeat("a", 128))
	audit.SystemFingerprint = auditPtr(strings.Repeat("b", 128))
	report := auditReport(t, audit, MaxSafeInteger, 0)
	body, err := CallReportJSON(report)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCallReport(body)
	if err != nil || decoded.Verify(binding, report.Hash(binding)) != nil || decoded.Usage.InputTokens != MaxSafeInteger {
		t.Fatalf("safe integer or boundary identifier lost: %v", err)
	}
	result, err := FirstReportDisposition(binding, report, budget, price)
	if err != nil || !result.MeasurementAnomaly || result.UsageKnown || result.KnownTokens != 0 {
		t.Fatalf("large valid first report did not preserve hold: %+v %v", result, err)
	}
	for _, tc := range []struct {
		name  string
		price Pricing
		cost  int64
	}{
		{"cost-over-hold", price, 7},
		{"cost-over-safe-integer", Pricing{Denominator: 1, InputMissMicroyuan: MaxSafeInteger}, budget.CostMicroyuan},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budget.CostMicroyuan = tc.cost
			result, err := FirstReportDisposition(binding, auditReport(t, completeAudit(), 10, 2), budget, tc.price)
			if err != nil || !result.PriceEligible || !result.MeasurementAnomaly || result.UsageKnown || result.BatchStopCode != BatchStopMeasurementAnomaly {
				t.Fatalf("priceable anomaly released its hold: %+v %v", result, err)
			}
		})
	}
}

func TestProviderReportRejectsMalformedTypedAndNestedValues(t *testing.T) {
	original := auditReport(t, completeAudit(), 10, 2)
	body := string(auditJSON(t, original))
	for _, raw := range []string{
		strings.Replace(body, `"input_tokens":10`, `"input_tokens":null`, 1),
		strings.Replace(body, `"input_tokens":10`, `"input_tokens":"10"`, 1),
		strings.Replace(body, `"input_tokens":10`, `"input_tokens":10,"input_tokens":10`, 1),
		strings.Replace(body, `"input_tokens":10`, `"input_tokens":10,"extra":false`, 1),
		strings.Replace(body, `"cached_input_tokens":4,`, "", 1),
		strings.Replace(body, `"provider_audit":`, `"extra":false,"provider_audit":`, 1),
		body + "null",
	} {
		if _, err := DecodeCallReport([]byte(raw)); err == nil {
			t.Fatal("nested invalid report accepted")
		}
	}
	if _, err := DecodeCallReport(bytes.Repeat([]byte(" "), MaxCallReportBytes+1)); !errors.Is(err, ErrCheckpointTooLarge) {
		t.Fatal("unbounded report input accepted")
	}
	observation := ObserveCallRequest{PhysicalCallID: auditBinding(t).PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200,
		BusinessOutcome: "rejected", ErrorCode: "OUTPUT_INVALID", UsageKnown: true, Usage: original.Usage}
	if _, err := ObservationHashV2(observation, original.ProviderAudit.AuditHash); err == nil {
		t.Fatal("unmapped wire error entered the domain hash")
	}
}

type auditInvalidFixture struct {
	Name      string `json:"name"`
	Raw       string `json:"raw,omitempty"`
	RawBase64 string `json:"raw_base64,omitempty"`
}

func invalidAuditFixtures(t *testing.T) []auditInvalidFixture {
	t.Helper()
	body := string(auditJSON(t, completeAudit()))
	fixtures := []auditInvalidFixture{
		{Name: "missing-nullable", Raw: strings.Replace(body, `"reasoning_tokens":null,`, "", 1)},
		{Name: "duplicate-key", Raw: strings.Replace(body, `"created":0`, `"created":0,"created":0`, 1)},
		{Name: "unknown-key", Raw: strings.Replace(body, `"created":0`, `"created":0,"extra":0`, 1)},
		{Name: "scalar-null", Raw: strings.Replace(body, `"response_complete":true`, `"response_complete":null`, 1)},
		{Name: "integer-string", Raw: strings.Replace(body, `"created":0`, `"created":"0"`, 1)},
		{Name: "fractional-number", Raw: strings.Replace(body, `"created":0`, `"created":0.0`, 1)},
		{Name: "exponent-number", Raw: strings.Replace(body, `"created":0`, `"created":0e0`, 1)},
		{Name: "boolean-number", Raw: strings.Replace(body, `"created":0`, `"created":false`, 1)},
		{Name: "reasoning-integer-string", Raw: strings.Replace(body, `"reasoning_tokens":null`, `"reasoning_tokens":"0"`, 1)},
		{Name: "reasoning-boolean", Raw: strings.Replace(body, `"reasoning_tokens":null`, `"reasoning_tokens":false`, 1)},
		{Name: "trailing-json", Raw: body + `{}`},
		{Name: "unpaired-surrogate", Raw: strings.Replace(body, `"synthetic-response-1"`, `"\ud800"`, 1)},
		{Name: "oversize-raw", Raw: strings.Repeat(" ", MaxProviderAuditBytes-len(body)+1) + body},
		{Name: "invalid-utf8", RawBase64: base64.StdEncoding.EncodeToString(bytes.Replace([]byte(body), []byte("synthetic-response-1"), []byte{0xff}, 1))},
	}
	mutations := []struct {
		name   string
		mutate func(*ProviderAudit)
	}{
		{"missing-created-identity", func(a *ProviderAudit) { a.Created = nil }},
		{"empty-identifier", func(a *ProviderAudit) { a.ResponseID = auditPtr("") }},
		{"overlong-identifier", func(a *ProviderAudit) { a.ResponseID = auditPtr(strings.Repeat("a", 129)) }},
		{"unsafe-created", func(a *ProviderAudit) { a.Created = auditPtr(MaxSafeInteger + 1) }},
		{"negative-created", func(a *ProviderAudit) { a.Created = auditPtr(int64(-1)) }},
		{"uppercase-response-hash", func(a *ProviderAudit) { a.ResponseSHA256 = auditPtr(strings.Repeat("A", 64)) }},
		{"unknown-enum", func(a *ProviderAudit) { a.IdentityState = "COMPATIBLE" }},
		{"wrong-provider", func(a *ProviderAudit) { a.Provider = "other" }},
		{"wrong-version", func(a *ProviderAudit) { a.SchemaVersion = 2 }},
		{"complete-with-zero-http-status", func(a *ProviderAudit) { a.HTTPStatus = 0 }},
		{"complete-with-invalid-http-status", func(a *ProviderAudit) { a.HTTPStatus = 600 }},
		{"complete-with-null-body-hash", func(a *ProviderAudit) { a.ResponseSHA256 = nil }},
		{"complete200-with-unavailable-identity", func(a *ProviderAudit) { a.IdentityState = ProviderIdentityUnavailable }},
		{"incomplete-with-http-status", func(a *ProviderAudit) { *a = incompleteAudit(); a.HTTPStatus = 200 }},
		{"non200-with-identity", func(a *ProviderAudit) { a.HTTPStatus = 429 }},
		{"observed-null", func(a *ProviderAudit) { a.ReasoningState = ReasoningObserved }},
		{"absent-zero", func(a *ProviderAudit) { a.ReasoningTokens = auditPtr(int64(0)) }},
		{"positive-nonthinking", func(a *ProviderAudit) { a.ReasoningState = ReasoningObserved; a.ReasoningTokens = auditPtr(int64(1)) }},
		{"invalid-reasoning-with-complete-usage", func(a *ProviderAudit) { a.ReasoningState = ReasoningInvalid; a.ModeState = ProviderModeInvalid }},
		{"isolated-reasoning", func(a *ProviderAudit) {
			a.UsageEvidence, a.ReasoningState, a.ReasoningTokens, a.ModeState = UsageEvidenceInvalid, ReasoningObserved, auditPtr(int64(0)), ProviderModeUnavailable
		}},
	}
	for _, mutation := range mutations {
		audit := completeAudit()
		mutation.mutate(&audit)
		audit.AuditHash = audit.Hash()
		fixtures = append(fixtures, auditInvalidFixture{Name: mutation.name, Raw: string(auditJSON(t, audit))})
	}
	badHash := completeAudit()
	badHash.AuditHash = strings.Repeat("f", 64)
	return append(fixtures, auditInvalidFixture{Name: "wrong-audit-hash", Raw: string(auditJSON(t, badHash))})
}

type reportFixture struct {
	Name        string            `json:"name"`
	Binding     ReportBinding     `json:"binding"`
	Report      CallReport        `json:"report"`
	ReportHash  string            `json:"report_hash"`
	ReceiptHash string            `json:"receipt_hash"`
	Disposition ReportDisposition `json:"disposition"`
}

func validReportFixtures(t *testing.T) []reportFixture {
	t.Helper()
	settled := ReportDisposition{PriceEligible: true, UsageKnown: true, KnownTokens: 12, KnownCostMicroyuan: 8, Settlement: "settled"}
	recorded := ReportDisposition{Settlement: "recorded", BatchStopCode: BatchStopChatUsageUnknown}
	var fixtures []reportFixture
	add := func(name string, audit ProviderAudit, input, output int64, disposition ReportDisposition) {
		report := auditReport(t, audit, input, output)
		receipt := ""
		if report.Usage != nil {
			receipt = report.Usage.ReceiptHash
		}
		binding := auditBinding(t)
		fixtures = append(fixtures, reportFixture{Name: name, Binding: binding, Report: report,
			ReportHash: report.Hash(binding), ReceiptHash: receipt, Disposition: disposition})
	}
	add("reasoning-absent", completeAudit(), 10, 2, settled)
	add("business-schema-rejected-metering-unchanged", completeAudit(), 10, 2, settled)
	audit := completeAudit()
	audit.ReasoningState, audit.ReasoningTokens = ReasoningObserved, auditPtr(int64(0))
	add("reasoning-explicit-zero", audit, 10, 2, settled)
	audit.ReasoningTokens, audit.ModeState = auditPtr(int64(2)), ProviderModeUnexpected
	modeStop := settled
	modeStop.BatchStopCode = BatchStopProviderModeInvalid
	add("reasoning-positive", audit, 10, 2, modeStop)
	audit = completeAudit()
	audit.ModeState = ProviderModeUnexpected
	add("unexpected-thinking-or-tools", audit, 10, 2, modeStop)
	audit.ModeState = ProviderModeInvalid
	add("invalid-choice-complete-usage", audit, 10, 2, modeStop)
	audit = completeAudit()
	audit.IdentityState, audit.ResponseModel = ProviderIdentityIncompatible, auditPtr("other-model")
	identityStop := recorded
	identityStop.BatchStopCode = BatchStopProviderIdentityInvalid
	add("incompatible-complete-usage", audit, 10, 2, identityStop)
	add("incompatible-token-anomaly", audit, 101, 2, ReportDisposition{MeasurementAnomaly: true, Settlement: "anomaly", BatchStopCode: BatchStopMeasurementAnomaly})
	add("compatible-token-anomaly", completeAudit(), 101, 2, ReportDisposition{PriceEligible: true, MeasurementAnomaly: true, Settlement: "anomaly", BatchStopCode: BatchStopMeasurementAnomaly})
	audit = completeAudit()
	audit.IdentityState, audit.ResponseID = ProviderIdentityInvalid, nil
	audit.UsageEvidence, audit.ReasoningState, audit.ModeState = UsageEvidenceUnavailable, ReasoningUnavailable, ProviderModeUnavailable
	add("invalid-partial-identity", audit, 0, 0, identityStop)
	audit.UsageEvidence = UsageEvidenceAbsent
	add("invalid-identity-with-absent-usage", audit, 0, 0, identityStop)
	audit.UsageEvidence = UsageEvidenceInvalid
	add("invalid-identity-with-invalid-usage", audit, 0, 0, identityStop)
	audit.UsageEvidence = UsageEvidenceUnavailable
	audit.ReasoningState, audit.ModeState = ReasoningInvalid, ProviderModeInvalid
	add("invalid-identity-with-invalid-reasoning", audit, 0, 0, identityStop)
	audit = completeAudit()
	audit.UsageEvidence, audit.ReasoningState, audit.ModeState = UsageEvidenceInvalid, ReasoningInvalid, ProviderModeInvalid
	invalidReasoningStop := recorded
	invalidReasoningStop.BatchStopCode = BatchStopProviderModeInvalid
	add("reasoning-invalid", audit, 0, 0, invalidReasoningStop)
	audit.UsageEvidence, audit.ReasoningState, audit.ModeState = UsageEvidenceAbsent, ReasoningUnavailable, ProviderModeUnavailable
	add("usage-absent", audit, 0, 0, recorded)
	audit.UsageEvidence = UsageEvidenceInvalid
	add("usage-invalid", audit, 0, 0, recorded)
	audit.UsageEvidence = UsageEvidenceUnavailable
	add("complete-identity-unavailable-usage", audit, 0, 0, recorded)
	add("incomplete-response", incompleteAudit(), 0, 0, recorded)
	audit = incompleteAudit()
	audit.ResponseComplete, audit.HTTPStatus, audit.ResponseSHA256 = true, 429, auditPtr(strings.Repeat("e", 64))
	httpStop := recorded
	httpStop.BatchStopCode = BatchStopProviderHTTPRejected
	add("complete-http-rejected", audit, 0, 0, httpStop)
	embeddingBinding := auditBinding(t)
	embeddingBinding.Subcall, embeddingBinding.ExpectedResponseModel = SubcallQueryEmbedding, ""
	usage := UsageReport{InputTokens: 10, ReceiptHash: strings.Repeat("f", 64)}
	usage.UsageHash = usage.Hash()
	embedding := CallReport{Usage: &usage}
	fixtures = append(fixtures, reportFixture{Name: "embedding-no-provider-audit", Binding: embeddingBinding, Report: embedding,
		ReportHash: embedding.Hash(embeddingBinding), ReceiptHash: usage.ReceiptHash,
		Disposition: ReportDisposition{PriceEligible: true, UsageKnown: true, KnownTokens: 10, Settlement: "settled"}})
	return fixtures
}

type invalidReportFixture struct {
	Name       string        `json:"name"`
	Binding    ReportBinding `json:"binding"`
	Raw        string        `json:"raw"`
	ReportHash string        `json:"report_hash"`
}

func invalidReportFixtures(t *testing.T) []invalidReportFixture {
	t.Helper()
	binding := auditBinding(t)
	var fixtures []invalidReportFixture
	add := func(name string, report CallReport, bound ReportBinding, reportHash string) {
		fixtures = append(fixtures, invalidReportFixture{Name: name, Binding: bound, Raw: string(auditJSON(t, report)), ReportHash: reportHash})
	}
	original := auditReport(t, completeAudit(), 10, 2)
	for _, tc := range []struct {
		name   string
		mutate func(*CallReport)
	}{
		{"wrong-receipt", func(r *CallReport) { r.Usage.ReceiptHash = strings.Repeat("f", 64); r.Usage.UsageHash = r.Usage.Hash() }},
		{"wrong-usage-hash", func(r *CallReport) { r.Usage.UsageHash = strings.Repeat("f", 64) }},
		{"complete-with-null-usage", func(r *CallReport) { r.Usage = nil }},
		{"chat-with-null-audit", func(r *CallReport) { r.ProviderAudit = nil }},
		{"unsafe-complete-total", func(r *CallReport) { r.Usage.InputTokens = MaxSafeInteger; r.Usage.UsageHash = r.Usage.Hash() }},
		{"reasoning-over-completion", func(r *CallReport) {
			r.ProviderAudit.ReasoningState, r.ProviderAudit.ReasoningTokens, r.ProviderAudit.ModeState = ReasoningObserved, auditPtr(int64(3)), ProviderModeUnexpected
			r.ProviderAudit.AuditHash = r.ProviderAudit.Hash()
		}},
		{"unknown-with-usage", func(r *CallReport) {
			r.ProviderAudit.UsageEvidence, r.ProviderAudit.ReasoningState, r.ProviderAudit.ModeState = UsageEvidenceAbsent, ReasoningUnavailable, ProviderModeUnavailable
			r.ProviderAudit.AuditHash = r.ProviderAudit.Hash()
		}},
	} {
		report := auditReport(t, completeAudit(), 10, 2)
		tc.mutate(&report)
		add(tc.name, report, binding, report.Hash(binding))
	}
	add("wrong-report-hash", original, binding, strings.Repeat("f", 64))
	for _, tc := range []struct {
		name   string
		mutate func(*ReportBinding)
	}{
		{"wrong-execution-binding", func(b *ReportBinding) { b.ExecutionBindingHash = strings.Repeat("f", 64) }},
		{"wrong-parameter-binding", func(b *ReportBinding) { b.ParameterHash = strings.Repeat("f", 64) }},
		{"wrong-physical-call", func(b *ReportBinding) { b.PhysicalCallID = "66666666-6666-4666-8666-666666666666" }},
		{"wrong-expected-model", func(b *ReportBinding) { b.ExpectedResponseModel = "other-model" }},
		{"nonchat-with-audit", func(b *ReportBinding) { b.Subcall = SubcallQueryEmbedding }},
		{"free-call-with-report", func(b *ReportBinding) { b.Subcall = SubcallGetOrder }},
	} {
		bound := binding
		tc.mutate(&bound)
		add(tc.name, original, bound, original.Hash(binding))
	}
	return fixtures
}

type observationAuditFixture struct {
	Name             string       `json:"name"`
	PhysicalCallID   string       `json:"physical_call_id"`
	TransportOutcome string       `json:"transport_outcome"`
	HTTPStatus       int          `json:"http_status"`
	ErrorCode        string       `json:"error_code"`
	BusinessOutcome  string       `json:"business_outcome"`
	Usage            *UsageReport `json:"usage"`
	AuditHash        string       `json:"audit_hash"`
	ObservationHash  string       `json:"observation_hash"`
}

func auditObservationFixtures(t *testing.T) []observationAuditFixture {
	t.Helper()
	report := auditReport(t, completeAudit(), 10, 2)
	var fixtures []observationAuditFixture
	for _, tc := range []struct {
		name, transport, business, code string
		status                          int
		usage                           *UsageReport
		auditHash                       string
	}{
		{"accepted", "response", "accepted", "", 200, report.Usage, report.ProviderAudit.AuditHash},
		{"schema-rejected", "response", "rejected", "MODEL_PROTOCOL_ERROR", 200, report.Usage, report.ProviderAudit.AuditHash},
		{"unknown-chat", "unknown", "unknown", "TIMEOUT", 0, nil, incompleteAudit().AuditHash},
		{"free-response", "response", "accepted", "", 200, nil, ""},
	} {
		observation := ObserveCallRequest{PhysicalCallID: auditBinding(t).PhysicalCallID, TransportOutcome: tc.transport,
			HTTPStatus: tc.status, ErrorCode: tc.code, BusinessOutcome: tc.business, UsageKnown: tc.usage != nil, Usage: tc.usage}
		hash, err := ObservationHashV2(observation, tc.auditHash)
		if err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, observationAuditFixture{Name: tc.name, PhysicalCallID: observation.PhysicalCallID,
			TransportOutcome: tc.transport, HTTPStatus: tc.status, ErrorCode: tc.code, BusinessOutcome: tc.business,
			Usage: tc.usage, AuditHash: tc.auditHash, ObservationHash: hash})
	}
	return fixtures
}

func TestProviderAuditFixture(t *testing.T) {
	lease, step := auditLeaseStep()
	flatBinding := map[string]any{"tenant_id": lease.TenantID, "run_id": lease.RunID, "worker_id": lease.WorkerID,
		"session_id": lease.SessionID, "attempt_no": lease.AttemptNo, "fencing_token": lease.FencingToken,
		"step_id": step.ID, "step_sequence": step.Sequence, "step_kind": step.Kind, "cursor_version": step.CursorVersion,
		"input_hash": step.InputHash, "profile_id": step.ProfileID, "profile_hash": step.ProfileHash,
		"snapshot_id": step.SnapshotID, "snapshot_hash": step.SnapshotHash}
	price, budget := auditPriceBudget()
	fixture := map[string]any{"schema_version": 1, "price": price, "budget": budget,
		"binding_vectors": []any{map[string]any{"name": "original-reservation", "binding": flatBinding, "execution_binding_hash": auditBinding(t).ExecutionBindingHash}},
		"valid_reports":   validReportFixtures(t), "invalid_audits": invalidAuditFixtures(t),
		"invalid_reports": invalidReportFixtures(t), "observations": auditObservationFixtures(t)}
	body, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	path := filepath.Join("..", "..", "api", "executor", "v2", "fixtures", "provider-audit.json")
	if *updateProviderAuditFixture {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("provider audit fixture differs; regenerate with go test ./internal/run -run TestProviderAuditFixture -update-provider-audit-fixture: %v", err)
	}
	// Consume the written typed data too; a serialization issue is not hidden by
	// comparing the generator's bytes with themselves during an explicit update.
	var loaded struct {
		ValidReports []reportFixture `json:"valid_reports"`
	}
	if err := json.Unmarshal(actual, &loaded); err != nil {
		t.Fatal(err)
	}
	for _, entry := range loaded.ValidReports {
		if err := entry.Report.Verify(entry.Binding, entry.ReportHash); err != nil {
			t.Fatalf("shared report %s: %v", entry.Name, err)
		}
	}
}
