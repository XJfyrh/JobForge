//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
	"github.com/xjfyrh/jobforge/internal/runworker"
)

const providerUnconfirmedCallsSQL = "select count(*) from physical_calls where run_id=$1 and ((subcall='chat' and status<>'known') or observation_hash is null or measurement_anomaly)"

// This opt-in test never runs in ordinary CI. Its installed registry and provider
// origin are production originals. Only the captured business/embedding data are
// synthetic. The launcher owns single-run admission and supplies secrets by pipe.
func TestRunRealProviderWorker(t *testing.T) {
	if os.Getenv("JOBFORGE_REAL_WORKER_ACCEPTANCE") != "approved-single-run-v1" {
		t.Skip("requires separately authorized isolated real-provider acceptance")
	}
	runProviderWorker(t, false, true)
}

// The isolated runner has no Internet route; this proxy never opens a tunnel.
func TestRunProviderProxyOffline(t *testing.T) {
	if os.Getenv("JOBFORGE_PROVIDER_PROXY_OFFLINE") != "1" {
		t.Skip("requires isolated installed Worker and PostgreSQL")
	}
	for _, agent := range []bool{false, true} {
		name := "fixed"
		if agent {
			name = "agent"
		}
		t.Run(name, func(t *testing.T) { runProviderWorker(t, true, agent) })
	}
}

func runProviderWorker(t *testing.T, offline, agent bool) {
	t.Helper()
	var proxyCalls atomic.Int32
	var proxyLeak atomic.Bool
	var workerPID atomic.Int64
	var modelEnvChecked, businessEnvChecked atomic.Bool
	var input struct {
		APIKey     string `json:"api_key"`
		Proxy      string `json:"proxy"`
		CA         string `json:"ca"`
		ObservedOn string `json:"observed_on"`
		PriceSHA   string `json:"price_sha"`
	}
	if offline {
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxyCalls.Add(1)
			modelEnvChecked.Store(offlineProviderEnvironment(int(workerPID.Load()), true))
			if r.Method != http.MethodConnect || r.Host != "api.deepseek.com:443" || r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" {
				proxyLeak.Store(true)
			}
			w.WriteHeader(http.StatusProxyAuthRequired)
		}))
		t.Cleanup(proxy.Close)
		input.APIKey, input.Proxy, input.CA = "synthetic-provider-only", proxy.URL, "/opt/jobforge-provider-ca.pem"
		input.ObservedOn, input.PriceSHA = time.Now().UTC().Format("2006-01-02"), strings.Repeat("a", 64)
	} else if json.NewDecoder(io.LimitReader(os.Stdin, 16384)).Decode(&input) != nil || input.APIKey == "" || !agentrun.ValidHash(input.PriceSHA) {
		t.Fatal("invalid private pipe input")
	}
	if parsed, err := time.Parse("2006-01-02", input.ObservedOn); err != nil || parsed.Format("2006-01-02") != time.Now().UTC().Format("2006-01-02") {
		t.Fatal("price snapshot is not current")
	}
	h := setupSupportProfileConfiguredHarness(t, "support-agent-real-single-v1", 3000000, agent, func(d *agentrun.SupportDefinition) {
		d.Model.ObservedOn, d.Price.ObservedOn, d.Price.SourceSHA256 = input.ObservedOn, input.ObservedOn, input.PriceSHA
		d.Program.AdapterSourceSHA256 = liveSourceHash(t, "/usr/local/lib/python3.12/site-packages/jobforge_agent/support_agent.py")
		d.Program.PromptSHA256 = d.Program.AdapterSourceSHA256 // Prompt and decision code share this installed module.
		d.Program.DecisionSchemaSHA256 = liveSourceHash(t, "/src/api/support/agent-v1/schema.json")
		d.Program.ProposalSchemaSHA256 = liveSourceHash(t, "/src/api/support/v1/schema.json")
	})
	snapshot := supportProfileCapture(t, h, "submit", "submit-real-single", "", false)
	r := h.submit(t, "tenant-north", "real-single")
	fixture := &supportExecutorHTTPFixture{executorHTTPFixture: &executorHTTPFixture{counts: map[string]int{}}, withOrder: false}
	fixture.snapshot = snapshot
	policyBytes, err := os.ReadFile("/src/policies/P03.md")
	if err != nil {
		t.Fatal("read public runtime missing-facts policy")
	}
	_, policyText, found := strings.Cut(string(policyBytes), "<!-- paragraph_id: P03.1 -->")
	policyText, _, _ = strings.Cut(policyText, "<!-- paragraph_id: P03.2 -->")
	policyText = strings.TrimSpace(policyText)
	if !found || policyText == "" {
		t.Fatal("missing public P03.1 paragraph")
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/chat/completions" {
			fixture.reject(w)
			return
		}
		if request.URL.Path == "/api/embed" {
			var body struct {
				Model    string   `json:"model"`
				Input    []string `json:"input"`
				Truncate bool     `json:"truncate"`
			}
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != "" || json.NewDecoder(http.MaxBytesReader(w, request.Body, 8192)).Decode(&body) != nil || body.Model != "all-minilm:22m" || len(body.Input) != 1 || body.Truncate {
				fixture.reject(w)
				return
			}
			vector := make([]float64, 384)
			vector[0] = 1
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "all-minilm:22m", "embeddings": [][]float64{vector}, "prompt_eval_count": 3})
			return
		}
		if offline {
			businessEnvChecked.Store(offlineProviderEnvironment(int(workerPID.Load()), false))
		}
		if strings.HasSuffix(request.URL.Path, "/search") {
			captured := httptest.NewRecorder()
			fixture.serveSupport(captured, request)
			if captured.Code != http.StatusOK {
				w.WriteHeader(captured.Code)
				return
			}
			var response map[string]any
			if json.Unmarshal(captured.Body.Bytes(), &response) != nil {
				fixture.reject(w)
				return
			}
			response["matches"] = []any{map[string]any{"index_id": snapshot.IndexID, "chunk_id": "P03.1", "policy_version": agentrun.SupportPolicyVersion, "evidence_ref": "business-policy:" + snapshot.IndexID + ":P03.1", "text": policyText, "source": "runtime/policies/P03.md", "distance": 0.1}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		fixture.serveSupport(w, request)
	})
	business := httptest.NewServer(handler)
	t.Cleanup(business.Close)
	listener, err := net.Listen("tcp", "127.0.0.1:11434")
	if err != nil {
		t.Fatal(err)
	}
	embedding := httptest.NewUnstartedServer(handler)
	_ = embedding.Listener.Close()
	embedding.Listener = listener
	embedding.Start()
	t.Cleanup(embedding.Close)
	gateway := executorGateway(t, h)
	adapterID := "support-fixed-v1"
	if agent {
		adapterID = "support-agent-v1"
	}
	launcherJSON(t, "/etc/jobforge/executor.json", runworker.Manifest{SchemaVersion: 1, ExecutorVersion: h.Profile.ExecutorVersion, Profiles: []runworker.ManifestProfile{{ProfileID: h.Profile.ID, ProfileHash: h.Profile.Hash, AdapterID: adapterID}}})
	config := map[string]any{"schema_version": 1, "profiles": []agentrun.Profile{h.Profile}, "tenants": map[string]any{"tenant-north": map[string]string{"business_origin": business.URL, "ollama_origin": "http://127.0.0.1:11434", "deepseek_proxy_origin": input.Proxy, "deepseek_ca_file": input.CA}}}
	launcherJSON(t, "/etc/jobforge/cloud/worker.json", config)
	credentials, err := json.Marshal(map[string]any{"control_token": "executor-synthetic-control-token", "tenants": map[string]any{"tenant-north": map[string]string{"business_read_key": "synthetic-business-read-key", "deepseek_api_key": input.APIKey}}})
	if err != nil {
		t.Fatal("encode private pipe")
	}
	input.APIKey = ""
	command := exec.Command("/usr/local/bin/agent-worker")
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1", "JOBFORGE_AGENT_WORKER_CONFIG=/etc/jobforge/cloud/worker.json", "JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE=/dev/stdin", "JOBFORGE_AGENT_GATEWAY=" + gateway.target, "JOBFORGE_AGENT_GRPC_TLS=false"}
	command.Stdin = bytes.NewReader(credentials)
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal("start formal Worker")
	}
	workerPID.Store(int64(command.Process.Pid))
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	groups := map[int]bool{}
	stopped := false
	workerExitCode := -1
	recordExit := func(err error) {
		if err == nil {
			workerExitCode = 0
			return
		}
		var status *exec.ExitError
		if errors.As(err, &status) {
			workerExitCode = status.ExitCode()
		}
	}
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case err := <-done:
			recordExit(err)
		case <-time.After(5 * time.Second):
			_ = command.Process.Kill()
			recordExit(<-done)
			t.Error("Worker required forced termination")
		}
	}
	// Export before the harness tears down its owned database, even on failure.
	t.Cleanup(func() {
		stop()
		for group := range groups {
			if !errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
				t.Error("guardian group survived Worker stop")
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		view, viewErr := h.Store.Get(ctx, r.TenantID, r.ID)
		calls, callErr := h.Store.Calls(ctx, r.TenantID, r.ID)
		var known, held int64
		budgetErr := h.Pool.QueryRow(ctx, "select known_cost_microyuan,held_cost_microyuan from budget_accounts where account_id=$1", r.Budget.Batch.ID).Scan(&known, &held)
		if viewErr != nil || callErr != nil || budgetErr != nil {
			t.Error("could not export complete budget evidence")
			return
		}
		report, _ := json.Marshal(map[string]any{"run_id": r.ID, "state": view.State, "calls": calls, "known_cost_microyuan": known, "held_cost_microyuan": held, "batch_cap_microyuan": 3000000, "worker_stopped": stopped, "worker_exit_code": workerExitCode, "guardian_groups_seen": len(groups), "synthetic_business": true, "real_provider": !offline, "profile_hash": h.Profile.Hash})
		t.Logf("REAL_PROVIDER_RECEIPT %s", report)
		if known+held > 3000000 {
			t.Error("batch ceiling violated")
		}
	})
	deadline := time.NewTimer(140 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			recordExit(err)
			stopped = true
			if offline {
				if proxyCalls.Load() != 1 || proxyLeak.Load() || fixture.badRequest.Load() || !modelEnvChecked.Load() || (!agent && !businessEnvChecked.Load()) {
					t.Fatal("scoped proxy/credential boundary failed")
				}
				if len(groups) < 2 {
					t.Fatal("formal child lifecycle evidence missing")
				}
				var unconfirmed int
				if err := h.Pool.QueryRow(h.Ctx, providerUnconfirmedCallsSQL, r.ID).Scan(&unconfirmed); err != nil || unconfirmed != 1 {
					t.Fatal("free observed calls were confused with the one unconfirmed chat")
				}
				t.Log("OFFLINE_PROXY_VERIFIED formal coordinator/Worker/guardian/step reached exactly one local CONNECT without credentials")
				return
			}
			t.Fatal("formal Worker stopped before terminal proposal; export receipt and do not retry")
		case <-deadline.C:
			t.Fatal("single-run deadline exceeded")
		case <-ticker.C:
			for _, pid := range executorChildren(command.Process.Pid) {
				groups[pid] = true
			}
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil {
				t.Fatal("read run status")
			}
			switch view.State {
			case agentrun.AwaitingApproval:
				stop()
				if fixture.badRequest.Load() {
					t.Fatal("business credential or fixture contract violation")
				}
				var badCalls int
				if err := h.Pool.QueryRow(h.Ctx, providerUnconfirmedCallsSQL, r.ID).Scan(&badCalls); err != nil || badCalls != 0 {
					t.Fatal("unconfirmed or unknown call")
				}
				liveSDKReadback(t, h.Service, r.ID)
				return
			case agentrun.Failed, agentrun.Cancelled, agentrun.Succeeded:
				t.Fatalf("real run ended without proposal: %s", view.State)
			}
		}
	}
}

func liveSourceHash(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("read public source artifact")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func liveSDKReadback(t *testing.T, service httpapi.API, runID string) {
	t.Helper()
	router, err := httpapi.NewRouter(service, map[string]httpapi.Identity{"synthetic-reader": {TenantID: "tenant-north", Role: "reader"}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	const code = `import json,sys
from jobforge import RunClient
with RunClient(sys.argv[1], "synthetic-reader") as c:
 r=c.get(sys.argv[2]);s=c.steps(r.run_id,limit=100);a=c.calls(r.run_id)
 assert r.state.value=="awaiting_approval" and c.result(r.run_id).available
 assert s.next_after is None and not a.batch_frozen
 print(json.dumps({"state":r.state.value,"steps":len(s.items),"physical_calls":len(a.items),"proposal":s.items[-1].output["proposal"]}))
`
	command := exec.CommandContext(ctx, "/usr/local/bin/python", "-I", "-c", code, server.URL, runID)
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "PYTHONDONTWRITEBYTECODE=1"}
	output := &runContractOutput{}
	command.Stdout, command.Stderr = output, output
	if command.Run() != nil || output.overflow {
		t.Fatal("installed SDK public readback failed")
	}
	t.Logf("REAL_PROVIDER_SDK %s", strings.TrimSpace(output.String()))
}

// Only the offline fixture calls this: all credentials are synthetic. Retain no
// values or raw environment, and require both guardian and step to be observed.
func offlineProviderEnvironment(workerPID int, model bool) bool {
	seen := 0
	for _, guardian := range executorChildren(workerPID) {
		for _, pid := range append([]int{guardian}, executorChildren(guardian)...) {
			raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
			if err != nil {
				return false
			}
			keys := map[string]bool{}
			for _, pair := range strings.Split(string(raw), "\x00") {
				key, _, _ := strings.Cut(pair, "=")
				keys[key] = true
			}
			for _, key := range []string{"DEEPSEEK_API_KEY", "JOBFORGE_DEEPSEEK_PROXY_ORIGIN", "JOBFORGE_DEEPSEEK_CA_FILE"} {
				if keys[key] != model {
					return false
				}
			}
			for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "JOBFORGE_TEST_DSN", "JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE"} {
				if keys[key] {
					return false
				}
			}
			if model && (keys["JOBFORGE_BUSINESS_READ_KEY"] || keys["JOBFORGE_OLLAMA_ORIGIN"]) {
				return false
			}
			seen++
		}
	}
	return seen >= 2
}
