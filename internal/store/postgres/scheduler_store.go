package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/store"
)

// SchedulerStore implements the scheduler.Store interface using PostgreSQL.
// It uses a dedicated connection for the advisory lock (session-level locks
// are tied to the connection, not the transaction).
type SchedulerStore struct {
	pool     *pgxpool.Pool
	lockConn *pgx.Conn // dedicated connection for advisory lock
}

// NewSchedulerStore creates a SchedulerStore. The lockConn is a dedicated
// connection used exclusively for the advisory lock; it must not be returned
// to the pool while the lock is held.
func NewSchedulerStore(pool *pgxpool.Pool, lockConn *pgx.Conn) *SchedulerStore {
	return &SchedulerStore{pool: pool, lockConn: lockConn}
}

// TryAcquireLock attempts to acquire the scheduler advisory lock on the
// dedicated connection. Returns true if this instance is now the leader.
func (s *SchedulerStore) TryAcquireLock(ctx context.Context) (bool, error) {
	var acquired bool
	err := s.lockConn.QueryRow(ctx, tryAdvisoryLock, schedulerAdvisoryLockID).Scan(&acquired)
	if err != nil {
		return false, fmt.Errorf("try advisory lock: %w", err)
	}
	return acquired, nil
}

// ReleaseLock releases the scheduler advisory lock.
func (s *SchedulerStore) ReleaseLock(ctx context.Context) error {
	var released bool
	err := s.lockConn.QueryRow(ctx, releaseAdvisoryLock, schedulerAdvisoryLockID).Scan(&released)
	if err != nil {
		return fmt.Errorf("release advisory lock: %w", err)
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
