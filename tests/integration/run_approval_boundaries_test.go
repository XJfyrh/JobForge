package integration

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xjfyrh/jobforge/internal/business"
	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func approvalSameAction(first, next *business.SignedAction) bool {
	if first == nil || next == nil {
		return false
	}
	a, errA := first.Authorization.Hash()
	b, errB := next.Authorization.Hash()
	p, errP := first.Parameters.Hash()
	q, errQ := next.Parameters.Hash()
	return errA == nil && errB == nil && errP == nil && errQ == nil && a == b && p == q && first.Signature == next.Signature
}

func TestRunApprovalLegacyPendingRemainsReadOnly(t *testing.T) {
	h := setupRecoveryHarness(t)
	r := approvalPending(t, h, "legacy-pending-read-only")
	v, err := h.Store.Approval(h.Ctx, r.TenantID, r.ID)
	if err != nil || v.Available || v.Status != "pending" {
		t.Fatal("schema 3 pending exposed approval capability", err)
	}
	_, err = h.Store.DecideApproval(h.Ctx, r.TenantID, r.ID, "synthetic-approver", "legacy-approve", agentrun.ApprovalRequest{SchemaVersion: 1, Decision: "approve", ProposalHash: v.ProposalHash})
	if !errors.Is(err, agentrun.ErrProfileUnavailable) {
		t.Fatal("legacy approval did not fail closed", err)
	}
	after, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	var authorizations int
	if err != nil || after.State != agentrun.AwaitingApproval || after.CursorVersion != r.CursorVersion {
		t.Fatal("legacy approval changed execution", err)
	}
	if err := h.Pool.QueryRow(h.Ctx, "select count(*) from action_authorizations where authorizing_run_id=$1", r.ID).Scan(&authorizations); err != nil || authorizations != 0 {
		t.Fatal("legacy approval persisted authorization", err)
	}
}

func TestRunApprovalFourActionAttemptsPreserveIdentityAndBudget(t *testing.T) {
	h := setupApprovalHarness(t)
	r := approvalPending(t, h, "four-action-attempts")
	approveRun(t, h, r, "approve")
	claimed := h.claim(t)
	first, err := h.Store.AuthorizeAction(h.Ctx, h.Principal, claimed.Lease, currentRunStep(claimed))
	if err != nil {
		t.Fatal(err)
	}
	before := ledgerView(t, h, claimed.Lease).Budget
	hash, _ := first.Action.Authorization.Hash()
	for attempt := 0; attempt < 4; attempt++ {
		step := currentRunStep(claimed)
		binding, err := h.Store.AuthorizeAction(h.Ctx, h.Principal, claimed.Lease, step)
		if err != nil || !approvalSameAction(first.Action, binding.Action) {
			t.Fatal("recovery changed operation/signature/content", err)
		}
		for _, kind := range []string{"receipt_query", "action_write"} {
			request := agentrun.ReserveActionCallRequest{Lease: claimed.Lease, Step: step, ID: uuid.NewString(), Kind: kind, OperationID: first.Action.Authorization.OperationID, AuthorizationHash: hash}
			permit, err := h.Store.ReserveActionCall(h.Ctx, h.Principal, request)
			if err != nil || !permit.NewlyReserved {
				t.Fatal("physical permit", attempt, kind, err)
			}
			replay, err := h.Store.ReserveActionCall(h.Ctx, h.Principal, request)
			if err != nil || replay.NewlyReserved {
				t.Fatal("physical permit replay", err)
			}
			request.ID = uuid.NewString()
			_, err = h.Store.ReserveActionCall(h.Ctx, h.Principal, request)
			want := agentrun.ErrCallConflict
			if attempt == 3 {
				want = agentrun.ErrBudgetExhausted
			}
			if !errors.Is(err, want) {
				t.Fatal("per-attempt/total physical bound", err, want)
			}
			// Deliberately leave every accepted permit uncertain. Closure does
			// not refund it or manufacture provider usage.
		}
		closed, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, step, "TIMEOUT")
		if err != nil {
			t.Fatal(err)
		}
		if attempt == 3 {
			if closed.State != agentrun.Failed || closed.RecoveryCount != 3 || closed.AttemptNo != 5 {
				t.Fatal("fourth action attempt did not exhaust three recoveries")
			}
		} else {
			ready := recoveryReady(t, h, r.ID, 6*time.Second)
			if ready.RecoveryCount != int64(attempt+1) {
				t.Fatal("action recovery count")
			}
			claimed = h.claim(t)
		}
	}
	calls, err := h.Store.ActionCalls(h.Ctx, r.TenantID, r.ID)
	if err != nil || len(calls.Items) != 8 {
		t.Fatal("4+4 physical bound", err)
	}
	for _, call := range calls.Items {
		if call.Status != "unknown" || call.OperationID != first.Action.Authorization.OperationID || call.ProviderMetering != "not_applicable" {
			t.Fatal("uncertain calls changed/refunded")
		}
	}
	after, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil || !reflect.DeepEqual(before, after.Budget) {
		t.Fatal("action recovery changed model accounts", err)
	}
	result, err := h.Store.Result(h.Ctx, r.TenantID, r.ID)
	if err != nil || result.Disposition != "unknown" {
		t.Fatal("exhaustion erased uncertain effect", err)
	}
}

func TestRunApprovalExpiredAuthorizationQueriesBeforeWriting(t *testing.T) {
	for _, found := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "found"}[found], func(t *testing.T) {
			h := setupApprovalHarness(t)
			r := approvalPending(t, h, "bounded-authorization")
			// Dedicated deterministic boundary fixture. It does not claim a
			// shortened lease/permission as natural process-fault evidence.
			var expiry time.Time
			if err := h.Pool.QueryRow(h.Ctx, `update runs set permission_expires_at=clock_timestamp()+interval '2 seconds'
				where run_id=$1 returning permission_expires_at`, r.ID).Scan(&expiry); err != nil {
				t.Fatal(err)
			}
			if _, err := h.Pool.Exec(h.Ctx, `update run_approvals set permission_expires_at=$2 where run_id=$1`, r.ID, expiry); err != nil {
				t.Fatal(err)
			}
			approveRun(t, h, r, "approve")
			claimed := h.claim(t)
			step := currentRunStep(claimed)
			binding, err := h.Store.AuthorizeAction(h.Ctx, h.Principal, claimed.Lease, step)
			if err != nil {
				t.Fatal(err)
			}
			hash, _ := binding.Action.Authorization.Hash()
			receipt := approvalSyntheticReceipt(*binding.Action)
			time.Sleep(time.Until(expiry) + 30*time.Millisecond)
			bindingAfter, err := h.Store.GetAction(h.Ctx, h.Principal, claimed.Lease, step)
			if err != nil || !approvalSameAction(binding.Action, bindingAfter.Action) {
				t.Fatal("expired identity unavailable/renewed", err)
			}
			query := agentrun.ReserveActionCallRequest{Lease: claimed.Lease, Step: step, ID: uuid.NewString(), Kind: "receipt_query", OperationID: binding.Action.Authorization.OperationID, AuthorizationHash: hash}
			if _, err := h.Store.ReserveActionCall(h.Ctx, h.Principal, query); err != nil {
				t.Fatal("expiry blocked receipt query", err)
			}
			write := query
			write.ID, write.Kind = uuid.NewString(), "action_write"
			if _, err := h.Store.ReserveActionCall(h.Ctx, h.Principal, write); !errors.Is(err, agentrun.ErrActionAuthorizationExpired) {
				t.Fatal("expired authorization obtained new write", err)
			}
			if found {
				if _, err := h.Store.ObserveActionCall(h.Ctx, h.Principal, agentrun.ObserveActionCallRequest{Lease: claimed.Lease, ID: query.ID, TransportOutcome: "response", ObservationHash: agentrun.Fingerprint("jobforge.run.action-receipt-observation.v1", receipt.ReceiptHash)}); err != nil {
					t.Fatal(err)
				}
				if _, err := h.Store.CompleteAction(h.Ctx, h.Principal, agentrun.CompleteActionRequest{Lease: claimed.Lease, Step: step, Receipt: receipt, PhysicalCallID: query.ID}); err != nil {
					t.Fatal("expired original receipt cannot complete", err)
				}
			} else if closed, err := h.Store.FailExecution(h.Ctx, h.Principal, claimed.Lease, step, "ACTION_AUTHORIZATION_EXPIRED"); err != nil || closed.State != agentrun.Failed {
				t.Fatal("expired unknown did not terminate", err)
			}
			calls, err := h.Store.ActionCalls(h.Ctx, r.TenantID, r.ID)
			if err != nil || len(calls.Items) != 1 || calls.Items[0].Kind != "receipt_query" {
				t.Fatal("expiry created a physical write", err)
			}
		})
	}
}

type approvalCancelBarrier struct {
	once    sync.Once
	entered chan uint32
	release chan struct{}
}

type approvalCancelBarrierKey struct{}

func (*approvalCancelBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "from runs") && strings.HasSuffix(data.SQL, "for update") {
		return context.WithValue(ctx, approvalCancelBarrierKey{}, true)
	}
	return ctx
}

func (b *approvalCancelBarrier) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if marked, _ := ctx.Value(approvalCancelBarrierKey{}).(bool); !marked || data.Err != nil {
		return
	}
	b.once.Do(func() {
		b.entered <- conn.PgConn().PID()
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	})
}

func TestRunApprovalCancelWinsConcurrentAuthorization(t *testing.T) {
	h := setupApprovalHarness(t)
	r := approvalPending(t, h, "cancel-authorize-race")
	approveRun(t, h, r, "approve")
	claimed := h.claim(t)
	ctx, cancel := context.WithTimeout(h.Ctx, 6*time.Second)
	defer cancel()
	barrier := &approvalCancelBarrier{entered: make(chan uint32, 1), release: make(chan struct{})}
	config := h.Pool.Config().Copy()
	config.ConnConfig.Tracer = barrier
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	store, err := runpostgres.New(pool, h.Options)
	if err != nil {
		t.Fatal(err)
	}
	service, err := agentrun.NewService(store, h.Capture, []string{"tenant-north", "tenant-south"})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, authorized := make(chan error, 1), make(chan error, 1)
	go func() { _, err := service.Cancel(ctx, r.TenantID, r.ID, "actual-concurrent-cancel"); cancelled <- err }()
	var pid uint32
	select {
	case pid = <-barrier.entered:
	case <-ctx.Done():
		t.Fatal("cancel did not hold its actual Run transaction")
	}
	go func() {
		_, err := h.Store.AuthorizeAction(ctx, h.Principal, claimed.Lease, currentRunStep(claimed))
		authorized <- err
	}()
	for {
		var blocked bool
		if err := h.Pool.QueryRow(ctx, `select exists(select 1 from pg_stat_activity where $1=any(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("authorization did not compete for cancellation's Run lock")
		}
	}
	close(barrier.release)
	if err := <-cancelled; err != nil {
		t.Fatal(err)
	}
	if err := <-authorized; !errors.Is(err, agentrun.ErrCancelRequested) {
		t.Fatal("cancel-first authorization", err)
	}
	var count int
	if err := h.Pool.QueryRow(ctx, `select count(*) from action_authorizations where authorizing_run_id=$1`, r.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("cancel-first persisted authority", err)
	}
}

func TestRunApprovalAuthorizedRetryWindowAndAcceptedReplay(t *testing.T) {
	h := setupApprovalHarness(t)
	r, _, _ := approvalAuthorizedCancelled(t, h, "authorized-window")
	var queries atomic.Int32
	reader := approvalReceiptReader(func(context.Context, string, agentrun.Profile, business.SignedAction) (business.ActionReceipt, bool, error) {
		queries.Add(1)
		return business.ActionReceipt{}, false, nil
	})
	service, err := agentrun.NewService(h.Store, h.Capture, []string{"tenant-north", "tenant-south"}, agentrun.WithReceiptReader(reader))
	if err != nil {
		t.Fatal(err)
	}
	request := agentrun.RetryRequest{SchemaVersion: 1, RunTimeoutSeconds: 600}
	child, err := service.Retry(h.Ctx, r.TenantID, r.ID, "accepted-retry-window", request)
	if err != nil || child.Run.Error == nil || child.Run.Error.Code != "ACTION_OUTCOME_UNKNOWN" {
		t.Fatal("accepted unknown successor", err)
	}
	// DB-time boundary fixture: set the original seven-day window to equality
	// with this statement's fresh clock. Subsequent fresh time is >= its bound.
	if _, err := h.Pool.Exec(h.Ctx, `with moment as (select clock_timestamp() as instant)
		update business_requests set created_at=moment.instant-interval '7 days',retry_until=moment.instant
		from moment where business_request_id=$1`, r.BusinessRequestID); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"accepted-retry-window", "accepted-retry-alias"} {
		replay, err := service.Retry(h.Ctx, r.TenantID, r.ID, key, request)
		if err != nil || !replay.Reused || replay.Run.ID != child.Run.ID {
			t.Fatal("accepted retry expired or queried again", err)
		}
	}
	if _, err := service.Retry(h.Ctx, r.TenantID, child.Run.ID, "new-after-window", request); !errors.Is(err, agentrun.ErrInvalidTransition) {
		t.Fatal("new successor survived original window", err)
	}
	if queries.Load() != 1 {
		t.Fatal("window/replay sent another receipt GET")
	}
}
