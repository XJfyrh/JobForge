package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store/postgres"
	"github.com/xjfyrh/jobforge/internal/tasks"
	"go.opentelemetry.io/otel/trace"
)

// modelFaultProxy forwards real inference to Ollama. Faults affect transport
// only: one 503 or a held response. It never fabricates embeddings or output.
type modelFaultProxy struct {
	server    *httptest.Server
	mode      atomic.Int32 // 0 forward, 1 fail once, 2 hold until client cancellation
	calls     atomic.Int32
	started   chan struct{}
	finished  chan struct{}
	cancelled chan struct{}
}

func newModelFaultProxy(t *testing.T, endpoint string) *modelFaultProxy {
	t.Helper()
	p := &modelFaultProxy{started: make(chan struct{}, 64), finished: make(chan struct{}, 64), cancelled: make(chan struct{}, 64)}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inference := r.URL.Path == "/api/embed" || r.URL.Path == "/api/chat"
		mode := p.mode.Load()
		if inference && p.mode.CompareAndSwap(1, 0) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if inference {
			p.calls.Add(1)
			p.started <- struct{}{}
		}
		req, err := http.NewRequestWithContext(r.Context(), r.Method, endpoint+r.URL.Path, r.Body)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		req.Header = r.Header.Clone()
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			if r.Context().Err() != nil {
				p.cancelled <- struct{}{}
			}
			w.WriteHeader(502)
			return
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(response.Body, tasks.MaxArtifactBytes+1))
		if err != nil {
			w.WriteHeader(502)
			return
		}
		if inference {
			p.finished <- struct{}{}
		}
		if inference && mode == 2 {
			<-r.Context().Done()
			p.cancelled <- struct{}{}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
	}))
	t.Cleanup(p.server.Close)
	return p
}

type realTaskHarness struct {
	api                    *httptest.Server
	js                     *postgres.JobStore
	ss                     *postgres.SchedulerStore
	artifacts              *tasks.PostgresArtifacts
	proxy                  *modelFaultProxy
	gateway, queue, tenant string
	traceCtx               context.Context
	traceSpan              trace.Span
}

func newRealTaskHarness(t *testing.T, endpoint string) *realTaskHarness {
	t.Helper()
	js := setupStore(t)
	ss, _ := setupSchedulerStore(t)
	h := &realTaskHarness{js: js, ss: ss, artifacts: tasks.NewPostgresArtifacts(testEnv.pool), proxy: newModelFaultProxy(t, endpoint), queue: "real-fault-" + uuid.NewString(), tenant: "real-" + uuid.NewString()}
	traceCtx, span := observability.Tracer("jobforge.acceptance").Start(t.Context(), "acceptance.lifecycle")
	h.traceCtx = traceCtx
	h.traceSpan = span
	t.Cleanup(func() { span.End() })
	cfg := &config.Config{APIKeys: map[string]string{"fault-key": h.tenant}}
	h.api = httptest.NewServer(apihttp.NewRouter(js, js, testTaskTypeCatalog(t), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil))
	t.Cleanup(h.api.Close)
	h.gateway = startTestWorkerGateway(t, 5*time.Second)
	return h
}

func (h *realTaskHarness) request(t *testing.T, path string, body any) map[string]any {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.api.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer fault-key")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(observability.TraceParentKey, observability.InjectTraceParent(h.traceCtx))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode >= 300 {
		t.Fatalf("HTTP %s: %d %v", path, resp.StatusCode, result)
	}
	return result
}

func (h *realTaskHarness) submit(t *testing.T, taskType, key string, timeout, maxAttempts int) string {
	t.Helper()
	body := map[string]any{"queue": h.queue, "type": taskType, "payload": json.RawMessage(taskPayload(taskType, key)), "timeout_seconds": timeout, "max_attempts": maxAttempts, "run_at": time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)}
	return h.request(t, "/v1/jobs", body)["job_id"].(string)
}

func (h *realTaskHarness) waitState(t *testing.T, id string, want domain.JobState) *domain.Job {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	var job *domain.Job
	for {
		select {
		case <-deadline.C:
			t.Fatalf("job %s did not reach %s; last=%+v", id, want, job)
		case <-tick.C:
			if _, err := h.ss.PromoteReady(t.Context(), 100); err != nil {
				t.Fatal(err)
			}
			var err error
			job, err = h.js.GetByID(t.Context(), h.tenant, id)
			if err != nil {
				t.Fatal(err)
			}
			if job.State == want {
				return job
			}
			if job.State.IsTerminal() && want != job.State {
				t.Fatalf("job %s reached %s instead of %s", id, job.State, want)
			}
		}
	}
}

func waitModelSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(65 * time.Second):
		t.Fatalf("missing model signal: %s", name)
	}
}

func (h *realTaskHarness) waitArtifact(t *testing.T, taskType, key string) *tasks.Artifact {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("artifact not published")
		case <-tick.C:
			a, err := h.artifacts.Lookup(t.Context(), h.tenant, taskType, key)
			if err == nil {
				return a
			}
			if !errors.Is(err, tasks.ErrNotFound) {
				t.Fatal(err)
			}
		}
	}
}

func (h *realTaskHarness) recoverNaturally(t *testing.T, id string) {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("expired lease was not recovered within 60 seconds")
		case <-tick.C:
			// No UPDATE of lease/run_at and no simulated expiry. This is the
			// production recovery transaction, driven by actual database time.
			rows, err := h.ss.RecoverExpiredAttempts(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var metrics *observability.Metrics
			for _, row := range rows {
				metrics.RecordRecovery(t.Context(), row)
			}
			job, err := h.js.GetByID(t.Context(), h.tenant, id)
			if err != nil {
				t.Fatal(err)
			}
			if job.State == domain.StateReady {
				return
			}
		}
	}
}

func (h *realTaskHarness) assertAttempt(t *testing.T, id string, attempt int, outcome, code string) {
	t.Helper()
	var gotOutcome string
	var gotCode *string
	if err := testEnv.pool.QueryRow(t.Context(), `select outcome, error_code from job_attempts where job_id = $1 and attempt_no = $2`, id, attempt).Scan(&gotOutcome, &gotCode); err != nil {
		t.Fatal(err)
	}
	if gotOutcome != outcome || (code != "" && (gotCode == nil || *gotCode != code)) {
		t.Fatalf("attempt outcome=%s code=%v", gotOutcome, gotCode)
	}
}

func (h *realTaskHarness) assertOneArtifact(t *testing.T, taskType, key string, first *tasks.Artifact) {
	t.Helper()
	a := h.waitArtifact(t, taskType, key)
	var count int
	if err := testEnv.pool.QueryRow(t.Context(), `select count(*) from task_artifacts where tenant_id=$1 and task_type=$2 and business_key=$3`, h.tenant, taskType, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("artifact publications=%d", count)
	}
	if first != nil && (a.ResultRef != first.ResultRef || !a.PublishedAt.Equal(first.PublishedAt) || !bytes.Equal(a.Body, first.Body)) {
		t.Fatal("first artifact changed")
	}
}

// TestRealTasksLifecycle deliberately injects transport failures around real
// model calls. Both jobs still use the production API, Gateway, lease, Runtime,
// adapters, business store and real process termination/reaping.
func TestRealTasksLifecycle(t *testing.T) {
	endpoint := realModelEndpoint(t)
	for _, taskType := range []string{"rag.index", "agent.extract"} {
		t.Run(taskType, func(t *testing.T) {
			for _, scenario := range []string{"retryable", "timeout", "cancel_manual_retry", "crash_before_publish", "crash_after_publish", "cancel_after_publish_manual_retry"} {
				t.Run(scenario, func(t *testing.T) {
					h := newRealTaskHarness(t, endpoint)
					t.Cleanup(func() {
						h.traceSpan.End()
						if t.Failed() {
							return
						}
						required := []string{"http.submit_job", "gateway.claim_jobs", "worker.execute", "business." + taskType}
						if scenario != "timeout" {
							required = append(required, "gateway.complete_job")
						}
						if strings.HasPrefix(scenario, "crash") {
							required = append(required, "scheduler.recover_lease")
						} else {
							required = append(required, "gateway.fail_job")
						}
						if strings.HasPrefix(scenario, "cancel") {
							required = append(required, "http.cancel_job", "http.retry_job")
						}
						assertTraceBackend(t, h.traceSpan.SpanContext().TraceID().String(), false, required...)
					})
					key := uuid.NewString()
					afterPublish := scenario == "crash_after_publish" || scenario == "cancel_after_publish_manual_retry"
					if scenario == "retryable" {
						h.proxy.mode.Store(1)
					}
					if scenario == "timeout" || scenario == "cancel_manual_retry" || scenario == "crash_before_publish" {
						h.proxy.mode.Store(2)
					}
					timeout, maxAttempts := 150, 3
					if scenario == "timeout" {
						timeout, maxAttempts = 1, 1
					}
					id := h.submit(t, taskType, key, timeout, maxAttempts)
					process := startBusinessWorker(t, h.gateway, h.queue, h.proxy.server.URL, afterPublish)
					if scenario == "retryable" {
						job := h.waitState(t, id, domain.StateSucceeded)
						if job.Attempt != 2 {
							t.Fatalf("attempt=%d", job.Attempt)
						}
						h.assertAttempt(t, id, 1, "failed_retry", "MODEL_UNAVAILABLE")
						h.assertOneArtifact(t, taskType, key, nil)
						process.stopAndWait(t)
						return
					}
					if scenario == "timeout" {
						h.waitState(t, id, domain.StateDead)
						h.assertAttempt(t, id, 1, "failed_dead", "TIMEOUT")
						if _, err := h.artifacts.Lookup(t.Context(), h.tenant, taskType, key); !errors.Is(err, tasks.ErrNotFound) {
							t.Fatalf("timeout published artifact: %v", err)
						}
						process.stopAndWait(t)
						return
					}
					var first *tasks.Artifact
					if afterPublish {
						first = h.waitArtifact(t, taskType, key)
					} else {
						waitModelSignal(t, h.proxy.started, "real inference started")
					}
					if strings.HasPrefix(scenario, "cancel") {
						h.request(t, "/v1/jobs/"+id+":cancel", nil)
						cancelled := h.waitState(t, id, domain.StateCancelled)
						if cancelled.ResultRef != nil {
							t.Fatal("cancel accepted result")
						}
						h.assertAttempt(t, id, 1, "cancelled", "CANCELLED")
						if !afterPublish {
							waitModelSignal(t, h.proxy.cancelled, "HTTP cancellation")
							if _, err := h.artifacts.Lookup(t.Context(), h.tenant, taskType, key); !errors.Is(err, tasks.ErrNotFound) {
								t.Fatalf("cancel published artifact: %v", err)
							}
						}
						process.stopAndWait(t)
						h.proxy.mode.Store(0)
						beforeCalls := h.proxy.calls.Load()
						clone := h.request(t, "/v1/jobs/"+id+":retry", nil)["job_id"].(string)
						if clone == id {
							t.Fatal("manual retry reused job ID")
						}
						workerB := startBusinessWorker(t, h.gateway, h.queue, h.proxy.server.URL, false)
						job := h.waitState(t, clone, domain.StateSucceeded)
						workerB.stopAndWait(t)
						if job.RetryOfJobID == nil || *job.RetryOfJobID != id {
							t.Fatal("retry lineage missing")
						}
						h.assertOneArtifact(t, taskType, key, first)
						if afterPublish && h.proxy.calls.Load() != beforeCalls {
							t.Fatal("manual retry recomputed published artifact")
						}
						return
					}
					if !afterPublish {
						waitModelSignal(t, h.proxy.finished, "real inference returned before crash")
					}
					claimed := h.waitState(t, id, domain.StateRunning)
					if claimed.ResultRef != nil {
						t.Fatal("result attached before Complete")
					}
					process.killAndWait(t)
					h.proxy.mode.Store(0)
					h.recoverNaturally(t, id)
					h.assertAttempt(t, id, 1, "lease_expired", "")
					beforeCalls := h.proxy.calls.Load()
					workerB := startBusinessWorker(t, h.gateway, h.queue, h.proxy.server.URL, false)
					job := h.waitState(t, id, domain.StateSucceeded)
					workerB.stopAndWait(t)
					if job.Attempt != 2 || job.FencingToken <= claimed.FencingToken {
						t.Fatal("recovery failed to advance attempt/token")
					}
					if _, err := h.js.CompleteAttempt(t.Context(), id, *claimed.LeaseOwner, claimed.FencingToken, "stale:result", 1); !errors.Is(err, domain.ErrStaleLease) {
						t.Fatalf("stale Complete=%v", err)
					}
					h.assertOneArtifact(t, taskType, key, first)
					if afterPublish && h.proxy.calls.Load() != beforeCalls {
						t.Fatal("recovery recomputed published artifact")
					}
					t.Logf("REAL %s: killed PID=%d, token %d -> %d, artifact reused=%t", scenario, process.cmd.Process.Pid, claimed.FencingToken, job.FencingToken, afterPublish)
				})
			}
		})
	}
}
