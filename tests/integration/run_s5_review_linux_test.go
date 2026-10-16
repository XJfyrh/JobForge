//go:build linux

package integration

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/observability"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/businessclient"
	"github.com/xjfyrh/jobforge/internal/run/httpapi"
)

// Explicit external-review mode in the synthetic integration image only.
// Both databases, HTTP/SDK, production Go/Python and business writes are real;
// model decisions and vectors are labelled synthetic and incur no cloud fee.
func s5ReviewEnvironment(t *testing.T, h *runHarness, businessServer *httptest.Server, gateway string, businessPool *pgxpool.Pool, lose *atomic.Bool) {
	t.Helper()
	dir := os.Getenv("JOBFORGE_S5_REVIEW_DIR")
	if !filepath.IsAbs(dir) {
		t.Fatal("review directory must be an explicit external absolute path")
	}
	shutdown, err := observability.SetupRunTracing(t.Context(), "jobforge-agent-control-review")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(shutdown)
	keys := map[string]string{}
	identities := map[string]httpapi.Identity{}
	for role, identity := range map[string]httpapi.Identity{
		"operator":       {TenantID: "tenant-north", Role: "operator"},
		"reader":         {TenantID: "tenant-north", Role: "reader"},
		"approver":       {TenantID: "tenant-north", Role: "approver", ActorID: "s5-reviewer"},
		"other_approver": {TenantID: "tenant-north", Role: "approver", ActorID: "s5-other-reviewer"},
		"foreign_reader": {TenantID: "tenant-south", Role: "reader"},
	} {
		keys[role] = uuid.NewString()
		identities[keys[role]] = identity
	}
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
	router, err := httpapi.NewRouter(h.Service, identities)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "0.0.0.0:8093")
	if err != nil {
		t.Fatal(err)
	}
	ui := httptest.NewUnstartedServer(router)
	_ = ui.Listener.Close()
	ui.Listener = listener
	ui.Start()
	t.Cleanup(ui.Close)
	metricsListener, err := net.Listen("tcp", "0.0.0.0:6063")
	if err != nil {
		t.Fatal(err)
	}
	metrics := httptest.NewUnstartedServer(observability.RunMetricsHandler(h.Pool))
	_ = metrics.Listener.Close()
	metrics.Listener = metricsListener
	metrics.Start()
	t.Cleanup(metrics.Close)
	manifest := map[string]any{"schema_version": 1, "source_kind": "synthetic_mechanism_review", "cloud_calls": 0,
		"control_database": h.Pool.Config().ConnConfig.Database, "business_database": businessPool.Config().ConnConfig.Database,
		"profile_id": h.Profile.ID, "profile_hash": h.Profile.Hash, "batch_id": "contract-batch",
		"ui": "http://localhost:8193/ui/", "keys_file": "review-keys.json"}
	launcherJSON(t, filepath.Join(dir, "review-keys.json"), keys)
	launcherJSON(t, filepath.Join(dir, "environment.json"), manifest)
	worker := approvalStartFormalWorker(t, h, businessServer.URL, gateway, 0)
	command := exec.Command("/usr/local/bin/python", "-c", s5ReviewSDK, dir)
	command.Env = append(os.Environ(), "OTEL_SERVICE_NAME=jobforge-sdk-s5-review")
	raw, err := command.Output()
	if err != nil {
		t.Fatal("installed SDK review submissions failed")
	}
	var ids []string
	if json.Unmarshal(raw, &ids) != nil || len(ids) != 3 {
		t.Fatal("SDK submission IDs")
	}
	for _, id := range ids {
		recoveryEventually(t, 30*time.Second, func() bool {
			view, err := h.Store.Get(h.Ctx, "tenant-north", id)
			if err != nil {
				t.Fatal(err)
			}
			if view.State.Terminal() {
				t.Fatal("synthetic review proposal failed", view.State, view.Error)
			}
			return view.State == agentrun.AwaitingApproval
		})
	}
	manifest["runs"] = map[string]string{"approve": ids[0], "reject": ids[1], "version_conflict": ids[2]}
	launcherJSON(t, filepath.Join(dir, "environment.json"), manifest)
	launcherJSON(t, filepath.Join(dir, "review.ready"), map[string]any{"cloud_calls": 0, "runs": 3})
	t.Log("S5 free review ready; credentials and database names are only in the external review directory")
	// File commands belong only to this explicitly started disposable review.
	// No production registry, arbitrary endpoint or paid execution entry is added.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(90 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatal("review window expired")
		case <-ticker.C:
			if _, err := os.Stat(filepath.Join(dir, "lose-next-action-response")); err == nil {
				lose.Store(true)
				if os.Remove(filepath.Join(dir, "lose-next-action-response")) != nil {
					t.Fatal("review command acknowledgement")
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "pause")); err == nil {
				recoveryStopWorker(t, worker, false)
				ui.Close()
				businessServer.Close()
				metrics.Close()
				launcherJSON(t, filepath.Join(dir, "paused.ready"), map[string]any{"workers_stopped": true, "http_stopped": true})
				for {
					if _, err := os.Stat(filepath.Join(dir, "finish")); err == nil {
						return
					}
					select {
					case <-deadline.C:
						t.Fatal("paused review expired")
					case <-ticker.C:
					}
				}
			}
		}
	}
}

const s5ReviewSDK = `
import json, pathlib, sys
from jobforge import RunClient
from opentelemetry import trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.sdk.resources import Resource
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
root=pathlib.Path(sys.argv[1])
keys=json.loads((root/'review-keys.json').read_text())
manifest=json.loads((root/'environment.json').read_text())
provider=TracerProvider(resource=Resource.create({'service.name':'jobforge-sdk-s5-review'}))
provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(timeout=2),max_queue_size=256,max_export_batch_size=64))
trace.set_tracer_provider(provider)
ids=[]
with RunClient('http://control:8093',keys['operator']) as client:
 for role in ('approve','reject','version_conflict'):
  run=client.submit(ticket_id='ticket-1',business_request_key='s5-review-'+role,profile_id=manifest['profile_id'],budget_batch_id=manifest['batch_id'],idempotency_key='s5-review-'+role)
  ids.append(run.run.run_id)
provider.shutdown()
print(json.dumps(ids))
`
