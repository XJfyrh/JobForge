//go:build linux

package integration

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/run/businessclient"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
	"github.com/xjfyrh/jobforge/internal/runworker"
)

// Only provider decisions/vectors are synthetic. Ticket capture, business HTTP,
// resolution/receipt transaction, installed Python/Go and both databases are real.
func approvalBusinessPools(t *testing.T) (*pgxpool.Pool, map[string]*business.Store) {
	t.Helper()
	dsn := os.Getenv("JOBFORGE_BUSINESS_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated pgvector business database unset: S4 joint layer not exercised")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal("business test connection")
	}
	t.Cleanup(admin.Close)
	name := "jobforge_s4_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(t.Context(), "create database "+quoted); err != nil {
		t.Fatal("isolated S4 business database", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "drop database "+quoted+" with (force)"); err != nil {
			t.Error(err)
		}
	})
	pool := func(user string) *pgxpool.Pool {
		cfg := admin.Config().Copy()
		cfg.ConnConfig.Database = name
		if user != "" {
			cfg.ConnConfig.User = user
			cfg.ConnConfig.Password = user
		}
		p, err := pgxpool.NewWithConfig(t.Context(), cfg)
		if err != nil {
			t.Fatal("isolated role pool")
		}
		t.Cleanup(p.Close)
		return p
	}
	bootstrap := pool("")
	m := business.NewMigrator(bootstrap)
	if err := m.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	stores := map[string]*business.Store{}
	for alias, user := range map[string]string{"loader": "jobforge_business_loader_login", "reader": "jobforge_business_reader", "writer": "jobforge_business_writer_login", "receipt": "jobforge_business_receipt_reader"} {
		stores[alias] = business.NewStore(pool(user))
	}
	if stores["reader"].CheckRuntimeRole(t.Context()) != nil || stores["writer"].CheckActionRole(t.Context(), true) != nil || stores["receipt"].CheckActionRole(t.Context(), false) != nil {
		t.Fatal("joint business role separation")
	}
	return bootstrap, stores
}

func approvalStartFormalWorker(t *testing.T, h *runHarness, origin, gateway string, index int) *recoveryWorkerProcess {
	t.Helper()
	dir := t.TempDir()
	config, keys := filepath.Join(dir, "worker.json"), filepath.Join(dir, "keys.json")
	launcherJSON(t, config, map[string]any{"schema_version": 1, "profiles": []agentrun.Profile{h.Profile}, "tenants": map[string]any{"tenant-north": map[string]string{"business_origin": origin, "ollama_origin": "http://127.0.0.1:11434", "action_origin": origin}}})
	token := "executor-synthetic-control-token"
	if index == 1 {
		token = "recovery-synthetic-control-token-two"
	}
	launcherJSON(t, keys, map[string]any{"control_token": token, "tenants": map[string]any{"tenant-north": map[string]string{"business_read_key": "synthetic-business-read-key", "deepseek_api_key": "synthetic-provider-key", "action_reader_key": "synthetic-action-reader-key", "action_writer_key": "synthetic-action-writer-key"}}})
	command := exec.Command("/usr/local/bin/agent-worker")
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "JOBFORGE_AGENT_WORKER_CONFIG=" + config, "JOBFORGE_AGENT_WORKER_CREDENTIALS_FILE=" + keys, "JOBFORGE_AGENT_GATEWAY=" + gateway, "JOBFORGE_AGENT_GRPC_TLS=false"}
	if os.Getenv("JOBFORGE_S5_REVIEW_DIR") != "" {
		command.Env = append(command.Env, "JOBFORGE_OTEL_EXPORTER=otlp", "OTEL_EXPORTER_OTLP_ENDPOINT="+os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "JOBFORGE_METRICS_ADDR=0.0.0.0:6064")
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	worker := &recoveryWorkerProcess{cmd: command, done: make(chan struct{})}
	go func() { worker.err = command.Wait(); close(worker.done) }()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		select {
		case <-worker.done:
		case <-time.After(3 * time.Second):
			t.Error("S4 formal worker did not join")
		}
	})
	return worker
}

func approvalSyntheticProvider(t *testing.T, chats *atomic.Int32, repeat ...bool) {
	t.Helper()
	decisions := []string{
		`{"type":"tool","name":"get_order","arguments":{"order_id":"order-1"}}`,
		`{"type":"tool","name":"search_policy","arguments":{"query":"delivery timing"}}`,
		`{"type":"tool","name":"get_delivery","arguments":{"order_id":"order-1"}}`,
		`{"type":"tool","name":"search_policy","arguments":{"query":"late delivery action"}}`,
		`{"type":"final","proposal":{"decision":"proposal","action":"escalate","conclusion":"delayed","requested_fields":[],"target_ticket_status":"escalated","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["T#/observed_at","E1#/order/promised_delivery_at","P01.1"]}]}}`,
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/api/version" {
			_ = json.NewEncoder(w).Encode(map[string]string{"version": "0.32.5"})
			return
		}
		if req.URL.Path == "/api/tags" {
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]string{"name": business.EmbeddingModel, "digest": business.EmbeddingDigest}}})
			return
		}
		if req.URL.Path == "/api/embed" {
			v := make([]float64, 384)
			v[0] = 1
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "all-minilm:22m", "embeddings": [][]float64{v}, "prompt_eval_count": 3})
			return
		}
		n := int(chats.Add(1)) - 1
		if len(repeat) == 1 && repeat[0] {
			n %= len(decisions)
		}
		if req.URL.Path != "/chat/completions" || n >= len(decisions) || req.Header.Get("Authorization") != "Bearer synthetic-provider-key" {
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-s4-chat", "object": "chat.completion", "created": 1, "model": "deepseek-flash", "system_fingerprint": "fp_synthetic_s4", "choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "logprobs": nil, "message": map[string]any{"role": "assistant", "content": decisions[n]}}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "prompt_cache_hit_tokens": 2, "prompt_cache_miss_tokens": 8}})
	})
	for _, address := range []string{"127.0.0.1:11434", "127.0.0.1:18093"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(handler)
		_ = server.Listener.Close()
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
	}
}

func TestRunApprovalExecutorRealBusinessNaturalRecovery(t *testing.T) {
	if os.Getenv("JOBFORGE_RUNEXECUTOR_INTEGRATION_TESTS") != "1" {
		t.Skip("requires fixed Linux --init, real control PG, pgvector business PG and installed SDK")
	}
	modes := []string{"normal", "commit_loss"}
	if os.Getenv("JOBFORGE_S5_REVIEW_DIR") != "" {
		modes = []string{"review"}
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			h := setupApprovalHarness(t)
			h.Ctx = t.Context()
			admin, stores := approvalBusinessPools(t)
			d, err := agentrun.DecodeSupportDefinition(h.Profile.Definition)
			if err != nil {
				t.Fatal(err)
			}
			observed, _ := time.Parse(time.RFC3339, d.Resources.ObservedAt)
			oid, did := "order-1", "delivery-1"
			dataset := business.Dataset{SchemaVersion: 1, DatasetVersion: agentrun.SupportDatasetID}
			for _, tenant := range []string{"tenant-north", "tenant-south"} {
				dataset.Policies = append(dataset.Policies, business.PolicyVersion{TenantID: tenant, PolicyVersion: d.Resources.PolicyVersion, Revision: 2, CorpusSHA256: d.Resources.CorpusSHA256})
				dataset.Tickets = append(dataset.Tickets, business.Ticket{TenantID: tenant, TicketID: "ticket-1", Revision: 1, ObservedAt: observed, OrderID: &oid, PolicyVersion: d.Resources.PolicyVersion, Subject: "Synthetic joint fixture", Description: "Delivery timing", Status: "open"})
				dataset.Orders = append(dataset.Orders, business.Order{TenantID: tenant, OrderID: oid, Revision: 1, DeliveryID: &did, Status: "shipped", OrderedAt: observed.Add(-48 * time.Hour), PromisedDeliveryAt: observed.Add(-time.Hour)})
				dataset.Deliveries = append(dataset.Deliveries, business.Delivery{TenantID: tenant, DeliveryID: did, OrderID: oid, AggregateRevision: 1, Status: "delivered", Events: []business.DeliveryEvent{{EventID: "synthetic-delivered", OccurredAt: observed, Status: "delivered", Note: "Synthetic joint observation"}}})
			}
			if err := stores["loader"].ImportDataset(h.Ctx, dataset); err != nil {
				t.Fatal(err)
			}
			for i := range d.Resources.Tenants {
				upload := business.IndexUpload{SchemaVersion: 1, TenantID: d.Resources.Tenants[i].TenantID, Profile: d.Resources.IndexProfile}
				for j := 0; j < 3; j++ {
					v := make([]float64, 384)
					v[j] = 1
					upload.Chunks = append(upload.Chunks, business.IndexChunk{ChunkID: fmt.Sprintf("P%02d.1", j+1), Source: "synthetic#policy", Text: "Synthetic delivery timing rule for mechanism checks", Embedding: v})
				}
				index, _, err := stores["loader"].PublishIndex(h.Ctx, upload)
				if err != nil {
					t.Fatal(err)
				}
				d.Resources.Tenants[i].IndexID = index.ID
				d.Resources.Tenants[i].IndexContentHash = index.ContentHash
			}
			ordinary, err := business.NewHTTPHandler(stores["reader"], map[string]business.Identity{"synthetic-business-capture-key": {TenantID: "tenant-north", Role: "operator"}, "synthetic-business-read-key": {TenantID: "tenant-north", Role: "reader"}})
			if err != nil {
				t.Fatal(err)
			}
			key := ed25519.NewKeyFromSeed(make([]byte, 32))
			actions, err := business.NewActionHTTPHandler(stores["writer"], stores["receipt"], map[string]business.Identity{"synthetic-action-writer-key": {TenantID: "tenant-north", Role: "action_writer"}, "synthetic-action-reader-key": {TenantID: "tenant-north", Role: "action_reader"}}, map[string]business.TrustedActionKey{d.Action.KeyID: {PublicKey: key.Public().(ed25519.PublicKey), Tenants: map[string]bool{"tenant-north": true}}})
			if err != nil {
				t.Fatal(err)
			}
			var writes atomic.Int32
			var queries atomic.Int32
			var lose atomic.Bool
			lose.Store(mode == "commit_loss")
			committed := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if !strings.HasPrefix(req.URL.Path, "/business/v1/actions/") {
					ordinary.ServeHTTP(w, req)
					return
				}
				if req.Method == http.MethodGet {
					queries.Add(1)
				}
				if req.Method == http.MethodPost {
					writes.Add(1)
					if lose.CompareAndSwap(true, false) {
						record := httptest.NewRecorder()
						actions.ServeHTTP(record, req)
						if record.Code == 200 {
							close(committed)
							<-req.Context().Done()
							return
						}
						for k, v := range record.Header() {
							w.Header()[k] = v
						}
						w.WriteHeader(record.Code)
						_, _ = w.Write(record.Body.Bytes())
						return
					}
				}
				actions.ServeHTTP(w, req)
			}))
			t.Cleanup(server.Close)
			d.Action.Origin = server.URL
			h.Profile, err = agentrun.BuildSupportProfile("approval-joint-synthetic-v1", d)
			if err != nil {
				t.Fatal(err)
			}
			h.Profile.Executable = true
			h.Options.Profiles = []agentrun.Profile{h.Profile}
			for i := range h.Options.Workers {
				h.Options.Workers[i].ProfileIDs = []string{h.Profile.ID}
			}
			h.Store, err = runpostgres.New(h.Pool, h.Options)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.Store.EnsureProfiles(h.Ctx); err != nil {
				t.Fatal(err)
			}
			capture, err := businessclient.New(server.URL, map[string]string{"tenant-north": "synthetic-business-capture-key"})
			if err != nil {
				t.Fatal(err)
			}
			h.Service, err = agentrun.NewService(h.Store, capture, []string{"tenant-north", "tenant-south"})
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := "/etc/jobforge/executor.json"
			original, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			launcherJSON(t, manifestPath, runworker.Manifest{SchemaVersion: 1, ExecutorVersion: h.Profile.ExecutorVersion, Profiles: []runworker.ManifestProfile{{ProfileID: h.Profile.ID, ProfileHash: h.Profile.Hash, AdapterID: "support-agent-v1"}}})
			t.Cleanup(func() { _ = os.WriteFile(manifestPath, original, 0600) })
			var chats atomic.Int32
			approvalSyntheticProvider(t, &chats, mode == "review")
			gateway, _ := recoveryExecutorGateway(t, h, "")
			if mode == "review" {
				s5ReviewEnvironment(t, h, server, gateway, admin, &lose)
				return
			}
			r := h.submit(t, "tenant-north", "joint-"+mode)
			worker := approvalStartFormalWorker(t, h, server.URL, gateway, 0)
			recoveryEventually(t, 20*time.Second, func() bool {
				view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if view.State.Terminal() {
					t.Fatalf("formal proposal failed: state=%s error=%v cursor=%d synthetic_chats=%d", view.State, view.Error, view.CursorVersion, chats.Load())
				}
				return view.State == agentrun.AwaitingApproval
			})
			prefix, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 32)
			if err != nil || len(prefix.Items) != 11 || chats.Load() != 5 {
				t.Fatal("formal Go/Python prefix", err)
			}
			approveRun(t, h, r, "approve")
			if mode == "commit_loss" {
				recoveryWaitSignal(t, committed)
				old := recoveryExecution(t, h, r)
				checkpoint, err := h.Store.GetCheckpoint(h.Ctx, old.WorkerID, old)
				if err != nil {
					t.Fatal("current action checkpoint", err)
				}
				before, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				recoveryStopWorker(t, worker, true)
				// Real commit + actual process death; never edit production clocks/state.
				var count, revision int
				if err := admin.QueryRow(h.Ctx, `select revision,(select count(*) from business.ticket_resolutions) from business.tickets where tenant_id='tenant-north' and ticket_id='ticket-1'`).Scan(&revision, &count); err != nil || revision != 2 || count != 1 {
					t.Fatal("business commit did not happen", err)
				}
				if _, err := h.Pool.Exec(h.Ctx, `update budget_accounts set frozen=true`); err != nil {
					t.Fatal(err)
				}
				h.Options.Profiles[0].Executable = false
				h.Store, err = runpostgres.New(h.Pool, h.Options)
				if err != nil {
					t.Fatal(err)
				}
				recoveryEventually(t, 35*time.Second, func() bool {
					var expired bool
					if err := h.Pool.QueryRow(h.Ctx, `select lease_until<=clock_timestamp() from runs where run_id=$1`, r.ID).Scan(&expired); err != nil {
						t.Fatal(err)
					}
					return expired
				})
				oldStep := currentRunStep(agentrun.ClaimedRun{Checkpoint: checkpoint})
				if _, err := h.Store.GetAction(h.Ctx, old.WorkerID, old, oldStep); !errors.Is(err, agentrun.ErrStaleLease) {
					t.Fatal("old action owner", err)
				}
				ready := recoveryReady(t, h, r.ID, 6*time.Second)
				if ready.RecoveryCount != 1 || time.Since(started) < 25*time.Second {
					t.Fatal("natural action lease/backoff bypass")
				}
				// Bind a new gateway to the disabled-profile store rather than
				// replacing a field while old gRPC handlers may still be returning.
				gateway, _ = recoveryExecutorGateway(t, h, "")
				worker = approvalStartFormalWorker(t, h, server.URL, gateway, 1)
				recoveryEventually(t, 15*time.Second, func() bool {
					view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
					if err != nil {
						t.Fatal(err)
					}
					return view.State.Terminal()
				})
				after, _ := h.Store.Get(h.Ctx, r.TenantID, r.ID)
				if !reflect.DeepEqual(before.Budget.Family.Used, after.Budget.Family.Used) || after.AttemptNo != 3 || after.RecoveryCount != 1 {
					t.Fatal("recovery changed model budget")
				}
				t.Logf("natural action recovery: elapsed_ms=%d, attempts=3, receipt_queries=2, new_model_calls=0", time.Since(started).Milliseconds())
			}
			recoveryEventually(t, 15*time.Second, func() bool {
				view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				return view.State.Terminal()
			})
			recoveryStopWorker(t, worker, false)
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil || view.State != agentrun.Succeeded || view.Outcome == nil || *view.Outcome != "applied" {
				t.Fatalf("action completion: state=%s error=%v", view.State, err)
			}
			steps, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 32)
			if err != nil || len(steps.Items) != 12 {
				t.Fatal("action checkpoint", err)
			}
			for i := range prefix.Items {
				if !reflect.DeepEqual(prefix.Items[i], steps.Items[i]) {
					t.Fatal("accepted model prefix changed")
				}
			}
			if writes.Load() != 1 || chats.Load() != 5 || queries.Load() != 1+int32(view.RecoveryCount) {
				t.Fatal("unexpected action/model resends", writes.Load(), queries.Load(), chats.Load())
			}
			effect, err := h.Store.Effect(h.Ctx, r.TenantID, r.ID)
			if err != nil || effect.State != "applied" {
				t.Fatal("actual business effect", err)
			}
			stored, err := stores["receipt"].Receipt(h.Ctx, r.TenantID, *effect.OperationID)
			if err != nil || stored.ReceiptHash != effect.Receipt.ReceiptHash {
				t.Fatal("control receipt differs from real business commit", err)
			}
			t.Logf("real business transaction + formal Go/Python (%s): writes=1, synthetic chats=5, recoveries=%d", mode, view.RecoveryCount)
		})
	}
}
