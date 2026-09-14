package integration

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	apihttp "github.com/xjfyrh/jobforge/internal/api/http"
	"github.com/xjfyrh/jobforge/internal/config"
	"github.com/xjfyrh/jobforge/internal/worker"
	"github.com/xjfyrh/jobforge/internal/worker/demo"
)

// TestPythonHTTPContract uses a separately installed Python SDK over actual
// TCP HTTP, a gRPC Gateway, registered Go Handlers and real PostgreSQL. No HTTP
// response fixture participates; CI explicitly supplies the interpreter.
func TestPythonHTTPContract(t *testing.T) {
	python := os.Getenv("JOBFORGE_TEST_PYTHON")
	if python == "" {
		t.Skip("set JOBFORGE_TEST_PYTHON to an interpreter with sdk/python installed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	oldProvider, oldPropagator := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(oldProvider)
		otel.SetTextMapPropagator(oldPropagator)
	})
	queue := "python-contract-" + uuid.NewString()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	js := setupStore(t)
	cfg := &config.Config{APIKeys: map[string]string{"contract-a": "contract-tenant-a", "contract-b": "contract-tenant-b"}, QueueHardLimit: 2}
	server := httptest.NewServer(apihttp.NewRouter(js, js, testTaskTypeCatalog(t), cfg, logger, nil))
	defer server.Close()
	registry := worker.NewRegistry()
	registry.Register("demo.echo", &demo.EchoHandler{})
	registry.Register("demo.sleep", &demo.SleepHandler{})
	registry.Register("demo.fail", &demo.FailHandler{})
	runtime := worker.NewRuntime(worker.RuntimeConfig{
		WorkerID: "python-contract-worker", InstanceID: "contract", Queues: []string{queue},
		Capacity: 2, GatewayAddr: startTestWorkerGateway(t, time.Minute),
		PollTimeout: time.Second, ShutdownGrace: 3 * time.Second,
	}, registry, logger, nil)
	runCapacityRuntime(t, runtime)
	cmd := exec.CommandContext(ctx, python, filepath.Join("..", "..", "sdk", "python", "tests", "http_contract.py"), server.URL, queue)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python HTTP contract: %v\n%s", err, output)
	}
	t.Logf("%s", output)
	// A real Python SDK parent must survive HTTP -> persisted job -> Worker.
	seen := map[string]bool{}
	for _, span := range exporter.GetSpans() {
		if span.SpanContext.TraceID().String() == "11111111111111111111111111111111" {
			seen[span.Name] = true
		}
	}
	for _, name := range []string{"http.submit_job", "gateway.claim_jobs", "worker.execute", "gateway.complete_job"} {
		if !seen[name] {
			t.Errorf("Python context missing from %s", name)
		}
	}
}
