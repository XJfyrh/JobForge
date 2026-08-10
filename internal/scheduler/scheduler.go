// Package scheduler implements the JobForge job promotion and lease recovery
// loop. It scans for scheduled/retry_wait jobs whose run_at has arrived and
// promotes them to ready, and recovers jobs whose leases have expired.
//
// Single-active election combines a PostgreSQL advisory lock (fast-path
// mutual exclusion) with a leadership lease row (ADR-0005): the leader
// refreshes last_seen on every scan cycle, standbys take over when the
// lease goes stale (covering leaders whose scan loop is stuck while their
// lock connection stays alive), and epoch fencing makes resurrected old
// leaders step down. Event-driven wakeup via pg_notify supplements the
// periodic scan (ADR-0003).
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/xjfyrh/jobforge/internal/observability"
	"github.com/xjfyrh/jobforge/internal/store"
)

// Store defines the persistence operations required by the Scheduler.
// Following Go convention, the interface is defined at the consumer side.
type Store interface {
	// TryBecomeLeader attempts to elect instanceID as leader (advisory lock
	// fast path plus the leadership lease row, ADR-0005). Returns the new
	// epoch and acquired=true on success; acquired=false when another
	// leader's lease is still fresh.
	TryBecomeLeader(ctx context.Context, instanceID string, staleAfter time.Duration) (int64, bool, error)

	// HeartbeatLeadership refreshes the leader's lease. Returns false when
	// the instance is no longer the leader of the given epoch (taken over);
	// the caller must step down immediately.
	HeartbeatLeadership(ctx context.Context, instanceID string, epoch int64) (bool, error)

	// ReleaseLeadership steps down gracefully so a standby can take over
	// immediately (clears leader_id, releases the advisory lock if held).
	ReleaseLeadership(ctx context.Context, instanceID string, epoch int64) error

	// PromoteReady transitions scheduled/retry_wait jobs to ready.
	// Returns the number of jobs promoted.
	PromoteReady(ctx context.Context, batchSize int) (int, error)

	// RecoverExpiredLeases recovers running jobs with expired leases
	// (back to ready) and cancelling jobs with expired leases (to cancelled).
	// Returns the total number of jobs recovered.
	RecoverExpiredLeases(ctx context.Context) (int, error)

	// QueueDepthMetrics samples pending jobs per (tenant, queue, state) for
	// the jobforge_queue_depth gauge (PRD 12.1 / FR-502).
	QueueDepthMetrics(ctx context.Context) ([]store.QueueDepthRow, error)

	// QuotaDrift compares tenant_quota_counters against the jobs aggregation
	// and returns every disagreeing tenant (PRD v0.3 FR-724).
	QuotaDrift(ctx context.Context) ([]store.QuotaDriftRow, error)

	// RepairQuotaCounters overwrites the derived counters with the jobs
	// aggregation (the source of truth). Returns rows changed.
	RepairQuotaCounters(ctx context.Context) (int, error)
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

	// InstanceID uniquely identifies this scheduler instance in the
	// leadership lease row (typically hostname-pid).
	InstanceID string

	// LeadershipTimeout bounds how long the leader may go without a
	// heartbeat before standbys may take over. The heartbeat rides on scan
	// cycles, so a stuck scan loop ages the lease past this bound and
	// triggers takeover (ADR-0005).
	LeadershipTimeout time.Duration

	// QuotaReconcileInterval is how often the leader reconciles
	// tenant_quota_counters against the jobs aggregation, records the drift
	// gauge and repairs discrepancies (PRD v0.3 FR-724). <= 0 disables it.
	QuotaReconcileInterval time.Duration
}

// DefaultConfig returns production-default Scheduler configuration.
func DefaultConfig() Config {
	return Config{
		ScanInterval:      1 * time.Second,
		PromoteBatchSize:  1000,
		LockRetryInterval: 2 * time.Second,
		LeadershipTimeout: 10 * time.Second,
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
	metrics  *observability.Metrics

	// lastQuotaReconcile gates the periodic quota reconcile so it runs at
	// most once per QuotaReconcileInterval instead of every scan cycle.
	lastQuotaReconcile time.Time
}

// New creates a Scheduler with the given dependencies.
func New(store Store, notifier Notifier, listener Listener, cfg Config, logger *slog.Logger, metrics *observability.Metrics) *Scheduler {
	return &Scheduler{
		store:    store,
		notifier: notifier,
		listener: listener,
		cfg:      cfg,
		logger:   logger,
		metrics:  metrics,
	}
}

// Run starts the Scheduler main loop. It blocks until ctx is cancelled.
// The loop: become leader → scan (heartbeating the lease each cycle) →
// release leadership → wait (notify or timer).
func (s *Scheduler) Run(ctx context.Context) error {
	s.logger.Info("scheduler starting",
		"scan_interval", s.cfg.ScanInterval,
		"batch_size", s.cfg.PromoteBatchSize,
		"instance_id", s.cfg.InstanceID,
		"leadership_timeout", s.cfg.LeadershipTimeout,
	)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		epoch, acquired, err := s.store.TryBecomeLeader(ctx, s.cfg.InstanceID, s.cfg.LeadershipTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Error("failed to acquire leadership", "error", err)
			s.sleep(ctx, s.cfg.LockRetryInterval)
			continue
		}

		if !acquired {
			// Standby mode: another leader's lease is fresh; wait and retry.
			s.logger.Debug("scheduler standby: leadership held by another instance")
			s.sleep(ctx, s.cfg.LockRetryInterval)
			continue
		}

		s.logger.Info("scheduler acquired leadership", "epoch", epoch)
		s.runLoop(ctx, epoch)

		// Lost leadership (ctx cancelled or lease taken over). Release on a
		// detached context: ctx may already be done, but stepping down cleanly
		// still matters for fast takeover. The epoch guard makes this a no-op
		// when a successor already owns the lease.
		relCtx, relCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.store.ReleaseLeadership(relCtx, s.cfg.InstanceID, epoch); err != nil {
			s.logger.Warn("release leadership failed", "epoch", epoch, "error", err)
		}
		relCancel()
		s.logger.Info("scheduler released leadership", "epoch", epoch)

		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// runLoop performs scan cycles until the context is cancelled or the
// leadership lease is taken over (epoch fencing detected by scanCycle).
func (s *Scheduler) runLoop(ctx context.Context, epoch int64) {
	// Perform an immediate scan on startup.
	if !s.scanCycle(ctx, epoch) {
		return
	}

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
			if !s.scanCycle(ctx, epoch) {
				return
			}
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
				if !s.scanCycle(ctx, epoch) {
					return
				}
				timer.Reset(s.cfg.ScanInterval)
			case <-ctx.Done():
				return
			}
		}
	}
}

// scanCycle performs one promote + recover cycle after refreshing the
// leadership lease. Returns false when the lease has been taken over
// (heartbeat matched no row); the caller must stop scanning immediately.
func (s *Scheduler) scanCycle(ctx context.Context, epoch int64) bool {
	if ctx.Err() != nil {
		return true
	}

	// Heartbeat the lease before doing any work: a stuck scan loop stops
	// reaching this point, which is exactly what ages the lease out and lets
	// a standby take over (ADR-0005). A heartbeat that matches no row means
	// a standby already took over while we were stuck: step down.
	stillLeader, err := s.store.HeartbeatLeadership(ctx, s.cfg.InstanceID, epoch)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		// Transient heartbeat errors must not demote a healthy leader; the
		// scan below will surface the same database problem.
		s.logger.Error("leadership heartbeat failed", "epoch", epoch, "error", err)
	} else if !stillLeader {
		s.logger.Info("leadership lost (taken over by a standby), stepping down", "epoch", epoch)
		return false
	}

	// scheduler.promote_jobs span (PRD 12.2).
	ctx, span := observability.Tracer("jobforge.scheduler").Start(ctx, "scheduler.promote_jobs")
	defer span.End()

	// Promote scheduled/retry_wait → ready.
	promoted, err := s.store.PromoteReady(ctx, s.cfg.PromoteBatchSize)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		s.logger.Error("promote failed", "error", err)
	} else if promoted > 0 {
		span.SetAttributes(attribute.Int("jobs.promoted", promoted))
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
			return true
		}
		s.logger.Error("lease recovery failed", "error", err)
	} else if recovered > 0 {
		span.SetAttributes(attribute.Int("jobs.recovered", recovered))
		s.logger.Info("recovered expired leases", "count", recovered)
		// Record lease expired metric (PRD 12.1).
		if s.metrics != nil {
			s.metrics.LeaseExpiredTotal.Add(ctx, int64(recovered),
				metric.WithAttributes(attribute.String("queue", "")))
		}
		// Notify gateway that recovered jobs are available.
		if s.notifier != nil {
			_ = s.notifier.NotifyJobReady(ctx, "")
		}
	}

	// Emit the jobforge_queue_depth gauge (PRD 12.1 / FR-502).
	s.recordQueueDepth(ctx)

	// Periodic quota counter reconcile + repair (PRD v0.3 FR-724).
	s.reconcileQuota(ctx)
	return true
}

// reconcileQuota compares tenant_quota_counters with the jobs aggregation at
// most once per QuotaReconcileInterval, records the drift gauge and repairs
// any drift with the jobs aggregation as the source of truth (ADR-0007 §7).
// Best-effort: errors are logged but never break the scan loop.
func (s *Scheduler) reconcileQuota(ctx context.Context) {
	if s.cfg.QuotaReconcileInterval <= 0 || ctx.Err() != nil {
		return
	}
	if time.Since(s.lastQuotaReconcile) < s.cfg.QuotaReconcileInterval {
		return
	}
	s.lastQuotaReconcile = time.Now()

	drift, err := s.store.QuotaDrift(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("quota reconcile failed", "error", err)
		}
		return
	}

	var totalDrift int64
	for _, r := range drift {
		d := r.Counter - r.Actual
		if d < 0 {
			d = -d
		}
		totalDrift += d
		s.logger.Warn("quota counter drift detected",
			"tenant", r.TenantID,
			"counter", r.Counter,
			"actual", r.Actual,
		)
	}
	if s.metrics != nil {
		s.metrics.QuotaCounterDrift.Record(ctx, totalDrift)
	}

	if totalDrift == 0 {
		return
	}

	// Repair from the jobs aggregation; the structured log above is the
	// audit trail (ADR-0007 §7).
	repaired, err := s.store.RepairQuotaCounters(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("quota counter repair failed", "error", err)
		}
		return
	}
	s.logger.Info("quota counters repaired", "drift", totalDrift, "rows_repaired", repaired)
	if s.metrics != nil {
		s.metrics.QuotaCounterDrift.Record(ctx, 0)
	}
}

// recordQueueDepth samples pending jobs per (tenant, queue, state) and
// records the jobforge_queue_depth gauge. Best-effort: errors are logged
// but never break the scan loop.
func (s *Scheduler) recordQueueDepth(ctx context.Context) {
	if s.metrics == nil || ctx.Err() != nil {
		return
	}
	rows, err := s.store.QueueDepthMetrics(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Error("queue depth metrics sample failed", "error", err)
		}
		return
	}
	for _, r := range rows {
		s.metrics.QueueDepth.Record(ctx, r.Count,
			metric.WithAttributes(
				attribute.String("tenant", r.TenantID),
				attribute.String("queue", r.Queue),
				attribute.String("state", r.State),
			))
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
