package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/grpcapi"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
	"github.com/xjfyrh/jobforge/internal/runexecutor"
	"github.com/xjfyrh/jobforge/internal/runinput"
	"github.com/xjfyrh/jobforge/internal/runworker"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

const executorProfileID = "c3b-runtime-profile-audit-v1"

func setupExecutorHarness(t *testing.T) *runHarness {
	t.Helper()
	if runtime.GOOS != "linux" || os.Getenv("JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS") != "1" {
		t.Skip("requires fixed Linux integration image, init, real PostgreSQL and JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS=1; skip is not acceptance")
	}
	h := setupRunHarness(t)
	h.Profile.ID, h.Profile.Hash = executorProfileID, agentrun.Fingerprint(executorProfileID)
	h.Profile.ExecutorVersion = runinput.ExecutorVersion
	h.Profile.ProviderAuditPolicy = agentrun.ProviderAuditPolicyDeepSeekV1
	h.Profile.ExpectedResponseModel = "deepseek-flash"
	h.Profile.MaxOutputTokens = 1024
	h.Principal = "c3b-runtime-worker"
	h.Options = runpostgres.Options{Profiles: []agentrun.Profile{h.Profile}, Workers: []agentrun.WorkerConfig{{ID: h.Principal, Tenants: []string{"tenant-a"}, ProfileIDs: []string{h.Profile.ID}, Capacity: 1}}, TenantCapacity: 1, ProfileCapacity: 1}
	var err error
	h.Store, err = runpostgres.New(h.Pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Store.EnsureProfiles(h.Ctx); err != nil {
		t.Fatal(err)
	}
	h.Service, err = agentrun.NewService(h.Store, h.Capture, []string{"tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

type executorRPCFaults struct {
	agentv1.AgentServiceClient
	target          string
	observe         func(context.Context, *agentv1.ObserveCallRequest, ...grpc.CallOption) (*agentv1.ObserveCallResponse, error)
	commit          func(context.Context, *agentv1.CommitStepRequest, ...grpc.CallOption) (*agentv1.CommitStepResponse, error)
	settle          func(context.Context, *agentv1.SettleUsageRequest, ...grpc.CallOption) (*agentv1.SettleUsageResponse, error)
	reserve         func(context.Context, *agentv1.ReserveCallRequest, ...grpc.CallOption) (*agentv1.ReserveCallResponse, error)
	confirmed       chan struct{}
	confirmOnce     sync.Once
	failed          atomic.Int64
	observeAttempts atomic.Int64
	commitAttempts  atomic.Int64
	claimAttempts   atomic.Int64
	looped          chan struct{}
}

func (c *executorRPCFaults) ReserveCall(ctx context.Context, req *agentv1.ReserveCallRequest, opts ...grpc.CallOption) (*agentv1.ReserveCallResponse, error) {
	if c.reserve != nil {
		return c.reserve(ctx, req, opts...)
	}
	return c.AgentServiceClient.ReserveCall(ctx, req, opts...)
}

func (c *executorRPCFaults) SettleUsage(ctx context.Context, req *agentv1.SettleUsageRequest, opts ...grpc.CallOption) (*agentv1.SettleUsageResponse, error) {
	if c.settle != nil {
		return c.settle(ctx, req, opts...)
	}
	return c.AgentServiceClient.SettleUsage(ctx, req, opts...)
}

func (c *executorRPCFaults) Claim(ctx context.Context, req *agentv1.ClaimRequest, opts ...grpc.CallOption) (*agentv1.ClaimResponse, error) {
	if c.claimAttempts.Add(1) == 2 {
		close(c.looped)
	}
	return c.AgentServiceClient.Claim(ctx, req, opts...)
}

func (c *executorRPCFaults) ObserveCall(ctx context.Context, req *agentv1.ObserveCallRequest, opts ...grpc.CallOption) (*agentv1.ObserveCallResponse, error) {
	c.observeAttempts.Add(1)
	if c.observe != nil {
		return c.observe(ctx, req, opts...)
	}
	return c.AgentServiceClient.ObserveCall(ctx, req, opts...)
}

func (c *executorRPCFaults) CommitStep(ctx context.Context, req *agentv1.CommitStepRequest, opts ...grpc.CallOption) (*agentv1.CommitStepResponse, error) {
	c.commitAttempts.Add(1)
	if c.commit != nil {
		return c.commit(ctx, req, opts...)
	}
	return c.AgentServiceClient.CommitStep(ctx, req, opts...)
}

func (c *executorRPCFaults) FailAttempt(ctx context.Context, req *agentv1.FailAttemptRequest, opts ...grpc.CallOption) (*agentv1.FailAttemptResponse, error) {
	c.failed.Add(1)
	return c.AgentServiceClient.FailAttempt(ctx, req, opts...)
}

func (c *executorRPCFaults) GetAcceptedCommit(ctx context.Context, req *agentv1.GetAcceptedCommitRequest, opts ...grpc.CallOption) (*agentv1.GetAcceptedCommitResponse, error) {
	response, err := c.AgentServiceClient.GetAcceptedCommit(ctx, req, opts...)
	if err == nil && response.Found {
		c.confirmOnce.Do(func() { close(c.confirmed) })
	}
	return response, err
}

func executorGateway(t *testing.T, h *runHarness) *executorRPCFaults {
	t.Helper()
	server, err := grpcapi.NewServer(h.Store, grpcapi.Config{Workers: h.Options.Workers, Credentials: map[string]string{h.Principal: "executor-synthetic-control-token"}})
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
			t.Errorf("gateway cleanup: %v", err)
		}
	})
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return &executorRPCFaults{AgentServiceClient: agentv1.NewAgentServiceClient(connection), target: listener.Addr().String(), confirmed: make(chan struct{}), looped: make(chan struct{})}
}

type executorHTTPFixture struct {
	mu              sync.Mutex
	counts          map[string]int
	correction      bool
	responseModel   string
	omitUsage       bool
	reasoningTokens int64
	rejectEveryChat bool
	blockOrder      bool
	blockFirstChat  atomic.Bool
	anomalyInput    atomic.Int64
	orderSeen       chan struct{}
	orderOnce       sync.Once
	chatSeen        chan struct{}
	chatOnce        sync.Once
	chatRelease     chan struct{}
	BusinessOrigin  string
	snapshot        agentrun.SnapshotBinding
	badRequest      atomic.Bool
}

func (f *executorHTTPFixture) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[path]
}

func executorHTTP(t *testing.T, h *runHarness, r agentrun.Run, correction, blockOrder bool) *executorHTTPFixture {
	t.Helper()
	f := &executorHTTPFixture{counts: make(map[string]int), correction: correction, blockOrder: blockOrder, orderSeen: make(chan struct{}), chatSeen: make(chan struct{}), chatRelease: make(chan struct{})}
	h.Capture.mu.Lock()
	for _, snapshot := range h.Capture.snapshots {
		if snapshot.ID == r.SnapshotID {
			f.snapshot = snapshot
		}
	}
	h.Capture.mu.Unlock()
	if f.snapshot.ID == "" {
		t.Fatal("admitted snapshot missing from synthetic capture")
	}
	business := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(business.Close)
	f.BusinessOrigin = business.URL
	for _, address := range []string{"127.0.0.1:11434", "127.0.0.1:18093"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
		_ = server.Listener.Close()
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
	}
	return f
}

func (f *executorHTTPFixture) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasSuffix(path, "/order") {
		path = "order"
	}
	if strings.HasSuffix(path, "/delivery") {
		path = "delivery"
	}
	if strings.HasSuffix(path, "/policies/search") {
		path = "search"
	}
	f.mu.Lock()
	f.counts[path]++
	number := f.counts[path]
	f.mu.Unlock()
	var response any
	switch path {
	case "order", "delivery":
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-business-read-key" || !strings.Contains(r.URL.Path, f.snapshot.ID) {
			f.badRequest.Store(true)
			w.WriteHeader(400)
			return
		}
		if path == "order" {
			f.orderOnce.Do(func() { close(f.orderSeen) })
			if f.blockOrder {
				<-r.Context().Done()
				return
			}
		}
		response = map[string]any{"snapshot_id": f.snapshot.ID, "evidence_ref": "business-evidence:" + f.snapshot.ID + ":" + path, "kind": path, "missing": false, path: map[string]any{"order_id": "order-1", "delivery_id": "delivery-1"}}
	case "search":
		var body struct {
			Model  string    `json:"embedding_model"`
			Digest string    `json:"embedding_digest"`
			Vector []float64 `json:"query_vector"`
		}
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer synthetic-business-read-key" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || len(body.Vector) != 384 || body.Model != "all-minilm:22m" {
			f.badRequest.Store(true)
			w.WriteHeader(400)
			return
		}
		var ticket struct {
			PolicyVersion string `json:"policy_version"`
		}
		_ = json.Unmarshal(f.snapshot.Ticket, &ticket)
		response = map[string]any{"snapshot_id": f.snapshot.ID, "matches": []any{map[string]any{"index_id": f.snapshot.IndexID, "chunk_id": "P01.1", "policy_version": ticket.PolicyVersion, "evidence_ref": "business-policy:" + f.snapshot.IndexID + ":P01.1", "text": "Synthetic mechanism policy", "source": "fixture", "distance": 0.1}}}
	case "/api/version":
		response = map[string]any{"version": "0.32.5"}
	case "/api/tags":
		response = map[string]any{"models": []any{map[string]any{"name": "all-minilm:22m", "digest": "1b226e2802dbb772b5fc32a58f103ca1804ef7501331012de126ab22f67475ef"}}}
	case "/api/embed":
		var body struct {
			Model    string   `json:"model"`
			Input    []string `json:"input"`
			Truncate bool     `json:"truncate"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || body.Model != "all-minilm:22m" || len(body.Input) != 1 || body.Input[0] != "delivery policy" || body.Truncate {
			f.badRequest.Store(true)
			w.WriteHeader(400)
			return
		}
		vector := make([]float64, 384)
		vector[0] = 1
		response = map[string]any{"model": "all-minilm:22m", "embeddings": [][]float64{vector}, "prompt_eval_count": 3}
	case "/chat/completions":
		var body map[string]any
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer synthetic-provider-key" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&body) != nil || body["model"] != "deepseek-flash" || body["stream"] != false || body["max_tokens"] != float64(1024) {
			f.badRequest.Store(true)
			w.WriteHeader(400)
			return
		}
		f.chatOnce.Do(func() { close(f.chatSeen) })
		if f.blockFirstChat.Load() && number == 1 {
			<-r.Context().Done()
			return
		}
		inputTokens := int64(10)
		if anomalous := f.anomalyInput.Load(); anomalous > 0 {
			select {
			case <-f.chatRelease:
			case <-r.Context().Done():
				return
			}
			inputTokens = anomalous
		}
		content, _ := json.Marshal(map[string]any{"decision": "no_action", "summary": "Synthetic mechanism result", "evidence_refs": []string{"business-evidence:" + f.snapshot.ID + ":ticket", "business-policy:" + f.snapshot.IndexID + ":P01.1"}, "action": ""})
		if f.rejectEveryChat || (f.correction && number == 1) {
			content = []byte(`{"invalid":"fixture"}`)
		}
		model := f.responseModel
		if model == "" {
			model = "deepseek-flash"
		}
		chat := map[string]any{"id": "synthetic-chat", "object": "chat.completion", "created": 1, "model": model, "system_fingerprint": "fp_synthetic", "choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "logprobs": nil, "message": map[string]any{"role": "assistant", "content": string(content)}}}}
		if !f.omitUsage {
			usage := map[string]any{"prompt_tokens": inputTokens, "completion_tokens": 5, "total_tokens": inputTokens + 5, "prompt_cache_hit_tokens": 2, "prompt_cache_miss_tokens": inputTokens - 2}
			if f.reasoningTokens > 0 {
				usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": f.reasoningTokens}
			}
			chat["usage"] = usage
		}
		response = chat
	default:
		f.badRequest.Store(true)
		w.WriteHeader(404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

type executorWorkerRun struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

func startExecutorWorker(t *testing.T, h *runHarness, f *executorHTTPFixture, client agentv1.AgentServiceClient, expectedErrors ...error) *executorWorkerRun {
	t.Helper()
	manifest := executorManifest(t, h.Profile)
	w, err := runworker.New(client, manifest, runworker.Config{Profiles: []agentrun.Profile{h.Profile}, Environments: map[string]runexecutor.Environment{h.Options.Workers[0].Tenants[0]: {BusinessOrigin: f.BusinessOrigin, BusinessReadKey: "synthetic-business-read-key", OllamaOrigin: "http://127.0.0.1:11434", DeepSeekKey: "synthetic-provider-key"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(h.Ctx, 30*time.Second)
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer executor-synthetic-control-token")
	running := &executorWorkerRun{cancel: cancel, done: make(chan struct{})}
	go func() { running.err = w.Run(ctx); close(running.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-running.done:
			accepted := running.err == nil || errors.Is(running.err, context.Canceled)
			for _, expected := range expectedErrors {
				accepted = accepted || errors.Is(running.err, expected)
			}
			if !accepted {
				t.Errorf("worker cleanup: %v", running.err)
			}
		case <-time.After(5 * time.Second):
			t.Error("worker did not join after cancellation")
		}
	})
	return running
}

// executorManifest selects one immutable entry from the test image's fixed
// manifest. Python independently checks that same installed entry; this cannot
// register an adapter, change its mapping or introduce a runtime module path.
func executorManifest(t *testing.T, profile agentrun.Profile) runworker.Manifest {
	t.Helper()
	manifest, err := runworker.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	for _, selected := range manifest.Profiles {
		if selected.ProfileID == profile.ID && selected.ProfileHash == profile.Hash {
			manifest.Profiles = []runworker.ManifestProfile{selected}
			return manifest
		}
	}
	t.Fatal("profile missing from installed synthetic manifest")
	return runworker.Manifest{}
}

func waitExecutorSignal(t *testing.T, running *executorWorkerRun, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-running.done:
		t.Fatalf("worker exited before required fact: %v", running.err)
	case <-time.After(20 * time.Second):
		stacks := make([]byte, 128<<10)
		stacks = stacks[:runtime.Stack(stacks, true)]
		t.Fatalf("executor signal not observed; goroutine stacks (no payload):\n%s", stacks)
	}
}
