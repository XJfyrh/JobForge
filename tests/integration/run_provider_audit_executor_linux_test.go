//go:build linux

package integration

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	"github.com/xjfyrh/jobforge/internal/runworker"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

// Every fault below crosses real TCP gRPC and PostgreSQL, with the installed
// guardian/step and actual independent pipes. Provider bodies are synthetic.
func TestRunProviderAuditExecutorProviderStop(t *testing.T) {
	for _, mode := range []string{"incompatible", "usage-absent", "reasoning"} {
		t.Run(mode, func(t *testing.T) {
			h := setupExecutorHarness(t)
			r := h.submit(t, "tenant-a", "provider-stop-"+mode)
			next := h.submit(t, "tenant-a", "provider-next-"+mode)
			fixture := executorHTTP(t, h, r, false, false)
			switch mode {
			case "incompatible":
				fixture.responseModel = "different-model"
			case "usage-absent":
				fixture.omitUsage = true
			case "reasoning":
				fixture.reasoningTokens = 2
			}
			client := executorGateway(t, h)
			running := startExecutorWorker(t, h, fixture, client, runworker.ErrBatchStopped)
			auditExecutorStopped(t, running)
			calls, err := h.Store.Calls(h.Ctx, r.TenantID, r.ID)
			if err != nil || !calls.BatchFrozen || calls.BatchStopCode == nil || len(calls.Items) != 7 {
				t.Fatalf("provider stop was not durable: frozen=%v calls=%d err=%v", calls.BatchFrozen, len(calls.Items), err)
			}
			chat := calls.Items[6]
			if chat.ProviderAudit == nil || chat.ReportHash == nil || chat.AuditStatus != "recorded" ||
				chat.ObservedAt != nil || fixture.count("/chat/completions") != 1 || client.claimAttempts.Load() != 1 {
				t.Fatal("provider stop lost audit or authorized a later execution")
			}
			if mode == "reasoning" {
				if !chat.UsageKnown || chat.SettledUsage == nil || chat.SettledUsage.OutputTokens != 5 ||
					chat.KnownTokens != 15 || chat.HeldTokens != 0 || chat.ProviderAudit.ReasoningTokens == nil ||
					*chat.ProviderAudit.ReasoningTokens != 2 {
					t.Fatal("mode stop lost valid usage or counted reasoning twice")
				}
			} else if chat.UsageKnown || chat.SettledUsage != nil || chat.KnownCostMicroyuan != 0 ||
				chat.HeldCostMicroyuan != chat.Reserved.CostMicroyuan {
				t.Fatal("unpriceable provider response released its hold")
			}
			auditExecutorNextClaim(t, h, next, false)
		})
	}
}

func auditExecutorStopped(t *testing.T, running *executorWorkerRun) {
	t.Helper()
	select {
	case <-running.done:
		if !errors.Is(running.err, runworker.ErrBatchStopped) {
			t.Fatalf("audited worker did not stop: %v", running.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("audited worker did not finish bounded cleanup")
	}
}

// Expiring isolated timing rows accelerates session recovery; it is not proof
// of waiting the production lease/session durations. The old Worker is joined.
func auditExecutorNextClaim(t *testing.T, h *runHarness, next agentrun.Run, allowed bool) {
	t.Helper()
	if _, err := h.Pool.Exec(h.Ctx, `update worker_sessions set seen_at=clock_timestamp()-interval '2 seconds',
		expires_at=clock_timestamp()-interval '1 second' where worker_id=$1`, h.Principal); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, `update runs set lease_until=clock_timestamp()-interval '1 millisecond'
		where worker_id=$1 and state='running'`, h.Principal); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.Sweep(h.Ctx, 100); err != nil {
		t.Fatal(err)
	}
	session, err := h.Store.Register(h.Ctx, h.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := h.Store.Claim(h.Ctx, h.Principal, session.ID)
	if allowed {
		if err != nil || claimed == nil || claimed.Lease.RunID != next.ID {
			t.Fatalf("fully persisted barrier blocked next case: claim=%v err=%v", claimed != nil, err)
		}
	} else if !errors.Is(err, agentrun.ErrBudgetExhausted) || claimed != nil {
		t.Fatalf("new session bypassed unresolved/frozen chat: claim=%v err=%v", claimed != nil, err)
	}
}

// Hold the actual Run row across the real RPC. pg_blocking_pids proves that the
// transaction reached PostgreSQL and was blocked before commit; no sleep serves
// as a substitute for this fact. The original bounded RPC deadline ends it.
func auditExecutorBlockBefore(ctx context.Context, h *runHarness, runID string, reached *atomic.Bool, invoke func() error) error {
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(h.Ctx) }()
	var blocker int32
	if err = tx.QueryRow(ctx, "select pg_backend_pid()").Scan(&blocker); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, "select run_id from runs where run_id=$1 for update", runID); err != nil {
		return err
	}
	finished := make(chan error, 1)
	go func() { finished <- invoke() }()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err = h.Pool.QueryRow(ctx, `select exists(select 1 from pg_stat_activity
			where datname=current_database() and $1=any(pg_blocking_pids(pid)))`, blocker).Scan(&waiting); err != nil {
			<-finished
			return fmt.Errorf("did not establish precommit database block: %w", err)
		}
		if waiting {
			reached.Store(true)
			return <-finished
		}
		select {
		case err = <-finished:
			return fmt.Errorf("RPC finished before database block: %w", err)
		case <-ticker.C:
		}
	}
}

func TestRunProviderAuditExecutorConfirmationWindows(t *testing.T) {
	for _, phase := range []string{"reserve", "report", "observe", "commit"} {
		for _, position := range []string{"before", "after"} {
			t.Run(phase+"-"+position, func(t *testing.T) {
				h := setupExecutorHarness(t)
				r := h.submit(t, "tenant-a", phase+"-"+position)
				next := h.submit(t, "tenant-a", "next-"+phase+"-"+position)
				fixture := executorHTTP(t, h, r, false, false)
				client := executorGateway(t, h)
				var reached atomic.Bool
				fault := func(ctx context.Context, invoke func() error) error {
					if position == "before" {
						return auditExecutorBlockBefore(ctx, h, r.ID, &reached, invoke)
					}
					if err := invoke(); err != nil {
						return err
					}
					reached.Store(true)
					return status.Error(codes.Unavailable, "synthetic committed confirmation loss")
				}
				switch phase {
				case "reserve":
					client.reserve = func(ctx context.Context, req *agentv1.ReserveCallRequest, opts ...grpc.CallOption) (*agentv1.ReserveCallResponse, error) {
						if req.Subcall != agentv1.Subcall_SUBCALL_CHAT {
							return client.AgentServiceClient.ReserveCall(ctx, req, opts...)
						}
						return nil, fault(ctx, func() error { _, err := client.AgentServiceClient.ReserveCall(ctx, req, opts...); return err })
					}
				case "report":
					client.settle = func(ctx context.Context, req *agentv1.SettleUsageRequest, opts ...grpc.CallOption) (*agentv1.SettleUsageResponse, error) {
						if req.ProviderAudit == nil {
							return client.AgentServiceClient.SettleUsage(ctx, req, opts...)
						}
						return nil, fault(ctx, func() error { _, err := client.AgentServiceClient.SettleUsage(ctx, req, opts...); return err })
					}
				case "observe":
					client.observe = func(ctx context.Context, req *agentv1.ObserveCallRequest, opts ...grpc.CallOption) (*agentv1.ObserveCallResponse, error) {
						if req.AuditHash == "" {
							return client.AgentServiceClient.ObserveCall(ctx, req, opts...)
						}
						return nil, fault(ctx, func() error { _, err := client.AgentServiceClient.ObserveCall(ctx, req, opts...); return err })
					}
				case "commit":
					client.commit = func(ctx context.Context, req *agentv1.CommitStepRequest, opts ...grpc.CallOption) (*agentv1.CommitStepResponse, error) {
						if req.Step.Kind != agentv1.StepKind_STEP_KIND_MODEL_PROPOSAL {
							return client.AgentServiceClient.CommitStep(ctx, req, opts...)
						}
						return nil, fault(ctx, func() error { _, err := client.AgentServiceClient.CommitStep(ctx, req, opts...); return err })
					}
				}
				running := startExecutorWorker(t, h, fixture, client, runworker.ErrBatchStopped)
				auditExecutorStopped(t, running)
				if !reached.Load() {
					t.Fatal("required database fault window was not established")
				}
				calls, err := h.Store.Calls(h.Ctx, r.TenantID, r.ID)
				if err != nil || calls.BatchFrozen || client.claimAttempts.Load() != 1 {
					t.Fatalf("unconfirmed path invented freeze or kept claiming: frozen=%v claims=%d err=%v", calls.BatchFrozen, client.claimAttempts.Load(), err)
				}
				wantCalls, wantSent := 7, 1
				if phase == "reserve" {
					wantSent = 0
					if position == "before" {
						wantCalls = 6
					}
				}
				if len(calls.Items) != wantCalls || fixture.count("/chat/completions") != wantSent {
					t.Fatal("uncertain confirmation resent or hid the original physical call")
				}
				if wantCalls == 7 {
					chat := calls.Items[6]
					wantReport := phase == "observe" || phase == "commit" || (phase == "report" && position == "after")
					wantObservation := phase == "commit" || (phase == "observe" && position == "after")
					if (chat.ReportHash != nil) != wantReport || chat.UsageKnown != wantReport || (chat.ObservedAt != nil) != wantObservation {
						t.Fatal("confirmation loss was confused with transaction rollback")
					}
				}
				// No reservation exists before a rejected Reserve. A completed Commit
				// also closes the barrier even when its reply was lost. Every other
				// paid window must stop a distinct session/driver at the database.
				allowed := (phase == "reserve" && position == "before") || (phase == "commit" && position == "after")
				auditExecutorNextClaim(t, h, next, allowed)
			})
		}
	}
}

func TestRunProviderAuditExecutorFinalCorrectionFailureAllowsNextCase(t *testing.T) {
	h := setupExecutorHarness(t)
	r := h.submit(t, "tenant-a", "twice-invalid")
	fixture := executorHTTP(t, h, r, true, false)
	fixture.rejectEveryChat = true
	client := executorGateway(t, h)
	running := startExecutorWorker(t, h, fixture, client)
	waitExecutorSignal(t, running, client.looped)
	running.cancel()
	select {
	case <-running.done:
	case <-time.After(3 * time.Second):
		t.Fatal("completed failing Worker did not join")
	}
	view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil || view.State != agentrun.Failed || view.CursorVersion != 5 || fixture.count("/chat/completions") != 2 {
		t.Fatalf("second correction failed incorrectly: state=%s cursor=%d err=%v", view.State, view.CursorVersion, err)
	}
	calls, err := h.Store.Calls(h.Ctx, r.TenantID, r.ID)
	if err != nil || calls.BatchFrozen || len(calls.Items) != 8 {
		t.Fatal("business validation failure became a provider freeze")
	}
	for _, chat := range calls.Items[6:] {
		if !chat.UsageKnown || chat.ObservedAt == nil || chat.ProviderAudit == nil || chat.BusinessOutcome == nil || *chat.BusinessOutcome != "rejected" {
			t.Fatal("failed proposal discarded independently valid usage/audit")
		}
	}
	next := h.submit(t, "tenant-a", "after-twice-invalid")
	auditExecutorNextClaim(t, h, next, true)
}
