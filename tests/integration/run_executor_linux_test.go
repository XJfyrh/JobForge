//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	"github.com/xjfyrh/jobforge/internal/runworker"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

func executorChildren(pid int) []int {
	files, _ := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	var children []int
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		for _, word := range strings.Fields(string(data)) {
			if child, err := strconv.Atoi(word); err == nil {
				children = append(children, child)
			}
		}
	}
	return children
}

type executorHelperInput struct {
	Gateway        string
	BusinessOrigin string
	Profile        agentrun.Profile
}

// TestRunExecutorWorkerProcessHelper is a test-binary-only entry. It bypasses
// TestMain's database setup via the existing helper flag and receives only the
// parent's synthetic deployment data. Production exposes no process selector.
func TestRunExecutorWorkerProcessHelper(t *testing.T) {
	if os.Getenv("JOBFORGE_RUNEXECUTOR_HELPER") != "1" {
		return
	}
	if os.Getenv("JOBFORGE_TEST_WORKER_HELPER") != "1" {
		t.Fatal("database bootstrap bypass missing")
	}
	var input executorHelperInput
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 16384)).Decode(&input); err != nil {
		t.Fatal("invalid synthetic helper deployment")
	}
	connection, err := grpc.NewClient(input.Gateway, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	manifest := executorManifest(t, input.Profile)
	worker, err := runworker.New(agentv1.NewAgentServiceClient(connection), manifest, runworker.Config{Profiles: []agentrun.Profile{input.Profile}, Environments: map[string]runexecutor.Environment{"tenant-a": {BusinessOrigin: input.BusinessOrigin, BusinessReadKey: "synthetic-business-read-key", OllamaOrigin: "http://127.0.0.1:11434", DeepSeekKey: "synthetic-provider-key"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer executor-synthetic-control-token")
	if err := worker.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRunExecutorWorkerSIGKILLRetainsUnknownAndRecoversCheckpoint(t *testing.T) {
	h := setupExecutorHarness(t)
	r := h.submit(t, "tenant-a", "worker-kill-paid-http")
	fixture := executorHTTP(t, h, r, false, false)
	fixture.blockFirstChat.Store(true)
	client := executorGateway(t, h)
	input, err := json.Marshal(executorHelperInput{Gateway: client.target, BusinessOrigin: fixture.BusinessOrigin, Profile: h.Profile})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(h.Ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestRunExecutorWorkerProcessHelper$", "-test.timeout=35s")
	command.Env = append(os.Environ(), "JOBFORGE_TEST_WORKER_HELPER=1", "JOBFORGE_RUNEXECUTOR_HELPER=1")
	command.Stdin = bytes.NewReader(input)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	helper := &executorWorkerRun{done: make(chan struct{})}
	go func() { helper.err = command.Wait(); close(helper.done) }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-helper.done:
		case <-time.After(3 * time.Second):
			t.Error("killed Worker was not reaped")
		}
	})
	waitExecutorSignal(t, helper, fixture.chatSeen)
	guardians := executorChildren(command.Process.Pid)
	if len(guardians) != 1 {
		t.Fatal("Worker helper did not own exactly one live guardian")
	}
	guardian := guardians[0]
	children := executorChildren(guardian)
	if len(children) != 1 {
		t.Fatal("guardian did not own exactly one live step")
	}
	old := agentrun.Lease{TenantID: r.TenantID, RunID: r.ID, WorkerID: h.Principal}
	if err := h.Pool.QueryRow(h.Ctx, "select session_id::text,attempt_no,fencing_token from runs where run_id=$1", r.ID).Scan(&old.SessionID, &old.AttemptNo, &old.FencingToken); err != nil {
		t.Fatal(err)
	}
	var oldCall string
	var reservedCost int64
	if err := h.Pool.QueryRow(h.Ctx, "select physical_call_id::text,reserved_cost_microyuan from physical_calls where run_id=$1 and subcall='chat'", r.ID).Scan(&oldCall, &reservedCost); err != nil {
		t.Fatal(err)
	}
	if reservedCost <= 0 {
		t.Fatal("synthetic paid call had no reserved exposure")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-helper.done:
	case <-time.After(3 * time.Second):
		t.Fatal("actual Worker Wait did not return")
	}
	var exit *exec.ExitError
	if !errors.As(helper.err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
		t.Fatal("Worker helper was not actually killed")
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for !errors.Is(syscall.Kill(-guardian, 0), syscall.ESRCH) {
		select {
		case <-deadline.C:
			t.Fatal("parent EOF left its process group alive or zombie")
		case <-ticker.C:
		}
	}
	if err := syscall.Kill(children[0], 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("orphaned step was not reaped by init")
	}
	view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != agentrun.Running || view.CursorVersion != 4 || fixture.count("/chat/completions") != 1 {
		t.Fatalf("death rewrote durable progress: state=%s cursor=%d", view.State, view.CursorVersion)
	}
	// The process has actually died and its old group is gone. Shorten only the
	// isolated database's timing facts; this is not evidence of waiting 30s/60s.
	if _, err := h.Pool.Exec(h.Ctx, "update runs set lease_until=clock_timestamp()-interval '1 millisecond' where run_id=$1", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update worker_sessions set seen_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' where worker_id=$1", h.Principal); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, "update runs set next_attempt_at=clock_timestamp() where run_id=$1", r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.GetCheckpoint(h.Ctx, h.Principal, old); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("old authority regained access after recovery: %v", err)
	}
	running := startExecutorWorker(t, h, fixture, client)
	waitExecutorSignal(t, running, client.looped)
	view, err = h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != agentrun.Succeeded || view.CursorVersion != 6 || view.AttemptNo != 2 {
		t.Fatalf("replacement Worker did not resume accepted checkpoint: state=%s cursor=%d attempt=%d", view.State, view.CursorVersion, view.AttemptNo)
	}
	for _, path := range []string{"order", "delivery", "search", "/api/version", "/api/tags", "/api/embed"} {
		if fixture.count(path) != 1 {
			t.Fatalf("accepted physical step replayed: %s=%d", path, fixture.count(path))
		}
	}
	if fixture.count("/chat/completions") != 2 {
		t.Fatal("replacement did not obtain exactly one new chat execution")
	}
	var chatCalls, knownChats, reusedSteps, newSteps int
	if err := h.Pool.QueryRow(h.Ctx, "select count(*),count(*) filter(where status='known') from physical_calls where run_id=$1 and subcall='chat'", r.ID).Scan(&chatCalls, &knownChats); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(h.Ctx, "select count(*) filter(where sequence<=4 and attempt_no=1),count(*) filter(where sequence>4 and attempt_no=2) from run_steps where run_id=$1", r.ID).Scan(&reusedSteps, &newSteps); err != nil {
		t.Fatal(err)
	}
	if chatCalls != 2 || knownChats != 1 || reusedSteps != 4 || newSteps != 2 {
		t.Fatal("recovery did not retain accepted steps and separately authorize the retry")
	}
	var oldStatus string
	var known *string
	if err := h.Pool.QueryRow(h.Ctx, "select status,usage_hash from physical_calls where physical_call_id=$1", oldCall).Scan(&oldStatus, &known); err != nil {
		t.Fatal(err)
	}
	var held int64
	if err := h.Pool.QueryRow(h.Ctx, "select held_cost_microyuan from budget_accounts where account_id=$1", r.Budget.Family.ID).Scan(&held); err != nil {
		t.Fatal(err)
	}
	var newSession string
	var newFence int64
	if err := h.Pool.QueryRow(h.Ctx, "select session_id::text,fencing_token from run_attempts where run_id=$1 and attempt_no=2", r.ID).Scan(&newSession, &newFence); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "unknown" || known != nil || held != reservedCost || newSession == old.SessionID || newFence <= old.FencingToken {
		t.Fatal("recovery erased unknown hold or reused old execution identity")
	}
}

func TestRunExecutorGuardianKilledDuringBusinessHTTP(t *testing.T) {
	h := setupExecutorHarness(t)
	r := h.submit(t, "tenant-a", "guardian-killed-in-http")
	fixture := executorHTTP(t, h, r, false, true)
	client := executorGateway(t, h)
	running := startExecutorWorker(t, h, fixture, client)
	waitExecutorSignal(t, running, fixture.orderSeen)
	guardian := 0
	for _, pid := range executorChildren(os.Getpid()) {
		command, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err == nil && bytes.Contains(command, []byte("jobforge_agent.guardian")) {
			if guardian != 0 {
				t.Fatal("more than one active guardian")
			}
			guardian = pid
		}
	}
	if guardian == 0 {
		t.Fatal("formal guardian not found while business HTTP was running")
	}
	group, err := syscall.Getpgid(guardian)
	if err != nil || group != guardian {
		t.Fatal("formal guardian did not own its process group")
	}
	children := executorChildren(guardian)
	if len(children) != 1 {
		t.Fatal("formal guardian did not own exactly one step")
	}
	if err := syscall.Kill(guardian, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitExecutorSignal(t, running, client.looped)
	if err := syscall.Kill(-guardian, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("old process group remains alive or unreaped")
	}
	if err := syscall.Kill(children[0], 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("old step remains alive or zombie")
	}
	var failure *string
	if err := h.Pool.QueryRow(h.Ctx, "select error_code from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&failure); err != nil {
		t.Fatal(err)
	}
	if failure == nil || *failure != "EXECUTOR_PROTOCOL_ERROR" || fixture.count("order") != 1 || fixture.count("delivery") != 0 || client.commitAttempts.Load() != 1 {
		t.Fatalf("guardian death altered continuation: failure=%v order=%d delivery=%d commits=%d", failure, fixture.count("order"), fixture.count("delivery"), client.commitAttempts.Load())
	}
}

func TestRunExecutorUsageAnomalyFailsLiveRunAndStopsWorker(t *testing.T) {
	h := setupExecutorHarness(t)
	r := h.submit(t, "tenant-a", "live-usage-anomaly")
	fixture := executorHTTP(t, h, r, false, false)
	fixture.anomalyInput.Store(h.Profile.MaxInputTokens + 1)
	client := executorGateway(t, h)
	running := startExecutorWorker(t, h, fixture, client, agentrun.ErrBudgetExhausted)
	waitExecutorSignal(t, running, fixture.chatSeen)
	guardians := executorChildren(os.Getpid())
	if len(guardians) != 1 {
		t.Fatal("expected one formal guardian during paid HTTP")
	}
	guardian := guardians[0]
	if group, err := syscall.Getpgid(guardian); err != nil || group != guardian {
		t.Fatal("guardian did not own its process group")
	}
	children := executorChildren(guardian)
	if len(children) != 1 {
		t.Fatal("expected one actual step during paid HTTP")
	}
	before, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	close(fixture.chatRelease)
	select {
	case <-running.done:
	case <-time.After(5 * time.Second):
		t.Fatal("measurement anomaly did not stop the Worker")
	}
	if !errors.Is(running.err, agentrun.ErrBudgetExhausted) {
		t.Fatalf("unexpected Worker shutdown result: %v", running.err)
	}
	if err := syscall.Kill(-guardian, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("measurement anomaly left its process group alive or zombie")
	}
	if err := syscall.Kill(children[0], 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("measurement anomaly left its step alive or zombie")
	}
	after, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.State != agentrun.Running || after.State != agentrun.Failed || after.CursorVersion != 4 ||
		after.Error == nil || after.Error.Code != "MODEL_PROTOCOL_ERROR" {
		t.Fatalf("live anomaly did not fail the Run permanently: state=%s cursor=%d error=%v", after.State, after.CursorVersion, after.Error)
	}
	for index, account := range ledgerAccounts(after) {
		original := ledgerAccounts(before)[index]
		if original.Frozen || !account.Frozen || account.Used != original.Used ||
			account.HeldTokens != original.HeldTokens || account.HeldCostMicroyuan != original.HeldCostMicroyuan ||
			account.KnownTokens != original.KnownTokens || account.KnownCostMicroyuan != original.KnownCostMicroyuan || account.HeldCostMicroyuan <= 0 {
			t.Fatal("anomaly did not freeze all three budgets with their full original exposure")
		}
	}
	var callStatus string
	var anomaly bool
	var usageHash *string
	if err := h.Pool.QueryRow(h.Ctx, "select status,measurement_anomaly,usage_hash from physical_calls where run_id=$1 and subcall='chat'", r.ID).Scan(&callStatus, &anomaly, &usageHash); err != nil {
		t.Fatal(err)
	}
	if callStatus != "unknown" || !anomaly || usageHash == nil || fixture.badRequest.Load() ||
		fixture.count("/chat/completions") != 1 || client.failed.Load() != 1 || client.claimAttempts.Load() != 1 || client.commitAttempts.Load() != 4 {
		t.Fatal("trusted anomaly lost its report, repeated work, corrected output or claimed again")
	}
}
