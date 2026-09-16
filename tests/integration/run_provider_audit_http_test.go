package integration

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
)

// TestRunProviderAuditPythonHTTPContract reads actual committed call rows via
// the installed SDK and production HTTP router; provider facts are synthetic.
func TestRunProviderAuditPythonHTTPContract(t *testing.T) {
	python := os.Getenv("JOBFORGE_TEST_PYTHON")
	if python == "" {
		t.Skip("JOBFORGE_TEST_PYTHON unset: installed audit SDK HTTP contract not exercised")
	}
	for _, scenario := range []string{"missing", "known_rejected", "incompatible", "unavailable", "conflict"} {
		t.Run(scenario, func(t *testing.T) {
			h := setupAuditHarness(t)
			claimed := auditAtModelStep(t, h, "tenant-a", "public-audit-"+scenario)
			request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			ledgerReserve(t, h, request)
			if scenario != "missing" {
				report := auditReportRequest(t, h, request, 20, 10)
				if scenario == "incompatible" {
					report.ProviderAudit.IdentityState = agentrun.ProviderIdentityIncompatible
					report.ProviderAudit.ResponseModel = auditValue("different-model")
					report = auditBindReport(t, h, request, agentrun.CallReport{Usage: report.Usage, ProviderAudit: report.ProviderAudit})
				}
				if scenario == "unavailable" {
					audit := &agentrun.ProviderAudit{SchemaVersion: 1, Provider: "deepseek", IdentityState: agentrun.ProviderIdentityUnavailable,
						UsageEvidence: agentrun.UsageEvidenceUnavailable, ReasoningState: agentrun.ReasoningUnavailable, ModeState: agentrun.ProviderModeUnavailable}
					report = auditBindReport(t, h, request, agentrun.CallReport{ProviderAudit: audit})
				}
				if _, err := h.Store.SettleUsage(h.Ctx, h.Principal, report); err != nil {
					t.Fatal(err)
				}
				if scenario == "known_rejected" {
					auditObserve(t, h, request, report, "rejected")
				}
				if scenario == "conflict" {
					conflict := auditReportRequest(t, h, request, 21, 10)
					result, err := h.Store.SettleUsage(h.Ctx, h.Principal, conflict)
					if err != nil || !result.ReportConflict {
						t.Fatalf("report conflict was not retained: %v", err)
					}
				}
			}
			router, err := httpapi.NewRouter(h.Service, map[string]httpapi.Identity{
				"audit-reader": {TenantID: "tenant-a", Role: "reader"}, "audit-operator": {TenantID: "tenant-a", Role: "operator"},
				"audit-foreign": {TenantID: "tenant-b", Role: "reader"},
			})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			root, err := filepath.Abs(filepath.Join("..", ".."))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(h.Ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, filepath.Join(root, "sdk", "python", "tests", "run_audit_http_contract.py"),
				server.URL, claimed.Checkpoint.Run.ID, request.PhysicalCallID, scenario)
			cmd.Dir, cmd.WaitDelay = root, 2*time.Second
			for _, entry := range os.Environ() {
				name, _, ok := strings.Cut(entry, "=")
				if !ok {
					continue
				}
				switch strings.ToUpper(name) {
				case "PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE", "LANG", "LC_ALL":
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, "PYTHONIOENCODING=utf-8", "PYTHONDONTWRITEBYTECODE=1")
			output := &runContractOutput{}
			cmd.Stdout, cmd.Stderr = output, output
			if err := cmd.Run(); err != nil {
				t.Fatalf("installed audit SDK contract failed: %v\n%s", err, output.String())
			}
			if output.overflow || strings.TrimSpace(output.String()) != "PASS real HTTP audit SDK: "+scenario {
				t.Fatal("audit SDK did not produce its bounded completion marker")
			}
		})
	}
}
