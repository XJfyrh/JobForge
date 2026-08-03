// Package postgres implements the store.JobStore interface using PostgreSQL.
// All state transitions are atomic within database transactions. The claim
// operation uses FOR UPDATE SKIP LOCKED to prevent concurrent Workers from
// claiming the same job.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/xjfyrh/jobforge/internal/domain"
	"github.com/xjfyrh/jobforge/internal/store"
)

// JobStore implements store.JobStore using PostgreSQL.
type JobStore struct {
	pool *pgxpool.Pool
}

// NewJobStore creates a new PostgreSQL-backed job store.
func NewJobStore(pool *pgxpool.Pool) *JobStore {
	return &JobStore{pool: pool}
}

// Ensure interface compliance at compile time.
var _ store.JobStore = (*JobStore)(nil)

// Ping performs a lightweight connectivity check against PostgreSQL.
// It satisfies the http.Pinger interface for health-readiness probes.
func (s *JobStore) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Enqueue persists a new job. If the idempotency key already exists for the
// tenant, it returns deduplicated=true without creating a duplicate.
// After a successful insert of a ready job, sends pg_notify to wake up
// Gateway Poll listeners (ADR-0003).
func (s *JobStore) Enqueue(ctx context.Context, job *domain.Job) (bool, error) {
	_, err := s.pool.Exec(ctx, enqueueInsert,
		job.ID, job.TenantID, job.Queue, job.Type, job.Payload,
		job.Priority, string(job.State), job.RunAt, job.Attempt,
		job.MaxAttempts, job.TimeoutSeconds, job.IdempotencyKey,
		job.FencingToken, job.TraceID, job.TraceContext, job.StateVersion,
		job.RetryOfJobID, job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("enqueue insert: %w", err)
	}

	// Fetch the job to determine if it was inserted or deduplicated.
	var inserted bool
	err = s.pool.QueryRow(ctx, enqueueSelectByID, job.ID).Scan(
		&job.ID, &job.TenantID, &job.Queue, &job.Type, &job.Payload,
		&job.Priority, &job.State, &job.RunAt, &job.Attempt,
		&job.MaxAttempts, &job.TimeoutSeconds, &job.IdempotencyKey,
		&job.LeaseOwner, &job.LeaseUntil, &job.FencingToken,
		&job.CancelRequestedAt, &job.TraceID, &job.TraceContext, &job.StateVersion,
		&job.RetryOfJobID, &job.CreatedAt, &job.UpdatedAt, &inserted,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Conflict path: our insert did nothing, fetch by idempotency key.
			return true, nil
		}
		return false, fmt.Errorf("enqueue select: %w", err)
	}

	// Notify listeners if a new ready job was inserted.
	if inserted && job.State == domain.StateReady {
		_, _ = s.pool.Exec(ctx, "select pg_notify('jobforge_job_ready', $1)", job.Queue)
	}

	return !inserted, nil
}

// GetByID retrieves a job by ID scoped to the given tenant.
func (s *JobStore) GetByID(ctx context.Context, tenantID, jobID string) (*domain.Job, error) {
	job, err := scanJob(s.pool.QueryRow(ctx, getByID, jobID, tenantID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.NewError(domain.CodeNotFound, domain.ErrNotFound,
				"job %s not found for tenant %s", jobID, tenantID)
		}
		return nil, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

// List retrieves jobs with keyset pagination.
func (s *JobStore) List(ctx context.Context, filter store.ListFilter) ([]*domain.Job, string, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}

	var stateStr *string
	if filter.State != nil {
		v := string(*filter.State)
		stateStr = &v
	}

	var cursorTime *time.Time
	if filter.Cursor != nil && *filter.Cursor != "" {
		t, err := time.Parse(time.RFC3339Nano, *filter.Cursor)
		if err != nil {
			return nil, "", domain.NewError(domain.CodeInvalidArgument, domain.ErrInvalidArgument,
				"invalid cursor format")
		}
		cursorTime = &t
	}

	rows, err := s.pool.Query(ctx, listJobs,
		filter.TenantID, stateStr, filter.Queue, filter.Type, cursorTime, limit+1,
	)
	if err != nil {
		return nil, "", fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var jobs []*domain.Job
	for rows.Next() {
		job, err := scanJobFromRows(rows)
		if err != nil {
			return nil, "", fmt.Errorf("scan job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("rows iteration: %w", err)
	}

	var nextCursor string
	if len(jobs) > limit {
		jobs = jobs[:limit]
		nextCursor = jobs[len(jobs)-1].CreatedAt.Format(time.RFC3339Nano)
	}

	return jobs, nextCursor, nil
}

// Claim atomically claims up to maxJobs ready jobs for a worker.
//
// Transaction invariant: SELECT FOR UPDATE SKIP LOCKED + UPDATE + INSERT
// job_attempts all happen in a single transaction. This guarantees that
// lease_owner, lease_until, attempt, fencing_token and state are updated
// atomically, and no two Workers can claim the same job.
func (s *JobStore) Claim(ctx context.Context, params store.ClaimParams) ([]*domain.Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin claim tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Select ready jobs with row-level locking.
	rows, err := tx.Query(ctx, claimSelect, params.Queue, params.Types, params.MaxJobs)
	if err != nil {
		return nil, fmt.Errorf("claim select: %w", err)
	}

	var candidates []*domain.Job
	for rows.Next() {
		job, err := scanJobFromRows(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan claim candidate: %w", err)
		}
		candidates = append(candidates, job)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim rows: %w", err)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	leaseUntil := time.Now().Add(params.LeaseTTL)
	claimed := make([]*domain.Job, 0, len(candidates))

	for _, candidate := range candidates {
		// Check tenant quota if limit is set (FR-302).
		// The running count includes jobs claimed earlier in this transaction
		// because PostgreSQL allows reading our own uncommitted changes.
		if params.TenantMaxInflight > 0 {
			var runningCount int
			err := tx.QueryRow(ctx,
				"select count(*) from jobs where tenant_id = $1 and state = 'running'",
				candidate.TenantID).Scan(&runningCount)
			if err != nil {
				return nil, fmt.Errorf("check tenant quota: %w", err)
			}
			if runningCount >= params.TenantMaxInflight {
				// Tenant quota full, skip this job.
				continue
			}
		}

		// Update lease fields atomically.
		job, err := scanJob(tx.QueryRow(ctx, claimUpdate, candidate.ID, params.WorkerID, leaseUntil))
		if err != nil {
			return nil, fmt.Errorf("claim update: %w", err)
		}

		// Record the attempt start.
		_, err = tx.Exec(ctx, claimInsertAttempt,
			job.ID, job.Attempt, params.WorkerID, job.FencingToken, job.TraceID)
		if err != nil {
			return nil, fmt.Errorf("claim insert attempt: %w", err)
		}

		claimed = append(claimed, job)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit claim tx: %w", err)
	}

	return claimed, nil
}

// Heartbeat extends the lease for a running/cancelling job.
func (s *JobStore) Heartbeat(ctx context.Context, jobID, workerID string, fencingToken int64, ttl time.Duration) error {
	leaseUntil := time.Now().Add(ttl)
	tag, err := s.pool.Exec(ctx, heartbeatUpdate, jobID, workerID, fencingToken, leaseUntil)
	if err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
			"heartbeat rejected: owner/token mismatch or job not in active state")
	}
	return nil
}

// Complete transitions a running job to succeeded within a transaction that
// also updates the attempt record and writes an outbox event.
func (s *JobStore) Complete(ctx context.Context, jobID, workerID string, fencingToken int64, _ string, durationMs int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin complete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, completeUpdate, jobID, workerID, fencingToken)
	if err != nil {
		return fmt.Errorf("complete update: %w", err)
	}

	if tag.RowsAffected() == 0 {
		// Determine specific rejection reason.
		var state string
		err := tx.QueryRow(ctx, completeRejectCancelling, jobID, workerID, fencingToken).Scan(&state)
		if err == nil {
			return domain.NewError(domain.CodeCancelRequested, domain.ErrCancelRequested,
				"job is cancelling: complete rejected")
		}
		return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
			"complete rejected: owner/token mismatch or job not running")
	}

	// Update attempt outcome.
	if err := updateAttempt(ctx, tx, jobID, workerID, "succeeded", nil, nil, &durationMs); err != nil {
		return err
	}

	// Write outbox event.
	if err := writeOutbox(ctx, tx, jobID, "job.succeeded"); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Fail transitions a running job to retry_wait or dead, or a cancelling job
// to cancelled. All within a single transaction with attempt and outbox writes.
func (s *JobStore) Fail(ctx context.Context, jobID, workerID string, fencingToken int64, errCode, errMsg string, retryable bool, durationMs int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fail tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Try cancelling → cancelled first.
	tag, err := tx.Exec(ctx, failUpdateCancelling, jobID, workerID, fencingToken)
	if err != nil {
		return fmt.Errorf("fail cancelling: %w", err)
	}

	var outcome string
	if tag.RowsAffected() > 0 {
		outcome = "cancelled"
	} else {
		// Determine retry vs dead.
		// Fetch current attempt count to decide.
		var attempt, maxAttempts int
		err := tx.QueryRow(ctx, "select attempt, max_attempts from jobs where id = $1 and lease_owner = $2 and fencing_token = $3 and state = 'running'",
			jobID, workerID, fencingToken).Scan(&attempt, &maxAttempts)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
					"fail rejected: owner/token mismatch or job not running")
			}
			return fmt.Errorf("fail check: %w", err)
		}

		if retryable && attempt < maxAttempts {
			// Calculate backoff: base=1s, max=5min, full jitter (0–1s random).
			jitter := time.Duration(rand.Int64N(int64(time.Second)))
			backoff := domain.Backoff(attempt, time.Second, 5*time.Minute, jitter)
			nextRetry := time.Now().Add(backoff)
			tag, err = tx.Exec(ctx, failUpdateRetry, jobID, workerID, fencingToken, nextRetry)
			if err != nil {
				return fmt.Errorf("fail retry: %w", err)
			}
			outcome = "failed_retry"
		} else {
			tag, err = tx.Exec(ctx, failUpdateDead, jobID, workerID, fencingToken)
			if err != nil {
				return fmt.Errorf("fail dead: %w", err)
			}
			outcome = "failed_dead"
		}

		if tag.RowsAffected() == 0 {
			return domain.NewError(domain.CodeStaleLease, domain.ErrStaleLease,
				"fail rejected: concurrent state change")
		}
	}

	// Update attempt outcome.
	if err := updateAttempt(ctx, tx, jobID, workerID, outcome, &errCode, &errMsg, &durationMs); err != nil {
		return err
	}

	// Write outbox event.
	eventType := "job." + outcome
	if err := writeOutbox(ctx, tx, jobID, eventType); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// Cancel requests cancellation. Waiting-state jobs go directly to cancelled;
// running jobs enter cancelling.
func (s *JobStore) Cancel(ctx context.Context, tenantID, jobID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin cancel tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Try waiting-state cancel first.
	tag, err := tx.Exec(ctx, cancelWaiting, jobID, tenantID)
	if err != nil {
		return fmt.Errorf("cancel waiting: %w", err)
	}
	if tag.RowsAffected() > 0 {
		if err := writeOutbox(ctx, tx, jobID, "job.cancelled"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// Try running → cancelling.
	tag, err = tx.Exec(ctx, cancelRunning, jobID, tenantID)
	if err != nil {
		return fmt.Errorf("cancel running: %w", err)
	}
	if tag.RowsAffected() > 0 {
		if err := writeOutbox(ctx, tx, jobID, "job.cancelling"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// Check if already terminal.
	var state string
	err = tx.QueryRow(ctx, checkTerminal, jobID, tenantID).Scan(&state)
	if err == nil {
		return domain.NewError(domain.CodeAlreadyTerminal, domain.ErrAlreadyTerminal,
			"job already in terminal state %q", state)
	}

	return domain.NewError(domain.CodeNotFound, domain.ErrNotFound,
		"job %s not found for tenant %s", jobID, tenantID)
}

// updateAttempt records the outcome of an attempt within the transaction.
func updateAttempt(ctx context.Context, tx pgx.Tx, jobID, _, outcome string, errCode, errMsg *string, durationMs *int64) error {
	// Get the current attempt number for this job.
	var attemptNo int
	err := tx.QueryRow(ctx, "select attempt from jobs where id = $1", jobID).Scan(&attemptNo)
	if err != nil {
		return fmt.Errorf("get attempt no: %w", err)
	}

	_, err = tx.Exec(ctx, updateAttemptOutcome, jobID, attemptNo, outcome, errCode, errMsg, durationMs)
	if err != nil {
		return fmt.Errorf("update attempt: %w", err)
	}
	return nil
}

// writeOutbox inserts an outbox event within the transaction.
func writeOutbox(ctx context.Context, tx pgx.Tx, aggregateID, eventType string) error {
	payload, _ := json.Marshal(map[string]string{
		"job_id":     aggregateID,
		"event_type": eventType,
	})
	_, err := tx.Exec(ctx, insertOutboxEvent, aggregateID, eventType, payload)
	if err != nil {
		return fmt.Errorf("write outbox: %w", err)
	}
	return nil
}

// scanJob scans a single job row from a Row.
func scanJob(row pgx.Row) (*domain.Job, error) {
	var j domain.Job
	var state string
	err := row.Scan(
		&j.ID, &j.TenantID, &j.Queue, &j.Type, &j.Payload,
		&j.Priority, &state, &j.RunAt, &j.Attempt,
		&j.MaxAttempts, &j.TimeoutSeconds, &j.IdempotencyKey,
		&j.LeaseOwner, &j.LeaseUntil, &j.FencingToken,
		&j.CancelRequestedAt, &j.TraceID, &j.TraceContext, &j.StateVersion,
		&j.RetryOfJobID, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	j.State = domain.JobState(state)
	return &j, nil
}

// scanJobFromRows scans a job from a Rows iterator.
func scanJobFromRows(rows pgx.Rows) (*domain.Job, error) {
	var j domain.Job
	var state string
	err := rows.Scan(
		&j.ID, &j.TenantID, &j.Queue, &j.Type, &j.Payload,
		&j.Priority, &state, &j.RunAt, &j.Attempt,
		&j.MaxAttempts, &j.TimeoutSeconds, &j.IdempotencyKey,
		&j.LeaseOwner, &j.LeaseUntil, &j.FencingToken,
		&j.CancelRequestedAt, &j.TraceID, &j.TraceContext, &j.StateVersion,
		&j.RetryOfJobID, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	j.State = domain.JobState(state)
	return &j, nil
}

// GetQueueDepth returns the number of pending jobs in a queue.
// Used for queue backpressure (FR-303).
func (s *JobStore) GetQueueDepth(ctx context.Context, queue string) (int, error) {
	var count int
	err := s.pool.QueryRow(ctx, getQueueDepth, queue).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("get queue depth: %w", err)
	}
	return count, nil
}

// ListAttempts returns the attempt timeline of a job scoped to the given
// tenant, ordered by attempt_no ascending (FR-002). Returns NOT_FOUND when
// the job does not exist for the tenant.
func (s *JobStore) ListAttempts(ctx context.Context, tenantID, jobID string) ([]store.AttemptRecord, error) {
	// Tenant scoping: reject unknown / foreign jobs first.
	if _, err := s.GetByID(ctx, tenantID, jobID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, listAttempts, jobID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()

	attempts := make([]store.AttemptRecord, 0)
	for rows.Next() {
		var a store.AttemptRecord
		a.JobID = jobID
		var outcome *string
		if err := rows.Scan(
			&a.AttemptNo, &a.WorkerID, &a.FencingToken, &a.StartedAt, &a.FinishedAt,
			&outcome, &a.ErrorCode, &a.ErrorMessage, &a.DurationMs, &a.TraceID,
		); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		if outcome != nil {
			a.Outcome = *outcome
		}
		attempts = append(attempts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("attempts rows: %w", err)
	}
	return attempts, nil
}
