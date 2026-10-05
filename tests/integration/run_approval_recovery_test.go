package integration

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

type approvalReceiptReader func(context.Context, string, agentrun.Profile, business.SignedAction) (business.ActionReceipt, bool, error)

func (f approvalReceiptReader) LookupReceipt(ctx context.Context, tenant string, profile agentrun.Profile, action business.SignedAction) (business.ActionReceipt, bool, error) {
	return f(ctx, tenant, profile, action)
}

func approvalSyntheticReceipt(action business.SignedAction) business.ActionReceipt {
	a := action.Authorization
	hash, _ := a.Hash()
	r := business.ActionReceipt{SchemaVersion: 1, TenantID: a.TenantID, OperationID: a.OperationID, BusinessRequestID: a.BusinessRequestID, AuthorizationHash: hash, ProposalHash: a.ProposalHash, ParametersHash: a.ParametersHash, ApprovalID: a.ApprovalID, TicketID: action.Parameters.TicketID, BeforeRevision: a.VersionVector.Ticket.Revision, AfterRevision: a.VersionVector.Ticket.Revision + 1, TicketStatus: action.Parameters.TargetTicketStatus, AppliedAt: a.AuthorizedAt + 1, RetainUntil: a.AuthorizedAt + 1 + business.ActionReceiptRetention.Microseconds()}
	r.ReceiptHash = r.Hash()
	return r
}

func approvalAuthorizedCancelled(t *testing.T, h *runHarness, key string) (agentrun.Run, business.SignedAction, agentrun.ClaimedRun) {
	t.Helper()
	r := approvalPending(t, h, key)
	approveRun(t, h, r, "approve")
	claimed := h.claim(t)
	binding, err := h.Store.AuthorizeAction(h.Ctx, h.Principal, claimed.Lease, currentRunStep(claimed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Service.Cancel(h.Ctx, r.TenantID, r.ID, "cancel-after-authorization"); err != nil {
		t.Fatal(err)
	}
	closed, err := h.Store.AcknowledgeStop(h.Ctx, h.Principal, claimed.Lease)
	if err != nil || closed.Run.State != agentrun.Cancelled {
		t.Fatal("cancel closure", err)
	}
	return closed.Run, *binding.Action, claimed
}

func TestRunApprovalReconcileAndRetryPreserveTerminalFacts(t *testing.T) {
	for _, scenario := range []string{"found", "absent", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			h := setupApprovalHarness(t)
			r, action, stale := approvalAuthorizedCancelled(t, h, "receipt-"+scenario)
			receipt := approvalSyntheticReceipt(action)
			var queries atomic.Int32
			reader := approvalReceiptReader(func(_ context.Context, tenant string, p agentrun.Profile, a business.SignedAction) (business.ActionReceipt, bool, error) {
				queries.Add(1)
				if tenant != r.TenantID || p.Hash != r.ProfileHash || a.Authorization.OperationID != action.Authorization.OperationID {
					t.Fatal("changed receipt identity")
				}
				if scenario == "unavailable" {
					return business.ActionReceipt{}, false, agentrun.ErrDependencyUnavailable
				}
				return receipt, scenario == "found", nil
			})
			// Capture/profile/model account availability cannot block receipt-only paths.
			h.Options.Profiles[0].Executable = false
			var err error
			h.Store, err = runpostgres.New(h.Pool, h.Options)
			if err != nil {
				t.Fatal(err)
			}
			h.Capture.unavailable = true
			h.Service, err = agentrun.NewService(h.Store, h.Capture, []string{"tenant-north", "tenant-south"}, agentrun.WithReceiptReader(reader))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(h.Ctx, `update budget_accounts set frozen=true`); err != nil {
				t.Fatal(err)
			}
			before, _ := h.Store.Get(h.Ctx, r.TenantID, r.ID)
			beforeResult, _ := h.Store.Result(h.Ctx, r.TenantID, r.ID)
			beforeEvents, _ := h.Store.Events(h.Ctx, r.TenantID, r.ID, 0, 100)
			captures := h.Capture.count()
			if beforeResult.Disposition != "unknown" {
				t.Fatal("cancelled authorization lost uncertainty")
			}
			if scenario == "found" {
				effect, err := h.Service.Reconcile(h.Ctx, r.TenantID, r.ID)
				if err != nil || effect.State != "applied" {
					t.Fatal("reconcile", err)
				}
				after, _ := h.Store.Get(h.Ctx, r.TenantID, r.ID)
				afterResult, _ := h.Store.Result(h.Ctx, r.TenantID, r.ID)
				afterEvents, _ := h.Store.Events(h.Ctx, r.TenantID, r.ID, 0, 100)
				if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(beforeResult, afterResult) || !reflect.DeepEqual(beforeEvents, afterEvents) {
					t.Fatal("reconcile changed terminal execution")
				}
				if _, err := h.Store.CompleteAction(h.Ctx, h.Principal, agentrun.CompleteActionRequest{Lease: stale.Lease, Step: currentRunStep(stale), Receipt: receipt}); !errors.Is(err, agentrun.ErrStaleLease) {
					t.Fatal("stale receipt changed execution", err)
				}
			}
			request := agentrun.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 3600}
			child, err := h.Service.Retry(h.Ctx, r.TenantID, r.ID, "receipt-retry", request)
			if scenario == "unavailable" {
				if !errors.Is(err, agentrun.ErrDependencyUnavailable) {
					t.Fatal("dependency inferred absence", err)
				}
				var count int
				if err := h.Pool.QueryRow(h.Ctx, `select count(*) from runs where retry_of_run_id=$1`, r.ID).Scan(&count); err != nil || count != 0 {
					t.Fatal("error created successor", err)
				}
			} else {
				if err != nil || child.Run.ProfileHash != r.ProfileHash || child.Run.SnapshotID != r.SnapshotID || child.Run.BusinessRequestID != r.BusinessRequestID || child.Run.Budget.Family.ID != r.Budget.Family.ID {
					t.Fatal("retry changed original binding", err)
				}
				if scenario == "found" && (child.Run.State != agentrun.Succeeded || child.Run.Outcome == nil || *child.Run.Outcome != "applied") {
					t.Fatal("known receipt replanned")
				}
				if scenario == "absent" && (child.Run.State != agentrun.Failed || child.Run.Error == nil || child.Run.Error.Code != string(agentrun.ErrActionOutcomeUnknown)) {
					t.Fatal("absent receipt became new action")
				}
				for _, key := range []string{"receipt-retry", "receipt-alias"} {
					replay, err := h.Service.Retry(h.Ctx, r.TenantID, r.ID, key, request)
					if err != nil || !replay.Reused || replay.Run.ID != child.Run.ID {
						t.Fatal("retry forked", err)
					}
				}
				calls, err := h.Store.ActionCalls(h.Ctx, r.TenantID, child.Run.ID)
				if err != nil || len(calls.Items) != 0 {
					t.Fatal("successor dispatched action", err)
				}
			}
			if queries.Load() != 1 || h.Capture.count() != captures {
				t.Fatal("receipt retry repeated physical GET/capture", queries.Load())
			}
		})
	}
}

func TestRunApprovalReceiptGateNaturalExpiryAndOldRelease(t *testing.T) {
	h := setupApprovalHarness(t)
	r, action, _ := approvalAuthorizedCancelled(t, h, "query-gate")
	first, err := h.Store.ReserveReceiptQuery(h.Ctx, r.TenantID, r.ID, "reconcile", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.ReserveReceiptQuery(h.Ctx, r.TenantID, r.ID, "retry", uuid.NewString()); !errors.Is(err, agentrun.ErrRateLimited) {
		t.Fatal("in-flight gate bypass", err)
	}
	// A crashed caller does not release or renew its original ten-second lease.
	time.Sleep(time.Until(first.Deadline) + 30*time.Millisecond)
	second, err := h.Store.ReserveReceiptQuery(h.Ctx, r.TenantID, r.ID, "retry", uuid.NewString())
	if err != nil {
		t.Fatal("natural gate expiry", err)
	}
	if err := h.Store.RecordReceiptQuery(h.Ctx, r.TenantID, first.ID, action, nil, "absent"); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := h.Pool.QueryRow(h.Ctx, `select query_id::text from tenant_receipt_query_gates where tenant_id=$1`, r.TenantID).Scan(&current); err != nil || current != second.ID {
		t.Fatal("old release cleared successor", err)
	}
	if _, err := h.Store.ReserveReceiptQuery(h.Ctx, r.TenantID, r.ID, "reconcile", uuid.NewString()); !errors.Is(err, agentrun.ErrRateLimited) {
		t.Fatal("new caller bypassed active successor", err)
	}
	if err := h.Store.RecordReceiptQuery(h.Ctx, r.TenantID, second.ID, action, nil, "unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.ReserveReceiptQuery(h.Ctx, r.TenantID, r.ID, "reconcile", uuid.NewString()); !errors.Is(err, agentrun.ErrRateLimited) {
		t.Fatal("rate cooldown bypass", err)
	}
}

func TestRunApprovalBatchGuardBlocksOtherPendingChat(t *testing.T) {
	h := setupApprovalHarness(t)
	r := approvalPending(t, h, "approved-before-pending")
	otherHarness := *h
	otherHarness.Principal = "recovery-worker-2"
	var err error
	otherHarness.Session, err = h.Store.Register(h.Ctx, otherHarness.Principal, uuid.NewString(), h.Profile.ExecutorVersion)
	if err != nil {
		t.Fatal(err)
	}
	other := recoveryAtDecisionSameSession(t, &otherHarness, "other-chat")
	request, report := recoveryChat(t, &otherHarness, other, false)
	approveRun(t, h, r, "approve")
	claimed := h.claim(t)
	if _, err := h.Store.AuthorizeAction(h.Ctx, h.Principal, claimed.Lease, currentRunStep(claimed)); !errors.Is(err, agentrun.ErrBudgetExhausted) {
		t.Fatal("other pending chat authorized write", err)
	}
	var count int
	if err := h.Pool.QueryRow(h.Ctx, `select count(*) from action_authorizations`).Scan(&count); err != nil || count != 0 {
		t.Fatal("blocked guard persisted authorization", err)
	}
	auditObserve(t, &otherHarness, request, report, "accepted")
}

func recoveryAtDecisionSameSession(t *testing.T, h *runHarness, key string) agentrun.ClaimedRun {
	t.Helper()
	supportProfileCapture(t, h, "submit", "submit-"+key, "", true)
	h.submit(t, "tenant-north", key)
	claimed := h.claim(t)
	recoveryCommit(t, h, &claimed, supportStorageResult(t, h, claimed, "proposal"))
	return claimed
}
