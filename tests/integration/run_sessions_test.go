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
