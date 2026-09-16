package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

// These tests execute the production Worker and installed guardian/step. Only
// the labelled business/model HTTP service and registry are synthetic; all
// control facts cross actual TCP gRPC and isolated PostgreSQL transactions.
func TestRunExecutorCompleteRegisteredFlow(t *testing.T) {
	for _, correction := range []bool{false, true} {
		name := "normal-six-steps"
		if correction {
			name = "one-correction-seven-steps"
		}
		t.Run(name, func(t *testing.T) {
			h := setupExecutorHarness(t)
			r := h.submit(t, "tenant-a", name)
			fixture := executorHTTP(t, h, r, correction, false)
			client := executorGateway(t, h)
			running := startExecutorWorker(t, h, fixture, client)
			waitExecutorSignal(t, running, client.looped)
			view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantSteps, wantChat := int64(6), 1
			if correction {
				wantSteps++
				wantChat++
			}
			if view.State != agentrun.Succeeded || view.CursorVersion != wantSteps {
				var failure string
				if err := h.Pool.QueryRow(h.Ctx, "select coalesce(error_code,'') from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&failure); err != nil {
					t.Fatal(err)
				}
				t.Fatalf("formal worker did not finish registered graph: state=%s cursor=%d domain_error=%s", view.State, view.CursorVersion, failure)
			}
			for _, path := range []string{"order", "delivery", "search", "/api/version", "/api/tags", "/api/embed"} {
				if fixture.count(path) != 1 {
					t.Fatalf("physical path %s count=%d", path, fixture.count(path))
				}
			}
			if fixture.count("/chat/completions") != wantChat || fixture.badRequest.Load() {
				t.Fatal("unexpected HTTP retry or request contract")
			}
			var calls, observed, known, tools, steps, rejected int
			if err := h.Pool.QueryRow(h.Ctx, `select count(*),count(observation_hash),count(*) filter(where status='known'),count(*) filter(where business_outcome='rejected') from physical_calls where run_id=$1`, r.ID).Scan(&calls, &observed, &known, &rejected); err != nil {
				t.Fatal(err)
			}
			if err := h.Pool.QueryRow(h.Ctx, "select count(*) from tool_invocations where run_id=$1", r.ID).Scan(&tools); err != nil {
				t.Fatal(err)
			}
			if err := h.Pool.QueryRow(h.Ctx, "select count(*) from run_steps where run_id=$1", r.ID).Scan(&steps); err != nil {
				t.Fatal(err)
			}
			wantRejected := 0
			if correction {
				wantRejected = 1
			}
			if calls != 6+wantChat || observed != calls || known != 1+wantChat || tools != 3 || steps != int(wantSteps) || rejected != wantRejected || client.failed.Load() != 0 {
				t.Fatalf("durable flow counts: calls=%d observed=%d known=%d tools=%d steps=%d rejected=%d fail=%d", calls, observed, known, tools, steps, rejected, client.failed.Load())
			}
		})
	}
}

func TestRunExecutorObserveUnconfirmedHasNoFollowingHTTP(t *testing.T) {
	for _, mode := range []string{"before-commit-lock", "after-commit-lost-ack"} {
		t.Run(mode, func(t *testing.T) {
			h := setupExecutorHarness(t)
			r := h.submit(t, "tenant-a", mode)
			fixture := executorHTTP(t, h, r, false, false)
			client := executorGateway(t, h)
			blocked := make(chan struct{})
			var once sync.Once
			client.observe = func(ctx context.Context, req *agentv1.ObserveCallRequest, opts ...grpc.CallOption) (*agentv1.ObserveCallResponse, error) {
				if mode == "after-commit-lost-ack" {
					response, err := client.AgentServiceClient.ObserveCall(ctx, req, opts...)
					if err != nil {
						return response, err
					}
					once.Do(func() { close(blocked) })
					return nil, status.Error(codes.Unavailable, "synthetic committed observation ACK loss")
				}
				tx, err := h.Pool.Begin(ctx)
				if err != nil {
					return nil, err
				}
				defer func() { _ = tx.Rollback(h.Ctx) }()
				var blocker int32
				if err = tx.QueryRow(ctx, "select pg_backend_pid()").Scan(&blocker); err != nil {
					return nil, err
				}
				if _, err = tx.Exec(ctx, "select physical_call_id from physical_calls where physical_call_id=$1 for update", req.PhysicalCallId); err != nil {
					return nil, err
				}
				type answer struct {
					response *agentv1.ObserveCallResponse
					err      error
				}
				finished := make(chan answer, 1)
				go func() {
					response, callErr := client.AgentServiceClient.ObserveCall(ctx, req, opts...)
					finished <- answer{response, callErr}
				}()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					var waiting bool
					if err = h.Pool.QueryRow(ctx, `select exists(select 1 from pg_stat_activity where datname=current_database() and $1=any(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
						result := <-finished
						return result.response, result.err
					}
					if waiting {
						once.Do(func() { close(blocked) })
						result := <-finished
						return result.response, result.err
					}
					select {
					case result := <-finished:
						return result.response, result.err
					case <-ticker.C:
					}
				}
			}
			running := startExecutorWorker(t, h, fixture, client)
			waitExecutorSignal(t, running, blocked)
			waitExecutorSignal(t, running, client.looped)
			if fixture.count("order") != 1 || fixture.count("delivery") != 0 || fixture.count("/api/version") != 0 || fixture.count("/chat/completions") != 0 {
				t.Fatal("unconfirmed observation authorized a later HTTP")
			}
			var calls, observed, steps int
			if err := h.Pool.QueryRow(h.Ctx, "select count(*),count(observation_hash) from physical_calls where run_id=$1", r.ID).Scan(&calls, &observed); err != nil {
				t.Fatal(err)
			}
			if err := h.Pool.QueryRow(h.Ctx, "select count(*) from run_steps where run_id=$1", r.ID).Scan(&steps); err != nil {
				t.Fatal(err)
			}
			wantObserved := 0
			if mode == "after-commit-lost-ack" {
				wantObserved = 1
			}
			if calls != 1 || observed != wantObserved || steps != 1 || client.observeAttempts.Load() > 2 {
				t.Fatalf("observation boundary: calls=%d observed=%d steps=%d attempts=%d", calls, observed, steps, client.observeAttempts.Load())
			}
		})
	}
}

func TestRunExecutorCommitACKLossDoesNotFailOrRepeatStep(t *testing.T) {
	h := setupExecutorHarness(t)
	r := h.submit(t, "tenant-a", "commit-ack-loss")
	fixture := executorHTTP(t, h, r, false, false)
	client := executorGateway(t, h)
	client.commit = func(ctx context.Context, req *agentv1.CommitStepRequest, opts ...grpc.CallOption) (*agentv1.CommitStepResponse, error) {
		response, err := client.AgentServiceClient.CommitStep(ctx, req, opts...)
		if err != nil {
			return response, err
		}
		return nil, status.Error(codes.Unavailable, "synthetic committed step ACK loss")
	}
	running := startExecutorWorker(t, h, fixture, client)
	waitExecutorSignal(t, running, client.confirmed)
	waitExecutorSignal(t, running, client.looped)
	var steps, calls int
	if err := h.Pool.QueryRow(h.Ctx, "select count(*) from run_steps where run_id=$1", r.ID).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(h.Ctx, "select count(*) from physical_calls where run_id=$1", r.ID).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if steps != 1 || calls != 0 || client.commitAttempts.Load() != 1 || client.failed.Load() != 0 || fixture.count("order") != 0 {
		t.Fatal("uncertain Commit repeated business or invented a failure")
	}
}

func TestRunExecutorBudgetRejectionRetainsOriginalFailure(t *testing.T) {
	h := setupExecutorHarness(t)
	r := h.submit(t, "tenant-a", "budget-rejection")
	if _, err := h.Pool.Exec(h.Ctx, "update budget_accounts set limit_physical_http=0 where scope='batch'"); err != nil {
		t.Fatal(err)
	}
	fixture := executorHTTP(t, h, r, false, false)
	client := executorGateway(t, h)
	running := startExecutorWorker(t, h, fixture, client)
	waitExecutorSignal(t, running, client.looped)
	var failure *string
	if err := h.Pool.QueryRow(h.Ctx, "select error_code from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&failure); err != nil {
		t.Fatal(err)
	}
	if failure == nil || *failure != "BUDGET_EXHAUSTED" || fixture.count("order") != 0 || client.failed.Load() != 1 {
		t.Fatalf("budget reason lost: error=%v order=%d Fail=%d", failure, fixture.count("order"), client.failed.Load())
	}
}

func TestRunExecutorCancelDuringRealHTTPStopsProcess(t *testing.T) {
	h := setupExecutorHarness(t)
	r := h.submit(t, "tenant-a", "cancel-http")
	fixture := executorHTTP(t, h, r, false, true)
	client := executorGateway(t, h)
	running := startExecutorWorker(t, h, fixture, client)
	waitExecutorSignal(t, running, fixture.orderSeen)
	if _, err := h.Service.Cancel(h.Ctx, r.TenantID, r.ID, "cancel-executor-http"); err != nil {
		t.Fatal(err)
	}
	waitExecutorSignal(t, running, client.looped)
	view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != agentrun.Cancelled || view.CursorVersion != 1 || fixture.count("order") != 1 || fixture.count("delivery") != 0 || client.failed.Load() != 0 {
		t.Fatalf("cancel crossed execution boundary: state=%s cursor=%d fail=%d", view.State, view.CursorVersion, client.failed.Load())
	}
}
