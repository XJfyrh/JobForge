package runworker

import (
	"context"
	"sync"
	"time"

	"github.com/xjfyrh/jobforge/internal/run"
	agentv1 "github.com/xjfyrh/jobforge/proto/jobforge/agent/v1"
)

type clockSample func() (int64, error)

// leaseKeeper is local revocation, never a substitute for database fencing.
// The mutex serializes renewal with irreversible stop and expiration. Each
// keeper belongs to exactly one Claim, so late callbacks cannot reach a new one.
type leaseKeeper struct {
	mu              sync.Mutex
	now             clockSample
	cancel          context.CancelFunc
	leaseDeadline   int64
	sessionDeadline int64
	stepDeadline    int64
	stopped         bool
	stopReason      string
	serverStop      bool
}

func newLeaseKeeper(lease *agentv1.RunLease, sessionDeadline, start, received int64, now clockSample, cancel context.CancelFunc) (*leaseKeeper, error) {
	if lease == nil || sessionDeadline <= received || now == nil || cancel == nil {
		return nil, ErrAuthority
	}
	leaseDeadline, err := stampDeadline(start, received, lease.AuthorityObservedAt, lease.LeaseUntil, 30*time.Second)
	if err != nil {
		return nil, err
	}
	attemptDeadline, err := stampDeadline(start, received, lease.AuthorityObservedAt, lease.AttemptDeadline, 180*time.Second)
	if err != nil {
		return nil, err
	}
	runDeadline, err := stampDeadline(start, received, lease.AuthorityObservedAt, lease.RunDeadline, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	return &leaseKeeper{now: now, cancel: cancel, leaseDeadline: leaseDeadline, sessionDeadline: sessionDeadline,
		stepDeadline: min(attemptDeadline, runDeadline)}, nil
}

func (k *leaseKeeper) Check() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.checkLocked()
}

func (k *leaseKeeper) checkLocked() error {
	if k.stopped {
		return ErrAuthority
	}
	now, err := k.now()
	if err != nil || now < 0 || now >= min(k.leaseDeadline, k.sessionDeadline, k.stepDeadline) {
		k.stopLocked("STALE_LEASE")
		return ErrAuthority
	}
	return nil
}

func (k *leaseKeeper) StepDeadline() int64 { return k.stepDeadline }

func (k *leaseKeeper) Stop(reason string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopLocked(reason)
}

func (k *leaseKeeper) stopLocked(reason string) {
	if !k.stopped {
		k.stopped, k.stopReason = true, reason
		k.cancel()
	}
}

func (k *leaseKeeper) stoppedByServer() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.serverStop
}

func (k *leaseKeeper) sessionExpiry() int64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.sessionDeadline
}

func (k *leaseKeeper) heartbeat(start, received int64, response *agentv1.HeartbeatResponse) error {
	if response == nil {
		k.Stop("STALE_LEASE")
		return ErrAuthority
	}
	if response.Signal == agentv1.ControlSignal_CONTROL_SIGNAL_STOP {
		if response.StopReason != run.StopCancel && response.StopReason != run.StopRunDeadline && response.StopReason != run.StopAttemptTimeout {
			k.Stop("STALE_LEASE")
			return ErrAuthority
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		// A real STOP remains acknowledgement evidence even if local expiration
		// already revoked execution. No deadline or send right is restored.
		k.serverStop = true
		k.stopLocked(response.StopReason)
		return ErrAuthority
	}
	leaseDeadline, leaseErr := stampDeadline(start, received, response.AuthorityObservedAt, response.LeaseUntil, 30*time.Second)
	sessionDeadline, sessionErr := stampDeadline(start, received, response.AuthorityObservedAt, response.SessionExpiresAt, 60*time.Second)
	if response.Signal != agentv1.ControlSignal_CONTROL_SIGNAL_CONTINUE || response.StopReason != "" || leaseErr != nil || sessionErr != nil {
		k.Stop("STALE_LEASE")
		return ErrAuthority
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	// Recheck against OLD authority at actual handling time. An RPC arriving
	// after the old lease expired cannot revive this attempt's local rights.
	if err := k.checkLocked(); err != nil {
		return err
	}
	if received >= min(k.leaseDeadline, k.sessionDeadline, k.stepDeadline) {
		k.stopLocked("STALE_LEASE")
		return ErrAuthority
	}
	k.leaseDeadline, k.sessionDeadline = leaseDeadline, sessionDeadline
	return nil
}

func (k *leaseKeeper) watch(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			k.Stop("STOP_REQUESTED")
			return
		case <-ticker.C:
			if k.Check() != nil {
				return
			}
		}
	}
}
