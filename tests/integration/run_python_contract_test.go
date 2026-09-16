package integration

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/xjfyrh/jobforge/internal/run/httpapi"
)

type runContractOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *runContractOutput) Write(data []byte) (int, error) {
	remaining := max(0, 4096-b.Len())
	if len(data) > remaining {
		b.overflow = true
	}
	_, _ = b.Buffer.Write(data[:min(len(data), remaining)])
	return len(data), nil
}

func TestRunPythonHTTPContract(t *testing.T) {
	python := os.Getenv("JOBFORGE_TEST_PYTHON")
	if python == "" {
		t.Skip("JOBFORGE_TEST_PYTHON unset: installed Run SDK real HTTP contract was not exercised")
	}
	h := setupRunHarness(t)
	providerBefore := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()); otel.SetTracerProvider(providerBefore) })
	router, err := httpapi.NewRouter(h.Service, map[string]httpapi.Identity{
		"contract-a":      {TenantID: "tenant-a", Role: "operator"},
		"contract-b":      {TenantID: "tenant-b", Role: "operator"},
		"contract-reader": {TenantID: "tenant-a", Role: "reader"},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(h.Ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, filepath.Join(root, "sdk", "python", "tests", "run_http_contract.py"),
		server.URL, h.Profile.ID, "contract-batch", "ticket-1")
	cmd.Dir, cmd.WaitDelay = root, 2*time.Second
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(name) {
		case "PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "HOME", "USERPROFILE", "LANG", "LC_ALL":
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "PYTHONIOENCODING=utf-8", "PYTHONDONTWRITEBYTECODE=1")
	output := &runContractOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run Python HTTP contract failed: %v\n%s", err, output.String())
	}
	if output.overflow || !strings.HasPrefix(output.String(), "PASS real HTTP Run SDK:") {
		t.Fatal("Run Python HTTP contract did not produce its bounded completion marker")
	}
	t.Log(output.String())
	seen := false
	for _, span := range exporter.GetSpans() {
		if span.Name == "run.http" && span.SpanContext.TraceID().String() == "11111111111111111111111111111111" {
			seen = true
		}
	}
	if !seen {
		t.Fatal("installed SDK parent did not reach the actual Run API span")
	}
}
