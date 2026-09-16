package integration

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

// runRPCObservationAfterLock establishes an actual PostgreSQL lock wait before
// sampling its clock. This rejects pre-lock or transport-generated observations.
func runRPCObservationAfterLock(ctx context.Context, t *testing.T, h *runHarness, query string, args []any,
	invoke func(context.Context) (*timestamppb.Timestamp, error)) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(h.Ctx) }()
	var blocker int32
	if err := tx.QueryRow(ctx, "select pg_backend_pid()").Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
	type response struct {
		stamp *timestamppb.Timestamp
		err   error
	}
	finished := make(chan response, 1)
	go func() {
		stamp, err := invoke(ctx)
		finished <- response{stamp: stamp, err: err}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := h.Pool.QueryRow(ctx, `select exists(select 1 from pg_stat_activity
			where datname=current_database() and $1=any(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case result := <-finished:
			t.Fatalf("authority RPC bypassed the required lock: %v", result.err)
		case <-ctx.Done():
			t.Fatal("authority RPC did not reach its PostgreSQL lock")
		case <-ticker.C:
		}
	}
	var lower, upper time.Time
	if err := h.Pool.QueryRow(ctx, "select clock_timestamp()").Scan(&lower); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if result.err != nil || result.stamp == nil || result.stamp.CheckValid() != nil {
			t.Fatalf("authority RPC omitted a valid observation: %v", result.err)
		}
		if err := h.Pool.QueryRow(ctx, "select clock_timestamp()").Scan(&upper); err != nil {
			t.Fatal(err)
		}
		observed := result.stamp.AsTime()
		if observed.Before(lower) || observed.After(upper) {
			t.Fatalf("observation %s is outside locked DB interval [%s, %s]", observed, lower, upper)
		}
		return observed
	case <-ctx.Done():
		t.Fatal("authority RPC did not finish after lock release")
		return time.Time{}
	}
}

func TestRunWorkerRPCAuthorityObservationsUseLockedDatabaseClock(t *testing.T) {
	h := setupRunHarness(t)
	client := startRunGateway(t, h)
	ctx := metadata.AppendToOutgoingContext(h.Ctx, "authorization", "Bearer contract-worker-token")
	secondCtx := metadata.AppendToOutgoingContext(h.Ctx, "authorization", "Bearer contract-second-worker-token")
	var registered *agentv1.RegisterResponse
	observed := runRPCObservationAfterLock(secondCtx, t, h,
		"select pg_advisory_xact_lock(hashtextextended($1,18))", []any{"contract-worker-2"},
		func(ctx context.Context) (*timestamppb.Timestamp, error) {
			var err error
			registered, err = client.Register(ctx, &agentv1.RegisterRequest{StartupId: uuid.NewString(), Version: h.Profile.ExecutorVersion})
			return registered.GetAuthorityObservedAt(), err
		})
	if registered.ExpiresAt.AsTime().Sub(observed) != 60*time.Second {
		t.Fatal("registration expiry is not anchored to its DB authority observation")
	}

	var replay *agentv1.RegisterResponse
	observed = runRPCObservationAfterLock(ctx, t, h,
		"select pg_advisory_xact_lock(hashtextextended($1,18))", []any{h.Principal},
		func(ctx context.Context) (*timestamppb.Timestamp, error) {
			var err error
			replay, err = client.Register(ctx, &agentv1.RegisterRequest{StartupId: h.Session.StartupID, Version: h.Session.Version})
			return replay.GetAuthorityObservedAt(), err
		})
	if replay.Session.SessionId != h.Session.ID || !replay.ExpiresAt.AsTime().Equal(h.Session.ExpiresAt) ||
		!observed.After(h.Session.AuthorityObservedAt) {
		t.Fatal("startup replay renewed its authority or reused its stale observation")
	}

	session := &agentv1.SessionIdentity{WorkerId: h.Principal, SessionId: h.Session.ID}
	var idle *agentv1.HeartbeatResponse
	observed = runRPCObservationAfterLock(ctx, t, h,
		"select session_id from worker_sessions where session_id=$1 for update", []any{h.Session.ID},
		func(ctx context.Context) (*timestamppb.Timestamp, error) {
			var err error
			idle, err = client.Heartbeat(ctx, &agentv1.HeartbeatRequest{Session: session})
			return idle.GetAuthorityObservedAt(), err
		})
	if idle.LeaseUntil != nil || idle.Signal != agentv1.ControlSignal_CONTROL_SIGNAL_CONTINUE ||
		idle.SessionExpiresAt.AsTime().Sub(observed) != 60*time.Second {
		t.Fatal("idle heartbeat invented execution authority or lost its DB time")
	}
	empty, err := client.Claim(ctx, &agentv1.ClaimRequest{Session: session})
	if err != nil || empty.Lease != nil {
		t.Fatalf("empty Claim unexpectedly granted authority: %v", err)
	}
	var unchangedExpiry time.Time
	if err := h.Pool.QueryRow(h.Ctx, "select expires_at from worker_sessions where session_id=$1", h.Session.ID).Scan(&unchangedExpiry); err != nil {
		t.Fatal(err)
	}
	if !unchangedExpiry.Equal(idle.SessionExpiresAt.AsTime()) {
		t.Fatal("empty Claim renewed session liveness")
	}

	r := h.submit(t, "tenant-a", "authority-clock")
	var claimed *agentv1.ClaimResponse
	observed = runRPCObservationAfterLock(ctx, t, h,
		"select account_id from budget_accounts where scope='batch' for update", nil,
		func(ctx context.Context) (*timestamppb.Timestamp, error) {
			var err error
			claimed, err = client.Claim(ctx, &agentv1.ClaimRequest{Session: session})
			return claimed.GetLease().GetAuthorityObservedAt(), err
		})
	lease := claimed.Lease
	var started time.Time
	if err := h.Pool.QueryRow(h.Ctx, "select started_at from run_attempts where run_id=$1 and attempt_no=1", r.ID).Scan(&started); err != nil {
		t.Fatal(err)
	}
	if !observed.Equal(started) || lease.LeaseUntil.AsTime().Sub(observed) != 30*time.Second ||
		lease.AttemptDeadline.AsTime().Sub(observed) != 180*time.Second {
		t.Fatal("Claim observation is not the atomic grant time")
	}

	var active *agentv1.HeartbeatResponse
	observed = runRPCObservationAfterLock(ctx, t, h,
		"select run_id from runs where run_id=$1 for update", []any{r.ID},
		func(ctx context.Context) (*timestamppb.Timestamp, error) {
			var err error
			active, err = client.Heartbeat(ctx, &agentv1.HeartbeatRequest{Session: session, Execution: lease.Execution})
			return active.GetAuthorityObservedAt(), err
		})
	if active.Signal != agentv1.ControlSignal_CONTROL_SIGNAL_CONTINUE || active.LeaseUntil.AsTime().Sub(observed) != 30*time.Second ||
		active.SessionExpiresAt.AsTime().Sub(observed) != 60*time.Second {
		t.Fatal("active heartbeat expiries lost their DB authority anchor")
	}
	if _, err := h.Service.Cancel(h.Ctx, r.TenantID, r.ID, "authority-stop"); err != nil {
		t.Fatal(err)
	}
	var stopped *agentv1.HeartbeatResponse
	runRPCObservationAfterLock(ctx, t, h,
		"select run_id from runs where run_id=$1 for update", []any{r.ID},
		func(ctx context.Context) (*timestamppb.Timestamp, error) {
			var err error
			stopped, err = client.Heartbeat(ctx, &agentv1.HeartbeatRequest{Session: session, Execution: lease.Execution})
			return stopped.GetAuthorityObservedAt(), err
		})
	if stopped.Signal != agentv1.ControlSignal_CONTROL_SIGNAL_STOP || stopped.StopReason != agentrun.StopCancel ||
		!stopped.LeaseUntil.AsTime().Equal(active.LeaseUntil.AsTime()) {
		t.Fatal("stopping heartbeat renewed execution authority")
	}
	if err := h.Pool.QueryRow(h.Ctx, `update worker_sessions set seen_at=clock_timestamp()-interval '2 seconds',
		expires_at=clock_timestamp()-interval '1 second' where session_id=$1 returning expires_at`, h.Session.ID).Scan(&unchangedExpiry); err != nil {
		t.Fatal(err)
	}
	observed = runRPCObservationAfterLock(ctx, t, h,
		"select run_id from runs where run_id=$1 for update", []any{r.ID},
		func(ctx context.Context) (*timestamppb.Timestamp, error) {
			var err error
			stopped, err = client.Heartbeat(ctx, &agentv1.HeartbeatRequest{Session: session, Execution: lease.Execution})
			return stopped.GetAuthorityObservedAt(), err
		})
	if stopped.Signal != agentv1.ControlSignal_CONTROL_SIGNAL_STOP || !observed.After(unchangedExpiry) ||
		!stopped.SessionExpiresAt.AsTime().Equal(unchangedExpiry) || !stopped.LeaseUntil.AsTime().Equal(active.LeaseUntil.AsTime()) {
		t.Fatal("expired stopping session was revived or lost its fresh observation")
	}
}

func TestRunWorkerRPCReportsLateMeasurementAnomalyWithoutRefund(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "rpc-usage-anomaly", "model_proposal")
	client := startRunGateway(t, h)
	ctx := metadata.AppendToOutgoingContext(h.Ctx, "authorization", "Bearer contract-worker-token")
	execution := &agentv1.ExecutionIdentity{TenantId: claimed.Lease.TenantID, RunId: claimed.Lease.RunID,
		Session:   &agentv1.SessionIdentity{WorkerId: h.Principal, SessionId: h.Session.ID},
		AttemptNo: claimed.Lease.AttemptNo, FencingToken: claimed.Lease.FencingToken}
	checkpoint, err := client.GetCheckpoint(ctx, &agentv1.GetCheckpointRequest{Execution: execution})
	if err != nil {
		t.Fatal(err)
	}
	request := &agentv1.ReserveCallRequest{Execution: execution, Step: checkpoint.Checkpoint.NextStep,
		PhysicalCallId: uuid.NewString(), Subcall: agentv1.Subcall_SUBCALL_CHAT,
		ParameterHash: agentrun.Fingerprint("synthetic-anomalous-response"), PriceHash: h.Profile.Pricing.Hash}
	reserved, err := client.ReserveCall(ctx, request)
	if err != nil || !reserved.NewlyReserved || reserved.Reservation.MeasurementAnomaly || reserved.Reservation.UsageKnown {
		t.Fatalf("new reservation has incorrect metering flags: %v", err)
	}
	before := ledgerView(t, h, claimed.Lease)
	if _, err := h.Service.Cancel(h.Ctx, claimed.Lease.TenantID, claimed.Lease.RunID, "rpc-anomaly-cancel"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AcknowledgeStopped(ctx, &agentv1.AcknowledgeStoppedRequest{Execution: execution}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(h.Ctx, `update worker_sessions set seen_at=clock_timestamp()-interval '2 seconds',
		expires_at=clock_timestamp()-interval '1 second' where session_id=$1`, h.Session.ID); err != nil {
		t.Fatal(err)
	}
	// More than 1024 output tokens is legal metering, not a legal model result.
	// The expired original session may retain this report but cannot execute.
	usage := ledgerUsage(h.Profile.MaxInputTokens+1, 1025)
	settle := &agentv1.SettleUsageRequest{Execution: execution, PhysicalCallId: request.PhysicalCallId,
		Usage: &agentv1.UsageReport{InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
			CachedInputTokens: usage.CachedInputTokens, ReceiptHash: usage.ReceiptHash, UsageHash: usage.UsageHash}}
	for range 2 {
		response, err := client.SettleUsage(ctx, settle)
		if err != nil || !response.Reservation.MeasurementAnomaly || response.Reservation.UsageKnown || response.NewlySettled {
			t.Fatalf("late/repeated anomaly was not explicitly reported: %v", err)
		}
	}
	after := ledgerView(t, h, claimed.Lease)
	priorAccounts := ledgerAccounts(before)
	for i, account := range ledgerAccounts(after) {
		prior := priorAccounts[i]
		if !account.Frozen || account.Used != prior.Used || account.HeldTokens != prior.HeldTokens || account.HeldCostMicroyuan != prior.HeldCostMicroyuan {
			t.Fatal("anomalous usage refunded a hold or failed to freeze an account")
		}
	}
	if after.State != agentrun.Cancelled || after.CursorVersion != before.CursorVersion || after.LeaseUntil != nil {
		t.Fatal("late anomaly changed terminal execution state")
	}
	usage.OutputTokens++
	usage.UsageHash = usage.Hash()
	settle.Usage.OutputTokens, settle.Usage.UsageHash = usage.OutputTokens, usage.UsageHash
	_, err = client.SettleUsage(ctx, settle)
	assertRunRPCError(t, err, codes.FailedPrecondition, "CALL_CONFLICT")
}
