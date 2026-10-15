//go:build linux

package integration

import (
	"context"
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
	for _, mode := range []string{"dynamic", "correction_then_dynamic", "duplicate", "correction_then_later_invalid", "correction_invalid", "adjudication", "adjudication_empty", "adjudication_truncated", "adjudication_oversize", "adjudication_long", "adjudication_cancel", "adjudication_supplement", "adjudication_supplement_empty", "adjudication_supplement_cancel", "adjudication_status", "adjudication_status_empty", "adjudication_status_supplement", "adjudication_status_cancel", "adjudication_bundle", "adjudication_bundle_fixed", "adjudication_bundle_cancel", "adjudication_bundle_v9", "adjudication_bundle_v9_cancel"} {
		t.Run(mode, func(t *testing.T) {
			adjudication := strings.HasPrefix(mode, "adjudication")
			bundle := strings.HasPrefix(mode, "adjudication_bundle")
			fixedBundle := mode == "adjudication_bundle_fixed"
			v9 := strings.HasPrefix(mode, "adjudication_bundle_v9")
			statusPolicy := strings.HasPrefix(mode, "adjudication_status")
			supplement := strings.HasPrefix(mode, "adjudication_supplement") || mode == "adjudication_status_supplement"
			cancelFinal := strings.HasSuffix(mode, "_cancel")
			var h *runHarness
			if adjudication {
				if bundle {
					schema := 16
					if fixedBundle {
						schema = 17
					}
					if v9 {
						schema = 18
					}
					h = setupAdjudicationProfileHarness(t, fixedBundle, schema)
				} else if statusPolicy {
					h = setupAdjudicationProfileHarness(t, false, 15)
				} else if supplement {
					h = setupAdjudicationProfileHarness(t, false, 14)
				} else {
					h = setupAdjudicationHarness(t)
				}
			} else {
				h = setupSupportProfileStrategyHarness(t, "support-agent-synthetic-v1", 20000000, true)
			}
			finalStarted := make(chan struct{})
			// Install a complete, version-matched synthetic manifest for this test,
			// restoring the S1 manifest before any other test runs.
			path := "/etc/jobforge/executor.json"
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			manifest := runworker.Manifest{SchemaVersion: 1, ExecutorVersion: h.Profile.ExecutorVersion,
				Profiles: []runworker.ManifestProfile{{ProfileID: h.Profile.ID, ProfileHash: h.Profile.Hash, AdapterID: "support-agent-v1"}}}
			if adjudication {
				manifest.SchemaVersion, manifest.Profiles[0].PromptVersion = 2, agentrun.SupportAgentPromptV4
				if supplement {
					manifest.Profiles[0].PromptVersion = agentrun.SupportAgentPromptV6
				}
				if statusPolicy {
					manifest.Profiles[0].PromptVersion = agentrun.SupportAgentPromptV7
				}
			}
			if bundle {
				manifest.Profiles[0].PromptVersion = agentrun.SupportAgentPromptV8
				if v9 {
					manifest.Profiles[0].PromptVersion = agentrun.SupportAgentPromptV9
				}
				if fixedBundle {
					manifest.Profiles[0].PromptVersion, manifest.Profiles[0].AdapterID = agentrun.SupportFixedPromptV2, "support-fixed-v1"
				}
			}
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
			if statusPolicy {
				snapshot = statusPolicyCapture(t, h, mode, "informational_only")
			}
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
			if adjudication {
				decisions = append(decisions, decisions[len(decisions)-1])
				decisions[len(decisions)-2] = `{"type":"final","proposal":{"decision":"proposal","action":"DRAFT_ONLY","conclusion":"conflicting","requested_fields":[],"target_ticket_status":"escalated","claims":[]}}`
			}
			if bundle {
				decisions = []string{decisions[0], decisions[2], decisions[4], decisions[5]}
				if fixedBundle {
					decisions = decisions[3:]
				}
			}
			if supplement {
				decisions = append(decisions, decisions[len(decisions)-1])
				decisions[len(decisions)-2] = `{"type":"policy_supplement","topic":"missing_facts","required_aliases":["P03.2"]}`
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
				if bundle && strings.HasSuffix(req.URL.Path, "/policies/bundle") {
					if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer synthetic-business-read-key" || req.URL.RawQuery != "" || req.URL.Path != "/business/v1/snapshots/"+snapshot.ID+"/policies/bundle" {
						f.reject(w)
						return
					}
					content, _ := bundleStorageContent(t, snapshot)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(content)
					return
				}
				if (supplement || statusPolicy) && strings.HasSuffix(req.URL.Path, "/policies/search") && f.count("search") >= 2 {
					statusRead := statusPolicy && f.count("search") == 2
					captured := httptest.NewRecorder()
					f.serveSupport(captured, req)
					if captured.Code != http.StatusOK {
						f.reject(w)
						return
					}
					var response map[string]any
					if json.Unmarshal(captured.Body.Bytes(), &response) != nil {
						f.reject(w)
						return
					}
					hit := response["matches"].([]any)[0].(map[string]any)
					hit["chunk_id"], hit["source"], hit["text"] = "P03.2", "P03.md", "Synthetic supplement definition"
					hit["evidence_ref"] = "business-policy:" + snapshot.IndexID + ":P03.2"
					if statusRead {
						hit["chunk_id"], hit["source"], hit["text"] = "P08.1", "P08.md", "Synthetic status prerequisite"
						hit["evidence_ref"] = "business-policy:" + snapshot.IndexID + ":P08.1"
					}
					if mode == "adjudication_supplement_empty" || mode == "adjudication_status_empty" {
						response["matches"] = []any{}
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(response)
					return
				}
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
					if json.Unmarshal(body["input"], &query) != nil || len(query) != 1 || (query[0] != "delivery timing" && query[0] != "late delivery action" && !(supplement && query[0] == "Missing related order delivery tracking event or vague complaint needing specific problem description clarification") && !(statusPolicy && query[0] == "Already informational_only ticket action none; already escalated preserve status no silent downgrade")) {
						f.reject(w)
						return
					}
					vector := make([]float64, 384)
					vector[0] = 1
					_ = json.NewEncoder(w).Encode(map[string]any{"model": "all-minilm:22m", "embeddings": [][]float64{vector}, "prompt_eval_count": 3})
					return
				}
				thinking := adjudication && (n == len(decisions) || supplement && n == len(decisions)-1)
				if req.Header.Get("Authorization") != "Bearer synthetic-provider-key" || n > len(decisions) || (!((supplement || statusPolicy || bundle) && thinking) && !strings.Contains(string(body["messages"]), "previous_tools")) {
					f.reject(w)
					return
				}
				if v9 && thinking && (!strings.Contains(string(body["messages"]), "CRITICAL AND TIMING COMPLETENESS:") || !strings.Contains(string(body["messages"]), "OUTSTANDING TIMING EVIDENCE:")) {
					f.reject(w)
					return
				}
				if bundle && (!strings.Contains(string(body["messages"]), "P05.1") || !strings.Contains(string(body["messages"]), "P10.2")) {
					f.reject(w)
					return
				}
				final := thinking
				wantTokens, wantMode := "1024", `{"type":"disabled"}`
				if final {
					if n == len(decisions) {
						close(finalStarted)
					}
					if cancelFinal && n == len(decisions) {
						<-req.Context().Done()
						return
					}
					wantTokens, wantMode = "16384", `{"type":"enabled"}`
					if string(body["reasoning_effort"]) != `"high"` || body["temperature"] != nil || strings.Contains(string(body["messages"]), "DRAFT_ONLY") {
						f.reject(w)
						return
					}
					if (supplement || statusPolicy || bundle) && (strings.Contains(string(body["messages"]), "previous_tools") || supplement && n == len(decisions) && strings.Contains(string(body["messages"]), "policy_supplement")) {
						f.reject(w)
						return
					}
				}
				if string(body["max_tokens"]) != wantTokens || string(body["thinking"]) != wantMode {
					f.reject(w)
					return
				}
				content, finish, reasoning, output, reasoningTokens := decisions[n-1], "stop", "", 5, 0
				if final {
					reasoning, output, reasoningTokens = strings.Repeat("synthetic reasoning ", 4000), 16384, 16000
					if mode == "adjudication_empty" {
						reasoning, reasoningTokens = "", 0
					}
					if mode == "adjudication_truncated" {
						content, finish, reasoningTokens = "", "length", 16384
					}
					if mode == "adjudication_oversize" {
						content = `{"too_long":"` + strings.Repeat("x", 17000) + `"}`
					}
					if mode == "adjudication_long" {
						select {
						case <-time.After(65 * time.Second):
						case <-req.Context().Done():
							return
						}
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-agent-chat", "object": "chat.completion", "created": 1, "model": "deepseek-flash", "system_fingerprint": "fp_synthetic_agent",
					"choices": []any{map[string]any{"index": 0, "finish_reason": finish, "logprobs": nil, "message": map[string]any{"role": "assistant", "content": content, "reasoning_content": reasoning}}},
					"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": output, "total_tokens": 10 + output, "prompt_cache_hit_tokens": 2, "prompt_cache_miss_tokens": 8, "completion_tokens_details": map[string]any{"reasoning_tokens": reasoningTokens}}})
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
			var expectedErrors []error
			if cancelFinal {
				expectedErrors = []error{runworker.ErrBatchStopped}
			}
			running := startExecutorWorker(t, h, f.executorHTTPFixture, client, expectedErrors...)
			t.Cleanup(func() {
				if !t.Failed() {
					return
				}
				var diagnostic string
				ctx, cancel := context.WithTimeout(context.WithoutCancel(h.Ctx), time.Second)
				defer cancel()
				if err := h.Pool.QueryRow(ctx, `select coalesce(json_agg(x)::text,'[]') from (select r.state,r.cursor_version,r.next_step_kind,r.error_code,a.error_code as attempt_error from runs r join run_attempts a using(run_id) where r.run_id=$1) x`, r.ID).Scan(&diagnostic); err == nil {
					t.Log("run failure metadata", diagnostic)
				}
				if err := h.Pool.QueryRow(ctx, `select coalesce(json_agg(x)::text,'[]') from (select step_kind,subcall,status,business_outcome,observation_error_code from physical_calls where run_id=$1 order by reserved_at) x`, r.ID).Scan(&diagnostic); err == nil {
					t.Log("call outcomes", diagnostic)
				}
				if err := h.Pool.QueryRow(ctx, `select coalesce(json_agg(x)::text,'[]') from (select step_kind,status,transport_outcome,business_outcome,observation_error_code,call_report->'provider_audit' as audit from physical_calls where run_id=$1 and kind='chat' order by reserved_at) x`, r.ID).Scan(&diagnostic); err == nil {
					t.Log("chat ledger metadata", diagnostic)
				} else {
					t.Log("diagnostic failed", err)
				}
			})
			if cancelFinal {
				waitExecutorSignal(t, running, finalStarted)
				if _, err := h.Store.Cancel(h.Ctx, r.TenantID, r.ID, "cancel-long-final"); err != nil {
					t.Fatal(err)
				}
				select {
				case <-running.done:
				case <-time.After(15 * time.Second):
					t.Fatal("cancelled final ignored lease revocation")
				}
				view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
				if err != nil || (view.State != agentrun.Stopping && view.State != agentrun.Cancelled) || view.CancelRequestedAt == nil || view.ProposalRef != nil || f.count("/chat/completions") != len(decisions) {
					t.Fatal("cancelled final produced proposal or repeated call", view.State, err)
				}
				if view.State == agentrun.Stopping {
					if _, err := h.Pool.Exec(h.Ctx, "update runs set lease_until=clock_timestamp()-interval '1 millisecond' where run_id=$1", r.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
						t.Fatal(err)
					}
					view, err = h.Store.Get(h.Ctx, r.TenantID, r.ID)
					if err != nil || view.State != agentrun.Cancelled {
						t.Fatal("cancelled final not closed by lease recovery", err)
					}
				}
				calls, err := h.Store.Calls(h.Ctx, r.TenantID, r.ID)
				if err != nil || len(calls.Items) == 0 {
					t.Fatal("missing cancellation exposure", err)
				}
				last := calls.Items[len(calls.Items)-1]
				if last.StepKind != "model_proposal" || last.UsageKnown || last.HeldTokens != last.Reserved.TotalTokens || last.HeldCostMicroyuan != last.Reserved.CostMicroyuan {
					t.Fatal("cancellation released unknown terminal exposure")
				}
				return
			}
			if mode == "adjudication_long" {
				select {
				case <-client.looped:
				case <-running.done:
					t.Fatal("long final stopped early", running.err)
				case <-time.After(100 * time.Second):
					t.Fatal("long final did not finish")
				}
			} else {
				waitExecutorSignal(t, running, client.looped)
			}
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantTools, wantSteps, wantCalls := agentrun.AwaitingApproval, 4, 11, 15
			wantChats := len(decisions)
			if adjudication {
				wantSteps, wantCalls = 12, 16
				if mode == "adjudication_truncated" || mode == "adjudication_oversize" {
					wantState, wantSteps = agentrun.Failed, 10
				}
			}
			if bundle {
				wantTools, wantSteps, wantCalls = 3, 9, 7
				if fixedBundle {
					wantSteps, wantCalls = 6, 4
				}
			}
			if supplement {
				wantTools, wantSteps, wantCalls = 5, 14, 21
				if mode == "adjudication_supplement_empty" {
					wantState, wantSteps, wantCalls, wantChats = agentrun.Failed, 11, 20, len(decisions)-1
				}
			}
			if statusPolicy {
				wantTools++
				wantSteps++
				wantCalls += 4
				if mode == "adjudication_status_empty" {
					wantState, wantSteps, wantCalls, wantChats = agentrun.Failed, 10, 19, 5
				}
			}
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
			if view.State != wantState || int(view.CursorVersion) != wantSteps || f.badRequest.Load() || f.count("/chat/completions") != wantChats {
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
