package integration

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	agentrun "github.com/xjfyrh/jobforge/internal/run"
	runpostgres "github.com/xjfyrh/jobforge/internal/run/postgres"
)

func TestRunSessionBindsStartupVersionAndRejectsLiveReplacement(t *testing.T) {
	ctx, pool := setupRunDB(t)
	store, err := runpostgres.New(pool, runpostgres.Options{Workers: []agentrun.WorkerConfig{
		{ID: "worker-session-test", Tenants: []string{"tenant-session-test"}, Capacity: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	startup := uuid.NewString()
	first, err := store.Register(ctx, "worker-session-test", startup, "v1")
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.Register(ctx, "worker-session-test", startup, "v1")
	if err != nil || repeated.ID != first.ID || !repeated.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("registration replay changed the accepted session: %v", err)
	}
	if _, err := store.Register(ctx, "worker-session-test", startup, "v2"); !errors.Is(err, agentrun.ErrConflict) {
		t.Fatalf("same startup changed its version: %v", err)
	}
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			if _, err := store.Register(ctx, "worker-session-test", uuid.NewString(), "v1"); !errors.Is(err, agentrun.ErrConflict) {
				t.Errorf("concurrent startup replaced a live session: %v", err)
			}
		})
	}
	wait.Wait()
	if _, err := store.Register(ctx, "unconfigured-worker", uuid.NewString(), "v1"); !errors.Is(err, agentrun.ErrForbidden) {
		t.Fatalf("body identity created an unconfigured principal: %v", err)
	}
	if _, err := store.HeartbeatSession(ctx, "another-principal", first.WorkerID, first.ID); !errors.Is(err, agentrun.ErrUnauthorized) {
		t.Fatalf("wrong principal renewed a session: %v", err)
	}
	renewed, err := store.HeartbeatSession(ctx, first.WorkerID, first.WorkerID, first.ID)
	if err != nil || !renewed.ExpiresAt.After(first.ExpiresAt) || renewed.ExpiresAt.Sub(renewed.SeenAt).Seconds() != 60 {
		t.Fatalf("session liveness renewal: %v", err)
	}
	// This owned test database controls time facts; no demonstration data is used.
	if _, err := pool.Exec(ctx, `update worker_sessions set seen_at=clock_timestamp()-interval '2 seconds',
		expires_at=clock_timestamp()-interval '1 second' where session_id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register(ctx, first.WorkerID, startup, "v1"); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("expired startup resurrected its session: %v", err)
	}
	if _, err := store.HeartbeatSession(ctx, first.WorkerID, first.WorkerID, first.ID); !errors.Is(err, agentrun.ErrStaleLease) {
		t.Fatalf("expired heartbeat resurrected liveness: %v", err)
	}
	second, err := store.Register(ctx, first.WorkerID, uuid.NewString(), "v1")
	if err != nil || second.ID == first.ID {
		t.Fatalf("new startup after expiry: %v", err)
	}
}

func TestRunExecutorVersionRejectsRegistrationAndOldClaim(t *testing.T) {
	h := setupRunHarness(t)
	if _, err := h.Store.Register(h.Ctx, "contract-worker-2", uuid.NewString(), "old-no-observation-ack"); !errors.Is(err, agentrun.ErrProfileUnavailable) {
		t.Fatalf("wrong executor registered: %v", err)
	}
	var count int
	if err := h.Pool.QueryRow(h.Ctx, "select count(*) from worker_sessions where worker_id='contract-worker-2'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected registration left a session: count=%d error=%v", count, err)
	}
	r := h.submit(t, "tenant-a", "executor-version-claim")
	// Simulate a session created before version enforcement. Checking only
	// Register would leave this original persisted session able to Claim.
	if _, err := h.Pool.Exec(h.Ctx, "update worker_sessions set version='old-no-observation-ack' where session_id=$1", h.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.Claim(h.Ctx, h.Principal, h.Session.ID); !errors.Is(err, agentrun.ErrProfileUnavailable) {
		t.Fatalf("old session acquired execution: %v", err)
	}
	view, err := h.Store.Get(h.Ctx, r.TenantID, r.ID)
	if err != nil || view.State != agentrun.Ready || view.AttemptNo != 0 {
		t.Fatalf("rejected Claim changed Run: state=%s attempt=%d error=%v", view.State, view.AttemptNo, err)
	}
}

func TestRunExecutorVersionBlocksExecutionButPreservesOriginalMetering(t *testing.T) {
	h := setupRunHarness(t)
	claimed := ledgerAtStep(t, h, "tenant-a", "executor-version-metering", "model_proposal")
	request := ledgerRequest(h, claimed, agentrun.SubcallChat, "")
	ledgerReserve(t, h, request)
	if _, err := h.Pool.Exec(h.Ctx, "update worker_sessions set version='old-no-observation-ack' where session_id=$1", h.Session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Store.ReserveCall(h.Ctx, h.Principal, request); !errors.Is(err, agentrun.ErrProfileUnavailable) {
		t.Fatalf("old execution repeated a reservation: %v", err)
	}
	usage := ledgerUsage(20, 10)
	settled, err := h.Store.SettleUsage(h.Ctx, h.Principal, agentrun.SettleUsageRequest{
		Lease: claimed.Lease, PhysicalCallID: request.PhysicalCallID, Usage: &usage,
	})
	if err != nil || !settled.NewlySettled || !settled.Reservation.UsageKnown {
		t.Fatalf("version mismatch destroyed narrow original-call accounting: %v", err)
	}
	view := ledgerView(t, h, claimed.Lease)
	if view.CursorVersion != claimed.Checkpoint.Run.CursorVersion || view.State != agentrun.Running {
		t.Fatal("late accounting acquired execution or advanced the cursor")
	}
}
