package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/store"
)

// SchedulerStore implements the scheduler.Store interface using PostgreSQL.
// It uses a dedicated connection for the advisory lock (session-level locks
// are tied to the connection, not the transaction). Leadership itself is
// tracked in the scheduler_leadership lease row (ADR-0005): the advisory lock
// is only the fast-path mutual exclusion, the row is the source of truth.
type SchedulerStore struct {
	pool     *pgxpool.Pool
	lockConn *pgx.Conn // dedicated connection for advisory lock

	// heldAdvisory records whether this store currently holds the advisory
	// lock, so ReleaseLeadership only unlocks what it acquired.
	heldAdvisory bool
}

// NewSchedulerStore creates a SchedulerStore. The lockConn is a dedicated
// connection used exclusively for the advisory lock; it must not be returned
// to the pool while the lock is held.
func NewSchedulerStore(pool *pgxpool.Pool, lockConn *pgx.Conn) *SchedulerStore {
	return &SchedulerStore{pool: pool, lockConn: lockConn}
}

// TryBecomeLeader attempts to make instanceID the scheduler leader
// (ADR-0005). Two paths:
//  1. The advisory lock try succeeds: the previous holder is necessarily
//     gone (session-level locks die with their connection), so the lease
//     row is taken over unconditionally — this keeps the NFR-004 fast path
//     (process termination → immediate takeover).
//  2. The advisory lock is held by someone else: the holder may be a stuck
//     leader whose scan loop no longer runs. The lease row is claimed only
//     when unowned or stale (last_seen older than staleAfter).
//
// Returns the new epoch and acquired=true on success.
func (s *SchedulerStore) TryBecomeLeader(ctx context.Context, instanceID string, staleAfter time.Duration) (int64, bool, error) {
	var lockAcquired bool
	err := s.lockConn.QueryRow(ctx, tryAdvisoryLock, schedulerAdvisoryLockID).Scan(&lockAcquired)
	if err != nil {
		return 0, false, fmt.Errorf("try advisory lock: %w", err)
	}

	if lockAcquired {
		epoch, err := s.claimLease(ctx, claimLeadershipForced, instanceID)
		if err != nil {
			_, _ = s.lockConn.Exec(ctx, releaseAdvisoryLock, schedulerAdvisoryLockID)
			return 0, false, err
		}
		s.heldAdvisory = true
		return epoch, true, nil
	}

	// Advisory lock held by another instance: only a stale lease allows
	// takeover (stuck-leader path).
	epoch, err := s.claimLease(ctx, claimLeadershipIfStale, instanceID, staleAfter)
	if err != nil {
		return 0, false, err
	}
	if epoch == 0 {
		return 0, false, nil
	}
	return epoch, true, nil
}

// claimLease runs a leadership claim upsert. Returns the new epoch, or 0
// when the conditional claim did not match (another leader's lease fresh).
func (s *SchedulerStore) claimLease(ctx context.Context, query string, args ...any) (int64, error) {
	var epoch int64
	err := s.pool.QueryRow(ctx, query, args...).Scan(&epoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("claim leadership: %w", err)
	}
	return epoch, nil
}

// HeartbeatLeadership refreshes the leader's lease (last_seen = now()).
// Returns false when the instance is no longer the leader of the given
// epoch: a standby took over while this instance was stuck, and the caller
// must step down immediately (epoch fencing, ADR-0005). While leader, also
// opportunistically re-acquires the advisory lock if it was lost (e.g. this
// instance took over a stuck leader that later released the lock).
func (s *SchedulerStore) HeartbeatLeadership(ctx context.Context, instanceID string, epoch int64) (bool, error) {
	var got int64
	err := s.pool.QueryRow(ctx, heartbeatLeadership, instanceID, epoch).Scan(&got)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("heartbeat leadership: %w", err)
	}
	if !s.heldAdvisory {
		var acquired bool
		if err := s.lockConn.QueryRow(ctx, tryAdvisoryLock, schedulerAdvisoryLockID).Scan(&acquired); err == nil {
			s.heldAdvisory = acquired
		}
	}
	return true, nil
}

// ReleaseLeadership steps down gracefully: clears leader_id (guarded by
// leader_id and epoch, so a stale release cannot disturb a successor) and
// releases the advisory lock if this store holds it. After this call a
// standby can take over immediately without waiting out the lease.
func (s *SchedulerStore) ReleaseLeadership(ctx context.Context, instanceID string, epoch int64) error {
	if _, err := s.pool.Exec(ctx, releaseLeadership, instanceID, epoch); err != nil {
		return fmt.Errorf("release leadership: %w", err)
	}
	if s.heldAdvisory {
		if _, err := s.lockConn.Exec(ctx, releaseAdvisoryLock, schedulerAdvisoryLockID); err != nil {
			return fmt.Errorf("release advisory lock: %w", err)
		}
		s.heldAdvisory = false
	}
	return nil
}

// PromoteReady transitions scheduled/retry_wait jobs whose run_at has arrived
// to the ready state. Returns the number of jobs promoted.
func (s *SchedulerStore) PromoteReady(ctx context.Context, batchSize int) (int, error) {
	rows, err := s.pool.Query(ctx, promoteReady, batchSize)
	if err != nil {
		return 0, fmt.Errorf("promote ready: %w", err)
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var id, queue string
		if err := rows.Scan(&id, &queue); err != nil {
			return count, fmt.Errorf("scan promoted row: %w", err)
		}
		count++
	}
	return count, rows.Err()
}

// recoveredJob holds the data returned by lease recovery queries.
type recoveredJob struct {
	ID           string
	Queue        string
	LeaseOwner   *string
	Attempt      int
	FencingToken int64
}

// RecoverExpiredLeases recovers running jobs with expired leases (back to
// ready) and cancelling jobs with expired leases (to cancelled). It writes
// audit records (job_attempts + outbox_events) for each recovered job.
// Returns the total number of jobs recovered.
func (s *SchedulerStore) RecoverExpiredLeases(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin recovery tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	total := 0

	// Recover running → ready.
	runningJobs, err := s.recoverQuery(ctx, tx, recoverRunningLeases)
	if err != nil {
		return 0, fmt.Errorf("recover running leases: %w", err)
	}
	for _, j := range runningJobs {
		if err := s.writeRecoveryAudit(ctx, tx, j); err != nil {
			return 0, err
		}
		total++
	}

	// Recover cancelling → cancelled.
	cancellingJobs, err := s.recoverQuery(ctx, tx, recoverCancellingLeases)
	if err != nil {
		return 0, fmt.Errorf("recover cancelling leases: %w", err)
	}
	for _, j := range cancellingJobs {
		if err := s.writeRecoveryAudit(ctx, tx, j); err != nil {
			return 0, err
		}
		total++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit recovery tx: %w", err)
	}
	return total, nil
}

// recoverQuery executes a recovery UPDATE ... RETURNING query.
func (s *SchedulerStore) recoverQuery(ctx context.Context, tx pgx.Tx, query string) ([]recoveredJob, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []recoveredJob
	for rows.Next() {
		var j recoveredJob
		if err := rows.Scan(&j.ID, &j.Queue, &j.LeaseOwner, &j.Attempt, &j.FencingToken); err != nil {
			return nil, fmt.Errorf("scan recovered job: %w", err)
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// writeRecoveryAudit writes job_attempts and outbox_events for a recovered job.
func (s *SchedulerStore) writeRecoveryAudit(ctx context.Context, tx pgx.Tx, j recoveredJob) error {
	workerID := ""
	if j.LeaseOwner != nil {
		workerID = *j.LeaseOwner
	}

	_, err := tx.Exec(ctx, insertRecoveryAttempt, j.ID, j.Attempt, workerID, j.FencingToken)
	if err != nil {
		return fmt.Errorf("insert recovery attempt for job %s: %w", j.ID, err)
	}

	payload, _ := json.Marshal(map[string]string{
		"job_id":  j.ID,
		"queue":   j.Queue,
		"worker":  workerID,
		"attempt": fmt.Sprintf("%d", j.Attempt),
	})
	_, err = tx.Exec(ctx, insertRecoveryOutbox, j.ID, payload)
	if err != nil {
		return fmt.Errorf("insert recovery outbox for job %s: %w", j.ID, err)
	}
	return nil
}

// QueueDepthMetrics samples pending jobs per (tenant, queue, state) so the
// Scheduler can emit the jobforge_queue_depth gauge (PRD 12.1 / FR-502).
func (s *SchedulerStore) QueueDepthMetrics(ctx context.Context) ([]store.QueueDepthRow, error) {
	rows, err := s.pool.Query(ctx, queueDepthMetrics)
	if err != nil {
		return nil, fmt.Errorf("queue depth metrics: %w", err)
	}
	defer rows.Close()

	var result []store.QueueDepthRow
	for rows.Next() {
		var r store.QueueDepthRow
		if err := rows.Scan(&r.TenantID, &r.Queue, &r.State, &r.Count); err != nil {
			return nil, fmt.Errorf("scan queue depth row: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
