// Package scheduler implements the JobForge job promotion and lease recovery
// loop. It scans for scheduled/retry_wait jobs whose run_at has arrived and
// promotes them to ready, and recovers jobs whose leases have expired.
//
// The Scheduler uses a PostgreSQL advisory lock for single-active election.
// Only the lock holder performs scans; standby instances poll for lock
// availability. Event-driven wakeup via pg_notify supplements the periodic
// scan (ADR-0003).
package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// Store defines the persistence operations required by the Scheduler.
// Following Go convention, the interface is defined at the consumer side.
type Store interface {
	// TryAcquireLock attempts to acquire the scheduler advisory lock.
	// Returns true if this instance is now the leader.
	TryAcquireLock(ctx context.Context) (bool, error)

	// ReleaseLock releases the scheduler advisory lock.
	ReleaseLock(ctx context.Context) error

	// PromoteReady transitions scheduled/retry_wait jobs to ready.
	// Returns the number of jobs promoted.
	PromoteReady(ctx context.Context, batchSize int) (int, error)

	// RecoverExpiredLeases recovers running jobs with expired leases
	// (back to ready) and cancelling jobs with expired leases (to cancelled).
	// Returns the total number of jobs recovered.
	RecoverExpiredLeases(ctx context.Context) (int, error)
}

// Notifier sends pg_notify signals to wake up waiting consumers.
type Notifier interface {
	// NotifyJobReady sends a notification that new jobs may be available
	// in the given queue.
	NotifyJobReady(ctx context.Context, queue string) error
}

// Listener provides event-driven wakeup for the Scheduler scan loop.
type Listener interface {
	// WaitForNotification blocks until a notification is received or the
	// context is cancelled. Returns true if a notification was received.
	WaitForNotification(ctx context.Context) bool
}

// Config holds Scheduler tuning parameters.
type Config struct {
	// ScanInterval is the fallback polling period when no NOTIFY is received.
	ScanInterval time.Duration

	// PromoteBatchSize limits how many jobs are promoted per scan cycle.
	PromoteBatchSize int

	// LockRetryInterval is how often standby instances try to acquire the lock.
	LockRetryInterval time.Duration
}

// DefaultConfig returns production-default Scheduler configuration.
func DefaultConfig() Config {
	return Config{
		ScanInterval:      1 * time.Second,
		PromoteBatchSize:  1000,
		LockRetryInterval: 2 * time.Second,
	}
}

// Scheduler runs the promotion and recovery loop. It is safe to run multiple
// instances concurrently; only the advisory lock holder performs work.
type Scheduler struct {
	store    Store
	notifier Notifier
	listener Listener
	cfg      Config
	logger   *slog.Logger
}

// New creates a Scheduler with the given dependencies.
func New(store Store, notifier Notifier, listener Listener, cfg Config, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		store:    store,
		notifier: notifier,
		listener: listener,
		cfg:      cfg,
		logger:   logger,
	}
}

// Run starts the Scheduler main loop. It blocks until ctx is cancelled.
// The loop: acquire lock → scan → promote → recover → wait (notify or timer).
func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Info("scheduler starting",
		"scan_interval", s.cfg.ScanInterval,
		"batch_size", s.cfg.PromoteBatchSize,
	)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		acquired, err := s.store.TryAcquireLock(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Error("failed to acquire advisory lock", "error", err)
			s.sleep(ctx, s.cfg.LockRetryInterval)
			continue
		}

		if !acquired {
			// Standby mode: wait and retry.
			s.logger.Debug("scheduler standby: lock held by another instance")
			s.sleep(ctx, s.cfg.LockRetryInterval)
			continue
		}

		s.logger.Info("scheduler acquired leadership")
		s.runLoop(ctx)

		// Lost leadership (ctx cancelled or lock released).
		_ = s.store.ReleaseLock(ctx)
		s.logger.Info("scheduler released leadership")

		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// runLoop performs scan cycles until context is cancelled.
func (s *Scheduler) runLoop(ctx context.Context) {
	// Perform an immediate scan on startup.
	s.scanCycle(ctx)

	timer := time.NewTimer(s.cfg.ScanInterval)
	defer timer.Stop()

	for {
		// Wait for either: notification, timer, or context cancellation.
		var notifyCtx context.Context
		var notifyCancel context.CancelFunc

		// Use the shorter of scan interval for the listener timeout.
		notifyCtx, notifyCancel = context.WithTimeout(ctx, s.cfg.ScanInterval)

		notified := s.listener.WaitForNotification(notifyCtx)
		notifyCancel()

		if ctx.Err() != nil {
			return
		}

		if notified {
			// Event-driven: scan immediately.
			s.scanCycle(ctx)
			// Reset the timer since we just scanned.
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.cfg.ScanInterval)
		} else {
			// Timer or timeout: check if we should scan.
			select {
			case <-timer.C:
				s.scanCycle(ctx)
				timer.Reset(s.cfg.ScanInterval)
			case <-ctx.Done():
				return
			}
		}
	}
}

// scanCycle performs one promote + recover cycle.
func (s *Scheduler) scanCycle(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}

	// Promote scheduled/retry_wait → ready.
	promoted, err := s.store.PromoteReady(ctx, s.cfg.PromoteBatchSize)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Error("promote failed", "error", err)
	} else if promoted > 0 {
		s.logger.Info("promoted jobs to ready", "count", promoted)
		// Notify gateway that new jobs are available.
		if s.notifier != nil {
			_ = s.notifier.NotifyJobReady(ctx, "")
		}
	}

	// Recover expired leases.
	recovered, err := s.store.RecoverExpiredLeases(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Error("lease recovery failed", "error", err)
	} else if recovered > 0 {
		s.logger.Info("recovered expired leases", "count", recovered)
		// Notify gateway that recovered jobs are available.
		if s.notifier != nil {
			_ = s.notifier.NotifyJobReady(ctx, "")
		}
	}
}

// sleep waits for the given duration or until context is cancelled.
func (s *Scheduler) sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
