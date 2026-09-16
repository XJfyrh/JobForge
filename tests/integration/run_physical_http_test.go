package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
)

// sendPhysicalFixture deliberately exists only in this test. It sends one POST
// to the test-owned URL only for a freshly acknowledged permit. It is not a
// production provider adapter, persisted permit replay, or guardian substitute.
func sendPhysicalFixture(ctx context.Context, client *http.Client, target string, permit agentrun.ReserveCallResponse) ([]byte, bool, error) {
	if !permit.NewlyReserved {
		return nil, false, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewBufferString(`{"synthetic_fixture":true}`))
	if err != nil {
		return nil, false, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16*1024+1))
	return body, true, err
}

func TestRunPhysicalHTTPFaultsKeepUnknownExposure(t *testing.T) {
	for _, fault := range []string{"lost_reservation_ack", "disconnect_after_receive", "truncated_response"} {
		t.Run(fault, func(t *testing.T) {
			h := setupRunHarness(t)
			claimed := ledgerAtStep(t, h, "tenant-a", "physical-fault", "model_proposal")
			var received atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if fault == "truncated_response" {
					w.Header().Set("Content-Length", "256")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"usage":`))
					return
				}
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error("synthetic server failed to take ownership of the test connection")
					return
				}
				_ = connection.Close()
			}))
			t.Cleanup(server.Close)
			client := server.Client()
			client.Timeout = 2 * time.Second
			request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			first, err := h.Store.ReserveCall(h.Ctx, h.Principal, request)
			if err != nil || !first.NewlyReserved {
				t.Fatalf("first physical permit: %+v %v", first, err)
			}
			before := ledgerView(t, h, claimed.Lease)
			duplicate, err := h.Store.ReserveCall(h.Ctx, h.Principal, request)
			if err != nil || duplicate.NewlyReserved {
				t.Fatalf("duplicate physical permit: %+v %v", duplicate, err)
			}
			if _, sent, err := sendPhysicalFixture(h.Ctx, client, server.URL, duplicate); err != nil || sent || received.Load() != 0 {
				t.Fatalf("lost reservation ACK reauthorized sending: sent=%t received=%d error=%v", sent, received.Load(), err)
			}
			if fault != "lost_reservation_ack" {
				if _, sent, err := sendPhysicalFixture(h.Ctx, client, server.URL, first); err == nil || !sent || received.Load() != 1 {
					t.Fatalf("real HTTP fault was not exercised exactly once: sent=%t received=%d error=%v", sent, received.Load(), err)
				}
			}
			ledgerObserve(t, h, request, "unknown", "unknown", nil)
			afterUnknown := ledgerView(t, h, claimed.Lease)
			if !reflect.DeepEqual(before.Budget, afterUnknown.Budget) || before.CursorVersion != afterUnknown.CursorVersion {
				t.Fatal("uncertain real network result refunded budget or advanced the checkpoint")
			}
			secondRequest := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
			second, err := h.Store.ReserveCall(h.Ctx, h.Principal, secondRequest)
			if err != nil || !second.NewlyReserved || second.Reservation.PhysicalCallID == first.Reservation.PhysicalCallID {
				t.Fatalf("new physical send did not obtain new identity and debit: %+v %v", second, err)
			}
			if _, sent, err := sendPhysicalFixture(h.Ctx, client, server.URL, second); err == nil || !sent {
				t.Fatalf("second real network fault absent: sent=%t error=%v", sent, err)
			}
			ledgerObserve(t, h, secondRequest, "unknown", "unknown", nil)
			wantReceived := int64(2)
			if fault == "lost_reservation_ack" {
				wantReceived = 1
			}
			if received.Load() != wantReceived {
				t.Fatalf("hidden HTTP retransmission: received=%d want=%d", received.Load(), wantReceived)
			}
			afterRedo := ledgerAccounts(ledgerView(t, h, claimed.Lease))
			for i, old := range ledgerAccounts(afterUnknown) {
				account := afterRedo[i]
				if account.Used.Chat != old.Used.Chat+1 || account.Used.PhysicalHTTP != old.Used.PhysicalHTTP+1 ||
					account.HeldTokens != old.HeldTokens+second.Reservation.Budget.TotalTokens ||
					account.HeldCostMicroyuan != old.HeldCostMicroyuan+second.Reservation.Budget.CostMicroyuan {
					t.Fatal("new send did not retain both unknown holds in every budget scope")
				}
			}
		})
	}
}

func TestRunPhysicalHTTPCancelStopsLaterSendsButAllowsLateMetering(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "physical-cancel", "model_proposal")
	received, release := make(chan struct{}), make(chan struct{})
	var count atomic.Int64
	usage := ledgerUsage(10, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			close(received)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(usage)
	}))
	t.Cleanup(server.Close)
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	permit, err := h.Store.ReserveCall(h.Ctx, h.Principal, request)
	if err != nil || !permit.NewlyReserved {
		t.Fatalf("initial authorization: %+v %v", permit, err)
	}
	sendCtx, cancel := context.WithTimeout(h.Ctx, 5*time.Second)
	defer cancel()
	type response struct {
		body []byte
		err  error
	}
	finished := make(chan response, 1)
	go func() {
		body, _, err := sendPhysicalFixture(sendCtx, server.Client(), server.URL, permit)
		finished <- response{body: body, err: err}
	}()
	select {
	case <-received:
	case <-sendCtx.Done():
		t.Fatal("real HTTP request never reached the fault server")
	}
	if _, err := h.Store.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "cancel-inflight-http"); err != nil {
		t.Fatal(err)
	}
	later, err := h.Store.ReserveCall(h.Ctx, h.Principal, ledgerRequest(h, claimed, agentrun.SubcallChat, ""))
	if !errors.Is(err, agentrun.ErrCancelRequested) || later.NewlyReserved {
		t.Fatalf("cancelled Run authorized a later physical request: %+v %v", later, err)
	}
	if _, sent, err := sendPhysicalFixture(sendCtx, server.Client(), server.URL, later); err != nil || sent {
		t.Fatalf("rejected permit produced an HTTP request: sent=%t error=%v", sent, err)
	}
	// The already admitted remote request can finish after cancellation. The
	// real server response is metering evidence, never renewed step authority.
	close(release)
	var remoteUsage agentrun.UsageReport
	select {
	case result := <-finished:
		if result.err != nil || json.Unmarshal(result.body, &remoteUsage) != nil || remoteUsage != usage {
			t.Fatalf("late remote metering response: %v", result.err)
		}
	case <-sendCtx.Done():
		t.Fatal("late remote response did not complete")
	}
	if count.Load() != 1 {
		t.Fatalf("cancelled flow sent %d HTTP requests", count.Load())
	}
	if _, err := h.Store.ObserveCall(h.Ctx, h.Principal, agentrun.ObserveCallRequest{Lease: claimed.Lease,
		Step: request.Step, PhysicalCallID: request.PhysicalCallID, TransportOutcome: "response", HTTPStatus: 200,
		BusinessOutcome: "accepted", UsageKnown: true, Usage: &remoteUsage}); !errors.Is(err, agentrun.ErrCancelRequested) {
		t.Fatalf("late network response regained normal execution authority: %v", err)
	}
	before := ledgerView(t, h, claimed.Lease)
	settled, err := h.Store.SettleUsage(h.Ctx, h.Principal, agentrun.SettleUsageRequest{Lease: claimed.Lease,
		PhysicalCallID: request.PhysicalCallID, Usage: &remoteUsage})
	if err != nil || !settled.NewlySettled || !settled.Reservation.UsageKnown {
		t.Fatalf("late complete usage was not settled: %+v %v", settled, err)
	}
	after := ledgerView(t, h, claimed.Lease)
	var active string
	if err := h.Pool.QueryRow(h.Ctx, "select active_call_id::text from runs where run_id=$1", claimed.Lease.RunID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if after.State != agentrun.Stopping || after.CursorVersion != before.CursorVersion || !after.UpdatedAt.Equal(before.UpdatedAt) || active != request.PhysicalCallID {
		t.Fatal("metering advanced progress, cleared active_call, or renewed execution")
	}
	if _, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease); err != nil {
		t.Fatal(err)
	}
	if ledgerView(t, h, claimed.Lease).State != agentrun.Cancelled {
		t.Fatal("explicit local stop did not converge to cancelled")
	}
}
