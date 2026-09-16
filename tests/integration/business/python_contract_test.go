package business_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
)

// contractOutput drains the child output while retaining at most four KiB.
type contractOutput struct {
	bytes.Buffer
	overflow bool
}

// Write bounds retained diagnostics even if the fixed test child misbehaves.
func (b *contractOutput) Write(p []byte) (int, error) {
	remaining := 4096 - b.Len()
	if len(p) > remaining {
		b.overflow = true
	}
	_, _ = b.Buffer.Write(p[:min(len(p), remaining)])
	return len(p), nil
}

type contractStatusWriter struct {
	http.ResponseWriter
	status int
}

// WriteHeader observes the actual status sent to the Python HTTP client.
func (w *contractStatusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func TestBusinessPythonHTTPContract(t *testing.T) {
	python := os.Getenv("JOBFORGE_TEST_PYTHON")
	if python == "" {
		t.Skip("JOBFORGE_TEST_PYTHON is unset; real Python business HTTP contract not exercised")
	}
	ctx, _, loaderPool, runtimePool := isolatedPools(t)
	loader, runtime := business.NewStore(loaderPool), business.NewStore(runtimePool)
	dataset, upload := fixture()
	if err := loader.ImportDataset(ctx, dataset); err != nil {
		t.Fatalf("seed Python contract: %v", err)
	}
	// The real pgvector query uses fixture basis vectors, not model output.
	index, _, err := loader.PublishIndex(ctx, upload)
	if err != nil {
		t.Fatalf("publish synthetic Python contract index: %v", err)
	}
	snapshot, _, err := runtime.CreateSnapshot(ctx, "tenant-north", business.SnapshotRequest{
		SchemaVersion: 1, TicketID: "ticket-1", RequestKey: "python-http-contract",
	})
	if err != nil {
		t.Fatalf("create Python contract snapshot: %v", err)
	}
	const readerKey = "synthetic-contract-reader-north"
	const otherReaderKey = "synthetic-contract-reader-south"
	handler, err := business.NewHTTPHandler(runtime, map[string]business.Identity{
		readerKey:      {TenantID: "tenant-north", Role: "reader"},
		otherReaderKey: {TenantID: "tenant-south", Role: "reader"},
	})
	if err != nil {
		t.Fatalf("construct Python contract HTTP handler: %v", err)
	}
	var requests atomic.Int64
	var crossTenantStatus atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		statusWriter := &contractStatusWriter{ResponseWriter: w, status: http.StatusOK}
		handler.ServeHTTP(statusWriter, r)
		if r.Header.Get("Authorization") == "Bearer "+otherReaderKey {
			crossTenantStatus.Store(int64(statusWriter.status))
		}
	}))
	t.Cleanup(server.Close)
	input, err := json.Marshal(map[string]any{
		"schema_version":   1,
		"url":              server.URL,
		"reader_key":       readerKey,
		"other_reader_key": otherReaderKey,
		"binding": map[string]any{
			"snapshot_id":    snapshot.ID,
			"order_id":       snapshot.Ticket.OrderID,
			"profile_hash":   index.ProfileHash,
			"index_id":       index.ID,
			"policy_version": index.Profile.PolicyVersion,
		},
	})
	if err != nil || len(input) > 4096 {
		t.Fatal("Python contract configuration exceeds its fixed stdin frame")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal("cannot resolve Python contract source root")
	}
	childCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, python, filepath.Join(root, "python", "tests", "business_http_contract.py"))
	cmd.Dir = root
	cmd.WaitDelay = 2 * time.Second
	// Do not inherit database DSNs, model credentials, proxies or cloud settings.
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
	cmd.Env = append(cmd.Env, "PYTHONPATH="+filepath.Join(root, "python"),
		"PYTHONIOENCODING=utf-8", "PYTHONDONTWRITEBYTECODE=1")
	cmd.Stdin = bytes.NewReader(input)
	output := &contractOutput{}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		t.Fatalf("Python business HTTP contract failed: %v (bounded output bytes=%d)", err, output.Len())
	}
	if output.overflow {
		t.Fatal("Python business HTTP contract exceeded its output limit")
	}
	var result struct {
		Status          string `json:"status"`
		HTTPRequests    int    `json:"http_requests"`
		LocalRejections int    `json:"local_rejections"`
		ModelRequests   int    `json:"model_requests"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || result.Status != "passed" ||
		result.HTTPRequests != 4 || result.LocalRejections != 2 || result.ModelRequests != 0 {
		t.Fatal("Python business HTTP contract did not return the expected bounded result")
	}
	if requests.Load() != 4 {
		t.Fatalf("local rejection dispatched business HTTP: got %d requests, want 4", requests.Load())
	}
	if crossTenantStatus.Load() != http.StatusNotFound {
		t.Fatalf("cross-tenant HTTP status = %d, want 404", crossTenantStatus.Load())
	}
}
