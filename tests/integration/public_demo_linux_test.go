//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/businessclient"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
)

// This entry is compiled only into the explicitly synthetic integration image.
// Admission, storage, business HTTP/writes and Python processes remain real;
// vectors and model responses are deterministic loopback fixtures.
func publicDemoEnvironment(t *testing.T, h *runHarness, businessServer *httptest.Server, gateway string) {
	t.Helper()
	reader, err := businessclient.NewActionClient(map[string]businessclient.ActionCredentials{"tenant-north": {Origin: businessServer.URL, ReaderKey: "synthetic-action-reader-key"}})
	if err != nil {
		t.Fatal(err)
	}
	capture, err := businessclient.New(businessServer.URL, map[string]string{"tenant-north": "synthetic-business-capture-key"})
	if err != nil {
		t.Fatal(err)
	}
	h.Service, err = agentrun.NewService(h.Store, capture, []string{"tenant-north", "tenant-south"}, agentrun.WithReceiptReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	router, err := httpapi.NewRouter(h.Service, map[string]httpapi.Identity{
		"demo-operator": {TenantID: "tenant-north", Role: "operator"},
		"demo-reader":   {TenantID: "tenant-north", Role: "reader"},
		"demo-approver": {TenantID: "tenant-north", Role: "approver", ActorID: "demo-reviewer"},
	}, httpapi.UIOption{TenantID: "tenant-north", Name: "免费机制演示（模型响应与向量为模拟）", ProfileID: h.Profile.ID, BudgetBatchID: "contract-batch"})
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]any{"schema_version": 1, "source_kind": "synthetic_mechanism_demo", "cloud_calls": 0,
		"profile_id": h.Profile.ID, "budget_batch_id": "contract-batch", "ticket_id": "ticket-1",
		"notice": "Model responses and vectors are simulated. Run, approval and business receipts use real Go/PostgreSQL/Python paths. Ledger amounts are synthetic."}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/demo.json" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(metadata)
			return
		}
		if r.URL.Path == "/ui/" {
			recorded := httptest.NewRecorder()
			router.ServeHTTP(recorded, r)
			for key, values := range recorded.Header() {
				w.Header()[key] = values
			}
			w.Header().Del("Content-Length")
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(bytes.Replace(recorded.Body.Bytes(), []byte("<body>"), []byte("<body><aside>免费机制演示 · 模型响应、向量及费用为模拟 · 工单、审批和回执使用隔离的真实数据库</aside>"), 1))
			return
		}
		router.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp", "0.0.0.0:8093")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	_ = server.Listener.Close()
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	approvalStartFormalWorker(t, h, businessServer.URL, gateway, 0)
	t.Log("Free public demo ready; only deterministic loopback model responses are enabled")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	timer := time.NewTimer(90 * time.Minute)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
		t.Log("Demo window ended after 90 minutes")
	}
}
