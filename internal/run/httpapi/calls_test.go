package httpapi

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/run"
)

var updateCallFixtures = flag.Bool("update-call-fixtures", false, "regenerate public call audit fixtures")

func TestCallEvidenceFixtures(t *testing.T) {
	values := callEvidenceFixtures()
	body, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	path := "../../../api/run/v2/calls-fixtures.json"
	if *updateCallFixtures {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != string(body) {
		t.Fatal("public call fixtures differ; regenerate from Go source")
	}
	for name, value := range values {
		t.Run(name, func(t *testing.T) {
			router, api := newTestRouter(t)
			api.fixture.CallReport = value
			for _, role := range []string{"reader", "operator"} {
				response := serve(router, request("GET", "/v2/runs/"+api.fixture.Run.ID+"/calls", role, nil))
				var got run.CallsResponse
				if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &got) != nil {
					t.Fatalf("call evidence response status=%d", response.Code)
				}
				wantJSON, _ := json.Marshal(value)
				gotJSON, _ := json.Marshal(got)
				if string(wantJSON) != string(gotJSON) || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("call evidence differs or permits caching")
				}
			}
		})
	}
}

func TestCallEvidenceRejectsForeignParametersAndOversize(t *testing.T) {
	router, api := newTestRouter(t)
	base := "/v2/runs/" + api.fixture.Run.ID + "/calls"
	for _, role := range []string{"foreign", "outsider"} {
		assertCode(t, serve(router, request("GET", base, role, nil)), 404, "NOT_FOUND")
	}
	assertCode(t, serve(router, request("GET", base, "", nil)), 401, "UNAUTHORIZED")
	for _, query := range []string{"?limit=1", "?cursor=x", "?tenant=other", "?after=0"} {
		assertCode(t, serve(router, request("GET", base+query, "reader", nil)), 400, "INVALID_ARGUMENT")
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		assertCode(t, serve(router, request(method, base, "operator", nil)), 400, "INVALID_ARGUMENT")
	}
	api.fixture.CallReport.Items = make([]run.CallView, 45)
	assertCode(t, serve(router, request("GET", base, "reader", nil)), 500, "INTERNAL")
	api.fixture.CallReport.Items = []run.CallView{{ProfileID: strings.Repeat("x", run.MaxCallsResponseBytes)}}
	response := serve(router, request("GET", base, "reader", nil))
	assertCode(t, response, 500, "INTERNAL")
	if response.Body.Len() > 1024 {
		t.Fatal("oversized audit was partially returned")
	}
	api.fixture.CallReport.Items = nil
	response = serve(router, request("GET", base, "reader", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"items":[]`) {
		t.Fatal("empty calls must be an array")
	}
}

func callEvidenceFixtures() map[string]run.CallsResponse {
	now := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	base := run.CallView{
		PhysicalCallID: "11111111-1111-4111-8111-111111111111", StepID: "22222222-2222-4222-8222-222222222222",
		StepKind: "model_proposal", AttemptNo: 1, Ordinal: 1, Subcall: run.SubcallChat,
		ParameterHash: strings.Repeat("a", 64), ProfileID: "synthetic-audited-profile", ProfileHash: strings.Repeat("b", 64), PriceHash: strings.Repeat("c", 64),
		ReservedAt: now, DispatchExpiresAt: now.Add(time.Second), CallDeadline: now.Add(time.Minute),
		Reserved:   run.CallBudget{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CostMicroyuan: 1000},
		HeldTokens: 150, HeldCostMicroyuan: 1000, AuditStatus: "missing",
	}
	wrap := func(call run.CallView) run.CallsResponse {
		return run.CallsResponse{RunID: "33333333-3333-4333-8333-333333333333", CapturedAt: now.Add(2 * time.Minute), Items: []run.CallView{call}}
	}
	values := map[string]run.CallsResponse{"missing": wrap(base)}
	legacy := base
	legacy.AuditStatus = "legacy_not_collected"
	values["legacy"] = wrap(legacy)
	free := base
	free.StepKind, free.Subcall, free.AuditStatus = "get_order", run.SubcallGetOrder, "not_applicable"
	free.Reserved, free.HeldTokens, free.HeldCostMicroyuan = run.CallBudget{}, 0, 0
	values["free"] = wrap(free)
	unknown := base
	unknown.ProviderAudit = &run.ProviderAudit{SchemaVersion: 1, Provider: "deepseek", IdentityState: run.ProviderIdentityUnavailable,
		UsageEvidence: run.UsageEvidenceUnavailable, ReasoningState: run.ReasoningUnavailable, ModeState: run.ProviderModeUnavailable}
	unknown.ProviderAudit.AuditHash = unknown.ProviderAudit.Hash()
	unknown.AuditHash, unknown.ReportRecordedAt, unknown.AuditStatus = &unknown.ProviderAudit.AuditHash, &now, "recorded"
	unknown.ReportHash = callPtr(run.CallReport{ProviderAudit: unknown.ProviderAudit}.Hash(run.ReportBinding{
		ExecutionBindingHash: strings.Repeat("d", 64), PhysicalCallID: base.PhysicalCallID, ParameterHash: base.ParameterHash,
		Subcall: run.SubcallChat, ExpectedResponseModel: "deepseek-flash"}))
	values["unavailable"] = wrap(unknown)
	for _, model := range []string{"deepseek-flash", "incompatible-model"} {
		call := base
		audit := run.ProviderAudit{SchemaVersion: 1, Provider: "deepseek", ResponseComplete: true, HTTPStatus: 200,
			ResponseSHA256: callPtr(strings.Repeat("e", 64)), IdentityState: run.ProviderIdentityCompatible,
			ResponseID: callPtr("synthetic-response"), ResponseModel: &model, SystemFingerprint: callPtr("synthetic-fingerprint"), Created: callPtr(int64(1789560000)),
			UsageEvidence: run.UsageEvidenceComplete, ReasoningState: run.ReasoningObserved, ReasoningTokens: callPtr(int64(0)), ModeState: run.ProviderModeNonthinking}
		name := "recorded"
		if model != "deepseek-flash" {
			audit.IdentityState, name = run.ProviderIdentityIncompatible, "incompatible"
		}
		audit.AuditHash = audit.Hash()
		receipt, _ := audit.ReceiptHash(base.PhysicalCallID)
		usage := run.UsageReport{InputTokens: 20, OutputTokens: 10, CachedInputTokens: 5, ReceiptHash: receipt}
		usage.UsageHash = usage.Hash()
		call.ProviderAudit, call.AuditHash, call.ObservedUsage = &audit, &audit.AuditHash, &usage
		call.ReportRecordedAt, call.AuditStatus = &now, "recorded"
		call.ReportHash = callPtr(run.CallReport{Usage: &usage, ProviderAudit: &audit}.Hash(run.ReportBinding{
			ExecutionBindingHash: strings.Repeat("d", 64), PhysicalCallID: base.PhysicalCallID, ParameterHash: base.ParameterHash,
			Subcall: run.SubcallChat, ExpectedResponseModel: "deepseek-flash"}))
		if name == "recorded" {
			call.UsageKnown, call.SettledUsage, call.SettledAt = true, &usage, &now
			call.HeldTokens, call.HeldCostMicroyuan, call.KnownTokens, call.KnownCostMicroyuan = 0, 0, 30, 200
			call.ObservedAt, call.TransportOutcome, call.HTTPStatus = &now, callPtr("response"), callPtr(200)
			call.BusinessOutcome, call.ErrorCode = callPtr("rejected"), callPtr("MODEL_PROTOCOL_ERROR")
		}
		response := wrap(call)
		if name == "incompatible" {
			response.BatchFrozen, response.BatchStopCode = true, callPtr(run.BatchStopCode("PROVIDER_IDENTITY_INVALID"))
		}
		values[name] = response
	}
	empty := wrap(base)
	empty.Items = []run.CallView{}
	values["empty"] = empty
	return values
}

func callPtr[T any](value T) *T { return &value }
