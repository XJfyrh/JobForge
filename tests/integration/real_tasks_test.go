package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/tasks"
	"github.com/xjfyrh/jobforge/internal/worker"
	"go.opentelemetry.io/otel"
)

const businessHelperModel = "JOBFORGE_TEST_BUSINESS_MODEL"
const businessHelperAfterPublish = "JOBFORGE_TEST_BUSINESS_AFTER_PUBLISH"

// Distinct secret markers cannot collide with traced business-<UUID> worker IDs.
const businessTestAPIKey = "acceptance-secret-business-tenant-a"
const businessTestForeignAPIKey = "acceptance-secret-business-tenant-b"

// TestBusinessWorkerProcessHelper is test-only fault wiring around real
// production handlers. No production payload or binary contains these hooks.
func TestBusinessWorkerProcessHelper(t *testing.T) {
	if os.Getenv(workerHelperFlag) != "1" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	if endpoint := os.Getenv("JOBFORGE_TEST_OTLP_ENDPOINT"); endpoint != "" {
		cfg := observability.DefaultConfig()
		cfg.ExporterType, cfg.ServiceName = "otlp", "jobforge-worker"
		shutdown, traceErr := observability.SetupTracing(ctx, cfg)
		if traceErr != nil {
			t.Fatal(traceErr)
		}
		defer func() {
			flush, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelFlush()
			_ = shutdown(flush)
		}()
	}
	pool, err := pgxpool.New(ctx, requiredHelperEnv(t, workerHelperDSN))
	if err != nil {
		t.Fatal("business helper database configuration invalid")
	}
	defer pool.Close()
	model, err := tasks.NewOllama(requiredHelperEnv(t, businessHelperModel), "")
	if err != nil {
		t.Fatal(err)
	}
	service := tasks.NewService(tasks.NewPostgresArtifacts(pool), model)
	registry := worker.NewRegistry()
	for _, taskType := range []string{"rag.index", "agent.extract"} {
		registry.Register(taskType, worker.HandlerFunc(func(ctx context.Context, job *worker.ClaimedJob) (string, error) {
			ref, err := service.Execute(ctx, job)
			if err == nil && os.Getenv(businessHelperAfterPublish) == "1" {
				<-ctx.Done()
				return "", ctx.Err()
			}
			return ref, err
		}))
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	runtime := worker.NewRuntime(worker.RuntimeConfig{WorkerID: requiredHelperEnv(t, workerHelperID), InstanceID: fmt.Sprintf("business-%d", os.Getpid()),
		Queues: []string{requiredHelperEnv(t, workerHelperQueue)}, Capacity: 1, GatewayAddr: requiredHelperEnv(t, workerHelperGateway),
		HeartbeatInterval: 200 * time.Millisecond, PollTimeout: time.Second, ShutdownGrace: time.Second, Version: "agent-rag-acceptance"}, registry, logger, nil)
	if err := runtime.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func startBusinessWorker(t *testing.T, gateway, queue, model string, afterPublish bool) *testWorkerProcess {
	t.Helper()
	p := &testWorkerProcess{done: make(chan struct{})}
	p.cmd = exec.Command(os.Args[0], "-test.run=^TestBusinessWorkerProcessHelper$", "-test.v=false")
	after := "0"
	if afterPublish {
		after = "1"
	}
	p.cmd.Env = helperEnvironment(map[string]string{workerHelperFlag: "1", workerHelperDSN: testEnv.dsn, workerHelperGateway: gateway,
		workerHelperQueue: queue, workerHelperID: "business-" + uuid.NewString(), businessHelperModel: model, businessHelperAfterPublish: after})
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	var err error
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { err := p.cmd.Wait(); p.mu.Lock(); p.waitErr = err; p.mu.Unlock(); close(p.done) }()
	t.Cleanup(func() { p.ensureStopped(t) })
	return p
}

func realModelEndpoint(t *testing.T) string {
	t.Helper()
	endpoint := os.Getenv("JOBFORGE_REAL_MODEL_URL")
	if endpoint == "" {
		t.Skip("real-model acceptance: set JOBFORGE_REAL_MODEL_URL to a pinned Ollama backend; substitutes are not acceptance")
	}
	if exportEndpoint := os.Getenv("JOBFORGE_TEST_OTLP_ENDPOINT"); exportEndpoint != "" {
		oldProvider, oldPropagator, oldErrors := otel.GetTracerProvider(), otel.GetTextMapPropagator(), otel.GetErrorHandler()
		t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", exportEndpoint)
		cfg := observability.DefaultConfig()
		cfg.ExporterType = "otlp"
		cfg.ServiceName = "jobforge-acceptance"
		shutdown, err := observability.SetupTracing(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdown(flush)
			otel.SetTracerProvider(oldProvider)
			otel.SetTextMapPropagator(oldPropagator)
			otel.SetErrorHandler(oldErrors)
		})
	}
	return endpoint
}

// TestRealTasksSDK executes both real models through SDK -> HTTP -> Gateway ->
// an actual Worker process, then reads and searches the persisted artifacts.
func TestRealTasksSDK(t *testing.T) {
	endpoint := realModelEndpoint(t)
	python := os.Getenv("JOBFORGE_TEST_PYTHON")
	if python == "" {
		t.Fatal("real SDK acceptance requires JOBFORGE_TEST_PYTHON")
	}
	js := setupStore(t)
	cfg := &config.Config{APIKeys: map[string]string{businessTestAPIKey: "business-tenant-a", businessTestForeignAPIKey: "business-tenant-b"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := httptest.NewServer(apihttp.NewRouter(js, js, testTaskTypeCatalog(t), cfg, logger, nil))
	defer api.Close()
	model, err := tasks.NewOllama(endpoint, "")
	if err != nil {
		t.Fatal(err)
	}
	artifacts := httptest.NewServer(tasks.NewArtifactRouter(tasks.NewPostgresArtifacts(testEnv.pool), model, cfg))
	defer artifacts.Close()
	queue := "real-sdk-" + uuid.NewString()
	process := startBusinessWorker(t, startTestWorkerGateway(t, 30*time.Second), queue, endpoint, false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, filepath.Join("..", "..", "examples", "agent_rag.py"), "--api-url", api.URL, "--artifact-url", artifacts.URL, "--queue", queue)
	cmd.Env = helperEnvironment(map[string]string{"JOBFORGE_API_KEY": businessTestAPIKey, "JOBFORGE_FOREIGN_API_KEY": businessTestForeignAPIKey})
	output, err := cmd.CombinedOutput()
	process.stopAndWait(t)
	if err != nil {
		t.Fatalf("real SDK acceptance: %v\n%s\nWorker: %s", err, output, process.output())
	}
	t.Logf("REAL MODEL SDK ACCEPTANCE\n%s", output)
	for _, line := range strings.Split(string(output), "\n") {
		if !strings.HasPrefix(line, "{\"task\"") {
			continue
		}
		var report struct {
			Task    string `json:"task"`
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal([]byte(line), &report); err != nil {
			t.Fatal(err)
		}
		assertTraceBackend(t, report.TraceID, true, "sdk.submit", "sdk.get", "http.submit_job", "gateway.claim_jobs", "worker.execute", "business."+report.Task, "gateway.complete_job", "business.artifact.get")
	}
}

func taskPayload(taskType, key string) []byte {
	p := tasks.Input{Version: 1, BusinessKey: key}
	if taskType == "rag.index" {
		p.CorpusVersion = "handbook-v1"
	} else {
		p.DocumentVersion, p.SchemaVersion = "purchase-order-v1", "purchase-order-v1"
	}
	data, _ := json.Marshal(p)
	return data
}
