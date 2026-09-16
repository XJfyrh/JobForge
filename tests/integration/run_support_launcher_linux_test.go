//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/grpcapi"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

// These two barriers delay transport delivery only; production Store decisions,
// PG commits, installed SDK and Worker/guardian/step processes remain real.
type launcherWorkerAPI struct {
	*runpostgres.Store
	blockClaim   bool
	claimSeen    chan struct{}
	claimOnce    sync.Once
	chatSeen     chan struct{}
	chatRelease  chan struct{}
	chatFinished chan struct{}
	claims       atomic.Int64
}

func (a *launcherWorkerAPI) Claim(ctx context.Context, principal, session string) (*agentrun.ClaimedRun, error) {
	a.claims.Add(1)
	if a.blockClaim {
		a.claimOnce.Do(func() { close(a.claimSeen) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.Store.Claim(ctx, principal, session)
}

func (a *launcherWorkerAPI) ReserveCall(ctx context.Context, principal string, request agentrun.ReserveCallRequest) (agentrun.ReserveCallResponse, error) {
	response, err := a.Store.ReserveCall(ctx, principal, request)
	if request.Subcall == agentrun.SubcallChat && err == nil {
		close(a.chatSeen) // PG has committed; the original RPC is still in flight.
		<-a.chatRelease
		close(a.chatFinished)
	}
	return response, err
}

type launcherPublicAPI struct {
	*agentrun.Service
	submitted chan agentrun.Run
	submits   atomic.Int64
}

func (a *launcherPublicAPI) Submit(ctx context.Context, tenant, key string, request agentrun.SubmitRequest) (agentrun.SubmitResponse, error) {
	a.submits.Add(1)
	response, err := a.Service.Submit(ctx, tenant, key, request)
	if err == nil {
		select {
		case a.submitted <- response.Run:
		default:
		}
	}
	return response, err
}

func launcherJSON(t *testing.T, path string, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return raw
}

func launcherSetup(t *testing.T, h *runHarness, origin, gateway string) ([]string, string) {
	t.Helper()
	inspection := supportInspectionRequest(t, h)
	budgets := make([]map[string]any, 0, 3)
	var batch agentrun.BudgetSpec
	for _, b := range inspection.Budgets {
		budgets = append(budgets, map[string]any{"account_id": b.ID, "scope": b.Scope, "scope_key": b.Key,
			"valid_from": b.ValidFrom, "valid_until": b.ValidUntil, "limits": b.Limits})
		if b.Scope == "batch" {
			batch = b
		}
	}
	control := map[string]any{"schema_version": 1, "tenants": []string{"tenant-north", "tenant-south"},
		"profiles": []agentrun.Profile{h.Profile}, "enabled_profiles": []string{h.Profile.ID}, "tenant_capacity": 1, "profile_capacity": 1,
		"workers": []any{map[string]any{"worker_id": h.Principal, "tenants": []string{"tenant-north", "tenant-south"}, "profile_ids": []string{h.Profile.ID}, "capacity": 1}},
		"budgets": budgets, "bindings": inspection.Bindings}
	worker := map[string]any{"schema_version": 1, "profiles": []agentrun.Profile{h.Profile}, "tenants": map[string]any{
		"tenant-north": map[string]string{"business_origin": origin, "ollama_origin": "http://127.0.0.1:11434"}}}
	configHashes := map[string]string{}
	for name, config := range map[string]any{"control.json": control, "worker.json": worker} {
		raw := launcherJSON(t, "/etc/jobforge/cloud/"+name, config)
		hash := sha256.Sum256(raw)
		configHashes[name] = hex.EncodeToString(hash[:])
	}
	launcherJSON(t, "/run/secrets/worker.json", map[string]any{"control_token": "executor-synthetic-control-token", "tenants": map[string]any{
		"tenant-north": map[string]string{"business_read_key": "synthetic-business-read-key", "deepseek_api_key": "synthetic-provider-key"}}})
	launcherJSON(t, "/run/secrets/driver.json", map[string]string{"tenant-north": "launcher-north-key", "tenant-south": "launcher-south-key"})
	cases := make([]map[string]any, 40)
	for i := range cases {
		tenant := "tenant-north"
		if i >= 20 {
			tenant = "tenant-south"
		}
		cases[i] = map[string]any{"ordinal": i + 1, "case_id": fmt.Sprintf("synthetic-%02d", i+1), "tenant_id": tenant,
			"ticket_id": "ticket-1", "business_request_key": fmt.Sprintf("launcher-%02d", i+1), "idempotency_key": fmt.Sprintf("launcher-submit-%02d", i+1)}
	}
	launcherJSON(t, "/etc/jobforge/cloud/launch.json", map[string]any{"schema_version": 1, "batch_account_id": batch.ID,
		"batch_key": batch.Key, "worker_id": h.Principal, "profile_id": h.Profile.ID, "profile_hash": h.Profile.Hash,
		"valid_from": batch.ValidFrom, "valid_until": batch.ValidUntil, "config_sha256": configHashes, "cases": cases})
	// The synthetic target owns this test-only file; retain the original registry
	// after this test so unrelated executor checks keep their installed manifest.
	original, err := os.ReadFile("/etc/jobforge/executor.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile("/etc/jobforge/executor.json", original, 0o644); err != nil {
			t.Error(err)
		}
	})
	launcherJSON(t, "/etc/jobforge/executor.json", map[string]any{"schema_version": 1, "executor_version": h.Profile.ExecutorVersion,
		"profiles": []any{map[string]string{"profile_id": h.Profile.ID, "profile_hash": h.Profile.Hash, "adapter_id": "support-fixed-v1"}}})
	// ConnString retains pgx's original DSN even after setupRunDB changes Database.
	// Give the real inspector the owned database, not the suite's base database.
	pg := h.Pool.Config().ConnConfig
	dsn := url.URL{Scheme: "postgres", Host: net.JoinHostPort(pg.Host, fmt.Sprint(pg.Port)),
		User: url.UserPassword(pg.User, pg.Password), Path: "/" + pg.Database, RawQuery: "sslmode=disable"}
	env := append(os.Environ(), "JOBFORGE_AGENT_CONFIG=/etc/jobforge/cloud/control.json", "JOBFORGE_AGENT_DSN="+dsn.String(),
		"JOBFORGE_AGENT_WORKER_CONFIG=/etc/jobforge/cloud/worker.json", "JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE=/run/secrets/worker.json",
		"JOBFORGE_AGENT_GATEWAY="+gateway, "JOBFORGE_AGENT_GRPC_TLS=false")
	prepare := exec.Command("python", "-m", "tools.support_evaluation.launcher", "prepare-state")
	prepare.Env = env
	if output, err := prepare.CombinedOutput(); err != nil {
		t.Fatalf("prepare actual launcher state: %v %s", err, output)
	}
	return env, filepath.Join("/var/lib/jobforge/batches", batch.ID)
}

func launcherWait(t *testing.T, done <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(12 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func launcherChildren(t *testing.T, pid int, done <-chan struct{}, diagnostic *bytes.Buffer) (worker, driver int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("actual launcher stopped before child discovery: %s", diagnostic.String())
		default:
		}
		for _, child := range executorChildren(pid) {
			raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", child))
			if err != nil {
				continue
			}
			if bytes.Contains(raw, []byte("/usr/local/bin/agent-worker")) {
				worker = child
			}
			if bytes.Contains(raw, []byte("tools.support_evaluation.driver")) {
				driver = child
			}
		}
		if worker != 0 && driver != 0 {
			return worker, driver
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("actual launcher did not start both fixed child processes")
	return 0, 0
}

func TestRunSupportLauncher(t *testing.T) {
	if os.Getenv("JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS") != "1" {
		t.Skip("requires installed Linux launcher, init, formal Worker and real PG; skip is not acceptance")
	}
	for _, mode := range []string{"driver_killed_after_submit", "launcher_term_with_reserve_response_pending"} {
		t.Run(mode, func(t *testing.T) {
			h := setupSupportProfileHarness(t, supportExecutorProfileID)
			snapshot := supportProfileCapture(t, h, "submit", "launcher-submit-01", "", true)
			fixture := supportExecutorHTTP(t, h, agentrun.Run{SnapshotID: snapshot.ID}, true, false)
			workerAPI := &launcherWorkerAPI{Store: h.Store, blockClaim: mode == "driver_killed_after_submit", claimSeen: make(chan struct{}),
				chatSeen: make(chan struct{}), chatRelease: make(chan struct{}), chatFinished: make(chan struct{})}
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(workerAPI.chatRelease) }) })
			gateway, err := grpcapi.NewServer(workerAPI, grpcapi.Config{Workers: h.Options.Workers,
				Credentials: map[string]string{h.Principal: "executor-synthetic-control-token"}})
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			gatewayDone := make(chan error, 1)
			go func() { gatewayDone <- gateway.Serve(listener) }()
			t.Cleanup(func() {
				release.Do(func() { close(workerAPI.chatRelease) })
				gateway.Stop()
				if err := <-gatewayDone; err != nil {
					t.Error(err)
				}
			})
			publicAPI := &launcherPublicAPI{Service: h.Service, submitted: make(chan agentrun.Run, 1)}
			router, err := httpapi.NewRouter(publicAPI, map[string]httpapi.Identity{
				"launcher-north-key": {TenantID: "tenant-north", Role: "operator"}, "launcher-south-key": {TenantID: "tenant-south", Role: "operator"}})
			if err != nil {
				t.Fatal(err)
			}
			httpListener, err := net.Listen("tcp", "0.0.0.0:8093")
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: router, ReadHeaderTimeout: time.Second}
			go func() { _ = server.Serve(httpListener) }()
			t.Cleanup(func() { _ = server.Close() })
			env, state := launcherSetup(t, h, fixture.BusinessOrigin, listener.Addr().String())
			command := exec.Command("python", "-m", "tools.support_evaluation.launcher")
			command.Env = env
			var diagnostic bytes.Buffer
			command.Stdout, command.Stderr = &diagnostic, &diagnostic
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			var exitErr error
			go func() { exitErr = command.Wait(); close(done) }()
			t.Cleanup(func() { _ = command.Process.Kill(); launcherWait(t, done, "launcher cleanup") })
			worker, driver := launcherChildren(t, command.Process.Pid, done, &diagnostic)
			var submitted agentrun.Run
			select {
			case submitted = <-publicAPI.submitted:
			case <-done:
				t.Fatalf("launcher stopped before Submit: %s", diagnostic.String())
			case <-time.After(12 * time.Second):
				t.Fatal("installed SDK never submitted")
			}
			var guardian, step int
			if workerAPI.blockClaim {
				launcherWait(t, workerAPI.claimSeen, "Claim barrier")
				if err := syscall.Kill(driver, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			} else {
				launcherWait(t, workerAPI.chatSeen, "committed Reserve before response delivery")
				children := executorChildren(worker)
				if len(children) != 1 {
					t.Fatal("Worker must own the actual guardian at Reserve")
				}
				guardian = children[0]
				children = executorChildren(guardian)
				if len(children) != 1 {
					t.Fatal("guardian must own the actual step at Reserve")
				}
				step = children[0]
				if err := command.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			}
			launcherWait(t, done, "actual launcher/Worker Wait")
			if exitErr == nil {
				t.Fatal("faulted launch was reported successful")
			}
			for _, pid := range []int{worker, driver, guardian, step} {
				if pid != 0 && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
					t.Fatalf("old fixed process %d survived launcher Wait", pid)
				}
			}
			if guardian != 0 && !errors.Is(syscall.Kill(-guardian, 0), syscall.ESRCH) {
				t.Fatal("old guardian process group survived")
			}
			release.Do(func() { close(workerAPI.chatRelease) })
			if guardian != 0 {
				launcherWait(t, workerAPI.chatFinished, "late original Reserve reply")
			}
			var stopped struct {
				DetectedAt      time.Time `json:"detected_at"`
				WaitCompletedAt time.Time `json:"wait_completed_at"`
				ChildrenReaped  bool      `json:"children_reaped"`
			}
			raw, err := os.ReadFile(filepath.Join(state, "stopped.json"))
			if err != nil || json.Unmarshal(raw, &stopped) != nil || !stopped.ChildrenReaped || stopped.DetectedAt.After(stopped.WaitCompletedAt) {
				t.Fatal("launcher did not persist observed/reaped stop facts", err)
			}
			view, err := h.Store.Get(h.Ctx, submitted.TenantID, submitted.ID)
			if err != nil {
				t.Fatal(err)
			}
			calls, err := h.Store.Calls(h.Ctx, submitted.TenantID, submitted.ID)
			if err != nil || fixture.count("/chat/completions") != 0 || fixture.badRequest.Load() {
				t.Fatal("stopped or undelivered permit sent provider HTTP", err)
			}
			if workerAPI.blockClaim {
				if view.State != agentrun.Ready || len(calls.Items) != 0 {
					t.Fatal("driver death changed the original ready Run")
				}
			} else {
				chat := calls.Items[len(calls.Items)-1]
				if chat.Subcall != agentrun.SubcallChat || chat.UsageKnown || chat.HeldCostMicroyuan != 2105344 || chat.HeldTokens != 1049600 || chat.ObservedAt != nil {
					t.Fatal("in-flight Reserve hold was refunded or late permit consumed")
				}
			}
			claims := workerAPI.claims.Load()
			again := exec.Command("python", "-m", "tools.support_evaluation.launcher")
			again.Env = env
			if err := again.Run(); err == nil {
				t.Fatal("attempted batch restarted")
			}
			var sessions int
			if err := h.Pool.QueryRow(h.Ctx, "select count(*) from worker_sessions where worker_id=$1", h.Principal).Scan(&sessions); err != nil || sessions != 1 || publicAPI.submits.Load() != 1 || workerAPI.claims.Load() != claims {
				t.Fatal("restart rejection created a session/Submit/Claim", err)
			}
			t.Logf("fault=%s detection=%s worker_wait=%s sessions=%d submits=%d chat_http=%d", mode,
				stopped.DetectedAt.Format(time.RFC3339Nano), stopped.WaitCompletedAt.Format(time.RFC3339Nano), sessions, publicAPI.submits.Load(), fixture.count("/chat/completions"))
		})
	}
}
