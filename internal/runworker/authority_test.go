package runworker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

var testDatabaseTime = time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)

func keeperFixture(t *testing.T) (*leaseKeeper, *atomic.Int64, context.Context) {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(1000)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	k, err := newLeaseKeeper(lifecycleLease(), 61000, 1000, 1000,
		func() (int64, error) { return clock.Load(), nil }, cancel)
	if err != nil {
		t.Fatal(err)
	}
	return k, clock, ctx
}

func continuingHeartbeat() *agentv1.HeartbeatResponse {
	observed := testDatabaseTime.Add(5 * time.Second)
	return &agentv1.HeartbeatResponse{Signal: agentv1.ControlSignal_CONTROL_SIGNAL_CONTINUE,
		AuthorityObservedAt: timestamppb.New(observed), LeaseUntil: timestamppb.New(observed.Add(30 * time.Second)),
		SessionExpiresAt: timestamppb.New(observed.Add(60 * time.Second))}
}

func TestLeaseRenewalKeepsFixedDeadline(t *testing.T) {
	k, clock, _ := keeperFixture(t)
	clock.Store(6000)
	if err := k.heartbeat(6000, 6000, continuingHeartbeat()); err != nil {
		t.Fatal(err)
	}
	if k.leaseDeadline != 36000 || k.sessionExpiry() != 66000 || k.StepDeadline() != 181000 {
		t.Fatal("renewal changed the wrong deadlines")
	}
	clock.Store(35000)
	if err := k.Check(); err != nil {
		t.Fatal("renewed lease did not remain usable")
	}
	clock.Store(36000)
	if err := k.Check(); !errors.Is(err, ErrAuthority) {
		t.Fatal("lease equality must revoke authority")
	}
}

func TestDelayedHeartbeatCannotReviveOldAuthority(t *testing.T) {
	for _, scenario := range []string{"old lease expired", "explicit stop", "processing queue crossed expiry", "fixed deadline expired"} {
		t.Run(scenario, func(t *testing.T) {
			k, clock, ctx := keeperFixture(t)
			received := int64(6000)
			switch scenario {
			case "old lease expired":
				received = 31000
				clock.Store(received)
			case "explicit stop":
				k.Stop("STALE_LEASE")
			case "processing queue crossed expiry":
				clock.Store(31000)
			case "fixed deadline expired":
				k.stepDeadline = 6000
				clock.Store(6000)
			}
			if err := k.heartbeat(6000, received, continuingHeartbeat()); !errors.Is(err, ErrAuthority) {
				t.Fatal("late renewal restored authority")
			}
			if k.leaseDeadline != 31000 || ctx.Err() == nil || k.stoppedByServer() {
				t.Fatal("revocation changed deadlines or invented server STOP")
			}
		})
	}
}

func TestAuthorityMappingRejectsMissingAndOverlongTimestamps(t *testing.T) {
	for _, modify := range []func(*agentv1.RunLease){
		func(l *agentv1.RunLease) { l.AuthorityObservedAt = nil },
		func(l *agentv1.RunLease) { l.LeaseUntil = timestamppb.New(testDatabaseTime.Add(31 * time.Second)) },
		func(l *agentv1.RunLease) {
			l.AttemptDeadline = timestamppb.New(testDatabaseTime.Add(181 * time.Second))
		},
		func(l *agentv1.RunLease) { l.RunDeadline = timestamppb.New(testDatabaseTime.Add(25 * time.Hour)) },
	} {
		lease := lifecycleLease()
		modify(lease)
		if _, err := newLeaseKeeper(lease, 61000, 1000, 1000, func() (int64, error) { return 1000, nil }, func() {}); err == nil {
			t.Fatal("accepted invalid authority sample")
		}
	}
}

func TestAuthorityWatchdogAndConcurrentStop(t *testing.T) {
	k, clock, step := keeperFixture(t)
	watchContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	joined := make(chan struct{})
	go func() { defer close(joined); k.watch(watchContext) }()
	clock.Store(31000)
	select {
	case <-step.Done():
	case <-time.After(time.Second):
		t.Fatal("independent watchdog failed to revoke expired lease")
	}
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("watchdog did not join")
	}
	var calls sync.WaitGroup
	for range 20 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			k.Stop("STOP_REQUESTED")
			_ = k.Check()
			_ = k.sessionExpiry()
			_ = k.heartbeat(6000, 6000, continuingHeartbeat())
		}()
	}
	calls.Wait()
	if k.Check() == nil {
		t.Fatal("concurrent callback revived authority")
	}
}

func TestOnlyExplicitServerSignalPermitsStopAcknowledgement(t *testing.T) {
	k, _, _ := keeperFixture(t)
	k.Stop("STOP_REQUESTED")
	if k.stoppedByServer() {
		t.Fatal("local reason was treated as server STOP")
	}
	response := continuingHeartbeat()
	response.Signal, response.StopReason = agentv1.ControlSignal_CONTROL_SIGNAL_STOP, run.StopCancel
	_ = k.heartbeat(6000, 6000, response)
	if !k.stoppedByServer() || k.Check() == nil {
		t.Fatal("server STOP must permit acknowledgement without restoring execution")
	}
}
