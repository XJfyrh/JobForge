//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/grpcapi"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
	"github.com/xjfyrh/jobforge/internal/runworker"
)

const recoveryFaultPath = "/etc/jobforge/recovery/fault.json"
const recoveryMarkerPath = "/etc/jobforge/recovery/marker.json"

type recoveryMarker struct {
	RunID   string `json:"run_id"`
	Attempt int64  `json:"attempt_no"`
	Kind    string `json:"step_kind"`
	Point   string `json:"point"`
	PID     int    `json:"pid"`
	StepID  string `json:"step_id"`
}

type recoveryExecutorAPI struct {
	*runpostgres.Store
	mode        string
	seen        chan struct{}
	confirmed   chan struct{}
	blocked     chan struct{}
	once        sync.Once
	confirmOnce sync.Once
	blockOnce   sync.Once
}

func (a *recoveryExecutorAPI) CommitStep(ctx context.Context, principal string, req agentrun.CommitStepRequest) (agentrun.CommitStepResponse, error) {
	response, err := a.Store.CommitStep(ctx, principal, req)
	if err == nil && req.Lease.AttemptNo == 1 && req.Step.Kind == "model_decision" && a.mode == "commit_ack" {
		a.once.Do(func() { close(a.seen) })
		return agentrun.CommitStepResponse{}, agentrun.ErrDependencyUnavailable
	}
	return response, err
}

func (a *recoveryExecutorAPI) GetAcceptedCommit(ctx context.Context, principal string, lease agentrun.Lease, step string) (agentrun.AcceptedCommitResponse, error) {
	response, err := a.Store.GetAcceptedCommit(ctx, principal, lease, step)
	if err == nil && response.Found && a.mode == "commit_ack" {
		a.confirmOnce.Do(func() { close(a.confirmed) })
	}
	return response, err
}

func (a *recoveryExecutorAPI) BeginTool(ctx context.Context, principal string, req agentrun.BeginToolRequest) (agentrun.BeginToolResponse, error) {
	if req.Lease.AttemptNo == 1 && req.Step.Kind == "get_order" && (a.mode == "after_commit_ack" || a.mode == "commit_ack") {
		a.blockOnce.Do(func() { close(a.blocked) })
		<-ctx.Done()
		return agentrun.BeginToolResponse{}, ctx.Err()
	}
	return a.Store.BeginTool(ctx, principal, req)
}

func recoveryExecutorGateway(t *testing.T, h *runHarness, mode string) (string, *recoveryExecutorAPI) {
	t.Helper()
	api := &recoveryExecutorAPI{Store: h.Store, mode: mode, seen: make(chan struct{}), confirmed: make(chan struct{}), blocked: make(chan struct{})}
	server, err := grpcapi.NewServer(api, grpcapi.Config{Workers: h.Options.Workers, Credentials: map[string]string{
		h.Options.Workers[0].ID: "executor-synthetic-control-token", h.Options.Workers[1].ID: "recovery-synthetic-control-token-two"}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return listener.Addr().String(), api
}

type recoveryWorkerProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func recoveryStartWorker(t *testing.T, h *runHarness, origin, gateway string, index int) *recoveryWorkerProcess {
	t.Helper()
	config, keys := recoveryWorkerFiles(t, h, origin, index)
	command := exec.Command("/usr/local/bin/agent-worker")
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "JOBFORGE_AGENT_WORKER_CONFIG=" + config,
		"JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE=" + keys, "JOBFORGE_AGENT_GATEWAY=" + gateway, "JOBFORGE_AGENT_GRPC_TLS=false"}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	worker := &recoveryWorkerProcess{cmd: command, done: make(chan struct{})}
	go func() { worker.err = command.Wait(); close(worker.done) }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-worker.done:
		case <-time.After(3 * time.Second):
			t.Error("formal worker did not join")
		}
	})
	return worker
}

func recoveryWorkerFiles(t *testing.T, h *runHarness, origin string, index int) (string, string) {
	t.Helper()
	dir := t.TempDir()
	config, keys := filepath.Join(dir, "worker.json"), filepath.Join(dir, "keys.json")
	launcherJSON(t, config, map[string]any{"schema_version": 1, "profiles": []agentrun.Profile{h.Profile}, "tenants": map[string]any{
		"tenant-north": map[string]string{"business_origin": origin, "ollama_origin": "http://127.0.0.1:11434"}}})
	token := "executor-synthetic-control-token"
	if index == 1 {
		token = "recovery-synthetic-control-token-two"
	}
	launcherJSON(t, keys, map[string]any{"control_token": token, "tenants": map[string]any{
		"tenant-north": map[string]string{"business_read_key": "synthetic-business-read-key", "deepseek_api_key": "synthetic-provider-key"}}})
	return config, keys
}

func recoveryStopWorker(t *testing.T, worker *recoveryWorkerProcess, kill bool) {
	t.Helper()
	signal := syscall.SIGTERM
	if kill {
		signal = syscall.SIGKILL
	}
	if err := worker.cmd.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	select {
	case <-worker.done:
	case <-time.After(4 * time.Second):
		t.Fatal("formal worker actual Wait did not return")
	}
	if kill {
		var exit *exec.ExitError
		if !errors.As(worker.err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() {
			t.Fatal("formal worker was not actually killed")
		}
	} else if worker.err != nil {
		t.Fatalf("formal worker stopped unexpectedly: %v", worker.err)
	}
}

func recoveryEventually(t *testing.T, timeout time.Duration, fact func() bool) {
	t.Helper()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for !fact() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("required actual recovery fact not observed")
		}
	}
}

func recoveryFault(t *testing.T, runID string, attempt int64, point string) {
	t.Helper()
	if err := os.Remove(recoveryMarkerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	launcherJSON(t, recoveryFaultPath, map[string]any{"run_id": runID, "attempt_no": attempt, "step_kind": "model_decision", "point": point})
}

func recoveryReadMarker(t *testing.T, runID string, attempt int64, sweep *runHarness, timeout time.Duration) recoveryMarker {
	t.Helper()
	var marker recoveryMarker
	recoveryEventually(t, timeout, func() bool {
		if sweep != nil {
			if _, err := sweep.Store.Sweep(sweep.Ctx, 100); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(recoveryMarkerPath)
		if err != nil {
			return false
		}
		if json.Unmarshal(raw, &marker) != nil || marker.RunID != runID || marker.Attempt != attempt {
			return false
		}
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", marker.PID))
		return err == nil && bytes.Contains(status, []byte("State:\tT"))
	})
	return marker
}

func recoveryGroup(t *testing.T, worker *recoveryWorkerProcess, marker recoveryMarker) int {
	t.Helper()
	guardians := executorChildren(worker.cmd.Process.Pid)
	if len(guardians) != 1 {
		t.Fatal("formal worker did not own one guardian")
	}
	children := executorChildren(guardians[0])
	if len(children) != 1 || children[0] != marker.PID {
		t.Fatal("factual marker did not identify the actual step")
	}
	group, err := syscall.Getpgid(guardians[0])
	if err != nil || group != guardians[0] {
		t.Fatal("guardian did not own the process group")
	}
	return group
}

func recoveryGroupGone(t *testing.T, group, step int) {
	t.Helper()
	recoveryEventually(t, 4*time.Second, func() bool {
		return errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) && errors.Is(syscall.Kill(step, 0), syscall.ESRCH)
	})
}

func recoveryExecutorSetup(t *testing.T, mode string) (*runHarness, agentrun.Run, *supportExecutorHTTPFixture, string, *recoveryExecutorAPI) {
	t.Helper()
	if os.Getenv("JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS") != "1" {
		t.Skip("requires fixed Linux image, --init, installed SDK and real PG; skip is not acceptance")
	}
	h := setupRecoveryHarness(t)
	h.Ctx = t.Context()
	path := "/etc/jobforge/executor.json"
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	launcherJSON(t, path, runworker.Manifest{SchemaVersion: 1, ExecutorVersion: h.Profile.ExecutorVersion,
		Profiles: []runworker.ManifestProfile{{ProfileID: h.Profile.ID, ProfileHash: h.Profile.Hash, AdapterID: "support-agent-v1"}}})
	t.Cleanup(func() {
		if err := os.WriteFile(path, original, 0600); err != nil {
			t.Error(err)
		}
		_ = os.Remove(recoveryFaultPath)
		_ = os.Remove(recoveryMarkerPath)
	})
	snapshot := supportProfileCapture(t, h, "submit", "submit-"+mode, "", true)
	r := h.submit(t, "tenant-north", mode)
	f := &supportExecutorHTTPFixture{executorHTTPFixture: &executorHTTPFixture{counts: map[string]int{}}, withOrder: true}
	f.snapshot = snapshot
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/chat/completions" && req.URL.Path != "/api/embed" {
			f.serveSupport(w, req)
			return
		}
		f.mu.Lock()
		f.counts[req.URL.Path]++
		f.mu.Unlock()
		var body map[string]json.RawMessage
		if req.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, req.Body, 131072)).Decode(&body) != nil {
			f.reject(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/api/embed" {
			vector := make([]float64, 384)
			vector[0] = 1
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "all-minilm:22m", "embeddings": [][]float64{vector}, "prompt_eval_count": 3})
			return
		}
		var messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}
		if req.Header.Get("Authorization") != "Bearer synthetic-provider-key" || json.Unmarshal(body["messages"], &messages) != nil || len(messages) < 2 {
			f.reject(w)
			return
		}
		var facts struct {
			Tools []json.RawMessage `json:"previous_tools"`
		}
		if json.Unmarshal([]byte(messages[len(messages)-1].Content), &facts) != nil {
			f.reject(w)
			return
		}
		decisions := []string{
			`{"type":"tool","name":"get_order","arguments":{"order_id":"order-1"}}`,
			`{"type":"tool","name":"search_policy","arguments":{"query":"delivery timing"}}`,
			`{"type":"tool","name":"get_delivery","arguments":{"order_id":"order-1"}}`,
			`{"type":"tool","name":"search_policy","arguments":{"query":"late delivery action"}}`,
			`{"type":"final","proposal":{"decision":"proposal","action":"escalate","conclusion":"delayed","requested_fields":[],"target_ticket_status":"escalated","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["T#/observed_at","E1#/order/promised_delivery_at","P01.1"]}]}}`,
		}
		if len(facts.Tools) >= len(decisions) {
			f.reject(w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-recovery-chat", "object": "chat.completion", "created": 1, "model": "deepseek-flash", "system_fingerprint": "fp_synthetic_recovery",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "logprobs": nil, "message": map[string]any{"role": "assistant", "content": decisions[len(facts.Tools)]}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "prompt_cache_hit_tokens": 2, "prompt_cache_miss_tokens": 8}})
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	f.BusinessOrigin = server.URL
	for _, address := range []string{"127.0.0.1:11434", "127.0.0.1:18093"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(handler)
		_ = server.Listener.Close()
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
	}
	gateway, api := recoveryExecutorGateway(t, h, mode)
	return h, r, f, gateway, api
}

func recoveryExecution(t *testing.T, h *runHarness, r agentrun.Run) agentrun.Lease {
	t.Helper()
	lease := agentrun.Lease{TenantID: r.TenantID, RunID: r.ID}
	if err := h.Pool.QueryRow(h.Ctx, "select worker_id,session_id::text,attempt_no,fencing_token from runs where run_id=$1", r.ID).Scan(&lease.WorkerID, &lease.SessionID, &lease.AttemptNo, &lease.FencingToken); err != nil {
		t.Fatal(err)
	}
	return lease
}

func recoveryAwaitCompletion(t *testing.T, h *runHarness, r agentrun.Run) agentrun.Run {
	t.Helper()
	var view agentrun.Run
	recoveryEventually(t, 30*time.Second, func() bool {
		var err error
		view, err = h.Store.Get(h.Ctx, r.TenantID, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		return view.State == agentrun.AwaitingApproval || view.State.Terminal()
	})
	if view.State != agentrun.AwaitingApproval || view.CursorVersion != 11 || view.RecoveryCount != 1 || view.AttemptNo != 2 {
		t.Fatalf("natural recovery failed: state=%s cursor=%d recovery=%d attempt=%d", view.State, view.CursorVersion, view.RecoveryCount, view.AttemptNo)
	}
	return view
}

func TestRunRecoveryExecutorNaturalLoss(t *testing.T) {
	for _, mode := range []string{"step_before_permit", "step_after_confirmed", "guardian_after_confirmed", "worker_after_confirmed", "reserved_before_send"} {
		t.Run(mode, func(t *testing.T) {
			h, r, f, gateway, _ := recoveryExecutorSetup(t, mode)
			point := "after_confirmed"
			if mode == "step_before_permit" {
				point = "before_intent"
			}
			if mode == "reserved_before_send" {
				point = "before_send"
			}
			recoveryFault(t, r.ID, 1, point)
			worker := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 0)
			marker := recoveryReadMarker(t, r.ID, 1, nil, 15*time.Second)
			group := recoveryGroup(t, worker, marker)
			old := recoveryExecution(t, h, r)
			prefix, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 32)
			if err != nil || len(prefix.Items) != 1 {
				t.Fatal("actual read_ticket prefix was not persisted")
			}
			started := time.Now()
			switch mode {
			case "worker_after_confirmed", "reserved_before_send":
				recoveryStopWorker(t, worker, true)
			case "guardian_after_confirmed":
				if err := syscall.Kill(group, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			default:
				if err := syscall.Kill(marker.PID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			}
			recoveryGroupGone(t, group, marker.PID)
			if mode != "worker_after_confirmed" && mode != "reserved_before_send" {
				recoveryStopWorker(t, worker, false)
			}
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil || view.State != agentrun.Running || view.RecoveryCount != 0 || view.CursorVersion != 1 {
				t.Fatalf("local death invented Fail or Commit: %v state=%s", err, view.State)
			}
			// Natural expiry rejects the old owner before any scanner reclaims it.
			recoveryEventually(t, 35*time.Second, func() bool {
				var expired bool
				if err := h.Pool.QueryRow(h.Ctx, "select lease_until<=clock_timestamp() from runs where run_id=$1", r.ID).Scan(&expired); err != nil {
					t.Fatal(err)
				}
				return expired
			})
			if _, err := h.Store.GetCheckpoint(h.Ctx, old.WorkerID, old); !errors.Is(err, agentrun.ErrStaleLease) {
				t.Fatalf("expired unreclaimed lease: %v", err)
			}
			if _, err := h.Store.HeartbeatExecution(h.Ctx, old.WorkerID, old); !errors.Is(err, agentrun.ErrStaleLease) {
				t.Fatalf("old heartbeat: %v", err)
			}
			ready := recoveryReady(t, h, r.ID, 6*time.Second)
			if ready.RecoveryCount != 1 || ready.AttemptNo != 1 || time.Since(started) < 25*time.Second {
				t.Fatal("did not use actual production lease/backoff")
			}
			var proof []byte
			var ordinal int64
			if err := h.Pool.QueryRow(h.Ctx, "select recovery_step,recovery_ordinal from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&proof, &ordinal); err != nil || ordinal != 1 {
				t.Fatal("natural closure lost proof")
			}
			step, err := agentrun.DecodeRecoveryStep(proof)
			if err != nil || step.ID != marker.StepID {
				t.Fatal("wrong pending proof")
			}
			if mode == "reserved_before_send" {
				calls, err := h.Store.Calls(h.Ctx, r.TenantID, r.ID)
				if err != nil || len(calls.Items) != 1 || calls.Items[0].UsageKnown || calls.Items[0].HeldCostMicroyuan != calls.Items[0].Reserved.CostMicroyuan || f.count("/chat/completions") != 0 {
					t.Fatal("unsent reservation lost its full hold")
				}
				if _, err := h.Store.Register(h.Ctx, old.WorkerID, "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa", h.Profile.ExecutorVersion); !errors.Is(err, agentrun.ErrConflict) {
					t.Fatal("same principal evicted live session")
				}
				var same agentrun.Session
				recoveryEventually(t, 35*time.Second, func() bool {
					var err error
					same, err = h.Store.Register(h.Ctx, old.WorkerID, "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa", h.Profile.ExecutorVersion)
					if err != nil && !errors.Is(err, agentrun.ErrConflict) {
						t.Fatal(err)
					}
					return err == nil
				})
				if !same.CreatedAt.After(oldSessionExpiry(t, h, old)) {
					t.Fatal("new startup bypassed natural 60s session protection")
				}
				if claim, err := h.Store.Claim(h.Ctx, old.WorkerID, same.ID); !errors.Is(err, agentrun.ErrBudgetExhausted) || claim != nil {
					t.Fatalf("unknown reservation resumed: %v", err)
				}
				return
			}
			if err := os.Remove(recoveryFaultPath); err != nil {
				t.Fatal(err)
			}
			replacement := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 1)
			after := recoveryAwaitCompletion(t, h, r)
			recoveryStopWorker(t, replacement, false)
			steps, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 32)
			if err != nil || steps.Items[0].CommitHash != prefix.Items[0].CommitHash || !bytes.Equal(steps.Items[0].Output, prefix.Items[0].Output) {
				t.Fatal("recovery repeated or rewrote accepted prefix")
			}
			wantChats := 6
			if mode == "step_before_permit" {
				wantChats = 5
			}
			if f.badRequest.Load() || f.count("/chat/completions") != wantChats || after.Budget.Family.Used.Chat != int64(wantChats) || f.count("order") != 1 || f.count("delivery") != 1 {
				t.Fatal("recovery repeated committed HTTP or reset counters")
			}
			t.Logf("natural loss %s: elapsed=%s recovery=1 chats=%d prefix preserved", mode, time.Since(started), wantChats)
		})
	}
}

func oldSessionExpiry(t *testing.T, h *runHarness, lease agentrun.Lease) time.Time {
	t.Helper()
	var expires time.Time
	if err := h.Pool.QueryRow(h.Ctx, "select expires_at from worker_sessions where session_id=$1", lease.SessionID).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	return expires
}

func recoveryWaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(15 * time.Second):
		t.Fatal("actual RPC barrier not reached")
	}
}

func TestRunRecoveryExecutorCommitBoundary(t *testing.T) {
	for _, mode := range []string{"commit_ack", "after_commit_ack"} {
		t.Run(mode, func(t *testing.T) {
			h, r, f, gateway, api := recoveryExecutorSetup(t, mode)
			worker := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 0)
			if mode == "commit_ack" {
				recoveryWaitSignal(t, api.seen)
				recoveryWaitSignal(t, api.confirmed)
				recoveryEventually(t, 4*time.Second, func() bool { return len(executorChildren(worker.cmd.Process.Pid)) == 0 })
				select {
				case <-api.blocked:
					t.Fatal("ACK uncertainty invented authority to execute next step")
				default:
				}
			} else {
				recoveryWaitSignal(t, api.blocked)
				// BeginTool precedes process creation. The previous actual step
				// has joined; no next child or permit may exist at this barrier.
				if len(executorChildren(worker.cmd.Process.Pid)) != 0 {
					t.Fatal("next process started before BeginTool authorization")
				}
			}
			prefix, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 32)
			if err != nil || len(prefix.Items) != 2 || prefix.Items[1].Kind != "model_decision" {
				t.Fatal("commit was not actually persisted before ACK barrier")
			}
			started := time.Now()
			recoveryStopWorker(t, worker, true)
			ready := recoveryReady(t, h, r.ID, 35*time.Second)
			if ready.CursorVersion != 2 || ready.RecoveryCount != 1 || time.Since(started) < 25*time.Second {
				t.Fatal("commit boundary did not naturally recover from accepted prefix")
			}
			replacement := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 1)
			after := recoveryAwaitCompletion(t, h, r)
			recoveryStopWorker(t, replacement, false)
			steps, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 32)
			if err != nil {
				t.Fatal(err)
			}
			for i := range prefix.Items {
				if prefix.Items[i].CommitHash != steps.Items[i].CommitHash || !bytes.Equal(prefix.Items[i].Output, steps.Items[i].Output) {
					t.Fatal("accepted commit was rewritten")
				}
			}
			if f.badRequest.Load() || f.count("/chat/completions") != 5 || f.count("order") != 1 || after.Budget.Family.Used.Chat != 5 {
				t.Fatal("ACK uncertainty repeated committed provider request")
			}
			t.Logf("%s: real Commit, bounded read confirmation, natural lease/backoff=%s, no prefix replay", mode, time.Since(started))
		})
	}
}

func TestRunRecoveryExecutorAttemptDeadline(t *testing.T) {
	h, r, f, gateway, _ := recoveryExecutorSetup(t, "attempt-deadline")
	recoveryFault(t, r.ID, 1, "before_intent")
	worker := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 0)
	marker := recoveryReadMarker(t, r.ID, 1, nil, 15*time.Second)
	group := recoveryGroup(t, worker, marker)
	old := recoveryExecution(t, h, r)
	var start, deadline time.Time
	if err := h.Pool.QueryRow(h.Ctx, "select started_at,deadline from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&start, &deadline); err != nil || deadline.Sub(start) != 180*time.Second {
		t.Fatal("formal attempt was not the production 180s deadline")
	}
	recoveryEventually(t, 185*time.Second, func() bool {
		return errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) && errors.Is(syscall.Kill(marker.PID, 0), syscall.ESRCH)
	})
	if time.Since(start) < 179*time.Second || f.count("/chat/completions") != 0 {
		t.Fatal("attempt timeout was shortened or sent an unpermitted request")
	}
	recoveryStopWorker(t, worker, false)
	ready := recoveryReady(t, h, r.ID, 6*time.Second)
	var code string
	var ordinal int64
	if err := h.Pool.QueryRow(h.Ctx, "select error_code,recovery_ordinal from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&code, &ordinal); err != nil || code != "ATTEMPT_DEADLINE_EXCEEDED" || ordinal != 1 || ready.RecoveryCount != 1 {
		t.Fatalf("natural attempt closure: code=%s ordinal=%d err=%v", code, ordinal, err)
	}
	if _, err := h.Store.HeartbeatExecution(h.Ctx, old.WorkerID, old); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatal("expired attempt retained heartbeat authority")
	}
	if err := os.Remove(recoveryFaultPath); err != nil {
		t.Fatal(err)
	}
	replacement := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 1)
	recoveryAwaitCompletion(t, h, r)
	recoveryStopWorker(t, replacement, false)
	t.Logf("real 180s attempt deadline, heartbeat maintained, actual group gone, natural recovery: elapsed=%s", time.Since(start))
}

func TestRunRecoveryExecutorRecoveryLimit(t *testing.T) {
	h, r, f, gateway, _ := recoveryExecutorSetup(t, "recovery-limit")
	recoveryFault(t, r.ID, 1, "before_intent")
	worker := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 0)
	start := time.Now()
	for attempt := int64(1); attempt <= 4; attempt++ {
		marker := recoveryReadMarker(t, r.ID, attempt, h, 40*time.Second)
		group := recoveryGroup(t, worker, marker)
		if err := syscall.Kill(marker.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		recoveryGroupGone(t, group, marker.PID)
		if attempt < 4 {
			recoveryFault(t, r.ID, attempt+1, "before_intent")
		}
	}
	failed := recoveryReady(t, h, r.ID, 35*time.Second)
	recoveryStopWorker(t, worker, false)
	if failed.State != agentrun.Failed || failed.RecoveryCount != 3 || failed.AttemptNo != 4 || failed.CursorVersion != 1 || f.count("/chat/completions") != 0 {
		t.Fatalf("fourth actual loss bypassed recovery limit: state=%s count=%d attempt=%d", failed.State, failed.RecoveryCount, failed.AttemptNo)
	}
	rows, err := h.Pool.Query(h.Ctx, "select attempt_no,recovery_ordinal,recovery_step is not null from run_attempts where run_id=$1 order by attempt_no", r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := int64(0)
	for rows.Next() {
		count++
		var attempt int64
		var ordinal *int64
		var proof bool
		if err := rows.Scan(&attempt, &ordinal, &proof); err != nil || attempt != count || (count <= 3 && (ordinal == nil || *ordinal != count || !proof)) || (count == 4 && (ordinal != nil || proof)) {
			t.Fatal("terminal close invented a fourth recovery proof")
		}
	}
	if err := rows.Err(); err != nil || count != 4 || time.Since(start) < 120*time.Second {
		t.Fatal("recovery limit did not run four natural leases")
	}
	t.Logf("four actual step kills, natural 30s leases and 1/2/4s backoff, exactly three proofs: elapsed=%s", time.Since(start))
}

func TestRunRecoveryExternalProxyAndSupervisor(t *testing.T) {
	for _, mode := range []string{"F06", "F07"} {
		t.Run(mode, func(t *testing.T) {
			h, r, f, gateway, _ := recoveryExecutorSetup(t, "external-"+mode)
			directory := t.TempDir()
			boundary, target := "first_search_commit", "worker"
			if mode == "F07" {
				boundary, target = "first_chat_observation", "step"
			}
			experiment := "external-" + mode
			launcherJSON(t, filepath.Join(directory, "fault.json"), map[string]any{"experiment": experiment, "run_id": r.ID, "tenant_id": r.TenantID, "profile_hash": h.Profile.Hash, "boundary": boundary})
			config, keys := recoveryWorkerFiles(t, h, f.BusinessOrigin, 0)
			secondConfig, secondKeys := recoveryWorkerFiles(t, h, f.BusinessOrigin, 1)
			resultPath, settingsPath := filepath.Join(directory, "result.json"), filepath.Join(directory, "settings.json")
			launcherJSON(t, settingsPath, map[string]any{"barriers": directory, "upstream_gateway": gateway, "result": resultPath,
				"workers": []map[string]string{{"worker_id": h.Options.Workers[0].ID, "config": config, "credentials": keys}, {"worker_id": h.Options.Workers[1].ID, "config": secondConfig, "credentials": secondKeys}},
				"row":     map[string]string{"experiment": experiment, "target": target}})
			command := exec.CommandContext(h.Ctx, "/usr/local/bin/python", "-I", "-m", "recovery_supervisor_check", settingsPath)
			command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "PYTHONDONTWRITEBYTECODE=1"}
			if err := command.Run(); err != nil {
				raw, _ := os.ReadFile(resultPath)
				t.Fatalf("external supervisor did not verify real process facts: %v %s", err, raw)
			}
			var receipt struct {
				Passed    bool      `json:"passed"`
				StepID    string    `json:"step_id"`
				Phase     string    `json:"phase"`
				Queued    int       `json:"ordinary_ack_queued_bytes"`
				Consumed  bool      `json:"python_ack_consumed"`
				Waited    bool      `json:"worker_waited"`
				GroupGone bool      `json:"group_gone"`
				KillSent  time.Time `json:"kill_sent_at"`
				WaitAt    time.Time `json:"worker_wait_completed_at"`
				GoneAt    time.Time `json:"group_gone_confirmed_at"`
				Child     string    `json:"child_group_at_boundary"`
			}
			raw, err := os.ReadFile(resultPath)
			if err != nil || json.Unmarshal(raw, &receipt) != nil || !receipt.Passed || !receipt.Waited || !receipt.GroupGone || receipt.Consumed {
				t.Fatal("external receipt lost factual cleanup/ACK distinction")
			}
			if receipt.KillSent.IsZero() || receipt.WaitAt.Before(receipt.KillSent) || receipt.GoneAt.Before(receipt.KillSent) || receipt.Child != "present" {
				t.Fatal("external receipt replaced signal time with cleanup or invented a child window")
			}
			wantCursor, wantChats := int64(5), 5
			if mode == "F07" {
				wantCursor, wantChats = 1, 6
				if receipt.Queued <= 0 || receipt.Phase != "observation_ack_pending" {
					t.Fatal("real ordinary ACK was not queued before actual step kill")
				}
			} else if receipt.Phase != "before_next_permit" {
				t.Fatal("F06 did not pass the original Commit ACK")
			}
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil || view.State != agentrun.Running || view.CursorVersion != wantCursor || view.RecoveryCount != 0 {
				t.Fatalf("external injection invented Fail/Commit: %v state=%s cursor=%d", err, view.State, view.CursorVersion)
			}
			start := time.Now()
			recoveryReady(t, h, r.ID, 35*time.Second)
			var proof []byte
			if err := h.Pool.QueryRow(h.Ctx, "select recovery_step from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&proof); err != nil {
				t.Fatal(err)
			}
			step, err := agentrun.DecodeRecoveryStep(proof)
			if err != nil || step.ID != receipt.StepID {
				t.Fatal("external RPC/PID window did not match the persisted original step")
			}
			replacement := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 1)
			recoveryAwaitCompletion(t, h, r)
			recoveryStopWorker(t, replacement, false)
			if f.badRequest.Load() || f.count("/chat/completions") != wantChats || f.count("order") != 1 || f.count("delivery") != 1 {
				t.Fatal("external recovery replayed accepted prefix or lost old chat exposure")
			}
			t.Logf("actual external %s: phase=%s queued=%d consumed=false kill=%s Wait=%s groupGone=%s natural recovery=%s", mode, receipt.Phase, receipt.Queued, receipt.KillSent.Format(time.RFC3339Nano), receipt.WaitAt.Format(time.RFC3339Nano), receipt.GoneAt.Format(time.RFC3339Nano), time.Since(start))
		})
	}
}

func TestRunRecoveryExecutorProtocolFactsCannotBecomePureLoss(t *testing.T) {
	for _, mode := range []string{"bad_frame", "ordinary_fragment", "metering_fragment", "stderr_oversize"} {
		t.Run(mode, func(t *testing.T) {
			h, r, f, gateway, _ := recoveryExecutorSetup(t, "protocol-"+mode)
			recoveryFault(t, r.ID, 1, mode)
			worker := recoveryStartWorker(t, h, f.BusinessOrigin, gateway, 0)
			marker := recoveryReadMarker(t, r.ID, 1, nil, 15*time.Second)
			if mode == "ordinary_fragment" || mode == "metering_fragment" {
				// The real fragment becomes observable at EOF caused by this
				// actual kill; a signal must not erase the broken protocol.
				group := recoveryGroup(t, worker, marker)
				if err := syscall.Kill(marker.PID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
				recoveryGroupGone(t, group, marker.PID)
			}
			recoveryEventually(t, 10*time.Second, func() bool {
				current, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
				return err == nil && current.State == agentrun.Failed && current.RecoveryCount == 0
			})
			recoveryStopWorker(t, worker, false)
			var proof bool
			if err := h.Pool.QueryRow(h.Ctx, "select recovery_step is not null or recovery_ordinal is not null from run_attempts where run_id=$1", r.ID).Scan(&proof); err != nil || proof || f.count("/chat/completions") != 0 {
				t.Fatal("protocol/size facts invented recovery or an HTTP permission")
			}
		})
	}
}
