//go:build linux

package integration

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

const supportExecutorProfileID = "support-executor-synthetic-audit-v1"

func supportExecutorHarness(t *testing.T, key string, withOrder bool) *runHarness {
	t.Helper()
	h := setupExecutorHarness(t)
	h.Profile.ID, h.Profile.Hash = supportExecutorProfileID, agentrun.Fingerprint(supportExecutorProfileID)
	h.Profile.Strategy = agentrun.SupportFixedStrategy
	h.Profile.Definition = json.RawMessage(`{"fixture":true,"proposal_schema":"support-proposal-v1"}`)
	h.Options.Profiles = []agentrun.Profile{h.Profile}
	h.Options.Workers[0].ProfileIDs = []string{h.Profile.ID}
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
	if !withOrder {
		captureKey := agentrun.SnapshotKey("tenant-a", "submit", "submit-"+key, "")
		snapshot, err := h.Capture.Capture(h.Ctx, "tenant-a", "ticket-1", captureKey)
		if err != nil {
			t.Fatal(err)
		}
		var ticket business.Ticket
		var vector business.VersionVector
		if json.Unmarshal(snapshot.Ticket, &ticket) != nil || json.Unmarshal(snapshot.VersionVector, &vector) != nil {
			t.Fatal("invalid synthetic captured source")
		}
		ticket.OrderID = nil
		vector.Order, vector.Delivery = business.OrderVersion{}, business.DeliveryVersion{}
		snapshot.Ticket, snapshot.VersionVector = supportStorageJSON(t, ticket), supportStorageJSON(t, vector)
		snapshot.ContentHash = agentrun.Fingerprint("support-executor-missing-order", string(snapshot.Ticket), string(snapshot.VersionVector))
		h.Capture.mu.Lock()
		h.Capture.snapshots["tenant-a:"+captureKey] = snapshot
		h.Capture.mu.Unlock()
	}
	return h
}

type supportExecutorHTTPFixture struct {
	*executorHTTPFixture
	withOrder bool
	modelJSON string
}

func supportExecutorHTTP(t *testing.T, h *runHarness, r agentrun.Run, withOrder, correction bool) *supportExecutorHTTPFixture {
	t.Helper()
	f := &supportExecutorHTTPFixture{executorHTTPFixture: &executorHTTPFixture{counts: map[string]int{}, correction: correction}, withOrder: withOrder,
		modelJSON: `{"decision":"proposal","action":"escalate","conclusion":"delayed","requested_fields":[],"target_ticket_status":"escalated","claims":[{"kind":"timing","test":"delivered_late","event_id":"synthetic-delivered","refs":["T#/observed_at","E1#/order/promised_delivery_at","P01.1"]}]}`}
	if !withOrder {
		f.modelJSON = `{"decision":"proposal","action":"request_information","conclusion":"insufficient","requested_fields":["ticket.order_id"],"target_ticket_status":"awaiting_information","claims":[{"kind":"missing","field":"ticket.order_id","refs":["T#/order_id","E1#/missing","P01.1"]}]}`
	}
	h.Capture.mu.Lock()
	for _, snapshot := range h.Capture.snapshots {
		if snapshot.ID == r.SnapshotID {
			f.snapshot = snapshot
		}
	}
	h.Capture.mu.Unlock()
	if f.snapshot.ID == "" {
		t.Fatal("missing admitted synthetic snapshot")
	}
	server := httptest.NewServer(http.HandlerFunc(f.serveSupport))
	t.Cleanup(server.Close)
	f.BusinessOrigin = server.URL
	// These are the fixed integration-image provider/embedding origins. No
	// production endpoint or configurable provider URL enters the fixture.
	for _, address := range []string{"127.0.0.1:11434", "127.0.0.1:18093"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		server := httptest.NewUnstartedServer(http.HandlerFunc(f.serveSupport))
		_ = server.Listener.Close()
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
	}
	return f
}

func (f *supportExecutorHTTPFixture) reject(w http.ResponseWriter) {
	f.badRequest.Store(true)
	w.WriteHeader(http.StatusBadRequest)
}

func (f *supportExecutorHTTPFixture) serveSupport(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasSuffix(path, "/order") {
		path = "order"
	} else if strings.HasSuffix(path, "/delivery") {
		path = "delivery"
	}
	if path != "order" && path != "delivery" && path != "/api/embed" && path != "/chat/completions" {
		// Existing metadata/search fixtures already include the complete fixed
		// embedding identity and snapshot/index/policy paragraph bindings.
		f.serve(w, r)
		return
	}
	f.mu.Lock()
	f.counts[path]++
	number := f.counts[path]
	f.mu.Unlock()
	var response any
	switch path {
	case "order", "delivery":
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer synthetic-business-read-key" ||
			r.URL.Path != "/business/v1/snapshots/"+f.snapshot.ID+"/"+path || path == "delivery" && !f.withOrder {
			f.reject(w)
			return
		}
		var ticket business.Ticket
		if json.Unmarshal(f.snapshot.Ticket, &ticket) != nil {
			f.reject(w)
			return
		}
		evidence := business.Evidence{SnapshotID: f.snapshot.ID, Kind: path, EvidenceRef: "business-evidence:" + f.snapshot.ID + ":" + path}
		if !f.withOrder {
			evidence.Missing, evidence.MissingReason = true, "not_associated"
		} else if path == "order" {
			deliveryID := "delivery-1"
			evidence.Order = &business.Order{TenantID: ticket.TenantID, OrderID: *ticket.OrderID, Revision: 1, DeliveryID: &deliveryID,
				Status: "shipped", OrderedAt: ticket.ObservedAt.Add(-48 * time.Hour), PromisedDeliveryAt: ticket.ObservedAt.Add(-time.Hour)}
		} else {
			evidence.Delivery = &business.Delivery{TenantID: ticket.TenantID, DeliveryID: "delivery-1", OrderID: *ticket.OrderID, AggregateRevision: 1,
				Status: "delivered", Events: []business.DeliveryEvent{{EventID: "synthetic-delivered", Status: "delivered", OccurredAt: ticket.ObservedAt,
					Note: "Synthetic process-contract observation"}}}
		}
		response = evidence
	case "/api/embed":
		var body struct {
			Model    string   `json:"model"`
			Input    []string `json:"input"`
			Truncate bool     `json:"truncate"`
		}
		missing := "true"
		if f.withOrder {
			missing = "false"
		}
		if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&body) != nil || body.Model != "all-minilm:22m" ||
			len(body.Input) != 1 || body.Truncate || len(body.Input[0]) > 512 || !strings.Contains(body.Input[0], "order_missing="+missing) ||
			!strings.Contains(body.Input[0], "policy=fixture-policy-v1") || !strings.Contains(body.Input[0], "Synthetic contract ticket") {
			f.reject(w)
			return
		}
		vector := make([]float64, 384)
		vector[0] = 1
		response = map[string]any{"model": "all-minilm:22m", "embeddings": [][]float64{vector}, "prompt_eval_count": 3}
	case "/chat/completions":
		if !f.supportChatRequest(w, r, number) {
			f.reject(w)
			return
		}
		content := f.modelJSON
		if f.correction && number == 1 {
			content = `{"summary":"unregistered free summary"}`
		}
		response = map[string]any{"id": "synthetic-support-chat", "object": "chat.completion", "created": 1, "model": "deepseek-flash", "system_fingerprint": "fp_synthetic_support",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "logprobs": nil, "message": map[string]any{"role": "assistant", "content": content}}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15, "prompt_cache_hit_tokens": 2, "prompt_cache_miss_tokens": 8}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (f *supportExecutorHTTPFixture) supportChatRequest(w http.ResponseWriter, r *http.Request, number int) bool {
	var body struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer synthetic-provider-key" ||
		json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&body) != nil || body.Model != "deepseek-flash" || body.Stream || body.MaxTokens != 1024 ||
		len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" ||
		!strings.Contains(body.Messages[0].Content, "exactly six non-null fields") {
		return false
	}
	correcting := strings.Contains(body.Messages[0].Content, "only correction attempt")
	if correcting != (f.correction && number == 2) {
		return false
	}
	var facts struct {
		Ticket        business.Ticket      `json:"T"`
		Order         business.Evidence    `json:"E1"`
		Delivery      *business.Evidence   `json:"E2"`
		Policies      []business.PolicyHit `json:"policies"`
		AvailableRefs []string             `json:"available_refs"`
	}
	decoder := json.NewDecoder(strings.NewReader(body.Messages[1].Content))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&facts) != nil || facts.Ticket.TenantID != f.snapshot.TenantID || facts.Ticket.TicketID != f.snapshot.TicketID ||
		facts.Ticket.Revision != 1 || facts.Order.SnapshotID != f.snapshot.ID || facts.Order.Missing == f.withOrder ||
		len(facts.Policies) != 1 || facts.Policies[0].IndexID != f.snapshot.IndexID || facts.Policies[0].ChunkID != "P01.1" ||
		!slices.Contains(facts.AvailableRefs, "P01.1") || (facts.Delivery != nil) != f.withOrder {
		return false
	}
	return !f.withOrder || facts.Delivery.SnapshotID == f.snapshot.ID && facts.Delivery.Delivery != nil &&
		facts.Delivery.Delivery.TenantID == f.snapshot.TenantID && facts.Delivery.Delivery.AggregateRevision == 1 &&
		len(facts.Delivery.Delivery.Events) == 1 && facts.Delivery.Delivery.Events[0].EventID == "synthetic-delivered"
}

// TestRunSupportExecutor exercises the installed production support parser,
// sources, template and conditional flow through real guardian/step processes.
// All HTTP facts and provider usage are synthetic; this is not model quality or
// forty-case support acceptance, and it sends no request to a cloud endpoint.
func TestRunSupportExecutor(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		withOrder, correction bool
	}{{"with_order_proposal", true, false}, {"missing_order_one_correction", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			h := supportExecutorHarness(t, tc.name, tc.withOrder)
			r := h.submit(t, "tenant-a", tc.name)
			fixture := supportExecutorHTTP(t, h, r, tc.withOrder, tc.correction)
			client := executorGateway(t, h)
			beforeChildren := executorChildren(os.Getpid())
			running := startExecutorWorker(t, h, fixture.executorHTTPFixture, client)
			waitExecutorSignal(t, running, client.looped)
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if view.State != agentrun.AwaitingApproval || view.Outcome != nil || view.ProposalRef == nil || view.CursorVersion != 6 {
				var failure string
				if err := h.Pool.QueryRow(h.Ctx, "select coalesce(error_code,'') from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&failure); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("support executor state=%s cursor=%d domain_error=%s", view.State, view.CursorVersion, failure)
			}
			page, err := h.Store.Steps(h.Ctx, r.TenantID, r.ID, 0, 20)
			if err != nil || len(page.Items) != 6 {
				t.Fatalf("support accepted steps: %v", err)
			}
			kinds := make([]string, 0, len(page.Items))
			for _, step := range page.Items {
				kinds = append(kinds, step.Kind)
			}
			want := []string{"read_ticket", "get_order", "get_delivery", "search_policy", "model_proposal", "submit_proposal"}
			if tc.correction {
				want = []string{"read_ticket", "get_order", "search_policy", "model_proposal", "protocol_correction", "submit_proposal"}
				marker, _, err := agentrun.CanonicalStepResultForStrategy(page.Items[3].Output, "model_proposal", agentrun.SupportFixedStrategy)
				if err != nil || !marker.CorrectionRequired || marker.Proposal != nil {
					t.Fatalf("correction did not preserve the rejected observation marker: %v", err)
				}
			}
			if !slices.Equal(kinds, want) {
				t.Fatalf("support executed wrong finite path: %v", kinds)
			}
			expected, err := agentrun.SupportProposalFromModel(fixture.snapshot, page.Items[:4], []byte(fixture.modelJSON))
			if err != nil {
				t.Fatal(err)
			}
			assertSupportStoredProposal(t, page.Items[4].Output, expected)
			assertSupportStoredProposal(t, page.Items[5].Output, expected)
			assertSupportExecutorFacts(t, h, r, fixture, tc.correction)
			afterChildren := executorChildren(os.Getpid())
			slices.Sort(beforeChildren)
			slices.Sort(afterChildren)
			if !slices.Equal(beforeChildren, afterChildren) || client.failed.Load() != 0 {
				t.Fatal("support worker leaked child processes or reported a failure")
			}
		})
	}
}

func assertSupportExecutorFacts(t *testing.T, h *runHarness, r agentrun.Run, fixture *supportExecutorHTTPFixture, correction bool) {
	t.Helper()
	for _, path := range []string{"order", "search", "/api/version", "/api/tags", "/api/embed"} {
		if fixture.count(path) != 1 {
			t.Fatalf("support physical path %s count=%d", path, fixture.count(path))
		}
	}
	wantDelivery, wantChat, wantTools, wantRejected := 1, 1, 3, 0
	if correction {
		wantDelivery, wantChat, wantTools, wantRejected = 0, 2, 2, 1
	}
	if fixture.count("delivery") != wantDelivery || fixture.count("/chat/completions") != wantChat || fixture.badRequest.Load() {
		t.Fatal("support adapter bypassed bounded request/source contracts or repeated HTTP")
	}
	var calls, observed, known, rejected, tools, approvals, slots int
	var ticket []byte
	if err := h.Pool.QueryRow(h.Ctx, `select count(*),count(observation_hash),count(*) filter(where status='known'),
		count(*) filter(where business_outcome='rejected') from physical_calls where run_id=$1`, r.ID).Scan(&calls, &observed, &known, &rejected); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(h.Ctx, `select (select count(*) from tool_invocations where run_id=$1),
		(select count(*) from run_approvals where run_id=$1 and status='pending'),(select coalesce(sum(used),0) from execution_slots),
		ticket_binding from runs where run_id=$1`, r.ID).Scan(&tools, &approvals, &slots, &ticket); err != nil {
		t.Fatal(err)
	}
	expectedTicket, err := agentrun.CanonicalCheckpointJSON(fixture.snapshot.Ticket)
	if err != nil {
		t.Fatal(err)
	}
	actualTicket, err := agentrun.CanonicalCheckpointJSON(ticket)
	if err != nil || !bytes.Equal(actualTicket, expectedTicket) || approvals != 1 || slots != 0 || calls != 7 || observed != calls ||
		known != 1+wantChat || tools != wantTools || rejected != wantRejected {
		t.Fatalf("support persistent facts: calls=%d observed=%d known=%d rejected=%d tools=%d approvals=%d slots=%d err=%v", calls, observed, known, rejected, tools, approvals, slots, err)
	}
}
