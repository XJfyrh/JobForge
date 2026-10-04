package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
)

func TestRunRecoveryConfirmedPendingAndSuccessorCommit(t *testing.T) {
	h := setupRecoveryHarness(t)
	old := recoveryAtDecision(t, h, "confirmed-recovery")
	step := currentRunStep(old)
	request, report := recoveryChat(t, h, old, true)
	before := ledgerView(t, h, old.Lease)
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, step, "TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	r := recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
	if r.RecoveryCount != 1 || r.AttemptNo != 1 || !reflect.DeepEqual(before.Budget, r.Budget) {
		t.Fatal("closure reset cumulative exposure")
	}
	var raw []byte
	var ordinal int64
	if err := h.Pool.QueryRow(h.Ctx, "select recovery_step,recovery_ordinal from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&raw, &ordinal); err != nil {
		t.Fatal(err)
	}
	proof, err := agentrun.DecodeRecoveryStep(raw)
	if err != nil || proof != step || ordinal != 1 {
		t.Fatalf("wrong closing proof: %v", err)
	}
	claimed := h.claim(t)
	if claimed.Lease.AttemptNo != 2 || claimed.Lease.FencingToken != old.Lease.FencingToken+1 || currentRunStep(claimed) != step || len(claimed.Checkpoint.Steps) != 1 {
		t.Fatal("Claim changed the pending step or prefix")
	}
	newCall := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, newCall)
	activeBefore := ledgerView(t, h, claimed.Lease)
	if _, err := h.Store.SettleUsage(h.Ctx, h.Principal, report); err != nil {
		t.Fatal("identical old report lost narrow replay with a new active call")
	}
	var active string
	if err := h.Pool.QueryRow(h.Ctx, "select active_call_id::text from runs where run_id=$1", r.ID).Scan(&active); err != nil || active != newCall.PhysicalCallID || !reflect.DeepEqual(activeBefore.Budget, ledgerView(t, h, claimed.Lease).Budget) {
		t.Fatal("old report replay changed the new active call or accounts")
	}
	if _, err := h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: old.Lease, Step: step, PhysicalCallID: request.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200, BusinessOutcome: "accepted"}); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("old observation: %v", err)
	}
	newReport := auditReportRequest(t, h, newCall, 10, 5)
	auditSettle(t, h, newReport)
	auditObserve(t, h, newCall, newReport, "accepted")
	commit := recoveryCommit(t, h, &claimed, recoveryToolDecision(t, claimed, newCall.PhysicalCallID, "get_order"))
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, commit); err != nil {
		t.Fatal("exact duplicate changed commit")
	}
	if _, err := h.Store.CommitStep(h.Ctx, h.Principal, agentrun.CommitStepRequest{Lease: old.Lease, Step: commit.Step, ResultJSON: commit.ResultJSON, CommitHash: commit.CommitHash}); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("old commit: %v", err)
	}
	recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	nextCall, _ := recoveryChat(t, h, claimed, true)
	recoveryCommit(t, h, &claimed, recoveryToolDecision(t, claimed, nextCall.PhysicalCallID, "search_policy"))
	if claimed.Checkpoint.Run.CursorVersion != 4 {
		t.Fatal("successor did not advance two more durable steps")
	}
	// Even another valid committed chat from this same Run/attempt cannot stand
	// in for the successor call of the recovered logical step. Recompute the
	// wrong step's hash so rejection must rely on the full cross-step binding.
	var wrongResult agentrun.StepResult
	if err := json.Unmarshal(commit.ResultJSON, &wrongResult); err != nil {
		t.Fatal(err)
	}
	wrongResult.PhysicalCallID = nextCall.PhysicalCallID
	wrongJSON, _ := json.Marshal(wrongResult)
	_, wrongCanonical, err := agentrun.CanonicalRegisteredStepResult(wrongJSON, step.Kind)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update run_steps set output=$3,commit_hash=$4 where run_id=$1 and step_id=$2", r.ID, step.ID, wrongCanonical, agentrun.CommitHash(step, wrongCanonical)); err != nil {
		t.Fatal(err)
	}
	blockedBefore := ledgerView(t, h, claimed.Lease)
	if _, err := h.Store.BeginTool(h.Ctx, h.Principal, agentrun.BeginToolRequest{Lease: claimed.Lease, Step: currentRunStep(claimed), ToolInvocationID: uuid.NewString()}); !errors.Is(err, agentrun.ErrBudgetExhausted) {
		t.Fatalf("another committed chat rescued wrong step: %v", err)
	}
	if !reflect.DeepEqual(blockedBefore.Budget, ledgerView(t, h, claimed.Lease).Budget) {
		t.Fatal("wrong successor charged a tool")
	}
	if _, err := h.Pool.Exec(h.Ctx, "update run_steps set output=$3,commit_hash=$4 where run_id=$1 and step_id=$2", r.ID, step.ID, commit.ResultJSON, commit.CommitHash); err != nil {
		t.Fatal(err)
	}
	// A new tool guard reads the old proof after next_step and cursor moved.
	result := supportStorageResult(t, h, claimed, "proposal")
	recoveryCommit(t, h, &claimed, result)
	after := ledgerView(t, h, claimed.Lease)
	if after.Budget.Family.KnownCostMicroyuan <= before.Budget.Family.KnownCostMicroyuan || after.Budget.Family.Used.Chat != 3 {
		t.Fatal("recovery erased old charge")
	}
	calls, err := h.Store.Calls(h.Ctx, r.TenantID, r.ID)
	if err != nil || calls.BatchFrozen {
		t.Fatalf("successor audit: %v", err)
	}
	var committedCall string
	if err := h.Pool.QueryRow(h.Ctx, "select output->>'physical_call_id' from run_steps where run_id=$1 and step_id=$2", r.ID, step.ID).Scan(&committedCall); err != nil {
		t.Fatal(err)
	}
	if committedCall != newCall.PhysicalCallID || committedCall == request.PhysicalCallID {
		t.Fatal("old call masqueraded as successor checkpoint")
	}
	recoverySDKHTTP(t, h, r.ID, request.PhysicalCallID, newCall.PhysicalCallID, step.ID)
}

func recoverySDKHTTP(t *testing.T, h *runHarness, runID, oldCall, newCall, stepID string) {
	t.Helper()
	python := os.Getenv("JOBFORGE_TEST_PYTHON")
	if python == "" {
		t.Skip("installed recovery SDK HTTP contract requires JOBFORGE_TEST_PYTHON; skip is not acceptance")
	}
	router, err := httpapi.NewRouter(h.Service, map[string]httpapi.Identity{"recovery-reader": {TenantID: "tenant-north", Role: "reader"}})
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
	command := exec.CommandContext(ctx, python, filepath.Join(root, "sdk", "python", "tests", "run_recovery_http_contract.py"), server.URL, runID, oldCall, newCall, stepID)
	command.Dir, command.WaitDelay = root, 2*time.Second
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE", "LANG", "LC_ALL":
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "PYTHONIOENCODING=utf-8", "PYTHONDONTWRITEBYTECODE=1")
	output := &runContractOutput{}
	command.Stdout, command.Stderr = output, output
	if err := command.Run(); err != nil {
		t.Fatalf("installed recovery SDK contract failed: %v\n%s", err, output.String())
	}
	if output.overflow || strings.TrimSpace(output.String()) != "PASS real HTTP recovery SDK" {
		t.Fatal("recovery SDK completion marker missing")
	}
}

func TestRunRecoveryProofCannotRescueIncompleteAudit(t *testing.T) {
	for _, mode := range []string{"missing_report", "missing_observation", "wrong_observation", "conflict", "anomaly"} {
		t.Run(mode, func(t *testing.T) {
			h := setupRecoveryHarness(t)
			old := recoveryAtDecision(t, h, "blocked-"+mode)
			req := ledgerRequest(h, old, agentrun.SubcallChat, "")
			ledgerReserve(t, h, req)
			report := auditReportRequest(t, h, req, 10, 5)
			if mode != "missing_report" {
				if mode == "anomaly" {
					report = auditReportRequest(t, h, req, h.Profile.MaxInputTokens+1, 5)
				}
				auditSettle(t, h, report)
			}
			if mode == "conflict" {
				auditSettle(t, h, auditReportRequest(t, h, req, 11, 5))
			}
			if mode == "wrong_observation" {
				auditObserve(t, h, req, report, "rejected")
			}
			before := ledgerView(t, h, old.Lease)
			if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, req.Step, "TIMEOUT"); err != nil {
				t.Fatalf("frozen cleanup refused: %v", err)
			}
			r := recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
			var proofCount int
			if err := h.Pool.QueryRow(h.Ctx, "select count(*) from run_attempts where run_id=$1 and recovery_ordinal=1", r.ID).Scan(&proofCount); err != nil || proofCount != 1 {
				t.Fatal("unknown audit suppressed closing proof")
			}
			claim, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
			if !errors.Is(err, agentrun.ErrBudgetExhausted) || claim != nil {
				t.Fatalf("incomplete audit rescued: %v", err)
			}
			after, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil || !reflect.DeepEqual(before.Budget, after.Budget) || after.AttemptNo != 1 {
				t.Fatal("blocked Claim charged or reset exposure")
			}
		})
	}
}

func TestRunRecoveryEveryProofIdentityFieldIsRequired(t *testing.T) {
	h := setupRecoveryHarness(t)
	old := recoveryAtDecision(t, h, "proof-identity")
	recoveryChat(t, h, old, true)
	if _, err := h.Store.FailExecution(h.Ctx, h.Principal, old.Lease, currentRunStep(old), "TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	r := recoveryReady(t, h, old.Lease.RunID, 3*time.Second)
	var original []byte
	if err := h.Pool.QueryRow(h.Ctx, "select recovery_step from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"step_id", "sequence", "kind", "cursor_version", "input_hash", "profile_id", "profile_hash", "snapshot_id", "snapshot_hash"} {
		t.Run(field, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(original, &value); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "step_id", "snapshot_id":
				value[field] = "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
			case "kind":
				value[field] = "protocol_correction"
			case "profile_id":
				value[field] = "other-profile"
			case "sequence", "cursor_version":
				value["sequence"], value["cursor_version"] = 3, 2
			default:
				value[field] = agentrun.Fingerprint("wrong-proof-field")
			}
			raw, _ := json.Marshal(value)
			if _, err := h.Pool.Exec(h.Ctx, "update run_attempts set recovery_step=$2 where run_id=$1 and attempt_no=1", r.ID, raw); err != nil {
				t.Fatal(err)
			}
			claimed, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
			if !errors.Is(err, agentrun.ErrBudgetExhausted) || claimed != nil {
				t.Fatalf("corrupt %s authorized Claim: %v", field, err)
			}
			if _, err := h.Pool.Exec(h.Ctx, "update run_attempts set recovery_step=$2 where run_id=$1 and attempt_no=1", r.ID, original); err != nil {
				t.Fatal(err)
			}
		})
	}
	if claimed := h.claim(t); claimed.Lease.AttemptNo != 2 {
		t.Fatal("restored valid proof did not resume")
	}
}
