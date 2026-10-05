package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/run/httpapi"
)

func approvalHTTPKeys() map[string]httpapi.Identity {
	return map[string]httpapi.Identity{
		"approval-reader":           {TenantID: "tenant-north", Role: "reader"},
		"approval-operator":         {TenantID: "tenant-north", Role: "operator"},
		"approval-approver":         {TenantID: "tenant-north", Role: "approver", ActorID: "stable-approver"},
		"approval-rotated":          {TenantID: "tenant-north", Role: "approver", ActorID: "stable-approver"},
		"approval-other":            {TenantID: "tenant-north", Role: "approver", ActorID: "other-approver"},
		"approval-foreign":          {TenantID: "tenant-south", Role: "reader"},
		"approval-foreign-operator": {TenantID: "tenant-south", Role: "operator"},
	}
}

func runApprovalSDK(t *testing.T, h *runHarness, args ...string) {
	t.Helper()
	python := os.Getenv("JOBFORGE_TEST_PYTHON")
	if python == "" {
		t.Skip("JOBFORGE_TEST_PYTHON unset: installed approval SDK HTTP not exercised")
	}
	router, err := httpapi.NewRouter(h.Service, approvalHTTPKeys())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(h.Ctx, 30*time.Second)
	defer cancel()
	command := append([]string{filepath.Join(root, "sdk", "python", "tests", "run_approval_http_contract.py"), server.URL}, args...)
	cmd := exec.CommandContext(ctx, python, command...)
	cmd.Dir = root
	cmd.WaitDelay = 2 * time.Second
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
		t.Fatalf("installed approval SDK HTTP: %v\n%s", err, output.String())
	}
	if output.overflow || strings.TrimSpace(output.String()) != "PASS real HTTP approval SDK: "+args[0] {
		t.Fatal("missing bounded SDK marker")
	}
}

func TestRunApprovalPythonHTTPContract(t *testing.T) {
	h := setupApprovalHarness(t)
	approved := approvalPending(t, h, "sdk-approved")
	rejected := approvalPending(t, h, "sdk-rejected")
	runApprovalSDK(t, h, "decisions", approved.ID, rejected.ID)
	// Current authorization precedes replay of the already accepted operation.
	keys := approvalHTTPKeys()
	keys["approval-approver"] = httpapi.Identity{TenantID: "tenant-north", Role: "operator"}
	router, err := httpapi.NewRouter(h.Service, keys)
	if err != nil {
		t.Fatal(err)
	}
	view, err := h.Store.Approval(h.Ctx, approved.TenantID, approved.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"schema_version": 1, "decision": "approve", "proposal_hash": view.ProposalHash})
	for _, test := range []struct {
		key    string
		status int
	}{{"approval-approver", 403}, {"approval-foreign", 404}, {"removed-key", 401}} {
		req := httptest.NewRequest(http.MethodPost, "/v2/runs/"+approved.ID+"/approval", strings.NewReader(string(body)))
		req.Header.Set("Authorization", "Bearer "+test.key)
		req.Header.Set("Idempotency-Key", "sdk-approve")
		req.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		if response.Code != test.status {
			t.Fatalf("current role/tenant replay: %d want %d", response.Code, test.status)
		}
	}
}
