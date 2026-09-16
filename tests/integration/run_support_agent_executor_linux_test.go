//go:build linux

package integration

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runworker"
)

// All endpoints in this test are synthetic. The installed Python adapter, Go
// worker, process barriers, gRPC and PostgreSQL remain real.
func TestRunSupportAgentExecutor(t *testing.T) {
	if os.Getenv("JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS") != "1" {
		t.Skip("requires fixed Linux image, init and real PostgreSQL; skip is not acceptance")
	}
	for _, mode := range []string{"dynamic", "correction_then_dynamic", "duplicate", "correction_then_later_invalid", "correction_invalid"} {
		t.Run(mode, func(t *testing.T) {
			h := setupSupportProfileStrategyHarness(t, "support-agent-synthetic-v1", 20000000, true)
			// Install a complete, version-matched synthetic manifest for this test,
			// restoring the S1 manifest before any other test runs.
			path := "/etc/jobforge/executor.json"
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			manifest := runworker.Manifest{SchemaVersion: 1, ExecutorVersion: h.Profile.ExecutorVersion,
				Profiles: []runworker.ManifestProfile{{ProfileID: h.Profile.ID, ProfileHash: h.Profile.Hash, AdapterID: "support-agent-v1"}}}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Error(err)
				}
			})
			snapshot := supportProfileCapture(t, h, "submit", "submit-"+mode, "", true)
			r := h.submit(t, "tenant-north", mode)
			f := &supportExecutorHTTPFixture{executorHTTPFixture: &executorHTTPFixture{counts: map[string]int{}}, withOrder: true}
			f.snapshot = snapshot
			decisions := []string{
				`{"type":"tool","name":"get_order","arguments":{"order_id":"order-1"}}`,
				`{"type":"tool","name":"search_policy","arguments":{"query":"delivery timing"}}`,
				`{"type":"tool","name":"get_delivery","arguments":{"order_id":"order-1"}}`,
				`{"type":"tool","name":"search_policy","arguments":{"query":"late delivery action"}}`,
				`{"type":"final","proposal":{"decision":"proposal","action":"escalate","conclusion":"delayed","requested_fields":[],"target_ticket_status":"escalated","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["T#/observed_at","E1#/order/promised_delivery_at","P01.1"]}]}}`,
			}
			switch mode {
			case "correction_then_dynamic":
				decisions = append([]string{`{}`}, decisions...)
			case "duplicate":
				decisions = []string{decisions[0], decisions[0]}
			case "correction_then_later_invalid":
				decisions = []string{`{}`, decisions[0], `{}`}
			case "correction_invalid":
				decisions = []string{`{}`, `{}`}
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/chat/completions" && req.URL.Path != "/api/embed" {
					f.serveSupport(w, req)
					return
				}
				f.mu.Lock()
				f.counts[req.URL.Path]++
				n := f.counts[req.URL.Path]
				f.mu.Unlock()
				var body map[string]json.RawMessage
				if req.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, req.Body, 131072)).Decode(&body) != nil {
					f.reject(w)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if req.URL.Path == "/api/embed" {
					var query []string
					if json.Unmarshal(body["input"], &query) != nil || len(query) != 1 || (query[0] != "delivery timing" && query[0] != "late delivery action") {
						f.reject(w)
						return
					}
					vector := make([]float64, 384)
					vector[0] = 1
					_ = json.NewEncoder(w).Encode(map[string]any{"model": "all-minilm:22m", "embeddings": [][]float64{vector}, "prompt_eval_count": 3})
					return
				}
				if req.Header.Get("Authorization") != "Bearer synthetic-provider-key" || n > len(decisions) || !strings.Contains(string(body["messages"]), "previous_tools") {
					f.reject(w)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-agent-chat", "object": "chat.completion", "created": 1, "model": "deepseek-flash", "system_fingerprint": "fp_synthetic_agent",
					"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "logprobs": nil, "message": map[string]any{"role": "assistant", "content": decisions[n-1]}}},
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
				s := httptest.NewUnstartedServer(handler)
				_ = s.Listener.Close()
				s.Listener = listener
				s.Start()
				t.Cleanup(s.Close)
			}
			client := executorGateway(t, h)
			running := startExecutorWorker(t, h, f.executorHTTPFixture, client)
			waitExecutorSignal(t, running, client.looped)
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantTools, wantSteps, wantCalls := agentrun.AwaitingApproval, 4, 11, 15
			switch mode {
			case "correction_then_dynamic":
				wantSteps++
				wantCalls++
			case "duplicate":
				wantState, wantTools, wantSteps, wantCalls = agentrun.Failed, 1, 4, 3
			case "correction_then_later_invalid":
				wantState, wantTools, wantSteps, wantCalls = agentrun.Failed, 1, 4, 4
			case "correction_invalid":
				wantState, wantTools, wantSteps, wantCalls = agentrun.Failed, 0, 2, 2
			}
			var failure string
			if err := h.Pool.QueryRow(h.Ctx, "select coalesce(error_code,'') from run_attempts where run_id=$1", r.ID).Scan(&failure); err != nil {
				t.Fatal(err)
			}
			if view.State != wantState || int(view.CursorVersion) != wantSteps || f.badRequest.Load() || f.count("/chat/completions") != len(decisions) {
				t.Fatalf("state=%s cursor=%d error=%s chats=%d bad_request=%v", view.State, view.CursorVersion, failure, f.count("/chat/completions"), f.badRequest.Load())
			}
			if wantState == agentrun.Failed && failure != "MODEL_PROTOCOL_ERROR" {
				t.Fatalf("wrong error: %s", failure)
			}
			var tools, calls, observed, unknown, slots int
			if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from tool_invocations where run_id=$1),count(*),count(observation_hash),count(*) filter(where kind='chat' and status<>'known'),(select coalesce(sum(used),0) from execution_slots) from physical_calls where run_id=$1`, r.ID).Scan(&tools, &calls, &observed, &unknown, &slots); err != nil {
				t.Fatal(err)
			}
			if tools != wantTools || calls != wantCalls || observed != calls || unknown != 0 || slots != 0 {
				t.Fatalf("tools=%d calls=%d observed=%d unknown=%d slots=%d", tools, calls, observed, unknown, slots)
			}
			running.cancel()
			select {
			case <-running.done:
			case <-time.After(5 * time.Second):
				t.Fatal("worker did not stop")
			}
			if err := h.Pool.QueryRow(h.Ctx, "select session_id from worker_sessions where worker_id=$1", h.Principal).Scan(&h.Session.ID); err != nil {
				t.Fatal(err)
			}
			supportProfileCapture(t, h, "submit", "submit-next-case", "", true)
			next := h.submit(t, "tenant-north", "next-case")
			claimed, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID)
			if err != nil || claimed == nil || claimed.Checkpoint.Run.ID != next.ID {
				t.Fatalf("audited terminal case blocked next Claim: %v", err)
			}
		})
	}
}
